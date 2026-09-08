package fabric

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// bindingWatchdogMarker names every goroutine spawned on the
// SubscribeBindingChanges path. The routing seam has its OWN copy of the
// watchdog pattern rather than reusing subscribeSubject, so the sibling
// event-plane goroutine tests provably do not cover it: their marker is
// "fabric.(*Fabric).subscribeSubject.func", which never appears in this seam's
// stacks. A residual watchdog shows up as a count that never falls back to
// baseline.
const bindingWatchdogMarker = "fabric.(*Fabric).SubscribeBindingChanges.func"

// recvBindingChange takes the next BindingChange from ch, failing at the gate.
func recvBindingChange(t *testing.T, ch <-chan BindingChange) BindingChange {
	t.Helper()
	select {
	case b := <-ch:
		return b
	case <-time.After(gate):
		t.Fatalf("no binding change within %s", gate)
		return BindingChange{}
	}
}

// TestBindingChangesFanOutToEverySubscriber is the reason this seam exists at
// all. Every Server keeps its own binding cache, so an invalidation must reach
// ALL of them; if SubscribeBindingChanges ever joins a queue group, the server
// hands each change to exactly one instance and the rest go on serving a stale
// binding — with no error, no gap and no missing ack to reveal it. This is the
// only test that fails when that happens.
//
// Two Fabrics against one NATS, because two connections is what two Servers
// actually are — and a queue group partitions across connections, so a
// single-connection test would be a weaker guard.
func TestBindingChangesFanOutToEverySubscriber(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	url := testServer(t)
	first := newFabric(t, Config{URL: url})
	second := newFabric(t, Config{URL: url})

	firstCh := make(chan BindingChange, 1)
	unsubFirst, err := first.SubscribeBindingChanges(ctx, func(b BindingChange) { firstCh <- b })
	if err != nil {
		t.Fatalf("SubscribeBindingChanges (first server): %v", err)
	}
	defer unsubFirst()

	secondCh := make(chan BindingChange, 1)
	unsubSecond, err := second.SubscribeBindingChanges(ctx, func(b BindingChange) { secondCh <- b })
	if err != nil {
		t.Fatalf("SubscribeBindingChanges (second server): %v", err)
	}
	defer unsubSecond()

	want := BindingChange{Tenant: "tenant-a", SessionID: "sess-1", Op: BindingBound}
	if err := first.PublishBindingChange(ctx, "tenant-a", want); err != nil {
		t.Fatalf("PublishBindingChange: %v", err)
	}

	// Both are awaited concurrently rather than one after the other, and the
	// gate is shared: under a queue group the server picks ONE subscriber, and
	// which one is not deterministic — so a sequential receive would fail on
	// whichever channel happened to lose, with a bare timeout that does not
	// name the broken invariant. Counting arrivals fails the same way every
	// time, and says what went wrong.
	got := map[string]BindingChange{}
	deadline := time.After(gate)
	for range 2 {
		select {
		case b := <-firstCh:
			got["the publishing server"] = b
		case b := <-secondCh:
			got["the second server"] = b
		case <-deadline:
			t.Fatalf("only %d of 2 subscribers received the change within %s (received: %v); every Server caches bindings independently, so every Server must be invalidated — a queue group delivers to exactly one and leaves the rest serving a stale binding",
				len(got), gate, got)
		}
	}
	for who, b := range got {
		if b != want {
			t.Errorf("%s received %+v, want %+v", who, b, want)
		}
	}
}

