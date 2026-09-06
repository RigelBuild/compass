package fabric

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// TestEventFabricRoundTrip defends the core promise of the event plane: an
// EventRef published on a comms subject reaches a subscriber on that subject
// with tenant, kind and row id intact through encode → JetStream → decode. If
// any field were lost the subscriber would re-read the wrong row, or no row.
func TestEventFabricRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f := newFabric(t, Config{})

	subject, err := CommsSubject("t1", KindMessagePosted)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}
	got := make(chan EventRef, 1)
	unsub, err := f.Subscribe(ctx, subject, func(r EventRef) { got <- r })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()

	want := EventRef{Tenant: "t1", Kind: KindMessagePosted, RowID: "msg-1"}
	if err := f.Publish(ctx, subject, want); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if delivered := recvRef(t, got); delivered != want {
		t.Fatalf("delivered %+v, want %+v", delivered, want)
	}
}

// TestEventFabricDedupsIdenticalPublishes defends the WithMsgID dedup the
// record calls for: two Servers publishing the same logical change — or one
// retrying a publish whose ack was lost — must deliver ONCE. Without it every
// retry would drive the subscriber's handler a second time.
//
// The second delivery is proven absent by a positive gate, not a sleep: a third
// publish with a DIFFERENT ref must arrive, and the duplicate must not have
// arrived before it. Ordering within one stream subject is the stream's, so if
// the duplicate were going to be delivered it would be delivered first.
func TestEventFabricDedupsIdenticalPublishes(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f := newFabric(t, Config{})

	subject, err := CommsSubject("t1", KindMessagePosted)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}
	got := make(chan EventRef, 4)
	unsub, err := f.Subscribe(ctx, subject, func(r EventRef) { got <- r })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()

	dup := EventRef{Tenant: "t1", Kind: KindMessagePosted, RowID: "msg-dup"}
	sentinel := EventRef{Tenant: "t1", Kind: KindMessagePosted, RowID: "msg-sentinel"}
	for _, ref := range []EventRef{dup, dup, sentinel} {
		if err := f.Publish(ctx, subject, ref); err != nil {
			t.Fatalf("Publish %s: %v", ref.RowID, err)
		}
	}

	if first := recvRef(t, got); first != dup {
		t.Fatalf("first delivery = %+v, want %+v", first, dup)
	}
	// The sentinel arriving second is what proves the duplicate was deduped:
	// the stream preserves per-subject order, so a stored duplicate would be
	// delivered ahead of it.
	if second := recvRef(t, got); second != sentinel {
		t.Fatalf("second delivery = %+v, want the sentinel %+v (the duplicate was not deduped)", second, sentinel)
	}
}

// TestEventFabricFiltersBySubject defends tenant isolation on a single shared
// stream. The COMPASS_COMMS stream captures every tenant's every kind, so the
// consumer's FilterSubject is the ONLY thing keeping tenant t2's events out of
// tenant t1's subscriber — a cross-tenant leak if it drifted.
func TestEventFabricFiltersBySubject(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f := newFabric(t, Config{})

	mine, err := CommsSubject("t1", KindMessagePosted)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}
	theirs, err := CommsSubject("t2", KindAccountChanged)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}

	got := make(chan EventRef, 4)
	unsub, err := f.Subscribe(ctx, mine, func(r EventRef) { got <- r })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()

	// The other tenant's event goes first. If the filter leaked it would be
	// delivered before the sentinel, since both are already stored.
	if err := f.Publish(ctx, theirs, EventRef{Tenant: "t2", Kind: KindAccountChanged, RowID: "acct-1"}); err != nil {
		t.Fatalf("Publish to the other tenant: %v", err)
	}
	want := EventRef{Tenant: "t1", Kind: KindMessagePosted, RowID: "msg-1"}
	if err := f.Publish(ctx, mine, want); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if delivered := recvRef(t, got); delivered != want {
		t.Fatalf("delivered %+v, want %+v — tenant t2's event leaked through the subject filter", delivered, want)
	}
}

