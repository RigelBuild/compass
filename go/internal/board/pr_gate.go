//go:build unix

package board

import (
	"strings"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

// derivePRState derives a gate and stale-approval flag from one review pass.
func derivePRState(pr *compassv1.PullRequest) (compassv1.BoardPrGate, bool) {
	if pr == nil {
		return compassv1.BoardPrGate_BOARD_PR_GATE_UNSPECIFIED, false
	}
	reviews := reviewState(pr)
	return prGate(pr, reviews), reviews.staleApproval
}

// PrGate derives the first unmet pull-request gate from its stored facts.
func PrGate(pr *compassv1.PullRequest) compassv1.BoardPrGate {
	gate, _ := derivePRState(pr)
	return gate
}

func prGate(pr *compassv1.PullRequest, reviews reviewStateResult) compassv1.BoardPrGate {
	switch {
	case strings.EqualFold(pr.GetForgeState(), "merged"), strings.EqualFold(pr.GetMergeQueue().GetState(), "merged"):
		return compassv1.BoardPrGate_BOARD_PR_GATE_MERGED
	case strings.EqualFold(pr.GetForgeState(), "closed"):
		return compassv1.BoardPrGate_BOARD_PR_GATE_CLOSED
	case queueGate(pr.GetMergeQueue().GetState()) != compassv1.BoardPrGate_BOARD_PR_GATE_UNSPECIFIED:
		return queueGate(pr.GetMergeQueue().GetState())
	case pr.GetDraft():
		return compassv1.BoardPrGate_BOARD_PR_GATE_DRAFT
	case strings.EqualFold(pr.GetMergeableState(), "dirty"):
		return compassv1.BoardPrGate_BOARD_PR_GATE_CONFLICTS
	}

	if gate := checksGate(pr.GetChecks()); gate != compassv1.BoardPrGate_BOARD_PR_GATE_UNSPECIFIED {
		return gate
	}
	if reviews.changesRequested {
		return compassv1.BoardPrGate_BOARD_PR_GATE_CHANGES_REQUESTED
	}
	if !reviews.approved {
		return compassv1.BoardPrGate_BOARD_PR_GATE_AWAITING_REVIEW
	}
	for _, thread := range pr.GetThreads() {
		if thread != nil && !thread.GetResolved() {
			return compassv1.BoardPrGate_BOARD_PR_GATE_AWAITING_REVIEW
		}
	}
	return compassv1.BoardPrGate_BOARD_PR_GATE_AWAITING_ENQUEUE
}

func queueGate(state string) compassv1.BoardPrGate {
	switch strings.ToLower(state) {
	case "queued", "testing":
		return compassv1.BoardPrGate_BOARD_PR_GATE_QUEUED
	case "failed":
		return compassv1.BoardPrGate_BOARD_PR_GATE_QUEUE_FAILED
	case "removed":
		return compassv1.BoardPrGate_BOARD_PR_GATE_QUEUE_REMOVED
	default:
		return compassv1.BoardPrGate_BOARD_PR_GATE_UNSPECIFIED
	}
}

func checksGate(checks *compassv1.ChecksSummary) compassv1.BoardPrGate {
	if checks == nil {
		return compassv1.BoardPrGate_BOARD_PR_GATE_CHECKS_PENDING
	}
	hasRequired := false
	for _, check := range checks.GetChecks() {
		if check == nil || !check.GetRequired() {
			continue
		}
		hasRequired = true
		if checkStateGate(check.GetState()) == compassv1.BoardPrGate_BOARD_PR_GATE_CHECKS_FAILED {
			return compassv1.BoardPrGate_BOARD_PR_GATE_CHECKS_FAILED
		}
	}
	if !hasRequired {
		if strings.TrimSpace(checks.GetState()) == "" {
			return compassv1.BoardPrGate_BOARD_PR_GATE_CHECKS_PENDING
		}
		return checkStateGate(checks.GetState())
	}
	for _, check := range checks.GetChecks() {
		if check != nil && check.GetRequired() &&
			checkStateGate(check.GetState()) == compassv1.BoardPrGate_BOARD_PR_GATE_CHECKS_PENDING {
			return compassv1.BoardPrGate_BOARD_PR_GATE_CHECKS_PENDING
		}
	}
	return compassv1.BoardPrGate_BOARD_PR_GATE_UNSPECIFIED
}

func checkStateGate(state string) compassv1.BoardPrGate {
	switch strings.ToLower(state) {
	case "failure", "cancelled":
		return compassv1.BoardPrGate_BOARD_PR_GATE_CHECKS_FAILED
	case "success", "neutral", "":
		return compassv1.BoardPrGate_BOARD_PR_GATE_UNSPECIFIED
	default:
		return compassv1.BoardPrGate_BOARD_PR_GATE_CHECKS_PENDING
	}
}

type reviewStateResult struct {
	changesRequested bool
	approved         bool
	staleApproval    bool
}

func reviewState(pr *compassv1.PullRequest) reviewStateResult {
	latest := make(map[string]*compassv1.Review, len(pr.GetReviews()))
	for _, review := range pr.GetReviews() {
		if review == nil || review.GetIsBot() || review.GetAuthor() == "" {
			continue
		}
		switch strings.ToLower(review.GetVerdict()) {
		case "approved", "changes_requested":
			latest[review.GetAuthor()] = review
		case "dismissed":
			delete(latest, review.GetAuthor())
		}
	}
	var result reviewStateResult
	for _, review := range latest {
		switch strings.ToLower(review.GetVerdict()) {
		case "changes_requested":
			result.changesRequested = true
		case "approved":
			if review.GetCommitId() != "" && review.GetCommitId() != pr.GetHeadSha() {
				result.staleApproval = true
			} else {
				result.approved = true
			}
		}
	}
	return result
}
