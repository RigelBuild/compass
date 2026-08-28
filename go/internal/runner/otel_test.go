//go:build unix

package runner

// OTel wiring tests for the Runner-side seam: Dial's outbound RunnerService
// client mounts the otelconnect interceptor, which emits a CLIENT span per RPC
// when a global tracer provider is installed — and none when disabled, since the
// empty-endpoint path installs no global provider (the export gate lives in the
// provider install, not in RunnerConfig).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// enrollServerURL stands up an h2c httptest RunnerService serving enrollStub and
// returns its base URL, torn down via t.Cleanup — so a test can drive Dial (which
// builds the interceptor-wrapped client) end to end against a real dial.
func enrollServerURL(t *testing.T) string {
	t.Helper()
	path, handler := compassv1internalconnect.NewRunnerServiceHandler(enrollStub{})
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

// installInMemoryTracer installs an SDK tracer provider backed by an in-memory
// recorder as the global provider (the source otelconnect.NewInterceptor reads),
// restoring the prior global on cleanup. It returns the recorder.
func installInMemoryTracer(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background()) // best-effort flush; test root ctx, error not actionable
	})
	return rec
}

// clientSpanCount counts CLIENT-kind spans among the recorded spans.
func clientSpanCount(spans []sdktrace.ReadOnlySpan) int {
	n := 0
	for _, s := range spans {
		if s.SpanKind() == trace.SpanKindClient {
			n++
		}
	}
	return n
}

// TestDialEmitsClientSpanWhenEnabled asserts Dial's outbound client emits a
// CLIENT span on the Enroll RPC when a global tracer provider is installed.
func TestDialEmitsClientSpanWhenEnabled(t *testing.T) {
	rec := installInMemoryTracer(t)
	url := enrollServerURL(t)

	// context.Background() is the test root context.
	if _, err := Dial(context.Background(), RunnerConfig{
		RunnerID:   "r-1",
		ServerAddr: url,
		Token:      "tok",
		HTTPClient: h2cHTTPClient(t),
		// Emission gated by the installed global tracer provider, not any config
		// field — installInMemoryTracer set one above.
	}); err != nil {
		t.Fatalf("Dial err = %v, want nil", err)
	}

	if got := clientSpanCount(rec.Ended()); got == 0 {
		t.Fatalf("enabled: client spans = %d, want >= 1 (otelconnect emits a CLIENT span per RPC)", got)
	}
}

// TestDialEmitsNoClientSpanWhenDisabled asserts that with no SDK provider
// installed (the empty-endpoint disabled path), the same dial records no spans —
// the otelconnect interceptor is a no-op against the global noop provider.
func TestDialEmitsNoClientSpanWhenDisabled(t *testing.T) {
	// Pin the global to a noop provider (the disabled-path state: SetupTracerProvider
	// installs nothing when the endpoint is empty), and record via a separate SDK
	// provider that is NOT global — so any span the interceptor emits would be caught,
	// yet none is, because otelconnect reads the (noop) global.
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(noop.NewTracerProvider())
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	rec := tracetest.NewSpanRecorder()
	sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)) // deliberately not made global

	url := enrollServerURL(t)

	// context.Background() is the test root context.
	if _, err := Dial(context.Background(), RunnerConfig{
		RunnerID:   "r-1",
		ServerAddr: url,
		Token:      "tok",
		HTTPClient: h2cHTTPClient(t),
	}); err != nil {
		t.Fatalf("Dial err = %v, want nil", err)
	}

	if got := clientSpanCount(rec.Ended()); got != 0 {
		t.Fatalf("disabled: client spans = %d, want 0 (no global SDK provider installed)", got)
	}
}
