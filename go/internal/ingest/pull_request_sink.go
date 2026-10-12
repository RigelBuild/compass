package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
	"google.golang.org/protobuf/types/known/timestamppb"
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
	reader    pullRequestReader
	sink      prSink
	forgeRef  *compassv1.ForgeRef
	queues    map[string]forge.MergeQueue
	queueCfgs QueueConfigs
	log       *slog.Logger
}

// NewPullRequestHydrator returns a hydrator reading from r, stamping ref and
// sinking to s. Queue lookup is optional and never blocks a successful PR hydrate.
func NewPullRequestHydrator(
	r pullRequestReader,
	s prSink,
	ref *compassv1.ForgeRef,
	queues map[string]forge.MergeQueue,
	queueCfgs QueueConfigs,
	log *slog.Logger,
) *PullRequestHydrator {
	if log == nil {
		log = slog.Default()
	}
	return &PullRequestHydrator{reader: r, sink: s, forgeRef: ref, queues: queues, queueCfgs: queueCfgs, log: log}
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
	if entry := h.mergeQueueEntry(ctx, repo, number); entry != nil {
		pr.MergeQueue = entry
	}
	in := IngestedPullRequest{PR: pr, CreatedAt: raw.CreatedAt, UpdatedAt: raw.UpdatedAt, ClosingRefs: raw.ClosingRefs}
	if err := h.sink.PublishPullRequestUpdate(ctx, in); err != nil {
		return fmt.Errorf("ingest: publish pull request #%d for %q: %w", number, repo, err)
	}
	return nil
}

func (h *PullRequestHydrator) mergeQueueEntry(ctx context.Context, repo string, number uint64) *compassv1.MergeQueueEntry {
	if h.queueCfgs == nil {
		return nil
	}
	provider, branch, err := h.queueCfgs.MergeQueueFor(ctx, repo)
	if err != nil {
		h.log.ErrorContext(ctx, "pull request hydrate: merge queue config", "repo", repo, "error", err)
		return nil
	}
	if provider == "" {
		return nil
	}
	queue := h.queues[provider]
	if queue == nil {
		h.log.ErrorContext(ctx, "pull request hydrate: merge queue provider unavailable", "repo", repo, "provider", provider)
		return nil
	}
	entries, err := queue.Entries(ctx, repo, branch, []uint64{number})
	if err != nil {
		h.log.ErrorContext(ctx, "pull request hydrate: merge queue lookup", "repo", repo, "number", number, "error", err)
		return nil
	}
	entry, ok := entries[number]
	if !ok {
		return nil
	}
	return &compassv1.MergeQueueEntry{Provider: provider, State: queueStateName(entry.State), ChangedAt: timestamppb.New(entry.ChangedAt)}
}

// listOpen lists the repo's open PRs for the backfill pass.
func (h *PullRequestHydrator) listOpen(ctx context.Context, repo string) ([]forge.UpdatedPull, error) {
	return h.reader.ListOpenPullRequests(ctx, repo)
}
