package usage

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type noopPruner struct{}

func (noopPruner) PruneTokenUsageBefore(context.Context, int64) (int64, error)   { return 0, nil }
func (noopPruner) PruneComputeUsageBefore(context.Context, int64) (int64, error) { return 0, nil }

type noopCloser struct{}

func (noopCloser) CloseOrphanedComputeIntervals(context.Context) (int64, error) { return 0, nil }

func TestSweepsStartOneRootSpan(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	for _, tc := range []struct {
		name string
		run  func(ctx context.Context)
	}{
		{"usage.retention.sweep", func(ctx context.Context) {
			NewRetentionSweeper(noopPruner{}, RetentionConfig{Retention: time.Hour, Log: log}).sweep(ctx)
		}},
		{"usage.compute.sweep", func(ctx context.Context) {
			NewComputeUsageSweeper(noopCloser{}, log).sweep(ctx)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
			prev := otel.GetTracerProvider()
			otel.SetTracerProvider(tp)
			t.Cleanup(func() {
				otel.SetTracerProvider(prev)
				if err := tp.Shutdown(context.WithoutCancel(t.Context())); err != nil {
					t.Errorf("tracer provider shutdown: %v", err)
				}
			})

			ctx, stray := tp.Tracer("test").Start(t.Context(), "stray")
			tc.run(ctx)
			stray.End()

			var got []sdktrace.ReadOnlySpan
			for _, s := range rec.Ended() {
				if s.Name() == tc.name {
					got = append(got, s)
				}
			}
			if len(got) != 1 {
				t.Fatalf("%s spans = %d, want 1", tc.name, len(got))
			}
			if got[0].Parent().IsValid() {
				t.Fatalf("%s parent = %s, want a root", tc.name, got[0].Parent().SpanID())
			}
		})
	}
}
