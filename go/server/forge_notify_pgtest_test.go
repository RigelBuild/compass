//go:build pgtest && unix

package server

// Store-gated end-to-end proofs for the RIG-2732 T7 GitHub agent-notification
// serve assembly: the pieces buildForgeNotifyLane composes (the forgeNotifyStore
// adapter over a real *store.Store binding (provider, host), the notify router +
// webhook arm behind the shared /webhooks/github fanoutSink, the hub-backed
// dispatcher) driven against a REAL Postgres — no live GitHub (the checks roller
// is a fake) and no live hub (the dispatcher is a fake recording notifications).
// Behind `pgtest && unix` (SKIP when no runtime). Each test opens its own
// isolated-schema store (forgeTestStore, sibling serve_forge_pgtest_test.go).
//
// The observable contracts (design.md:1080-1087), scoped to what this slice wires:
//   - App-gated: buildBoardWebhookWiring with no App configured returns all-nil,
//     so no notify lane is built (the gate moved to the shared site, RIG-2991).
//   - Routed notify: an event fed through the assembled lane's sink dispatches a
//     ForgeNotification to the seeded subscriber AND advances the shared FETCH
//     cursor — but NEVER advances the subscriber's delivered_revision (W3).
//   - No live session: the dispatcher's no-session error is non-fatal; the fetch
//     cursor still advances and delivered_revision stays unadvanced.
//
// These pgtests only need to COMPILE locally; the CI gate runs them against
// suite Postgres.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/ingest"
	"github.com/RigelBuild/compass/go/internal/store"
)

// recordingDispatcher is the notify dispatcher fake: it records every dispatched
// ForgeNotification per account (no live hub). When noSession is true it returns
// the same no-live-session sentinel the real forgeNotifyDispatcher returns, so
// the router's log-and-move-on isolation is exercised without a hub.
type recordingDispatcher struct {
	sent      []*compassv1internal.ForgeNotification
	accounts  []string
	noSession bool
}

func (d *recordingDispatcher) Notify(_ context.Context, account string, n *compassv1internal.ForgeNotification) error {
	if d.noSession {
		return errNoLiveSession
	}
	d.sent = append(d.sent, n)
	d.accounts = append(d.accounts, account)
	return nil
}

// fixedChecksRoller is the CHECKS roll-up fake (the *forge.GitHub role): the
// tests drive a COMMENT event, so RollUp is never called, but a named fake keeps
// the router's ChecksRoller seam satisfied with a concrete type.
type fixedChecksRoller struct{}

func (fixedChecksRoller) RollUp(_ context.Context, _ string, _ uint64, _, _ string) (forge.ConditionalResult[forge.Checks], error) {
	return forge.ConditionalResult[forge.Checks]{}, nil
}

