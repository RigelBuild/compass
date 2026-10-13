package ingest

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/RigelBuild/compass/go/internal/forge"
)

// installSpanRecorder makes a recording provider global for one test, so the
// package tracer (resolved from the global at Start) records into it.
func installSpanRecorder(t *testing.T) (*tracetest.SpanRecorder, trace.Tracer) {
	t.Helper()
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
	return rec, tp.Tracer("test")
}

// endedNamed returns the ended spans called name.
func endedNamed(rec *tracetest.SpanRecorder, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

// requireOneRoot asserts exactly one span called name was recorded, and that it
// is a root: a stray span on the unit's ctx must not become its parent.
func requireOneRoot(t *testing.T, rec *tracetest.SpanRecorder, name string, stray trace.Span) {
	t.Helper()
	got := endedNamed(rec, name)
	if len(got) != 1 {
		t.Fatalf("%s spans = %d, want 1", name, len(got))
	}
	if got[0].Parent().IsValid() {
		t.Fatalf("%s parent = %s, want a root", name, got[0].Parent().SpanID())
	}
	if got[0].SpanContext().TraceID() == stray.SpanContext().TraceID() {
		t.Fatalf("%s joined the stray trace, want its own", name)
	}
}

func TestSweepUnitsStartOneRootSpan(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(ctx context.Context)
	}{
		{"ingest.board.reconcile", func(ctx context.Context) {
			l := &fakeUpdatedLister{results: []forge.ConditionalResult[[]forge.Issue]{{}}}
			newBoardHarness(t, l, newBoardStore("o/r"), &recordingSink{}).sweep(ctx)
		}},
		{"ingest.notify.reconcile", func(ctx context.Context) {
			newReconciler(t, &fakeReader{}, &fakeNotifyStore{}, &fakeDispatcher{}).sweep(ctx)
		}},
		{"ingest.mergequeue.sweep", func(ctx context.Context) {
			configs := fixedQueueConfig{provider: "trunk", branch: "main"}
			NewMergeQueueSweeper(nil, configs, &fixedQueuePRStore{}, &recordingQueueSink{}, time.Hour).sweep(ctx)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, tracer := installSpanRecorder(t)
			ctx, stray := tracer.Start(t.Context(), "stray")
			defer stray.End()
			tc.run(ctx)
			requireOneRoot(t, rec, tc.name, stray)
		})
	}
}

func TestNotifyWebhookArmRouteSpanIsChildOfEnqueuer(t *testing.T) {
	rec, tracer := installSpanRecorder(t)
	r := &fakeEventRouter{notify: make(chan struct{}, 1)}
	arm := NewNotifyWebhookArm(r, NotifyArmConfig{QueueSize: 16})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- arm.Run(ctx) }()

	webhookCtx, webhook := tracer.Start(t.Context(), "webhook")
	arm.Enqueue(webhookCtx, notifyEvent(1))
	webhook.End()
	<-r.notify
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want nil on cancel", err)
	}

	got := endedNamed(rec, "ingest.notify.route")
	if len(got) != 1 {
		t.Fatalf("route spans = %d, want 1", len(got))
	}
	if got[0].Parent().SpanID() != webhook.SpanContext().SpanID() {
		t.Fatalf("route parent = %s, want the webhook span %s", got[0].Parent().SpanID(), webhook.SpanContext().SpanID())
	}
}

func TestBoardWebhookArmPassLinksEveryEnqueuer(t *testing.T) {
	rec, tracer := installSpanRecorder(t)
	arm, _ := newArmHarness(t, &fakeHydrator{}, newFakeTargets("owner/repo"), 16)

	want := make([]trace.SpanContext, 0, 2)
	for _, n := range []uint64{7, 8} {
		ctx, span := tracer.Start(t.Context(), "webhook")
		arm.Enqueue(ctx, issueEvent("owner/repo", n, changeUpdate))
		span.End()
		want = append(want, span.SpanContext())
	}
	drainAll(t.Context(), arm)

	got := endedNamed(rec, "ingest.board.drain")
	if len(got) != 1 {
		t.Fatalf("pass spans = %d, want 1 for the coalesced batch", len(got))
	}
	pass := got[0]
	if pass.Parent().IsValid() {
		t.Fatalf("pass parent = %s, want a root", pass.Parent().SpanID())
	}
	linked := map[trace.SpanID]bool{}
	for _, l := range pass.Links() {
		linked[l.SpanContext.SpanID()] = true
	}
	for _, sc := range want {
		if !linked[sc.SpanID()] {
			t.Fatalf("pass links %v, want a link to enqueuer %s", pass.Links(), sc.SpanID())
		}
	}
}

