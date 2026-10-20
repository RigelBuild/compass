//go:build unix

package server

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// intentCase is one create kind driven under a write intent.
type intentCase struct {
	name   string
	method string // the provider create method
	kind   forge.WriteKind
	call   func(key, body string) *compassv1internal.ForgeCallRequest
	// script sets the create's result and the recovery lookup's hit.
	script func(author, reviewer *forge.FakeProvider)
	// creator is the fake that receives the create.
	reviewer bool
	// rowKind is the ownership row an issue or PR create writes; 0 for comment and review.
	rowKind store.ForgeArtifactKind
}

func intentCases() []intentCase {
	return []intentCase{
		{
			name: "issue", method: "CreateIssue", kind: forge.WriteKindIssue, rowKind: store.ForgeArtifactKindIssue,
			call: func(key, body string) *compassv1internal.ForgeCallRequest { return createIssueCall(body, key) },
			script: func(a, _ *forge.FakeProvider) {
				a.CreateIssueResult = forge.Issue{Number: 41}
				a.FindWriteResult, a.FindWriteFound = forge.WrittenRef{Number: 41}, true
			},
		},
		{
			name: "pull_request", method: "CreatePullRequest", kind: forge.WriteKindPullRequest, rowKind: store.ForgeArtifactKindPullRequest,
			call: func(key, body string) *compassv1internal.ForgeCallRequest { return createPRCall(body, key) },
			script: func(a, _ *forge.FakeProvider) {
				a.CreatePRResult = forge.PullRequest{Number: 55, State: "open", BaseRef: "main", CreatedAt: nowStub(), URL: "https://forge/pr/55"}
				a.FindPRResult, a.FindPRFound = forge.PullRequest{Number: 55, State: "open", BaseRef: "main", CreatedAt: nowStub(), URL: "https://forge/pr/55"}, true
			},
		},
		{
			name: "comment_on_issue", method: "CommentOnIssue", kind: forge.WriteKindComment,
			call: func(key, body string) *compassv1internal.ForgeCallRequest {
				return &compassv1internal.ForgeCallRequest{ClientRequestId: key, Call: &compassv1internal.ForgeCallRequest_CommentOnIssue{
					CommentOnIssue: &compassv1internal.CommentOnIssueRequest{Repo: testRepo, IssueNumber: 7, Body: body},
				}}
			},
			script: func(a, _ *forge.FakeProvider) {
				a.CommentResult = forge.Comment{ID: 99, Key: "99"}
				a.FindWriteResult, a.FindWriteFound = forge.WrittenRef{Key: "99"}, true
			},
		},
		{
			name: "comment_on_pull_request", method: "CommentOnPullRequest", kind: forge.WriteKindComment,
			call: func(key, body string) *compassv1internal.ForgeCallRequest {
				return &compassv1internal.ForgeCallRequest{ClientRequestId: key, Call: &compassv1internal.ForgeCallRequest_CommentOnPullRequest{
					CommentOnPullRequest: &compassv1internal.CommentOnPullRequestRequest{Repo: testRepo, PrNumber: 7, Body: body},
				}}
			},
			script: func(a, _ *forge.FakeProvider) {
				a.CommentResult = forge.Comment{ID: 98, Key: "98"}
				a.FindWriteResult, a.FindWriteFound = forge.WrittenRef{Key: "98"}, true
			},
		},
		{
			name: "submit_review", method: "SubmitReview", kind: forge.WriteKindReview, reviewer: true,
			call: func(key, body string) *compassv1internal.ForgeCallRequest {
				return &compassv1internal.ForgeCallRequest{ClientRequestId: key, Call: &compassv1internal.ForgeCallRequest_SubmitReview{
					SubmitReview: &compassv1internal.SubmitReviewRequest{Repo: testRepo, PrNumber: 7, Verdict: "comment", Body: body},
				}}
			},
			script: func(_, r *forge.FakeProvider) {
				r.SubmitReviewResult = forge.SubmittedReview{ID: 77, Verdict: "comment"}
				r.FindWriteResult, r.FindWriteFound = forge.WrittenRef{Key: "77"}, true
			},
		},
	}
}

