//go:build unix

package server

// Default-lane (no database) tests for the DL-050 forge-write chokepoint. Driven
// against forge.FakeProvider (author + reviewer, so F1 dispatch is observable) and a
// faithful in-memory forgeStore modeling caller→Author resolution and the F3 memo +
// DL-055 row. The real-Postgres contract is in the store pgtest suite.

import (
	"context"
	"errors"
	"fmt"
	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/mirror"
	"github.com/RigelBuild/compass/go/internal/refs"
	"github.com/RigelBuild/compass/go/internal/store"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ownerHeaderSentinel is the one literal owner-header marker StampOwner writes;
// a stamped body carries exactly one, a stripped body carries none.
const ownerHeaderSentinel = "<!-- compass:owner "

// --- fake store -------------------------------------------------------------

// Models caller resolution, write intents (one non-failed attempt per agent and key), and ownership rows.
type fakeForgeStore struct {
	accounts map[store.AccountID]store.Account
	intents  []store.WriteIntent
	recorded []store.AuthoredArtifact
	// beforeHold runs inside HoldWriteIntent before the key is read, to interleave another call.
	beforeHold func()
	getErr     error // if set, GetAccount returns it verbatim
	recErr     error // if set, every store write after the forge create returns it verbatim
	// clock stamps a new intent's CreatedAt.
	clock func() time.Time

	prCreates []prCreate

	// Records transition order and can force a memo-write fault.
	transitions []recordedTransition
	transErr    error

	// DL-053 subscriptions: subs is keyed by subscription id; subKey indexes the
	// UNIQUE (agent, coordinate) to its existing id, as the store upsert does.
	subs          map[string]store.AgentForgeSubscription
	subKey        map[string]string
	nextSub       int
	subErr        error
	delErr        error
	scopes        map[string]bool
	scopeErr      error
	scopeCalls    int
	scopeArgs     []string
	mirrorModes   map[string]store.ForgeMirrorMode
	mirrorModeErr error
	prRows        []store.PullRequestRow
	prErr         error
}

// recordedTransition is one RecordStateTransition the fake saw: the full
// argument list, so a test can assert the coordinate, the APPLIED portable
// state, the acting agent, and the clock the chokepoint stamped.
type recordedTransition struct {
	provider store.ForgeProvider
	host     string
	repo     string
	kind     store.ForgeArtifactKind
	number   uint64
	state    string
	agent    store.AccountID
	at       time.Time
}

func newFakeForgeStore() *fakeForgeStore {
	return &fakeForgeStore{
		accounts:    make(map[store.AccountID]store.Account),
		clock:       nowStub,
		subs:        make(map[string]store.AgentForgeSubscription),
		subKey:      make(map[string]string),
		mirrorModes: make(map[string]store.ForgeMirrorMode),
	}
}

// subCoordKey builds the UNIQUE (agent, coordinate) index key the real store's
// ON CONFLICT constrains on, so a repeat subscribe re-lands on the same id.
func subCoordKey(agent store.AccountID, sub store.AgentForgeSubscription) string {
	return fmt.Sprintf("%s|%d|%s|%s|%d|%d|%s", agent, sub.Provider, sub.Host, sub.Repo, sub.Kind, sub.Number, sub.Project)
}

// EnsureAgentForgeSubscription mirrors the real store's idempotent upsert: a
// repeat (agent, coordinate) returns the stored id; a new coordinate mints one.
func (f *fakeForgeStore) EnsureAgentForgeSubscription(_ context.Context, sub store.AgentForgeSubscription) (string, error) {
	if f.subErr != nil {
		return "", f.subErr
	}
	key := subCoordKey(sub.AgentAccountID, sub)
	if id, ok := f.subKey[key]; ok {
		return id, nil
	}
	f.nextSub++
	id := fmt.Sprintf("sub-%d", f.nextSub)
	sub.ID = id
	f.subs[id] = sub
	f.subKey[key] = id
	return id, nil
}

// DeleteAgentForgeSubscription mirrors the real store's id+agent-scoped delete:
// an unknown id, or one owned by another agent, is ErrNotFound.
func (f *fakeForgeStore) DeleteAgentForgeSubscription(_ context.Context, agent store.AccountID, subscriptionID string) error {
	if f.delErr != nil {
		return f.delErr
	}
	sub, ok := f.subs[subscriptionID]
	if !ok || sub.AgentAccountID != agent {
		return store.ErrNotFound
	}
	delete(f.subs, subscriptionID)
	delete(f.subKey, subCoordKey(agent, sub))
	return nil
}

func (f *fakeForgeStore) HasForgeScope(_ context.Context, accountID store.AccountID, provider store.ForgeProvider, host, repo string) (bool, error) {
	f.scopeCalls++
	f.scopeArgs = append(f.scopeArgs, fmt.Sprintf("%s|%d|%s|%s", accountID, provider, host, repo))
	if f.scopeErr != nil {
		return false, f.scopeErr
	}
	key := fmt.Sprintf("%s|%d|%s|%s", accountID, provider, host, repo)
	owner := f.accounts[accountID].Agent
	if owner != nil {
		ownerKey := fmt.Sprintf("%s|%d|%s|%s", owner.OwnerUserID, provider, host, repo)
		if f.scopes[ownerKey] || f.scopes[fmt.Sprintf("%s|%d|%s|*", owner.OwnerUserID, provider, host)] {
			return true, nil
		}
	}
	return f.scopes[key] || f.scopes[fmt.Sprintf("%s|%d|%s|*", accountID, provider, host)], nil
}

func (f *fakeForgeStore) ForgeMirrorMode(_ context.Context, provider store.ForgeProvider, host, repo string) (store.ForgeMirrorMode, error) {
	if f.mirrorModeErr != nil {
		return store.ForgeMirrorOff, f.mirrorModeErr
	}
	return f.mirrorModes[fmt.Sprintf("%d|%s|%s", provider, host, repo)], nil
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

// prCreate is one CreatePullRequestWithLink call the fake saw.
type prCreate struct {
	row  store.PullRequestRow
	link *store.ForgeCoord
}

// CreatePullRequestWithLink records the DL-055 row and keeps the PR row and
// link for assertions.
func (f *fakeForgeStore) CreatePullRequestWithLink(_ context.Context, a store.AuthoredArtifact, pr store.PullRequestRow, issue *store.ForgeCoord) error {
	if f.recErr != nil {
		return f.recErr
	}
	f.recordArtifact(a)
	f.prCreates = append(f.prCreates, prCreate{row: pr, link: issue})
	return nil
}

// RecordWriteIntent returns the agent's non-failed attempt for the key, or
// inserts a pending one, as the partial unique index does.
func (f *fakeForgeStore) RecordWriteIntent(_ context.Context, in store.WriteIntent) (store.WriteIntent, error) {
	if in.RequestKey == "" {
		return store.WriteIntent{}, fmt.Errorf("%w: client request id is required", store.ErrInvalidArgument)
	}
	for _, w := range f.intents {
		if w.AgentAccountID == in.AgentAccountID && w.RequestKey == in.RequestKey && w.State != store.WriteStateFailed {
			return w, nil
		}
	}
	in.State = store.WriteStatePending
	in.CreatedAt = f.clock()
	in.ForgeKey = ""
	f.intents = append(f.intents, in)
	return in, nil
}

func (f *fakeForgeStore) HoldWriteIntent(_ context.Context, in store.WriteIntent) (store.WriteIntent, error) {
	if f.beforeHold != nil {
		f.beforeHold()
	}
	for _, w := range f.intents {
		if w.AgentAccountID == in.AgentAccountID && w.RequestKey == in.RequestKey && w.State != store.WriteStateFailed {
			return w, nil
		}
	}
	in.State = store.WriteStateHeld
	in.CreatedAt = f.clock()
	f.intents = append(f.intents, in)
	return in, nil
}

func (f *fakeForgeStore) PromoteWriteIntent(_ context.Context, id string) error {
	for i := range f.intents {
		if f.intents[i].ID == id && f.intents[i].State == store.WriteStateHeld {
			f.intents[i].State = store.WriteStatePending
			return nil
		}
	}
	return fmt.Errorf("%w: write intent %q is not held", store.ErrConflict, id)
}

// CompleteWriteIntent writes the ownership row (issue, PR) or keeps the forge
// key (comment, review) and marks the pending attempt done.
func (f *fakeForgeStore) CompleteWriteIntent(_ context.Context, id string, artifact *store.AuthoredArtifact, forgeKey string) error {
	if f.recErr != nil {
		return f.recErr
	}
	w, err := f.intent(id)
	if err != nil {
		return err
	}
	if w.State != store.WriteStatePending {
		return fmt.Errorf("%w: write intent %q is %s", store.ErrConflict, id, w.State)
	}
	if artifact != nil {
		f.recordArtifact(*artifact)
		forgeKey = strconv.FormatUint(artifact.Number, 10)
	}
	w.State = store.WriteStateDone
	w.ForgeKey = forgeKey
	return nil
}

// EndWriteIntent moves a pending attempt to failed or lost.
func (f *fakeForgeStore) EndWriteIntent(_ context.Context, id string, state store.WriteState) error {
	w, err := f.intent(id)
	if err != nil {
		return err
	}
	if w.State != store.WriteStatePending {
		return fmt.Errorf("%w: write intent %q is no longer pending", store.ErrConflict, id)
	}
	w.State = state
	return nil
}

// PendingWriteIntents lists pending attempts created before olderThan.
func (f *fakeForgeStore) PendingWriteIntents(_ context.Context, olderThan time.Time, limit int) ([]store.WriteIntent, error) {
	var out []store.WriteIntent
	for _, w := range f.intents {
		if w.State == store.WriteStatePending && w.CreatedAt.Before(olderThan) && len(out) < limit {
			out = append(out, w)
		}
	}
	return out, nil
}

// DeleteExpiredHeldWriteIntents drops held keys created before olderThan; the
// fake keeps insertion order, which is oldest first.
func (f *fakeForgeStore) DeleteExpiredHeldWriteIntents(_ context.Context, olderThan time.Time, limit int) (int64, error) {
	var n int64
	kept := f.intents[:0]
	for _, w := range f.intents {
		if w.State == store.WriteStateHeld && w.CreatedAt.Before(olderThan) && n < int64(limit) {
			n++
			continue
		}
		kept = append(kept, w)
	}
	f.intents = kept
	return n, nil
}

func (f *fakeForgeStore) AuthoredArtifactByCoordinateFold(_ context.Context, provider store.ForgeProvider, host, repo string, kind store.ForgeArtifactKind, number uint64) (store.AuthoredArtifact, error) {
	for _, r := range f.recorded {
		if r.Provider == provider && r.Host == host && strings.EqualFold(r.Repo, repo) && r.Kind == kind && r.Number == number {
			return r, nil
		}
	}
	return store.AuthoredArtifact{}, store.ErrNotFound
}

// RecordStateTransition mirrors the real store's upsert-latest-wins memo write:
// it appends to an ordered log so a test can prove the memo landed STRICTLY
// AFTER the provider call (and never at all when the provider failed).
func (f *fakeForgeStore) RecordStateTransition(_ context.Context, provider store.ForgeProvider, host, repo string, kind store.ForgeArtifactKind, number uint64, state string, agent store.AccountID, at time.Time) error {
	if f.transErr != nil {
		return f.transErr
	}
	f.transitions = append(f.transitions, recordedTransition{
		provider: provider, host: host, repo: repo, kind: kind,
		number: number, state: state, agent: agent, at: at,
	})
	return nil
}

func (f *fakeForgeStore) PullRequestsByCoords(_ context.Context, _ []store.ForgeCoord) ([]store.PullRequestRow, error) {
	return f.prRows, f.prErr
}

// recordArtifact upserts an ownership row by coordinate, as the real store's
// write-once authorship insert does.
func (f *fakeForgeStore) recordArtifact(a store.AuthoredArtifact) {
	for i, r := range f.recorded {
		if r.Provider == a.Provider && r.Host == a.Host && strings.EqualFold(r.Repo, a.Repo) && r.Kind == a.Kind && r.Number == a.Number {
			f.recorded[i].SessionID = a.SessionID
			return
		}
	}
	f.recorded = append(f.recorded, a)
}

func (f *fakeForgeStore) intent(id string) (*store.WriteIntent, error) {
	for i := range f.intents {
		if f.intents[i].ID == id {
			return &f.intents[i], nil
		}
	}
	return nil, fmt.Errorf("%w: write intent %q", store.ErrNotFound, id)
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
	if err := reg.register(forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, host: testHost}, author, reviewer, true); err != nil {
		t.Fatalf("register: %v", err)
	}

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
	res := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall(forged, "k-384"))
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
		ClientRequestId: "k-414",
		Call: &compassv1internal.ForgeCallRequest_SubmitReview{SubmitReview: &compassv1internal.SubmitReviewRequest{
			Repo: testRepo, PrNumber: 7, Verdict: "comment", Body: "looks good",
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
		ClientRequestId: "k-455",
		Call:            &compassv1internal.ForgeCallRequest_GetIssue{GetIssue: &compassv1internal.GetIssueRequest{Repo: testRepo, IssueNumber: 9}},
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

// list_issues honors its wire contract: limit 0 returns the default 30, a set
// limit returns at most that many, and a limit past 100 is capped. The limit
// reaches the provider so it stops paging instead of walking every page.
func TestForgeListIssuesAppliesLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit uint32
		want  int
	}{
		{"zero uses default 30", 0, 30},
		{"explicit limit", 5, 5},
		{"over the cap is 100", 500, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			author := forge.NewFakeProvider("gh-author")
			svc, _ := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
			for i := range 150 {
				author.ListIssuesResult = append(author.ListIssuesResult, forge.Issue{Number: uint64(150 - i)})
			}
			call := &compassv1internal.ForgeCallRequest{
				ClientRequestId: "k-490",
				Call:            &compassv1internal.ForgeCallRequest_ListIssues{ListIssues: &compassv1internal.ListIssuesRequest{Repo: testRepo, Limit: tc.limit}},
			}
			issues := svc.ExecuteForgeCallAsAccountMust(t, call).GetIssues().GetIssues()
			if len(issues) != tc.want {
				t.Fatalf("list_issues limit=%d returned %d issues, want %d", tc.limit, len(issues), tc.want)
			}
			if issues[0].GetNumber() != 150 {
				t.Fatalf("first issue = #%d, want #150: the clamp must keep the provider's leading rows", issues[0].GetNumber())
			}
			if got := author.Calls()[0].Filter.Limit; got != tc.want {
				t.Fatalf("provider filter Limit = %d, want %d: the provider must stop paging at the limit", got, tc.want)
			}
		})
	}
}

