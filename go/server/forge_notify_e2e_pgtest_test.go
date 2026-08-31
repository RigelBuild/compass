//go:build pgtest && unix

package server

// RIG-2848 mounted, whole-wire forge-notification e2e: a fake forge emits a
// SIGNED webhook into the Server's mounted /webhooks/{github,linear} ingress and
// the event flows through the REAL notify pipeline onto a subscribed, live agent
// session, observed as a ForgeNotification control frame on the fake runner's
// recorded wire. No seam fakes sit on the delivery path — the fakes are only at
// the forge network boundary (the signed-webhook senders in
// forge_webhook_fakes_test.go, reused verbatim) and the container engine (the
// recordingRunner over the mounted RunnerService door).
//
// The wire stood up here, inline, is production's (serve.go's
// buildForgeNotifyLane / buildLinearNotifyLane, minus the App-gated client):
//
//	fake forge (signed webhook)
//	  -> POST /webhooks/{github,linear}  (NewGitHubWebhookHandler / NewLinearWebhookHandler)
//	     verify signature + parse -> ForgeEventSink.Enqueue
//	  -> ingest.NotifyWebhookArm (Run drains queue -> router.Route)
//	  -> ingest.NotifyRouter (real: NewNotifyRouter over the store-backed adapter)
//	  -> forgeNotifyDispatcher{hub}.Notify -> hub.SessionForAccount -> hub.DispatchControl
//	  -> recordingRunner observes SessionsResponse_DeliverControl carrying the
//	     AgentControl.ForgeNotification
//
// PACKAGE server: the store-backed notify adapters — forgeNotifyStore and
// forgeNotifyDispatcher — are UNEXPORTED server-package types, and only this
// package can construct them directly (the App-gated buildForgeNotifyLane needs a
// GitHub App token source / real client for its checks roller + reconciler,
// which this test-only wire has no business standing up). So the whole-wire
// notify e2e lives in package server and REUSES the placement harness's
// attachFakeRunner + recordingRunner (service_placement_pgtest_test.go) and the
// landed webhook fakes + scripted checks roller (forge_notify_matrix_test.go).
//
// REAL vs FAKED — real: the Postgres store.Store (subscription rows +
// artifact-cursor upserts), the runnerhub.Hub (account->session binding at
// Provision+Start's promoteSession, and the send-only DispatchControl), the
// mounted ingress handlers (signature verify + parse), the notify arm + router,
// and the hub dispatch. Faked: the forge network boundary (signed-webhook
// senders) and the container engine (recordingRunner).
//
// FORK-1 (Linear COMMENT): #768 landed the cross-producer unit regression
// TestCrossProducerLinearCommentNoPhantomDiff, which OWNS the phantom-diff proof
// (webhook vs sweep produce a byte-identical SnapshotComment). Here, at the e2e
// tier, we assert the Linear COMMENT is DELIVERED end to end by its stable
// comment key (CommentRef.CommentKey) onto the subscriber's live session; wiring
// a second reconcile-sweep pass of the identical state through this fixture to
// re-prove the no-phantom invariant would duplicate that landed unit regression
// for no added e2e coverage (the delivery wire, not the digest equality, is what
// this tier exercises), so the phantom-diff proof stays with the unit regression.
//
// EVENT-GATING: dispatch is async (the arm drains on a goroutine, DispatchControl
// is send-only), so every assertion polls the recorded wire under a bounded
// deadline with runtime.Gosched() between polls (the attachFakeRunner gate shape,
// service_placement_pgtest_test.go:597-612) — never a time.Sleep. A negative
// (out-of-scope, or documented-gap) assertion gates on the POSITIVE in-scope
// frame arriving first, THEN reads the negative slice: because the arm drains one
// event at a time on a single goroutine, an in-scope frame arriving after the
// same-provider event that should NOT deliver proves that earlier event was
// routed to completion and produced nothing.
//
// The Linear webhook fakes carry no webhookTimestamp (the notify path never reads
// it), so the mounted Linear handler's freshness gate is pinned to the epoch so a
// zero timestamp reads as fresh — the same now-injection the Linear handler unit
// tests use (linear_webhook_test.go).
//
// context.Background() is the test root (the _test.go thread-context exemption,
// rule://go-thread-context): the one root ctx threads into every store / hub /
// arm call below, and the arms' Run loops are bounded by a child ctx cancelled at
// cleanup.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/ingest"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// notifyE2EHost / notifyE2ERepo are the GitHub coordinate every GitHub cell
// binds; notifyE2ETeam / notifyE2EProject are the Linear team key (RIG, never
// SEA) + project id its cells bind. The fakes and the subscription rows must
// agree on these for a fan-out to resolve.
const (
	notifyE2EGitHubRepo = "octo/repo"
	notifyE2ELinearTeam = "RIG"
	notifyE2EProject    = "proj-alpha"
)

// notifyE2EWire is the mounted forge-notify wire: a real store + real hub with a
// recordingRunner door, and the GitHub + Linear notify lanes (store-backed
// adapter -> router -> arm) mounted behind the real webhook handlers on one
// httptest server.
type notifyE2EWire struct {
	ctx     context.Context
	store   *store.Store
	hub     *runnerhub.Hub
	runner  *recordingRunner
	srv     *httptest.Server
	secret  []byte
	adminID store.AccountID
}

