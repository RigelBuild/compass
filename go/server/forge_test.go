//go:build unix

package server

// Default-lane (no database) tests for the DL-050 forge-write chokepoint
// (forgeService.ExecuteForgeCallAsAccount). The service is driven against the
// exported forge.FakeProvider (author + reviewer roles, so F1 dispatch is
// observable) and a FAITHFUL in-memory forgeStore that models exactly the two
// store contracts the chokepoint depends on: the caller→Author resolution and
// the F3 memo hit/miss + DL-055 row (a row lands ONLY after a create success, so
// a retry after a FAILED create finds no memo and re-attempts). The store
// interface is narrow precisely so this lane can fake it — the real-Postgres
// contract for the memo/row is proven in the store package's own pgtest suite
// (forge_authored_pgtest_test.go), and the end-to-end socket proof is T5/T8's.
//
// Session ids here MUST satisfy the owner.go:40 handle grammar
// (^[a-z0-9][a-z0-9-]{0,38}$): StampOwner interpolates the session id into the
// header, so a non-conforming id is a StampOwner error — the coupling is pinned
// here, not discovered in review (A9).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/board"
	"github.com/sealedsecurity/compass/go/internal/forge"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// ownerHeaderSentinel is the one literal owner-header marker StampOwner writes;
// a stamped body carries exactly one, a stripped body carries none.
const ownerHeaderSentinel = "<!-- compass:owner "

// --- fake store -------------------------------------------------------------

// fakeForgeStore is a faithful in-memory forgeStore. accounts resolves the
// caller→Author identity; the memo map models the F3 dedup keyed by
// (agent, client_request_id); RecordAuthoredArtifact writes both the coordinate
// row and the memo in one step, exactly as the real store's single statement
// does — so a create that never calls record (a failed create) leaves no memo,
// and a retry re-attempts.
type fakeForgeStore struct {
	accounts map[store.AccountID]store.Account
	memo     map[string]store.AuthoredArtifact // key: agent|client_request_id
	recorded []store.AuthoredArtifact
	getErr   error // if set, GetAccount returns it verbatim
	recErr   error // if set, RecordAuthoredArtifact returns it verbatim
}

func newFakeForgeStore() *fakeForgeStore {
	return &fakeForgeStore{
		accounts: make(map[store.AccountID]store.Account),
		memo:     make(map[string]store.AuthoredArtifact),
	}
}

func (f *fakeForgeStore) GetAccount(_ context.Context, id store.AccountID) (store.Account, error) {
	if f.getErr != nil {
		return store.Account{}, f.getErr
	}
	acc, ok := f.accounts[id]
	if !ok {
		return store.Account{}, store.ErrNotFound
	}
	return acc, nil
}

func (f *fakeForgeStore) AuthoredArtifactByRequestID(_ context.Context, agent store.AccountID, clientRequestID string) (store.AuthoredArtifact, bool, error) {
	if clientRequestID == "" {
		return store.AuthoredArtifact{}, false, nil
	}
	a, ok := f.memo[string(agent)+"|"+clientRequestID]
	return a, ok, nil
}

func (f *fakeForgeStore) RecordAuthoredArtifact(_ context.Context, a store.AuthoredArtifact) error {
	if f.recErr != nil {
		return f.recErr
	}
	f.recorded = append(f.recorded, a)
	if a.ClientRequestID != "" {
		f.memo[string(a.AgentAccountID)+"|"+a.ClientRequestID] = a
	}
	return nil
}

// seedAgent registers an agent account and its owning user so resolveIdentity
// finds both handles.
func (f *fakeForgeStore) seedAgent(agentID store.AccountID, agentHandle string, ownerID store.AccountID, ownerHandle string) {
	f.accounts[agentID] = store.Account{ID: agentID, Handle: agentHandle, Agent: &store.AgentAccount{OwnerUserID: ownerID}}
	f.accounts[ownerID] = store.Account{ID: ownerID, Handle: ownerHandle, User: &store.UserAccount{}}
}

// --- harness ----------------------------------------------------------------

const (
	testAgentID     = store.AccountID("acct-agent")
	testOwnerID     = store.AccountID("acct-owner")
	testAgentHandle = "scout"
	testOwnerHandle = "matt"
	testSessionID   = "sess-01" // conforms to owner.go:40 grammar (A9)
	testRepo        = "owner/repo"
	testHost        = "github.com"
)

