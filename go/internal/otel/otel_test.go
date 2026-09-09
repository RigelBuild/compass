package otel

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// passthrough is the inner handler for interceptor tests: it returns an empty
// response and no error, so the assertion is about the interceptor alone.
func passthrough(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
	return connect.NewResponse(&struct{}{}), nil
}

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

// TestSessionIDInterceptor covers the inbound J1 half: the PostHog session id
// header must land on the handler span as semconv session.id.
//
// The assertions read the RECORDED span out of a SpanRecorder rather than
// trusting the call: a span attribute is only useful if it reaches the exporter,
// and stamping a non-recording span silently drops it.
func TestSessionIDInterceptor(t *testing.T) {
	interceptor := NewSessionIDInterceptor()

	// stampedSessionIDs runs one request through the interceptor against a real
	// recording provider and returns the session.id values on the exported span.
	stampedSessionIDs := func(t *testing.T, header string, set bool) []string {
		t.Helper()
		rec := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
		ctx, span := tp.Tracer("test").Start(context.Background(), "rpc")

		req := connect.NewRequest(&struct{}{})
		if set {
			req.Header().Set("X-POSTHOG-SESSION-ID", header)
		}
		if _, err := interceptor(passthrough)(ctx, req); err != nil {
			t.Fatalf("interceptor: %v", err)
		}
		span.End()

		ended := rec.Ended()
		if len(ended) != 1 {
			t.Fatalf("recorded %d spans, want 1", len(ended))
		}
		var got []string
		for _, attr := range ended[0].Attributes() {
			if attr.Key == "session.id" {
				got = append(got, attr.Value.AsString())
			}
		}
		return got
	}

	t.Run("a session id header lands on the span as session.id", func(t *testing.T) {
		got := stampedSessionIDs(t, "0198f2c1-7b3a-7000-8b1e-2f9d4c5a6e70", true)
		if len(got) != 1 || got[0] != "0198f2c1-7b3a-7000-8b1e-2f9d4c5a6e70" {
			t.Fatalf("session.id = %v, want the header value once", got)
		}
	})

	t.Run("no header stamps nothing", func(t *testing.T) {
		// The analytics-off path: the UI sends no header, and the span must not
		// carry an empty session.id that would join to nothing.
		if got := stampedSessionIDs(t, "", false); len(got) != 0 {
			t.Fatalf("session.id = %v, want none", got)
		}
	})

	t.Run("a blank header stamps nothing", func(t *testing.T) {
		if got := stampedSessionIDs(t, "   ", true); len(got) != 0 {
			t.Fatalf("session.id = %v, want none for a whitespace-only header", got)
		}
	})

	t.Run("surrounding whitespace is trimmed", func(t *testing.T) {
		got := stampedSessionIDs(t, "  sess-42\t", true)
		if len(got) != 1 || got[0] != "sess-42" {
			t.Fatalf("session.id = %v, want the trimmed value", got)
		}
	})

	t.Run("an oversized header is refused, not truncated", func(t *testing.T) {
		// The bound caps a single attribute's size and refuses payload-shaped
		// values; per-request volume is governed by the door's own limits. A
		// truncated id joins to nothing in PostHog, so refusing beats
		// half-stamping.
		got := stampedSessionIDs(t, strings.Repeat("a", maxSessionIDLen+1), true)
		if len(got) != 0 {
			t.Fatalf("session.id = %v, want none for an oversized header", got)
		}
		// The boundary itself is admitted, so the cap is off-by-none.
		atCap := stampedSessionIDs(t, strings.Repeat("a", maxSessionIDLen), true)
		if len(atCap) != 1 {
			t.Fatalf("session.id = %v, want the value at exactly the cap", atCap)
		}
	})

	t.Run("an invalid-UTF8 header is refused, so it cannot poison the export batch", func(t *testing.T) {
		// Go's HTTP parser rejects control bytes but admits high bytes, and OTLP
		// attributes are proto3 strings: the marshaller fails the WHOLE
		// ExportTraceServiceRequest on an invalid one, dropping every span
		// batched alongside it. This interceptor runs ahead of auth, so
		// admitting it would be an unauthenticated way to blind the trace
		// backend for everyone.
		for _, bad := range []string{"sess-\xff\xfe-1", "sess-\xe9-1", "\xc3"} {
			if got := stampedSessionIDs(t, bad, true); len(got) != 0 {
				t.Fatalf("session.id = %q for invalid-UTF8 header %q, want none", got, bad)
			}
		}
		// A multi-byte but VALID id is still admitted, so the check rejects bad
		// encoding rather than everything non-ASCII.
		if got := stampedSessionIDs(t, "sess-é-1", true); len(got) != 1 {
			t.Fatalf("session.id = %v, want a valid multi-byte id admitted", got)
		}
	})

	t.Run("no active span is a no-op, not a panic", func(t *testing.T) {
		// The provider-absent path: SpanFromContext returns a non-recording
		// span, and stamping it must be harmless.
		req := connect.NewRequest(&struct{}{})
		req.Header().Set("X-POSTHOG-SESSION-ID", "sess-noop")
		if _, err := interceptor(passthrough)(context.Background(), req); err != nil {
			t.Fatalf("interceptor with no span: %v", err)
		}
	})
}