// list_issues with no state asks the provider for open issues, per the gateway
// contract; GitHub's own empty-state default is "all", which returned closed rows.
func TestForgeListIssuesEmptyStateMeansOpen(t *testing.T) {
	for _, tc := range []struct{ state, want string }{
		{"", "open"},
		{"closed", "closed"},
		{"all", "all"},
	} {
		t.Run("state="+tc.state, func(t *testing.T) {
			author := forge.NewFakeProvider("gh-author")
			svc, _ := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
			call := &compassv1internal.ForgeCallRequest{
				ClientRequestId: "k-518",
				Call:            &compassv1internal.ForgeCallRequest_ListIssues{ListIssues: &compassv1internal.ListIssuesRequest{Repo: testRepo, State: tc.state}},
			}
			svc.ExecuteForgeCallAsAccountMust(t, call)
			calls := author.Calls()
			if len(calls) != 1 || calls[0].Filter.State != tc.want {
				t.Fatalf("provider calls = %+v, want one ListIssues with State %q", calls, tc.want)
			}
		})
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

	_, err := svc.ExecuteForgeCallAsAccount(context.Background(), "", testSessionID, createIssueCall("b", "k-540"))
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
		ClientRequestId: "k-570",
		Call:            &compassv1internal.ForgeCallRequest_CreateIssue{CreateIssue: &compassv1internal.CreateIssueRequest{Repo: "", Body: "b"}},
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

func TestForgeOwnerInstallationErrorsMapToExpectedCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		// The router's shape: the resolver's no-installation error inside the lookup wrapper.
		{name: "no installation", err: fmt.Errorf("%w: %w", forge.ErrInstallationLookup, fmt.Errorf("forge: owner %q: %w", "owner-b", forge.ErrNoInstallation)), want: "not_found"},
		{name: "malformed repo", err: fmt.Errorf("%w: %q", forge.ErrMalformedRepo, "no-slash"), want: "invalid_argument"},
		{name: "lookup unavailable", err: fmt.Errorf("%w: %w", forge.ErrInstallationLookup, errors.New("refresh throttled")), want: "unavailable"},
		{name: "app credential failure", err: fmt.Errorf("%w: %w", forge.ErrInstallationLookup, forge.ErrAppCredentials), want: "internal"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			author := forge.NewFakeProvider("gh-author")
			reviewer := forge.NewFakeProvider("gh-reviewer")
			svc, _ := newForgeServiceForTest(t, author, reviewer)
			author.SetError("GetIssue", tc.err)
			result := svc.ExecuteForgeCallAsAccountMust(t, &compassv1internal.ForgeCallRequest{
				Call: &compassv1internal.ForgeCallRequest_GetIssue{GetIssue: &compassv1internal.GetIssueRequest{Repo: testRepo, IssueNumber: 1}},
			})
			if got := result.GetError().GetCode(); got != tc.want {
				t.Fatalf("error code = %q, want %q (%v)", got, tc.want, tc.err)
			}
		})
	}
}
func TestForgeStatusError403And404FlattenToByteIdenticalNotFound(t *testing.T) {
	run := func(status int) *compassv1internal.ForgeCallError {
		author := forge.NewFakeProvider("gh-author")
		reviewer := forge.NewFakeProvider("gh-reviewer")
		svc, _ := newForgeServiceForTest(t, author, reviewer)
		author.SetError("GetIssue", &forge.StatusError{Status: status, Message: "secret-" + itoa(status)})
		call := &compassv1internal.ForgeCallRequest{
			ClientRequestId: "k-593",
			Call:            &compassv1internal.ForgeCallRequest_GetIssue{GetIssue: &compassv1internal.GetIssueRequest{Repo: testRepo, IssueNumber: 1}},
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

	res := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("b", "k-620"))
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

	fe := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("b", "k-638")).GetError()
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
	// A GitHub body also reserves the write marker after the owner header.
	marker := len(forge.WithWriteMarker("", "00000000-0000-0000-0000-000000000000"))
	fit := strings.Repeat("x", limit-len(hdr)-marker)
	over := fit + "x"

	t.Run("exactly at budget stamps and calls provider", func(t *testing.T) {
		author := forge.NewFakeProvider("gh-author")
		reviewer := forge.NewFakeProvider("gh-reviewer")
		author.BodyLimitResult = limit
		svc, _ := newForgeServiceForTest(t, author, reviewer)
		res := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall(fit, "k-669"))
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
		res := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall(over, "k-683"))
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
		ClientRequestId: "k-775",
		Call: &compassv1internal.ForgeCallRequest_SubmitReview{SubmitReview: &compassv1internal.SubmitReviewRequest{
			Repo: testRepo, PrNumber: 1, Verdict: "approve", Body: "ok",
		}},
	}
	if res := svc.ExecuteForgeCallAsAccountMust(t, review); res.GetError() != nil {
		t.Fatalf("submit_review errored: %v", res.GetError())
	}
	if len(reviewer.Calls()) != 1 || len(author.Calls()) != 0 {
		t.Fatalf("submit_review dispatch: reviewer=%d author=%d, want reviewer=1 author=0", len(reviewer.Calls()), len(author.Calls()))
	}

	if res := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("b", "k-787")); res.GetError() != nil {
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

// The PR-create path must stamp the author body once and record a PullRequest
// row with its returned coordinate and kind.
func TestForgeCreatePullRequestStampsAndRecordsPRKind(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, st := newForgeServiceForTest(t, author, reviewer)
	author.CreatePRResult = forge.PullRequest{Number: 55, URL: "u"}

	res := svc.ExecuteForgeCallAsAccountMust(t, createPRCall("body", "k-811"))
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

// Retrying with a create_issue request ID on create_pull_request is a different
// target, so it is refused with no provider call rather than replaying the issue.
func TestForgeCreateRetryMismatchedArmIsRefused(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, st := newForgeServiceForTest(t, author, reviewer)
	author.CreateIssueResult = forge.Issue{Number: 42, URL: "u"}

	first := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("b", "req-x"))
	if first.GetError() != nil || first.GetIssue().GetNumber() != 42 {
		t.Fatalf("first create_issue = %v, want issue 42", first)
	}

	second := svc.ExecuteForgeCallAsAccountMust(t, createPRCall("different", "req-x"))
	if fe := second.GetError(); fe == nil || fe.GetCode() != "failed_precondition" || !strings.Contains(fe.GetMessage(), "another target") {
		t.Fatalf("mismatched-arm retry = %v, want failed_precondition naming another target", second)
	}
	if len(author.Calls()) != 1 {
		t.Fatalf("provider calls total = %d, want 1 (the refused retry makes none)", len(author.Calls()))
	}
	if len(st.recorded) != 1 {
		t.Fatalf("recorded rows = %d, want 1 (no second row)", len(st.recorded))
	}
}

// A non-agent caller cannot create a stamped artifact: reject in-band before
// stamping or calling the provider.
func TestForgeNonAgentCallerIsInvalidArgumentZeroProviderCalls(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, st := newForgeServiceForTest(t, author, reviewer)

	const nonAgentID = store.AccountID("acct-user")
	st.accounts[nonAgentID] = store.Account{ID: nonAgentID, Handle: "auser", User: &store.UserAccount{}}

	res, err := svc.ExecuteForgeCallAsAccount(context.Background(), nonAgentID, testSessionID, createIssueCall("b", "k-884"))
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
			ClientRequestId: "k-912",
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
			ClientRequestId: "k-932",
			Call: &compassv1internal.ForgeCallRequest_CommentOnPullRequest{CommentOnPullRequest: &compassv1internal.CommentOnPullRequestRequest{
				Repo: testRepo, PrNumber: 4, Body: "hi",
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

// Budget exhaustion and HTTP 429 map to resource_exhausted. Rate-limit hints are
// clamped into retry_after_ms; absent or nonpositive hints remain zero.
func TestForgeBudgetExhaustedAnd429MapToResourceExhausted(t *testing.T) {
	run := func(scripted error) *compassv1internal.ForgeCallError {
		author := forge.NewFakeProvider("gh-author")
		reviewer := forge.NewFakeProvider("gh-reviewer")
		svc, _ := newForgeServiceForTest(t, author, reviewer)
		author.SetError("CreateIssue", scripted)
		return svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("b", "k-962")).GetError()
	}
	for _, tc := range []struct {
		name        string
		err         error
		wantRetryMs uint32
	}{
		{"budget_sentinel", forge.ErrBudgetExhausted, 0},
		{"status_429", &forge.StatusError{Status: 429, Message: "rate limited"}, 0},
		{"ratelimit_hint", fmt.Errorf("x: %w", &forge.RateLimitError{RetryAfter: 90 * time.Second}), 90000},
		{"ratelimit_zero_hint", &forge.RateLimitError{RetryAfter: 0}, 0},
		{"ratelimit_negative_hint", &forge.RateLimitError{RetryAfter: -5 * time.Second}, 0},
		{"ratelimit_oversized_hint", &forge.RateLimitError{RetryAfter: (math.MaxUint32 + 1) * time.Millisecond}, math.MaxUint32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fe := run(tc.err)
			if fe == nil || fe.GetCode() != "resource_exhausted" {
				t.Fatalf("%s error = %v, want resource_exhausted", tc.name, fe)
			}
			if fe.GetRetryAfterMs() != tc.wantRetryMs {
				t.Errorf("%s RetryAfterMs = %d, want %d", tc.name, fe.GetRetryAfterMs(), tc.wantRetryMs)
			}
		})
	}
}

// If provider creation succeeds but its row/memo write fails, return internal and
// persist no row. A retry may create a duplicate because the memo was not stored.
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

// --- tests: DL-053 subscribe/unsubscribe arms (RIG-2732 Piece 1) -------------

// subscribeCall builds a Subscribe request against the default GitHub coordinate.
func subscribeCall(kind compassv1internal.ForgeArtifactKind, number uint64) *compassv1internal.ForgeCallRequest {
	return &compassv1internal.ForgeCallRequest{
		Call: &compassv1internal.ForgeCallRequest_Subscribe{Subscribe: &compassv1internal.SubscribeForgeRequest{
			Repo: testRepo, Kind: kind, Number: number,
		}},
	}
}

// unsubscribeCall builds an Unsubscribe request by subscription id.
func unsubscribeCall(subscriptionID string) *compassv1internal.ForgeCallRequest {
	return &compassv1internal.ForgeCallRequest{
		Call: &compassv1internal.ForgeCallRequest_Unsubscribe{Unsubscribe: &compassv1internal.UnsubscribeForgeRequest{
			SubscriptionId: subscriptionID,
		}},
	}
}

// TestForgeSubscribeReturnsIdAndIsIdempotent pins the subscribe arm: a subscribe
// returns a non-empty subscription id, and a REPEAT subscribe to the same
// artifact returns the SAME id (the store upsert dedups on the UNIQUE
// coordinate) — not a fresh row, not an error.
func TestForgeSubscribeReturnsIdAndIsIdempotent(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, st := newForgeServiceForTest(t, author, reviewer)

	res := svc.ExecuteForgeCallAsAccountMust(t, subscribeCall(compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE, 42))
	sub := res.GetSubscribed()
	if sub == nil || sub.GetSubscriptionId() == "" {
		t.Fatalf("subscribe result = %v, want a subscription id", res.GetResult())
	}
	first := sub.GetSubscriptionId()

	res2 := svc.ExecuteForgeCallAsAccountMust(t, subscribeCall(compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE, 42))
	if got := res2.GetSubscribed().GetSubscriptionId(); got != first {
		t.Fatalf("repeat subscribe id = %q, want %q (idempotent)", got, first)
	}
	if len(st.subs) != 1 {
		t.Fatalf("subscription rows = %d, want 1 (no duplicate)", len(st.subs))
	}
}

// TestForgeSubscribeCarriesScopeAndProject pins the request coordinate passed to the store.
func TestForgeSubscribeCarriesScopeAndProject(t *testing.T) {
	linearRef := &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR}
	tests := []struct {
		name      string
		forge     *compassv1.ForgeRef
		scope     compassv1internal.ForgeSubscriptionScope
		project   string
		number    uint64
		wantScope store.ForgeSubscriptionScope
	}{
		{
			name:      "GitHub container",
			scope:     compassv1internal.ForgeSubscriptionScope_FORGE_SUBSCRIPTION_SCOPE_CONTAINER,
			wantScope: store.ForgeSubscriptionScopeContainer,
		},
		{
			name:      "Linear project container",
			forge:     linearRef,
			scope:     compassv1internal.ForgeSubscriptionScope_FORGE_SUBSCRIPTION_SCOPE_CONTAINER,
			project:   "proj-alpha",
			wantScope: store.ForgeSubscriptionScopeContainer,
		},
		{
			name:      "unspecified scope keeps artifact number",
			number:    7,
			wantScope: store.ForgeSubscriptionScopeUnspecified,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			author := forge.NewFakeProvider("gh-author")
			reviewer := forge.NewFakeProvider("gh-reviewer")
			svc, st := newForgeServiceForTest(t, author, reviewer)
			registerLinearForTest(t, svc)

			call := subscribeCall(compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE, tt.number)
			call.Forge = tt.forge
			call.GetSubscribe().Scope = tt.scope
			call.GetSubscribe().Project = tt.project
			res := svc.ExecuteForgeCallAsAccountMust(t, call)
			sub := res.GetSubscribed()
			if sub == nil || sub.GetSubscriptionId() == "" {
				t.Fatalf("subscribe result = %v, want a subscription id", res.GetResult())
			}

			got, ok := st.subs[sub.GetSubscriptionId()]
			if !ok {
				t.Fatalf("recorded subscription %q not found", sub.GetSubscriptionId())
			}
			if got.Number != tt.number {
				t.Errorf("recorded number = %d, want %d", got.Number, tt.number)
			}
			if got.Scope != tt.wantScope {
				t.Errorf("recorded scope = %d, want %d", got.Scope, tt.wantScope)
			}
			if got.Project != tt.project {
				t.Errorf("recorded project = %q, want %q", got.Project, tt.project)
			}
		})
	}
}