// newNotifyE2EWire stands up the store + hub + fake Runner door, builds the
// GitHub and Linear notify lanes directly over the unexported forgeNotifyStore +
// forgeNotifyDispatcher (NOT the App-gated buildForgeNotifyLane), and mounts the
// real webhook handlers on a test server. Each lane's arm Run loop is bounded by
// a child ctx cancelled at cleanup.
func newNotifyE2EWire(t *testing.T) *notifyE2EWire {
	t.Helper()
	ctx := context.Background() // the test root context (rule://go-thread-context _test.go exemption)
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)

	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	// The Runner's bearer, resolvable through the production resolver, so the
	// mounted door authenticates the fake Runner exactly as a real one.
	if err := st.PutTokenHash(ctx, sha256.Sum256([]byte(fakeRunnerToken)),
		store.Subject{Kind: store.SubjectRunner, ID: fakeRunnerID}); err != nil {
		t.Fatalf("PutTokenHash(runner): %v", err)
	}

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	brd := board.NewProjection(bus)
	tail := newSessionTail()
	hub := newRunnerHub(st, brd, tail, nil, slog.New(slog.DiscardHandler))
	runner := attachFakeRunner(t, st, hub, false)

	secret := []byte("notify-e2e-webhook-secret")
	log := slog.New(slog.DiscardHandler)
	secretFn := func(context.Context) ([]byte, error) { return secret, nil }

	// The GitHub notify lane, assembled directly over the store adapter (the
	// production seam serve.go's buildForgeNotifyLane wires, minus the App client).
	ghRouter := ingest.NewNotifyRouter(
		&forgeNotifyStore{st: st, provider: store.ForgeProviderGitHub, host: "github.com"},
		&forgeNotifyDispatcher{hub: hub},
		&matrixChecksRoller{}, // the CHECKS cell never reaches the roller (Number==0 rejected upstream), so a trivially-scripted roller is correct here.
		mxRef(),
		log,
	)
	ghArm := ingest.NewNotifyWebhookArm(ghRouter, ingest.NotifyArmConfig{Log: log})

	// The Linear notify lane, its Linear-provider-bound sibling.
	lnRouter := ingest.NewNotifyRouter(
		&forgeNotifyStore{st: st, provider: store.ForgeProviderLinear, host: "linear.app"},
		&forgeNotifyDispatcher{hub: hub},
		&matrixChecksRoller{},
		&compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR, Host: "linear.app"},
		log,
	)
	lnArm := ingest.NewNotifyWebhookArm(lnRouter, ingest.NotifyArmConfig{Log: log})

	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() {
		// Run returns nil on ctx cancel; a per-event route error is logged and
		// the drain continues (the arm's own contract, notify_webhook.go:93-108).
		_ = ghArm.Run(runCtx)
	}()
	go func() {
		_ = lnArm.Run(runCtx)
	}()

	mux := http.NewServeMux()
	ghPath, ghHandler := NewGitHubWebhookHandler(secretFn, ghArm, log)
	mux.Handle(ghPath, ghHandler)
	lnPath, lnHandler := NewLinearWebhookHandler(secretFn, lnArm, nil, log)
	// Pin the Linear freshness clock to the epoch: the notify fakes carry no
	// webhookTimestamp (the notify path never reads it), so a zero timestamp must
	// read as fresh — the same now-injection the Linear handler unit tests use.
	lnHandler.(*linearWebhookHandler).now = func() time.Time { return time.UnixMilli(0) }
	mux.Handle(lnPath, lnHandler)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &notifyE2EWire{
		ctx:     ctx,
		store:   st,
		hub:     hub,
		runner:  runner,
		srv:     srv,
		secret:  secret,
		adminID: admin.ID,
	}
}

// seedAgent creates an agent account under the admin owner — the subscriber whose
// account a subscription row names and whose live session the notification lands
// on.
func (w *notifyE2EWire) seedAgent(t *testing.T, handle string) store.AccountID {
	t.Helper()
	a, err := w.store.CreateAgent(w.ctx, w.adminID, store.NewAgent{Handle: handle, DisplayName: handle})
	if err != nil {
		t.Fatalf("CreateAgent(%q): %v", handle, err)
	}
	return a.ID
}

// subscribe inserts one subscription row and returns its id. The row is what
// makes the subscriber resolve at SubscribersForArtifact — a real store row on
// the real fan-out query, not a seam fake.
func (w *notifyE2EWire) subscribe(t *testing.T, sub store.AgentForgeSubscription) string {
	t.Helper()
	id, err := w.store.EnsureAgentForgeSubscription(w.ctx, sub)
	if err != nil {
		t.Fatalf("EnsureAgentForgeSubscription: %v", err)
	}
	return id
}

