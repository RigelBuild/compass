package fabric

import (
	"context"
	"testing"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
)

// recvEvent takes the next RunnerEvent from ch, failing at the gate.
func recvEvent(t *testing.T, ch <-chan RunnerEvent) RunnerEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("the runner-events channel closed before delivering an event")
		}
		return ev
	case <-time.After(gate):
		t.Fatalf("no runner event within %s", gate)
		return RunnerEvent{}
	}
}

// TestSendCommandPublishesADecodableCommand defends the command plane's wire
// contract: what a Server sends must proto-unmarshal on the Runner side, on the
// Runner's OWN subject. A drift in either the encoding or the subject means the
// Runner never sees its commands — and, being best-effort core NATS, sees no
// error either.
func TestSendCommandPublishesADecodableCommand(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	url := testServer(t)
	f := newFabric(t, Config{URL: url})

	// A raw core-NATS subscriber stands in for the Runner, so the test asserts
	// the wire and not the fabric's own decoder.
	runner, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(runner.Close)

	subject, err := RunnerCommandSubject("runner-7")
	if err != nil {
		t.Fatalf("RunnerCommandSubject: %v", err)
	}
	sub, err := runner.SubscribeSync(subject)
	if err != nil {
		t.Fatalf("SubscribeSync(%q): %v", subject, err)
	}
	if err := runner.FlushWithContext(ctx); err != nil {
		t.Fatalf("flushing the runner subscription: %v", err)
	}

	want := &compassv1internal.SessionsResponse{
		RequestId: "req-1",
		Command: &compassv1internal.SessionsResponse_Stop{
			Stop: &compassv1.StopAgentSessionRequest{},
		},
	}
	if err := f.SendCommand(ctx, "runner-7", want); err != nil {
		t.Fatalf("SendCommand: %v", err)
	}

	msg, err := sub.NextMsgWithContext(ctx)
	if err != nil {
		t.Fatalf("waiting for the command on %q: %v", subject, err)
	}
	var got compassv1internal.SessionsResponse
	if err := proto.Unmarshal(msg.Data, &got); err != nil {
		t.Fatalf("the published command must proto-unmarshal: %v", err)
	}
	if got.GetRequestId() != want.GetRequestId() {
		t.Errorf("request id = %q, want %q", got.GetRequestId(), want.GetRequestId())
	}
	if got.GetStop() == nil {
		t.Error("the command oneof must survive the wire; GetStop() is nil")
	}
}

// TestSendCommandIsPerRunner defends the addressing itself: a command for one
// Runner must not land on another's subject. With several Runners on one NATS
// this is the only thing keeping commands from being executed by the wrong
// Runner.
func TestSendCommandIsPerRunner(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	url := testServer(t)
	f := newFabric(t, Config{URL: url})

	other, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(other.Close)

	mineSubject, err := RunnerCommandSubject("runner-mine")
	if err != nil {
		t.Fatalf("RunnerCommandSubject: %v", err)
	}
	theirsSubject, err := RunnerCommandSubject("runner-theirs")
	if err != nil {
		t.Fatalf("RunnerCommandSubject: %v", err)
	}
	mine, err := other.SubscribeSync(mineSubject)
	if err != nil {
		t.Fatalf("SubscribeSync(%q): %v", mineSubject, err)
	}
	theirs, err := other.SubscribeSync(theirsSubject)
	if err != nil {
		t.Fatalf("SubscribeSync(%q): %v", theirsSubject, err)
	}
	if err := other.FlushWithContext(ctx); err != nil {
		t.Fatalf("flushing subscriptions: %v", err)
	}

	if err := f.SendCommand(ctx, "runner-mine", &compassv1internal.SessionsResponse{RequestId: "req-mine"}); err != nil {
		t.Fatalf("SendCommand: %v", err)
	}
	if _, err := mine.NextMsgWithContext(ctx); err != nil {
		t.Fatalf("the addressed runner did not receive its command: %v", err)
	}
	// Core NATS delivers in order on one connection, and the flush above
	// established both interests before the publish, so if the command had
	// fanned out it would already be queued here.
	if n, _, err := theirs.Pending(); err != nil {
		t.Fatalf("reading the other runner's pending count: %v", err)
	} else if n != 0 {
		t.Fatalf("the other runner has %d pending message(s); commands must not fan out", n)
	}
}