// TestForgeSubscribeLinearPullRequestIsInvalidArgument rejects a subscription Linear can never notify.
func TestForgeSubscribeLinearPullRequestIsInvalidArgument(t *testing.T) {
	svc, st := newForgeServiceForTest(t, forge.NewFakeProvider("gh-author"), forge.NewFakeProvider("gh-reviewer"))
	registerLinearForTest(t, svc)

	call := subscribeCall(compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST, 3)
	call.Forge = &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR}
	res := svc.ExecuteForgeCallAsAccountMust(t, call)
	if code := res.GetError().GetCode(); code != "invalid_argument" {
		t.Fatalf("error code = %q (result %v), want invalid_argument", code, res.GetResult())
	}
	if len(st.subs) != 0 {
		t.Fatalf("recorded subscriptions = %d, want 0", len(st.subs))
	}
}

// registerLinearForTest adds the Linear coordinate beside the default GitHub one.
func registerLinearForTest(t *testing.T, svc *forgeService) {
	t.Helper()
	if err := registerLinearForgeCoordinate(svc.providers, forge.NewFakeProvider("linear")); err != nil {
		t.Fatalf("register linear: %v", err)
	}
}

// TestForgeSubscribeUnspecifiedKindIsInvalidArgument pins the kind guard: a
// subscribe with an UNSPECIFIED kind is an in-band invalid_argument with no row
// written — the arm rejects the zero kind before the store.
func TestForgeSubscribeUnspecifiedKindIsInvalidArgument(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, st := newForgeServiceForTest(t, author, reviewer)

	res := svc.ExecuteForgeCallAsAccountMust(t, subscribeCall(compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_UNSPECIFIED, 1))
	if fe := res.GetError(); fe == nil || fe.GetCode() != "invalid_argument" {
		t.Fatalf("unspecified kind error = %v, want invalid_argument", res.GetError())
	}
	if len(st.subs) != 0 {
		t.Fatalf("subscription rows = %d, want 0", len(st.subs))
	}
}

