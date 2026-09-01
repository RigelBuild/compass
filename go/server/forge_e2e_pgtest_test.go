//go:build pgtest && unix

package server

// End-to-end T8 of the FROZEN Compass forge-write design
// (docs/designs/server/compass-forge-write-path/design.md §T8, acceptance
// :876-885): the WHOLE agent-initiated forge-WRITE wire, driven over a REAL
// per-container AgentGateway unix socket against a real Postgres + a real
// Runner-over-stub-engine, with the forge chokepoint mounted on the
// server-package hub via hub.SetForgeCaller — the exact production seam
// serve.go's buildForgeWriteService wires, but over forge.FakeProvider fakes
// (author + reviewer roles, so F1 dispatch is observable) instead of the real
// GitHub credential. Where forge_test.go drives the forgeService seam DIRECTLY
// (newForgeService, no wire), this drives every hop the record names:
//
//	in-container agent  ->  AgentGateway.Forge (per-container unix socket)
//	  ->  Runner gateway.Forge (maps socket->container->bound session)
//	  ->  RelayForgeCall(session_id, call)  (Runner asserts NO account)
//	  ->  Hub.RelayForgeCall (resolves session_id->caller account, fail-closed)
//	  ->  forgeService.ExecuteForgeCallAsAccount under the resolved caller
//	  ->  StampOwner + F3 dedup + DL-055 row (store) + provider dispatch
//
// PACKAGE PLACEMENT mirrors lifecycle_e2e_pgtest_test.go's option B: the hub
// needs a real ForgeCaller, which is *forgeService — unexported, in package
// server. Only package server can construct it (newForgeService) and wire it
// (hub.SetForgeCaller), so the whole-wire forge test lives in package server and
// REUSES that file's ported whole-wire scaffold (newE2EWire, dialPeer,
// provisionWhenSeamLiveE2E, the stub runtime + socket helpers), mounting the
// forge caller on the wire's hub after construction — the same post-construction
// SetForgeCaller the production serve loop uses. The load-bearing WHY-comments on
// those ported helpers live in lifecycle_e2e_pgtest_test.go; this file adds only
// the forge-specific wiring beside them.
//
// FAKES, NOT GITHUB: the registry is populated with forge.FakeProvider author +
// reviewer roles (and a Linear fake), so the stamp/dedup/dispatch/flattening is
// observable without a network — the wire shape is exercised independently of
// the real *forge.GitHub client the production buildForgeWriteService registers.
// This E2E proves the whole wire over the same registry shape the production
// serve loop assembles, without a live forge.
//
// Each assertion carries a mutation comment: the plausible regression in the
// (already merged, green) T4/T5 spine that would redden it.

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// forgeE2EHost is the coordinate host the FakeProvider registry's default GitHub
// coordinate binds — the same value serve.go's write wiring registers for the
// real GitHub client. A create with an unset ForgeRef resolves this default.
const forgeE2EHost = "github.com"

// forgeE2ERepo is the owner/name repo every GitHub-addressed forge call here uses.
const forgeE2ERepo = "owner/repo"

// forgeE2EWire is the whole-wire forge fixture: the ported spawn/despawn wire
// (a real store + server-package hub + real Runner over a socket-serving stub
// engine + a bound supervisor session) with the forge chokepoint mounted on its
// hub via hub.SetForgeCaller over FakeProvider author/reviewer clients (plus a
// Linear fake). The supervisor's per-container socket is the wire the tests drive
// AgentGateway.Forge over; the supervisor is the resolved CALLER.
type forgeE2EWire struct {
	*e2eWire
	author   *forge.FakeProvider // the GitHub author role every ordinary write dispatches on
	reviewer *forge.FakeProvider // the GitHub reviewer role submit_review dispatches on (F1)
	linear   *forge.FakeProvider // the Linear coordinate (issues-only; PR/review ops scripted ErrUnsupported)
}