// TestEventFabricConcreteAndWildcardConsumersCoexist proves that concrete and
// tenant-wildcard subscriptions are independent durables on one fabric. The
// concrete filter must exclude t2, while the wildcard receives both tenants.
func TestEventFabricConcreteAndWildcardConsumersCoexist(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f := newFabric(t, Config{})
	concreteSubject, err := CommsSubject("t1", KindMessagePosted)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}
	wildcardSubject, err := CommsWildcardSubject(KindMessagePosted)
	if err != nil {
		t.Fatalf("CommsWildcardSubject: %v", err)
	}
	if durableName(wildcardSubject) == durableName(concreteSubject) {
		t.Fatalf("wildcard and concrete durable names collide: %q", durableName(wildcardSubject))
	}
	concrete := make(chan EventRef, 4)
	wildcard := make(chan EventRef, 4)
	unsubConcrete, err := f.Subscribe(ctx, concreteSubject, func(r EventRef) { concrete <- r })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsubConcrete()
	unsubWildcard, err := f.SubscribeKind(ctx, KindMessagePosted, func(r EventRef) { wildcard <- r })
	if err != nil {
		t.Fatalf("SubscribeKind: %v", err)
	}
	defer unsubWildcard()
	t1 := EventRef{Tenant: "t1", Kind: KindMessagePosted, RowID: "msg-t1"}
	t2 := EventRef{Tenant: "t2", Kind: KindMessagePosted, RowID: "msg-t2"}
	sentinel := EventRef{Tenant: "t1", Kind: KindMessagePosted, RowID: "msg-sentinel"}
	for _, ref := range []EventRef{t1, t2, sentinel} {
		subject, subjectErr := CommsSubject(ref.Tenant, ref.Kind)
		if subjectErr != nil {
			t.Fatalf("CommsSubject(%s): %v", ref.Tenant, subjectErr)
		}
		if err := f.Publish(ctx, subject, ref); err != nil {
			t.Fatalf("Publish %s: %v", ref.RowID, err)
		}
	}
	if got := recvRef(t, concrete); got != t1 {
		t.Fatalf("concrete first delivery = %+v, want %+v (t2 leaked)", got, t1)
	}
	if got := recvRef(t, concrete); got != sentinel {
		t.Fatalf("concrete second delivery = %+v, want sentinel %+v (filter leaked)", got, sentinel)
	}
	first, second := recvRef(t, wildcard), recvRef(t, wildcard)
	seen := map[string]bool{first.RowID: true, second.RowID: true}
	if !seen[t1.RowID] || !seen[t2.RowID] || len(seen) != 2 {
		t.Fatalf("wildcard deliveries = %v, want t1 and t2", seen)
	}
}

// TestUnsubscribeStopsDelivery defends that Unsubscribe actually stops the
// consume context. A leaked consumer would keep draining the shared durable
// consumer after its owner is gone — events claimed by nobody, which on a
// durable consumer means silently dropped for every other instance too.
//
// Absence of delivery is proven positively: after unsubscribing, a fresh
// subscriber on the same subject receives the event the stale callback must not
// have seen.
func TestUnsubscribeStopsDelivery(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f := newFabric(t, Config{})

	subject, err := CommsSubject("t1", KindChannelChanged)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}

	var stale atomic.Int64
	first := make(chan EventRef, 1)
	unsub, err := f.Subscribe(ctx, subject, func(r EventRef) {
		stale.Add(1)
		select {
		case first <- r:
		default:
		}
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Prove the subscription is live before tearing it down, so the assertion
	// below distinguishes "stopped" from "never started".
	warmup := EventRef{Tenant: "t1", Kind: KindChannelChanged, RowID: "ch-warmup"}
	if err := f.Publish(ctx, subject, warmup); err != nil {
		t.Fatalf("Publish warmup: %v", err)
	}
	if got := recvRef(t, first); got != warmup {
		t.Fatalf("warmup delivered %+v, want %+v", got, warmup)
	}
	before := stale.Load()

	unsub()
	// Idempotent by contract: a second call must not double-Stop.
	unsub()

	// A second subscriber on the same subject picks up where the consumer left
	// off; its delivery is the gate.
	second := make(chan EventRef, 1)
	unsub2, err := f.Subscribe(ctx, subject, func(r EventRef) { second <- r })
	if err != nil {
		t.Fatalf("second Subscribe: %v", err)
	}
	defer unsub2()

	want := EventRef{Tenant: "t1", Kind: KindChannelChanged, RowID: "ch-after"}
	if err := f.Publish(ctx, subject, want); err != nil {
		t.Fatalf("Publish after unsubscribe: %v", err)
	}
	if got := recvRef(t, second); got != want {
		t.Fatalf("second subscriber delivered %+v, want %+v", got, want)
	}
	if after := stale.Load(); after != before {
		t.Fatalf("the unsubscribed callback ran %d more time(s) after Unsubscribe", after-before)
	}
}

// TestSubscribeStopsWhenContextIsDone defends the other teardown path: a
// Subscribe whose ctx is cancelled must stop consuming without the caller
// calling Unsubscribe. Otherwise a server shutdown that cancels its root
// context would leave every subscription's goroutine running.
func TestSubscribeStopsWhenContextIsDone(t *testing.T) {
	t.Parallel()
	f := newFabric(t, Config{})

	subject, err := CommsSubject("t1", KindTopicUpserted)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}

	// Rooted at context.Background() because this is a test root.
	subCtx, cancel := context.WithCancel(context.Background())
	live := make(chan EventRef, 1)
	unsub, err := f.Subscribe(subCtx, subject, func(r EventRef) { live <- r })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()

	pubCtx := testCtx(t)
	warmup := EventRef{Tenant: "t1", Kind: KindTopicUpserted, RowID: "topic-warmup"}
	if err := f.Publish(pubCtx, subject, warmup); err != nil {
		t.Fatalf("Publish warmup: %v", err)
	}
	if got := recvRef(t, live); got != warmup {
		t.Fatalf("warmup delivered %+v, want %+v", got, warmup)
	}

	cancel()

	// Gate on the replacement subscription receiving, exactly as the
	// Unsubscribe test does.
	after := make(chan EventRef, 1)
	unsub2, err := f.Subscribe(pubCtx, subject, func(r EventRef) { after <- r })
	if err != nil {
		t.Fatalf("second Subscribe: %v", err)
	}
	defer unsub2()

	want := EventRef{Tenant: "t1", Kind: KindTopicUpserted, RowID: "topic-after"}
	if err := f.Publish(pubCtx, subject, want); err != nil {
		t.Fatalf("Publish after cancel: %v", err)
	}
	if got := recvRef(t, after); got != want {
		t.Fatalf("delivered %+v, want %+v", got, want)
	}
	select {
	case leaked := <-live:
		t.Fatalf("the cancelled subscription delivered %+v; its consume context was not stopped", leaked)
	default:
	}
}