// TestForgeSubscribeEmptyRepoIsInvalidArgument pins that an empty repo is an
// in-band invalid_argument before any store touch (resolveTarget guard).
func TestForgeSubscribeEmptyRepoIsInvalidArgument(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, st := newForgeServiceForTest(t, author, reviewer)

	call := &compassv1internal.ForgeCallRequest{
		ClientRequestId: "k-1170",
		Call: &compassv1internal.ForgeCallRequest_Subscribe{Subscribe: &compassv1internal.SubscribeForgeRequest{
			Repo: "", Kind: compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE, Number: 1,
		}},
	}
	res := svc.ExecuteForgeCallAsAccountMust(t, call)
	if fe := res.GetError(); fe == nil || fe.GetCode() != "invalid_argument" {
		t.Fatalf("empty repo error = %v, want invalid_argument", res.GetError())
	}
	if len(st.subs) != 0 {
		t.Fatalf("subscription rows = %d, want 0", len(st.subs))
	}
}

// TestForgeUnsubscribeSucceedsThenNotFound pins the unsubscribe arm: an existing
// subscription id unsubscribes to the Unsubscribed arm and removes the row; a
// repeat (now-unknown) id is an in-band not_found — never a Connect teardown.
func TestForgeUnsubscribeSucceedsThenNotFound(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, st := newForgeServiceForTest(t, author, reviewer)

	sub := svc.ExecuteForgeCallAsAccountMust(t, subscribeCall(compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST, 7)).GetSubscribed()
	id := sub.GetSubscriptionId()

	res := svc.ExecuteForgeCallAsAccountMust(t, unsubscribeCall(id))
	if res.GetUnsubscribed() == nil {
		t.Fatalf("unsubscribe result = %v, want Unsubscribed", res.GetResult())
	}
	if len(st.subs) != 0 {
		t.Fatalf("subscription rows = %d, want 0 after unsubscribe", len(st.subs))
	}

	again := svc.ExecuteForgeCallAsAccountMust(t, unsubscribeCall(id))
	if fe := again.GetError(); fe == nil || fe.GetCode() != "not_found" {
		t.Fatalf("repeat unsubscribe error = %v, want not_found", again.GetError())
	}
}

// TestForgeUnsubscribeBogusIdIsInbandNotFound pins that an unknown subscription
// id is an in-band not_found ForgeCallError, NOT a Connect error.
func TestForgeUnsubscribeBogusIdIsInbandNotFound(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, _ := newForgeServiceForTest(t, author, reviewer)

	res := svc.ExecuteForgeCallAsAccountMust(t, unsubscribeCall("no-such-sub"))
	if fe := res.GetError(); fe == nil || fe.GetCode() != "not_found" {
		t.Fatalf("bogus unsubscribe error = %v, want not_found", res.GetError())
	}
}

// Store failures in either subscription path return an in-band internal error,
// not a Connect stream teardown.
func TestForgeSubscribeUnsubscribeStoreFaultIsInbandInternal(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, st := newForgeServiceForTest(t, author, reviewer)

	st.subErr = errors.New("boom: subscribe store fault")
	subRes := svc.ExecuteForgeCallAsAccountMust(t, subscribeCall(compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE, 42))
	if fe := subRes.GetError(); fe == nil || fe.GetCode() != "internal" {
		t.Fatalf("subscribe store-fault error = %v, want in-band internal", subRes.GetError())
	}

	st.delErr = errors.New("boom: unsubscribe store fault")
	delRes := svc.ExecuteForgeCallAsAccountMust(t, unsubscribeCall("any-id"))
	if fe := delRes.GetError(); fe == nil || fe.GetCode() != "internal" {
		t.Fatalf("unsubscribe store-fault error = %v, want in-band internal", delRes.GetError())
	}
}

// scopedWriteCalls holds one request per coordinate write arm, keyed by oneof field name.
func editIssueCall(title string) *compassv1internal.ForgeCallRequest {
	return &compassv1internal.ForgeCallRequest{
		Call: &compassv1internal.ForgeCallRequest_EditIssue{EditIssue: &compassv1internal.EditIssueRequest{
			Repo: testRepo, IssueNumber: 1, Title: &title,
		}},
	}
}

func scopedWriteCalls() map[string]*compassv1internal.ForgeCallRequest {
	return map[string]*compassv1internal.ForgeCallRequest{
		"create_issue":                  createIssueCall("body", "k-1248"),
		"create_pull_request":           createPRCall("body", "k-1249"),
		"comment_on_issue":              {ClientRequestId: "k-ci", Call: &compassv1internal.ForgeCallRequest_CommentOnIssue{CommentOnIssue: &compassv1internal.CommentOnIssueRequest{Repo: testRepo, IssueNumber: 1, Body: "body"}}},
		"comment_on_pull_request":       {ClientRequestId: "k-cp", Call: &compassv1internal.ForgeCallRequest_CommentOnPullRequest{CommentOnPullRequest: &compassv1internal.CommentOnPullRequestRequest{Repo: testRepo, PrNumber: 1, Body: "body"}}},
		"submit_review":                 {ClientRequestId: "k-sr", Call: &compassv1internal.ForgeCallRequest_SubmitReview{SubmitReview: &compassv1internal.SubmitReviewRequest{Repo: testRepo, PrNumber: 1, Verdict: "approve"}}},
		"subscribe":                     subscribeCall(compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE, 1),
		"transition_issue_state":        {Call: &compassv1internal.ForgeCallRequest_TransitionIssueState{TransitionIssueState: &compassv1internal.TransitionIssueStateRequest{Repo: testRepo, IssueNumber: 1, State: "closed"}}},
		"transition_pull_request_state": {Call: &compassv1internal.ForgeCallRequest_TransitionPullRequestState{TransitionPullRequestState: &compassv1internal.TransitionPullRequestStateRequest{Repo: testRepo, PrNumber: 1, State: "closed"}}},
		"edit_issue":                    editIssueCall("new title"),
		"submit_stack":                  stackCall("k-ss", "", nil, &compassv1internal.StackEntry{HeadRef: "feature", Title: "t", Body: "body"}),
		"retire_head":                   retireCall("feature"),
	}
}

// A new ForgeCallRequest arm must be either a gated write (so the rejection test
// covers it), an explicit ungated read or caller-scoped arm, or an arm gated by
// the mirror policy (its scope refusal is a pgtest: it needs the mirror store).
func TestForgeCallArmsHaveScopeClassification(t *testing.T) {
	ungated := map[string]bool{"get_issue": true, "list_issues": true, "get_pull_request": true, "unsubscribe": true, "stack_status": true}
	mirrorGated := map[string]bool{"mirror_messages": true}
	writes := scopedWriteCalls()
	fields := (&compassv1internal.ForgeCallRequest{}).ProtoReflect().Descriptor().Oneofs().ByName("call").Fields()
	if n := len(writes) + len(ungated) + len(mirrorGated); n != fields.Len() {
		t.Fatalf("classified arms = %d, descriptor arms = %d", n, fields.Len())
	}
	for i := range fields.Len() {
		name := string(fields.Get(i).Name())
		call, isWrite := writes[name]
		classes := 0
		for _, in := range []bool{isWrite, ungated[name], mirrorGated[name]} {
			if in {
				classes++
			}
		}
		if classes != 1 {
			t.Errorf("arm %q must be exactly one of gated write, ungated, or mirror-gated", name)
			continue
		}
		if isWrite && call.ProtoReflect().WhichOneof(fields.Get(i).ContainingOneof()).Name() != fields.Get(i).Name() {
			t.Errorf("scopedWriteCalls[%q] sets a different arm", name)
		}
	}
}