// newForgeServiceForTest builds a forgeService over a seeded fake store, a real
// (store-less) IssueProjection, and a registry whose default coordinate carries
// the given author + reviewer fakes. Returns the service and the store so a test
// can assert the recorded rows.
func newForgeServiceForTest(t *testing.T, author, reviewer *forge.FakeProvider) (*forgeService, *fakeForgeStore) {
	t.Helper()
	st := newFakeForgeStore()
	st.seedAgent(testAgentID, testAgentHandle, testOwnerID, testOwnerHandle)

	reg := newForgeProviderRegistry()
	reg.register(forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, host: testHost}, author, reviewer, true)

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := &forgeService{
		store:     st,
		issueBrd:  board.NewIssueProjection(bus, nil),
		providers: reg,
		now:       nowStub,
	}
	return svc, st
}

// nowStub is a fixed clock so the DL-055 row's created_at is deterministic.
func nowStub() time.Time { return time.Unix(1_700_000_000, 0) }

// ExecuteForgeCallAsAccountMust drives a call as the seeded test agent and fails
// the test on a Connect (transport) error, returning the in-band result — the
// shape every non-guard test asserts against.
func (s *forgeService) ExecuteForgeCallAsAccountMust(t *testing.T, call *compassv1internal.ForgeCallRequest) *compassv1internal.ForgeCallResult {
	t.Helper()
	res, err := s.ExecuteForgeCallAsAccount(context.Background(), testAgentID, testSessionID, call)
	if err != nil {
		t.Fatalf("ExecuteForgeCallAsAccount returned a Connect error: %v", err)
	}
	return res
}

// --- write requests helpers -------------------------------------------------

func createIssueCall(body, clientReqID string) *compassv1internal.ForgeCallRequest {
	return &compassv1internal.ForgeCallRequest{
		ClientRequestId: clientReqID,
		Call: &compassv1internal.ForgeCallRequest_CreateIssue{CreateIssue: &compassv1internal.CreateIssueRequest{
			Repo: testRepo, Title: "t", Body: body,
		}},
	}
}

func createPRCall(body, clientReqID string) *compassv1internal.ForgeCallRequest {
	return &compassv1internal.ForgeCallRequest{
		ClientRequestId: clientReqID,
		Call: &compassv1internal.ForgeCallRequest_CreatePullRequest{CreatePullRequest: &compassv1internal.CreatePullRequestRequest{
			Repo: testRepo, Title: "t", Body: body, HeadRef: "feature", BaseRef: "main",
		}},
	}
}

// --- tests: stamping --------------------------------------------------------

// TestCreateIssueStampsExactlyOneHeaderReplacingForged pins the load-bearing
// security property: every write is stamped (a header is present in the body the
// provider saw), and a forged header the agent hand-wrote into its own body is
// REPLACED — exactly one header comes out, naming the caller, not the victim.
func TestForgeCreateIssueStampsExactlyOneHeaderReplacingForged(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, _ := newForgeServiceForTest(t, author, reviewer)

	forged := "<!-- compass:owner v1 agent=victim owner=boss session=s -->\nhello"
	res := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall(forged, ""))
	if res.GetError() != nil {
		t.Fatalf("create_issue errored: %v", res.GetError())
	}
	calls := author.Calls()
	if len(calls) != 1 {
		t.Fatalf("author provider calls = %d, want 1", len(calls))
	}
	body := calls[0].Payload.(forge.CreateIssue).Body
	if n := strings.Count(body, ownerHeaderSentinel); n != 1 {
		t.Fatalf("stamped body carries %d owner headers, want exactly 1:\n%s", n, body)
	}
	if strings.Contains(body, "agent=victim") {
		t.Fatalf("forged victim header survived the stamp:\n%s", body)
	}
	if !strings.Contains(body, "agent="+testAgentHandle) {
		t.Fatalf("stamp does not attribute the caller %q:\n%s", testAgentHandle, body)
	}
}

