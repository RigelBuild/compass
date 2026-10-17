package otel

import (
	"context"
	"slices"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// HandlerInterceptors is the one interceptor for a server mount: ic first (it
// must be non-nil), then NewTraceResponseInterceptor, then rest in order.
func HandlerInterceptors(ic connect.ServerInterceptor, rest ...connect.ServerInterceptor) connect.ServerInterceptor {
	interceptors := make([]connect.ServerInterceptor, 0, len(rest)+2)
	interceptors = append(interceptors, ic, NewTraceResponseInterceptor())
	interceptors = append(interceptors, rest...)
	return func(next connect.ServerFunc) connect.ServerFunc {
		for _, wrap := range slices.Backward(interceptors) {
			next = wrap(next)
		}
		return next
	}
}

// traceResponseHeader is the W3C trace-context response header (still a draft;
// DECIDED by compass-obs to use the standard name + "00-…" grammar anyway).
const traceResponseHeader = "traceresponse"

// NewTraceResponseInterceptor sets traceresponse on a successful unary response
// immediately before its first Send, when the handler span has a valid context.
func NewTraceResponseInterceptor() connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			if spec.StreamType != connect.StreamTypeUnary {
				return next(ctx, spec, stream)
			}
			info, ok := connect.CallInfoForServerContext(ctx)
			sc := trace.SpanContextFromContext(ctx)
			if !ok || !sc.IsValid() {
				return next(ctx, spec, stream)
			}
			buffered := &unaryResponseBuffer{stream: stream}
			if err := next(ctx, spec, buffered); err != nil {
				return err
			}
			if !buffered.sent && !buffered.headers {
				return nil
			}
			info.ResponseHeader().Set(traceResponseHeader, formatTraceResponse(sc))
			if buffered.headers {
				if err := stream.SendHeaders(); err != nil {
					return err
				}
			}
			if buffered.sent {
				return stream.Send(buffered.response)
			}
			return nil
		}
	}
}

type unaryResponseBuffer struct {
	stream   connect.ServerStream
	response any
	sent     bool
	headers  bool
}

func (s *unaryResponseBuffer) Receive(msg any) error {
	return s.stream.Receive(msg)
}

func (s *unaryResponseBuffer) SendHeaders() error {
	s.headers = true
	return nil
}

func (s *unaryResponseBuffer) Send(msg any) error {
	s.response = msg
	s.sent = true
	return nil
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
// X-POSTHOG-SESSION-ID header onto the handler span as semconv `session.id`.
func NewSessionIDInterceptor() connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			if info, ok := connect.CallInfoForServerContext(ctx); ok {
				if id := sessionIDFromHeader(info.RequestHeader()); id != "" {
					trace.SpanFromContext(ctx).SetAttributes(semconv.SessionID(id))
				}
			}
			return next(ctx, spec, stream)
		}
	}
}

// sessionIDFromHeader extracts a usable session id from the request, or "" when
// the header is absent, blank, or not worth trusting. Returning "" rather than
// repairing a bad value is deliberate: a truncated or re-encoded id joins to
// nothing in PostHog, so it would add span weight while still failing the pivot.
//
// The UTF-8 check is load-bearing: OTLP span attributes are proto3 strings, and
// one invalid value would drop every span batched with it.
func sessionIDFromHeader(h *connect.Header) string {
	id := strings.TrimSpace(h.Get(PostHogSessionHeader))
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