// TestPublishRejectsInvalidInput defends the fail-closed publish path. Each of
// these would otherwise become a stored, undecodable, or mis-subjected message
// that only surfaces later on the DLQ — far from the caller that caused it.
func TestPublishRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f := newFabric(t, Config{})

	subject, err := CommsSubject("t1", KindMessagePosted)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}
	for _, tc := range []struct {
		name    string
		subject string
		ref     EventRef
	}{
		{"no tenant", subject, EventRef{Kind: KindMessagePosted, RowID: "m1"}},
		{"no kind", subject, EventRef{Tenant: "t1", RowID: "m1"}},
		{"no row id", subject, EventRef{Tenant: "t1", Kind: KindMessagePosted}},
		{"empty subject", "", EventRef{Tenant: "t1", Kind: KindMessagePosted, RowID: "m1"}},
		{"wildcard subject root", "*.t1.comms.message_posted", EventRef{Tenant: "t1", Kind: KindMessagePosted, RowID: "m1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := f.Publish(ctx, tc.subject, tc.ref); err == nil {
				t.Fatalf("Publish(%q, %+v): want an error", tc.subject, tc.ref)
			}
		})
	}
}

// TestSubscribeRequiresCallback defends against the nil-callback footgun: a
// Subscribe with no handler would create a durable consumer that acks every
// event and hands it to nobody — a silent, permanent event sink on a shared
// consumer.
func TestSubscribeRequiresCallback(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f := newFabric(t, Config{})
	subject, err := CommsSubject("t1", KindMessagePosted)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}
	if _, err := f.Subscribe(ctx, subject, nil); err == nil {
		t.Fatal("Subscribe with a nil callback: want an error")
	}
}