// seedNotifySubscription creates an agent + its owning user and an artifact-scope
// subscription at the coordinate, returning the agent account id and the
// subscription id. The FK on agent_forge_subscriptions requires a real agent.
func seedNotifySubscription(t *testing.T, st *store.Store, repo string, kind store.ForgeArtifactKind, number uint64) (store.AccountID, string) {
	t.Helper()
	ctx := context.Background() // test root
	owner, err := st.CreateUser(ctx, store.NewUser{Handle: "notify-owner", DisplayName: "Notify Owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "notify-agent", DisplayName: "Notify Agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	subID, err := st.EnsureAgentForgeSubscription(ctx, store.AgentForgeSubscription{
		AgentAccountID: agent.ID,
		Provider:       store.ForgeProviderGitHub,
		Host:           forgeTestHost,
		Repo:           repo,
		Kind:           kind,
		Number:         number,
	})
	if err != nil {
		t.Fatalf("EnsureAgentForgeSubscription: %v", err)
	}
	return agent.ID, subID
}

// deliveredRevision reads one subscription's per-subscriber DELIVERY cursor
// directly — the W3 assertion surface (no public reader exposes it in isolation).
func deliveredRevision(t *testing.T, st *store.Store, agent store.AccountID, subID string) string {
	t.Helper()
	// AdvanceForgeDeliveredRevision is the only writer; the value is otherwise
	// read only via ListForgeNotifyTargets, so enumerate and match the sub.
	targets, err := st.ListForgeNotifyTargets(context.Background(), store.ForgeProviderGitHub, forgeTestHost)
	if err != nil {
		t.Fatalf("ListForgeNotifyTargets: %v", err)
	}
	for _, tg := range targets {
		for _, s := range tg.Subscribers {
			if s.SubscriptionID == subID {
				return s.DeliveredRevision
			}
		}
	}
	t.Fatalf("subscription %q not found among notify targets", subID)
	return ""
}

// deliveredRevisionAt is deliveredRevision's provider-bound sibling: it reads one
// subscription's DELIVERY cursor for an arbitrary (provider, host), so a Linear
// container-scope sub can be asserted the same way.
func deliveredRevisionAt(t *testing.T, st *store.Store, provider store.ForgeProvider, host string, subID string) string {
	t.Helper()
	targets, err := st.ListForgeNotifyTargets(context.Background(), provider, host)
	if err != nil {
		t.Fatalf("ListForgeNotifyTargets: %v", err)
	}
	for _, tg := range targets {
		for _, s := range tg.Subscribers {
			if s.SubscriptionID == subID {
				return s.DeliveredRevision
			}
		}
	}
	t.Fatalf("subscription %q not found among notify targets", subID)
	return ""
}

// commentEvent builds a GitHub issue-comment ForgeEvent at the coordinate — the
// simplest non-CHECKS event, so the router never touches the checks roller.
func notifyCommentEvent(repo string, number uint64, url string) forge.ForgeEvent {
	return forge.ForgeEvent{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
		Host:     forgeTestHost,
		Repo:     repo,
		Kind:     compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE,
		Number:   number,
		URL:      url,
		Change:   compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_COMMENT,
		Comment: &compassv1internal.CommentRef{
			Url:  url,
			Body: "a new comment",
		},
	}
}

// --- test: App-config-absent boot leaves both forge lanes off -----------------

// TestForgeNotifyLaneDisabledWithoutApp proves the App gate at the shared wiring
// site (RIG-2991): buildBoardWebhookWiring with no App configured returns all-nil
// — both the board lane and the notify lane are hard-off, so the caller composes
// no notify sink onto the ingress. The App gate now lives on the shared wiring
// (which builds the one client both lanes ride), not on buildForgeNotifyLane. A
// nil hub is fine: the gate short-circuits before any lane assembly.
func TestForgeNotifyLaneDisabledWithoutApp(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background() // test root

	// issueBrd is nil-safe here: the App-absent gate short-circuits before any
	// lane assembly, so the projection is never dereferenced.
	lane, notifyLane, sink, secret, client, err := buildBoardWebhookWiring(ctx, ServeConfig{Forge: ForgeConfig{Host: forgeTestHost}}, st, nil, nil, &fakeResolver{}, slog.Default())
	if err != nil {
		t.Fatalf("buildBoardWebhookWiring (App absent): %v", err)
	}
	if notifyLane != nil {
		t.Fatal("notifyLane != nil with no App configured, want nil (notify lane hard-off)")
	}
	if lane != nil || sink != nil || secret != nil || client != nil {
		t.Fatalf("wiring not all-nil with no App configured: lane==nil? %t sink==nil? %t secret==nil? %t client==nil? %t", lane == nil, sink == nil, secret == nil, client == nil)
	}
}

// --- test: routed notify advances the fetch cursor but NOT delivered_revision -

// TestForgeNotifyRoutedAdvancesFetchCursorOnly drives the assembled lane's sink:
// a COMMENT event fed through arm.Enqueue (the ForgeEventSink) drains through the
// notify router over the REAL store adapters, which (1) dispatches a
// ForgeNotification to the seeded subscriber and (2) advances the shared FETCH
// cursor (forge_artifact_cursors) with the new snapshot + revision — while the
// subscriber's delivered_revision stays unadvanced (W3: the ack arm owns that,
// never the route). Only the dispatcher and the checks roller are fakes; the
// store adapters + router + arm are the real assembled seams.
func TestForgeNotifyRoutedAdvancesFetchCursorOnly(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background() // test root
	const (
		repo   = "a/b"
		number = uint64(42)
		url    = "https://github.com/a/b/issues/42#c1"
	)
	agent, subID := seedNotifySubscription(t, st, repo, store.ForgeArtifactKindIssue, number)

	notifyStore := &forgeNotifyStore{st: st, provider: store.ForgeProviderGitHub, host: forgeTestHost}
	disp := &recordingDispatcher{}
	forgeRef := &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: forgeTestHost}
	router := ingest.NewNotifyRouter(notifyStore, disp, fixedChecksRoller{}, nil, nil, forgeRef, nil)
	arm := ingest.NewNotifyWebhookArm(router, ingest.NotifyArmConfig{})

	// Precondition: never observed → no fetch cursor, empty delivered_revision.
	if got := deliveredRevision(t, st, agent, subID); got != "" {
		t.Fatalf("precondition delivered_revision = %q, want empty", got)
	}

	// Drive the arm's async drain (the sink path) and bound the wait on the
	// dispatch + cursor advance rather than a fixed sleep.
	drainCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- arm.Run(drainCtx) }()

	arm.Enqueue(ctx, notifyCommentEvent(repo, number, url))

	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	var cursorRevision string
	for cursorRevision == "" {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for the notify route to advance the fetch cursor")
		case <-tick.C:
			cur, err := st.LoadForgeArtifactCursor(ctx, store.ForgeProviderGitHub, forgeTestHost, repo, store.ForgeArtifactKindIssue, number)
			if err != nil {
				t.Fatalf("LoadForgeArtifactCursor: %v", err)
			}
			if cur != nil {
				cursorRevision = cur.Revision
			}
		}
	}

	// Stop the drain and confirm a clean (nil) shutdown.
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("arm.Run returned a non-nil error on ctx-cancel: %v", err)
	}

	// (1) The dispatcher saw exactly one ForgeNotification for the subscriber,
	// carrying the right coordinate + change + the cursor's revision.
	if len(disp.sent) != 1 {
		t.Fatalf("dispatched notifications = %d, want 1", len(disp.sent))
	}
	n := disp.sent[0]
	if disp.accounts[0] != string(agent) {
		t.Errorf("notified account = %q, want %q", disp.accounts[0], agent)
	}
	if n.GetSubscriptionId() != subID {
		t.Errorf("notification subscription_id = %q, want %q", n.GetSubscriptionId(), subID)
	}
	if n.GetRepo() != repo || n.GetNumber() != number {
		t.Errorf("notification coordinate = %s#%d, want %s#%d", n.GetRepo(), n.GetNumber(), repo, number)
	}
	if n.GetChange() != compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_COMMENT {
		t.Errorf("notification change = %v, want COMMENT", n.GetChange())
	}
	if n.GetRevision() != cursorRevision {
		t.Errorf("notification revision = %q, want the cursor revision %q", n.GetRevision(), cursorRevision)
	}

	// (2) The shared FETCH cursor advanced with a snapshot; delivered_revision
	// did NOT advance (W3 — the route never touches it).
	cur, err := st.LoadForgeArtifactCursor(ctx, store.ForgeProviderGitHub, forgeTestHost, repo, store.ForgeArtifactKindIssue, number)
	if err != nil {
		t.Fatalf("LoadForgeArtifactCursor (post): %v", err)
	}
	if cur == nil {
		t.Fatal("fetch cursor is nil after route, want an advanced row")
	}
	if len(cur.Snapshot) == 0 {
		t.Error("fetch cursor snapshot is empty after route, want the observed snapshot")
	}
	if got := deliveredRevision(t, st, agent, subID); got != "" {
		t.Fatalf("delivered_revision = %q after route, want empty (W3: the route never advances it)", got)
	}
}

