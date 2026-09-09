//go:build unix

package runnerhub

// Unit coverage for the cross-turn causal link (relay_comms.go linkTrigger).
// The e2e assertion in the server suite drives the happy path over a real door;
// what it cannot cheaply drive is the REJECTION half — that an empty or
// malformed trigger_traceparent yields no link at all. A malformed value is the
// sharp edge: the shipped parse helper returns its INPUT context unchanged on a
// parse failure, so parsing against the handler's own context would hand back
// the LOCAL span context and link the reply span to itself. These cases pin
// that down.

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// triggerTraceparent is a valid W3C traceparent standing in for the turn that
// triggered the agent's reply, and triggerTraceID is the trace id inside it.
const (
	triggerTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	triggerTraceID     = "4bf92f3577b34da6a3ce929d0e0e4736"
)

// recordSpanWithTrigger starts a recording span (standing in for the
// otelconnect RelayCommsCall origin span), runs linkTrigger against tp on its
// context, and returns the ended span.
func recordSpanWithTrigger(t *testing.T, name, tp string) sdktrace.ReadOnlySpan {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))

	// context.Background() as the test root (_test.go exemption).
	ctx, span := provider.Tracer("runnerhub-link-test").Start(context.Background(), name)
	linkTrigger(ctx, tp)
	span.End()

	ended := rec.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want exactly 1", len(ended))
	}
	return ended[0]
}

// TestLinkTriggerAddsLinkForValidTraceparent asserts a non-empty, valid
// trigger_traceparent produces exactly one link at the trigger's trace — and
// that it is a LINK, never a parent: the reply span stays a fresh root whose
// own trace id is independent of the trigger's.
func TestLinkTriggerAddsLinkForValidTraceparent(t *testing.T) {
	span := recordSpanWithTrigger(t, "reply", triggerTraceparent)

	links := span.Links()
	if len(links) != 1 {
		t.Fatalf("links = %d, want 1", len(links))
	}
	want, err := trace.TraceIDFromHex(triggerTraceID)
	if err != nil {
		t.Fatalf("TraceIDFromHex: %v", err)
	}
	if got := links[0].SpanContext.TraceID(); got != want {
		t.Errorf("link trace id = %s, want the trigger's %s", got, want)
	}
	if got := span.SpanContext().TraceID(); got == want {
		t.Errorf("reply span trace id = %s, want a trace INDEPENDENT of the trigger's — a link, not a parent", got)
	}
	if parent := span.Parent(); parent.IsValid() {
		t.Errorf("reply span has parent %s, want a fresh root", parent.SpanID())
	}
	// The discriminator is the contract a trace consumer selects the cross-turn
	// edge by: in production this span also carries otelconnect's transport
	// link, so an unstamped link is indistinguishable from that one. Asserted
	// against literals, not against linkKindAttr, so a rename of either half
	// reddens here instead of silently agreeing with itself.
	var stamped bool
	for _, a := range links[0].Attributes {
		if string(a.Key) == "compass.link.kind" && a.Value.AsString() == "cross_turn_trigger" {
			stamped = true
		}
	}
	if !stamped {
		t.Errorf("link attributes = %+v, want compass.link.kind=cross_turn_trigger", links[0].Attributes)
	}
}

// TestLinkTriggerAddsNoLinkWithoutValidTrigger asserts the rejection half: an
// empty trigger_traceparent adds no link at all (not an empty one), and a
// malformed one adds none either rather than degrading into a self-link.
func TestLinkTriggerAddsNoLinkWithoutValidTrigger(t *testing.T) {
	for _, tc := range []struct {
		name string
		tp   string
	}{
		{"empty", ""},
		{"malformed", "not-a-traceparent"},
		{"bad hex", "00-xyz-abc-01"},
		{"all-zero ids", "00-00000000000000000000000000000000-0000000000000000-00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			span := recordSpanWithTrigger(t, "reply", tc.tp)
			if links := span.Links(); len(links) != 0 {
				t.Fatalf("links = %d (first target trace %s), want 0",
					len(links), links[0].SpanContext.TraceID())
			}
		})
	}
}