// newForgeE2EWire stands up the ported whole-wire fixture and mounts the forge
// caller on its hub over fakes, registering the GitHub coordinate as the default
// (author+reviewer) and a Linear coordinate (issues-only). Mirrors serve.go's
// buildForgeWriteService assembly + hub.SetForgeCaller, but with fakes so the
// stamp/dedup/dispatch is observable without a real forge.
func newForgeE2EWire(t *testing.T) *forgeE2EWire {
	t.Helper()
	w := newE2EWire(t)

	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	// The Linear stand-in: issues-only (DL-051), so its PR/review ops return
	// ErrUnsupported exactly as forge.Linear does — the chokepoint flattens that
	// to in-band unimplemented. One fake serves both roles (Linear has no
	// author/reviewer split), matching serve.go's Linear registration.
	linear := forge.NewFakeProvider("linear")
	linear.SetError("CreatePullRequest", forge.ErrUnsupported)

	reg := newForgeProviderRegistry()
	reg.register(forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, host: forgeE2EHost}, author, reviewer, true)
	reg.register(forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR}, linear, linear, false)

	// The chokepoint reads tracked issues off a store-backed issue projection
	// (its own bus, distinct from the wire's). None of these writes are tracked,
	// so the projection is effectively a create/dispatch sink here — but it is the
	// REAL store-backed one so a DL-055 row read-back rides the same store.
	brdBus := events.NewBus[busPayload]()
	t.Cleanup(brdBus.Close)
	issueBrd := board.NewIssueProjection(brdBus, w.store)

	// The production seam: mount the forge caller after the hub + service both
	// exist (breaking their construction cycle exactly as SetForgeCaller does in
	// serve.go). Before this, RelayForgeCall fail-closes CodeUnavailable.
	w.hub.SetForgeCaller(newForgeService(w.store, issueBrd, reg))

	return &forgeE2EWire{e2eWire: w, author: author, reviewer: reviewer, linear: linear}
}

