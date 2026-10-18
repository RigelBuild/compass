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

// PostHogSessionHeader is the frontend/backend correlation header, not W3C
// trace context. CORS must allow it so browser requests reach this interceptor.
const PostHogSessionHeader = "X-POSTHOG-SESSION-ID"

// maxSessionIDLen bounds one attacker-controlled span attribute. Session IDs
// are UUID-shaped; the limit allows format changes without payload-sized values.
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

// sessionIDFromHeader rejects invalid values rather than repairing them: a
// transformed ID would not join in PostHog. Invalid UTF-8 would drop its OTLP batch.
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
