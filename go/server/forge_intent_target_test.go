//go:build unix

package server

import (
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
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
