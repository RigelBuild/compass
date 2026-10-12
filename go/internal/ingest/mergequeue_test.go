package ingest

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/store"
)

func TestPullRequestHydratorMergeQueue(t *testing.T) {
	queueEntry := forge.QueueEntry{Number: 7, State: forge.QueueTesting, ChangedAt: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	for _, tt := range []struct {
		name         string
		configs      QueueConfigs
		queues       map[string]forge.MergeQueue
		queue        *fixedQueue
		wantCalls    int
		wantState    string
		wantProvider string
		wantLogError bool
	}{
		{name: "configured queue attaches entry", configs: fixedQueueConfig{provider: "trunk", branch: "main", repos: []string{"owner/repo"}}, queue: &fixedQueue{entry: queueEntry}, wantCalls: 1, wantState: "testing", wantProvider: "trunk"},
		{name: "configured queue failure keeps hydration green", configs: fixedQueueConfig{provider: "trunk", branch: "main", repos: []string{"owner/repo"}}, queue: &fixedQueue{err: errors.New("offline")}, wantCalls: 1, wantLogError: true},
		{name: "unconfigured repo makes no provider call", queue: &fixedQueue{entry: queueEntry}, wantCalls: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.queue != nil {
				tt.queues = map[string]forge.MergeQueue{"trunk": tt.queue}
			}
			pulls := &fakePulls{}
			sink := &recordingPRSink{}
			log := &queueLogHandler{}
			hydrator := NewPullRequestHydrator(pulls, sink, testForgeRef(), tt.queues, tt.configs, slog.New(log))
			if err := hydrator.Hydrate(context.Background(), "owner/repo", 7); err != nil {
				t.Fatalf("Hydrate: %v", err)
			}
			if tt.queue != nil && tt.queue.calls != tt.wantCalls {
				t.Fatalf("queue calls = %d, want %d", tt.queue.calls, tt.wantCalls)
			}
			if len(sink.got) != 1 {
				t.Fatalf("sink calls = %d, want 1", len(sink.got))
			}
			entry := sink.got[0].PR.GetMergeQueue()
			if tt.wantProvider == "" {
				if entry != nil {
					t.Fatalf("merge queue = %v, want nil", entry)
				}
			} else if entry.GetProvider() != tt.wantProvider || entry.GetState() != tt.wantState || entry.GetChangedAt().AsTime() != queueEntry.ChangedAt {
				t.Fatalf("merge queue = %v, want provider=%s state=%s changed_at=%v", entry, tt.wantProvider, tt.wantState, queueEntry.ChangedAt)
			}
			if log.errors != tt.wantLogError {
				t.Fatalf("queue error logged = %v, want %v", log.errors, tt.wantLogError)
			}
		})
	}
}

type fixedQueueConfig struct {
	provider string
	branch   string
	repos    []string
}

func (c fixedQueueConfig) MergeQueueFor(context.Context, string) (string, string, error) {
	return c.provider, c.branch, nil
}

func (c fixedQueueConfig) MergeQueueRepos(context.Context) ([]string, error) { return c.repos, nil }

func (c fixedQueueConfig) MergeQueueHost() string { return "github.com" }

type fixedQueue struct {
	entry forge.QueueEntry
	err   error
	calls int
}

func (q *fixedQueue) Name() string { return "trunk" }

func (q *fixedQueue) Entries(_ context.Context, _, _ string, numbers []uint64) (map[uint64]forge.QueueEntry, error) {
	q.calls++
	if q.err != nil {
		return nil, q.err
	}
	if len(numbers) == 0 {
		return map[uint64]forge.QueueEntry{}, nil
	}
	return map[uint64]forge.QueueEntry{q.entry.Number: q.entry}, nil
}

type queueLogHandler struct{ errors bool }

func (h *queueLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *queueLogHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		h.errors = true
	}
	return nil
}
func (h *queueLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *queueLogHandler) WithGroup(string) slog.Handler      { return h }

func TestMergeQueueSweeperBatchesAtFifty(t *testing.T) {
	numbers := make([]uint64, 51)
	for i := range numbers {
		numbers[i] = uint64(i + 1)
	}
	queue := &recordingBatchQueue{}
	store := &fixedQueuePRStore{numbers: numbers}
	sink := &recordingQueueSink{}
	configs := fixedQueueConfig{provider: "trunk", branch: "main", repos: []string{"owner/repo"}}
	NewMergeQueueSweeper(map[string]forge.MergeQueue{"trunk": queue}, configs, store, sink, time.Hour).sweep(context.Background())
	if !slices.Equal(queue.batchSizes, []int{50, 1}) {
		t.Fatalf("provider batch sizes = %v, want [50 1]", queue.batchSizes)
	}
	if len(sink.entries) != 51 {
		t.Fatalf("published entries = %d, want 51", len(sink.entries))
	}
}

type recordingBatchQueue struct{ batchSizes []int }

func (*recordingBatchQueue) Name() string { return "trunk" }

func (q *recordingBatchQueue) Entries(_ context.Context, _, _ string, numbers []uint64) (map[uint64]forge.QueueEntry, error) {
	q.batchSizes = append(q.batchSizes, len(numbers))
	return map[uint64]forge.QueueEntry{}, nil
}

func TestMergeQueueSweeperPublishesConfiguredEntries(t *testing.T) {
	entry := forge.QueueEntry{Number: 7, State: forge.QueueTesting, ChangedAt: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	queue := &fixedQueue{entry: entry}
	configs := fixedQueueConfig{provider: "trunk", branch: "main", repos: []string{"owner/repo"}}
	store := &fixedQueuePRStore{numbers: []uint64{7}}
	sink := &recordingQueueSink{}
	sweeper := NewMergeQueueSweeper(map[string]forge.MergeQueue{"trunk": queue}, configs, store, sink, time.Hour)
	sweeper.sweep(context.Background())
	if queue.calls != 1 || store.calls != 1 || len(sink.entries) != 1 {
		t.Fatalf("calls = provider %d, store %d, sink %d; want 1 each", queue.calls, store.calls, len(sink.entries))
	}
	got := sink.entries[0]
	if got.GetProvider() != "trunk" || got.GetState() != "testing" || !got.GetChangedAt().AsTime().Equal(entry.ChangedAt) {
		t.Fatalf("published entry = %v, want testing entry for PR #7", got)
	}
}

func TestMergeQueueSweeperClearsMissingEntries(t *testing.T) {
	queue := &fixedQueue{entry: forge.QueueEntry{Number: 99}}
	configs := fixedQueueConfig{provider: "trunk", branch: "main", repos: []string{"owner/repo"}}
	sink := &recordingQueueSink{}
	NewMergeQueueSweeper(map[string]forge.MergeQueue{"trunk": queue}, configs, &fixedQueuePRStore{numbers: []uint64{7}}, sink, time.Hour).sweep(context.Background())
	if len(sink.entries) != 1 || sink.entries[0] != nil {
		t.Fatalf("missing queue entry published = %v, want nil removal", sink.entries)
	}
}

type fixedQueuePRStore struct {
	numbers []uint64
	calls   int
}

func (s *fixedQueuePRStore) OpenPullRequests(context.Context, string) ([]uint64, error) {
	s.calls++
	return s.numbers, nil
}

type recordingQueueSink struct{ entries []*compassv1.MergeQueueEntry }

func (s *recordingQueueSink) PublishPullRequestMergeQueue(_ context.Context, _ store.ForgeCoord, entry *compassv1.MergeQueueEntry) error {
	s.entries = append(s.entries, entry)
	return nil
}