// TestForgeCreateOverTheWire drives the create arm end-to-end over the socket and
// pins the DL-050 stamp, the DL-055 ownership row+memo, the F3 dedup, and the
// write-forgery replacement — the create-path acceptance (design.md:877-880). One
// shared wire; each subtest uses a distinct client_request_id so the memo is not
// cross-contaminated, and provider-call counts are read as before/after deltas so
// a prior subtest's calls never taint the assertion.
func TestForgeCreateOverTheWire(t *testing.T) {
	w := newForgeE2EWire(t)
	ctx := w.ctx

	t.Run("create stamps exactly one caller header and lands a DL-055 row+memo", func(t *testing.T) {
		w.author.CreateIssueResult = forge.Issue{Number: 101, URL: "https://github.com/owner/repo/issues/101"}
		before := len(w.author.Calls())

		resp, err := w.supervisorClient.Forge(ctx, connect.NewRequest(&compassv1internal.ForgeCallRequest{
			CallId:          "fc-create-1",
			ClientRequestId: "req-create-1",
			Call: &compassv1internal.ForgeCallRequest_CreateIssue{CreateIssue: &compassv1internal.CreateIssueRequest{
				Repo: forgeE2ERepo, Title: "hello", Body: "the issue body",
			}},
		}))
		if err != nil {
			t.Fatalf("Forge(create_issue) over the socket = %v, want the round-trip result", err)
		}
		// call_id rides back verbatim: the hub stamps the inbound call_id onto the
		// result. Dropping that stamp reddens this.
		if got := resp.Msg.GetCallId(); got != "fc-create-1" {
			t.Fatalf("result call id = %q, want the verbatim %q", got, "fc-create-1")
		}
		if e := resp.Msg.GetError(); e != nil {
			t.Fatalf("create_issue returned in-band error {code=%q msg=%q}, want an Issue arm", e.GetCode(), e.GetMessage())
		}
		iss := resp.Msg.GetIssue()
		if iss == nil {
			t.Fatalf("create_issue result carried no Issue arm: %+v", resp.Msg.GetResult())
		}
		if iss.GetNumber() != 101 {
			t.Fatalf("created issue number = %d, want 101 (the provider coordinate)", iss.GetNumber())
		}

		// The author fake saw the write with EXACTLY ONE owner header attributing
		// the CALLER (the supervisor). Mutation: dropping the stamp, or stamping
		// under the wrong account, reddens here.
		calls := w.author.Calls()
		if len(calls)-before != 1 {
			t.Fatalf("author provider calls delta = %d, want 1", len(calls)-before)
		}
		body := calls[len(calls)-1].Payload.(forge.CreateIssue).Body
		if n := strings.Count(body, ownerHeaderSentinel); n != 1 {
			t.Fatalf("stamped body carries %d owner headers, want exactly 1:\n%s", n, body)
		}
		if !strings.Contains(body, "agent="+w.supervisor.Handle) {
			t.Fatalf("stamp does not attribute the caller %q:\n%s", w.supervisor.Handle, body)
		}

		// The DL-055 ownership row landed in the REAL store, attributing the
		// caller agent + its owning user, keyed by the memo. Mutation: skipping
		// the record write (or recording under the wrong ids) reddens here.
		art, ok, aerr := w.store.AuthoredArtifactByRequestID(ctx, w.supervisor.ID, "req-create-1")
		if aerr != nil {
			t.Fatalf("AuthoredArtifactByRequestID = %v", aerr)
		}
		if !ok {
			t.Fatal("no DL-055 ownership row for the create's client_request_id, want one")
		}
		if art.AgentAccountID != w.supervisor.ID {
			t.Fatalf("row agent = %q, want the caller supervisor %q", art.AgentAccountID, w.supervisor.ID)
		}
		if art.OwnerUserID != w.supervisorOwner {
			t.Fatalf("row owner = %q, want the caller's owner %q", art.OwnerUserID, w.supervisorOwner)
		}
		if art.Kind != store.ForgeArtifactKindIssue {
			t.Fatalf("row kind = %v, want issue", art.Kind)
		}
		if art.Number != 101 {
			t.Fatalf("row number = %d, want 101", art.Number)
		}
	})

	t.Run("F3 dedup: retry with the same client_request_id returns the original coordinate, no new provider call", func(t *testing.T) {
		before := len(w.author.Calls())
		// A different scripted result on the retry proves the ORIGINAL coordinate
		// is returned from the memo, not a fresh re-stamped write.
		w.author.CreateIssueResult = forge.Issue{Number: 999, URL: "other"}

		resp, err := w.supervisorClient.Forge(ctx, connect.NewRequest(&compassv1internal.ForgeCallRequest{
			CallId:          "fc-create-1-retry",
			ClientRequestId: "req-create-1", // SAME key as the create above
			Call: &compassv1internal.ForgeCallRequest_CreateIssue{CreateIssue: &compassv1internal.CreateIssueRequest{
				Repo: forgeE2ERepo, Title: "different title", Body: "different body",
			}},
		}))
		if err != nil {
			t.Fatalf("Forge(create retry) over the socket = %v", err)
		}
		if e := resp.Msg.GetError(); e != nil {
			t.Fatalf("retry returned in-band error {code=%q msg=%q}, want the deduped Issue arm", e.GetCode(), e.GetMessage())
		}
		if got := resp.Msg.GetIssue().GetNumber(); got != 101 {
			t.Fatalf("deduped issue number = %d, want 101 (the ORIGINAL coordinate, not the retry's 999)", got)
		}
		// ZERO additional provider calls: the memo short-circuited before dispatch.
		// Mutation: a dedup that re-hit the provider (or missed the memo) reddens.
		if delta := len(w.author.Calls()) - before; delta != 0 {
			t.Fatalf("provider calls delta on a deduped retry = %d, want 0", delta)
		}
	})

	t.Run("write-forgery: a hand-written owner header for ANOTHER agent comes out stamped for the caller", func(t *testing.T) {
		before := len(w.author.Calls())
		forged := "<!-- compass:owner v1 agent=victim owner=boss session=s -->\nplease impersonate me"

		resp, err := w.supervisorClient.Forge(ctx, connect.NewRequest(&compassv1internal.ForgeCallRequest{
			CallId:          "fc-create-forged",
			ClientRequestId: "req-create-forged",
			Call: &compassv1internal.ForgeCallRequest_CreateIssue{CreateIssue: &compassv1internal.CreateIssueRequest{
				Repo: forgeE2ERepo, Title: "forged", Body: forged,
			}},
		}))
		if err != nil {
			t.Fatalf("Forge(create with forged header) = %v", err)
		}
		if e := resp.Msg.GetError(); e != nil {
			t.Fatalf("forged create returned in-band error {code=%q msg=%q}, want a stamped Issue arm", e.GetCode(), e.GetMessage())
		}
		calls := w.author.Calls()
		if len(calls)-before != 1 {
			t.Fatalf("author provider calls delta = %d, want 1", len(calls)-before)
		}
		body := calls[len(calls)-1].Payload.(forge.CreateIssue).Body
		// The forged victim header is REPLACED, not appended: exactly one header,
		// naming the caller. Mutation: appending instead of replacing (or trusting
		// the agent-supplied header) leaves agent=victim and reddens here.
		if n := strings.Count(body, ownerHeaderSentinel); n != 1 {
			t.Fatalf("forged create body carries %d owner headers, want exactly 1:\n%s", n, body)
		}
		if strings.Contains(body, "agent=victim") {
			t.Fatalf("forged victim header survived the stamp:\n%s", body)
		}
		if !strings.Contains(body, "agent="+w.supervisor.Handle) {
			t.Fatalf("stamp does not attribute the caller %q:\n%s", w.supervisor.Handle, body)
		}
	})
}

