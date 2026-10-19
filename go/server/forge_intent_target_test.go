//go:build unix

package server

import (
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

const otherRepo = "owner/other"

// A reused client_request_id on another target is refused with no provider call,
// for every create arm; the same target still replays.
func TestForgeCreateKeyReusedOnAnotherTargetIsRefused(t *testing.T) {
	issue := func(repo string) *compassv1internal.ForgeCallRequest {
		return &compassv1internal.ForgeCallRequest{ClientRequestId: "k", Call: &compassv1internal.ForgeCallRequest_CreateIssue{CreateIssue: &compassv1internal.CreateIssueRequest{Repo: repo, Title: "t", Body: "b"}}}
	}
	pr := func(repo, head string) *compassv1internal.ForgeCallRequest {
		return &compassv1internal.ForgeCallRequest{ClientRequestId: "k", Call: &compassv1internal.ForgeCallRequest_CreatePullRequest{CreatePullRequest: &compassv1internal.CreatePullRequestRequest{Repo: repo, Title: "t", Body: "b", HeadRef: head, BaseRef: "main"}}}
	}
	issueComment := func(repo string, n uint64) *compassv1internal.ForgeCallRequest {
		return &compassv1internal.ForgeCallRequest{ClientRequestId: "k", Call: &compassv1internal.ForgeCallRequest_CommentOnIssue{CommentOnIssue: &compassv1internal.CommentOnIssueRequest{Repo: repo, IssueNumber: n, Body: "b"}}}
	}
	prComment := func(repo string, n uint64) *compassv1internal.ForgeCallRequest {
		return &compassv1internal.ForgeCallRequest{ClientRequestId: "k", Call: &compassv1internal.ForgeCallRequest_CommentOnPullRequest{CommentOnPullRequest: &compassv1internal.CommentOnPullRequestRequest{Repo: repo, PrNumber: n, Body: "b"}}}
	}
	review := func(repo string, n uint64) *compassv1internal.ForgeCallRequest {
		return &compassv1internal.ForgeCallRequest{ClientRequestId: "k", Call: &compassv1internal.ForgeCallRequest_SubmitReview{SubmitReview: &compassv1internal.SubmitReviewRequest{Repo: repo, PrNumber: n, Verdict: "comment", Body: "b"}}}
	}
	for _, tc := range []struct {
		name          string
		first, second *compassv1internal.ForgeCallRequest
		same          *compassv1internal.ForgeCallRequest
	}{
		{"issue repo", issue(testRepo), issue(otherRepo), issue("Owner/Repo")},
		{"pr repo", pr(testRepo, "feature"), pr(otherRepo, "feature"), pr(testRepo, "feature")},
		{"pr head", pr(testRepo, "feature"), pr(testRepo, "other"), pr(testRepo, "feature")},
		{"issue comment parent", issueComment(testRepo, 1), issueComment(testRepo, 2), issueComment(testRepo, 1)},
		{"issue comment repo", issueComment(testRepo, 1), issueComment(otherRepo, 1), issueComment(testRepo, 1)},
		{"pr comment parent", prComment(testRepo, 1), prComment(testRepo, 2), prComment(testRepo, 1)},
		{"review parent", review(testRepo, 1), review(testRepo, 2), review(testRepo, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			author := forge.NewFakeProvider("gh-author")
			reviewer := forge.NewFakeProvider("gh-reviewer")
			author.CreateIssueResult = forge.Issue{Number: 42, URL: "u"}
			author.CreatePRResult = forge.PullRequest{Number: 43, URL: "u", HeadRef: "feature"}
			author.CommentResult = forge.Comment{ID: 44, URL: "u"}
			reviewer.SubmitReviewResult = forge.SubmittedReview{ID: 45, URL: "u"}
			svc, _ := newForgeServiceForTest(t, author, reviewer)
			if res := svc.ExecuteForgeCallAsAccountMust(t, tc.first); res.GetError() != nil {
				t.Fatalf("first call = %v", res)
			}
			calls := len(author.Calls()) + len(reviewer.Calls())

			res := svc.ExecuteForgeCallAsAccountMust(t, tc.second)
			if fe := res.GetError(); fe == nil || fe.GetCode() != "failed_precondition" || !strings.Contains(fe.GetMessage(), "another target") {
				t.Fatalf("reused key on another target = %v, want failed_precondition", res)
			}
			if got := len(author.Calls()) + len(reviewer.Calls()); got != calls {
				t.Fatalf("provider calls %d -> %d, want none on the refused call", calls, got)
			}
			if res := svc.ExecuteForgeCallAsAccountMust(t, tc.same); res.GetError() != nil {
				t.Fatalf("same-target retry = %v, want the replay", res)
			}
			if got := len(author.Calls()) + len(reviewer.Calls()); got != calls {
				t.Fatalf("provider calls %d -> %d, want none on the replay", calls, got)
			}
		})
	}
}

// The same key and head submitted to two repos whose PR numbers collide: the
// second repo's replay is refused before any push or PATCH.
func TestSubmitStackKeyReusedInAnotherRepoWritesNothing(t *testing.T) {
	svc, st, p, m := newStackServiceForTest(t)
	first := stackCall("k", "", []byte("bundle"), stackEntry("a", "owner/repo#7"))
	stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, first))
	number := p.prs["a"].Number
	pushes, updates := len(m.pushes), p.count("update")

	// The other repo has its own PR with the same number on the same head, recorded to nobody.
	delete(p.prs, "a")
	p.prs["a"] = &forge.PullRequest{Number: number, Title: "theirs", State: "open", HeadRef: "a", BaseRef: "main"}
	second := stackCall("k", "", []byte("bundle"), stackEntry("a", "owner/other#7"))
	second.GetSubmitStack().Repo = otherRepo
	got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, second))
	if got[0].GetStatus() != failedStatus || got[0].GetError().GetCode() != "failed_precondition" || !strings.Contains(got[0].GetError().GetMessage(), "another target") {
		t.Fatalf("second repo = %v, want failed_precondition naming another target", got)
	}
	if len(m.pushes) != pushes || p.count("update") != updates {
		t.Fatalf("pushes %d->%d updates %d->%d, want no write on the second repo", pushes, len(m.pushes), updates, p.count("update"))
	}
	if len(st.recorded) != 1 {
		t.Fatalf("rows = %d, want only the first repo's", len(st.recorded))
	}
}