// countCalls counts the fake's calls to one method.
func countCalls(p *forge.FakeProvider, method string) int {
	n := 0
	for _, c := range p.Calls() {
		if c.Method == method {
			n++
		}
	}
	return n
}

// createdBody returns the body the forge received on the create.
func createdBody(t *testing.T, p *forge.FakeProvider, method string) string {
	t.Helper()
	for _, c := range p.Calls() {
		if c.Method != method {
			continue
		}
		switch in := c.Payload.(type) {
		case forge.CreateIssue:
			return in.Body
		case forge.CreatePR:
			return in.Body
		case forge.SubmitReview:
			return in.Body
		default:
			return c.Body
		}
	}
	t.Fatalf("no %s call", method)
	return ""
}

// assertOneObject checks the end state every recovery path must reach: one
// forge create, one done intent, and one ownership record.
func assertOneObject(t *testing.T, tc intentCase, creator *forge.FakeProvider, st *fakeForgeStore) {
	t.Helper()
	if n := countCalls(creator, tc.method); n != 1 {
		t.Fatalf("%s calls = %d, want 1", tc.method, n)
	}
	if len(st.intents) != 1 || st.intents[0].State != store.WriteStateDone {
		t.Fatalf("intents = %+v, want one done", st.intents)
	}
	if tc.rowKind != 0 {
		if len(st.recorded) != 1 || st.recorded[0].Kind != tc.rowKind || st.recorded[0].ClientRequestID != "" {
			t.Fatalf("ownership rows = %+v, want one %v row with no request key", st.recorded, tc.rowKind)
		}
		return
	}
	if st.intents[0].ForgeKey == "" {
		t.Fatal("comment or review intent kept no forge key")
	}
}

func newIntentService(t *testing.T, tc intentCase) (*forgeService, *fakeForgeStore, *forge.FakeProvider, *forge.FakeProvider) {
	t.Helper()
	author := forge.NewFakeProvider("gh-author")
	reviewer := forge.NewFakeProvider("gh-reviewer")
	tc.script(author, reviewer)
	svc, st := newForgeServiceForTest(t, author, reviewer)
	creator := author
	if tc.reviewer {
		creator = reviewer
	}
	return svc, st, author, creator
}

// TestForgeWriteIntentLostResponseRecovers: the create lands but its response
// is lost; a same-key retry, or the sweep alone, records it with no second create.
func TestForgeWriteIntentLostResponseRecovers(t *testing.T) {
	for _, tc := range intentCases() {
		for _, path := range []string{"retry", "sweep"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				svc, st, _, creator := newIntentService(t, tc)
				creator.SetError(tc.method, &forge.StatusError{Status: 502, Message: "bad gateway"})
				if fe := svc.ExecuteForgeCallAsAccountMust(t, tc.call("k1", "b")).GetError(); fe == nil {
					t.Fatal("lost-response create returned success")
				}
				if len(st.intents) != 1 || st.intents[0].State != store.WriteStatePending {
					t.Fatalf("intent after a 5xx = %+v, want pending", st.intents)
				}
				creator.SetError(tc.method, nil)
				if path == "retry" {
					res := svc.ExecuteForgeCallAsAccountMust(t, tc.call("k1", "b"))
					if res.GetError() != nil {
						t.Fatalf("retry = %v, want the recovered object", res.GetError())
					}
				} else {
					svc.now = func() time.Time { return nowStub().Add(2 * time.Minute) }
					if err := svc.sweepWriteIntents(t.Context()); err != nil {
						t.Fatalf("sweep: %v", err)
					}
				}
				assertOneObject(t, tc, creator, st)
			})
		}
	}
}

