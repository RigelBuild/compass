//go:build unix

package delivery

// T5/RIG-2892 trace-propagation acceptance cases for the delivery consumer.
// Each drives the consumer through the real events bus + hand-written fakes and
// event-gates on observed dispatches — never a sleep (rule://no-retries).
// context.Background() is the test root (rule://go-thread-context exemption for
// _test.go); it is threaded into every span/dispatch below and never re-rooted.

import (
	"context"
	"testing"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/store"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// spanTraceparent starts a span on a fresh SDK provider and returns the ctx
// carrying it plus the W3C traceparent the propagator serializes from it. The
// provider is a local one (not the global), so the span context rides ctx
// without the test depending on any global provider being installed.
func spanTraceparent(t *testing.T) (context.Context, string) {
	t.Helper()
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "publisher")
	t.Cleanup(func() { span.End() })
	got := otelx.Traceparent(ctx)
	if got == "" {
		t.Fatal("precondition: active span produced an empty traceparent")
	}
	return ctx, got
}

// publishCtxResponse publishes a MessagePosted onto the bus under ctx, so the
// bus stamps ctx's active-span traceparent onto the Stamped envelope — the
// publish-point origin the consumer's Run loop extracts (Move 1 + Move 2).
func publishCtxResponse(bus *events.Bus[*compassv1.SubscribeCommsResponse], ctx context.Context, msg *compassv1.Message) {
	bus.PublishCtx(ctx, postedResponse(msg))
}

// TestLiveDeliverAndSteerCarryPublisherTraceparent pins Moves 2+3 on the live
// path: a message published under an active span carries that span's
// traceparent onto BOTH the deliver op (to a plain subscriber) and the steer op
// (to a mentioned member). Empty when the publish had no span (empty-in ⇒
// empty-out). Mirrors TestDeliverAndSteerCarryAuthorFromHandle's harness.
func TestLiveDeliverAndSteerCarryPublisherTraceparent(t *testing.T) {
	t.Run("with span", func(t *testing.T) {
		c, disp, res, reads := newTestConsumer(t)
		const ch store.ChannelID = "chan-1"
		const human store.AccountID = "human-1"
		const agentA, agentB store.AccountID = "agent-a", "agent-b"

		reads.subscribers[ch] = []store.AccountID{agentA, agentB}
		reads.members[ch] = []store.AccountID{agentA, agentB}
		reads.handles["aa"] = agentAccount(agentA, "aa")
		reads.accounts[human] = store.Account{ID: human, Handle: "matt"}
		res.bind(agentA, "sess-a")
		res.bind(agentB, "sess-b")
		startConsumer(t, c)

		ctx, want := spanTraceparent(t)
		// @aa steers agent-a; agent-b (subscribed, unmentioned) gets a deliver.
		publishCtxResponse(c.bus, ctx, wireText("m1", human, "hey @aa"))
		disp.waitForDispatches(t, 2)

		got := disp.snapshot()
		a := recordsFor(got, "sess-a")
		if len(a) != 1 || a[0].kind != opSteer || a[0].traceparent != want {
			t.Fatalf("sess-a records = %+v, want one steer with traceparent=%q", a, want)
		}
		b := recordsFor(got, "sess-b")
		if len(b) != 1 || b[0].kind != opDeliver || b[0].traceparent != want {
			t.Fatalf("sess-b records = %+v, want one deliver with traceparent=%q", b, want)
		}
	})

	t.Run("no span emits empty", func(t *testing.T) {
		c, disp, res, reads := newTestConsumer(t)
		const ch store.ChannelID = "chan-1"
		const human store.AccountID = "human-1"
		const agentA store.AccountID = "agent-a"

		reads.subscribers[ch] = []store.AccountID{agentA}
		reads.members[ch] = []store.AccountID{agentA}
		reads.accounts[human] = store.Account{ID: human, Handle: "matt"}
		res.bind(agentA, "sess-a")
		startConsumer(t, c)

		// A plain Publish (no span, no PublishCtx) ⇒ empty envelope traceparent.
		c.bus.Publish(postedResponse(wireText("m1", human, "hi")))
		disp.waitForDispatches(t, 1)

		got := disp.snapshot()
		if len(got) != 1 || got[0].kind != opDeliver || got[0].traceparent != "" {
			t.Fatalf("dispatch = %+v, want one deliver with empty traceparent", got)
		}
	})
}

