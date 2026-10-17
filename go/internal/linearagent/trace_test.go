package linearagent

import (
	"context"
	"testing"
	"testing/synctest"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

type activityContextClient struct {
	contexts chan context.Context
}

func (c *activityContextClient) CreateActivity(ctx context.Context, _ string, _ ActivityContent) error {
	c.contexts <- ctx
	return nil
}

func (*activityContextClient) UpdateSession(context.Context, string, []ExternalURL) error {
	return nil
}

func TestTailCommsRestoresEventTrace(t *testing.T) {
	previous := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		if err := tp.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("tracer provider shutdown: %v", err)
		}
	})

	synctest.Test(t, func(t *testing.T) {
		client := &activityContextClient{contexts: make(chan context.Context, 1)}
		d := NewDispatcher(DispatcherParams{Client: client})
		d.armReply("topic", "sess", "manager")
		bus := events.NewBus[*compassv1.SubscribeCommsResponse]()

		serveCtx, serveSpan := tp.Tracer("tail-test").Start(t.Context(), "serve")
		defer serveSpan.End()
		ctx, cancel := context.WithCancel(serveCtx)
		done := make(chan error, 1)
		go func() { done <- d.TailComms(ctx, bus) }()
		defer func() {
			cancel()
			<-done
		}()
		synctest.Wait()

		eventCtx, eventSpan := tp.Tracer("tail-test").Start(t.Context(), "message")
		bus.Publish(eventCtx, managerPost("topic", "manager"))
		eventSpan.End()
		synctest.Wait()
		got := <-client.contexts
		gotSC := trace.SpanContextFromContext(got)
		if gotSC.TraceID() != eventSpan.SpanContext().TraceID() {
			t.Fatalf("CreateActivity trace id = %s, want message trace %s", gotSC.TraceID(), eventSpan.SpanContext().TraceID())
		}
		if gotSC.TraceID() == serveSpan.SpanContext().TraceID() {
			t.Fatalf("CreateActivity inherited serve trace %s", gotSC.TraceID())
		}
	})
}

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
