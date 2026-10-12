//go:build unix

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestTracedIngressLinksPublicTraceparent(t *testing.T) {
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})
	const remoteTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	handler := tracedIngress(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), "public-hook")
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/hook", nil)
	request.Header.Set("Traceparent", remoteTraceparent)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("recorded spans = %d, want one: %+v", len(spans), spans)
	}
	span := spans[0]
	if span.SpanKind != trace.SpanKindServer {
		t.Fatalf("span kind = %v, want server", span.SpanKind)
	}
	if span.Parent.SpanID().IsValid() {
		t.Fatalf("remote traceparent became parent %s, want only a link", span.Parent.SpanID())
	}
	if len(span.Links) != 1 || span.Links[0].SpanContext.TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("span links = %+v, want one link to the remote trace", span.Links)
	}
}