// goLive provisions + starts a session for account so its account->session
// binding is recorded (promoteSession) and SessionForAccount resolves — the
// precondition for the dispatcher to reach a live session. It uses the runner's
// FIFO overrides so distinct subscribers get distinct containers/sessions.
func (w *notifyE2EWire) goLive(t *testing.T, account store.AccountID, container, session string) {
	t.Helper()
	w.runner.setContainerNames(container)
	w.runner.setStartIDs(session)
	presp, _, err := w.hub.Provision(w.ctx, "", &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: string(account)})
	if err != nil {
		t.Fatalf("Provision(%s): %v", account, err)
	}
	sresp, err := w.hub.Start(w.ctx, "", &compassv1.StartAgentSessionRequest{ContainerName: presp.GetContainerName()})
	if err != nil {
		t.Fatalf("Start(%s): %v", account, err)
	}
	if got := sresp.GetSessionId(); got != session {
		t.Fatalf("Start session id = %q, want %q", got, session)
	}
	// The account->session binding must resolve, or the dispatcher would fall to
	// errNoLiveSession and drop the notification.
	if got, ok := w.hub.SessionForAccount(account); !ok || got != session {
		t.Fatalf("SessionForAccount(%s) = (%q, %v), want (%q, true)", account, got, ok, session)
	}
}

// postGitHub drives one fake signed GitHub webhook through the mounted ingress
// and asserts the ack, so a signature/parse regression reddens at the POST.
func (w *notifyE2EWire) postGitHub(t *testing.T, sw signedWebhook) {
	t.Helper()
	req, err := http.NewRequestWithContext(w.ctx, http.MethodPost, w.srv.URL+githubWebhookPath, bytes.NewReader(sw.body))
	if err != nil {
		t.Fatalf("build github request: %v", err)
	}
	req.Header.Set(githubEventHeader, sw.event)
	req.Header.Set(githubDeliveryHeader, sw.delivery)
	req.Header.Set(githubSignatureHeader, sw.sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /webhooks/github: %v", err)
	}
	_ = resp.Body.Close() // ack body is empty; drain-and-close, nothing actionable.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("github ingress = %d, want 200 (signature verify + parse accept)", resp.StatusCode)
	}
}

// postLinear drives one fake signed Linear webhook through the mounted ingress
// and asserts the ack.
func (w *notifyE2EWire) postLinear(t *testing.T, sw signedLinearWebhook) {
	t.Helper()
	req, err := http.NewRequestWithContext(w.ctx, http.MethodPost, w.srv.URL+linearWebhookPath, bytes.NewReader(sw.body))
	if err != nil {
		t.Fatalf("build linear request: %v", err)
	}
	req.Header.Set(linearSignatureHeader, sw.sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /webhooks/linear: %v", err)
	}
	_ = resp.Body.Close() // ack body is empty; drain-and-close, nothing actionable.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("linear ingress = %d, want 200 (signature verify + parse accept)", resp.StatusCode)
	}
}

// forgeNotifications returns every ForgeNotification pushed to the runner as a
// send-only DeliverControl, in order. The e2e's observable "the notification
// reached the subscriber's live session" is exactly this recorded wire frame.
func (r *recordingRunner) forgeNotifications() []*compassv1internal.ForgeNotification {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*compassv1internal.ForgeNotification
	for _, c := range r.seen {
		if dc := c.GetDeliverControl(); dc != nil {
			if fn := dc.GetOp().GetForgeNotification(); fn != nil {
				out = append(out, fn)
			}
		}
	}
	return out
}

// forgeNotificationsForSession filters the recorded ForgeNotification frames to
// the ones dispatched at sessionID — the per-subscriber slice a scope assertion
// reads to prove an out-of-scope session got zero frames.
func (r *recordingRunner) forgeNotificationsForSession(sessionID string) []*compassv1internal.ForgeNotification {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*compassv1internal.ForgeNotification
	for _, c := range r.seen {
		if dc := c.GetDeliverControl(); dc != nil && dc.GetSessionId() == sessionID {
			if fn := dc.GetOp().GetForgeNotification(); fn != nil {
				out = append(out, fn)
			}
		}
	}
	return out
}

// waitForForgeNotification event-gates until at least one ForgeNotification is
// recorded on sessionID, then returns that session's slice — never a sleep.
// Bounded by testTimeout so a wedged arm/dispatch fails fast.
func waitForForgeNotification(t *testing.T, r *recordingRunner, sessionID string) []*compassv1internal.ForgeNotification {
	t.Helper()
	deadline := timeAfter()
	for {
		got := r.forgeNotificationsForSession(sessionID)
		if len(got) >= 1 {
			return got
		}
		select {
		case <-deadline:
			t.Fatalf("no forge notification on session %q within %s (total frames seen: %d)", sessionID, testTimeout, len(r.forgeNotifications()))
		default:
		}
		runtime.Gosched()
	}
}

// --- GitHub cells ------------------------------------------------------------

