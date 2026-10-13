//go:build unix

package server

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestGitCredentialRefreshStartsOneRootSpan(t *testing.T) {
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

	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	broker := newGitCredentialTestBroker(nil, nil, &now, nil)
	ctx, stray := tp.Tracer("test").Start(t.Context(), "stray")
	broker.refreshDue(ctx)
	stray.End()

	var got []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == "server.gitcredential.refresh" {
			got = append(got, s)
		}
	}
	if len(got) != 1 {
		t.Fatalf("refresh spans = %d, want 1", len(got))
	}
	if got[0].Parent().IsValid() {
		t.Fatalf("refresh parent = %s, want a root", got[0].Parent().SpanID())
	}
}