// TestSubmitReviewStripsInlineCommentOwnerHeaders pins A6: the top-level review
// body is stamped normally, but every inline review-comment body is STRIPPED of
// any owner-header block and NEVER stamped — a hand-written header in an inline
// body would otherwise impersonate another agent on the display path.
func TestForgeSubmitReviewStripsInlineCommentOwnerHeaders(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, _ := newForgeServiceForTest(t, author, reviewer)

	forgedInline := "<!-- compass:owner v1 agent=victim owner=boss session=s -->\nnit: rename"
	call := &compassv1internal.ForgeCallRequest{
		Call: &compassv1internal.ForgeCallRequest_SubmitReview{SubmitReview: &compassv1internal.SubmitReviewRequest{
			Repo: testRepo, PullNumber: 7, Verdict: "comment", Body: "looks good",
			Comments: []*compassv1internal.ReviewCommentInput{{Path: "a.go", Line: 3, Body: forgedInline}},
		}},
	}
	res := svc.ExecuteForgeCallAsAccountMust(t, call)
	if res.GetError() != nil {
		t.Fatalf("submit_review errored: %v", res.GetError())
	}
	calls := reviewer.Calls()
	if len(calls) != 1 {
		t.Fatalf("reviewer calls = %d, want 1", len(calls))
	}
	in := calls[0].Payload.(forge.SubmitReview)
	if strings.Count(in.Body, ownerHeaderSentinel) != 1 {
		t.Fatalf("review body not stamped exactly once:\n%s", in.Body)
	}
	if len(in.Comments) != 1 {
		t.Fatalf("inline comments = %d, want 1", len(in.Comments))
	}
	if strings.Contains(in.Comments[0].Body, ownerHeaderSentinel) {
		t.Fatalf("inline comment body was NOT stripped of its owner header:\n%s", in.Comments[0].Body)
	}
	if strings.Contains(in.Comments[0].Body, "agent=victim") {
		t.Fatalf("forged inline header survived:\n%s", in.Comments[0].Body)
	}
}

// TestGetIssueReadBodyHasNoOwnerHeader pins that a read body never carries a
// compass:owner header on the wire — provider truth is stripped/parsed (DL-050),
// not passed through raw.
func TestForgeGetIssueReadBodyHasNoOwnerHeader(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, _ := newForgeServiceForTest(t, author, reviewer)

	author.GetIssueResult = forge.Issue{
		Number: 9,
		Body:   "<!-- compass:owner v1 agent=" + testAgentHandle + " owner=" + testOwnerHandle + " session=" + testSessionID + " -->\n🧭 Written by **@scout** (Compass agent, owned by **@matt**)\n\n---\n\nthe real body",
	}
	call := &compassv1internal.ForgeCallRequest{
		Call: &compassv1internal.ForgeCallRequest_GetIssue{GetIssue: &compassv1internal.GetIssueRequest{Repo: testRepo, IssueNumber: 9}},
	}
	res := svc.ExecuteForgeCallAsAccountMust(t, call)
	iss := res.GetIssue()
	if iss == nil {
		t.Fatalf("get_issue returned no issue arm: %v", res.GetError())
	}
	if strings.Contains(iss.GetBody(), ownerHeaderSentinel) {
		t.Fatalf("read body leaked an owner header:\n%s", iss.GetBody())
	}
	if iss.GetAgent().GetAgentHandle() != testAgentHandle {
		t.Fatalf("parsed display attribution = %q, want %q", iss.GetAgent().GetAgentHandle(), testAgentHandle)
	}
}

// --- tests: guards ----------------------------------------------------------

// TestEmptyCallerIsConnectErrorWithZeroProviderCalls pins that an empty caller
// fails as a Connect error (not in-band) with ZERO provider calls — resolution
// short-circuits before any store or provider touch.
func TestForgeEmptyCallerIsConnectErrorWithZeroProviderCalls(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, _ := newForgeServiceForTest(t, author, reviewer)

	_, err := svc.ExecuteForgeCallAsAccount(context.Background(), "", testSessionID, createIssueCall("b", ""))
	if err == nil {
		t.Fatal("empty caller = nil error, want a Connect error")
	}
	if len(author.Calls())+len(reviewer.Calls()) != 0 {
		t.Fatalf("provider calls on empty caller = %d, want 0", len(author.Calls())+len(reviewer.Calls()))
	}
}

// TestUnsetOneofArmIsConnectInvalidArgument pins that an unset oneof arm is a
// Connect CodeInvalidArgument (a malformed request), NOT an in-band tool error
// (A1) — the executeBoardCall default-arm convention.
func TestForgeUnsetOneofArmIsConnectInvalidArgument(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, _ := newForgeServiceForTest(t, author, reviewer)

	_, err := svc.ExecuteForgeCallAsAccount(context.Background(), testAgentID, testSessionID, &compassv1internal.ForgeCallRequest{})
	if err == nil {
		t.Fatal("unset arm = nil error, want CodeInvalidArgument Connect error")
	}
}