// --- test: no live session is non-fatal, fetch cursor still advances ----------

// TestForgeNotifyNoLiveSessionIsNonFatal proves the no-session path: the
// dispatcher returns the no-live-session sentinel, so the router logs and moves
// on (no crash, no panic). The shared FETCH cursor still advances (fetch-side
// truth is unconditional, DL-053's split), and delivered_revision stays
// unadvanced (the reconcile sweep re-notifies from the durable gap). Driven
// directly through router.Route (the arm's log-and-continue drain is covered in
// the T7b ingest unit test), over the REAL store adapters.
func TestForgeNotifyNoLiveSessionIsNonFatal(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background() // test root
	const (
		repo   = "a/b"
		number = uint64(7)
		url    = "https://github.com/a/b/issues/7#c1"
	)
	agent, subID := seedNotifySubscription(t, st, repo, store.ForgeArtifactKindIssue, number)

	notifyStore := &forgeNotifyStore{st: st, provider: store.ForgeProviderGitHub, host: forgeTestHost}
	disp := &recordingDispatcher{noSession: true}
	forgeRef := &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: forgeTestHost}
	router := ingest.NewNotifyRouter(notifyStore, disp, fixedChecksRoller{}, nil, nil, forgeRef, nil)

	// The route must NOT fail: a per-subscriber dispatch error is logged and
	// skipped, never propagated (design.md:233-243).
	if err := router.Route(ctx, notifyCommentEvent(repo, number, url)); err != nil {
		t.Fatalf("Route returned an error on a no-session dispatch, want nil (non-fatal): %v", err)
	}

	// The fetch cursor advanced despite the failed dispatch (fetch-side truth is
	// unconditional).
	cur, err := st.LoadForgeArtifactCursor(ctx, store.ForgeProviderGitHub, forgeTestHost, repo, store.ForgeArtifactKindIssue, number)
	if err != nil {
		t.Fatalf("LoadForgeArtifactCursor: %v", err)
	}
	if cur == nil || cur.Revision == "" {
		t.Fatal("fetch cursor did not advance after a no-session route, want an advanced row")
	}

	// delivered_revision stays unadvanced (W3 — the ack arm owns it, and the
	// dispatch never landed anyway).
	if got := deliveredRevision(t, st, agent, subID); got != "" {
		t.Fatalf("delivered_revision = %q after a no-session route, want empty", got)
	}

	// The dispatcher was consulted (the router did attempt the notify) but
	// recorded nothing (the sentinel path).
	if len(disp.sent) != 0 {
		t.Fatalf("recorded notifications = %d after no-session, want 0", len(disp.sent))
	}
}