// TestHeldThenSettleCarriesOriginTraceparentAcrossSettleEdge pins Move 5 (the
// load-bearing invariant): a message held while its agent author streams is
// posted under span A on the bus goroutine, but fired at the author's settle
// edge on the bare settle-drain loop ctx (no active span). The origin
// traceparent captured at hold() must be restamped at fireHeld(), so the
// settled deliver carries A's trace across the goroutine boundary. Empty origin
// ⇒ empty on the wire.
func TestHeldThenSettleCarriesOriginTraceparentAcrossSettleEdge(t *testing.T) {
	t.Run("origin restamped across the settle edge", func(t *testing.T) {
		c, disp, res, reads := newTestConsumer(t)
		const ch store.ChannelID = "chan-1"
		const authorAgent store.AccountID = "agent-author"
		const recipient store.AccountID = "agent-recip"

		reads.subscribers[ch] = []store.AccountID{recipient, authorAgent}
		reads.agents[authorAgent] = true
		res.bind(authorAgent, "sess-author")
		res.bind(recipient, "sess-recip")
		reads.seedMessage(textMessage("m1", authorAgent, "settled body"))
		startConsumer(t, c)

		ctx, want := spanTraceparent(t)
		// Post under span A while the author streams: HELD, nothing dispatched.
		publishCtxResponse(c.bus, ctx, wireText("m1", authorAgent, "initial body"))
		c.waitHeld(t, "sess-author", 1)
		if got := disp.snapshot(); len(got) != 0 {
			t.Fatalf("dispatched %d before settle, want 0 (held)", len(got))
		}

		// Settle fires the held deliver on the bare drain ctx (no span). The
		// origin traceparent must be restamped from the held entry.
		c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
		disp.waitForDispatches(t, 1)

		got := disp.snapshot()
		if got[0].sessionID != "sess-recip" || got[0].messageID != "m1" {
			t.Fatalf("post-settle dispatch = %+v, want {sess-recip, m1}", got[0])
		}
		if got[0].traceparent != want {
			t.Fatalf("held-then-settled deliver traceparent = %q, want origin %q", got[0].traceparent, want)
		}
	})

	t.Run("empty origin stays empty", func(t *testing.T) {
		c, disp, res, reads := newTestConsumer(t)
		const ch store.ChannelID = "chan-1"
		const authorAgent store.AccountID = "agent-author"
		const recipient store.AccountID = "agent-recip"

		reads.subscribers[ch] = []store.AccountID{recipient, authorAgent}
		reads.agents[authorAgent] = true
		res.bind(authorAgent, "sess-author")
		res.bind(recipient, "sess-recip")
		reads.seedMessage(textMessage("m1", authorAgent, "settled body"))
		startConsumer(t, c)

		// Posted with no span ⇒ empty origin held ⇒ empty on the fired deliver.
		c.bus.Publish(postedResponse(wireText("m1", authorAgent, "initial body")))
		c.waitHeld(t, "sess-author", 1)
		c.OnSessionSettled("sess-author", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
		disp.waitForDispatches(t, 1)

		got := disp.snapshot()
		if got[0].traceparent != "" {
			t.Fatalf("held-then-settled deliver traceparent = %q, want empty", got[0].traceparent)
		}
	})
}

// TestDispatchNeverBlocksWithoutProviderOrSpan is the invariant guard: with no
// global tracer/meter provider installed and no active span, the dispatch path
// still delivers and emits an empty traceparent — the trace machinery never
// blocks or fails a delivery.
func TestDispatchNeverBlocksWithoutProviderOrSpan(t *testing.T) {
	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const human store.AccountID = "human-1"
	const agentA store.AccountID = "agent-a"

	reads.subscribers[ch] = []store.AccountID{agentA}
	reads.accounts[human] = store.Account{ID: human, Handle: "matt"}
	res.bind(agentA, "sess-a")
	startConsumer(t, c)

	// No global provider installed by this test, no span on the publish ctx.
	c.bus.Publish(postedResponse(wireText("m1", human, "hi")))
	disp.waitForDispatches(t, 1)

	got := disp.snapshot()
	if len(got) != 1 || got[0].messageID != "m1" || got[0].traceparent != "" {
		t.Fatalf("dispatch = %+v, want one deliver of m1 with empty traceparent", got)
	}
}

// TestDispatchMetricIncrementsWithOpKindOnly pins Move 7's cardinality rule:
// compass.delivery.dispatched increments once per dispatch, labelled with
// compass.op.kind (steer|deliver) and NOTHING else — never per-session,
// per-channel, or per-message. Asserted via an in-memory metric reader over a
// consumer built while a local SDK meter provider is the global.
func TestDispatchMetricIncrementsWithOpKindOnly(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	// Install the SDK meter as the global BEFORE NewConsumer, so the consumer's
	// counter is created against this reader (Move 7 creates it once at
	// construction from the global meter). Restore the prior global on cleanup so
	// this test never leaks its provider into another.
	prevMP := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(prevMP) })
	otel.SetMeterProvider(mp)

	c, disp, res, reads := newTestConsumer(t)
	const ch store.ChannelID = "chan-1"
	const human store.AccountID = "human-1"
	const agentA, agentB store.AccountID = "agent-a", "agent-b"

	reads.subscribers[ch] = []store.AccountID{agentA, agentB}
	reads.members[ch] = []store.AccountID{agentA, agentB}
	reads.handles["aa"] = agentAccount(agentA, "aa")
	reads.accounts[human] = store.Account{ID: human, Handle: "matt"}
	res.bind(agentA, "sess-a")
	res.bind(agentB, "sess-b")
	startConsumer(t, c)

	// @aa steers agent-a; agent-b gets a deliver — one of each op kind.
	c.bus.Publish(postedResponse(wireText("m1", human, "hey @aa")))
	disp.waitForDispatches(t, 2)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	counts := dispatchedCounts(t, &rm)
	if counts["steer"] != 1 {
		t.Fatalf("steer count = %d, want 1 (counts=%v)", counts["steer"], counts)
	}
	if counts["deliver"] != 1 {
		t.Fatalf("deliver count = %d, want 1 (counts=%v)", counts["deliver"], counts)
	}
}

// dispatchedCounts extracts the compass.delivery.dispatched sum per op-kind and
// asserts op.kind is the ONLY attribute on every data point (the cardinality
// hard rule).
func dispatchedCounts(t *testing.T, rm *metricdata.ResourceMetrics) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "compass.delivery.dispatched" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("compass.delivery.dispatched data = %T, want Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				attrs := dp.Attributes.ToSlice()
				if len(attrs) != 1 || attrs[0].Key != attribute.Key("compass.op.kind") {
					t.Fatalf("data point attrs = %v, want exactly {compass.op.kind}", attrs)
				}
				out[attrs[0].Value.AsString()] += dp.Value
			}
		}
	}
	return out
}