// TestPoisonMessageParksOnDLQ defends the record's "max_deliver + dead-letter
// subject so a poison message parks instead of redelivering forever". A
// callback that always fails must be retried up to MaxDeliver and then parked —
// not retried in perpetuity, which would wedge the shared durable consumer's
// ack window and stall every subsequent event on that subject.
//
// The DLQ arrival is the gate; a raw core-NATS subscription reads it, and the
// park headers must identify the original subject so an operator needs no log
// correlation.
func TestPoisonMessageParksOnDLQ(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	url := testServer(t)
	// MaxDeliver 2 with an immediate Nak keeps the test fast without weakening
	// the invariant: the budget must be finite and the park must happen at it.
	f := newFabric(t, Config{URL: url, MaxDeliver: 2, Log: quietLogger(t)})

	// A raw connection reads the DLQ, proving the park is observable to a plain
	// core-NATS consumer (the DLQ is a diagnostic tap, not a stream).
	raw, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(raw.Close)
	dlq, err := raw.SubscribeSync(DLQSubject)
	if err != nil {
		t.Fatalf("SubscribeSync(%q): %v", DLQSubject, err)
	}
	if err := raw.FlushWithContext(ctx); err != nil {
		t.Fatalf("flushing the dlq subscription: %v", err)
	}

	subject, err := CommsSubject("t1", KindMessageUpdated)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}

	var attempts atomic.Int64
	// The callback panics: the fabric must treat a subscriber panic as a
	// failure (neither crashing the process nor acking an unhandled event), so
	// this exercises the panic guard and the retry budget together.
	unsub, err := f.Subscribe(ctx, subject, func(EventRef) {
		attempts.Add(1)
		panic("subscriber is broken")
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()

	poison := EventRef{Tenant: "t1", Kind: KindMessageUpdated, RowID: "msg-poison"}
	if err := f.Publish(ctx, subject, poison); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	msg, err := dlq.NextMsgWithContext(ctx)
	if err != nil {
		t.Fatalf("waiting for the parked message on %q: %v", DLQSubject, err)
	}
	parked, err := decodeEventRef(msg.Data)
	if err != nil {
		t.Fatalf("the parked payload must be the original event: %v", err)
	}
	if parked != poison {
		t.Fatalf("parked %+v, want %+v", parked, poison)
	}
	if got := msg.Header.Get(dlqHeaderSubject); got != subject {
		t.Errorf("park header %s = %q, want %q", dlqHeaderSubject, got, subject)
	}
	if msg.Header.Get(dlqHeaderReason) == "" {
		t.Errorf("park header %s is empty; an operator reading the dlq has no reason", dlqHeaderReason)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("callback ran %d time(s), want exactly MaxDeliver=2 attempts before parking", got)
	}
}

// TestWildcardConsumerParksWithConcreteSubject defends the DLQ's provenance for a
// wildcard consumer. park writes msg.Subject() — the CONCRETE delivered subject —
// not the consumer's filter, so a message parked by a SubscribeKind consumer still
// names its tenant. With the filter subject the header would read
// compass.*.comms.<kind> and an operator reading the DLQ could not tell which
// tenant the poison event belonged to.
func TestWildcardConsumerParksWithConcreteSubject(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	url := testServer(t)
	f := newFabric(t, Config{URL: url, MaxDeliver: 2, Log: quietLogger(t)})

	raw, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(raw.Close)
	dlq, err := raw.SubscribeSync(DLQSubject)
	if err != nil {
		t.Fatalf("SubscribeSync(%q): %v", DLQSubject, err)
	}
	if err := raw.FlushWithContext(ctx); err != nil {
		t.Fatalf("flushing the dlq subscription: %v", err)
	}

	concrete, err := CommsSubject("t1", KindMessagePosted)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}
	unsub, err := f.SubscribeKind(ctx, KindMessagePosted, func(EventRef) {
		panic("subscriber is broken")
	})
	if err != nil {
		t.Fatalf("SubscribeKind: %v", err)
	}
	defer unsub()

	poison := EventRef{Tenant: "t1", Kind: KindMessagePosted, RowID: "msg-poison"}
	if err := f.Publish(ctx, concrete, poison); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	msg, err := dlq.NextMsgWithContext(ctx)
	if err != nil {
		t.Fatalf("waiting for the parked message on %q: %v", DLQSubject, err)
	}
	parked, err := decodeEventRef(msg.Data)
	if err != nil {
		t.Fatalf("the parked payload must be the original event: %v", err)
	}
	if parked != poison {
		t.Fatalf("parked %+v, want %+v", parked, poison)
	}
	got := msg.Header.Get(dlqHeaderSubject)
	if got != concrete {
		t.Errorf("park header %s = %q, want concrete subject %q", dlqHeaderSubject, got, concrete)
	}
	wildcard, err := CommsWildcardSubject(KindMessagePosted)
	if err != nil {
		t.Fatalf("CommsWildcardSubject: %v", err)
	}
	if got == wildcard {
		t.Errorf("park header %s used wildcard subject %q", dlqHeaderSubject, wildcard)
	}
	if msg.Header.Get(dlqHeaderReason) == "" {
		t.Errorf("park header %s is empty; an operator reading the dlq has no reason", dlqHeaderReason)
	}
}

// TestSubscriberPanicDoesNotBlockOtherEvents defends the panic guard's
// consequence for throughput: one broken event must not wedge the subject. The
// poison event exhausts its budget and parks, and the next event is delivered —
// which is only true if the failing message is Term'd rather than left pending.
func TestSubscriberPanicDoesNotBlockOtherEvents(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f := newFabric(t, Config{MaxDeliver: 1, Log: quietLogger(t)})

	subject, err := CommsSubject("t1", KindAgentWorkspaceChanged)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}

	good := make(chan EventRef, 1)
	unsub, err := f.Subscribe(ctx, subject, func(r EventRef) {
		if r.RowID == "poison" {
			panic("subscriber is broken")
		}
		good <- r
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()

	if err := f.Publish(ctx, subject, EventRef{Tenant: "t1", Kind: KindAgentWorkspaceChanged, RowID: "poison"}); err != nil {
		t.Fatalf("Publish poison: %v", err)
	}
	want := EventRef{Tenant: "t1", Kind: KindAgentWorkspaceChanged, RowID: "ws-ok"}
	if err := f.Publish(ctx, subject, want); err != nil {
		t.Fatalf("Publish good: %v", err)
	}
	if got := recvRef(t, good); got != want {
		t.Fatalf("delivered %+v, want %+v", got, want)
	}
}

// TestUndecodablePayloadParksImmediately defends the decode-failure path's
// distinct policy: bytes that cannot parse will never parse, so redelivering
// them burns the budget for nothing. It must park on the FIRST delivery.
//
// The malformed payload is published raw through JetStream on a comms subject,
// which is what a rolling deploy of an incompatible publisher would look like.
func TestUndecodablePayloadParksImmediately(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	url := testServer(t)
	f := newFabric(t, Config{URL: url, MaxDeliver: 5, Log: quietLogger(t)})

	raw, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(raw.Close)
	dlq, err := raw.SubscribeSync(DLQSubject)
	if err != nil {
		t.Fatalf("SubscribeSync(%q): %v", DLQSubject, err)
	}
	if err := raw.FlushWithContext(ctx); err != nil {
		t.Fatalf("flushing the dlq subscription: %v", err)
	}

	subject, err := CommsSubject("t1", KindChannelGroupChanged)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}

	var calls atomic.Int64
	unsub, err := f.Subscribe(ctx, subject, func(EventRef) { calls.Add(1) })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()

	// Publishing through the fabric's own JetStream context, but with bytes the
	// codec cannot read.
	if _, err := f.js.Publish(ctx, subject, []byte("{not an event ref")); err != nil {
		t.Fatalf("publishing a malformed payload: %v", err)
	}

	msg, err := dlq.NextMsgWithContext(ctx)
	if err != nil {
		t.Fatalf("waiting for the parked message on %q: %v", DLQSubject, err)
	}
	if got, want := string(msg.Data), "{not an event ref"; got != want {
		t.Errorf("parked payload = %q, want the original bytes %q", got, want)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("callback ran %d time(s) for an undecodable payload, want 0", got)
	}
}