func TestForgeNotifyE2E_GitHubComment(t *testing.T) {
	const (
		inSession  = "sess-gh-comment-in"
		outSession = "sess-gh-comment-out"
	)
	t.Run("issue comment (COMMENT, kind=ISSUE)", func(t *testing.T) {
		w := newNotifyE2EWire(t)
		gh := newFakeGitHubForge(w.secret, notifyE2EGitHubRepo)

		inAcct := w.seedAgent(t, "sub-in")
		outAcct := w.seedAgent(t, "sub-out")
		w.subscribe(t, store.AgentForgeSubscription{
			AgentAccountID: inAcct, Provider: store.ForgeProviderGitHub, Host: "github.com",
			Repo: notifyE2EGitHubRepo, Kind: store.ForgeArtifactKindIssue, Number: 11,
			Scope: store.ForgeSubscriptionScopeArtifact,
		})
		// Out-of-scope: same repo, a DIFFERENT issue number — must get nothing.
		w.subscribe(t, store.AgentForgeSubscription{
			AgentAccountID: outAcct, Provider: store.ForgeProviderGitHub, Host: "github.com",
			Repo: notifyE2EGitHubRepo, Kind: store.ForgeArtifactKindIssue, Number: 99,
			Scope: store.ForgeSubscriptionScopeArtifact,
		})
		w.goLive(t, inAcct, "compass-agent-gh-comment-in", inSession)
		w.goLive(t, outAcct, "compass-agent-gh-comment-out", outSession)
		w.runner.forget() // drop the provision/start noise so only handler-driven frames remain

		w.postGitHub(t, gh.commentOnIssue(t, 11, "https://gh/octo/repo/issues/11#c1", "hi there", "octocat"))

		got := waitForForgeNotification(t, w.runner, inSession)
		if len(got) != 1 {
			t.Fatalf("in-scope frames = %d, want 1 (an issue comment fans out to exactly its artifact subscriber)", len(got))
		}
		n := got[0]
		if n.GetChange() != mxComment {
			t.Errorf("Change = %v, want COMMENT (a normalize/route regression would carry the wrong kind)", n.GetChange())
		}
		if n.GetKind() != mxIssue {
			t.Errorf("Kind = %v, want ISSUE (the issue/PR discriminator flipped)", n.GetKind())
		}
		if n.GetRepo() != notifyE2EGitHubRepo || n.GetNumber() != 11 {
			t.Errorf("coordinate = %s#%d, want %s#11 (the coordinate must survive the wire)", n.GetRepo(), n.GetNumber(), notifyE2EGitHubRepo)
		}
		if n.GetComment().GetForgeAccount() != "octocat" {
			t.Errorf("Comment.ForgeAccount = %q, want octocat (the commenter attribution dropped)", n.GetComment().GetForgeAccount())
		}
		if n.GetComment().GetCommentKey() == "" {
			t.Error("Comment.CommentKey = empty, want the stable comment key (snapshot keying lost)")
		}
		// The out-of-scope subscriber's session got nothing: gate satisfied above,
		// so the router ran to completion for this event.
		if out := w.runner.forgeNotificationsForSession(outSession); len(out) != 0 {
			t.Errorf("out-of-scope frames = %d, want 0 (a different-number subscriber must not receive)", len(out))
		}
	})

	t.Run("PR conversation comment (COMMENT, kind=PR)", func(t *testing.T) {
		w := newNotifyE2EWire(t)
		gh := newFakeGitHubForge(w.secret, notifyE2EGitHubRepo)
		inAcct := w.seedAgent(t, "sub-in")
		w.subscribe(t, store.AgentForgeSubscription{
			AgentAccountID: inAcct, Provider: store.ForgeProviderGitHub, Host: "github.com",
			Repo: notifyE2EGitHubRepo, Kind: store.ForgeArtifactKindPullRequest, Number: 12,
			Scope: store.ForgeSubscriptionScopeArtifact,
		})
		w.goLive(t, inAcct, "compass-agent-gh-prcomment", inSession)
		w.runner.forget()

		w.postGitHub(t, gh.commentOnPR(t, 12, "https://gh/octo/repo/pull/12#c2", "looks good", "octocat"))

		got := waitForForgeNotification(t, w.runner, inSession)
		n := got[0]
		if n.GetChange() != mxComment || n.GetKind() != mxPR {
			t.Errorf("(Change, Kind) = (%v, %v), want (COMMENT, PR) (the pull_request marker discriminator dropped)", n.GetChange(), n.GetKind())
		}
		if n.GetNumber() != 12 {
			t.Errorf("Number = %d, want 12", n.GetNumber())
		}
	})

	t.Run("PR review comment (COMMENT, kind=PR)", func(t *testing.T) {
		w := newNotifyE2EWire(t)
		gh := newFakeGitHubForge(w.secret, notifyE2EGitHubRepo)
		inAcct := w.seedAgent(t, "sub-in")
		w.subscribe(t, store.AgentForgeSubscription{
			AgentAccountID: inAcct, Provider: store.ForgeProviderGitHub, Host: "github.com",
			Repo: notifyE2EGitHubRepo, Kind: store.ForgeArtifactKindPullRequest, Number: 12,
			Scope: store.ForgeSubscriptionScopeArtifact,
		})
		w.goLive(t, inAcct, "compass-agent-gh-reviewcomment", inSession)
		w.runner.forget()

		w.postGitHub(t, gh.reviewCommentOnPR(t, 12, "https://gh/octo/repo/pull/12#rc1", "nit here", "octocat"))

		got := waitForForgeNotification(t, w.runner, inSession)
		n := got[0]
		if n.GetChange() != mxComment || n.GetKind() != mxPR {
			t.Errorf("(Change, Kind) = (%v, %v), want (COMMENT, PR) (pull_request_review_comment mis-normalized)", n.GetChange(), n.GetKind())
		}
	})
}