// TestBindingSubjectIsNotCapturedByTheCommsStream defends the token order, on
// the degenerate tenant value that order exists for. compass.*.comms.* — the
// COMPASS_COMMS stream's capture — matches any four-token subject whose THIRD
// token is "comms", so the rejected grammar compass.routing.<tenant>.binding
// would put a tenant literally named "comms" INSIDE the JetStream comms stream:
// compass.routing.comms.binding. A stream is an ordinary subscriber in the
// account sublist, so that capture is ADDITIVE and the hubs would still receive
// the message; the harm is that a best-effort invalidation would ALSO be
// persisted into a durable stream specified to hold only EventRefs, burning its
// storage and MaxAge budget on traffic no comms consumer can use. That is why
// the assertion below is that the stream stayed EMPTY.
//
// Both directions are asserted, because a shared NATS makes cross-plane leakage
// possible either way: a binding change must not enter the stream, and a comms
// publish must not reach a binding subscriber.
func TestBindingSubjectIsNotCapturedByTheCommsStream(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	url := testServer(t)
	f := newFabric(t, Config{URL: url})

	// The stream must exist BEFORE the publish, or "the stream is empty" would
	// prove nothing.
	stream, err := f.ensureStream(ctx)
	if err != nil {
		t.Fatalf("ensureStream: %v", err)
	}

	// A raw core-NATS subscriber on the binding wildcard, so the isolation
	// assertion is about the wire and not the fabric's own dispatch.
	raw, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(raw.Close)
	bindings, err := raw.SubscribeSync(RoutingBindingWildcardSubject())
	if err != nil {
		t.Fatalf("SubscribeSync(%q): %v", RoutingBindingWildcardSubject(), err)
	}
	if err := raw.FlushWithContext(ctx); err != nil {
		t.Fatalf("flushing the binding subscription: %v", err)
	}

	// "comms" as a tenant id: legal (ValidSubjectToken permits it) and exactly
	// the value the token order defends against.
	const tenant = "comms"
	change := BindingChange{Tenant: tenant, SessionID: "sess-collide", Op: BindingUnbound}
	if err := f.PublishBindingChange(ctx, tenant, change); err != nil {
		t.Fatalf("PublishBindingChange: %v", err)
	}
	if _, err := bindings.NextMsgWithContext(ctx); err != nil {
		t.Fatalf("the binding subscriber did not receive its own plane's change: %v", err)
	}

	// PublishBindingChange flushed, so the server has already decided whether
	// the subject matched the stream — nothing is in flight.
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("stream Info: %v", err)
	}
	if info.State.Msgs != 0 {
		subject, serr := RoutingBindingSubject(tenant)
		if serr != nil {
			t.Fatalf("RoutingBindingSubject(%q): %v", tenant, serr)
		}
		t.Fatalf("%s holds %d message(s) after a binding change on %q; the core-NATS invalidation plane was captured by %q",
			f.cfg.streamName(), info.State.Msgs, subject, commsStreamSubjects)
	}

	// The other direction: a real comms publish for the same degenerate tenant
	// must not reach a binding subscriber.
	commsSubject, err := CommsSubject(tenant, KindMessagePosted)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}
	if err := f.Publish(ctx, commsSubject, EventRef{Tenant: tenant, Kind: KindMessagePosted, RowID: "m1"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// Publish is acked by the stream, and the binding subscription's interest
	// was established before it, so a leaked message would already be queued.
	if n, _, err := bindings.Pending(); err != nil {
		t.Fatalf("reading the binding subscription's pending count: %v", err)
	} else if n != 0 {
		t.Fatalf("the binding subscriber has %d pending message(s) after a comms publish on %q; the planes must not cross", n, commsSubject)
	}
	// Positive control for the count above: this comms publish MUST be in the
	// stream. Without it, "Msgs == 0" would also pass if the stream simply
	// never counted anything on this server, and the isolation assertion would
	// be vacuous.
	info, err = stream.Info(ctx)
	if err != nil {
		t.Fatalf("stream Info after the comms publish: %v", err)
	}
	if info.State.Msgs != 1 {
		t.Fatalf("%s holds %d message(s) after a comms publish on %q, want 1; the earlier empty-stream assertion proves nothing if the stream never captures",
			f.cfg.streamName(), info.State.Msgs, commsSubject)
	}
}

// TestBindingChangeIsTenantScoped defends the subject's tenant token. A Server
// resolving sessions for one tenant only — or an operator tapping one tenant's
// subject — must not see another tenant's invalidations, which would drop cache
// entries by a key from the wrong namespace.
func TestBindingChangeIsTenantScoped(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	url := testServer(t)
	f := newFabric(t, Config{URL: url})

	raw, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(raw.Close)

	subjectA, err := RoutingBindingSubject("tenant-a")
	if err != nil {
		t.Fatalf("RoutingBindingSubject: %v", err)
	}
	onlyA, err := raw.SubscribeSync(subjectA)
	if err != nil {
		t.Fatalf("SubscribeSync(%q): %v", subjectA, err)
	}
	// A wildcard subscriber alongside it, so the test gates on a POSITIVE
	// delivery rather than on the absence of one: tenant-b's change arriving
	// here is proof the publish happened at all.
	everyTenant, err := raw.SubscribeSync(RoutingBindingWildcardSubject())
	if err != nil {
		t.Fatalf("SubscribeSync(%q): %v", RoutingBindingWildcardSubject(), err)
	}
	if err := raw.FlushWithContext(ctx); err != nil {
		t.Fatalf("flushing subscriptions: %v", err)
	}

	changeB := BindingChange{Tenant: "tenant-b", SessionID: "sess-b", Op: BindingBound}
	if err := f.PublishBindingChange(ctx, "tenant-b", changeB); err != nil {
		t.Fatalf("PublishBindingChange: %v", err)
	}
	if _, err := everyTenant.NextMsgWithContext(ctx); err != nil {
		t.Fatalf("the wildcard subscriber did not receive tenant-b's change: %v", err)
	}
	if n, _, err := onlyA.Pending(); err != nil {
		t.Fatalf("reading tenant-a's pending count: %v", err)
	} else if n != 0 {
		t.Fatalf("tenant-a's subscriber has %d pending message(s); a binding change must not cross tenants", n)
	}
}

// TestBindingChangeRejectsInvalidInput defends the fail-closed publish path at
// both gates: the subject builder and PublishBindingChange itself. On a
// best-effort plane every one of these failures is otherwise silent — a
// corrupted subject publishes to a subject nobody watches, and a change naming
// no session invalidates cache key "".
func TestBindingChangeRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f := newFabric(t, Config{})

	for _, tenant := range []string{"", "tenant.a", "tenant*", "tenant>", "tenant a", "tenant\tb", "tenant\nc"} {
		if _, err := RoutingBindingSubject(tenant); err == nil {
			t.Errorf("RoutingBindingSubject(%q): want an error", tenant)
		}
		change := BindingChange{Tenant: tenant, SessionID: "sess-1", Op: BindingBound}
		if err := f.PublishBindingChange(ctx, tenant, change); err == nil {
			t.Errorf("PublishBindingChange for tenant %q: want an error", tenant)
		}
	}

	// A well-formed tenant with a payload that names nothing, or names a
	// nonsense op, must still fail.
	for _, change := range []BindingChange{
		{Tenant: "tenant-a", SessionID: "", Op: BindingBound},
		{Tenant: "tenant-a", SessionID: "sess-1", Op: ""},
		{Tenant: "tenant-a", SessionID: "sess-1", Op: "rebound"},
	} {
		if err := f.PublishBindingChange(ctx, "tenant-a", change); err == nil {
			t.Errorf("PublishBindingChange(%+v): want an error", change)
		}
	}

	// A payload whose tenant disagrees with the subject it is published on:
	// the read side is tenant-wildcard and its callback never sees the subject,
	// so the payload's tenant is the receiver's only scope.
	crossed := BindingChange{Tenant: "tenant-b", SessionID: "sess-1", Op: BindingBound}
	if err := f.PublishBindingChange(ctx, "tenant-a", crossed); err == nil {
		t.Error("PublishBindingChange with a payload tenant that differs from the subject tenant: want an error")
	}

	if _, err := f.SubscribeBindingChanges(ctx, nil); err == nil {
		t.Error("SubscribeBindingChanges with a nil callback: want an error")
	}
}