func TestBoardWebhookArmRetryKeepsEnqueuerLinkAcrossHolds(t *testing.T) {
	rec, tracer := installSpanRecorder(t)
	pulls := &fakePulls{errFor: map[uint64]error{9: forge.ErrBudgetExhausted}}
	arm, _ := newPRArm(t, pulls, nil)
	ctx, webhook := tracer.Start(t.Context(), "webhook")
	arm.Enqueue(ctx, prEvent(9, changeUpdate))
	webhook.End()
	if _, paused := arm.drainBatch(t.Context(), <-arm.queue); !paused {
		t.Fatal("first pass did not pause")
	}
	key := boardCoord{repo: "owner/repo", kind: boardKindPR, number: 9}
	for i := range 3 {
		if _, paused := arm.retryPending(t.Context()); !paused {
			t.Fatalf("retry %d did not pause with the gate shut", i)
		}
		if n := len(arm.pending[key]); n != 1 {
			t.Fatalf("after retry %d held traceparents = %d, want 1", i, n)
		}
	}
	pulls.mu.Lock()
	pulls.errFor = nil
	pulls.mu.Unlock()
	if _, paused := arm.retryPending(t.Context()); paused {
		t.Fatal("retry paused with the gate open")
	}

	passes := endedNamed(rec, "ingest.board.drain")
	if len(passes) != 5 {
		t.Fatalf("pass spans = %d, want 5 (first, 3 held retries, final)", len(passes))
	}
	for i, p := range passes {
		if l := p.Links(); len(l) != 1 || l[0].SpanContext.SpanID() != webhook.SpanContext().SpanID() {
			t.Fatalf("pass %d links %v, want only the webhook span %s", i, l, webhook.SpanContext().SpanID())
		}
	}
}

// Events seen during a pause must join the eventual retry pass.
func TestBoardWebhookArmHeldEventLinksRetry(t *testing.T) {
	rec, tracer := installSpanRecorder(t)
	pulls := &fakePulls{errFor: map[uint64]error{9: forge.ErrBudgetExhausted}}
	arm, _ := newPRArm(t, pulls, nil)

	ctx, firstWebhook := tracer.Start(t.Context(), "webhook")
	arm.Enqueue(ctx, prEvent(9, changeUpdate))
	firstWebhook.End()
	if _, paused := arm.drainBatch(t.Context(), <-arm.queue); !paused {
		t.Fatal("first pass did not pause")
	}

	ctx, secondWebhook := tracer.Start(t.Context(), "webhook")
	arm.Enqueue(ctx, prEvent(9, changeUpdate))
	secondWebhook.End()
	if _, paused := arm.drainBatch(t.Context(), <-arm.queue); paused {
		t.Fatal("held event paused a fresh batch")
	}

	pulls.mu.Lock()
	pulls.errFor = nil
	pulls.mu.Unlock()
	if _, paused := arm.retryPending(t.Context()); paused {
		t.Fatal("retry paused with the gate open")
	}

	passes := endedNamed(rec, "ingest.board.drain")
	if len(passes) == 0 {
		t.Fatal("no retry pass recorded")
	}
	links := passes[len(passes)-1].Links()
	if len(links) != 2 {
		t.Fatalf("retry pass links = %d, want 2 carried traces", len(links))
	}
	linked := map[trace.SpanID]bool{}
	for _, link := range links {
		linked[link.SpanContext.SpanID()] = true
	}
	for _, span := range []trace.Span{firstWebhook, secondWebhook} {
		if !linked[span.SpanContext().SpanID()] {
			t.Fatalf("retry pass links %v, want webhook span %s", links, span.SpanContext().SpanID())
		}
	}
}