func TestForgeNotifyE2E_GitHubState(t *testing.T) {
	const inSession = "sess-gh-state-in"
	cases := []struct {
		name      string
		kind      store.ForgeArtifactKind
		number    uint64
		send      func(gh *fakeGitHubForge) signedWebhook
		wantState string
	}{
		{
			name: "issue closed (STATE=closed)", kind: store.ForgeArtifactKindIssue, number: 11,
			send:      func(gh *fakeGitHubForge) signedWebhook { return gh.closeIssue(t, 11, "https://gh/octo/repo/issues/11") },
			wantState: "closed",
		},
		{
			name: "PR merged (STATE=merged)", kind: store.ForgeArtifactKindPullRequest, number: 12,
			send:      func(gh *fakeGitHubForge) signedWebhook { return gh.mergePR(t, 12, "https://gh/octo/repo/pull/12") },
			wantState: "merged",
		},
		{
			name: "PR closed unmerged (STATE=closed)", kind: store.ForgeArtifactKindPullRequest, number: 12,
			send:      func(gh *fakeGitHubForge) signedWebhook { return gh.closePR(t, 12, "https://gh/octo/repo/pull/12") },
			wantState: "closed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newNotifyE2EWire(t)
			gh := newFakeGitHubForge(w.secret, notifyE2EGitHubRepo)
			inAcct := w.seedAgent(t, "sub-in")
			w.subscribe(t, store.AgentForgeSubscription{
				AgentAccountID: inAcct, Provider: store.ForgeProviderGitHub, Host: "github.com",
				Repo: notifyE2EGitHubRepo, Kind: tc.kind, Number: tc.number,
				Scope: store.ForgeSubscriptionScopeArtifact,
			})
			w.goLive(t, inAcct, "compass-agent-gh-state", inSession)
			w.runner.forget()

			w.postGitHub(t, tc.send(gh))

			got := waitForForgeNotification(t, w.runner, inSession)
			n := got[0]
			if n.GetChange() != mxState {
				t.Errorf("Change = %v, want STATE (a close/merge action mis-mapped to UPDATE/OPENED)", n.GetChange())
			}
			if n.GetState() != tc.wantState {
				t.Errorf("State = %q, want %q (the forge state string dropped on the wire)", n.GetState(), tc.wantState)
			}
			if n.GetNumber() != tc.number {
				t.Errorf("Number = %d, want %d", n.GetNumber(), tc.number)
			}
		})
	}
}

func TestForgeNotifyE2E_GitHubUpdate(t *testing.T) {
	const inSession = "sess-gh-update-in"
	cases := []struct {
		name   string
		kind   store.ForgeArtifactKind
		number uint64
		send   func(gh *fakeGitHubForge) signedWebhook
	}{
		{
			name: "issue edited (UPDATE)", kind: store.ForgeArtifactKindIssue, number: 11,
			send: func(gh *fakeGitHubForge) signedWebhook { return gh.editIssue(t, 11, "https://gh/octo/repo/issues/11") },
		},
		{
			name: "PR edited (UPDATE)", kind: store.ForgeArtifactKindPullRequest, number: 12,
			send: func(gh *fakeGitHubForge) signedWebhook { return gh.editPR(t, 12, "https://gh/octo/repo/pull/12") },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newNotifyE2EWire(t)
			gh := newFakeGitHubForge(w.secret, notifyE2EGitHubRepo)
			inAcct := w.seedAgent(t, "sub-in")
			w.subscribe(t, store.AgentForgeSubscription{
				AgentAccountID: inAcct, Provider: store.ForgeProviderGitHub, Host: "github.com",
				Repo: notifyE2EGitHubRepo, Kind: tc.kind, Number: tc.number,
				Scope: store.ForgeSubscriptionScopeArtifact,
			})
			w.goLive(t, inAcct, "compass-agent-gh-update", inSession)
			w.runner.forget()

			w.postGitHub(t, tc.send(gh))

			got := waitForForgeNotification(t, w.runner, inSession)
			n := got[0]
			if n.GetChange() != mxUpdate {
				t.Errorf("Change = %v, want UPDATE (an edited action mis-mapped)", n.GetChange())
			}
			if n.GetNumber() != tc.number {
				t.Errorf("Number = %d, want %d", n.GetNumber(), tc.number)
			}
		})
	}
}