// The caller already authored repo B's PR on the head, so requireOwn would pass
// without beginWrite; the derived key's repo A target still refuses it first.
func TestSubmitStackKeyReusedOnOwnPRInAnotherRepoIsRefusedFirst(t *testing.T) {
	svc, st, p, m := newStackServiceForTest(t)
	stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, stackCall("k", "", []byte("bundle"), stackEntry("a", "owner/repo#7"))))

	p.prs["a"] = &forge.PullRequest{Number: 900, Title: "mine in B", State: "open", HeadRef: "a", BaseRef: "main"}
	st.recordArtifact(store.AuthoredArtifact{Provider: store.ForgeProviderGitHub, Host: testHost, Repo: otherRepo, Kind: store.ForgeArtifactKindPullRequest, Number: 900, AgentAccountID: testAgentID, OwnerUserID: testOwnerID})
	applied, pushes, events := len(m.applied), len(m.pushes), len(*p.events)

	second := stackCall("k", "", []byte("bundle"), stackEntry("a", ""))
	second.GetSubmitStack().Repo = otherRepo
	got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, second))
	if got[0].GetStatus() != failedStatus || got[0].GetError().GetCode() != "failed_precondition" || !strings.Contains(got[0].GetError().GetMessage(), "another target") {
		t.Fatalf("repo B = %v, want failed_precondition naming another target", got)
	}
	if len(m.applied) != applied || len(m.pushes) != pushes || len(*p.events) != events {
		t.Fatalf("applied %d->%d pushes %d->%d events %v, want no mirror or provider work", applied, len(m.applied), pushes, len(m.pushes), (*p.events)[events:])
	}
}

// Another call claims the key for repo A in the window before this call's claim
// on repo B lands: the claim sees their row, so this call writes nothing.
func TestSubmitStackClaimRaceWithAnotherTargetWritesNothing(t *testing.T) {
	svc, st, p, m := newStackServiceForTest(t)
	p.prs["a"] = &forge.PullRequest{Number: 900, Title: "mine in B", State: "open", HeadRef: "a", BaseRef: "main"}
	st.recordArtifact(store.AuthoredArtifact{Provider: store.ForgeProviderGitHub, Host: testHost, Repo: otherRepo, Kind: store.ForgeArtifactKindPullRequest, Number: 900, AgentAccountID: testAgentID, OwnerUserID: testOwnerID})
	st.beforeHold = func() {
		st.beforeHold = nil
		// The racing call is a create on repo A under the same derived key.
		if _, err := st.RecordWriteIntent(t.Context(), store.WriteIntent{ID: "11111111-1111-4111-8111-111111111111", Provider: store.ForgeProviderGitHub, Host: testHost, Repo: testRepo, Kind: forge.WriteKindPullRequest, HeadRef: "a", AgentAccountID: testAgentID, OwnerUserID: testOwnerID, RequestKey: "k:a"}); err != nil {
			t.Fatalf("racing record: %v", err)
		}
	}

	call := stackCall("k", "", []byte("bundle"), stackEntry("a", ""))
	call.GetSubmitStack().Repo = otherRepo
	got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, call))
	if got[0].GetStatus() != failedStatus || !strings.Contains(got[0].GetError().GetMessage(), "another target") {
		t.Fatalf("raced claim = %v, want refused naming another target", got)
	}
	if len(m.applied)+len(m.pushes) != 0 || p.count("find")+p.count("update")+p.count("create") != 0 {
		t.Fatalf("applied=%v pushes=%v events=%v, want no mirror or provider work", m.applied, m.pushes, *p.events)
	}
}

