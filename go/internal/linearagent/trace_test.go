package linearagent

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestDispatcherSpanIsChildOfEnqueuer(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		if err := tp.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("tracer provider shutdown: %v", err)
		}
	})

	comms := &recordingComms{posted: make(chan struct{}, 1)}
	d := newTestDispatcher(t, dispatcherDeps{
		res:    &fakeResolver{manager: "mgr", homeChannel: "chan"},
		comms:  comms,
		topics: &fakeTopics{topicID: "topic"},
		assoc:  &fakeAssoc{},
		client: &recordingClient{},
	})
	stop := runDispatcher(t, d)

	webhookCtx, webhook := tp.Tracer("test").Start(t.Context(), "webhook")
	if err := d.Enqueue(webhookCtx, &SessionEvent{Action: "created", AgentSession: AgentSession{ID: "sess-1"}}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	webhook.End()
	<-comms.posted
	stop() // joins Run, so the dispatch span has ended

	var found bool
	for _, s := range rec.Ended() {
		if s.Name() != "linearagent.dispatch" {
			continue
		}
		found = true
		if s.Parent().SpanID() != webhook.SpanContext().SpanID() {
			t.Fatalf("dispatch parent = %s, want the webhook span %s", s.Parent().SpanID(), webhook.SpanContext().SpanID())
		}
	}
	if !found {
		t.Fatal("no linearagent.dispatch span recorded")
	}
}