// TestEmptyRepoIsInvalidArgumentBeforeAnyTouch pins that an empty repo is an
// in-band invalid_argument BEFORE any store or provider touch.
func TestForgeEmptyRepoIsInvalidArgumentBeforeAnyTouch(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, st := newForgeServiceForTest(t, author, reviewer)

	call := &compassv1internal.ForgeCallRequest{
		Call: &compassv1internal.ForgeCallRequest_CreateIssue{CreateIssue: &compassv1internal.CreateIssueRequest{Repo: "", Body: "b"}},
	}
	res := svc.ExecuteForgeCallAsAccountMust(t, call)
	if fe := res.GetError(); fe == nil || fe.GetCode() != "invalid_argument" {
		t.Fatalf("empty repo error = %v, want invalid_argument", res.GetError())
	}
	if len(author.Calls()) != 0 || len(st.recorded) != 0 {
		t.Fatalf("empty repo touched provider/store: provider=%d store=%d", len(author.Calls()), len(st.recorded))
	}
}

// --- tests: error mapping ---------------------------------------------------

// TestStatusError403And404FlattenToByteIdenticalNotFound pins the #995 T2
// flattening: a 403 and a 404 map to a BYTE-IDENTICAL not_found in-band error —
// neither the code nor the message distinguishes them.
func TestForgeStatusError403And404FlattenToByteIdenticalNotFound(t *testing.T) {
	run := func(status int) *compassv1internal.ForgeCallError {
		author := forge.NewFakeProvider("gh-author")
		reviewer := forge.NewFakeProvider("gh-reviewer")
		svc, _ := newForgeServiceForTest(t, author, reviewer)
		author.SetError("GetIssue", &forge.StatusError{Status: status, Message: "secret-" + itoa(status)})
		call := &compassv1internal.ForgeCallRequest{
			Call: &compassv1internal.ForgeCallRequest_GetIssue{GetIssue: &compassv1internal.GetIssueRequest{Repo: testRepo, IssueNumber: 1}},
		}
		return svc.ExecuteForgeCallAsAccountMust(t, call).GetError()
	}
	e403, e404 := run(403), run(404)
	if e403 == nil || e404 == nil {
		t.Fatalf("nil error: 403=%v 404=%v", e403, e404)
	}
	if e403.GetCode() != "not_found" {
		t.Fatalf("403 code = %q, want not_found", e403.GetCode())
	}
	if e403.GetCode() != e404.GetCode() || e403.GetMessage() != e404.GetMessage() {
		t.Fatalf("403 and 404 not byte-identical: 403=(%q,%q) 404=(%q,%q)",
			e403.GetCode(), e403.GetMessage(), e404.GetCode(), e404.GetMessage())
	}
}

// TestStatusError422IsInvalidArgumentCarryingForgeMessage pins that a 422 maps
// to invalid_argument and carries the forge's own validation message (a
// genuinely invalid submission the model must see).
func TestForgeStatusError422IsInvalidArgumentCarryingForgeMessage(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, _ := newForgeServiceForTest(t, author, reviewer)
	author.SetError("CreateIssue", &forge.StatusError{Status: 422, Message: "label does not exist"})

	res := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("b", ""))
	fe := res.GetError()
	if fe == nil || fe.GetCode() != "invalid_argument" {
		t.Fatalf("422 error = %v, want invalid_argument", fe)
	}
	if fe.GetMessage() != "label does not exist" {
		t.Fatalf("422 message = %q, want the forge validation message", fe.GetMessage())
	}
}

// TestUnsupportedIsUnimplementedNamingProviderAndOp pins ErrUnsupported →
// unimplemented naming the provider and op.
func TestForgeUnsupportedIsUnimplementedNamingProviderAndOp(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, _ := newForgeServiceForTest(t, author, reviewer)
	author.SetError("CreateIssue", forge.ErrUnsupported)

	fe := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("b", "")).GetError()
	if fe == nil || fe.GetCode() != "unimplemented" {
		t.Fatalf("unsupported error = %v, want unimplemented", fe)
	}
	if !strings.Contains(fe.GetMessage(), "gh-author") || !strings.Contains(fe.GetMessage(), "create_issue") {
		t.Fatalf("unimplemented message %q does not name provider+op", fe.GetMessage())
	}
}