// TestSubscribeIsIdempotentAcrossInstances defends the durable-consumer design:
// two Fabrics on the same subject share ONE durable consumer, so each event is
// claimed by exactly one of them (§Q3's queue-group semantics). Two independent
// consumers would double-handle every comms event across a two-Server
// deployment.
func TestSubscribeIsIdempotentAcrossInstances(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	url := testServer(t)
	a := newFabric(t, Config{URL: url})
	b := newFabric(t, Config{URL: url})

	subject, err := CommsSubject("t1", KindMessagePosted)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}

	var (
		mu    sync.Mutex
		seen  []EventRef
		total = make(chan struct{}, 8)
	)
	record := func(r EventRef) {
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
		total <- struct{}{}
	}
	unsubA, err := a.Subscribe(ctx, subject, record)
	if err != nil {
		t.Fatalf("Subscribe on a: %v", err)
	}
	defer unsubA()
	unsubB, err := b.Subscribe(ctx, subject, record)
	if err != nil {
		t.Fatalf("Subscribe on b: %v", err)
	}
	defer unsubB()

	want := EventRef{Tenant: "t1", Kind: KindMessagePosted, RowID: "msg-shared"}
	if err := a.Publish(ctx, subject, want); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// One claim is the invariant. Gate on the first, then assert no second
	// arrives before a sentinel published afterwards is claimed — the same
	// positive-gate pattern as the dedup test.
	select {
	case <-total:
	case <-time.After(gate):
		t.Fatalf("no instance claimed the event within %s", gate)
	}
	sentinel := EventRef{Tenant: "t1", Kind: KindMessagePosted, RowID: "msg-sentinel"}
	if err := a.Publish(ctx, subject, sentinel); err != nil {
		t.Fatalf("Publish sentinel: %v", err)
	}
	select {
	case <-total:
	case <-time.After(gate):
		t.Fatalf("no instance claimed the sentinel within %s", gate)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("claims = %+v (%d), want exactly 2 — a shared durable consumer must not double-deliver", seen, len(seen))
	}
	if seen[0] != want || seen[1] != sentinel {
		t.Fatalf("claims = %+v, want [%+v %+v]", seen, want, sentinel)
	}
}

// TestEnsureStreamIsIdempotent defends CreateOrUpdateStream over CreateStream:
// a container restart, a second Server, and a config change must all converge
// on the same stream rather than one of them failing with "stream already
// exists".
func TestEnsureStreamIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	url := testServer(t)
	a := newFabric(t, Config{URL: url})
	b := newFabric(t, Config{URL: url})

	for i, f := range []*Fabric{a, b, a, b} {
		s, err := f.ensureStream(ctx)
		if err != nil {
			t.Fatalf("ensureStream #%d: %v", i, err)
		}
		if got := s.CachedInfo().Config.Name; got != DefaultStreamName {
			t.Fatalf("ensureStream #%d returned stream %q, want %q", i, got, DefaultStreamName)
		}
	}
}

// TestEnsureStreamErrorIsNotCached defends the retryability of a failed
// topology call: caching a failure would poison the Fabric for its whole
// lifetime, so a NATS blip during startup would permanently disable publishing
// on a process that is otherwise healthy and reconnected.
func TestEnsureStreamErrorIsNotCached(t *testing.T) {
	t.Parallel()
	f := newFabric(t, Config{})

	// An already-cancelled context fails the topology call without touching the
	// server. Rooted at Background because this is a test root.
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.ensureStream(dead); err == nil {
		t.Fatal("ensureStream with a cancelled context: want an error")
	}
	if f.stream != nil {
		t.Fatal("a failed ensureStream must not cache a stream handle")
	}
	if _, err := f.ensureStream(testCtx(t)); err != nil {
		t.Fatalf("ensureStream must be retryable after a failure: %v", err)
	}
}

// TestInvokeConvertsPanicToError defends the guard in isolation: a subscriber
// callback runs on the fabric's goroutine, so an unrecovered panic there would
// take the whole server down. It must become an error the delivery path can act
// on.
func TestInvokeConvertsPanicToError(t *testing.T) {
	t.Parallel()
	ref := EventRef{Tenant: "t1", Kind: KindMessagePosted, RowID: "m1"}

	if err := invoke(func(EventRef) {}, ref); err != nil {
		t.Fatalf("a callback that returns normally must not error: %v", err)
	}

	err := invoke(func(EventRef) { panic(errors.New("boom")) }, ref)
	if err == nil {
		t.Fatal("a panicking callback must yield an error, not a nil (which would ack an unhandled event)")
	}
	if got := err.Error(); !strings.Contains(got, "m1") || !strings.Contains(got, "boom") {
		t.Errorf("error %q should name the event and the cause", got)
	}
}

