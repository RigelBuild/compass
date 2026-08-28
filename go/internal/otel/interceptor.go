package otel

import (
	"context"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/trace"
)

// traceResponseHeader is the W3C trace-context response header (still a draft;
// DECIDED by compass-obs to use the standard name + "00-…" grammar anyway).
const traceResponseHeader = "traceresponse"

// NewTraceResponseInterceptor sets the "traceresponse" response header from the
// handler span's context on every unary response (the UI/PostHog trace_id
// source). It is a no-op when there is no active span (no provider installed,
// or an unsampled/absent span context).
func NewTraceResponseInterceptor() connect.UnaryInterceptorFunc {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			resp, err := next(ctx, req)
			if err != nil {
				return resp, err
			}
			sc := trace.SpanContextFromContext(ctx)
			if resp != nil && sc.IsValid() {
				resp.Header().Set(traceResponseHeader, formatTraceResponse(sc))
			}
			return resp, err
		})
	})
}

// formatTraceResponse renders a span context as the W3C "00-…" grammar
// ("00-<32hex traceid>-<16hex spanid>-<2hex flags>").
func formatTraceResponse(sc trace.SpanContext) string {
	return "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-" + sc.TraceFlags().String()
}