// --- tests: byte budget (A9) ------------------------------------------------

// TestBodyBudgetBoundaryStampsThenErrorsWithoutProviderCall pins the reserved-
// header byte budget: a body of exactly limit−len(header) stamps and reaches the
// provider; one byte more is an in-band invalid_argument with ZERO provider
// calls (the stamp is refused before the write).
func TestForgeBodyBudgetBoundaryStampsThenErrorsWithoutProviderCall(t *testing.T) {
	// The reserved header length = the stamp of an empty body under an unlimited
	// budget (the header prefix, nothing else).
	hdr, err := forge.StampOwner("", forge.Author{AgentHandle: testAgentHandle, OwnerHandle: testOwnerHandle, SessionID: testSessionID}, 0)
	if err != nil {
		t.Fatalf("reference stamp: %v", err)
	}
	const limit = 500
	fit := strings.Repeat("x", limit-len(hdr))
	over := fit + "x"

	t.Run("exactly at budget stamps and calls provider", func(t *testing.T) {
		author := forge.NewFakeProvider("gh-author")
		reviewer := forge.NewFakeProvider("gh-reviewer")
		author.BodyLimitResult = limit
		svc, _ := newForgeServiceForTest(t, author, reviewer)
		res := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall(fit, ""))
		if res.GetError() != nil {
			t.Fatalf("at-budget body errored: %v", res.GetError())
		}
		if len(author.Calls()) != 1 {
			t.Fatalf("at-budget provider calls = %d, want 1", len(author.Calls()))
		}
	})

	t.Run("one byte over errors without a provider call", func(t *testing.T) {
		author := forge.NewFakeProvider("gh-author")
		reviewer := forge.NewFakeProvider("gh-reviewer")
		author.BodyLimitResult = limit
		svc, _ := newForgeServiceForTest(t, author, reviewer)
		res := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall(over, ""))
		fe := res.GetError()
		if fe == nil || fe.GetCode() != "invalid_argument" {
			t.Fatalf("over-budget error = %v, want invalid_argument", fe)
		}
		if len(author.Calls()) != 0 {
			t.Fatalf("over-budget provider calls = %d, want 0 (stamp refused before write)", len(author.Calls()))
		}
	})
}

// --- tests: F3 idempotency --------------------------------------------------

// TestCreateRetriedWithSameKeyReturnsOriginalCoordinateZeroProviderCalls pins
// F3: a second create with the same client_request_id returns the ORIGINAL
// recorded coordinate with ZERO additional provider calls.
func TestForgeCreateRetriedWithSameKeyReturnsOriginalCoordinateZeroProviderCalls(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, st := newForgeServiceForTest(t, author, reviewer)
	author.CreateIssueResult = forge.Issue{Number: 42, URL: "u"}

	first := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("b", "req-1"))
	if first.GetError() != nil {
		t.Fatalf("first create errored: %v", first.GetError())
	}
	if first.GetIssue().GetNumber() != 42 {
		t.Fatalf("first create issue number = %d, want 42", first.GetIssue().GetNumber())
	}
	if len(st.recorded) != 1 {
		t.Fatalf("recorded rows after first create = %d, want 1", len(st.recorded))
	}

	// A different body on the retry proves the ORIGINAL coordinate is returned,
	// not a re-stamped fresh write.
	author.CreateIssueResult = forge.Issue{Number: 99, URL: "other"}
	second := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("different", "req-1"))
	if second.GetError() != nil {
		t.Fatalf("retry errored: %v", second.GetError())
	}
	if second.GetIssue().GetNumber() != 42 {
		t.Fatalf("retry issue number = %d, want 42 (original coordinate)", second.GetIssue().GetNumber())
	}
	if len(author.Calls()) != 1 {
		t.Fatalf("provider calls total = %d, want 1 (retry deduped)", len(author.Calls()))
	}
	if len(st.recorded) != 1 {
		t.Fatalf("recorded rows total = %d, want 1 (no second row)", len(st.recorded))
	}
}

