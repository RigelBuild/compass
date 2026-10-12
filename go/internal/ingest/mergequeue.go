package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const mergeQueueBatchSize = 50

// QueueConfigs resolves the queue config for each configured repository and
// lists the repositories the sweep refreshes.
type QueueConfigs interface {
	MergeQueueFor(ctx context.Context, repo string) (provider, branch string, err error)
	MergeQueueRepos(ctx context.Context) ([]string, error)
	MergeQueueHost() string
}

// MergeQueueStore lists open pull requests for one repository.
type MergeQueueStore interface {
	OpenPullRequests(ctx context.Context, repo string) ([]uint64, error)
}

// MergeQueueSink stores a queue entry and republishes linked board issues when it changes.
type MergeQueueSink interface {
	PublishPullRequestMergeQueue(ctx context.Context, coord store.ForgeCoord, entry *compassv1.MergeQueueEntry) error
}

// MergeQueueSweeper polls configured queue providers and publishes state changes.
type MergeQueueSweeper struct {
	queues map[string]forge.MergeQueue
	cfgs   QueueConfigs
	store  MergeQueueStore
	sink   MergeQueueSink
	every  time.Duration
	log    *slog.Logger
}

// NewMergeQueueSweeper builds the periodic merge-queue refresh worker.
func NewMergeQueueSweeper(queues map[string]forge.MergeQueue, cfgs QueueConfigs, st MergeQueueStore, sink MergeQueueSink, every time.Duration) *MergeQueueSweeper {
	if every <= 0 {
		every = 2 * time.Minute
	}
	return &MergeQueueSweeper{queues: queues, cfgs: cfgs, store: st, sink: sink, every: every, log: slog.Default()}
}

// Run sweeps once at startup, then at every cadence until ctx ends.
func (s *MergeQueueSweeper) Run(ctx context.Context) error {
	s.sweep(ctx)
	ticker := time.NewTicker(s.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

func (s *MergeQueueSweeper) sweep(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	repos, err := s.cfgs.MergeQueueRepos(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "merge queue sweep: list configured repos", "error", err)
		return
	}
	for _, repo := range repos {
		if ctx.Err() != nil {
			return
		}
		provider, branch, err := s.cfgs.MergeQueueFor(ctx, repo)
		if err != nil {
			s.log.ErrorContext(ctx, "merge queue sweep: resolve repo config", "repo", repo, "error", err)
			continue
		}
		queue := s.queues[provider]
		if queue == nil {
			s.log.ErrorContext(ctx, "merge queue sweep: provider unavailable", "repo", repo, "provider", provider)
			continue
		}
		numbers, err := s.store.OpenPullRequests(ctx, repo)
		if err != nil {
			s.log.ErrorContext(ctx, "merge queue sweep: list open pull requests", "repo", repo, "error", err)
			continue
		}
		for start := 0; start < len(numbers); start += mergeQueueBatchSize {
			end := min(start+mergeQueueBatchSize, len(numbers))
			entries, err := queue.Entries(ctx, repo, branch, numbers[start:end])
			if err != nil {
				s.log.ErrorContext(ctx, "merge queue sweep: read provider entries", "repo", repo, "error", err)
				break
			}
			if err := s.publishChanged(ctx, s.cfgs.MergeQueueHost(), repo, provider, numbers[start:end], entries); err != nil {
				s.log.ErrorContext(ctx, "merge queue sweep: publish entries", "repo", repo, "error", err)
				break
			}
		}
	}
}

func (s *MergeQueueSweeper) publishChanged(ctx context.Context, host, repo, provider string, numbers []uint64, entries map[uint64]forge.QueueEntry) error {
	for _, number := range numbers {
		entry, ok := entries[number]
		var wire *compassv1.MergeQueueEntry
		if ok {
			wire = &compassv1.MergeQueueEntry{Provider: provider, State: queueStateName(entry.State), ChangedAt: timestamppb.New(entry.ChangedAt)}
		}
		coord := store.ForgeCoord{Provider: store.ForgeProviderGitHub, Host: host, Repo: repo, Number: number}
		if err := s.sink.PublishPullRequestMergeQueue(ctx, coord, wire); err != nil {
			return fmt.Errorf("publish pull request #%d queue state: %w", number, err)
		}
	}
	return nil
}

func queueStateName(state forge.QueueState) string {
	switch state {
	case forge.QueueQueued:
		return "queued"
	case forge.QueueTesting:
		return "testing"
	case forge.QueueFailed:
		return "failed"
	case forge.QueueRemoved:
		return "removed"
	case forge.QueueMerged:
		return "merged"
	default:
		return ""
	}
}
