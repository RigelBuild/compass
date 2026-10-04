package usage

import (
	"context"
	"log/slog"
	"time"
)

const computeSweepInterval = time.Hour

type computeIntervalCloser interface {
	CloseOrphanedComputeIntervals(ctx context.Context) (int64, error)
}

// ComputeUsageSweeper closes intervals left without a binding or end event.
type ComputeUsageSweeper struct {
	store computeIntervalCloser
	log   *slog.Logger
}

// NewComputeUsageSweeper returns a sweeper that closes orphaned intervals.
func NewComputeUsageSweeper(s computeIntervalCloser, log *slog.Logger) *ComputeUsageSweeper {
	if log == nil {
		log = slog.Default()
	}
	return &ComputeUsageSweeper{store: s, log: log}
}

// Run sweeps once at startup and then hourly until ctx ends. A failed sweep is
// logged and retried on the next tick so it cannot stop the serve group.
func (w *ComputeUsageSweeper) Run(ctx context.Context) error {
	w.sweep(ctx)
	ticker := time.NewTicker(computeSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			w.sweep(ctx)
		}
	}
}

func (w *ComputeUsageSweeper) sweep(ctx context.Context) {
	closed, err := w.store.CloseOrphanedComputeIntervals(ctx)
	switch {
	case ctx.Err() != nil:
	case err != nil:
		w.log.ErrorContext(ctx, "compute usage: close orphaned intervals", "error", err)
	case closed > 0:
		w.log.InfoContext(ctx, "compute usage: closed orphaned intervals", "closed", closed)
	}
}