// TestRetryAfterFailedCreateReAttempts pins F3's other half: a create that
// FAILED at the provider wrote no memo row, so a retry with the same key
// re-attempts (a second provider call), and succeeds.
func TestForgeRetryAfterFailedCreateReAttempts(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, st := newForgeServiceForTest(t, author, reviewer)

	author.SetError("CreateIssue", &forge.StatusError{Status: 422, Message: "bad"})
	failed := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("b", "req-1"))
	if failed.GetError() == nil {
		t.Fatal("first create should have failed")
	}
	if len(st.recorded) != 0 {
		t.Fatalf("failed create wrote %d rows, want 0", len(st.recorded))
	}

	author.SetError("CreateIssue", nil) // clear
	author.CreateIssueResult = forge.Issue{Number: 7}
	retry := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("b", "req-1"))
	if retry.GetError() != nil {
		t.Fatalf("retry after failure errored: %v", retry.GetError())
	}
	if len(author.Calls()) != 2 {
		t.Fatalf("provider calls = %d, want 2 (retry re-attempts)", len(author.Calls()))
	}
	if len(st.recorded) != 1 {
		t.Fatalf("recorded rows = %d, want 1 (row on the successful retry)", len(st.recorded))
	}
}

// --- tests: F1 dual-client dispatch -----------------------------------------

// TestF1DispatchReviewerVsAuthorClient pins F1: submit_review dispatches on the
// REVIEWER client (and never the author), while create_issue dispatches on the
// AUTHOR client (and never the reviewer).
func TestForgeF1DispatchReviewerVsAuthorClient(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, _ := newForgeServiceForTest(t, author, reviewer)

	review := &compassv1internal.ForgeCallRequest{
		Call: &compassv1internal.ForgeCallRequest_SubmitReview{SubmitReview: &compassv1internal.SubmitReviewRequest{
			Repo: testRepo, PullNumber: 1, Verdict: "approve", Body: "ok",
		}},
	}
	if res := svc.ExecuteForgeCallAsAccountMust(t, review); res.GetError() != nil {
		t.Fatalf("submit_review errored: %v", res.GetError())
	}
	if len(reviewer.Calls()) != 1 || len(author.Calls()) != 0 {
		t.Fatalf("submit_review dispatch: reviewer=%d author=%d, want reviewer=1 author=0", len(reviewer.Calls()), len(author.Calls()))
	}

	if res := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("b", "")); res.GetError() != nil {
		t.Fatalf("create_issue errored: %v", res.GetError())
	}
	if len(author.Calls()) != 1 {
		t.Fatalf("create_issue dispatch: author=%d, want 1", len(author.Calls()))
	}
	if len(reviewer.Calls()) != 1 {
		t.Fatalf("create_issue leaked onto reviewer: reviewer total=%d, want 1 (unchanged)", len(reviewer.Calls()))
	}
}

// --- tests: create_pull_request arm (M2) ------------------------------------

// TestForgeCreatePullRequestStampsAndRecordsPRKind pins the PR-create twin of
// createIssue: the body reaching the AUTHOR client is stamped exactly once, the
// result is a PullRequest arm, and the DL-055 row lands with Kind=pull_request
// and the returned coordinate's number — the copy-paste class of bug (wrong
// role, missing stamp, wrong kind constant) a happy-path assertion catches.
func TestForgeCreatePullRequestStampsAndRecordsPRKind(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, st := newForgeServiceForTest(t, author, reviewer)
	author.CreatePRResult = forge.PullRequest{Number: 55, URL: "u"}

	res := svc.ExecuteForgeCallAsAccountMust(t, createPRCall("body", ""))
	if res.GetError() != nil {
		t.Fatalf("create_pull_request errored: %v", res.GetError())
	}
	if res.GetPullRequest() == nil {
		t.Fatalf("create_pull_request returned no PR arm: %v", res)
	}
	if len(author.Calls()) != 1 || len(reviewer.Calls()) != 0 {
		t.Fatalf("PR-create dispatch: author=%d reviewer=%d, want author=1 reviewer=0", len(author.Calls()), len(reviewer.Calls()))
	}
	body := author.Calls()[0].Payload.(forge.CreatePR).Body
	if n := strings.Count(body, ownerHeaderSentinel); n != 1 {
		t.Fatalf("PR body carries %d owner headers, want exactly 1:\n%s", n, body)
	}
	if len(st.recorded) != 1 {
		t.Fatalf("recorded rows = %d, want 1", len(st.recorded))
	}
	if st.recorded[0].Kind != store.ForgeArtifactKindPullRequest {
		t.Fatalf("recorded kind = %v, want pull_request", st.recorded[0].Kind)
	}
	if st.recorded[0].Number != 55 {
		t.Fatalf("recorded number = %d, want 55", st.recorded[0].Number)
	}
}

