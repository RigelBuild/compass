package otel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func installHTTPTransportRecorder(t *testing.T) (*tracetest.SpanRecorder, trace.Tracer) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		if err := provider.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("tracer provider shutdown: %v", err)
		}
	})
	return recorder, provider.Tracer("test")
}

func TestHTTPTransportRecordsRedactedSpan(t *testing.T) {
	recorder, tracer := installHTTPTransportRecorder(t)
	var traceparent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceparent = r.Header.Get("Traceparent")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	ctx, parent := tracer.Start(t.Context(), "parent")
	defer parent.End()
	target := strings.Replace(server.URL, "://", "://user:pw@", 1) + "/v?token=s3cret&page=2"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := (&http.Client{Transport: HTTPTransport(nil)}).Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("closing response body: %v", err)
	}
	if traceparent != "" {
		t.Errorf("traceparent header = %q, want empty", traceparent)
	}

	var spans []sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		if span.SpanKind() == trace.SpanKindClient {
			spans = append(spans, span)
		}
	}
	if len(spans) != 1 {
		t.Fatalf("client spans = %d, want 1", len(spans))
	}
	span := spans[0]
	if span.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Errorf("client parent = %s, want %s", span.Parent().SpanID(), parent.SpanContext().SpanID())
	}
	var gotURL string
	for _, attr := range span.Attributes() {
		if string(attr.Key) == "url.full" {
			gotURL = attr.Value.AsString()
		}
	}
	if gotURL == "" {
		t.Fatal("client span has no url.full attribute")
	}
	parsed, err := url.Parse(gotURL)
	if err != nil {
		t.Fatalf("parsing recorded url.full: %v", err)
	}
	if parsed.Query().Get("token") != "REDACTED" || parsed.Query().Get("page") != "REDACTED" {
		t.Errorf("url.full = %q, want query values redacted", gotURL)
	}
	if parsed.Path != "/v" {
		t.Errorf("url.full path = %q, want /v", parsed.Path)
	}
	if strings.Contains(gotURL, "s3cret") || strings.Contains(gotURL, "page=2") || strings.Contains(gotURL, "pw") {
		t.Errorf("url.full contains a query value or userinfo: %q", gotURL)
	}
}

func TestHTTPTransportWithoutProviderSendsNoTraceparent(t *testing.T) {
	resetGlobals(t)
	var traceparent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceparent = r.Header.Get("Traceparent")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	client := &http.Client{Transport: HTTPTransport(nil)}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("closing response body: %v", err)
	}
	if traceparent != "" {
		t.Errorf("traceparent header = %q, want empty", traceparent)
	}
}
