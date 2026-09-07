package otel

import (
	"context"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
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

// PostHogSessionHeader is PostHog's own frontend<->backend correlation header,
// the name posthog-js sends. It is NOT W3C trace-context: the two are parallel
// signals joined by a shared key, never merged data planes.
//
// Exported because the CORS builders must allow it as a REQUEST header. A
// browser preflight that omits it fails the whole request, so the interceptor
// below would be unreachable from the UI that is its only source.
const PostHogSessionHeader = "X-POSTHOG-SESSION-ID"

// maxSessionIDLen caps a SINGLE attribute's size and refuses payload-shaped
// values; per-request volume is governed by the door's own rate and size
// limits, not by this constant. The value is attacker-controlled (any client
// can set the header) and a span attribute flows to the trace backend, so an
// unbounded copy would let one request carry an arbitrarily large blob into
// Tempo. PostHog session ids are UUID-shaped; 200 leaves room for a format
// change without admitting a payload.
const maxSessionIDLen = 200

// NewSessionIDInterceptor stamps the PostHog session id from the request's
// X-POSTHOG-SESSION-ID header onto the handler span as semconv `session.id`
// (J1, the correlation-key join). It is the INBOUND half of the seam whose
// outbound half is NewTraceResponseInterceptor: product events carry the OTel
// trace id, backend spans carry the product session id, and either side can be
// pivoted to the other by the shared key — without either system holding the
// other's data, which is what keeps the join billing-safe.
//
// It never creates a span. With no provider installed the span is non-recording
// and SetAttributes is dropped, so the analytics-off path costs one map lookup
// and adds nothing to the wire.
func NewSessionIDInterceptor() connect.UnaryInterceptorFunc {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			// Stamped BEFORE the handler runs: a span the handler ends, or one
			// that errors, must still carry the key, or the failures — the
			// traces most worth pivoting to from a product funnel — would be
			// exactly the ones missing it.
			if id := sessionIDFromHeader(req); id != "" {
				trace.SpanFromContext(ctx).SetAttributes(semconv.SessionID(id))
			}
			return next(ctx, req)
		})
	})
}

// sessionIDFromHeader extracts a usable session id from the request, or "" when
// the header is absent, blank, or not worth trusting. Returning "" rather than
// repairing a bad value is deliberate in every case: a truncated or re-encoded
// id joins to nothing in PostHog, so it would add span weight while still
// failing the pivot, and a silent half-key is harder to notice than an absent
// one.
//
// The UTF-8 check is load-bearing, not hygiene. Go's HTTP parser rejects control
// bytes but ADMITS high bytes, OTLP span attributes are proto3 strings, and the
// protobuf marshaller fails the whole ExportTraceServiceRequest on an invalid
// one — so a single bad header would drop every span batched with it, including
// other callers'. Since this interceptor deliberately runs ahead of auth, that
// would be an unauthenticated observability denial-of-service, surfaced only in
// the exporter's own logs.
func sessionIDFromHeader(req connect.AnyRequest) string {
	id := strings.TrimSpace(req.Header().Get(PostHogSessionHeader))
	if id == "" || len(id) > maxSessionIDLen || !utf8.ValidString(id) {
		return ""
	}
	return id
}

// formatTraceResponse renders a span context as the W3C "00-…" grammar
// ("00-<32hex traceid>-<16hex spanid>-<2hex flags>").
func formatTraceResponse(sc trace.SpanContext) string {
	return "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-" + sc.TraceFlags().String()
}
