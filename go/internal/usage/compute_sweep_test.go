package usage_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/RigelBuild/compass/go/internal/usage"
)

type computeCloser struct {
	calls int
}

func (c *computeCloser) CloseOrphanedComputeIntervals(context.Context) (int64, error) {
	c.calls++
	return 0, nil
}

func TestComputeUsageSweeperRunsImmediatelyAndStopsOnCancellation(t *testing.T) {
	closer := &computeCloser{}
	sweeper := usage.NewComputeUsageSweeper(closer, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := sweeper.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil after cancellation", err)
	}
	if closer.calls != 1 {
		t.Fatalf("CloseOrphanedComputeIntervals calls = %d, want one immediate pass", closer.calls)
	}
}