// TestSessionIDInterceptorStampOrdering pins WHEN the stamp happens, which the
// value-handling subtests above cannot see: they read the ended span, and both
// a before-handler and an after-handler stamp leave the attribute there.
func TestSessionIDInterceptorStampOrdering(t *testing.T) {
	interceptor := NewSessionIDInterceptor()

	t.Run("stamps before the handler runs, so an erroring RPC keeps the key", func(t *testing.T) {
		// The traces most worth pivoting to from a product funnel are the
		// failures, so the key must not depend on the handler succeeding.
		rec := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
		ctx, span := tp.Tracer("test").Start(context.Background(), "rpc")

		req := connect.NewRequest(&struct{}{})
		req.Header().Set("X-POSTHOG-SESSION-ID", "sess-err")
		failing := func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
			return nil, errSentinel
		}
		if _, err := interceptor(failing)(ctx, req); err != errSentinel {
			t.Fatalf("interceptor swallowed the handler error: %v", err)
		}
		span.End()

		var got []string
		for _, attr := range rec.Ended()[0].Attributes() {
			if attr.Key == "session.id" {
				got = append(got, attr.Value.AsString())
			}
		}
		if len(got) != 1 || got[0] != "sess-err" {
			t.Fatalf("session.id = %v on a failed RPC, want it stamped anyway", got)
		}
	})

	t.Run("the key is already on the span WHILE the handler runs", func(t *testing.T) {
		// The subtest above proves an erroring RPC keeps the key, but it cannot
		// tell "stamped before next" from "stamped unconditionally after next" —
		// both leave the attribute on the ended span. This one pins the ordering
		// itself by reading the LIVE span from inside the handler, which is the
		// property the interceptor's doc comment actually claims. It matters for
		// any handler, sampler, or span processor that reads session.id while the
		// span is open: under a post-handler stamp they would all see nothing.
		rec := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
		ctx, span := tp.Tracer("test").Start(context.Background(), "rpc")

		req := connect.NewRequest(&struct{}{})
		req.Header().Set("X-POSTHOG-SESSION-ID", "sess-live")

		var seen []string
		observing := func(hctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			live := trace.SpanFromContext(hctx)
			if !live.IsRecording() {
				t.Fatal("handler span is not recording; the observation would be vacuous")
			}
			ro, ok := live.(sdktrace.ReadOnlySpan)
			if !ok {
				t.Fatal("handler span is not a ReadOnlySpan; cannot observe live attributes")
			}
			for _, attr := range ro.Attributes() {
				if attr.Key == "session.id" {
					seen = append(seen, attr.Value.AsString())
				}
			}
			return connect.NewResponse(&struct{}{}), nil
		}
		if _, err := interceptor(observing)(ctx, req); err != nil {
			t.Fatalf("interceptor: %v", err)
		}
		span.End()

		if len(seen) != 1 || seen[0] != "sess-live" {
			t.Fatalf("session.id visible inside the handler = %v, want [sess-live]: the stamp must happen BEFORE next, not after", seen)
		}
	})
}

var errSentinel = &sentinelError{}

type sentinelError struct{}

func (*sentinelError) Error() string { return "boom" }