// TestForgeCreateRetryMismatchedArmReturnsStoredKind pins the F3 mismatched-arm
// property: a create_issue recorded under a client_request_id, then a
// create_pull_request RETRIED with the SAME key, returns the ORIGINAL Issue
// coordinate (the STORED kind), never a PR arm, with ZERO additional provider
// calls — the memo keys on (agent, request id), not on the arm.
func TestForgeCreateRetryMismatchedArmReturnsStoredKind(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, st := newForgeServiceForTest(t, author, reviewer)
	author.CreateIssueResult = forge.Issue{Number: 42, URL: "u"}

	first := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("b", "req-x"))
	if first.GetError() != nil || first.GetIssue().GetNumber() != 42 {
		t.Fatalf("first create_issue = %v, want issue 42", first)
	}

	second := svc.ExecuteForgeCallAsAccountMust(t, createPRCall("different", "req-x"))
	if second.GetError() != nil {
		t.Fatalf("mismatched-arm retry errored: %v", second.GetError())
	}
	if second.GetPullRequest() != nil {
		t.Fatalf("mismatched-arm retry returned a PR arm, want the stored ISSUE arm")
	}
	if second.GetIssue().GetNumber() != 42 {
		t.Fatalf("mismatched-arm retry issue number = %d, want 42 (stored coordinate)", second.GetIssue().GetNumber())
	}
	if len(author.Calls()) != 1 {
		t.Fatalf("provider calls total = %d, want 1 (PR retry deduped, zero provider calls)", len(author.Calls()))
	}
	if len(st.recorded) != 1 {
		t.Fatalf("recorded rows = %d, want 1 (no second row)", len(st.recorded))
	}
}

// TestForgeNonAgentCallerIsInvalidArgumentZeroProviderCalls pins the security
// guard: a caller AccountID that resolves to a NON-agent account (Agent==nil, a
// plain user) cannot author a stamped forge artifact — the write fails in-band
// with invalid_argument BEFORE any stamp or provider touch. Inverting or
// dropping the guard would otherwise ship green while a user/service account
// drove an attributed write.
func TestForgeNonAgentCallerIsInvalidArgumentZeroProviderCalls(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, st := newForgeServiceForTest(t, author, reviewer)

	const nonAgentID = store.AccountID("acct-user")
	st.accounts[nonAgentID] = store.Account{ID: nonAgentID, Handle: "auser", User: &store.UserAccount{}}

	res, err := svc.ExecuteForgeCallAsAccount(context.Background(), nonAgentID, testSessionID, createIssueCall("b", ""))
	if err != nil {
		t.Fatalf("non-agent caller returned a Connect error, want in-band: %v", err)
	}
	fe := res.GetError()
	if fe == nil || fe.GetCode() != "invalid_argument" {
		t.Fatalf("non-agent caller error = %v, want invalid_argument", res.GetError())
	}
	if !strings.Contains(fe.GetMessage(), "not an agent account") {
		t.Fatalf("non-agent message = %q, want it to name the non-agent caller", fe.GetMessage())
	}
	if len(author.Calls())+len(reviewer.Calls()) != 0 {
		t.Fatalf("non-agent caller touched a provider = %d, want 0", len(author.Calls())+len(reviewer.Calls()))
	}
	if len(st.recorded) != 0 {
		t.Fatalf("non-agent caller recorded %d rows, want 0", len(st.recorded))
	}
}