// TestForgeNotifyE2E_GitHubCheckSuiteGap asserts the DOCUMENTED GAP: a
// check_suite webhook carries a head SHA but Number==0, and NotifyRouter.Route
// rejects Number==0 (notify_router.go:153) — so NO notification is delivered and
// the arm logs a route error. The head_sha->PR-number resolution is unlanded
// (see TestForgeNotifyMatrix_CheckSuiteZeroNumberRejected). A following, in-scope
// comment on the same GitHub arm is the ordered-drain sync point: the arm drains
// one event at a time on a single goroutine, so the comment frame arriving proves
// the check_suite was already routed to completion and produced nothing.
func TestForgeNotifyE2E_GitHubCheckSuiteGap(t *testing.T) {
	const inSession = "sess-gh-checks-in"
	w := newNotifyE2EWire(t)
	gh := newFakeGitHubForge(w.secret, notifyE2EGitHubRepo)
	inAcct := w.seedAgent(t, "sub-in")
	// A live subscriber on an issue coordinate, so the trailing comment delivers.
	w.subscribe(t, store.AgentForgeSubscription{
		AgentAccountID: inAcct, Provider: store.ForgeProviderGitHub, Host: "github.com",
		Repo: notifyE2EGitHubRepo, Kind: store.ForgeArtifactKindIssue, Number: 11,
		Scope: store.ForgeSubscriptionScopeArtifact,
	})
	w.goLive(t, inAcct, "compass-agent-gh-checks", inSession)
	w.runner.forget()

	// The gap event first, then the delivering event; one arm, one goroutine, FIFO.
	w.postGitHub(t, gh.completeCheckSuite(t, "abc123headsha"))
	w.postGitHub(t, gh.commentOnIssue(t, 11, "https://gh/octo/repo/issues/11#c1", "after checks", "octocat"))

	got := waitForForgeNotification(t, w.runner, inSession)
	if len(got) != 1 {
		t.Fatalf("frames = %d, want exactly 1 (the check_suite must deliver nothing — only the trailing comment)", len(got))
	}
	if got[0].GetChange() != mxComment {
		t.Errorf("Change = %v, want COMMENT (a CHECKS frame leaking would mean the zero-number guard regressed)", got[0].GetChange())
	}
	// Belt-and-suspenders: no CHECKS frame anywhere on the wire.
	for _, n := range w.runner.forgeNotifications() {
		if n.GetChange() == mxChecks {
			t.Errorf("a CHECKS notification was delivered, want none (check_suite Number==0 must be rejected at the router)")
		}
	}
}

// TestForgeNotifyE2E_GitHubRepoContainer covers the REPO container scope: a
// CONTAINER-scope subscriber (Number=0) on the repo receives an OPENED event's
// fan-out, while an out-of-scope subscriber on a different repo container does
// not.
func TestForgeNotifyE2E_GitHubRepoContainer(t *testing.T) {
	const (
		inSession  = "sess-gh-container-in"
		outSession = "sess-gh-container-out"
	)
	w := newNotifyE2EWire(t)
	gh := newFakeGitHubForge(w.secret, notifyE2EGitHubRepo)
	inAcct := w.seedAgent(t, "sub-in")
	outAcct := w.seedAgent(t, "sub-out")
	w.subscribe(t, store.AgentForgeSubscription{
		AgentAccountID: inAcct, Provider: store.ForgeProviderGitHub, Host: "github.com",
		Repo: notifyE2EGitHubRepo, Kind: store.ForgeArtifactKindIssue,
		Scope: store.ForgeSubscriptionScopeContainer, // Number 0 (container)
	})
	// Out-of-scope: a container on a DIFFERENT repo — must not fan in.
	w.subscribe(t, store.AgentForgeSubscription{
		AgentAccountID: outAcct, Provider: store.ForgeProviderGitHub, Host: "github.com",
		Repo: "octo/other", Kind: store.ForgeArtifactKindIssue,
		Scope: store.ForgeSubscriptionScopeContainer,
	})
	w.goLive(t, inAcct, "compass-agent-gh-container-in", inSession)
	w.goLive(t, outAcct, "compass-agent-gh-container-out", outSession)
	w.runner.forget()

	w.postGitHub(t, gh.openIssue(t, 42, "https://gh/octo/repo/issues/42"))

	got := waitForForgeNotification(t, w.runner, inSession)
	if len(got) != 1 {
		t.Fatalf("container frames = %d, want 1 (an OPENED issue fans out to the repo container subscriber)", len(got))
	}
	if got[0].GetChange() != mxOpened {
		t.Errorf("Change = %v, want OPENED (a new-artifact event must fan out as OPENED)", got[0].GetChange())
	}
	if got[0].GetNumber() != 42 {
		t.Errorf("Number = %d, want 42", got[0].GetNumber())
	}
	if out := w.runner.forgeNotificationsForSession(outSession); len(out) != 0 {
		t.Errorf("other-repo container frames = %d, want 0 (a different-repo container must not fan in)", len(out))
	}
}

// --- Linear cells ------------------------------------------------------------