// TestPublishRejectsCrossTenantRef defends the EventRef↔subject binding, and it
// is a cross-tenant leak that it closes: a head-only subject guard accepted
// tenant-a's ref published on tenant-b's subject, so the instance that claimed
// it — scoped to tenant-b — would be told to re-read a tenant-a row.
//
// Absence of the bad event is proven positively: the same subscriber receives a
// later, well-formed publish on that subject, and the rejected ref must not
// have arrived ahead of it (per-subject stream order means a stored one would).
func TestPublishRejectsCrossTenantRef(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f := newFabric(t, Config{})

	theirs, err := CommsSubject("tenant-b", KindMessagePosted)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}

	got := make(chan EventRef, 2)
	unsub, err := f.Subscribe(ctx, theirs, func(r EventRef) { got <- r })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()

	crossTenant := EventRef{Tenant: "tenant-a", Kind: KindMessagePosted, RowID: "r1"}
	err = f.Publish(ctx, theirs, crossTenant)
	if err == nil {
		t.Fatal("Publish of a tenant-a ref on tenant-b's subject: want an error, got nil")
	}
	// The message must name both sides, or an operator cannot tell which half
	// of the mismatch is the bug.
	if msg := err.Error(); !strings.Contains(msg, "tenant-a") || !strings.Contains(msg, "tenant-b") {
		t.Errorf("error %q must name both the ref's tenant and the subject's", msg)
	}

	want := EventRef{Tenant: "tenant-b", Kind: KindMessagePosted, RowID: "r2"}
	if err := f.Publish(ctx, theirs, want); err != nil {
		t.Fatalf("Publish of a matching ref: %v", err)
	}
	if delivered := recvRef(t, got); delivered != want {
		t.Fatalf("delivered %+v, want %+v — the cross-tenant ref was stored and delivered", delivered, want)
	}
}

// TestParkReasonIsSanitizedAndBounded defends the park path against the reason
// string it does not control. A subscriber panic embeds an arbitrary consumer
// value in the cause, and that cause goes onto the wire twice — a NATS header
// and the +TERM ack body — neither of which TermWithReason sanitizes or bounds.
// An unbounded or CR/LF-bearing reason could make the park itself fail, which is
// the worst place to fail: the poison message would keep redelivering.
func TestParkReasonIsSanitizedAndBounded(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	url := testServer(t)
	f := newFabric(t, Config{URL: url, MaxDeliver: 1, Log: quietLogger(t)})

	raw, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(raw.Close)
	dlq, err := raw.SubscribeSync(DLQSubject)
	if err != nil {
		t.Fatalf("SubscribeSync(%q): %v", DLQSubject, err)
	}
	if err := raw.FlushWithContext(ctx); err != nil {
		t.Fatalf("flushing the dlq subscription: %v", err)
	}

	subject, err := CommsSubject("t1", KindMessageUpdated)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}

	// A hostile panic value: CR/LF to corrupt the ack line and the header, and
	// multiple KB to blow any size bound. This is test code deliberately
	// driving the package's documented panic guard, as the DLQ tests above do.
	hostile := "line-one\r\nline-two " + strings.Repeat("x", 4096)
	unsub, err := f.Subscribe(ctx, subject, func(EventRef) { panic(hostile) })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()

	poison := EventRef{Tenant: "t1", Kind: KindMessageUpdated, RowID: "msg-hostile"}
	if err := f.Publish(ctx, subject, poison); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// The park still happening is half the invariant: a reason the wire rejects
	// would take the DLQ publish down with it.
	msg, err := dlq.NextMsgWithContext(ctx)
	if err != nil {
		t.Fatalf("waiting for the parked message on %q: %v", DLQSubject, err)
	}
	reason := msg.Header.Get(dlqHeaderReason)
	if reason == "" {
		t.Fatalf("park header %s is empty; an operator reading the dlq has no reason", dlqHeaderReason)
	}
	if strings.ContainsAny(reason, "\r\n") {
		t.Errorf("park reason %q contains CR/LF; it must be stripped before the header and the +TERM body", reason)
	}
	if len(reason) > maxParkReason {
		t.Errorf("park reason is %d bytes, want at most maxParkReason=%d", len(reason), maxParkReason)
	}
	// Bounding must not empty it out: the surviving prefix is what an operator
	// reads.
	if !strings.Contains(reason, "line-one") {
		t.Errorf("park reason %q lost the head of the cause; truncation must keep the prefix", reason)
	}
}

