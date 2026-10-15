//go:build unix

package board

import (
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

func TestPrGate(t *testing.T) {
	for _, tt := range prGateCases {
		t.Run(tt.name, func(t *testing.T) {
			if got := PrGate(tt.pr); got != tt.want {
				t.Errorf("PrGate() = %v, want %v", got, tt.want)
			}
		})
	}
}

var prGateCases = []struct {
	name string
	pr   *compassv1.PullRequest
	want compassv1.BoardPrGate
}{
	{
		name: "nil pull request",
		want: compassv1.BoardPrGate_BOARD_PR_GATE_UNSPECIFIED,
	},
	{
		name: "merged",
		pr:   &compassv1.PullRequest{ForgeState: "merged"},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_MERGED,
	},
	{
		name: "merged queue entry",
		pr:   &compassv1.PullRequest{MergeQueue: &compassv1.MergeQueueEntry{State: "merged"}},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_MERGED,
	},
	{
		name: "closed",
		pr:   &compassv1.PullRequest{ForgeState: "closed"},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_CLOSED,
	},
	{
		name: "queued",
		pr:   &compassv1.PullRequest{MergeQueue: &compassv1.MergeQueueEntry{State: "queued"}},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_QUEUED,
	},
	{
		name: "testing",
		pr:   &compassv1.PullRequest{MergeQueue: &compassv1.MergeQueueEntry{State: "testing"}},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_QUEUED,
	},
	{
		name: "queue failed",
		pr:   &compassv1.PullRequest{MergeQueue: &compassv1.MergeQueueEntry{State: "failed"}},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_QUEUE_FAILED,
	},
	{
		name: "queue removed",
		pr:   &compassv1.PullRequest{MergeQueue: &compassv1.MergeQueueEntry{State: "removed"}},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_QUEUE_REMOVED,
	},
	{
		name: "draft takes precedence over conflicts",
		pr:   &compassv1.PullRequest{Draft: true, MergeableState: "dirty"},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_DRAFT,
	},
	{
		name: "conflicts take precedence over failed checks",
		pr:   &compassv1.PullRequest{MergeableState: "dirty", Checks: &compassv1.ChecksSummary{State: "failure"}},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_CONFLICTS,
	},
	{
		name: "failed required check takes precedence over changes requested",
		pr: &compassv1.PullRequest{
			Checks:  &compassv1.ChecksSummary{Checks: []*compassv1.Check{{Required: true, State: "failure"}}},
			Reviews: []*compassv1.Review{{Author: "reviewer", Verdict: "changes_requested"}},
		},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_CHECKS_FAILED,
	},
	{
		name: "queued takes precedence over failed checks",
		pr: &compassv1.PullRequest{
			MergeQueue: &compassv1.MergeQueueEntry{State: "queued"},
			Checks:     &compassv1.ChecksSummary{State: "failure"},
		},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_QUEUED,
	},
	{
		name: "required success overrides failed rollup",
		pr: &compassv1.PullRequest{Checks: &compassv1.ChecksSummary{
			State: "failure", Checks: []*compassv1.Check{{Required: true, State: "success"}},
		}},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_AWAITING_REVIEW,
	},
	{
		name: "required check failed",
		pr: &compassv1.PullRequest{Checks: &compassv1.ChecksSummary{
			Checks: []*compassv1.Check{{Required: true, State: "failure"}},
		}},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_CHECKS_FAILED,
	},
	{
		name: "failed non-required check does not block",
		pr: &compassv1.PullRequest{Checks: &compassv1.ChecksSummary{
			State: "success", Checks: []*compassv1.Check{{Required: false, State: "failure"}},
		}},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_AWAITING_REVIEW,
	},
	{
		name: "required check pending",
		pr: &compassv1.PullRequest{Checks: &compassv1.ChecksSummary{
			Checks: []*compassv1.Check{{Required: true, State: "in_progress"}},
		}},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_CHECKS_PENDING,
	},
	{
		name: "rollup failure without required checks",
		pr:   &compassv1.PullRequest{Checks: &compassv1.ChecksSummary{State: "failure"}},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_CHECKS_FAILED,
	},
	{
		name: "rollup pending without required checks",
		pr:   &compassv1.PullRequest{Checks: &compassv1.ChecksSummary{State: "pending"}},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_CHECKS_PENDING,
	},
	{
		name: "missing checks is pending",
		pr:   &compassv1.PullRequest{Reviews: []*compassv1.Review{{Author: "reviewer", Verdict: "approved"}}},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_CHECKS_PENDING,
	},
	{
		name: "empty rollup is pending",
		pr: &compassv1.PullRequest{
			Checks:  &compassv1.ChecksSummary{},
			Reviews: []*compassv1.Review{{Author: "reviewer", Verdict: "approved"}},
		},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_CHECKS_PENDING,
	},
	{
		name: "latest review per author",
		pr: &compassv1.PullRequest{
			Checks: &compassv1.ChecksSummary{State: "success"},
			Reviews: []*compassv1.Review{
				{Author: "reviewer", Verdict: "approved"},
				{Author: "reviewer", Verdict: "changes_requested"},
			},
		},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_CHANGES_REQUESTED,
	},
	{
		name: "approval survives a later comment",
		pr: &compassv1.PullRequest{
			Checks: &compassv1.ChecksSummary{State: "success"},
			Reviews: []*compassv1.Review{
				{Author: "reviewer", Verdict: "approved"},
				{Author: "reviewer", Verdict: "commented"},
			},
		},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_AWAITING_ENQUEUE,
	},
	{
		name: "changes requested survives a later comment",
		pr: &compassv1.PullRequest{
			Checks: &compassv1.ChecksSummary{State: "success"},
			Reviews: []*compassv1.Review{
				{Author: "reviewer", Verdict: "changes_requested"},
				{Author: "reviewer", Verdict: "commented"},
			},
		},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_CHANGES_REQUESTED,
	},
	{
		name: "approval by another author does not erase changes requested",
		pr: &compassv1.PullRequest{
			Checks: &compassv1.ChecksSummary{State: "success"},
			Reviews: []*compassv1.Review{
				{Author: "reviewer", Verdict: "changes_requested"},
				{Author: "other", Verdict: "approved"},
			},
		},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_CHANGES_REQUESTED,
	},
	{
		name: "dismissed review clears that author's approval",
		pr: &compassv1.PullRequest{
			Checks: &compassv1.ChecksSummary{State: "success"},
			Reviews: []*compassv1.Review{
				{Author: "reviewer", Verdict: "approved"},
				{Author: "reviewer", Verdict: "dismissed"},
			},
		},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_AWAITING_REVIEW,
	},
	{
		name: "dismissed review clears changes requested without erasing another approval",
		pr: &compassv1.PullRequest{
			Checks: &compassv1.ChecksSummary{State: "success"},
			Reviews: []*compassv1.Review{
				{Author: "reviewer", Verdict: "changes_requested"},
				{Author: "other", Verdict: "approved"},
				{Author: "reviewer", Verdict: "dismissed"},
			},
		},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_AWAITING_ENQUEUE,
	},

	{
		name: "bot approval is ignored",
		pr: &compassv1.PullRequest{
			Checks:  &compassv1.ChecksSummary{State: "success"},
			Reviews: []*compassv1.Review{{Author: "bot", IsBot: true, Verdict: "approved"}},
		},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_AWAITING_REVIEW,
	},
	{
		name: "no approval",
		pr:   &compassv1.PullRequest{Checks: &compassv1.ChecksSummary{State: "success"}},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_AWAITING_REVIEW,
	},
	{
		name: "stale approval awaits review",
		pr: &compassv1.PullRequest{
			Checks:  &compassv1.ChecksSummary{State: "success"},
			HeadSha: "new",
			Reviews: []*compassv1.Review{{Author: "reviewer", Verdict: "approved", CommitId: "old"}},
		},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_AWAITING_REVIEW,
	},
	{
		name: "approval without commit id",
		pr: &compassv1.PullRequest{
			Checks:  &compassv1.ChecksSummary{State: "success"},
			HeadSha: "new",
			Reviews: []*compassv1.Review{{Author: "reviewer", Verdict: "approved"}},
		},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_AWAITING_ENQUEUE,
	},
	{
		name: "approval for head",
		pr: &compassv1.PullRequest{
			Checks:  &compassv1.ChecksSummary{State: "success"},
			HeadSha: "head",
			Reviews: []*compassv1.Review{{Author: "reviewer", Verdict: "approved", CommitId: "head"}},
		},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_AWAITING_ENQUEUE,
	},
	{
		name: "unresolved thread waits for review",
		pr: &compassv1.PullRequest{
			Checks:  &compassv1.ChecksSummary{State: "success"},
			Reviews: []*compassv1.Review{{Author: "reviewer", Verdict: "approved"}},
			Threads: []*compassv1.ReviewThread{{Resolved: false}},
		},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_AWAITING_REVIEW,
	},
	{
		name: "resolved thread allows enqueue",
		pr: &compassv1.PullRequest{
			Checks:  &compassv1.ChecksSummary{State: "success"},
			Reviews: []*compassv1.Review{{Author: "reviewer", Verdict: "approved"}},
			Threads: []*compassv1.ReviewThread{{Resolved: true}},
		},
		want: compassv1.BoardPrGate_BOARD_PR_GATE_AWAITING_ENQUEUE,
	},
}
