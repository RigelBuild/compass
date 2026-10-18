package otel

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// fakeServerStream records unary responses and runs a hook before each send.
type fakeServerStream struct {
	response []any
	onSend   func() error
}

func (s *fakeServerStream) Receive(any) error { return nil }

func (s *fakeServerStream) SendHeaders() error { return nil }

func (s *fakeServerStream) Send(msg any) error {
	if s.onSend != nil {
		if err := s.onSend(); err != nil {
			return err
		}
	}
	s.response = append(s.response, msg)
	return nil
}

func serverCallInfo(header string, set bool) *connect.CallInfo {
	info := &connect.CallInfo{}
	if set {
		info.RequestHeader().Set(PostHogSessionHeader, header)
	}
	return info
}
func runServerInterceptor(
	ctx context.Context,
	interceptor connect.ServerInterceptor,
	spec connect.Spec,
	info *connect.CallInfo,
	stream connect.ServerStream,
	next connect.ServerFunc,
) error {
	server := connect.NewServer(interceptor)
	server.Register(connect.Method{Spec: spec, Handler: next})
	return server.Call(ctx, spec.Procedure, info, stream)
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

// TestTraceResponseInterceptor asserts the response header is set before the
// unary adapter's first Send, omitted without a span or on handler failure.
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
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	})
	spec := connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: "/test.Service/Call"}
	want := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

	t.Run("valid span sets header before send", func(t *testing.T) {
		ctx := trace.ContextWithSpanContext(t.Context(), validSC)
		info := &connect.CallInfo{}
		stream := &fakeServerStream{onSend: func() error {
			if got := info.ResponseHeader().Get(traceResponseHeader); got != want {
				t.Fatalf("header at first send = %q, want %q", got, want)
			}
			return nil
		}}
		next := func(_ context.Context, _ connect.Spec, stream connect.ServerStream) error {
			return stream.Send(&struct{}{})
		}
		if err := runServerInterceptor(ctx, interceptor, spec, info, stream, next); err != nil {
			t.Fatalf("interceptor err = %v, want nil", err)
		}
		if got := info.ResponseHeader().Get(traceResponseHeader); got != want {
			t.Errorf("%s = %q, want %q", traceResponseHeader, got, want)
		}
	})

	t.Run("no span sets no header", func(t *testing.T) {
		info := &connect.CallInfo{}
		stream := &fakeServerStream{}
		next := func(_ context.Context, _ connect.Spec, stream connect.ServerStream) error {
			return stream.Send(&struct{}{})
		}
		if err := runServerInterceptor(t.Context(), interceptor, spec, info, stream, next); err != nil {
			t.Fatalf("interceptor err = %v, want nil", err)
		}
		if got := info.ResponseHeader().Get(traceResponseHeader); got != "" {
			t.Errorf("%s = %q, want empty", traceResponseHeader, got)
		}
	})

	t.Run("handler error has no trace response header", func(t *testing.T) {
		ctx := trace.ContextWithSpanContext(t.Context(), validSC)
		info := &connect.CallInfo{}
		wantErr := connect.NewError(connect.CodeInternal, errSentinel.Error()).WithCause(errSentinel)
		next := func(context.Context, connect.Spec, connect.ServerStream) error {
			return wantErr
		}
		if got := runServerInterceptor(ctx, interceptor, spec, info, &fakeServerStream{}, next); got != wantErr {
			t.Errorf("interceptor err = %v, want the handler's error unchanged", got)
		}
		if got := info.ResponseHeader().Get(traceResponseHeader); got != "" {
			t.Errorf("error response %s = %q, want empty", traceResponseHeader, got)
		}
	})

	t.Run("send followed by handler error has no trace response header", func(t *testing.T) {
		ctx := trace.ContextWithSpanContext(t.Context(), validSC)
		info := &connect.CallInfo{}
		sendErr := errors.New("failed to send")
		stream := &fakeServerStream{}
		next := func(_ context.Context, _ connect.Spec, stream connect.ServerStream) error {
			if err := stream.Send(&struct{}{}); err != nil {
				return err
			}
			return sendErr
		}
		if got := runServerInterceptor(ctx, interceptor, spec, info, stream, next); got != sendErr {
			t.Errorf("interceptor err = %v, want handler error", got)
		}
		if got := info.ResponseHeader().Get(traceResponseHeader); got != "" {
			t.Errorf("error response %s = %q, want empty", traceResponseHeader, got)
		}
		if len(stream.response) != 0 {
			t.Errorf("sent %d responses before handler succeeded, want none", len(stream.response))
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
		ctx, span := tp.Tracer("test").Start(t.Context(), "rpc")
		info := serverCallInfo(header, set)
		spec := connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: "/test.Service/Call"}
		if err := runServerInterceptor(ctx, interceptor, spec, info, &fakeServerStream{}, func(context.Context, connect.Spec, connect.ServerStream) error {
			return nil
		}); err != nil {
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
		// An invalid proto3 string makes OTLP reject the whole batch. The header
		// reaches this interceptor before auth, so accepting it could blind traces.
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
		info := serverCallInfo("sess-noop", true)
		spec := connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: "/test.Service/Call"}
		if err := runServerInterceptor(context.Background(), interceptor, spec, info, &fakeServerStream{}, func(context.Context, connect.Spec, connect.ServerStream) error {
			return nil
		}); err != nil {
			t.Fatalf("interceptor with no span: %v", err)
		}
	})
}