// TestMalformedBindingChangeIsDropped defends the plane's resilience where
// TestPoisonMessageParksOnDLQ defends JetStream's. There is no ack to withhold,
// no Nak to send and no DLQ to park on here, so one unparseable publish must be
// logged and dropped while the subscription keeps running — the next valid
// change arriving is the gate.
//
// The garbage goes out over a plain nats.Connect, because the fabric's own
// publish path cannot produce it: BindingChange.valid rejects it first. Which is
// the point — COMPASS's NATS carries no per-tenant authorization yet (OQ-3), so
// the fabric is not the only writer that can reach this subject.
func TestMalformedBindingChangeIsDropped(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	url := testServer(t)
	f := newFabric(t, Config{URL: url, Log: quietLogger(t)})

	got := make(chan BindingChange, 1)
	unsub, err := f.SubscribeBindingChanges(ctx, func(b BindingChange) { got <- b })
	if err != nil {
		t.Fatalf("SubscribeBindingChanges: %v", err)
	}
	defer unsub()

	writer, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(writer.Close)

	subject, err := RoutingBindingSubject("tenant-a")
	if err != nil {
		t.Fatalf("RoutingBindingSubject: %v", err)
	}
	// Two shapes of bad payload: bytes that are not JSON at all, and JSON that
	// decodes but names no session. The second is the dangerous one — it would
	// otherwise invalidate cache key "" and look like a successful delivery.
	for _, bad := range [][]byte{
		[]byte("{not json"),
		[]byte(`{"tenant":"tenant-a","session_id":"","op":"bound"}`),
	} {
		if err := writer.Publish(subject, bad); err != nil {
			t.Fatalf("publishing a malformed binding change: %v", err)
		}
	}
	// Flush the writer BEFORE the valid publish. Core NATS preserves order per
	// publisher only, and these are two connections — without this the
	// malformed bytes could still be in flight when the valid change is
	// delivered, so the assertion below would pass without the drop path having
	// run at all, and would keep passing if the rejection were deleted.
	if err := writer.FlushWithContext(ctx); err != nil {
		t.Fatalf("flushing the malformed publishes: %v", err)
	}

	want := BindingChange{Tenant: "tenant-a", SessionID: "sess-after", Op: BindingUnbound}
	if err := f.PublishBindingChange(ctx, "tenant-a", want); err != nil {
		t.Fatalf("PublishBindingChange: %v", err)
	}
	// Both malformed payloads are now known to the server, so if either were
	// surfaced instead of dropped it would be queued AHEAD of the valid change
	// and this assertion would name it.
	if received := recvBindingChange(t, got); received != want {
		t.Fatalf("received %+v, want %+v; a malformed payload must be dropped, not surfaced", received, want)
	}
}