// --- test: Linear routed OPENED fans out to the matching project only ---------

// seedLinearContainerSub creates an agent + owning user and a Linear
// container-scope subscription at (LINEAR, "linear.app", repo/team, ISSUE) bound
// to project, returning the agent account id and subscription id. A Linear
// container subscription REQUIRES a project (store enforces it,
// forge_subscriptions.go:108-111).
func seedLinearContainerSub(t *testing.T, st *store.Store, handle, repo, project string) (store.AccountID, string) {
	t.Helper()
	ctx := context.Background() // test root
	owner, err := st.CreateUser(ctx, store.NewUser{Handle: handle + "-owner", DisplayName: "Owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: handle, DisplayName: "Agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	subID, err := st.EnsureAgentForgeSubscription(ctx, store.AgentForgeSubscription{
		AgentAccountID: agent.ID,
		Provider:       store.ForgeProviderLinear,
		Host:           "linear.app",
		Repo:           repo,
		Kind:           store.ForgeArtifactKindIssue,
		Scope:          store.ForgeSubscriptionScopeContainer,
		Project:        project,
	})
	if err != nil {
		t.Fatalf("EnsureAgentForgeSubscription: %v", err)
	}
	return agent.ID, subID
}

// linearOpenedEvent builds a Linear issue-OPENED ForgeEvent at the LINEAR
// coordinate carrying a project — the container-fan-out trigger.
func linearOpenedEvent(repo string, number uint64, project, url string) forge.ForgeEvent {
	return forge.ForgeEvent{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR,
		Host:     "linear.app",
		Repo:     repo,
		Kind:     compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE,
		Number:   number,
		Project:  project,
		URL:      url,
		Change:   compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_OPENED,
	}
}

// TestLinearNotifyRoutedOpenedFansOutToProject drives the Linear notify lane's
// assembled router over the REAL store adapters bound to (LINEAR, "linear.app"):
// an OPENED Issue in project-alpha fans out to ONLY the alpha container
// subscriber, never the beta one (W2 / DL-267: a Linear container is a PROJECT,
// so an OPENED matches only its project's subscribers). The dispatcher + checks
// roller are fakes; the store adapters + router are the real assembled seams. The
// shared FETCH cursor advances (fetch-side truth); delivered_revision stays
// unadvanced (W3). Runs in CI; compiles locally.
func TestLinearNotifyRoutedOpenedFansOutToProject(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background() // test root
	const (
		repo   = "RIG"
		host   = "linear.app"
		number = uint64(42)
		alpha  = "proj-alpha"
		beta   = "proj-beta"
		url    = "https://linear.app/rig/issue/RIG-42"
	)
	alphaAgent, alphaSub := seedLinearContainerSub(t, st, "lin-alpha", repo, alpha)
	_, betaSub := seedLinearContainerSub(t, st, "lin-beta", repo, beta)

	notifyStore := &forgeNotifyStore{st: st, provider: store.ForgeProviderLinear, host: host}
	disp := &recordingDispatcher{}
	forgeRef := &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR, Host: host}
	router := ingest.NewNotifyRouter(notifyStore, disp, fixedChecksRoller{}, nil, nil, forgeRef, nil)

	if err := router.Route(ctx, linearOpenedEvent(repo, number, alpha, url)); err != nil {
		t.Fatalf("Route: %v", err)
	}

	// Exactly the alpha subscriber is notified — never beta.
	if len(disp.sent) != 1 {
		t.Fatalf("dispatched notifications = %d, want 1 (alpha only)", len(disp.sent))
	}
	if disp.accounts[0] != string(alphaAgent) {
		t.Errorf("notified account = %q, want the alpha agent %q", disp.accounts[0], alphaAgent)
	}
	n := disp.sent[0]
	if n.GetSubscriptionId() != alphaSub {
		t.Errorf("notification subscription_id = %q, want alpha %q (not beta %q)", n.GetSubscriptionId(), alphaSub, betaSub)
	}
	if n.GetForge().GetProvider() != compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR {
		t.Errorf("notification provider = %v, want LINEAR", n.GetForge().GetProvider())
	}
	if n.GetChange() != compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_OPENED {
		t.Errorf("notification change = %v, want OPENED", n.GetChange())
	}

	// The shared FETCH cursor advanced at the container coordinate (number=0 for
	// a container OPENED is NOT how the router keys it — the event carries the
	// artifact number, so the cursor lands at (repo, ISSUE, number)).
	cur, err := st.LoadForgeArtifactCursor(ctx, store.ForgeProviderLinear, host, repo, store.ForgeArtifactKindIssue, number)
	if err != nil {
		t.Fatalf("LoadForgeArtifactCursor: %v", err)
	}
	if cur == nil || cur.Revision == "" {
		t.Fatal("fetch cursor did not advance after the Linear OPENED route")
	}
}

// --- test: suppress-path delivery-cursor advance over the real store adapter --

// scriptedIdentityResolver is the ingest.IdentityResolver fake for the T2 store
// pgtests: it resolves each seeded account id to its owner-qualified handle and a
// single author handle for OPENED. It lets a self-comment event's actor match the
// subscriber so the suppress path fires against the real store adapter.
type scriptedIdentityResolver struct {
	accounts map[string]ingest.Handle
	author   ingest.Handle
}

func (r *scriptedIdentityResolver) HandleForAccount(_ context.Context, accountID string) (ingest.Handle, error) {
	return r.accounts[accountID], nil
}

func (r *scriptedIdentityResolver) AuthorHandle(_ context.Context, _ string, _ compassv1internal.ForgeArtifactKind, _ uint64) (ingest.Handle, error) {
	return r.author, nil
}

// selfCommentEvent builds a GitHub issue-comment event whose Compass commenter is
// owner-qualified (owner/agent), so the router resolves an actor handle that can
// match a subscriber.
func selfCommentEvent(repo string, number uint64, url, owner, agent string) forge.ForgeEvent {
	ev := notifyCommentEvent(repo, number, url)
	ev.Comment.Agent = &compassv1.AgentAttribution{AgentHandle: agent, OwnerHandle: owner}
	return ev
}

// TestForgeNotifySuppressAdvancesCaughtUpCursor drives the assembled router over
// the REAL store adapter: a self-comment from the subscribing agent is suppressed
// (no dispatch) AND, because the subscriber is caught up to the prior cursor
// revision, its delivered_revision advances to the route revision through the
// CAS. A first delivered comment establishes the caught-up state (its ack is
// simulated by advancing delivered_revision to the cursor revision); the second,
// self-authored comment is the suppressed one.
func TestForgeNotifySuppressAdvancesCaughtUpCursor(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background() // test root
	const (
		repo   = "a/b"
		number = uint64(42)
		owner  = "own"
		agent  = "atlas"
	)
	agentID, subID := seedNotifySubscription(t, st, repo, store.ForgeArtifactKindIssue, number)

	notifyStore := &forgeNotifyStore{st: st, provider: store.ForgeProviderGitHub, host: forgeTestHost}
	disp := &recordingDispatcher{}
	forgeRef := &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: forgeTestHost}
	ids := &scriptedIdentityResolver{accounts: map[string]ingest.Handle{string(agentID): {Owner: owner, Agent: agent}}}
	router := ingest.NewNotifyRouter(notifyStore, disp, fixedChecksRoller{}, nil, ids, forgeRef, nil)

	// A human comment first: delivered, advancing the shared cursor. Simulate the
	// agent's ack so its delivered_revision catches up to the cursor revision.
	if err := router.Route(ctx, notifyCommentEvent(repo, number, "https://github.com/a/b/issues/42#c1")); err != nil {
		t.Fatalf("Route (human comment): %v", err)
	}
	cur, err := st.LoadForgeArtifactCursor(ctx, store.ForgeProviderGitHub, forgeTestHost, repo, store.ForgeArtifactKindIssue, number)
	if err != nil || cur == nil {
		t.Fatalf("LoadForgeArtifactCursor: %v (cur=%v)", err, cur)
	}
	if err := st.AdvanceForgeDeliveredRevision(ctx, agentID, subID, cur.Revision); err != nil {
		t.Fatalf("simulate ack: %v", err)
	}
	if got := deliveredRevision(t, st, agentID, subID); got != cur.Revision {
		t.Fatalf("precondition: delivered_revision = %q, want the caught-up cursor revision %q", got, cur.Revision)
	}

	// The self-comment: suppressed, and the caught-up subscriber advances.
	if err := router.Route(ctx, selfCommentEvent(repo, number, "https://github.com/a/b/issues/42#c2", owner, agent)); err != nil {
		t.Fatalf("Route (self comment): %v", err)
	}
	// No new dispatch for the self-comment (only the first human comment).
	if len(disp.sent) != 1 {
		t.Fatalf("dispatched notifications = %d, want 1 (the self-comment is suppressed)", len(disp.sent))
	}
	after, err := st.LoadForgeArtifactCursor(ctx, store.ForgeProviderGitHub, forgeTestHost, repo, store.ForgeArtifactKindIssue, number)
	if err != nil || after == nil {
		t.Fatalf("LoadForgeArtifactCursor (post): %v (cur=%v)", err, after)
	}
	if got := deliveredRevision(t, st, agentID, subID); got != after.Revision {
		t.Fatalf("delivered_revision = %q after suppress, want the advanced route revision %q", got, after.Revision)
	}
}