// TestForgeWriteIntentFailedRecordRecovers: the create succeeds but the store
// write after it fails; the intent stays pending and recovery records it.
func TestForgeWriteIntentFailedRecordRecovers(t *testing.T) {
	for _, tc := range intentCases() {
		for _, path := range []string{"retry", "sweep"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				svc, st, _, creator := newIntentService(t, tc)
				st.recErr = errors.New("store: connection reset")
				if fe := svc.ExecuteForgeCallAsAccountMust(t, tc.call("k1", "b")).GetError(); fe == nil || fe.GetCode() != "internal" {
					t.Fatalf("failed record = %v, want internal", fe)
				}
				if st.intents[0].State != store.WriteStatePending {
					t.Fatalf("intent after a failed record = %s, want pending", st.intents[0].State)
				}
				st.recErr = nil
				if path == "retry" {
					if res := svc.ExecuteForgeCallAsAccountMust(t, tc.call("k1", "b")); res.GetError() != nil {
						t.Fatalf("retry = %v", res.GetError())
					}
				} else {
					svc.now = func() time.Time { return nowStub().Add(2 * time.Minute) }
					if err := svc.sweepWriteIntents(t.Context()); err != nil {
						t.Fatalf("sweep: %v", err)
					}
				}
				assertOneObject(t, tc, creator, st)
			})
		}
	}
}

// TestForgeWriteIntentStripsAgentMarker: an agent-written marker never reaches
// the forge; a GitHub body carries only this intent's marker.
func TestForgeWriteIntentStripsAgentMarker(t *testing.T) {
	const forged = "<!-- compass:write 11111111-1111-1111-1111-111111111111 -->"
	for _, tc := range intentCases() {
		t.Run(tc.name, func(t *testing.T) {
			svc, st, _, creator := newIntentService(t, tc)
			if res := svc.ExecuteForgeCallAsAccountMust(t, tc.call("k1", "hi "+forged)); res.GetError() != nil {
				t.Fatalf("create: %v", res.GetError())
			}
			body := createdBody(t, creator, tc.method)
			if strings.Contains(body, forged) {
				t.Fatalf("agent marker reached the forge:\n%s", body)
			}
			want := 1
			if tc.kind == forge.WriteKindPullRequest {
				want = 0 // a PR is recovered by its head
			}
			if n := strings.Count(body, "<!-- compass:write "); n != want {
				t.Fatalf("body carries %d write markers, want %d:\n%s", n, want, body)
			}
			if want == 1 && !strings.Contains(body, forge.WithWriteMarker("", st.intents[0].ID)[2:]) {
				t.Fatalf("body lacks this intent's marker:\n%s", body)
			}
		})
	}
}

// TestForgeWriteIntentRefusalFailsAndRetryCreates: a forge refusal ends the
// attempt as failed, and a same-key retry makes a new attempt that creates.
func TestForgeWriteIntentRefusalFailsAndRetryCreates(t *testing.T) {
	for _, tc := range intentCases() {
		t.Run(tc.name, func(t *testing.T) {
			svc, st, _, creator := newIntentService(t, tc)
			creator.SetError(tc.method, &forge.StatusError{Status: 422, Message: "invalid"})
			if fe := svc.ExecuteForgeCallAsAccountMust(t, tc.call("k1", "b")).GetError(); fe == nil || fe.GetCode() != "invalid_argument" {
				t.Fatalf("refused create = %v, want invalid_argument", fe)
			}
			if st.intents[0].State != store.WriteStateFailed {
				t.Fatalf("refused intent = %s, want failed", st.intents[0].State)
			}
			creator.SetError(tc.method, nil)
			if res := svc.ExecuteForgeCallAsAccountMust(t, tc.call("k1", "b")); res.GetError() != nil {
				t.Fatalf("retry after refusal = %v", res.GetError())
			}
			if n := countCalls(creator, tc.method); n != 2 {
				t.Fatalf("%s calls = %d, want 2", tc.method, n)
			}
			if len(st.intents) != 2 || st.intents[1].State != store.WriteStateDone {
				t.Fatalf("intents = %+v, want failed then done", st.intents)
			}
		})
	}
}

