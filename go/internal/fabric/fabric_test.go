package fabric

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	natsserver "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// gate is the bound on every positive async assertion in this package. Long
// enough that a loaded CI box does not fail a correct implementation, short
// enough that a genuine hang fails the test rather than the suite's deadline.
// It is a FAILURE bound, never a wait: no test sleeps for delivery, and no test
// retries.
const gate = 10 * time.Second

// testServer runs an in-process NATS with JetStream for one test, and returns
// its client URL. Hermetic by construction: Port -1 picks a free port, so
// parallel tests never collide, and StoreDir under t.TempDir means the stream's
// file storage is discarded with the test.
//
// SyncInterval is the record's `sync_interval: 100ms`. It is a SERVER option,
// not a stream one (see SUBJECTS.md), so this is the only place in the repo the
// tests can exercise the record's value rather than the server default.
func testServer(t *testing.T) string {
	t.Helper()
	srv := natsserver.RunServer(&server.Options{
		Port:         -1,
		JetStream:    true,
		StoreDir:     t.TempDir(),
		SyncInterval: 100 * time.Millisecond,
		NoLog:        true,
		NoSigs:       true,
	})
	t.Cleanup(srv.Shutdown)
	return srv.ClientURL()
}

// newFabric returns a Fabric against a fresh in-process server, closed at test
// end. cfg.URL is filled in; every other field the caller sets is honored.
func newFabric(t *testing.T, cfg Config) *Fabric {
	t.Helper()
	if cfg.URL == "" {
		cfg.URL = testServer(t)
	}
	f, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return f
}

// testCtx is a context bounded by the gate, so a wedged test fails at the gate
// with its own message instead of at the package deadline. Rooted at
// context.Background() because this is a test root — the only place besides
// main() where minting a root context is correct.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), gate)
	t.Cleanup(cancel)
	return ctx
}

// recvRef takes the next EventRef from ch, failing at the gate.
func recvRef(t *testing.T, ch <-chan EventRef) EventRef {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(gate):
		t.Fatalf("no event within %s", gate)
		return EventRef{}
	}
}

// pollUntil blocks until cond reports true, failing at the gate. For the two
// states in this package that expose no event to block on — a goroutine count
// read out of runtime.Stack, and a JetStream consumer's server-side
// NumAckPending — there is no channel or WaitGroup to receive from, so a
// bounded poll is the only gate available. It is still a FAILURE bound and not
// a wait: it returns the moment cond holds, and a genuine hang fails the test
// with what it was waiting for.
func pollUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(gate)
	// A ticker rather than a sleep so the loop is driven by a channel the
	// deadline can race, and the failure is the gate's rather than the tick's.
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("waited %s for %s", gate, what)
			return
		}
	}
}