// TestForgeNotifySuppressDoesNotAdvanceTrailingCursor is the forward-masking
// guard over the real store: a self-comment is suppressed, but the subscriber
// TRAILS the prior cursor revision (an undelivered earlier event), so its
// delivered_revision does NOT advance — the CAS gate leaves the gap for the
// sweep. The subscriber never acked the first comment, so it stays trailing.
func TestForgeNotifySuppressDoesNotAdvanceTrailingCursor(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background() // test root
	const (
		repo   = "a/b"
		number = uint64(43)
		owner  = "own"
		agent  = "atlas"
	)
	agentID, subID := seedNotifySubscription(t, st, repo, store.ForgeArtifactKindIssue, number)

	notifyStore := &forgeNotifyStore{st: st, provider: store.ForgeProviderGitHub, host: forgeTestHost}
	disp := &recordingDispatcher{}
	forgeRef := &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: forgeTestHost}
	ids := &scriptedIdentityResolver{accounts: map[string]ingest.Handle{string(agentID): {Owner: owner, Agent: agent}}}
	router := ingest.NewNotifyRouter(notifyStore, disp, fixedChecksRoller{}, nil, ids, forgeRef, nil)

	// A human comment: delivered, cursor advances — but the agent NEVER acks, so
	// delivered_revision stays "" while the cursor moved ahead (the trailing gap).
	if err := router.Route(ctx, notifyCommentEvent(repo, number, "https://github.com/a/b/issues/43#c1")); err != nil {
		t.Fatalf("Route (human comment): %v", err)
	}
	if got := deliveredRevision(t, st, agentID, subID); got != "" {
		t.Fatalf("precondition: delivered_revision = %q, want empty (trailing, unacked)", got)
	}

	// The self-comment: suppressed, but the trailing subscriber must NOT advance.
	if err := router.Route(ctx, selfCommentEvent(repo, number, "https://github.com/a/b/issues/43#c2", owner, agent)); err != nil {
		t.Fatalf("Route (self comment): %v", err)
	}
	if got := deliveredRevision(t, st, agentID, subID); got != "" {
		t.Fatalf("delivered_revision = %q after suppress, want empty (trailing sub NOT advanced — the E1 gap survives for the sweep)", got)
	}
}