func TestForgeScopeGateRejectsEveryCoordinateWrite(t *testing.T) {
	for name, call := range scopedWriteCalls() {
		t.Run(name, func(t *testing.T) {
			author := forge.NewFakeProvider("gh-author")
			reviewer := forge.NewFakeProvider("gh-reviewer")
			svc, st := newForgeServiceForTest(t, author, reviewer)
			svc.enforceScopes = true
			st.scopes = make(map[string]bool)

			result := svc.ExecuteForgeCallAsAccountMust(t, call)
			fe := result.GetError()
			if fe == nil || fe.GetCode() != "not_found" || fe.GetMessage() != "forge: artifact not found" {
				t.Fatalf("result error = %v, want fixed in-band not_found", fe)
			}
			if got := len(author.Calls()) + len(reviewer.Calls()); got != 0 {
				t.Fatalf("provider calls = %d, want 0", got)
			}
			want := fmt.Sprintf("%s|%d|%s|%s", testAgentID, store.ForgeProviderGitHub, testHost, testRepo)
			if len(st.scopeArgs) != 1 || st.scopeArgs[0] != want {
				t.Fatalf("scope checks = %v, want [%s]", st.scopeArgs, want)
			}
			if len(st.recorded) != 0 || len(st.transitions) != 0 || len(st.subs) != 0 {
				t.Fatalf("rejected write persisted state: recorded=%d transitions=%d subscriptions=%d", len(st.recorded), len(st.transitions), len(st.subs))
			}
		})
	}
}

// TestForgeScopeGateAllowsEveryGrantedWrite: an owner grant lets each write arm
// reach its provider exactly as it would with enforcement off.
func TestForgeScopeGateAllowsEveryGrantedWrite(t *testing.T) {
	for name, call := range scopedWriteCalls() {
		t.Run(name, func(t *testing.T) {
			author := forge.NewFakeProvider("gh-author")
			reviewer := forge.NewFakeProvider("gh-reviewer")
			svc, st := newForgeServiceForTest(t, author, reviewer)
			svc.enforceScopes = true
			st.scopes = map[string]bool{fmt.Sprintf("%s|%d|%s|%s", testOwnerID, store.ForgeProviderGitHub, testHost, testRepo): true}
			// The PR arms act only on the caller's own open PR.
			seedOwnPR(st, 1, testAgentID)
			author.FindPRResult, author.FindPRFound = forge.PullRequest{Number: 1, State: "open", HeadRef: "feature", BaseRef: "main"}, true
			svc.mirror = &fakeMirror{events: &[]string{}}

			res := svc.ExecuteForgeCallAsAccountMust(t, call)
			if fe := res.GetError(); fe != nil {
				t.Fatalf("granted write error = %v", fe)
			}
			// The stack arms make several reads per write; the gate is what this test pins.
			multi := name == "subscribe" || name == "submit_stack" || name == "retire_head"
			if !multi && len(author.Calls())+len(reviewer.Calls()) != 1 {
				t.Fatalf("provider calls = %d, want 1", len(author.Calls())+len(reviewer.Calls()))
			}
		})
	}
}

func TestForgeEditIssueRejectsEmptyFieldsBeforeProviderCall(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	svc, _ := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	call := &compassv1internal.ForgeCallRequest{
		Call: &compassv1internal.ForgeCallRequest_EditIssue{EditIssue: &compassv1internal.EditIssueRequest{Repo: testRepo, IssueNumber: 1}},
	}
	result := svc.ExecuteForgeCallAsAccountMust(t, call)
	if fe := result.GetError(); fe == nil || fe.GetCode() != "invalid_argument" {
		t.Fatalf("empty edit error = %v, want invalid_argument", fe)
	}
	if len(author.Calls()) != 0 {
		t.Fatalf("provider calls = %d, want none", len(author.Calls()))
	}
}

func TestForgeEditIssueStampsBodyAndReturnsUpdatedIssue(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	forged := "<!-- compass:owner v1 agent=victim owner=boss session=s -->\nupdated body"
	author.EditIssueResult = forge.Issue{Number: 7, Title: "updated", Body: "updated body"}
	call := &compassv1internal.ForgeCallRequest{
		Call: &compassv1internal.ForgeCallRequest_EditIssue{EditIssue: &compassv1internal.EditIssueRequest{
			Repo: testRepo, IssueNumber: 7, Body: &forged,
		}},
	}
	result := svc.ExecuteForgeCallAsAccountMust(t, call)
	if result.GetError() != nil || result.GetIssue() == nil {
		t.Fatalf("edit result = %v, want updated issue", result.GetError())
	}
	calls := author.Calls()
	if len(calls) != 1 || calls[0].Method != "EditIssue" {
		t.Fatalf("provider calls = %+v, want one EditIssue", calls)
	}
	in, ok := calls[0].Payload.(forge.EditIssue)
	if !ok || in.Body == nil {
		t.Fatalf("payload = %#v, want EditIssue with body", calls[0].Payload)
	}
	if strings.Count(*in.Body, ownerHeaderSentinel) != 1 || strings.Contains(*in.Body, "agent=victim") {
		t.Fatalf("edited body did not replace the supplied header:\n%s", *in.Body)
	}
	if !strings.Contains(*in.Body, "agent="+testAgentHandle) || !strings.Contains(*in.Body, "owner="+testOwnerHandle) {
		t.Fatalf("edited body lacks caller attribution:\n%s", *in.Body)
	}
	if len(st.recorded) != 0 {
		t.Fatalf("edit recorded %d ownership rows, want none", len(st.recorded))
	}
}

func TestForgeEditIssueTitleOnlyLeavesBodyUnset(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	svc, _ := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	result := svc.ExecuteForgeCallAsAccountMust(t, editIssueCall("new title"))
	if result.GetError() != nil {
		t.Fatalf("title-only edit error = %v", result.GetError())
	}
	calls := author.Calls()
	if len(calls) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(calls))
	}
	in, ok := calls[0].Payload.(forge.EditIssue)
	if !ok || in.Title == nil || *in.Title != "new title" || in.Body != nil {
		t.Fatalf("title-only input = %#v, want title and nil body", calls[0].Payload)
	}
}

// A body edit reserves the owner header against the provider limit before calling EditIssue.
func TestForgeEditIssueBodyLimitRejectsBeforeProviderCall(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	body := "four"
	header, err := forge.StampOwner("", forge.Author{
		AgentHandle: testAgentHandle,
		OwnerHandle: testOwnerHandle,
		SessionID:   testSessionID,
	}, 0)
	if err != nil {
		t.Fatalf("reference stamp: %v", err)
	}
	author.BodyLimitResult = len(header) + len(body) - 1
	svc, _ := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	call := &compassv1internal.ForgeCallRequest{
		Call: &compassv1internal.ForgeCallRequest_EditIssue{EditIssue: &compassv1internal.EditIssueRequest{
			Repo: testRepo, IssueNumber: 1, Body: &body,
		}},
	}

	result := svc.ExecuteForgeCallAsAccountMust(t, call)
	if fe := result.GetError(); fe == nil || fe.GetCode() != "invalid_argument" {
		t.Fatalf("oversized edit error = %v, want invalid_argument", fe)
	}
	for _, got := range author.Calls() {
		if got.Method == "EditIssue" {
			t.Fatalf("provider calls = %+v, want no EditIssue call", author.Calls())
		}
	}
}

func TestForgeEditIssueOutsideScopeReturnsNotFound(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	svc.enforceScopes = true
	st.scopes = map[string]bool{}
	result := svc.ExecuteForgeCallAsAccountMust(t, editIssueCall("renamed"))
	if fe := result.GetError(); fe == nil || fe.GetCode() != "not_found" || fe.GetMessage() != "forge: artifact not found" {
		t.Fatalf("out-of-scope edit = %v, want fixed not_found", fe)
	}
	if len(author.Calls()) != 0 {
		t.Fatalf("out-of-scope edit provider calls = %d, want none", len(author.Calls()))
	}
}

func TestForgeScopeGateAllowsOwnerGrantAndPreservesDefaultOff(t *testing.T) {
	t.Run("owner grant allows agent write", func(t *testing.T) {
		author := forge.NewFakeProvider("gh-author")
		reviewer := forge.NewFakeProvider("gh-reviewer")
		svc, st := newForgeServiceForTest(t, author, reviewer)
		svc.enforceScopes = true
		st.scopes = map[string]bool{fmt.Sprintf("%s|%d|%s|%s", testOwnerID, store.ForgeProviderGitHub, testHost, testRepo): true}

		res := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("body", "k-1347"))
		if res.GetError() != nil || len(author.Calls()) != 1 || st.scopeCalls != 1 {
			t.Fatalf("owner grant result=%v provider calls=%d scope checks=%d", res.GetError(), len(author.Calls()), st.scopeCalls)
		}
	})
	t.Run("default off does not check scope", func(t *testing.T) {
		author := forge.NewFakeProvider("gh-author")
		reviewer := forge.NewFakeProvider("gh-reviewer")
		svc, st := newForgeServiceForTest(t, author, reviewer)

		res := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("body", "k-1357"))
		if res.GetError() != nil || len(author.Calls()) != 1 || st.scopeCalls != 0 {
			t.Fatalf("default-off result=%v provider calls=%d scope checks=%d", res.GetError(), len(author.Calls()), st.scopeCalls)
		}
	})
}

