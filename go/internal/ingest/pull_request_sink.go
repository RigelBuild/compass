package ingest

import (
	"context"
	"fmt"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
)

// IngestedPullRequest carries what the wire PullRequest lacks: forge times and closing refs.
type IngestedPullRequest struct {
	PR          *compassv1.PullRequest
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ClosingRefs []forge.IssueRef
}

// pullRequestReader is the PR read surface, satisfied by *forge.GitHub.
type pullRequestReader interface {
	GetPullRequest(ctx context.Context, repo string, number uint64) (forge.PullRequest, error)
	ListOpenPullRequests(ctx context.Context, repo string) ([]forge.UpdatedPull, error)
}

// prSink receives each hydrated PR; the board's IssueProjection satisfies it.
type prSink interface {
	PublishPullRequestUpdate(ctx context.Context, in IngestedPullRequest) error
}

// PullRequestHydrator reads one PR, translates it the way the create path does,
// and sinks it to the board.
type PullRequestHydrator struct {
	reader   pullRequestReader
	sink     prSink
	forgeRef *compassv1.ForgeRef
}

// NewPullRequestHydrator returns a hydrator reading from r, stamping ref and
// sinking to s.
func NewPullRequestHydrator(r pullRequestReader, s prSink, ref *compassv1.ForgeRef) *PullRequestHydrator {
	return &PullRequestHydrator{reader: r, sink: s, forgeRef: ref}
}

// Hydrate reads PR number in repo and sinks it. Errors wrap the cause, so a
// caller can errors.Is forge.ErrBudgetExhausted.
func (h *PullRequestHydrator) Hydrate(ctx context.Context, repo string, number uint64) error {
	raw, err := h.reader.GetPullRequest(ctx, repo, number)
	if err != nil {
		return fmt.Errorf("ingest: read pull request #%d for %q: %w", number, repo, err)
	}
	_, author, ok := forge.StripOwner(raw.Body)
	var attr *compassv1.AgentAttribution
	if ok && author.AgentHandle != "" {
		attr = &compassv1.AgentAttribution{AgentHandle: author.AgentHandle}
	}
	pr := forge.TranslatePullRequest(raw, attr)
	pr.Forge = &compassv1.ForgeRef{Provider: h.forgeRef.GetProvider(), Host: h.forgeRef.GetHost()}
	pr.Repo = repo
	in := IngestedPullRequest{PR: pr, CreatedAt: raw.CreatedAt, UpdatedAt: raw.UpdatedAt, ClosingRefs: raw.ClosingRefs}
	if err := h.sink.PublishPullRequestUpdate(ctx, in); err != nil {
		return fmt.Errorf("ingest: publish pull request #%d for %q: %w", number, repo, err)
	}
	return nil
}

// listOpen lists the repo's open PRs for the backfill pass.
func (h *PullRequestHydrator) listOpen(ctx context.Context, repo string) ([]forge.UpdatedPull, error) {
	return h.reader.ListOpenPullRequests(ctx, repo)
}