// TestForgeNotifySuppressNeverAdvancesContainerCursor is the scope carve-out over
// the real store: a self-authored OPENED to a CONTAINER-scope subscriber is
// suppressed but its container delivery cursor is NEVER advanced (an artifact
// revision in the container row would poison the container sweep). The container
// sub is caught up ("" == "") so only the scope check prevents the advance.
func TestForgeNotifySuppressNeverAdvancesContainerCursor(t *testing.T) {
	st := forgeTestStore(t)
	ctx := context.Background() // test root
	const (
		repo    = "RIG"
		host    = "linear.app"
		number  = uint64(77)
		project = "proj-A"
		owner   = "own"
		agent   = "atlas"
		url     = "https://linear.app/rig/issue/RIG-77"
	)
	agentID, subID := seedLinearContainerSub(t, st, "lin-self", repo, project)

	notifyStore := &forgeNotifyStore{st: st, provider: store.ForgeProviderLinear, host: host}
	disp := &recordingDispatcher{}
	forgeRef := &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR, Host: host}
	ids := &scriptedIdentityResolver{
		accounts: map[string]ingest.Handle{string(agentID): {Owner: owner, Agent: agent}},
		author:   ingest.Handle{Owner: owner, Agent: agent}, // the OPENED author IS the subscriber.
	}
	router := ingest.NewNotifyRouter(notifyStore, disp, fixedChecksRoller{}, nil, ids, forgeRef, nil)

	if err := router.Route(ctx, linearOpenedEvent(repo, number, project, url)); err != nil {
		t.Fatalf("Route (self OPENED): %v", err)
	}
	// Suppressed: no dispatch.
	if len(disp.sent) != 0 {
		t.Fatalf("dispatched notifications = %d, want 0 (self-opened suppressed)", len(disp.sent))
	}
	// The container subscriber's delivery cursor is untouched (never advanced).
	if got := deliveredRevisionAt(t, st, store.ForgeProviderLinear, host, subID); got != "" {
		t.Fatalf("container delivered_revision = %q after suppress, want empty (NEVER advanced — poisons the container sweep)", got)
	}
}