// TestForgeCommentArmsStampBodies pins that both comment arms stamp the body on
// the AUTHOR client and return the matching ack arm (IssueComment vs PrComment)
// — the arms are hand-copied twins, so a missing stamp or crossed result variant
// is exactly the copy-paste bug an explicit assertion catches.
func TestForgeCommentArmsStampBodies(t *testing.T) {
	t.Run("issue", func(t *testing.T) {
		author := forge.NewFakeProvider("gh-author")
		reviewer := forge.NewFakeProvider("gh-reviewer")
		svc, _ := newForgeServiceForTest(t, author, reviewer)
		call := &compassv1internal.ForgeCallRequest{
			Call: &compassv1internal.ForgeCallRequest_CommentOnIssue{CommentOnIssue: &compassv1internal.CommentOnIssueRequest{
				Repo: testRepo, IssueNumber: 3, Body: "hi",
			}},
		}
		res := svc.ExecuteForgeCallAsAccountMust(t, call)
		if res.GetIssueComment() == nil {
			t.Fatalf("comment_on_issue returned no IssueComment arm: %v", res.GetError())
		}
		if len(author.Calls()) != 1 {
			t.Fatalf("comment_on_issue author calls = %d, want 1", len(author.Calls()))
		}
		if n := strings.Count(author.Calls()[0].Body, ownerHeaderSentinel); n != 1 {
			t.Fatalf("comment body carries %d owner headers, want exactly 1", n)
		}
	})
	t.Run("pull_request", func(t *testing.T) {
		author := forge.NewFakeProvider("gh-author")
		reviewer := forge.NewFakeProvider("gh-reviewer")
		svc, _ := newForgeServiceForTest(t, author, reviewer)
		call := &compassv1internal.ForgeCallRequest{
			Call: &compassv1internal.ForgeCallRequest_CommentOnPullRequest{CommentOnPullRequest: &compassv1internal.CommentOnPullRequestRequest{
				Repo: testRepo, PullNumber: 4, Body: "hi",
			}},
		}
		res := svc.ExecuteForgeCallAsAccountMust(t, call)
		if res.GetPrComment() == nil {
			t.Fatalf("comment_on_pull_request returned no PrComment arm: %v", res.GetError())
		}
		if len(author.Calls()) != 1 {
			t.Fatalf("comment_on_pull_request author calls = %d, want 1", len(author.Calls()))
		}
		if n := strings.Count(author.Calls()[0].Body, ownerHeaderSentinel); n != 1 {
			t.Fatalf("PR-comment body carries %d owner headers, want exactly 1", n)
		}
	})
}

// TestForgeBudgetExhaustedAnd429MapToResourceExhausted pins the rate-limit arm
// of the single error-mapping function: both the ErrBudgetExhausted sentinel and
// a *StatusError{429} flatten to an in-band resource_exhausted.
func TestForgeBudgetExhaustedAnd429MapToResourceExhausted(t *testing.T) {
	run := func(scripted error) *compassv1internal.ForgeCallError {
		author := forge.NewFakeProvider("gh-author")
		reviewer := forge.NewFakeProvider("gh-reviewer")
		svc, _ := newForgeServiceForTest(t, author, reviewer)
		author.SetError("CreateIssue", scripted)
		return svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("b", "")).GetError()
	}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"budget_sentinel", forge.ErrBudgetExhausted},
		{"status_429", &forge.StatusError{Status: 429, Message: "rate limited"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fe := run(tc.err)
			if fe == nil || fe.GetCode() != "resource_exhausted" {
				t.Fatalf("%s error = %v, want resource_exhausted", tc.name, fe)
			}
		})
	}
}

// TestForgeRecordFailureAfterProviderSuccessIsInternal pins the record-after-
// success fault path: the forge create SUCCEEDED (one provider call) but the
// DL-055 row+memo write failed, so the call returns an in-band internal error
// and NO row lands — documenting the inherent F3 consequence that a retry then
// re-attempts (the artifact already exists forge-side, so a duplicate is
// possible; the memo, not the row, is what a successful retry would dedup).
func TestForgeRecordFailureAfterProviderSuccessIsInternal(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, st := newForgeServiceForTest(t, author, reviewer)
	author.CreateIssueResult = forge.Issue{Number: 7}
	st.recErr = errors.New("record: db unavailable")

	res := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("b", "req-1"))
	fe := res.GetError()
	if fe == nil || fe.GetCode() != "internal" {
		t.Fatalf("record-after-success error = %v, want internal", fe)
	}
	if len(author.Calls()) != 1 {
		t.Fatalf("provider calls = %d, want 1 (the create ran before record failed)", len(author.Calls()))
	}
	if len(st.recorded) != 0 {
		t.Fatalf("recorded rows = %d, want 0 (record failed)", len(st.recorded))
	}
}

// --- small helpers ----------------------------------------------------------

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