func TestForgeScopeGateStoreErrorAndMemoHit(t *testing.T) {
	t.Run("scope store error prevents provider write", func(t *testing.T) {
		author := forge.NewFakeProvider("gh-author")
		reviewer := forge.NewFakeProvider("gh-reviewer")
		svc, st := newForgeServiceForTest(t, author, reviewer)
		svc.enforceScopes = true
		st.scopeErr = errors.New("scope database unavailable")

		res := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("body", "k-1372"))
		if fe := res.GetError(); fe == nil || fe.GetCode() != "internal" {
			t.Fatalf("scope store error = %v, want internal", fe)
		}
		if len(author.Calls()) != 0 {
			t.Fatalf("provider calls = %d, want 0", len(author.Calls()))
		}
	})
	// The scope check runs before the intent is recorded, so an ungranted call
	// leaves no pending intent; a same-key retry after a revoked grant is refused.
	t.Run("same-key retry after a revoked grant is refused", func(t *testing.T) {
		author := forge.NewFakeProvider("gh-author")
		reviewer := forge.NewFakeProvider("gh-reviewer")
		svc, st := newForgeServiceForTest(t, author, reviewer)
		first := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("body", "memo-key"))
		if first.GetError() != nil {
			t.Fatalf("first create: %v", first.GetError())
		}
		svc.enforceScopes = true
		second := svc.ExecuteForgeCallAsAccountMust(t, createIssueCall("different", "memo-key"))
		if fe := second.GetError(); fe == nil || fe.GetCode() != "not_found" {
			t.Fatalf("revoked retry = %v, want not_found", second)
		}
		if len(author.Calls()) != 1 || len(st.intents) != 1 {
			t.Fatalf("provider calls=%d intents=%d, want 1 and 1", len(author.Calls()), len(st.intents))
		}
	})
}

func TestForgeScopeGateKeepsReadsAndCallerScopedUnsubscribeUngated(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	svc, st := newForgeServiceForTest(t, author, reviewer)
	svc.enforceScopes = true
	st.scopes = make(map[string]bool)

	read := &compassv1internal.ForgeCallRequest{Call: &compassv1internal.ForgeCallRequest_GetPullRequest{GetPullRequest: &compassv1internal.GetPullRequestRequest{Repo: testRepo, PrNumber: 1}}}
	if res := svc.ExecuteForgeCallAsAccountMust(t, read); res.GetError() != nil {
		t.Fatalf("read result = %v", res.GetError())
	}
	otherAgent := store.AccountID("acct-other-agent")
	st.seedAgent(otherAgent, "other", store.AccountID("acct-other-owner"), "other-owner")
	subID, err := st.EnsureAgentForgeSubscription(context.Background(), store.AgentForgeSubscription{
		AgentAccountID: otherAgent, Provider: store.ForgeProviderGitHub, Host: testHost, Repo: testRepo,
		Kind: store.ForgeArtifactKindIssue, Number: 1,
	})
	if err != nil {
		t.Fatalf("seed foreign subscription: %v", err)
	}
	res := svc.ExecuteForgeCallAsAccountMust(t, unsubscribeCall(subID))
	if fe := res.GetError(); fe == nil || fe.GetCode() != "not_found" {
		t.Fatalf("foreign unsubscribe error = %v, want caller-scoped not_found", fe)
	}
	if st.scopeCalls != 0 {
		t.Fatalf("read or unsubscribe made %d scope checks, want 0", st.scopeCalls)
	}
	if len(author.Calls()) != 1 {
		t.Fatalf("read provider calls=%d, want 1", len(author.Calls()))
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

// An empty host would become the provider's default and resolve every
// host-less ForgeRef to a coordinate the DL-055 row rejects, so register refuses it.
func TestForgeProviderRegistryRejectsEmptyHost(t *testing.T) {
	reg := newForgeProviderRegistry()
	gh := forge.NewFakeProvider("gh")
	err := reg.register(forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB}, gh, gh, true)
	if err == nil {
		t.Fatal("register with an empty host returned nil, want an error")
	}
	if _, ok := reg.resolve(nil); ok {
		t.Fatal("rejected registration still resolves as the default coordinate")
	}
	if err := reg.register(forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, host: testHost}, gh, gh, true); err != nil {
		t.Fatalf("register with a host: %v", err)
	}
	if got, ok := reg.resolve(nil); !ok || got.host != testHost {
		t.Fatalf("resolve(nil) = %+v, %v; want host %q", got, ok, testHost)
	}
	hostless := &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB}
	if got, ok := reg.resolve(hostless); !ok || got.host != testHost {
		t.Fatalf("resolve(github, no host) = %+v, %v; want host %q", got, ok, testHost)
	}
}

// --- tests: create_pull_request issue link -----------------------------------

func createPRWithIssue(clientReqID string, issue *compassv1internal.PullRequestIssueLink) *compassv1internal.ForgeCallRequest {
	call := createPRCall("body", clientReqID)
	call.GetCreatePullRequest().Issue = issue
	return call
}

// TestForgeCreatePullRequestStoresIssueLink pins the link coordinate the store
// receives: an empty repo means the PR's repo, and another forge keeps its own.
func TestForgeCreatePullRequestStoresIssueLink(t *testing.T) {
	linearRef := &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR}
	gheRef := &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "ghe.example"}
	cases := []struct {
		name    string
		prForge *compassv1.ForgeRef
		issue   *compassv1internal.PullRequestIssueLink
		want    *store.ForgeCoord
	}{
		{"no link", nil, nil, nil},
		{"same forge, empty repo", nil, &compassv1internal.PullRequestIssueLink{Number: 7},
			&store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: testHost, Repo: testRepo, Number: 7}},
		{"same forge, other repo", nil, &compassv1internal.PullRequestIssueLink{Repo: "o/other", Number: 8},
			&store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: testHost, Repo: "o/other", Number: 8}},
		{"blank repo and unset provider mean this PR", gheRef, &compassv1internal.PullRequestIssueLink{Forge: &compassv1.ForgeRef{}, Repo: " ", Number: 6},
			&store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: "ghe.example", Repo: testRepo, Number: 6}},
		{"linear issue", nil, &compassv1internal.PullRequestIssueLink{Forge: linearRef, Repo: "ENG", Number: 9},
			&store.ForgeCoord{Provider: store.ForgeProviderLinear, Host: forge.LinearHost, Repo: "ENG", Number: 9}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			author := forge.NewFakeProvider("gh-author")
			svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
			if err := registerLinearForgeCoordinate(svc.providers, forge.NewFakeProvider("linear")); err != nil {
				t.Fatalf("register linear: %v", err)
			}
			ghe := forge.NewFakeProvider("ghe")
			if err := svc.providers.register(forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, host: "ghe.example"}, ghe, ghe, false); err != nil {
				t.Fatalf("register ghe: %v", err)
			}
			created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			author.CreatePRResult = forge.PullRequest{Number: 55, State: "open", CreatedAt: created}
			ghe.CreatePRResult = author.CreatePRResult

			call := createPRWithIssue("k-link", tc.issue)
			call.Forge = tc.prForge
			res := svc.ExecuteForgeCallAsAccountMust(t, call)
			if res.GetError() != nil {
				t.Fatalf("create_pull_request errored: %v", res.GetError())
			}
			if len(st.prCreates) != 1 {
				t.Fatalf("PR creates = %d, want 1", len(st.prCreates))
			}
			got := st.prCreates[0]
			if (got.link == nil) != (tc.want == nil) || (got.link != nil && *got.link != *tc.want) {
				t.Fatalf("link = %+v, want %+v", got.link, tc.want)
			}
			if !got.row.CreatedAt.Equal(created) || !got.row.UpdatedAt.Equal(time.Unix(0, 0)) {
				t.Fatalf("row times created=%v updated=%v, want %v and the epoch", got.row.CreatedAt, got.row.UpdatedAt, created)
			}
		})
	}
}

// TestForgeCreatePullRequestRejectsBadIssueLink pins each shape error and that a
// rejected link reaches neither the provider nor the store.
func TestForgeCreatePullRequestRejectsBadIssueLink(t *testing.T) {
	cases := []struct {
		name  string
		issue *compassv1internal.PullRequestIssueLink
		code  string
	}{
		{"zero number", &compassv1internal.PullRequestIssueLink{Repo: "o/r"}, "invalid_argument"},
		{"unknown forge", &compassv1internal.PullRequestIssueLink{
			Forge: &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "nowhere.example"}, Repo: "o/r", Number: 1,
		}, "not_found"},
		{"empty repo on another forge", &compassv1internal.PullRequestIssueLink{
			Forge: &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR}, Number: 1,
		}, "invalid_argument"},
		{"blank repo on another host", &compassv1internal.PullRequestIssueLink{
			Forge: &compassv1.ForgeRef{Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, Host: "ghe.example"}, Repo: "  ", Number: 1,
		}, "invalid_argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			author := forge.NewFakeProvider("gh-author")
			svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
			if err := registerLinearForgeCoordinate(svc.providers, forge.NewFakeProvider("linear")); err != nil {
				t.Fatalf("register linear: %v", err)
			}
			ghe := forge.NewFakeProvider("ghe")
			if err := svc.providers.register(forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, host: "ghe.example"}, ghe, ghe, false); err != nil {
				t.Fatalf("register ghe: %v", err)
			}
			res := svc.ExecuteForgeCallAsAccountMust(t, createPRWithIssue("k-badlink", tc.issue))
			if fe := res.GetError(); fe == nil || fe.GetCode() != tc.code {
				t.Fatalf("error = %v, want code %q", fe, tc.code)
			}
			if len(author.Calls()) != 0 || len(st.recorded) != 0 {
				t.Fatalf("provider calls=%d recorded=%d, want 0 and 0", len(author.Calls()), len(st.recorded))
			}
		})
	}
}