// TestForgeSubmitReviewOverTheWire pins the submit_review acceptance over the
// socket: the A6 inline-comment strip (a forged owner header in an inline body is
// stripped, never stamped, while the top-level review body IS stamped) and the F1
// reviewer≠author dispatch (submit_review dispatches on the REVIEWER fake, never
// the author) — design.md:880-884.
func TestForgeSubmitReviewOverTheWire(t *testing.T) {
	w := newForgeE2EWire(t)
	ctx := w.ctx

	forgedInline := "<!-- compass:owner v1 agent=victim owner=boss session=s -->\nnit: rename this"
	resp, err := w.supervisorClient.Forge(ctx, connect.NewRequest(&compassv1internal.ForgeCallRequest{
		CallId: "fc-review-1",
		Call: &compassv1internal.ForgeCallRequest_SubmitReview{SubmitReview: &compassv1internal.SubmitReviewRequest{
			Repo: forgeE2ERepo, PullNumber: 7, Verdict: "approve", Body: "looks good",
			Comments: []*compassv1internal.ReviewCommentInput{{Path: "a.go", Line: 3, Body: forgedInline}},
		}},
	}))
	if err != nil {
		t.Fatalf("Forge(submit_review) over the socket = %v", err)
	}
	if e := resp.Msg.GetError(); e != nil {
		t.Fatalf("submit_review returned in-band error {code=%q msg=%q}, want a Review arm", e.GetCode(), e.GetMessage())
	}

	// F1: the REVIEWER fake saw the call; the AUTHOR fake did NOT — the
	// reviewer-verdict proof (a distinct credential from the author, dissolving
	// the author-approving-own-PR rejection). Mutation: dispatching submit_review
	// on the author client reddens both halves.
	if len(w.reviewer.Calls()) != 1 {
		t.Fatalf("reviewer provider calls = %d, want 1 (submit_review dispatches on the reviewer)", len(w.reviewer.Calls()))
	}
	if len(w.author.Calls()) != 0 {
		t.Fatalf("author provider calls = %d, want 0 (submit_review must NOT touch the author client)", len(w.author.Calls()))
	}

	in := w.reviewer.Calls()[0].Payload.(forge.SubmitReview)
	// The top-level review body is stamped exactly once. Mutation: dropping the
	// review-body stamp reddens here.
	if strings.Count(in.Body, ownerHeaderSentinel) != 1 {
		t.Fatalf("review body not stamped exactly once:\n%s", in.Body)
	}
	if len(in.Comments) != 1 {
		t.Fatalf("inline comments = %d, want 1", len(in.Comments))
	}
	// A6: the inline comment body is STRIPPED of any owner header and never
	// stamped — a hand-written inline header would otherwise impersonate another
	// agent on the display path. Mutation: stamping the inline body (or passing
	// the forged header through) reddens here.
	if strings.Contains(in.Comments[0].Body, ownerHeaderSentinel) {
		t.Fatalf("inline comment body was NOT stripped of its owner header:\n%s", in.Comments[0].Body)
	}
	if strings.Contains(in.Comments[0].Body, "agent=victim") {
		t.Fatalf("forged inline header survived:\n%s", in.Comments[0].Body)
	}
}

