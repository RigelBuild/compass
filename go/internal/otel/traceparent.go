package otel

import (
	"context"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// spanContextIsValid reports whether ctx carries a valid (parseable,
// non-zero-id) remote span context — the signal that a traceparent parsed.
func spanContextIsValid(ctx context.Context) bool {
	return trace.SpanContextFromContext(ctx).IsValid()
}

// traceparentKey is the single W3C trace-context carrier key.
const traceparentKey = "traceparent"

// tcPropagator is the W3C TraceContext propagator backing Traceparent and
// ContextWithTraceparent. It is the same propagator installed globally in the
// enabled Setup* path, so serialization is identical in and out of process.
var tcPropagator = propagation.TraceContext{}

// Traceparent serializes ctx's active span context to the W3C string
// ("00-<32hex>-<16hex>-<2hex>"), or "" when there is no valid active span.
func Traceparent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	tcPropagator.Inject(ctx, carrier)
	return carrier.Get(traceparentKey)
}

// ContextWithTraceparent returns ctx carrying the remote span context parsed
// from tp; malformed or empty tp returns ctx unchanged.
func ContextWithTraceparent(ctx context.Context, tp string) context.Context {
	if tp == "" {
		return ctx
	}
	out := tcPropagator.Extract(ctx, propagation.MapCarrier{traceparentKey: tp})
	// A malformed traceparent yields no valid remote span context, so the
	// extracted context carries no span — indistinguishable from the input for
	// callers, but returning ctx keeps the "unchanged" contract explicit.
	if !spanContextIsValid(out) {
		return ctx
	}
	return out
}
