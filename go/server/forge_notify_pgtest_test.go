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
//   - App-gated: buildForgeNotifyLane with no App configured returns a nil lane.
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

// --- test: App-config-absent boot leaves the notify lane off ------------------

// TestForgeNotifyLaneDisabledWithoutApp proves the App gate: buildForgeNotifyLane
// with no App configured returns (nil, nil) — the notify lane is hard-off, so
// the caller composes no notify sink onto the ingress. No hub is needed on this
// path (the gate short-circuits before any assembly), so a nil hub is passed.
func TestForgeNotifyLaneDisabledWithoutApp(t *testing.T) {
	st := forgeTestStore(t)

	lane, err := buildForgeNotifyLane(ServeConfig{Forge: ForgeConfig{Host: forgeTestHost}}, st, nil, &fakeResolver{}, nil)
	if err != nil {
		t.Fatalf("buildForgeNotifyLane (App absent): %v", err)
	}
	if lane != nil {
		t.Fatal("lane != nil with no App configured, want nil (notify lane hard-off)")
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
	router := ingest.NewNotifyRouter(notifyStore, disp, fixedChecksRoller{}, forgeRef, nil)
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
	router := ingest.NewNotifyRouter(notifyStore, disp, fixedChecksRoller{}, forgeRef, nil)

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