// TestUnsubscribeDrainsBufferedEvents defends the durability contract across
// teardown: the pull consumer prefetches, so at Unsubscribe there are events
// this instance has already CLAIMED but not yet run. Stopping discards them, and
// on a shared durable consumer a claimed-not-acked event only returns to anyone
// after AckWait (~30s) — a silent stall for events the fabric had accepted.
// Draining runs them through the callback and acks them instead.
//
// The buffer is established positively rather than by sleeping: the callback
// blocks on the first event, and the test waits for the server to report all n
// as delivered-unacked before tearing down, so all n are provably in this
// client's hands.
func TestUnsubscribeDrainsBufferedEvents(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f := newFabric(t, Config{})

	subject, err := CommsSubject("t1", KindMessagePosted)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}

	const n = 5
	published := make([]EventRef, 0, n)
	for i := range n {
		ref := EventRef{Tenant: "t1", Kind: KindMessagePosted, RowID: fmt.Sprintf("msg-%d", i)}
		if err := f.Publish(ctx, subject, ref); err != nil {
			t.Fatalf("Publish %s: %v", ref.RowID, err)
		}
		published = append(published, ref)
	}

	var (
		got     = make(chan EventRef, n)
		release = make(chan struct{})
		gateOne sync.Once
	)
	unsub, err := f.Subscribe(ctx, subject, func(r EventRef) {
		got <- r
		// Only the first delivery blocks; that is enough to let the rest pile
		// up in the consumer's buffer, which is what teardown must not discard.
		gateOne.Do(func() {
			select {
			case <-release:
			case <-time.After(gate):
			}
		})
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()

	// Wait for the server to have handed every event to this client. Polling
	// the consumer, not sleeping: the assertion below is only meaningful once
	// the buffer it defends actually exists.
	stream, err := f.ensureStream(ctx)
	if err != nil {
		t.Fatalf("ensureStream: %v", err)
	}
	cons, err := stream.Consumer(ctx, durableName(subject))
	if err != nil {
		t.Fatalf("Consumer(%q): %v", durableName(subject), err)
	}
	pollUntil(t, "all events claimed by the subscriber", func() bool {
		info, err := cons.Info(ctx)
		if err != nil {
			t.Fatalf("consumer Info: %v", err)
		}
		return info.NumAckPending == n
	})

	// Tear down with the buffer full, THEN let the blocked callback go: a
	// discarding teardown loses every buffered event, a draining one runs them.
	unsub()
	close(release)

	seen := make(map[string]int, n)
	for range n {
		seen[recvRef(t, got).RowID]++
	}
	for _, ref := range published {
		if seen[ref.RowID] != 1 {
			t.Errorf("%s was delivered %d time(s), want exactly 1 — Unsubscribe discarded a claimed event", ref.RowID, seen[ref.RowID])
		}
	}
}

// TestSubscribeWatchdogExitsOnClose defends Close as a complete shutdown of the
// event plane. The watchdog selects on the subscribe context, and nats.go closes
// neither a ConsumeContext's buffer nor its own subscription channel when the
// connection closes — so with an UNCANCELLED context (a Server whose root
// context outlives its fabric, the common shape) Close left the watchdog parked
// forever, holding a consumer on a dead connection.
//
// The context here is deliberately never cancelled: Close alone must do it.
func TestSubscribeWatchdogExitsOnClose(t *testing.T) {
	// Deliberately NOT parallel: this counts goroutines process-wide, and a
	// sibling test's live Subscribe is indistinguishable from a leak of this
	// one's. Go runs non-parallel tests while the parallel ones are paused.
	f := newFabric(t, Config{})

	subject, err := CommsSubject("t-watchdog", KindMessagePosted)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}

	// Every goroutine spawned on the subscribe path carries this in its stack,
	// so a residual watchdog shows up as a count that never falls back to
	// baseline. The watchdog lives in subscribeSubject, the body Subscribe and
	// SubscribeKind share, so this marker covers both entry points.
	const marker = "fabric.(*Fabric).subscribeSubject.func"
	baseline := countGoroutinesWith(t, marker)

	// Rooted at context.Background() because this is a test root, and an
	// uncancelled context is the whole point of the test.
	unsub, err := f.Subscribe(context.Background(), subject, func(EventRef) {})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()

	// Prove the watchdog started, so the assertion below distinguishes "exited"
	// from "never ran".
	pollUntil(t, "the subscribe watchdog to start", func() bool {
		return countGoroutinesWith(t, marker) > baseline
	})

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	pollUntil(t, "the subscribe watchdog to exit after Close", func() bool {
		return countGoroutinesWith(t, marker) <= baseline
	})
}

// countGoroutinesWith counts live goroutines whose stack mentions marker. Used
// instead of a goleak dependency, which this module does not carry: the whole
// assertion is one count, and adding a test-only dep for it is not worth it.
func countGoroutinesWith(t *testing.T, marker string) int {
	t.Helper()
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		// A full buffer means the dump was truncated and a goroutine may have
		// been cut off mid-frame, so the count would be wrong.
		if n < len(buf) {
			return strings.Count(string(buf[:n]), marker)
		}
		buf = make([]byte, 2*len(buf))
	}
}

