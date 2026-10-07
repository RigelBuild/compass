package usage_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/RigelBuild/compass/go/internal/usage"
)

// chanPruner hands each prune cutoff to the test. Blocking on the receive is
// what lets the synctest clock run forward to the next tick.
type chanPruner struct {
	cutoffs        chan int64
	computeCutoffs chan int64
	err            error
}

func (p *chanPruner) PruneTokenUsageBefore(ctx context.Context, beforeUnixMs int64) (int64, error) {
	select {
	case p.cutoffs <- beforeUnixMs:
		return 0, p.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (p *chanPruner) PruneComputeUsageBefore(ctx context.Context, beforeUnixMs int64) (int64, error) {
	select {
	case p.computeCutoffs <- beforeUnixMs:
		return 0, p.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func TestRetentionSweeperPrunesAtStartAndDaily(t *testing.T) {
	const day = 24 * time.Hour
	tests := []struct {
		name      string
		retention time.Duration // the configured window
		window    time.Duration // the window the cutoffs must use
		err       error         // what every prune returns
	}{
		{name: "configured_window", retention: 7 * day, window: 7 * day},
		{name: "failed_prune_is_retried_next_day", retention: day, window: day, err: errors.New("database down")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := &chanPruner{cutoffs: make(chan int64), computeCutoffs: make(chan int64), err: tt.err}
				w := usage.NewRetentionSweeper(p, usage.RetentionConfig{
					Retention: tt.retention,
					Log:       slog.New(slog.DiscardHandler),
				})
				ctx, cancel := context.WithCancel(t.Context())
				errc := make(chan error, 1)
				start := time.Now()
				go func() { errc <- w.Run(ctx) }()

				for _, at := range []time.Time{start, start.Add(day)} {
					want := at.Add(-tt.window).UnixMilli()
					if got := <-p.cutoffs; got != want {
						t.Fatalf("token prune cutoff = %d, want %d", got, want)
					}
					if got := <-p.computeCutoffs; got != want {
						t.Fatalf("compute prune cutoff = %d, want %d", got, want)
					}
					if now := time.Now(); !now.Equal(at) {
						t.Fatalf("prune ran at %v, want %v", now, at)
					}
				}
				cancel()
				if err := <-errc; err != nil {
					t.Fatalf("Run = %v, want nil after cancel", err)
				}
			})
		})
	}
}

func TestRetentionSweeperZeroRetentionPrunesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// A prune blocks on the unread channel, so Run returns only if it never prunes.
		p := &chanPruner{cutoffs: make(chan int64)}
		w := usage.NewRetentionSweeper(p, usage.RetentionConfig{Log: slog.New(slog.DiscardHandler)})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		errc := make(chan error, 1)
		go func() { errc <- w.Run(ctx) }()
		synctest.Wait()
		select {
		case err := <-errc:
			if err != nil {
				t.Fatalf("Run = %v, want nil", err)
			}
		default:
			t.Fatal("Run with a zero retention window is still running, want it to return without pruning")
		}
	})
}
