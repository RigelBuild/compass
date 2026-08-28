package otel

import (
	"context"
	"regexp"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// w3cTraceparent matches the W3C traceparent grammar: 00-<32hex>-<16hex>-<2hex>.
var w3cTraceparent = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)

// resetGlobals restores the global tracer/meter providers and propagator so an
// enabled-path test never leaks its installed globals into another test.
func resetGlobals(t *testing.T) {
	t.Helper()
	prevTP := otel.GetTracerProvider()
	prevMP := otel.GetMeterProvider()
	prevProp := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetMeterProvider(prevMP)
		otel.SetTextMapPropagator(prevProp)
	})
}

// TestSetupTracerProviderDisabled asserts the empty-endpoint path returns a
// non-nil no-op shutdown and installs NO global tracer provider.
func TestSetupTracerProviderDisabled(t *testing.T) {
	resetGlobals(t)
	// A sentinel noop provider proves Setup* did not overwrite the global.
	sentinel := noop.NewTracerProvider()
	otel.SetTracerProvider(sentinel)

	shutdown, err := SetupTracerProvider(context.Background(), Config{ServiceName: "compass-server"})
	if err != nil {
		t.Fatalf("disabled SetupTracerProvider err = %v, want nil", err)
	}
	if shutdown == nil {
		t.Fatal("disabled SetupTracerProvider returned nil shutdown, want non-nil no-op")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown err = %v, want nil", err)
	}
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); ok {
		t.Fatal("disabled path installed an SDK TracerProvider, want none")
	}
}

// TestSetupMeterProviderDisabled asserts the empty-endpoint path returns a
// non-nil no-op shutdown and installs NO global meter provider.
func TestSetupMeterProviderDisabled(t *testing.T) {
	resetGlobals(t)
	shutdown, err := SetupMeterProvider(context.Background(), Config{ServiceName: "compass-runner"})
	if err != nil {
		t.Fatalf("disabled SetupMeterProvider err = %v, want nil", err)
	}
	if shutdown == nil {
		t.Fatal("disabled SetupMeterProvider returned nil shutdown, want non-nil no-op")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown err = %v, want nil", err)
	}
}

// TestSetupTracerProviderEnabledResource asserts the enabled path installs a
// global SDK TracerProvider, a W3C propagator, and a resource carrying
// service.name/service.version.
func TestSetupTracerProviderEnabledResource(t *testing.T) {
	resetGlobals(t)
	cfg := Config{ServiceName: "compass-server", ServiceVersion: "1.2.3", Endpoint: "http://localhost:4318"}

	shutdown, err := SetupTracerProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("enabled SetupTracerProvider err = %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) }) // best-effort flush; no collector, error not actionable in test

	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Fatalf("enabled path did not install an SDK TracerProvider, got %T", otel.GetTracerProvider())
	}

	// The enabled path installs the global W3C TraceContext propagator.
	if fields := otel.GetTextMapPropagator().Fields(); len(fields) == 0 || fields[0] != "traceparent" {
		t.Errorf("global propagator fields = %v, want traceparent", fields)
	}
}

// TestNewResourceAttributes asserts the resource the enabled providers carry
// tags service.name/service.version from Config. newResource is the single
// resource builder both SetupTracerProvider and SetupMeterProvider use, so this
// locks the attribute contract at its source.
func TestNewResourceAttributes(t *testing.T) {
	res, err := newResource(context.Background(), Config{ServiceName: "compass-server", ServiceVersion: "1.2.3"})
	if err != nil {
		t.Fatalf("newResource err = %v", err)
	}
	attrs := map[string]string{}
	for _, kv := range res.Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	if attrs["service.name"] != "compass-server" {
		t.Errorf("resource service.name = %q, want compass-server", attrs["service.name"])
	}
	if attrs["service.version"] != "1.2.3" {
		t.Errorf("resource service.version = %q, want 1.2.3", attrs["service.version"])
	}
}

// TestTraceparentNoSpan asserts Traceparent returns "" when ctx has no span.
func TestTraceparentNoSpan(t *testing.T) {
	if got := Traceparent(context.Background()); got != "" {
		t.Errorf("Traceparent(no span) = %q, want empty", got)
	}
}

// TestTraceparentWithSpan asserts Traceparent returns a W3C-grammar string
// under a started span.
func TestTraceparentWithSpan(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	defer span.End()

	got := Traceparent(ctx)
	if !w3cTraceparent.MatchString(got) {
		t.Errorf("Traceparent(span) = %q, want W3C grammar 00-<32hex>-<16hex>-<2hex>", got)
	}
}

// TestContextWithTraceparentRoundTrip asserts a valid traceparent round-trips
// into a context carrying the matching remote span context.
func TestContextWithTraceparentRoundTrip(t *testing.T) {
	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	ctx := ContextWithTraceparent(context.Background(), tp)

	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		t.Fatal("round-tripped span context is invalid")
	}
	if got := Traceparent(ctx); got != tp {
		t.Errorf("round-trip Traceparent = %q, want %q", got, tp)
	}
}

// TestContextWithTraceparentMalformed asserts a malformed traceparent yields
// the input context unchanged (no span attached).
func TestContextWithTraceparentMalformed(t *testing.T) {
	base := context.Background()
	for _, bad := range []string{"", "not-a-traceparent", "00-xyz-abc-01", "00-00000000000000000000000000000000-0000000000000000-00"} {
		got := ContextWithTraceparent(base, bad)
		if sc := trace.SpanContextFromContext(got); sc.IsValid() {
			t.Errorf("ContextWithTraceparent(%q) attached a valid span context, want unchanged", bad)
		}
	}
}