// TestForgeNotifyE2E_LinearComment is the Fork-1 anchor cell: the Linear COMMENT
// is delivered end to end onto the subscriber's live session, identified by its
// stable comment key. The phantom-diff (webhook-vs-sweep) proof stays with the
// landed unit regression TestCrossProducerLinearCommentNoPhantomDiff (#768) — see
// the file header for why the e2e asserts delivery-by-key, not a second sweep.
func TestForgeNotifyE2E_LinearComment(t *testing.T) {
	const (
		inSession  = "sess-ln-comment-in"
		outSession = "sess-ln-comment-out"
	)
	w := newNotifyE2EWire(t)
	ln := newFakeLinearForge(w.secret, notifyE2ELinearTeam, notifyE2EProject)
	inAcct := w.seedAgent(t, "sub-in")
	outAcct := w.seedAgent(t, "sub-out")
	w.subscribe(t, store.AgentForgeSubscription{
		AgentAccountID: inAcct, Provider: store.ForgeProviderLinear, Host: "linear.app",
		Repo: notifyE2ELinearTeam, Kind: store.ForgeArtifactKindIssue, Number: 5,
		Scope: store.ForgeSubscriptionScopeArtifact,
	})
	// Out-of-scope: same team, a different issue number.
	w.subscribe(t, store.AgentForgeSubscription{
		AgentAccountID: outAcct, Provider: store.ForgeProviderLinear, Host: "linear.app",
		Repo: notifyE2ELinearTeam, Kind: store.ForgeArtifactKindIssue, Number: 77,
		Scope: store.ForgeSubscriptionScopeArtifact,
	})
	w.goLive(t, inAcct, "compass-agent-ln-comment-in", inSession)
	w.goLive(t, outAcct, "compass-agent-ln-comment-out", outSession)
	w.runner.forget()

	w.postLinear(t, ln.commentOnIssue(t, 5, "https://linear.app/rigel/RIG-5", "a comment", "matt"))

	got := waitForForgeNotification(t, w.runner, inSession)
	if len(got) != 1 {
		t.Fatalf("in-scope frames = %d, want 1 (a Linear comment fans out to exactly its artifact subscriber)", len(got))
	}
	n := got[0]
	if n.GetChange() != mxComment {
		t.Errorf("Change = %v, want COMMENT (Comment/create mis-normalized)", n.GetChange())
	}
	if n.GetRepo() != notifyE2ELinearTeam || n.GetNumber() != 5 {
		t.Errorf("coordinate = %s#%d, want %s#5", n.GetRepo(), n.GetNumber(), notifyE2ELinearTeam)
	}
	// The Fork-1 invariant at the e2e tier: the comment is delivered by its
	// stable key (the snapshot-keying identity #768's fix keys on).
	if n.GetComment().GetCommentKey() != "cmt-1" {
		t.Errorf("Comment.CommentKey = %q, want cmt-1 (the stable comment key that keys the snapshot dropped)", n.GetComment().GetCommentKey())
	}
	if n.GetComment().GetForgeAccount() != "matt" {
		t.Errorf("Comment.ForgeAccount = %q, want matt (the commenter attribution dropped)", n.GetComment().GetForgeAccount())
	}
	if out := w.runner.forgeNotificationsForSession(outSession); len(out) != 0 {
		t.Errorf("out-of-scope frames = %d, want 0 (a different-number subscriber must not receive)", len(out))
	}
}

func TestForgeNotifyE2E_LinearState(t *testing.T) {
	const inSession = "sess-ln-state-in"
	t.Run("issue state change (STATE)", func(t *testing.T) {
		w := newNotifyE2EWire(t)
		ln := newFakeLinearForge(w.secret, notifyE2ELinearTeam, notifyE2EProject)
		inAcct := w.seedAgent(t, "sub-in")
		w.subscribe(t, store.AgentForgeSubscription{
			AgentAccountID: inAcct, Provider: store.ForgeProviderLinear, Host: "linear.app",
			Repo: notifyE2ELinearTeam, Kind: store.ForgeArtifactKindIssue, Number: 5,
			Scope: store.ForgeSubscriptionScopeArtifact,
		})
		w.goLive(t, inAcct, "compass-agent-ln-state", inSession)
		w.runner.forget()

		w.postLinear(t, ln.changeIssueState(t, 5, "https://linear.app/rigel/RIG-5", "completed"))

		got := waitForForgeNotification(t, w.runner, inSession)
		n := got[0]
		if n.GetChange() != mxState {
			t.Errorf("Change = %v, want STATE (an Issue/update with a stateId change mis-mapped to UPDATE)", n.GetChange())
		}
		// "completed" is a closed Linear workflow-state type; pin the mapped
		// verdict literally.
		if n.GetState() != "closed" {
			t.Errorf("State = %q, want closed (MapLinearState verdict dropped on the wire)", n.GetState())
		}
	})

	t.Run("issue create (OPENED, artifact subscriber)", func(t *testing.T) {
		w := newNotifyE2EWire(t)
		ln := newFakeLinearForge(w.secret, notifyE2ELinearTeam, notifyE2EProject)
		inAcct := w.seedAgent(t, "sub-in")
		// An OPENED event reaches container-scope subscribers; an artifact-scope
		// subscriber on the same number also matches (opened OR exact-number).
		w.subscribe(t, store.AgentForgeSubscription{
			AgentAccountID: inAcct, Provider: store.ForgeProviderLinear, Host: "linear.app",
			Repo: notifyE2ELinearTeam, Kind: store.ForgeArtifactKindIssue,
			Scope: store.ForgeSubscriptionScopeContainer, Project: notifyE2EProject,
		})
		w.goLive(t, inAcct, "compass-agent-ln-opened", inSession)
		w.runner.forget()

		w.postLinear(t, ln.openIssue(t, 6, "https://linear.app/rigel/RIG-6"))

		got := waitForForgeNotification(t, w.runner, inSession)
		if got[0].GetChange() != mxOpened {
			t.Errorf("Change = %v, want OPENED (Issue/create mis-mapped)", got[0].GetChange())
		}
	})
}