// TestSessionIDInterceptorStampOrdering pins that session.id is attached before
// the handler runs, including handlers that return errors.
func TestSessionIDInterceptorStampOrdering(t *testing.T) {
	interceptor := NewSessionIDInterceptor()
	spec := connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: "/test.Service/Call"}

	t.Run("stamps before an erroring handler", func(t *testing.T) {
		rec := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
		ctx, span := tp.Tracer("test").Start(t.Context(), "rpc")
		info := serverCallInfo("sess-err", true)
		failing := func(context.Context, connect.Spec, connect.ServerStream) error { return errSentinel }
		if err := runServerInterceptor(ctx, interceptor, spec, info, &fakeServerStream{}, failing); err != errSentinel {
			t.Fatalf("interceptor err = %v, want handler error", err)
		}
		span.End()
		var got []string
		for _, attr := range rec.Ended()[0].Attributes() {
			if attr.Key == "session.id" {
				got = append(got, attr.Value.AsString())
			}
		}
		if len(got) != 1 || got[0] != "sess-err" {
			t.Fatalf("session.id = %v on a failed RPC, want [sess-err]", got)
		}
	})

	t.Run("the key is visible while the handler runs", func(t *testing.T) {
		rec := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
		ctx, span := tp.Tracer("test").Start(t.Context(), "rpc")
		info := serverCallInfo("sess-live", true)
		var seen []string
		observing := func(hctx context.Context, _ connect.Spec, _ connect.ServerStream) error {
			live := trace.SpanFromContext(hctx)
			if !live.IsRecording() {
				t.Fatal("handler span is not recording; observation would be vacuous")
			}
			ro, ok := live.(sdktrace.ReadOnlySpan)
			if !ok {
				t.Fatal("handler span is not a ReadOnlySpan")
			}
			for _, attr := range ro.Attributes() {
				if attr.Key == "session.id" {
					seen = append(seen, attr.Value.AsString())
				}
			}
			return nil
		}
		if err := runServerInterceptor(ctx, interceptor, spec, info, &fakeServerStream{}, observing); err != nil {
			t.Fatalf("interceptor: %v", err)
		}
		span.End()
		if len(seen) != 1 || seen[0] != "sess-live" {
			t.Fatalf("session.id visible inside handler = %v, want [sess-live]", seen)
		}
	})
}

var errSentinel = &sentinelError{}

type sentinelError struct{}

func (*sentinelError) Error() string { return "boom" }
