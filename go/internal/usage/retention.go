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

// pruner is the part of Store the sweeper drives.
type pruner interface {
	PruneTokenUsageBefore(ctx context.Context, beforeUnixMs int64) (int64, error)
}

// RetentionConfig configures a RetentionSweeper.
type RetentionConfig struct {
	// Retention is how long raw events are kept. 0 or less keeps every event,
	// because the sweep does not run.
	Retention time.Duration
	// Log is the sweep logger; nil uses slog.Default().
	Log *slog.Logger
}

// RetentionSweeper deletes the raw events older than the retention window.
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
		// The serve group treats a nil return as a clean exit, not a failure.
		w.log.InfoContext(ctx, "usage retention: raw token-usage prune disabled")
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

// sweep runs one prune. A failed prune is logged, and the next tick retries it.
func (w *RetentionSweeper) sweep(ctx context.Context) {
	deleted, err := w.store.PruneTokenUsageBefore(ctx, time.Now().Add(-w.retention).UnixMilli())
	switch {
	case ctx.Err() != nil:
		// Shutdown interrupted the prune; the next start sweeps again.
	case err != nil:
		w.log.ErrorContext(ctx, "usage retention: prune raw token-usage events", "error", err)
	case deleted > 0:
		w.log.InfoContext(ctx, "usage retention: pruned raw token-usage events",
			"deleted", deleted, "retention", w.retention)
	}
}