// An own PR's entry leaves a held claim, so a later create on another target
// under the same key is refused rather than writing.
func TestSubmitStackOwnPRClaimRefusesLaterOtherTarget(t *testing.T) {
	svc, st, p, _ := newStackServiceForTest(t)
	p.prs["a"] = &forge.PullRequest{Number: 900, Title: "mine", State: "open", HeadRef: "a", BaseRef: "main"}
	seedOwnPR(st, 900, testAgentID)
	if s := statuses(stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, stackCall("k", "", nil, stackEntry("a", ""))))); s[0] != doneStatus {
		t.Fatalf("own PR submit = %v, want done", s)
	}
	if len(st.intents) != 1 || st.intents[0].State != store.WriteStateHeld {
		t.Fatalf("intents = %+v, want one held claim", st.intents)
	}
	creates := p.count("create")
	second := stackCall("k", "", nil, stackEntry("a", "owner/other#7"))
	second.GetSubmitStack().Repo = otherRepo
	delete(p.prs, "a")
	got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, second))
	if got[0].GetStatus() != failedStatus || !strings.Contains(got[0].GetError().GetMessage(), "another target") || p.count("create") != creates {
		t.Fatalf("other target after own claim = %v creates %d->%d, want refused with no create", got, creates, p.count("create"))
	}
}

// Entry 2 fails preflight: every entry still reports a status.
func TestSubmitStackPreflightFailureOnSecondEntryStatusesEveryEntry(t *testing.T) {
	svc, _, p, m := newStackServiceForTest(t)
	p.prs["b"] = &forge.PullRequest{Number: 11, State: "open", HeadRef: "other", BaseRef: "a"}
	got := stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, stackCall("k", "", []byte("bundle"), stackEntry("a", "owner/repo#7"), stackEntry("b", "owner/repo#7"))))
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}
	if got[0].GetStatus() != skippedStatus || got[0].GetHeadRef() != "a" {
		t.Fatalf("entry a = %v, want skipped", got[0])
	}
	if got[1].GetStatus() != failedStatus || got[1].GetError().GetCode() != "failed_precondition" {
		t.Fatalf("entry b = %v, want failed_precondition", got[1])
	}
	if len(m.applied)+len(m.pushes) != 0 || p.count("create")+p.count("update") != 0 {
		t.Fatalf("applied=%v pushes=%v events=%v, want no writes", m.applied, m.pushes, *p.events)
	}
}

// The sweep keeps a held key for a day, then deletes it; an own PR's claim
// leaves nothing behind after the idempotency window.
func TestWriteSweepDeletesHeldClaimAfterADay(t *testing.T) {
	svc, st, p, _ := newStackServiceForTest(t)
	p.prs["a"] = &forge.PullRequest{Number: 900, Title: "mine", State: "open", HeadRef: "a", BaseRef: "main"}
	seedOwnPR(st, 900, testAgentID)
	stackEntries(t, svc.ExecuteForgeCallAsAccountMust(t, stackCall("k", "", nil, stackEntry("a", ""))))
	if len(st.intents) != 1 || st.intents[0].State != store.WriteStateHeld {
		t.Fatalf("intents = %+v, want one held claim", st.intents)
	}
	svc.now = func() time.Time { return st.intents[0].CreatedAt.Add(23 * time.Hour) }
	if err := svc.sweepWriteIntents(t.Context()); err != nil || len(st.intents) != 1 {
		t.Fatalf("sweep within a day = %v, intents %+v; want the claim kept", err, st.intents)
	}
	svc.now = func() time.Time { return st.intents[0].CreatedAt.Add(25 * time.Hour) }
	if err := svc.sweepWriteIntents(t.Context()); err != nil || len(st.intents) != 0 {
		t.Fatalf("sweep after a day = %v, intents %+v; want the claim deleted", err, st.intents)
	}
}