// TestForgeWriteIntentRateLimitStaysPending: a 429 may hide a landed create,
// so the attempt stays pending and a retry never resends.
func TestForgeWriteIntentRateLimitStaysPending(t *testing.T) {
	tc := intentCases()[0]
	svc, st, author, _ := newIntentService(t, tc)
	author.SetError(tc.method, &forge.StatusError{Status: 429, Message: "slow down"})
	svc.ExecuteForgeCallAsAccountMust(t, tc.call("k1", "b"))
	if st.intents[0].State != store.WriteStatePending {
		t.Fatalf("intent after 429 = %s, want pending", st.intents[0].State)
	}
	author.SetError(tc.method, nil)
	author.FindWriteFound = false
	fe := svc.ExecuteForgeCallAsAccountMust(t, tc.call("k1", "b")).GetError()
	if fe == nil || fe.GetCode() != "unavailable" || fe.GetMessage() != writePendingMessage {
		t.Fatalf("retry of an unfound pending intent = %v, want unavailable pending", fe)
	}
	if n := countCalls(author, tc.method); n != 1 {
		t.Fatalf("%s calls = %d, want 1", tc.method, n)
	}
}

// TestForgeWriteIntentGitHubNothingFoundTurnsLost: after a day with nothing
// listed the sweep marks the attempt lost; a retry returns that, never a create.
func TestForgeWriteIntentGitHubNothingFoundTurnsLost(t *testing.T) {
	for _, tc := range intentCases() {
		t.Run(tc.name, func(t *testing.T) {
			svc, st, author, creator := newIntentService(t, tc)
			creator.SetError(tc.method, &forge.StatusError{Status: 504, Message: "timeout"})
			svc.ExecuteForgeCallAsAccountMust(t, tc.call("k1", "b"))
			creator.SetError(tc.method, nil)
			author.FindWriteFound, author.FindPRFound = false, false
			creator.FindWriteFound = false

			svc.now = func() time.Time { return nowStub().Add(time.Hour) }
			if err := svc.sweepWriteIntents(t.Context()); err != nil {
				t.Fatalf("sweep at an hour: %v", err)
			}
			if st.intents[0].State != store.WriteStatePending {
				t.Fatalf("intent at an hour = %s, want pending", st.intents[0].State)
			}
			svc.now = func() time.Time { return nowStub().Add(25 * time.Hour) }
			if err := svc.sweepWriteIntents(t.Context()); err != nil {
				t.Fatalf("sweep after a day: %v", err)
			}
			if st.intents[0].State != store.WriteStateLost {
				t.Fatalf("intent after a day = %s, want lost", st.intents[0].State)
			}
			fe := svc.ExecuteForgeCallAsAccountMust(t, tc.call("k1", "b")).GetError()
			if fe == nil || fe.GetMessage() != writeLostMessage {
				t.Fatalf("retry of a lost intent = %v, want the lost result", fe)
			}
			if n := countCalls(creator, tc.method); n != 1 {
				t.Fatalf("%s calls = %d, want 1", tc.method, n)
			}
		})
	}
}

