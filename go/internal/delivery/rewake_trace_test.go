//go:build unix

package delivery

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/RigelBuild/compass/go/internal/store"
)

// spanWaker records the span context each wake ran under.
type spanWaker struct{ woke chan trace.SpanContext }

func (w spanWaker) WakeAgent(ctx context.Context, _ store.AccountID) {
	w.woke <- trace.SpanContextFromContext(ctx)
}

func TestOwedRewakeStartsOneRootSpanPerWake(t *testing.T) {
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

	c, _, _, reads := newTestConsumer(t)
	w := spanWaker{woke: make(chan trace.SpanContext, 1)}
	c.SetAgentWaker(w)
	reads.seedOwedMention("agent-a", "chan-1", textMessage("owed-1", "human-1", "@a paged"))

	c.OnRunnerReady()
	ctx, stray := tp.Tracer("test").Start(t.Context(), "stray")
	c.drainOwedRewake(ctx)
	stray.End()
	woke := <-w.woke

	if !woke.IsValid() {
		t.Fatal("wake ran with no span, want a delivery.rewake root")
	}
	if woke.TraceID() == stray.SpanContext().TraceID() {
		t.Fatal("wake joined the caller's trace, want its own root")
	}
	// Started is recorded before the wake runs, so no wait on the span's end.
	for _, s := range rec.Started() {
		if s.SpanContext().SpanID() == woke.SpanID() {
			if s.Name() != "delivery.rewake" || s.Parent().IsValid() {
				t.Fatalf("wake span = %q parent %s, want a delivery.rewake root", s.Name(), s.Parent().SpanID())
			}
			return
		}
	}
	t.Fatal("wake span was not started by the package tracer")
}
