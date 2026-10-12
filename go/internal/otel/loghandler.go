package otel

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// TraceHandler adds the active span's ids to log records.
type TraceHandler struct {
	inner slog.Handler
}

// NewTraceHandler wraps inner with trace-id stamping.
func NewTraceHandler(inner slog.Handler) *TraceHandler {
	return &TraceHandler{inner: inner}
}

// Enabled defers to the wrapped handler.
func (h *TraceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle adds the ids when ctx carries a valid span, then delegates.
func (h *TraceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r = r.Clone()
		r.AddAttrs(
			//nolint:sloglint // TraceHandler is the one writer of the reserved trace keys
			slog.String("trace_id", sc.TraceID().String()),
			//nolint:sloglint // TraceHandler is the one writer of the reserved trace keys
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs keeps the wrapper around the derived handler.
func (h *TraceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TraceHandler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup keeps the wrapper around the derived handler.
func (h *TraceHandler) WithGroup(name string) slog.Handler {
	return &TraceHandler{inner: h.inner.WithGroup(name)}
}