// TestForgeWriteIntentInFlightSweepNeverResends: a sweep that runs while the
// create is still in flight sends nothing; once the object lists the intent is
// done, and a late refusal from the first create cannot turn it failed.
func TestForgeWriteIntentInFlightSweepNeverResends(t *testing.T) {
	for _, tc := range intentCases() {
		t.Run(tc.name, func(t *testing.T) {
			svc, st, author, creator := newIntentService(t, tc)
			rf, _ := svc.providers.resolve(nil)
			id, fe := svc.resolveIdentity(t.Context(), testAgentID, testSessionID)
			if fe != nil {
				t.Fatalf("resolveIdentity: %v", fe)
			}
			target := writeTarget{rf: rf, id: id, caller: testAgentID, session: testSessionID, repo: testRepo, kind: tc.kind}
			if tc.kind == forge.WriteKindPullRequest {
				target.headRef, target.baseRef = "feature", "main"
			} else if tc.kind != forge.WriteKindIssue {
				target.parent = 7
			}
			// The create is in flight: its intent is recorded and nothing lists yet.
			intent, prior, fe := svc.beginWrite(t.Context(), "k1", target)
			if fe != nil || prior != nil {
				t.Fatalf("beginWrite = %v, %v", prior, fe)
			}
			found, foundPR, foundCreator := author.FindWriteFound, author.FindPRFound, creator.FindWriteFound
			author.FindWriteFound, author.FindPRFound, creator.FindWriteFound = false, false, false

			svc.now = func() time.Time { return nowStub().Add(2 * time.Minute) }
			if err := svc.sweepWriteIntents(t.Context()); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if fe := svc.ExecuteForgeCallAsAccountMust(t, tc.call("k1", "b")).GetError(); fe == nil || fe.GetCode() != "unavailable" {
				t.Fatalf("retry while in flight = %v, want unavailable", fe)
			}
			if n := countCalls(creator, tc.method); n != 0 {
				t.Fatalf("%s calls while in flight = %d, want 0", tc.method, n)
			}

			author.FindWriteFound, author.FindPRFound, creator.FindWriteFound = found, foundPR, foundCreator
			if err := svc.sweepWriteIntents(t.Context()); err != nil {
				t.Fatalf("sweep after listing: %v", err)
			}
			// The first create's late refusal arrives after recovery.
			svc.failWrite(t.Context(), intent.ID, &forge.StatusError{Status: 422, Message: "late"}, forgeOp{op: tc.method})
			if st.intents[0].State != store.WriteStateDone {
				t.Fatalf("intent after a late refusal = %s, want done", st.intents[0].State)
			}
			if tc.rowKind != 0 && len(st.recorded) != 1 {
				t.Fatalf("ownership rows = %d, want 1", len(st.recorded))
			}
			if n := countCalls(creator, tc.method); n != 0 {
				t.Fatalf("%s calls = %d, want 0", tc.method, n)
			}
		})
	}
}

// TestForgeWriteIntentKeylessCreateIsInvalidArgument: every create needs a key.
func TestForgeWriteIntentKeylessCreateIsInvalidArgument(t *testing.T) {
	for _, tc := range intentCases() {
		t.Run(tc.name, func(t *testing.T) {
			svc, st, _, creator := newIntentService(t, tc)
			fe := svc.ExecuteForgeCallAsAccountMust(t, tc.call("", "b")).GetError()
			if fe == nil || fe.GetCode() != "invalid_argument" {
				t.Fatalf("keyless create = %v, want invalid_argument", fe)
			}
			if len(creator.Calls()) != 0 || len(st.intents) != 0 {
				t.Fatalf("keyless create touched provider=%d intents=%d", len(creator.Calls()), len(st.intents))
			}
		})
	}
}

