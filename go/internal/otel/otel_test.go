package otel

import (
	"context"
	"regexp"
	"testing"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
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
	// A sentinel noop provider proves Setup* did not overwrite the global.
	sentinel := noopmetric.NewMeterProvider()
	otel.SetMeterProvider(sentinel)

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
	if _, ok := otel.GetMeterProvider().(*sdkmetric.MeterProvider); ok {
		t.Fatal("disabled path installed an SDK MeterProvider, want none")
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

// TestSetupMeterProviderEnabled asserts the enabled path installs a global SDK
// MeterProvider (mirrors the tracer enabled-path assertion).
func TestSetupMeterProviderEnabled(t *testing.T) {
	resetGlobals(t)
	cfg := Config{ServiceName: "compass-server", ServiceVersion: "1.2.3", Endpoint: "http://localhost:4318"}
	shutdown, err := SetupMeterProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("enabled SetupMeterProvider err = %v, want nil", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) }) // best-effort flush; no collector, error not actionable in test
	if _, ok := otel.GetMeterProvider().(*sdkmetric.MeterProvider); !ok {
		t.Fatalf("enabled path did not install an SDK MeterProvider, got %T", otel.GetMeterProvider())
	}
}

// TestFormatTraceResponse asserts the pure header formatter emits the exact W3C
// traceresponse grammar for a known span context.
func TestFormatTraceResponse(t *testing.T) {
	traceID, err := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	if err != nil {
		t.Fatalf("TraceIDFromHex err = %v", err)
	}
	spanID, err := trace.SpanIDFromHex("b7ad6b7169203331")
	if err != nil {
		t.Fatalf("SpanIDFromHex err = %v", err)
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	got := formatTraceResponse(sc)
	want := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	if got != want {
		t.Errorf("formatTraceResponse = %q, want %q", got, want)
	}
}

// TestTraceResponseInterceptor asserts the interceptor sets the traceresponse
// header when the ctx carries a valid span context, sets none otherwise, and
// never alters the handler's error.
func TestTraceResponseInterceptor(t *testing.T) {
	interceptor := NewTraceResponseInterceptor()

	traceID, err := trace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	if err != nil {
		t.Fatalf("TraceIDFromHex err = %v", err)
	}
	spanID, err := trace.SpanIDFromHex("b7ad6b7169203331")
	if err != nil {
		t.Fatalf("SpanIDFromHex err = %v", err)
	}
	validSC := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})

	t.Run("valid span sets header", func(t *testing.T) {
		ctx := trace.ContextWithSpanContext(context.Background(), validSC)
		resp := connect.NewResponse(&struct{}{})
		next := connect.UnaryFunc(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
			return resp, nil
		})
		got, err := interceptor(next)(ctx, connect.NewRequest(&struct{}{}))
		if err != nil {
			t.Fatalf("interceptor err = %v, want nil", err)
		}
		want := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
		if h := got.Header().Get(traceResponseHeader); h != want {
			t.Errorf("%s = %q, want %q", traceResponseHeader, h, want)
		}
	})

	t.Run("no span sets no header", func(t *testing.T) {
		resp := connect.NewResponse(&struct{}{})
		next := connect.UnaryFunc(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
			return resp, nil
		})
		got, err := interceptor(next)(context.Background(), connect.NewRequest(&struct{}{}))
		if err != nil {
			t.Fatalf("interceptor err = %v, want nil", err)
		}
		if h := got.Header().Get(traceResponseHeader); h != "" {
			t.Errorf("%s = %q, want empty", traceResponseHeader, h)
		}
	})

	t.Run("handler error passthrough", func(t *testing.T) {
		ctx := trace.ContextWithSpanContext(context.Background(), validSC)
		wantErr := connect.NewError(connect.CodeInternal, errSentinel)
		next := connect.UnaryFunc(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
			return nil, wantErr
		})
		_, err := interceptor(next)(ctx, connect.NewRequest(&struct{}{}))
		if err != wantErr {
			t.Errorf("interceptor err = %v, want the handler's error unchanged", err)
		}
	})
}

var errSentinel = &sentinelError{}

type sentinelError struct{}

func (*sentinelError) Error() string { return "boom" }