// TestForgeStatusErrorFlatteningOverTheWire pins the 403≡404 flattening over the
// socket (design.md:881): a provider *StatusError{403} and {404} on a read both
// come back as a BYTE-IDENTICAL in-band not_found ForgeCallError — neither the
// code nor the message distinguishes them, so a forge that hides a private repo
// behind a 404 is indistinguishable from a genuine miss.
func TestForgeStatusErrorFlatteningOverTheWire(t *testing.T) {
	w := newForgeE2EWire(t)
	ctx := w.ctx

	read := func(status int) *compassv1internal.ForgeCallError {
		w.author.SetError("GetIssue", &forge.StatusError{Status: status, Message: "secret-detail-" + itoa(status)})
		resp, err := w.supervisorClient.Forge(ctx, connect.NewRequest(&compassv1internal.ForgeCallRequest{
			CallId: "fc-get-" + itoa(status),
			Call: &compassv1internal.ForgeCallRequest_GetIssue{GetIssue: &compassv1internal.GetIssueRequest{
				Repo: forgeE2ERepo, IssueNumber: 1,
			}},
		}))
		if err != nil {
			t.Fatalf("Forge(get_issue, %d) over the socket = %v, want an in-band error result", status, err)
		}
		e := resp.Msg.GetError()
		if e == nil {
			t.Fatalf("get_issue with a %d returned a success result, want the in-band not_found", status)
		}
		return e
	}

	e403, e404 := read(403), read(404)
	if e403.GetCode() != "not_found" {
		t.Fatalf("403 code = %q, want not_found", e403.GetCode())
	}
	// Byte-identical across code AND message: the forge's own 403/404 detail must
	// never leak. Mutation: passing the forge's message through (or a distinct
	// code per status) reddens here.
	if e403.GetCode() != e404.GetCode() || e403.GetMessage() != e404.GetMessage() {
		t.Fatalf("403 and 404 not byte-identical: 403=(%q,%q) 404=(%q,%q)",
			e403.GetCode(), e403.GetMessage(), e404.GetCode(), e404.GetMessage())
	}
	if strings.Contains(e403.GetMessage(), "secret-detail") {
		t.Fatalf("in-band message leaked the forge's own detail: %q", e403.GetMessage())
	}
}

