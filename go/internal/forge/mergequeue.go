package forge

import (
	"context"
	"time"
)

// QueueState is the normalized merge-queue state for a pull request.
type QueueState int

const (
	QueueQueued QueueState = iota
	QueueTesting
	QueueFailed
	QueueRemoved
	QueueMerged
)

// QueueEntry is one observed merge-queue entry.
type QueueEntry struct {
	Number    uint64
	State     QueueState
	ChangedAt time.Time
}

// MergeQueue reads pull-request state from a configured merge queue.
type MergeQueue interface {
	Name() string
	Entries(ctx context.Context, repo, targetBranch string, numbers []uint64) (map[uint64]QueueEntry, error)
}