// TestSendCommandRejectsInvalidInput defends the fail-closed command path. A nil
// command would publish an empty message the Runner cannot classify, and an
// invalid runner id would build a corrupted subject — both silent on a
// best-effort plane, so both must fail at the caller.
func TestSendCommandRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	f := newFabric(t, Config{})

	if err := f.SendCommand(ctx, "runner-1", nil); err == nil {
		t.Error("SendCommand with a nil command: want an error")
	}
	for _, id := range []string{"", "runner.1", "runner*", "runner>", "runner 1"} {
		if err := f.SendCommand(ctx, id, &compassv1internal.SessionsResponse{RequestId: "r"}); err == nil {
			t.Errorf("SendCommand to runner id %q: want an error", id)
		}
	}
}

// TestEventsYieldsDecodedRunnerEvents defends the fan-in wire contract in the
// other direction: a raw PublishEventsRequest published by a Runner must arrive
// as a RunnerEvent with every field intact. RunnerSeq is load-bearing (a gap is
// how the Server detects in-transit loss), and IdempotencyKey is what makes the
// durable commit at-most-once — losing either silently breaks a guarantee
// upstream.
func TestEventsYieldsDecodedRunnerEvents(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	url := testServer(t)
	f := newFabric(t, Config{URL: url})

	events, err := f.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	runner, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(runner.Close)

	want := &compassv1internal.PublishEventsRequest{
		RunnerSeq:      42,
		SessionId:      "sess-1",
		IdempotencyKey: "idem-1",
	}
	body, err := proto.Marshal(want)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	msg := nats.NewMsg(RunnerEventsSubject())
	msg.Data = body
	msg.Header.Set(RunnerIDHeader, "runner-7")
	if err := runner.PublishMsg(msg); err != nil {
		t.Fatalf("publishing a runner event: %v", err)
	}

	got := recvEvent(t, events)
	if got.RunnerID != "runner-7" {
		t.Errorf("runner id = %q, want %q (the fan-in subject is shared, so the header is the only attribution)", got.RunnerID, "runner-7")
	}
	if got.Event == nil {
		t.Fatal("RunnerEvent.Event must never be nil for an event read off the channel")
	}
	if got.Event.GetRunnerSeq() != want.GetRunnerSeq() {
		t.Errorf("runner seq = %d, want %d (gap detection depends on it)", got.Event.GetRunnerSeq(), want.GetRunnerSeq())
	}
	if got.Event.GetSessionId() != want.GetSessionId() {
		t.Errorf("session id = %q, want %q", got.Event.GetSessionId(), want.GetSessionId())
	}
	if got.Event.GetIdempotencyKey() != want.GetIdempotencyKey() {
		t.Errorf("idempotency key = %q, want %q (at-most-once commit depends on it)", got.Event.GetIdempotencyKey(), want.GetIdempotencyKey())
	}
}

// TestEventsClosesTheChannelOnContextDone defends the lifetime contract: a
// receiver ranging over the channel must see a clean close when the caller's
// context ends, not a goroutine that lives on holding a subscription. A
// never-closed channel is how a shutdown hangs.
func TestEventsClosesTheChannelOnContextDone(t *testing.T) {
	t.Parallel()
	f := newFabric(t, Config{})

	// Rooted at context.Background() because this is a test root.
	ctx, cancel := context.WithCancel(context.Background())
	events, err := f.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	cancel()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("want a closed channel after cancel, got an event")
		}
	case <-time.After(gate):
		t.Fatalf("the runner-events channel was not closed within %s of cancel", gate)
	}
}