// TestForgeWriteIntentOlderSameHeadPRStaysPending rejects a PR created before the intent.
func TestForgeWriteIntentOlderSameHeadPRStaysPending(t *testing.T) {
	tc := intentCases()[1]
	svc, st, _, creator := newIntentService(t, tc)
	creator.SetError(tc.method, &forge.StatusError{Status: 502, Message: "bad gateway"})
	if fe := svc.ExecuteForgeCallAsAccountMust(t, tc.call("k1", "b")).GetError(); fe == nil {
		t.Fatal("lost-response create returned success")
	}
	creator.SetError(tc.method, nil)
	creator.FindPRResult = forge.PullRequest{
		Number: 55, State: "open", BaseRef: "main",
		CreatedAt: st.intents[0].CreatedAt.Add(-writePRClockSkew - time.Second), URL: "https://forge/pr/55",
	}
	res := svc.ExecuteForgeCallAsAccountMust(t, tc.call("k1", "b"))
	if fe := res.GetError(); fe == nil || fe.GetCode() != "unavailable" {
		t.Fatalf("retry with an older same-head PR = %v, want unavailable", fe)
	}
	if st.intents[0].State != store.WriteStatePending || len(st.recorded) != 0 || countCalls(creator, tc.method) != 1 {
		t.Fatalf("intent=%s ownership=%d creates=%d; want pending, none, one", st.intents[0].State, len(st.recorded), countCalls(creator, tc.method))
	}
}

// An omitted base means the default branch, so recovery checks only the creation time;
// an explicit base must match the found PR's base.
func TestForgeWriteIntentPRRecoveryBase(t *testing.T) {
	for _, tc := range []struct {
		name, reqBase, prBase string
		recovered             bool
	}{
		{"omitted base recovers", "", "trunk", true},
		{"explicit base mismatch stays pending", "main", "release", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ic := intentCases()[1]
			svc, st, _, creator := newIntentService(t, ic)
			call := createPRCall("b", "k1")
			call.GetCreatePullRequest().BaseRef = tc.reqBase
			creator.SetError(ic.method, &forge.StatusError{Status: 502, Message: "bad gateway"})
			if fe := svc.ExecuteForgeCallAsAccountMust(t, call).GetError(); fe == nil {
				t.Fatal("lost-response create returned success")
			}
			creator.SetError(ic.method, nil)
			creator.FindPRResult = forge.PullRequest{Number: 55, State: "open", BaseRef: tc.prBase, CreatedAt: st.intents[0].CreatedAt, URL: "https://forge/pr/55"}
			creator.FindPRFound = true
			fe := svc.ExecuteForgeCallAsAccountMust(t, call).GetError()
			if got := fe == nil && st.intents[0].State == store.WriteStateDone; got != tc.recovered {
				t.Fatalf("recovered = %t (err %v, state %s), want %t", got, fe, st.intents[0].State, tc.recovered)
			}
			if countCalls(creator, ic.method) != 1 {
				t.Fatalf("creates = %d, want 1", countCalls(creator, ic.method))
			}
		})
	}
}

// TestForgeWriteIntentRecoveryRestoresPullRequestRows checks recovered PR completion as one store operation.
func TestForgeWriteIntentRecoveryRestoresPullRequestRows(t *testing.T) {
	tc := intentCases()[1]
	svc, st, author, creator := newIntentService(t, tc)
	call := createPRWithIssue("k1", &compassv1internal.PullRequestIssueLink{Number: 7})
	creator.SetError(tc.method, &forge.StatusError{Status: 502, Message: "bad gateway"})
	if fe := svc.ExecuteForgeCallAsAccountMust(t, call).GetError(); fe == nil {
		t.Fatal("lost-response create returned success")
	}
	creator.SetError(tc.method, nil)
	creator.FindPRResult = forge.PullRequest{
		Number: 55, State: "open", BaseRef: "main", CreatedAt: st.intents[0].CreatedAt, URL: "https://forge/pr/55",
	}
	creator.FindPRFound = true
	res := svc.ExecuteForgeCallAsAccountMust(t, call)
	if res.GetError() != nil {
		t.Fatalf("recovery retry = %v", res.GetError())
	}
	if len(st.recorded) != 1 || st.recorded[0].Kind != store.ForgeArtifactKindPullRequest || st.recorded[0].Number != 55 {
		t.Fatalf("ownership rows = %+v, want recovered PR ownership", st.recorded)
	}
	wantLink := store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: testHost, Repo: testRepo, Number: 7}
	if len(st.prRows) != 1 || st.prRows[0].Coord.Number != 55 || len(st.prLinks) != 1 || st.prLinks[0] != wantLink {
		t.Fatalf("recovered PR rows=%+v links=%+v, want link %+v", st.prRows, st.prLinks, wantLink)
	}
	if len(st.intents) != 1 || st.intents[0].State != store.WriteStateDone || st.intents[0].ForgeURL != "https://forge/pr/55" {
		t.Fatalf("completed recovered intent = %+v", st.intents)
	}
	if countCalls(creator, tc.method) != 1 || countCalls(author, "FindPullRequestByHead") != 1 {
		t.Fatalf("create calls=%d PR lookups=%d, want one each", countCalls(creator, tc.method), countCalls(author, "FindPullRequestByHead"))
	}
}