// TestForgeCreatePullRequestMemoHitWritesNothing pins that a retried create with
// a link returns the memo and writes no second PR row or link.
func TestForgeCreatePullRequestMemoHitWritesNothing(t *testing.T) {
	author := forge.NewFakeProvider("gh-author")
	svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	author.CreatePRResult = forge.PullRequest{Number: 55}
	link := &compassv1internal.PullRequestIssueLink{Number: 7}

	first := svc.ExecuteForgeCallAsAccountMust(t, createPRWithIssue("req-1", link))
	again := svc.ExecuteForgeCallAsAccountMust(t, createPRWithIssue("req-1", link))
	if first.GetError() != nil || again.GetError() != nil {
		t.Fatalf("errors: first=%v again=%v", first.GetError(), again.GetError())
	}
	if again.GetPullRequest().GetNumber() != 55 {
		t.Fatalf("retry number = %d, want 55", again.GetPullRequest().GetNumber())
	}
	if len(author.Calls()) != 1 || len(st.prCreates) != 1 {
		t.Fatalf("provider calls=%d PR creates=%d, want 1 and 1", len(author.Calls()), len(st.prCreates))
	}
}

func TestForgeMirrorAllowedModes(t *testing.T) {
	ref := refs.Ref{Kind: refs.KindForge, Owner: "owner", Repo: "repo", Number: 12}
	tests := []struct {
		name string
		mode store.ForgeMirrorMode
		need store.ForgeMirrorMode
		code string
	}{
		{name: "off refuses manual", mode: store.ForgeMirrorOff, need: store.ForgeMirrorManual, code: "failed_precondition"},
		{name: "off refuses auto", mode: store.ForgeMirrorOff, need: store.ForgeMirrorAuto, code: "failed_precondition"},
		{name: "manual admits manual", mode: store.ForgeMirrorManual, need: store.ForgeMirrorManual},
		{name: "manual refuses auto", mode: store.ForgeMirrorManual, need: store.ForgeMirrorAuto, code: "failed_precondition"},
		{name: "auto admits manual", mode: store.ForgeMirrorAuto, need: store.ForgeMirrorManual},
		{name: "auto admits auto", mode: store.ForgeMirrorAuto, need: store.ForgeMirrorAuto},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			author := forge.NewFakeProvider("gh-author")
			svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
			st.mirrorModes[fmt.Sprintf("%d|%s|%s", store.ForgeProviderGitHub, testHost, testRepo)] = tc.mode
			_, _, _, fe := svc.mirrorAllowed(context.Background(), st.accounts[testOwnerID], tc.need, ref)
			if tc.code == "" {
				if fe != nil {
					t.Fatalf("mirrorAllowed error = %v, want allowed", fe)
				}
				return
			}
			if fe == nil || fe.GetCode() != tc.code || !strings.Contains(fe.GetMessage(), testRepo) {
				t.Fatalf("mirrorAllowed error = %v, want %s naming %s", fe, tc.code, testRepo)
			}
		})
	}

	t.Run("agent scope refusal", func(t *testing.T) {
		author := forge.NewFakeProvider("gh-author")
		svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
		svc.enforceScopes = true
		st.mirrorModes[fmt.Sprintf("%d|%s|%s", store.ForgeProviderGitHub, testHost, testRepo)] = store.ForgeMirrorAuto
		_, _, _, fe := svc.mirrorAllowed(context.Background(), st.accounts[testAgentID], store.ForgeMirrorAuto, ref)
		if fe == nil || fe.GetCode() != "not_found" || len(author.Calls()) != 0 || st.scopeCalls != 1 {
			t.Fatalf("mirrorAllowed = %v, provider calls=%d, scope calls=%d; want not_found, 0, 1", fe, len(author.Calls()), st.scopeCalls)
		}
	})
}

func TestResolveMirrorCoordinateLinear(t *testing.T) {
	linear := forge.NewFakeProvider("linear")
	svc, _ := newForgeServiceForTest(t, forge.NewFakeProvider("gh-author"), forge.NewFakeProvider("gh-reviewer"))
	reg := newForgeProviderRegistry()
	if err := reg.register(forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR, host: "linear.app"}, linear, linear, false); err != nil {
		t.Fatal(err)
	}
	svc.providers = reg
	rf, repo, number, fe := svc.resolveMirrorCoordinate(refs.Ref{Kind: refs.KindTracker, Key: "RIG", Number: 12})
	if fe != nil || rf.provider != compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR || rf.host != "linear.app" || repo != "RIG" || number != 12 {
		t.Fatalf("Linear coordinate = (%+v, %q, %d, %v)", rf, repo, number, fe)
	}
}

func TestForgeMirrorPostUsesManualBodyAndLimit(t *testing.T) {
	ctx := context.Background()
	ref := refs.Ref{Kind: refs.KindForge, Owner: "owner", Repo: "repo", Number: 12}
	author := forge.NewFakeProvider("gh-author")
	author.BodyLimitResult = 1024
	author.CommentResult = forge.Comment{ID: 1, Key: "c-1", URL: "https://github.com/owner/repo/issues/12#issuecomment-1"}
	svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	st.accounts[testOwnerID] = store.Account{ID: testOwnerID, Handle: testOwnerHandle, User: &store.UserAccount{}}
	st.mirrorModes[fmt.Sprintf("%d|%s|%s", store.ForgeProviderGitHub, testHost, testRepo)] = store.ForgeMirrorManual

	comment, err := svc.PostMirror(ctx, testOwnerID, "", store.ForgeMirrorManual, ref, "", "client-1", "source body")
	if err != nil {
		t.Fatalf("PostMirror: %v", err)
	}
	if comment.URL != author.CommentResult.URL {
		t.Fatalf("PostMirror comment URL = %q, want %q", comment.URL, author.CommentResult.URL)
	}
	calls := author.Calls()
	if len(calls) != 1 || calls[0].Method != "CommentOnIssue" || calls[0].ClientID != "client-1" {
		t.Fatalf("provider calls = %+v, want one idempotent issue comment", calls)
	}
	clean, _, ok := forge.StripOwner(calls[0].Body)
	if ok || !strings.Contains(clean, "source body") || strings.Contains(calls[0].Body, "Mirrored by @") {
		t.Fatalf("manual body = %q, want no owner claim and only source text", calls[0].Body)
	}
	if len(calls[0].Body) > author.BodyLimit() {
		t.Fatalf("manual body has %d bytes, exceeds provider limit %d", len(calls[0].Body), author.BodyLimit())
	}
	if got := svc.ProviderBodyLimit(ref); got != author.BodyLimit() {
		t.Fatalf("ProviderBodyLimit = %d, want provider limit %d", got, author.BodyLimit())
	}
}

func TestForgeMirrorUserAttribution(t *testing.T) {
	ctx := context.Background()
	ref := refs.Ref{Kind: refs.KindForge, Owner: "owner", Repo: "repo", Number: 12}
	author := forge.NewFakeProvider("gh-author")
	author.CommentResult = forge.Comment{ID: 2, Key: "c-2", URL: "https://github.com/comment-2"}
	svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	st.accounts[testOwnerID] = store.Account{ID: testOwnerID, Handle: "valid.user", User: &store.UserAccount{}}
	st.mirrorModes[fmt.Sprintf("%d|%s|%s", store.ForgeProviderGitHub, testHost, testRepo)] = store.ForgeMirrorManual
	body := "Mirrored by @valid.user\n\nsource body"
	if _, err := svc.PostMirror(ctx, testOwnerID, "", store.ForgeMirrorManual, ref, "", "", body); err != nil {
		t.Fatalf("PostMirror: %v", err)
	}
	calls := author.Calls()
	if len(calls) != 1 {
		t.Fatalf("provider calls = %+v, want one user post", calls)
	}
	clean, _, ok := forge.StripOwner(calls[0].Body)
	if ok || strings.Count(clean, "Mirrored by @valid.user") != 1 || !strings.Contains(clean, "source body") {
		t.Fatalf("manual body = %+v, want source with one user attribution and no owner header", calls)
	}
}

