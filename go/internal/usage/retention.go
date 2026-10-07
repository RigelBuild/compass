package usage

import (
	"context"
	"log/slog"
	"time"
)

// DefaultRetention is the server's default window for raw events. The rollups
// outlive it, so the usage charts keep their history.
const DefaultRetention = 90 * 24 * time.Hour

// sweepInterval is the time between prunes. The cutoff moves one UTC day at a
// time, so a faster sweep would only rescan the same events.
const sweepInterval = 24 * time.Hour

// pruner is the raw-event retention surface the sweeper drives.
type pruner interface {
	PruneTokenUsageBefore(ctx context.Context, beforeUnixMs int64) (int64, error)
	PruneComputeUsageBefore(ctx context.Context, beforeUnixMs int64) (int64, error)
}

// RetentionConfig configures a RetentionSweeper.
type RetentionConfig struct {
	// Retention is how long raw events are kept. 0 or less keeps every event,
	// because the sweep does not run.
	Retention time.Duration
	// Log is the sweep logger; nil uses slog.Default().
	Log *slog.Logger
}

// RetentionSweeper deletes raw usage events older than the configured window.
type RetentionSweeper struct {
	store     pruner
	retention time.Duration
	log       *slog.Logger
}

// NewRetentionSweeper returns a sweeper that prunes s.
func NewRetentionSweeper(s pruner, cfg RetentionConfig) *RetentionSweeper {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &RetentionSweeper{store: s, retention: cfg.Retention, log: log}
}

// Run prunes at once and then on every sweepInterval tick, until ctx ends. It
// never returns a prune error: in the serve group that would stop the server.
func (w *RetentionSweeper) Run(ctx context.Context) error {
	if w.retention <= 0 {
		w.log.InfoContext(ctx, "usage retention: raw-event prune disabled")
		return nil
	}
	w.sweep(ctx)
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.sweep(ctx)
		}
	}
}

// sweep runs one prune for each raw usage log.
func (w *RetentionSweeper) sweep(ctx context.Context) {
	cutoff := time.Now().Add(-w.retention).UnixMilli()
	if deleted, err := w.store.PruneTokenUsageBefore(ctx, cutoff); err != nil {
		if ctx.Err() == nil {
			w.log.ErrorContext(ctx, "usage retention: prune raw token-usage events", "error", err)
		}
	} else if deleted > 0 {
		w.log.InfoContext(ctx, "usage retention: pruned raw token-usage events",
			"deleted", deleted, "retention", w.retention)
	}
	if deleted, err := w.store.PruneComputeUsageBefore(ctx, cutoff); err != nil {
		if ctx.Err() == nil {
			w.log.ErrorContext(ctx, "usage retention: prune raw compute-usage events", "error", err)
		}
	} else if deleted > 0 {
		w.log.InfoContext(ctx, "usage retention: pruned raw compute-usage events",
			"deleted", deleted, "retention", w.retention)
	}
}