// TestSubscribeKindReceivesEveryTenant is the load-bearing test for the
// tenant-wildcard subscribe. The T3 delivery consumer is a per-Server
// singleton serving every tenant, while each event is published on its own
// concrete compass.<tenant>.comms.<kind>; if the wildcard captured only some
// tenants, delivery for the rest would silently stop and only the cursor sweep
// would recover it. One SubscribeKind must see BOTH tenants' events with the
// tenant field intact — intact because the subscriber re-reads Postgres under
// that tenant, so a lost or wrong tenant is a cross-tenant read.
func TestSubscribeKindReceivesEveryTenant(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f := newFabric(t, Config{})

	got := make(chan EventRef, 4)
	unsub, err := f.SubscribeKind(ctx, KindMessagePosted, func(r EventRef) { got <- r })
	if err != nil {
		t.Fatalf("SubscribeKind: %v", err)
	}
	defer unsub()

	want := map[string]EventRef{
		"t1": {Tenant: "t1", Kind: KindMessagePosted, RowID: "msg-t1"},
		"t2": {Tenant: "t2", Kind: KindMessagePosted, RowID: "msg-t2"},
	}
	for tenant, ref := range want {
		subject, err := CommsSubject(tenant, KindMessagePosted)
		if err != nil {
			t.Fatalf("CommsSubject(%q): %v", tenant, err)
		}
		if err := f.Publish(ctx, subject, ref); err != nil {
			t.Fatalf("Publish for %q: %v", tenant, err)
		}
	}

	// Two distinct stream subjects, so their relative delivery order is not
	// guaranteed; collect both and compare as a set.
	seen := make(map[string]EventRef, len(want))
	for range want {
		ref := recvRef(t, got)
		if _, dup := seen[ref.Tenant]; dup {
			t.Fatalf("tenant %q delivered twice; got %+v", ref.Tenant, ref)
		}
		seen[ref.Tenant] = ref
	}
	for tenant, wantRef := range want {
		gotRef, ok := seen[tenant]
		if !ok {
			t.Fatalf("tenant %q never reached the wildcard subscriber (got %+v)", tenant, seen)
		}
		if gotRef != wantRef {
			t.Fatalf("tenant %q delivered %+v, want %+v", tenant, gotRef, wantRef)
		}
	}
}

// TestSubscribeKindIsolatesKinds defends the half of the subject that is NOT
// wildcarded. The stream captures compass.*.comms.*, so the consumer's
// FilterSubject is the only thing keeping the other six kinds out — and a
// delivery consumer woken for every topic_upsert would do a Postgres re-read
// per unrelated write.
//
// Absence is proven by a positive gate, not a sleep: the foreign-kind event is
// published and acked into the stream FIRST, so if the kind filter leaked it
// would already be stored and deliverable when the message_posted sentinel
// arrives.
func TestSubscribeKindIsolatesKinds(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f := newFabric(t, Config{})

	got := make(chan EventRef, 4)
	unsub, err := f.SubscribeKind(ctx, KindMessagePosted, func(r EventRef) { got <- r })
	if err != nil {
		t.Fatalf("SubscribeKind: %v", err)
	}
	defer unsub()

	other := EventRef{Tenant: "t1", Kind: KindTopicUpserted, RowID: "topic-1"}
	otherSubject, err := CommsSubject(other.Tenant, other.Kind)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}
	if err := f.Publish(ctx, otherSubject, other); err != nil {
		t.Fatalf("Publish the foreign kind: %v", err)
	}

	sentinel := EventRef{Tenant: "t2", Kind: KindMessagePosted, RowID: "msg-sentinel"}
	sentinelSubject, err := CommsSubject(sentinel.Tenant, sentinel.Kind)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}
	if err := f.Publish(ctx, sentinelSubject, sentinel); err != nil {
		t.Fatalf("Publish the sentinel: %v", err)
	}

	if delivered := recvRef(t, got); delivered != sentinel {
		t.Fatalf("delivered %+v, want the sentinel %+v — the wildcard leaked a %s event",
			delivered, sentinel, other.Kind)
	}
}

// TestSubscribeKindRejectsBadInput defends the wildcard entry point's own
// guards. An invalid kind must fail at the builder rather than reach
// CreateOrUpdateConsumer, and a nil callback must be refused rather than
// panicking on the first delivery — the same contract Subscribe has.
func TestSubscribeKindRejectsBadInput(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f := newFabric(t, Config{})

	if _, err := f.SubscribeKind(ctx, KindMessagePosted, nil); err == nil {
		t.Error("SubscribeKind with a nil callback = nil error, want a refusal")
	}
	if _, err := f.SubscribeKind(ctx, EventKind("bad.kind"), func(EventRef) {}); err == nil {
		t.Error("SubscribeKind with a reserved-character kind = nil error, want a refusal")
	}
	// A wildcard kind would put all seven comms kinds on one consumer.
	if _, err := f.SubscribeKind(ctx, EventKind("*"), func(EventRef) {}); err == nil {
		t.Error("SubscribeKind with a wildcard kind = nil error, want a refusal")
	}
}
