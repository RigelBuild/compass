package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// logLine writes one record through a TraceHandler over JSON and decodes it.
func logLine(t *testing.T, ctx context.Context, wrap func(slog.Handler) slog.Handler) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	log := slog.New(wrap(slog.NewJSONHandler(&buf, nil)))
	log.InfoContext(ctx, "probe", "k", "v")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}
	return rec
}

func sdkSpan(t *testing.T, sampler sdktrace.Sampler) (context.Context, trace.SpanContext) {
	t.Helper()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sampler))
	t.Cleanup(func() {
		if err := tp.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	ctx, span := tp.Tracer("test").Start(t.Context(), "op")
	t.Cleanup(func() { span.End() })
	return ctx, span.SpanContext()
}

func providerOff(t *testing.T) {
	t.Helper()
	resetGlobals(t)
	otel.SetTracerProvider(noop.NewTracerProvider())
}

type traceHandlerCase struct {
	name        string
	setup       func(*testing.T) (context.Context, trace.SpanContext)
	wrap        func(slog.Handler) slog.Handler
	group       string
	wantService bool
	wantTraceID string
	wantSpanID  string
}

func TestTraceHandlerStampsActiveSpan(t *testing.T) {
	const remoteTraceID = "0123456789abcdef0123456789abcdef"
	const remoteSpanID = "0123456789abcdef"

	tests := []traceHandlerCase{
		{
			name: "sampled SDK span",
			setup: func(t *testing.T) (context.Context, trace.SpanContext) {
				t.Helper()
				return sdkSpan(t, sdktrace.AlwaysSample())
			},
			wrap: func(h slog.Handler) slog.Handler { return NewTraceHandler(h) },
		},
		{
			name: "derived WithAttrs logger",
			setup: func(t *testing.T) (context.Context, trace.SpanContext) {
				t.Helper()
				return sdkSpan(t, sdktrace.AlwaysSample())
			},
			wrap: func(h slog.Handler) slog.Handler {
				return NewTraceHandler(h).WithAttrs([]slog.Attr{slog.String("svc", "x")})
			},
			wantService: true,
		},
		{
			name: "WithGroup logger",
			setup: func(t *testing.T) (context.Context, trace.SpanContext) {
				t.Helper()
				return sdkSpan(t, sdktrace.AlwaysSample())
			},
			wrap:  func(h slog.Handler) slog.Handler { return NewTraceHandler(h).WithGroup("request") },
			group: "request",
		},
		{
			name: "bare ctx",
			setup: func(t *testing.T) (context.Context, trace.SpanContext) {
				t.Helper()
				return t.Context(), trace.SpanContext{}
			},
			wrap: func(h slog.Handler) slog.Handler { return NewTraceHandler(h) },
		},
		{
			name: "provider off with no parent",
			setup: func(t *testing.T) (context.Context, trace.SpanContext) {
				t.Helper()
				providerOff(t)
				ctx, span := otel.Tracer("test").Start(t.Context(), "op")
				span.End()
				return ctx, trace.SpanContextFromContext(ctx)
			},
			wrap: func(h slog.Handler) slog.Handler { return NewTraceHandler(h) },
		},
		{
			name: "provider off with remote parent",
			setup: func(t *testing.T) (context.Context, trace.SpanContext) {
				t.Helper()
				providerOff(t)
				parent := ContextWithTraceparent(t.Context(), "00-"+remoteTraceID+"-"+remoteSpanID+"-01")
				ctx, span := otel.Tracer("test").Start(parent, "op")
				span.End()
				return ctx, trace.SpanContextFromContext(ctx)
			},
			wrap:        func(h slog.Handler) slog.Handler { return NewTraceHandler(h) },
			wantTraceID: remoteTraceID,
			wantSpanID:  remoteSpanID,
		},
		{
			name: "unsampled SDK span",
			setup: func(t *testing.T) (context.Context, trace.SpanContext) {
				t.Helper()
				return sdkSpan(t, sdktrace.NeverSample())
			},
			wrap: func(h slog.Handler) slog.Handler { return NewTraceHandler(h) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, sc := tt.setup(t)
			assertTraceRecord(t, logLine(t, ctx, tt.wrap), sc, tt)
		})
	}
}

func assertTraceRecord(t *testing.T, rec map[string]any, sc trace.SpanContext, tc traceHandlerCase) {
	t.Helper()
	if tc.group != "" {
		rec = assertGroupPlacement(t, rec, tc.group)
	}
	if rec["k"] != "v" {
		t.Errorf("record = %v, want its own k attribute", rec)
	}
	if tc.wantService && rec["svc"] != "x" {
		t.Errorf("record = %v, want derived svc attribute", rec)
	}
	assertTraceIDs(t, rec, sc, tc.wantTraceID, tc.wantSpanID)
}

func assertGroupPlacement(t *testing.T, rec map[string]any, group string) map[string]any {
	t.Helper()
	grouped, ok := rec[group].(map[string]any)
	if !ok {
		t.Fatalf("record = %v, want %s group", rec, group)
	}
	for _, key := range []string{"trace_id", "span_id"} {
		if _, ok := rec[key]; ok {
			t.Errorf("record = %v, want %s only inside group %s", rec, key, group)
		}
	}
	return grouped
}

func assertTraceIDs(t *testing.T, rec map[string]any, sc trace.SpanContext, wantTraceID, wantSpanID string) {
	t.Helper()
	if wantTraceID != "" {
		if rec["trace_id"] != wantTraceID || rec["span_id"] != wantSpanID {
			t.Errorf("record = %v, want remote trace_id %s and parent span_id %s", rec, wantTraceID, wantSpanID)
		}
		return
	}
	if sc.IsValid() {
		if rec["trace_id"] != sc.TraceID().String() || rec["span_id"] != sc.SpanID().String() {
			t.Errorf("record = %v, want trace_id %s and span_id %s", rec, sc.TraceID(), sc.SpanID())
		}
		return
	}
	for _, key := range []string{"trace_id", "span_id"} {
		if _, ok := rec[key]; ok {
			t.Errorf("record = %v, want no %s", rec, key)
		}
	}
}