// TestCloseWaitsForDrain defends Close's documented promise that "a shutdown
// does not lose an already-published event". nc.Drain is ASYNCHRONOUS — it
// returns as soon as the connection enters DRAINING — so a Close that returned
// straight after it returned while the flush was still in flight, and the
// contract was a hope rather than a fact.
//
// Deterministic, no sleep: the connection reporting CLOSED is a state Close must
// already have observed by the time it returns.
func TestCloseWaitsForDrain(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f, err := New(Config{URL: testServer(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	subject, err := CommsSubject("t-drain", KindMessagePosted)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}
	if err := f.Publish(ctx, subject, EventRef{Tenant: "t-drain", Kind: KindMessagePosted, RowID: "m1"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Immediately, with no intervening wait: if Close returned mid-drain the
	// connection would still be DRAINING here.
	if !f.nc.IsClosed() {
		t.Fatal("Close returned while the connection was still draining; the flush it promises was not confirmed")
	}
}

// TestCloseIsPromptWithCallerClosedHandler defends Close's drain wait against a
// caller's own nats.ClosedHandler. Config.Options are appended AFTER the
// fabric's, and a nats.Option is just a mutator on nats.Options — so when the
// drain-completion signal came from a fabric-installed ClosedHandler, a caller
// adding one (a supported use: a monitoring hook) silently overwrote it, and
// every Close burned the full closeTimeout and then logged a false "drain did
// not complete" warning. Close now watches the connection's own status, which
// no option can clobber.
func TestCloseIsPromptWithCallerClosedHandler(t *testing.T) {
	t.Parallel()
	var callerRan atomic.Bool
	f, err := New(Config{
		URL: testServer(t),
		Options: []nats.Option{
			nats.ClosedHandler(func(_ *nats.Conn) { callerRan.Store(true) }),
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	start := time.Now()
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close must return well under closeTimeout (10s): if it depended on the
	// clobbered handler it would burn the full timeout. A generous ceiling
	// keeps the test non-flaky while still failing the 10s-stall bug.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Close took %s; expected prompt return (caller ClosedHandler disarmed the drain signal?)", elapsed)
	}
	// The caller's handler is NOT synchronous with Close: nc.close notifies
	// status listeners (which is what Close now waits on) before it pushes
	// ClosedCB onto the connection's async-callback dispatcher, so the handler
	// lands just after Close returns. pollUntil is the suite's FAILURE bound,
	// not a wait — a suppressed handler still fails, it just takes `gate`.
	pollUntil(t, "the caller's own ClosedHandler to run (the fabric must not suppress it)", callerRan.Load)
}

// TestNewRequiresURL defends the fail-closed constructor: a Fabric with no
// connection string must not come back as a usable object that fails later at
// an arbitrary Publish.
func TestNewRequiresURL(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}); err == nil {
		t.Fatal("New with no URL: want an error, got nil")
	}
}

// TestNewUnreachableURL defends that New actually connects. If it deferred the
// dial, a misconfigured NATS URL would surface as a mysterious publish failure
// at the first comms write instead of at startup.
func TestNewUnreachableURL(t *testing.T) {
	t.Parallel()
	// Port 1 on the loopback with no listener: connection refused, fast.
	if _, err := New(Config{URL: "nats://127.0.0.1:1", Options: []nats.Option{nats.Timeout(2 * time.Second)}}); err == nil {
		t.Fatal("New against an unreachable server: want an error, got nil")
	}
}

// TestFabricImplementsBothSeamsOverOneConnection defends the record's
// one-connection-per-party contract. That *Fabric satisfies both interfaces is
// a compile-time assertion in fabric.go; what a test can add is that the two
// seams are the SAME object over ONE nats.Conn — if EventFabric and
// RunnerFabric were ever split into two clients, each Runner and Server would
// hold two connections, which the record forbids.
func TestFabricImplementsBothSeamsOverOneConnection(t *testing.T) {
	t.Parallel()
	f := newFabric(t, Config{})
	var (
		ef EventFabric  = f
		rf RunnerFabric = f
	)
	efFabric, ok := ef.(*Fabric)
	if !ok {
		t.Fatalf("EventFabric is backed by %T, want *Fabric", ef)
	}
	rfFabric, ok := rf.(*Fabric)
	if !ok {
		t.Fatalf("RunnerFabric is backed by %T, want *Fabric", rf)
	}
	if efFabric != rfFabric {
		t.Fatal("the two seams must be one object, or a party holds two fabrics")
	}
	if efFabric.nc != rfFabric.nc {
		t.Fatal("the two seams must share one nats connection")
	}
}

// TestCloseIsIdempotentAndFailsClosed defends two things at once: Close can be
// called twice (a deferred Close beside an explicit one is not a bug), and
// post-Close work is refused rather than silently no-oping — a Publish that
// quietly succeeded after shutdown would look like a delivered event.
func TestCloseIsIdempotentAndFailsClosed(t *testing.T) {
	t.Parallel()
	f, err := New(Config{URL: testServer(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	ctx := testCtx(t)
	subject, err := CommsSubject("t-closed", KindMessagePosted)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}
	if err := f.Publish(ctx, subject, EventRef{Tenant: "t-closed", Kind: KindMessagePosted, RowID: "m1"}); !errors.Is(err, errClosed) {
		t.Fatalf("Publish after Close: want errClosed, got %v", err)
	}
	if _, err := f.Subscribe(ctx, subject, func(EventRef) {}); !errors.Is(err, errClosed) {
		t.Fatalf("Subscribe after Close: want errClosed, got %v", err)
	}
	if _, err := f.SubscribeKind(ctx, KindMessagePosted, func(EventRef) {}); !errors.Is(err, errClosed) {
		t.Fatalf("SubscribeKind after Close: want errClosed, got %v", err)
	}
	if err := f.SendCommand(ctx, "r1", nil); !errors.Is(err, errClosed) {
		t.Fatalf("SendCommand after Close: want errClosed, got %v", err)
	}
	if _, err := f.Events(ctx); !errors.Is(err, errClosed) {
		t.Fatalf("Events after Close: want errClosed, got %v", err)
	}
}

// TestDurableNameHasNoDots defends the JetStream naming constraint the
// subject→durable mapping exists for: a consumer name containing a "." is
// rejected by the server, so a subject passed through verbatim would fail every
// Subscribe.
func TestDurableNameHasNoDots(t *testing.T) {
	t.Parallel()
	subject, err := CommsSubject("tenant-a", KindMessagePosted)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}
	got := durableName(subject)
	if strings.ContainsAny(got, ".*> \t") {
		t.Fatalf("durableName(%q) = %q: contains a character JetStream forbids in a consumer name", subject, got)
	}
	// sha256("compass.tenant-a.comms.message_posted"), hex, behind the
	// greppable "comms-" prefix — see durableName's injectivity note.
	if want := "comms-f48b30590555bc3c1a67cfe133311032c5d8d7115e7d3b70fc89ae6156e25211"; got != want {
		t.Fatalf("durableName(%q) = %q, want %q", subject, got, want)
	}
}

// TestDurableNameIsInjectiveAcrossUnderscoreSplits defends the property the old
// "."→"_" substitution only claimed to have. ValidSubjectToken permits "_", so
// two distinct, individually-valid comms subjects collapsed to one durable
// name — and because Subscribe uses CreateOrUpdateConsumer, the second
// subscriber would silently re-point the first's shared consumer FilterSubject:
// cross-tenant mis-delivery.
func TestDurableNameIsInjectiveAcrossUnderscoreSplits(t *testing.T) {
	t.Parallel()
	// tenant "a" + kind "b_comms_c" vs tenant "a_comms_b" + kind "c".
	a := "compass.a.comms.b_comms_c"
	b := "compass.a_comms_b.comms.c"
	if durableName(a) == durableName(b) {
		t.Fatalf("durableName not injective: %q and %q both map to %q", a, b, durableName(a))
	}
}

// TestStreamConfigMatchesTheRecord defends the stream values §Q3 fixes: file
// storage (not memory — the whole point of putting comms on JetStream), the
// exact comms wildcard (a wider one would capture client.* traffic; a narrower
// one would silently drop a tenant), and limits retention (so a bounded replay
// stays possible).
func TestStreamConfigMatchesTheRecord(t *testing.T) {
	t.Parallel()
	cfg := Config{URL: "nats://ignored"}.streamConfig()
	if cfg.Name != DefaultStreamName {
		t.Errorf("stream name = %q, want %q", cfg.Name, DefaultStreamName)
	}
	if len(cfg.Subjects) != 1 || cfg.Subjects[0] != "compass.*.comms.*" {
		t.Errorf("stream subjects = %v, want [compass.*.comms.*]", cfg.Subjects)
	}
	if cfg.Storage != jetstream.FileStorage {
		t.Errorf("stream storage = %v, want file storage (durability is the reason comms rides JetStream)", cfg.Storage)
	}
	if cfg.Retention != jetstream.LimitsPolicy {
		t.Errorf("stream retention = %v, want limits", cfg.Retention)
	}
	if cfg.Duplicates <= 0 {
		t.Error("stream duplicate window must be positive, or WithMsgID dedup is a no-op")
	}
}

// TestConsumerConfigMatchesTheRecord defends the consumer values §Q3 fixes:
// explicit acks and a FINITE MaxDeliver. An unlimited MaxDeliver (the server
// default) would redeliver a poison message forever and the DLQ would never be
// reached.
func TestConsumerConfigMatchesTheRecord(t *testing.T) {
	t.Parallel()
	subject, err := CommsSubject("t1", KindMessagePosted)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}
	cfg := Config{}.consumerConfig(subject)
	if cfg.AckPolicy != jetstream.AckExplicitPolicy {
		t.Errorf("ack policy = %v, want explicit", cfg.AckPolicy)
	}
	if cfg.MaxDeliver <= 0 {
		t.Errorf("max deliver = %d, want a finite budget so a poison message parks", cfg.MaxDeliver)
	}
	if cfg.FilterSubject != subject {
		t.Errorf("filter subject = %q, want %q", cfg.FilterSubject, subject)
	}
	if cfg.Durable == "" {
		t.Error("consumer must be durable so instances share one consumer and a restart resumes")
	}
}

// quietLogger routes the fabric's diagnostics into the test log instead of
// stderr. Tests that deliberately drive a failure path (a poison message, an
// undecodable payload) are EXPECTED to log at error level; sending that to
// t.Log keeps a passing run clean while preserving the output on a failure.
func quietLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// testWriter adapts *testing.T to io.Writer for quietLogger.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("fabric log: %s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