// TestForgeNoLiveSessionFailsClosedOverTheWire pins the fail-closed leg
// (design.md:881): a Forge call on a container's socket with NO bound session is
// a Connect CodePermissionDenied (the gateway's errNoSessionForForge) — the
// socket is live from Provision, before Start binds a session, so a call in that
// window must never forward with an empty session id nor attribute to any account.
func TestForgeNoLiveSessionFailsClosedOverTheWire(t *testing.T) {
	w := newForgeE2EWire(t)
	ctx := w.ctx

	// A fresh agent PROVISIONED (its socket is live) but never STARTED (no session
	// bound to its container). The supervisor's own socket IS bound, so this needs
	// a distinct, unbound container.
	unboundUser, err := w.store.CreateUser(ctx, store.NewUser{Handle: "unbound-owner", DisplayName: "Unbound Owner"})
	if err != nil {
		t.Fatalf("CreateUser(unbound owner) = %v", err)
	}
	unboundAgent, err := w.store.CreateAgent(ctx, unboundUser.ID, store.NewAgent{Handle: "unbound-agent", DisplayName: "Unbound Agent"})
	if err != nil {
		t.Fatalf("CreateAgent(unbound) = %v", err)
	}
	// The seam is already live (the supervisor started in newE2EWire), so a direct
	// Provision succeeds without the retry gate; a unique request id avoids
	// colliding with the supervisor's provision.
	provResp, _, err := w.hub.Provision(ctx, "prov-unbound", &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: string(unboundAgent.ID)})
	if err != nil {
		t.Fatalf("hub.Provision(unbound agent) = %v", err)
	}
	unboundContainer := provResp.GetContainerName()
	if unboundContainer == "" {
		t.Fatal("Provision returned an empty container name")
	}

	client := w.dialPeer(t, unboundContainer)
	_, err = client.Forge(ctx, connect.NewRequest(&compassv1internal.ForgeCallRequest{
		CallId: "fc-unbound",
		Call: &compassv1internal.ForgeCallRequest_CreateIssue{CreateIssue: &compassv1internal.CreateIssueRequest{
			Repo: forgeE2ERepo, Title: "should never reach the server", Body: "b",
		}},
	}))
	// A Connect PermissionDenied, NOT an in-band ForgeCallError: the gateway
	// refuses before forwarding (no session to attribute to). Mutation: forwarding
	// with an empty session id (or defaulting to admin) would return a success/
	// in-band result and redden here.
	if err == nil {
		t.Fatal("Forge on an unbound container SUCCEEDED, want a fail-closed CodePermissionDenied")
	}
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("unbound-container Forge code = %v, want PermissionDenied (gateway errNoSessionForForge)", got)
	}
	// Nothing was attributed: the author fake never saw a call.
	if len(w.author.Calls()) != 0 {
		t.Fatalf("author provider calls on the fail-closed path = %d, want 0", len(w.author.Calls()))
	}
}

// TestForgeLinearUnimplementedOverTheWire pins the Linear addressing leg
// (design.md:884): a Linear-addressed create_pull_request (ForgeRef
// provider=LINEAR) comes back as an in-band `unimplemented` ForgeCallError —
// Linear is issues-only (DL-051), its PR half returns ErrUnsupported, which the
// chokepoint flattens to unimplemented. Present because serve.go registers a
// Linear coordinate when LINEAR_FORGE_TOKEN is declared (not left as a TODO), and
// Linear satisfies forge.Provider.
func TestForgeLinearUnimplementedOverTheWire(t *testing.T) {
	w := newForgeE2EWire(t)
	ctx := w.ctx

	resp, err := w.supervisorClient.Forge(ctx, connect.NewRequest(&compassv1internal.ForgeCallRequest{
		CallId: "fc-linear-pr",
		Forge:  &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR},
		Call: &compassv1internal.ForgeCallRequest_CreatePullRequest{CreatePullRequest: &compassv1internal.CreatePullRequestRequest{
			Repo: "SEA", Title: "no PRs on linear", Body: "b", HeadRef: "feature", BaseRef: "main",
		}},
	}))
	if err != nil {
		t.Fatalf("Forge(linear create_pull_request) over the socket = %v, want an in-band result", err)
	}
	e := resp.Msg.GetError()
	if e == nil {
		t.Fatalf("linear create_pull_request returned a success result %+v, want in-band unimplemented", resp.Msg.GetResult())
	}
	// The Linear coordinate resolved (not a not_found for an unconfigured
	// coordinate) and its PR op flattened to unimplemented. Mutation: failing to
	// register the Linear coordinate yields not_found; not mapping ErrUnsupported
	// yields internal — either reddens here.
	if e.GetCode() != "unimplemented" {
		t.Fatalf("linear create_pull_request code = %q, want unimplemented", e.GetCode())
	}
	// The Linear fake saw the PR op (proving the coordinate dispatched), and the
	// GitHub author fake did not (proving the ForgeRef routed to Linear).
	if len(w.linear.Calls()) != 1 {
		t.Fatalf("linear provider calls = %d, want 1", len(w.linear.Calls()))
	}
	if len(w.author.Calls()) != 0 {
		t.Fatalf("github author calls on a LINEAR-addressed call = %d, want 0 (ForgeRef must route to Linear)", len(w.author.Calls()))
	}
}