// TestEventsChannelClosesOnClose is the other half of the lifetime contract,
// and the one the ctx test above cannot see: with an UNCANCELLED context —
// a Server whose root context outlives the fabric it closes, which is the
// ordinary shutdown shape — Close alone must close the channel.
//
// It could not, before: the pump selected only on ctx.Done() and the raw
// subscription channel, and nats.go closes neither a ChanSubscription's channel
// nor its buffer when the connection closes. So a consumer ranging over Events
// blocked forever at shutdown, with the pump goroutine leaked behind it — a hung
// process, not a slow one.
func TestEventsChannelClosesOnClose(t *testing.T) {
	t.Parallel()
	f, err := New(Config{URL: testServer(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Rooted at context.Background() because this is a test root, and its never
	// being cancelled is the invariant under test: Close is the only teardown.
	events, err := f.Events(context.Background())
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("want a closed channel after Close, got an event")
		}
	case <-time.After(gate):
		t.Fatalf("the runner-events channel was not closed within %s of Close; a ranging consumer would hang forever", gate)
	}
}

// TestEventsSkipsUndecodableMessages defends the best-effort plane's resilience:
// one malformed publish must not tear down the fan-in for every other Runner.
// The next valid event arriving is the gate.
func TestEventsSkipsUndecodableMessages(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	url := testServer(t)
	f := newFabric(t, Config{URL: url, Log: quietLogger(t)})

	events, err := f.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	runner, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(runner.Close)

	// Bytes that are not a valid PublishEventsRequest: a wire type of 7 is
	// invalid in protobuf, so this cannot be read as an empty message.
	if err := runner.Publish(RunnerEventsSubject(), []byte{0xFF, 0xFF, 0xFF}); err != nil {
		t.Fatalf("publishing a malformed event: %v", err)
	}
	body, err := proto.Marshal(&compassv1internal.PublishEventsRequest{RunnerSeq: 7, SessionId: "sess-ok"})
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	if err := runner.Publish(RunnerEventsSubject(), body); err != nil {
		t.Fatalf("publishing a valid event: %v", err)
	}

	got := recvEvent(t, events)
	if got.Event.GetSessionId() != "sess-ok" {
		t.Fatalf("session id = %q, want %q — the malformed message was not skipped cleanly", got.Event.GetSessionId(), "sess-ok")
	}
}

// TestEventsQueueGroupDeliversOnce defends the queue-group semantics the record
// requires for fan-in: with two Servers subscribed, exactly ONE handles each
// Runner event. Plain subscriptions would have both Servers process every event
// — a double write-through for every agent frame.
func TestEventsQueueGroupDeliversOnce(t *testing.T) {
	t.Parallel()
	ctx := testCtx(t)
	url := testServer(t)
	a := newFabric(t, Config{URL: url})
	b := newFabric(t, Config{URL: url})

	eventsA, err := a.Events(ctx)
	if err != nil {
		t.Fatalf("Events on a: %v", err)
	}
	eventsB, err := b.Events(ctx)
	if err != nil {
		t.Fatalf("Events on b: %v", err)
	}

	runner, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(runner.Close)

	const n = 20
	for i := range n {
		body, err := proto.Marshal(&compassv1internal.PublishEventsRequest{RunnerSeq: uint64(i), SessionId: "sess-1"})
		if err != nil {
			t.Fatalf("proto.Marshal: %v", err)
		}
		if err := runner.Publish(RunnerEventsSubject(), body); err != nil {
			t.Fatalf("publishing event %d: %v", i, err)
		}
	}

	// Gate on the exact expected count: n events published, n claims total
	// across both instances. A plain (non-queue) subscription would yield 2n
	// and this loop would see the extras.
	seen := make(map[uint64]int, n)
	for range n {
		var ev RunnerEvent
		select {
		case ev = <-eventsA:
		case ev = <-eventsB:
		case <-time.After(gate):
			t.Fatalf("only %d of %d events claimed within %s", len(seen), n, gate)
		}
		seen[ev.Event.GetRunnerSeq()]++
	}
	if len(seen) != n {
		t.Fatalf("claimed %d distinct sequences, want %d", len(seen), n)
	}
	for seq, count := range seen {
		if count != 1 {
			t.Errorf("sequence %d was claimed %d times, want exactly 1", seq, count)
		}
	}
	// No duplicate is still queued behind the n claims.
	select {
	case ev := <-eventsA:
		t.Fatalf("instance a claimed a duplicate of sequence %d", ev.Event.GetRunnerSeq())
	case ev := <-eventsB:
		t.Fatalf("instance b claimed a duplicate of sequence %d", ev.Event.GetRunnerSeq())
	default:
	}
}