// TestRoutingFabricFailsClosedAfterClose defends the fail-closed gate on the
// third seam, the same way TestCloseIsIdempotentAndFailsClosed does for the
// other two. A PublishBindingChange that silently no-oped after shutdown would
// look like a delivered invalidation, and on a plane with no ack there is
// nothing else that would ever contradict it.
func TestRoutingFabricFailsClosedAfterClose(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f, err := New(Config{URL: testServer(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	change := BindingChange{Tenant: "tenant-a", SessionID: "sess-1", Op: BindingBound}
	if err := f.PublishBindingChange(ctx, "tenant-a", change); !errors.Is(err, errClosed) {
		t.Errorf("PublishBindingChange after Close: want errClosed, got %v", err)
	}
	if _, err := f.SubscribeBindingChanges(ctx, func(BindingChange) {}); !errors.Is(err, errClosed) {
		t.Errorf("SubscribeBindingChanges after Close: want errClosed, got %v", err)
	}
}

// TestForgedTenantBindingChangeIsDropped defends the read side of the tenant
// invariant, and it is a cross-tenant cache attack that it closes.
// PublishBindingChange refuses a change whose payload tenant disagrees with the
// subject it publishes under, but the fabric is NOT the only writer that can
// reach this subject: COMPASS's NATS carries no per-tenant authorization yet
// (OQ-3), so any client that can reach the client port can publish any subject.
// This test bypasses the fabric with a raw connection exactly as a rogue
// publisher would.
//
// SubscribeBindingChanges is tenant-WILDCARD and its callback receives the
// decoded change and no subject, so b.Tenant is the receiver's only scope — a
// delivered mismatch is an instruction to drop another tenant's cache entry.
//
// The forged op is "unbound" deliberately: that is the dangerous one. A forged
// "bound" degrades to a spurious re-read that Postgres self-corrects, while an
// "unbound" is a stale NEGATIVE the receiver may drop outright without ever
// consulting the arbiter.
//
// Dropped, not parked: core NATS has no ack to withhold and no DLQ, which is
// the one difference from TestForgedTenantRefIsParked on the JetStream seam.
func TestForgedTenantBindingChangeIsDropped(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	url := testServer(t)
	f := newFabric(t, Config{URL: url, Log: quietLogger(t)})

	delivered := make(chan BindingChange, 2)
	unsub, err := f.SubscribeBindingChanges(ctx, func(b BindingChange) { delivered <- b })
	if err != nil {
		t.Fatalf("SubscribeBindingChanges: %v", err)
	}
	defer unsub()

	raw, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(raw.Close)

	// tenant-attacker's own subject carrying a tenant-victim payload. The
	// attacker needs no access to the victim's subject at all, which is why the
	// subject alone cannot be the scope.
	attackerSubject, err := RoutingBindingSubject("tenant-attacker")
	if err != nil {
		t.Fatalf("RoutingBindingSubject: %v", err)
	}
	forged, err := BindingChange{Tenant: "tenant-victim", SessionID: "sess-forged", Op: BindingUnbound}.encode()
	if err != nil {
		t.Fatalf("encoding the forged change: %v", err)
	}
	if err := raw.Publish(attackerSubject, forged); err != nil {
		t.Fatalf("raw Publish(%q): %v", attackerSubject, err)
	}
	// Flush the ATTACKER's connection before the legitimate publish. Core NATS
	// preserves order per publisher and these are two publishers, so without
	// this the forgery could still be in flight when the assertion below runs
	// and "it did not arrive" would mean "not yet". The flush round-trips a
	// PING, so once it returns the server has processed the forged publish and
	// already made its delivery decision for this subscription.
	if err := raw.FlushWithContext(ctx); err != nil {
		t.Fatalf("flushing the forged publish: %v", err)
	}

	// The positive gate on a negative assertion, and it doubles as proof the
	// drop did not kill the subscription: a mismatch is unprocessable, but it
	// must cost only its own message.
	good := BindingChange{Tenant: "tenant-attacker", SessionID: "sess-legit", Op: BindingBound}
	if err := f.PublishBindingChange(ctx, "tenant-attacker", good); err != nil {
		t.Fatalf("PublishBindingChange: %v", err)
	}

	if got := recvBindingChange(t, delivered); got != good {
		t.Fatalf("first delivery = %+v, want the legitimate %+v — a change claiming tenant %q was delivered from %q, and the receiver would drop that tenant's cache entry",
			got, good, "tenant-victim", attackerSubject)
	}
	select {
	case extra := <-delivered:
		t.Fatalf("a second change reached the subscriber: %+v, want only the legitimate one", extra)
	default:
	}
}

// TestUnrecognizedBindingOpIsDelivered defends the read side's DELIBERATE
// laxity about Op, which is not an oversight in decodeBindingChange but the
// forward-compatibility contract. A subscriber running older code than the
// publisher must still invalidate: dropping an op it does not recognize would
// leave a genuinely-changed binding cached as stale, and on a plane with no
// ack, no retry and no DLQ nothing would ever contradict it. "Invalidate and
// re-read" stays correct for every op this type can grow, so it is the only
// safe response to an unknown one.
//
// An EMPTY op is still rejected, and that is the line: absent is not unknown.
// An empty op is a payload that names no operation at all — the same class of
// nothing as an empty session id — and it is what a zero-valued or truncated
// publish produces rather than a newer peer.
//
// Both payloads go out raw, because the fabric's own publish path cannot
// produce either: BindingChange.valid rejects them first (see
// TestPublishBindingChangeRejectsAnUnrecognizedOp for that side).
func TestUnrecognizedBindingOpIsDelivered(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	url := testServer(t)
	f := newFabric(t, Config{URL: url, Log: quietLogger(t)})

	delivered := make(chan BindingChange, 2)
	unsub, err := f.SubscribeBindingChanges(ctx, func(b BindingChange) { delivered <- b })
	if err != nil {
		t.Fatalf("SubscribeBindingChanges: %v", err)
	}
	defer unsub()

	raw, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(raw.Close)

	subject, err := RoutingBindingSubject("tenant-a")
	if err != nil {
		t.Fatalf("RoutingBindingSubject: %v", err)
	}
	// Order matters and is guaranteed: both publishes share one connection, so
	// core NATS preserves their order. The empty-op payload goes FIRST, so if
	// it were wrongly delivered it would arrive ahead of the one this test
	// expects and the assertion names it rather than timing out.
	for _, payload := range []string{
		`{"tenant":"tenant-a","session_id":"sess-empty-op","op":""}`,
		`{"tenant":"tenant-a","session_id":"sess-newer","op":"resumed"}`,
	} {
		if err := raw.Publish(subject, []byte(payload)); err != nil {
			t.Fatalf("raw Publish(%q): %v", subject, err)
		}
	}

	// "resumed" is a value no BindingOp constant defines today, which is the
	// whole point: this is what a newer publisher looks like from here.
	want := BindingChange{Tenant: "tenant-a", SessionID: "sess-newer", Op: "resumed"}
	if got := recvBindingChange(t, delivered); got != want {
		t.Fatalf("first delivery = %+v, want %+v — an op this subscriber does not recognize must be carried through, and an empty one must not be",
			got, want)
	}
	select {
	case extra := <-delivered:
		t.Fatalf("a second change reached the subscriber: %+v; the empty-op payload must be dropped", extra)
	default:
	}
}

// TestPublishBindingChangeRejectsAnUnrecognizedOp pins the OTHER half of the
// asymmetry TestUnrecognizedBindingOpIsDelivered describes, and the asymmetry
// itself is the contract: lax on receive, strict on publish. A caller minting
// an op the type does not define is a bug in this process, and this is the only
// point on the whole plane where such a bug can be reported to someone holding
// a stack — after the publish there is no ack, no retry and no DLQ.
//
// So the strict closed-set check must survive on the publish side even though
// the receive side deliberately dropped it.
func TestPublishBindingChangeRejectsAnUnrecognizedOp(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f := newFabric(t, Config{})

	// The same value the receive side carries through, so the two tests read as
	// the pair they are.
	unknown := BindingChange{Tenant: "tenant-a", SessionID: "sess-newer", Op: "resumed"}
	if err := f.PublishBindingChange(ctx, "tenant-a", unknown); err == nil {
		t.Error("PublishBindingChange with an op no BindingOp constant defines: want an error; the closed-set check must stay on the publish side")
	}
	// The two ops that ARE defined must pass, or the check above would be
	// satisfied by a publish path that rejects everything.
	for _, op := range []BindingOp{BindingBound, BindingUnbound} {
		change := BindingChange{Tenant: "tenant-a", SessionID: "sess-ok", Op: op}
		if err := f.PublishBindingChange(ctx, "tenant-a", change); err != nil {
			t.Errorf("PublishBindingChange with op %q: %v", op, err)
		}
	}
}

// TestSubscribeBindingChangesWatchdogExitsOnClose defends Close as a complete
// shutdown of the ROUTING plane. This seam has its own watchdog goroutine
// rather than reusing subscribeSubject, so the event plane's
// TestSubscribeWatchdogExitsOnClose provably does not cover it — a different
// function, a different marker, a separately-written select.
//
// The context is deliberately never cancelled, which is the case f.teardown
// exists for: a Server whose root context outlives its fabric is the ORDINARY
// shutdown shape, and a watchdog selecting on ctx.Done() alone would park
// forever holding a subscription on a dead connection. Close must release it
// with no help from the caller's context.
func TestSubscribeBindingChangesWatchdogExitsOnClose(t *testing.T) {
	// Deliberately NOT parallel: this counts goroutines process-wide, and a
	// sibling test's live SubscribeBindingChanges is indistinguishable from a
	// leak of this one's. Go runs non-parallel tests while the parallel ones
	// are paused.
	//
	// New directly rather than newFabric: this test closes the fabric itself,
	// and newFabric's cleanup would then fail the test on the second Close.
	f, err := New(Config{URL: testServer(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	baseline := countGoroutinesWith(t, bindingWatchdogMarker)

	// Rooted at context.Background() because this is a test root, and an
	// UNCANCELLED context is the whole point of the test.
	unsub, err := f.SubscribeBindingChanges(context.Background(), func(BindingChange) {})
	if err != nil {
		t.Fatalf("SubscribeBindingChanges: %v", err)
	}
	defer unsub()

	// Prove the watchdog started, so the assertion below distinguishes "exited"
	// from "never ran".
	pollUntil(t, "the binding-change watchdog to start", func() bool {
		return countGoroutinesWith(t, bindingWatchdogMarker) > baseline
	})

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	pollUntil(t, "the binding-change watchdog to exit after Close", func() bool {
		return countGoroutinesWith(t, bindingWatchdogMarker) <= baseline
	})
}

// TestSubscribeBindingChangesStopsWhenContextIsDone defends the second of the
// three teardown paths into the same sync.Once: a subscription whose ctx is
// cancelled must tear itself down with no Unsubscribe call and no Close.
// Otherwise a Server shutdown that cancels its root context would leave a
// watchdog per subscription running against a connection it no longer owns.
func TestSubscribeBindingChangesStopsWhenContextIsDone(t *testing.T) {
	// Not parallel, for the same process-wide goroutine count as the Close
	// test above.
	f := newFabric(t, Config{})

	baseline := countGoroutinesWith(t, bindingWatchdogMarker)

	// Rooted at context.Background() because this is a test root; the fabric
	// stays open, so cancelling this context is the only teardown in play.
	subCtx, cancel := context.WithCancel(context.Background())
	unsub, err := f.SubscribeBindingChanges(subCtx, func(BindingChange) {})
	if err != nil {
		cancel()
		t.Fatalf("SubscribeBindingChanges: %v", err)
	}
	defer unsub()

	pollUntil(t, "the binding-change watchdog to start", func() bool {
		return countGoroutinesWith(t, bindingWatchdogMarker) > baseline
	})

	cancel()
	pollUntil(t, "the binding-change watchdog to exit after its context was cancelled", func() bool {
		return countGoroutinesWith(t, bindingWatchdogMarker) <= baseline
	})
}

// TestUnsubscribeBindingChangesStopsDelivery defends the third path — the
// caller's own Unsubscribe — and the sync.Once that makes all three safe.
//
// Two things are asserted, and the second is why the Once is there. A leaked
// subscription on this plane is quiet in a way a JetStream one is not: there is
// no consumer to hold and no ack to withhold, so a stale callback simply keeps
// running against cache entries its owner has abandoned. And a SECOND unsub()
// must be safe: the caller's defer commonly runs after ctx was already
// cancelled or the fabric closed, so a double teardown is the ordinary case,
// not an abuse — without the Once it would Drain a subscription twice and close
// an already-closed channel, which panics on the caller's goroutine.
//
// Absence of delivery is proven POSITIVELY: a second subscription on the same
// wildcard receives the change the stale callback must not have seen, so the
// test cannot pass merely because nothing was published.
func TestUnsubscribeBindingChangesStopsDelivery(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f := newFabric(t, Config{})

	var stale atomic.Int64
	first := make(chan BindingChange, 1)
	unsub, err := f.SubscribeBindingChanges(ctx, func(b BindingChange) {
		stale.Add(1)
		select {
		case first <- b:
		default:
		}
	})
	if err != nil {
		t.Fatalf("SubscribeBindingChanges: %v", err)
	}

	// Prove the subscription is live before tearing it down, so the assertion
	// below distinguishes "stopped" from "never started".
	warmup := BindingChange{Tenant: "tenant-a", SessionID: "sess-warmup", Op: BindingBound}
	if err := f.PublishBindingChange(ctx, "tenant-a", warmup); err != nil {
		t.Fatalf("PublishBindingChange warmup: %v", err)
	}
	if got := recvBindingChange(t, first); got != warmup {
		t.Fatalf("warmup delivered %+v, want %+v", got, warmup)
	}
	before := stale.Load()

	unsub()
	// Idempotent by contract: a second call must not double-Drain or re-close.
	unsub()

	second := make(chan BindingChange, 1)
	unsub2, err := f.SubscribeBindingChanges(ctx, func(b BindingChange) { second <- b })
	if err != nil {
		t.Fatalf("second SubscribeBindingChanges: %v", err)
	}
	defer unsub2()

	want := BindingChange{Tenant: "tenant-a", SessionID: "sess-after", Op: BindingUnbound}
	if err := f.PublishBindingChange(ctx, "tenant-a", want); err != nil {
		t.Fatalf("PublishBindingChange after unsubscribe: %v", err)
	}
	if got := recvBindingChange(t, second); got != want {
		t.Fatalf("second subscriber delivered %+v, want %+v", got, want)
	}
	// The live subscription received, so the unsubscribed one has demonstrably
	// had its chance; a delivery here would be a leak, not a race.
	if after := stale.Load(); after != before {
		t.Fatalf("the unsubscribed callback ran %d more time(s) after Unsubscribe", after-before)
	}
}

// TestInvokeBindingChangeConvertsPanicToError defends the guard in isolation.
// fn is consumer code that runs on the NATS DISPATCHER goroutine, so an
// unrecovered panic there is not a failed delivery — it is the process, and
// every other plane sharing this connection, gone over one bad cache entry.
// The panic must become an error the delivery path can log and drop.
func TestInvokeBindingChangeConvertsPanicToError(t *testing.T) {
	t.Parallel()
	b := BindingChange{Tenant: "tenant-a", SessionID: "sess-1", Op: BindingBound}

	if err := invokeBindingChange(func(BindingChange) {}, b); err != nil {
		t.Fatalf("a callback that returns normally must not error: %v", err)
	}

	err := invokeBindingChange(func(BindingChange) { panic(errors.New("boom")) }, b)
	if err == nil {
		t.Fatal("a panicking callback must yield an error, not a nil — the delivery path would otherwise log nothing and the panic would be invisible")
	}
	// The log line is the ONLY trace this message will ever leave: no ack, no
	// DLQ, no redelivery. So it has to name the session whose invalidation was
	// lost and the cause.
	if got := err.Error(); !strings.Contains(got, "sess-1") || !strings.Contains(got, "boom") {
		t.Errorf("error %q should name the session and the cause", got)
	}
}

// TestBindingSubscriberPanicDoesNotStopTheSubscription defends the guard's
// consequence for the plane: one broken subscriber must cost exactly its own
// message. The panicking callback runs on the dispatcher goroutine shared by
// every delivery on this subscription, so without the recover the first bad
// change would take down the process — and with a recover but no continuation
// it would still wedge the subscription and stop invalidating every OTHER
// session, which on a cache plane means serving stale bindings indefinitely.
//
// The gate is the next valid change arriving, which is the only observation
// that separates "recovered and carried on" from "recovered and died quietly".
func TestBindingSubscriberPanicDoesNotStopTheSubscription(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f := newFabric(t, Config{Log: quietLogger(t)})

	good := make(chan BindingChange, 1)
	unsub, err := f.SubscribeBindingChanges(ctx, func(b BindingChange) {
		if b.SessionID == "sess-poison" {
			panic("binding-change subscriber is broken")
		}
		good <- b
	})
	if err != nil {
		t.Fatalf("SubscribeBindingChanges: %v", err)
	}
	defer unsub()

	poison := BindingChange{Tenant: "tenant-a", SessionID: "sess-poison", Op: BindingUnbound}
	if err := f.PublishBindingChange(ctx, "tenant-a", poison); err != nil {
		t.Fatalf("PublishBindingChange poison: %v", err)
	}
	// One publisher, one connection: core NATS preserves order, so the poison
	// change is dispatched before this one. The good change arriving therefore
	// proves the dispatcher survived the panic rather than proving it raced
	// ahead of it.
	want := BindingChange{Tenant: "tenant-a", SessionID: "sess-ok", Op: BindingBound}
	if err := f.PublishBindingChange(ctx, "tenant-a", want); err != nil {
		t.Fatalf("PublishBindingChange good: %v", err)
	}
	if got := recvBindingChange(t, good); got != want {
		t.Fatalf("delivered %+v, want %+v — a panicking subscriber must not stop invalidation for every other session", got, want)
	}
}