// TestForgeWriteIntentReplayReturnsStoredURLs checks URLs for done comments and reviews.
func TestForgeWriteIntentReplayReturnsStoredURLs(t *testing.T) {
	for _, tc := range intentCases() {
		if tc.kind != forge.WriteKindComment && tc.kind != forge.WriteKindReview {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			svc, st, _, creator := newIntentService(t, tc)
			const url = "https://forge/object/77"
			if tc.kind == forge.WriteKindReview {
				creator.SubmitReviewResult.URL = url
			} else {
				creator.CommentResult.URL = url
			}
			first := svc.ExecuteForgeCallAsAccountMust(t, tc.call("k1", "b"))
			if first.GetError() != nil {
				t.Fatalf("first create = %v", first.GetError())
			}
			second := svc.ExecuteForgeCallAsAccountMust(t, tc.call("k1", "b"))
			if second.GetError() != nil {
				t.Fatalf("replay = %v", second.GetError())
			}
			var firstURL, secondURL string
			switch {
			case tc.kind == forge.WriteKindReview:
				firstURL, secondURL = first.GetReview().GetUrl(), second.GetReview().GetUrl()
			case tc.name == "comment_on_issue":
				firstURL, secondURL = first.GetIssueComment().GetUrl(), second.GetIssueComment().GetUrl()
			default:
				firstURL, secondURL = first.GetPrComment().GetUrl(), second.GetPrComment().GetUrl()
			}
			if firstURL != url || secondURL != url || st.intents[0].ForgeURL != url {
				t.Fatalf("first URL=%q replay URL=%q stored URL=%q, want %q", firstURL, secondURL, st.intents[0].ForgeURL, url)
			}
			if countCalls(creator, tc.method) != 1 {
				t.Fatalf("%s calls = %d, want 1", tc.method, countCalls(creator, tc.method))
			}
		})
	}
}

// TestForgeWriteIntent408StaysPendingNoResend treats an HTTP timeout as an unknown outcome.
func TestForgeWriteIntent408StaysPendingNoResend(t *testing.T) {
	tc := intentCases()[0]
	svc, st, author, creator := newIntentService(t, tc)
	creator.SetError(tc.method, &forge.StatusError{Status: 408, Message: "request timeout"})
	author.FindWriteFound = false
	if fe := svc.ExecuteForgeCallAsAccountMust(t, tc.call("k1", "b")).GetError(); fe == nil {
		t.Fatal("408 create returned success")
	}
	if st.intents[0].State != store.WriteStatePending {
		t.Fatalf("intent after 408 = %s, want pending", st.intents[0].State)
	}
	creator.SetError(tc.method, nil)
	retry := svc.ExecuteForgeCallAsAccountMust(t, tc.call("k1", "b"))
	if fe := retry.GetError(); fe == nil || fe.GetCode() != "unavailable" {
		t.Fatalf("same-key retry after 408 = %v, want unavailable", fe)
	}
	if countCalls(creator, tc.method) != 1 {
		t.Fatalf("%s calls = %d, want one create", tc.method, countCalls(creator, tc.method))
	}
}