func TestForgeNotifyE2E_LinearUpdate(t *testing.T) {
	const inSession = "sess-ln-update-in"
	w := newNotifyE2EWire(t)
	ln := newFakeLinearForge(w.secret, notifyE2ELinearTeam, notifyE2EProject)
	inAcct := w.seedAgent(t, "sub-in")
	w.subscribe(t, store.AgentForgeSubscription{
		AgentAccountID: inAcct, Provider: store.ForgeProviderLinear, Host: "linear.app",
		Repo: notifyE2ELinearTeam, Kind: store.ForgeArtifactKindIssue, Number: 5,
		Scope: store.ForgeSubscriptionScopeArtifact,
	})
	w.goLive(t, inAcct, "compass-agent-ln-update", inSession)
	w.runner.forget()

	w.postLinear(t, ln.editIssue(t, 5, "https://linear.app/rigel/RIG-5"))

	got := waitForForgeNotification(t, w.runner, inSession)
	if got[0].GetChange() != mxUpdate {
		t.Errorf("Change = %v, want UPDATE (an Issue/update with no stateId change mis-mapped to STATE)", got[0].GetChange())
	}
}

// TestForgeNotifyE2E_LinearProjectContainer covers the Linear PROJECT container
// scope: a CONTAINER-scope subscriber matched by project id receives an OPENED
// event carrying that project, while an out-of-scope subscriber on a different
// project does not.
func TestForgeNotifyE2E_LinearProjectContainer(t *testing.T) {
	const (
		inSession  = "sess-ln-container-in"
		outSession = "sess-ln-container-out"
	)
	w := newNotifyE2EWire(t)
	ln := newFakeLinearForge(w.secret, notifyE2ELinearTeam, notifyE2EProject)
	inAcct := w.seedAgent(t, "sub-in")
	outAcct := w.seedAgent(t, "sub-out")
	w.subscribe(t, store.AgentForgeSubscription{
		AgentAccountID: inAcct, Provider: store.ForgeProviderLinear, Host: "linear.app",
		Repo: notifyE2ELinearTeam, Kind: store.ForgeArtifactKindIssue,
		Scope: store.ForgeSubscriptionScopeContainer, Project: notifyE2EProject,
	})
	// Out-of-scope: a container on a DIFFERENT project — must not match.
	w.subscribe(t, store.AgentForgeSubscription{
		AgentAccountID: outAcct, Provider: store.ForgeProviderLinear, Host: "linear.app",
		Repo: notifyE2ELinearTeam, Kind: store.ForgeArtifactKindIssue,
		Scope: store.ForgeSubscriptionScopeContainer, Project: "proj-beta",
	})
	w.goLive(t, inAcct, "compass-agent-ln-container-in", inSession)
	w.goLive(t, outAcct, "compass-agent-ln-container-out", outSession)
	w.runner.forget()

	w.postLinear(t, ln.openIssue(t, 8, "https://linear.app/rigel/RIG-8"))

	got := waitForForgeNotification(t, w.runner, inSession)
	if len(got) != 1 {
		t.Fatalf("project container frames = %d, want 1 (an OPENED issue fans out to the matching-project container)", len(got))
	}
	if got[0].GetChange() != mxOpened {
		t.Errorf("Change = %v, want OPENED", got[0].GetChange())
	}
	if out := w.runner.forgeNotificationsForSession(outSession); len(out) != 0 {
		t.Errorf("other-project container frames = %d, want 0 (a different-project container must not match)", len(out))
	}
}

// TestForgeNotifyE2E_LinearRemoveDropped asserts an Issue/remove is
// counted-and-dropped (no notification kind models deletion): it produces zero
// frames. A trailing, in-scope comment on the same Linear arm is the ordered-drain
// sync point — its frame arriving proves the remove was fully processed and
// delivered nothing.
func TestForgeNotifyE2E_LinearRemoveDropped(t *testing.T) {
	const inSession = "sess-ln-remove-in"
	w := newNotifyE2EWire(t)
	ln := newFakeLinearForge(w.secret, notifyE2ELinearTeam, notifyE2EProject)
	inAcct := w.seedAgent(t, "sub-in")
	w.subscribe(t, store.AgentForgeSubscription{
		AgentAccountID: inAcct, Provider: store.ForgeProviderLinear, Host: "linear.app",
		Repo: notifyE2ELinearTeam, Kind: store.ForgeArtifactKindIssue, Number: 5,
		Scope: store.ForgeSubscriptionScopeArtifact,
	})
	w.goLive(t, inAcct, "compass-agent-ln-remove", inSession)
	w.runner.forget()

	// The dropped event first, then the delivering comment; one arm, FIFO.
	w.postLinear(t, ln.removeIssue(t, 5, "https://linear.app/rigel/RIG-5"))
	w.postLinear(t, ln.commentOnIssue(t, 5, "https://linear.app/rigel/RIG-5", "after remove", "matt"))

	got := waitForForgeNotification(t, w.runner, inSession)
	if len(got) != 1 {
		t.Fatalf("frames = %d, want exactly 1 (Issue/remove must deliver nothing — only the trailing comment)", len(got))
	}
	if got[0].GetChange() != mxComment {
		t.Errorf("Change = %v, want COMMENT (a frame from the removed issue would mean remove was mis-mapped to a delivering kind)", got[0].GetChange())
	}
}