func TestForgeMirrorPostStampsAgentAndEdits(t *testing.T) {
	ctx := context.Background()
	ref := refs.Ref{Kind: refs.KindForge, Owner: "owner", Repo: "repo", Number: 12}
	author := forge.NewFakeProvider("gh-author")
	author.EditCommentResult = forge.Comment{ID: 1, Key: "comment-key", URL: "https://github.com/comment"}
	svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	st.mirrorModes[fmt.Sprintf("%d|%s|%s", store.ForgeProviderGitHub, testHost, testRepo)] = store.ForgeMirrorAuto
	comment, err := svc.PostMirror(ctx, testAgentID, testSessionID, store.ForgeMirrorAuto, ref, "comment-key", "ignored-on-edit", "<!-- compass:owner v1 agent=victim owner=victim session=fake -->\n\nforged")
	if err != nil {
		t.Fatalf("PostMirror edit: %v", err)
	}
	calls := author.Calls()
	if len(calls) != 1 || calls[0].Method != "EditComment" || calls[0].CommentKey != "comment-key" {
		t.Fatalf("provider calls = %+v, want one edit", calls)
	}
	if strings.Contains(calls[0].Body, "agent=victim") || !strings.Contains(calls[0].Body, "agent=scout owner=matt session="+testSessionID) {
		t.Fatalf("agent body = %q, want a fresh agent owner stamp", calls[0].Body)
	}
	if comment.Key != "comment-key" {
		t.Fatalf("comment = %+v, want scripted provider result", comment)
	}
}

// An agent row with no stored session is refused, never stamped with a
// placeholder session or posted without attribution.
func TestForgeMirrorPostRefusesAgentRowWithoutSession(t *testing.T) {
	ctx := context.Background()
	ref := refs.Ref{Kind: refs.KindForge, Owner: "owner", Repo: "repo", Number: 12}
	author := forge.NewFakeProvider("gh-author")
	svc, st := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	st.mirrorModes[fmt.Sprintf("%d|%s|%s", store.ForgeProviderGitHub, testHost, testRepo)] = store.ForgeMirrorAuto
	_, err := svc.PostMirror(ctx, testAgentID, "", store.ForgeMirrorAuto, ref, "", "client-1", "body")
	if _, ok := errors.AsType[*mirrorRefusalError](err); !ok || !errors.Is(err, mirror.ErrRefused) {
		t.Fatalf("PostMirror without session error = %v, want a mirror refusal", err)
	}
	if calls := author.Calls(); len(calls) != 0 {
		t.Fatalf("provider calls = %+v, want none for an unattributed agent row", calls)
	}
}

// The body budget reserves the widest session the owner header admits.
func TestForgeMirrorOwnerAllowanceCoversWidestSession(t *testing.T) {
	// Two 39-character handles, a 39-character session, and the fixed header
	// text; pinned so a change to either the grammar or the header is noticed.
	const want = 304
	if mirrorOwnerHeaderAllowance != want {
		t.Fatalf("mirrorOwnerHeaderAllowance = %d, want %d for a 39-character session", mirrorOwnerHeaderAllowance, want)
	}
}

func TestForgeMirrorBodyLimitReservesOwnerHeader(t *testing.T) {
	ref := refs.Ref{Kind: refs.KindForge, Owner: "owner", Repo: "repo", Number: 12}
	author := forge.NewFakeProvider("gh-author")
	author.BodyLimitResult = mirrorOwnerHeaderAllowance
	svc, _ := newForgeServiceForTest(t, author, forge.NewFakeProvider("gh-reviewer"))
	if got := svc.BodyLimit(ref); got <= 0 || got > author.BodyLimit() {
		t.Fatalf("BodyLimit = %d for provider cap %d, want positive finite allowance", got, author.BodyLimit())
	}
}

func TestForgeMirrorRecheckTimerIsStoppedOnShutdown(t *testing.T) {
	svc, _ := newForgeServiceForTest(t, forge.NewFakeProvider("gh-author"), forge.NewFakeProvider("gh-reviewer"))
	var gotDelay time.Duration
	stopped := false
	svc.scheduleMirrorAfter = func(delay time.Duration, _ func()) func() bool {
		gotDelay = delay
		return func() bool {
			stopped = true
			return true
		}
	}
	svc.SetMirrorLifetime(context.Background())
	recheckAt := time.Now().Add(time.Hour)
	svc.scheduleMirrorRecheck(context.Background(), store.ForgeMirrorRow{ID: "recheck-row", RecheckAt: &recheckAt}, nil, false)
	if gotDelay <= 0 || gotDelay > time.Hour {
		t.Fatalf("scheduled delay = %s, want a positive delay up to one hour", gotDelay)
	}
	svc.StopMirrorLifetime()
	if !stopped {
		t.Fatal("shutdown did not stop pending mirror recheck")
	}
}

func TestForgeMirrorRecheckKeepsOneEarlierPendingTimer(t *testing.T) {
	svc, _ := newForgeServiceForTest(t, forge.NewFakeProvider("gh-author"), forge.NewFakeProvider("gh-reviewer"))
	delays := make([]time.Duration, 0, 3)
	stops := make([]bool, 0, 3)
	callbacks := make([]func(), 0, 3)
	svc.scheduleMirrorAfter = func(delay time.Duration, fn func()) func() bool {
		delays = append(delays, delay)
		callbacks = append(callbacks, fn)
		stopped := false
		stops = append(stops, false)
		index := len(stops) - 1
		return func() bool {
			stops[index] = true
			stopped = true
			return stopped
		}
	}
	svc.SetMirrorLifetime(context.Background())
	t.Cleanup(svc.StopMirrorLifetime)
	now := time.Now()
	for _, due := range []time.Time{now.Add(time.Hour), now.Add(2 * time.Hour), now.Add(30 * time.Minute)} {
		svc.scheduleMirrorRecheck(context.Background(), store.ForgeMirrorRow{ID: "coalesced", RecheckAt: &due}, nil, false)
	}
	if len(delays) != 2 || delays[0] < 59*time.Minute || delays[1] < 29*time.Minute || delays[1] > 30*time.Minute {
		t.Fatalf("scheduled delays = %v, want original timer kept for later due time and replaced by earlier due time", delays)
	}
	if len(svc.mirrorTimers) != 1 || !stops[0] {
		t.Fatalf("pending timers = %d, first stopped = %v; want one pending timer and replaced timer stopped", len(svc.mirrorTimers), stops[0])
	}
	callbacks[0]()
	if len(svc.mirrorTimers) != 1 {
		t.Fatalf("pending timers after firing replaced callback = %d, want current timer retained", len(svc.mirrorTimers))
	}
}

type mirrorIdentityProvider struct {
	forge.Provider
	login string
	err   error
}

func (p *mirrorIdentityProvider) AppLogin(ctx context.Context) (string, error) {
	return p.login, p.err
}

func TestForgeMirrorCommentRecoveryRequiresAppIdentity(t *testing.T) {
	t.Run("only app comments are returned", func(t *testing.T) {
		author := forge.NewFakeProvider("gh-author")
		reviewer := forge.NewFakeProvider("gh-reviewer")
		identity := &mirrorIdentityProvider{Provider: author, login: "compass[bot]"}
		svc, st := newForgeServiceForTest(t, author, reviewer)
		svc.providers.entries[forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, host: testHost}] = forgeProviderRoles{author: identity, reviewer: reviewer}
		st.mirrorModes[fmt.Sprintf("%d|%s|%s", store.ForgeProviderGitHub, testHost, testRepo)] = store.ForgeMirrorManual
		author.ListCommentsResult = []forge.Comment{
			{Key: "app", ForgeAccount: "Compass[bot]"},
			{Key: "user", ForgeAccount: "alice"},
		}

		got, err := svc.ListMirrorComments(context.Background(), testOwnerID,
			refs.Ref{Kind: refs.KindForge, Owner: "owner", Repo: "repo", Number: 12}, time.Time{})
		if err != nil {
			t.Fatalf("ListMirrorComments: %v", err)
		}
		if len(got) != 1 || got[0].Key != "app" {
			t.Fatalf("recovery comments = %+v, want only Compass App comment", got)
		}
		calls := author.Calls()
		if len(calls) != 1 || calls[0].Method != "ListCommentsSince" {
			t.Fatalf("provider calls = %+v, want one comment listing", calls)
		}
	})

	t.Run("identity lookup failure prevents listing", func(t *testing.T) {
		author := forge.NewFakeProvider("gh-author")
		reviewer := forge.NewFakeProvider("gh-reviewer")
		identity := &mirrorIdentityProvider{Provider: author, err: errors.New("identity unavailable")}
		svc, st := newForgeServiceForTest(t, author, reviewer)
		svc.providers.entries[forgeCoordinate{provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB, host: testHost}] = forgeProviderRoles{author: identity, reviewer: reviewer}
		st.mirrorModes[fmt.Sprintf("%d|%s|%s", store.ForgeProviderGitHub, testHost, testRepo)] = store.ForgeMirrorManual

		if _, err := svc.ListMirrorComments(context.Background(), testOwnerID,
			refs.Ref{Kind: refs.KindForge, Owner: "owner", Repo: "repo", Number: 12}, time.Time{}); err == nil {
			t.Fatal("ListMirrorComments error = nil, want identity lookup failure")
		}
		if calls := author.Calls(); len(calls) != 0 {
			t.Fatalf("provider calls = %+v, want no comment listing without a verified App identity", calls)
		}
	})
}

// seedAgent registers an agent account and its owning user so resolveIdentity
// finds both handles.
func (f *fakeForgeStore) seedAgent(agentID store.AccountID, agentHandle string, ownerID store.AccountID, ownerHandle string) {
	f.accounts[agentID] = store.Account{ID: agentID, Handle: agentHandle, Agent: &store.AgentAccount{OwnerUserID: ownerID}}
	f.accounts[ownerID] = store.Account{ID: ownerID, Handle: ownerHandle, User: &store.UserAccount{}}
}
