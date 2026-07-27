package events

// Tests for the server event bus. The first fifteen subtests pin the bus
// contract — monotonic seq, replay ordering, eviction, resync. The remaining
// subtests pin the fan-out contracts: lag/overrun, clean shutdown vs lag,
// HeadSeq, and the replay->live handoff.
//
// White-box (package events) so the eviction and overrun contracts can be
// stated in terms of ringCapacity / liveBufferCapacity rather than a magic 1024
// that would silently drift if the constant changed.

import (
	"testing"
	"time"
)

// testTimeout bounds every live-channel wait so a broken handoff fails fast
// instead of hanging the suite. It is a safety net, never a synchronization
// device: the tests event-gate on channel receives, not on elapsed time.
const testTimeout = 5 * time.Second

// ev is a minimal payload with a distinguishable variant tag, standing in for
// the proto oneof. kind lets the payload-preservation contract assert the
// bus keeps distinct variants distinct.
type ev struct {
	kind string
	n    int
}

func ready() ev { return ev{kind: "status"} }

// recvWithin returns the next event on ch, or (zero, false) if ch is closed.
// It fails the test if nothing arrives within testTimeout — an event gate with
// a deadline, not a sleep.
func recvWithin[P any](t *testing.T, ch <-chan Stamped[P]) (Stamped[P], bool) {
	t.Helper()
	select {
	case e, ok := <-ch:
		return e, ok
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for a live event")
		return Stamped[P]{}, false
	}
}

func seqsOf[P any](events []Stamped[P]) []uint64 {
	seqs := make([]uint64, len(events))
	for i, e := range events {
		seqs[i] = e.Seq
	}
	return seqs
}

func equalSeqs(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPublishAssignsMonotonicSeqsReplayedInOrder(t *testing.T) {
	bus := NewBus[ev]()
	bus.Publish(ready())
	bus.Publish(ready())
	bus.Publish(ready())

	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe(0,0): %v", err)
	}
	if got := seqsOf(sub.Replay); !equalSeqs(got, []uint64{1, 2, 3}) {
		t.Fatalf("replay seqs = %v, want [1 2 3]", got)
	}
}

func TestSubscribeWithCursorFiltersReplayToSeqsAboveIt(t *testing.T) {
	bus := NewBus[ev]()
	for range 5 {
		bus.Publish(ready())
	}
	sub, err := bus.Subscribe(3, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe(3): %v", err)
	}
	if got := seqsOf(sub.Replay); !equalSeqs(got, []uint64{4, 5}) {
		t.Fatalf("sinceSeq=3 replay = %v, want [4 5] (only seqs strictly above 3)", got)
	}
}

func TestSubscribeAtZeroSnapshotsTheWholeRing(t *testing.T) {
	bus := NewBus[ev]()
	for range 7 {
		bus.Publish(ready())
	}
	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe(0,0): %v", err)
	}
	if len(sub.Replay) != 7 {
		t.Fatalf("replay len = %d, want 7", len(sub.Replay))
	}
	if sub.Replay[0].Seq != 1 {
		t.Fatalf("first replay seq = %d, want 1", sub.Replay[0].Seq)
	}
	if last := sub.Replay[len(sub.Replay)-1].Seq; last != 7 {
		t.Fatalf("last replay seq = %d, want 7", last)
	}
}

func TestSubscribeBelowEvictedSpanUnderflows(t *testing.T) {
	bus := NewBus[ev]()
	for range ringCapacity + 10 {
		bus.Publish(ready())
	}
	// seqs 1..=10 were evicted; oldest retained is 11. A cursor of 5 wants
	// events from 6 onward, but 6..=10 are gone — the gap is unrecoverable.
	if _, err := bus.Subscribe(5, bus.InstanceEpoch()); err != ErrBufferUnderflow {
		t.Fatalf("Subscribe(5) below evicted span: err = %v, want ErrBufferUnderflow", err)
	}
}

func TestSubscribeAtEvictionBoundarySucceedsAndReplaysFromNewOldest(t *testing.T) {
	bus := NewBus[ev]()
	// One eviction: fill the ring, then push one more so seq 1 drops and the
	// oldest retained becomes seq 2.
	for range ringCapacity + 1 {
		bus.Publish(ready())
	}
	// cursor=1 wants events from 2 onward, which are exactly what remains — the
	// boundary case where oldest.seq (2) > sinceSeq+1 (2) is false.
	sub, err := bus.Subscribe(1, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe(1) at eviction boundary: %v", err)
	}
	if sub.Replay[0].Seq != 2 {
		t.Fatalf("first replay seq = %d, want 2 (new oldest)", sub.Replay[0].Seq)
	}
	if last := sub.Replay[len(sub.Replay)-1].Seq; last != uint64(ringCapacity+1) {
		t.Fatalf("last replay seq = %d, want %d", last, ringCapacity+1)
	}
}

func TestLiveTailDeliversEventsPublishedAfterSubscribe(t *testing.T) {
	bus := NewBus[ev]()
	bus.Publish(ready())

	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe(0,0): %v", err)
	}
	if len(sub.Replay) != 1 {
		t.Fatalf("replay len = %d, want 1 (the pre-subscribe event)", len(sub.Replay))
	}

	bus.Publish(ready())
	live, ok := recvWithin(t, sub.Live)
	if !ok {
		t.Fatal("live tail closed instead of delivering the post-subscribe event")
	}
	if live.Seq != 2 {
		t.Fatalf("live seq = %d, want 2 (the seq after the snapshot's)", live.Seq)
	}
}

func TestPublishStampsTimestampAndPreservesPayloadVariant(t *testing.T) {
	bus := NewBus[ev]()
	bus.Publish(ev{kind: "status"})
	bus.Publish(ev{kind: "resync"})

	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe(0,0): %v", err)
	}
	if sub.Replay[0].AtUnixMS <= 0 {
		t.Fatalf("first event AtUnixMS = %d, want > 0 (publish must stamp a timestamp)", sub.Replay[0].AtUnixMS)
	}
	if sub.Replay[0].Payload.kind != "status" {
		t.Fatalf("first payload kind = %q, want status", sub.Replay[0].Payload.kind)
	}
	// The second publish carried a different variant; the bus must preserve it
	// distinctly rather than coercing every event to one shape.
	if sub.Replay[1].Payload.kind != "resync" {
		t.Fatalf("second payload kind = %q, want resync", sub.Replay[1].Payload.kind)
	}
}

func TestSubscribeAtOrBeyondNextSeqUnderflows(t *testing.T) {
	bus := NewBus[ev]()
	bus.Publish(ready())
	bus.Publish(ready())
	bus.Publish(ready())
	// Three publishes leave nextSeq at 4; the highest cursor a caller can
	// legitimately hold is 3.
	if _, err := bus.Subscribe(4, bus.InstanceEpoch()); err != ErrBufferUnderflow {
		t.Fatalf("Subscribe(4) == nextSeq: err = %v, want ErrBufferUnderflow", err)
	}
	if _, err := bus.Subscribe(999, bus.InstanceEpoch()); err != ErrBufferUnderflow {
		t.Fatalf("Subscribe(999) beyond nextSeq: err = %v, want ErrBufferUnderflow", err)
	}
	// The boundary just below nextSeq is the highest valid cursor: every
	// retained seq is <= it, so there is nothing to replay — it tails live.
	sub, err := bus.Subscribe(3, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe(3) == nextSeq-1 should be valid: %v", err)
	}
	if len(sub.Replay) != 0 {
		t.Fatalf("replay len = %d, want 0 (highest valid cursor has nothing above it)", len(sub.Replay))
	}
}

func TestSubscribeOnEmptyBusRejectsSeqOneButAllowsSnapshotCursor(t *testing.T) {
	bus := NewBus[ev]()
	// No publishes: nextSeq is still 1, so the only in-range cursor is 0.
	if _, err := bus.Subscribe(1, bus.InstanceEpoch()); err != ErrBufferUnderflow {
		t.Fatalf("Subscribe(1) on empty bus: err = %v, want ErrBufferUnderflow", err)
	}
	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe(0,0) on empty bus should always be valid: %v", err)
	}
	if len(sub.Replay) != 0 {
		t.Fatalf("replay len = %d, want 0 (empty bus has nothing to snapshot)", len(sub.Replay))
	}
}

func TestPositionedCursorWithMismatchedEpochForcesResync(t *testing.T) {
	bus := NewBus[ev]()
	bus.Publish(ready())
	bus.Publish(ready())
	bus.Publish(ready())
	// nextSeq is 4 and the ring holds 1..=3, so cursor 1 is otherwise a
	// perfectly in-range positioned cursor. The epoch guard must still reject
	// it: a cursor from a prior instance names seqs in a seq space this boot
	// does not share.
	wrongEpoch := bus.InstanceEpoch() + 1
	if _, err := bus.Subscribe(1, wrongEpoch); err != ErrBufferUnderflow {
		t.Fatalf("Subscribe(1, wrongEpoch): err = %v, want ErrBufferUnderflow", err)
	}
}

func TestPositionedCursorWithUnsetEpochForcesResync(t *testing.T) {
	bus := NewBus[ev]()
	bus.Publish(ready())
	bus.Publish(ready())
	// Epoch 0 is the wire's "unset" — an old client that never observed an
	// instance epoch. A positioned cursor from such a client must resync.
	if _, err := bus.Subscribe(1, 0); err != ErrBufferUnderflow {
		t.Fatalf("Subscribe(1, 0): err = %v, want ErrBufferUnderflow", err)
	}
}

func TestPositionedCursorWithMatchingEpochTailsGapFree(t *testing.T) {
	bus := NewBus[ev]()
	for range 4 {
		bus.Publish(ready())
	}
	// Same cursor, stamped with this instance's live epoch: the guard passes
	// and the in-range cursor replays exactly the seqs above it.
	sub, err := bus.Subscribe(2, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe(2, epoch): %v", err)
	}
	if got := seqsOf(sub.Replay); !equalSeqs(got, []uint64{3, 4}) {
		t.Fatalf("matching-epoch replay = %v, want [3 4]", got)
	}
}

func TestSnapshotCursorIgnoresEpoch(t *testing.T) {
	bus := NewBus[ev]()
	for range 3 {
		bus.Publish(ready())
	}
	// A sinceSeq=0 snapshot re-reads the whole ring regardless of epoch, so the
	// epoch is never consulted — neither the unset 0 nor a foreign value can
	// turn a snapshot into a resync.
	if _, err := bus.Subscribe(0, 0); err != nil {
		t.Fatalf("Subscribe(0, 0): %v, want ok", err)
	}
	foreignEpoch := bus.InstanceEpoch() + 1
	if _, err := bus.Subscribe(0, foreignEpoch); err != nil {
		t.Fatalf("Subscribe(0, foreignEpoch): %v, want ok", err)
	}
}

func TestResponsesCarryTheInstanceEpoch(t *testing.T) {
	bus := NewBus[ev]()
	bus.Publish(ready())
	bus.Publish(ready())
	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe(0,0): %v", err)
	}
	if sub.Epoch != bus.InstanceEpoch() {
		t.Fatalf("sub.Epoch = %d, want %d", sub.Epoch, bus.InstanceEpoch())
	}
	for _, e := range sub.Replay {
		if e.InstanceEpoch != bus.InstanceEpoch() {
			t.Fatalf("replay event seq %d InstanceEpoch = %d, want %d", e.Seq, e.InstanceEpoch, bus.InstanceEpoch())
		}
	}
}

func TestBusIsGenericOverArbitraryPayloadTypes(t *testing.T) {
	// A payload type unrelated to the proto layer. Round-tripping it proves the
	// bus is genuinely generic, not hard-wired to one message type.
	type other struct{ v uint32 }

	bus := NewBus[other]()
	bus.Publish(other{v: 10})
	bus.Publish(other{v: 20})
	bus.Publish(other{v: 30})

	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe(0,0): %v", err)
	}
	if got := seqsOf(sub.Replay); !equalSeqs(got, []uint64{1, 2, 3}) {
		t.Fatalf("generic replay seqs = %v, want [1 2 3]", got)
	}
	want := []other{{v: 10}, {v: 20}, {v: 30}}
	for i, e := range sub.Replay {
		if e.Payload != want[i] {
			t.Fatalf("replay[%d].Payload = %+v, want %+v", i, e.Payload, want[i])
		}
		if e.AtUnixMS <= 0 {
			t.Fatalf("replay[%d].AtUnixMS = %d, want > 0", i, e.AtUnixMS)
		}
		if e.InstanceEpoch != bus.InstanceEpoch() {
			t.Fatalf("replay[%d].InstanceEpoch = %d, want %d", i, e.InstanceEpoch, bus.InstanceEpoch())
		}
	}

	// Live tail must deliver the arbitrary payload too, round-tripped by value.
	bus.Publish(other{v: 40})
	live, ok := recvWithin(t, sub.Live)
	if !ok {
		t.Fatal("live tail closed instead of delivering the post-subscribe generic event")
	}
	if live.Seq != 4 {
		t.Fatalf("live seq = %d, want 4", live.Seq)
	}
	if live.Payload != (other{v: 40}) {
		t.Fatalf("live payload = %+v, want {v:40}", live.Payload)
	}
}

// ---- Fan-out contracts: lag, overrun, and shutdown. ----

// A subscriber that fills its live buffer without draining overruns: the bus
// latches it lagged and closes its channel, distinct from a clean shutdown. The
// ring still retains the events, so a re-Subscribe within the window recovers.
func TestOverrunClosesLiveAndLatchesLagged(t *testing.T) {
	bus := NewBus[ev]()
	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe(0,0): %v", err)
	}

	// Fill the buffer (liveBufferCapacity accepted) then one more, which the
	// non-blocking fan-out cannot place — that publish marks the subscriber
	// lagged and closes its channel.
	for i := range liveBufferCapacity + 1 {
		bus.Publish(ev{n: i})
	}

	// Drain: the buffered events come through, then the channel is closed.
	delivered := 0
	for {
		_, ok := recvWithin(t, sub.Live)
		if !ok {
			break
		}
		delivered++
	}
	if delivered != liveBufferCapacity {
		t.Fatalf("delivered %d events before close, want %d (the full buffer)", delivered, liveBufferCapacity)
	}
	if !sub.Lagged() {
		t.Fatal("Lagged() = false after overrun, want true")
	}

	// The ring still holds the last ringCapacity events, so a re-subscribe
	// within the window recovers gap-free.
	recover, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("re-Subscribe(0,0) after lag: %v", err)
	}
	if len(recover.Replay) != ringCapacity {
		t.Fatalf("recovery replay len = %d, want %d (the retained ring)", len(recover.Replay), ringCapacity)
	}
	if last := recover.Replay[len(recover.Replay)-1].Seq; last != uint64(ringCapacity+1) {
		t.Fatalf("recovery last seq = %d, want %d", last, ringCapacity+1)
	}
}

// Close ends every open subscriber's Live channel WITHOUT the lag latch: a
// silent drain, not a resync. This is the discriminator the stream edge uses to
// end cleanly vs emit a terminal ResyncRequired.
func TestCloseEndsLiveSilentlyNotLagged(t *testing.T) {
	bus := NewBus[ev]()
	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe(0,0): %v", err)
	}

	bus.Close()

	if _, ok := recvWithin(t, sub.Live); ok {
		t.Fatal("Live delivered an event after Close, want closed channel")
	}
	if sub.Lagged() {
		t.Fatal("Lagged() = true after clean Close, want false (silent shutdown)")
	}

	// Close is idempotent.
	bus.Close()
}

func TestHeadSeqIsRingHeadAtSubscribeTime(t *testing.T) {
	t.Run("fresh bus yields 0", func(t *testing.T) {
		bus := NewBus[ev]()
		sub, err := bus.Subscribe(0, 0)
		if err != nil {
			t.Fatalf("Subscribe(0,0): %v", err)
		}
		if sub.HeadSeq != 0 {
			t.Fatalf("HeadSeq = %d on fresh bus, want 0", sub.HeadSeq)
		}
	})

	t.Run("equals nextSeq-1 after publishes", func(t *testing.T) {
		bus := NewBus[ev]()
		for range 5 {
			bus.Publish(ready())
		}
		snap, err := bus.Subscribe(0, 0)
		if err != nil {
			t.Fatalf("Subscribe(0,0): %v", err)
		}
		if snap.HeadSeq != 5 {
			t.Fatalf("snapshot HeadSeq = %d, want 5", snap.HeadSeq)
		}
		// HeadSeq is the ring head, independent of the cursor.
		tail, err := bus.Subscribe(3, bus.InstanceEpoch())
		if err != nil {
			t.Fatalf("Subscribe(3): %v", err)
		}
		if tail.HeadSeq != 5 {
			t.Fatalf("positioned HeadSeq = %d, want 5", tail.HeadSeq)
		}
	})
}

// ---- Subscription.Cancel and sticky-Close lifecycle contracts. ----

// Cancel removes the subscription's slot from the fan-out set and closes its
// Live channel; it is not an overrun, so Lagged stays false.
func TestCancelUnsubscribesAndClosesLive(t *testing.T) {
	bus := NewBus[ev]()
	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe(0,0): %v", err)
	}
	if got := len(bus.subscribers); got != 1 {
		t.Fatalf("subscribers = %d after Subscribe, want 1", got)
	}

	sub.Cancel()

	// Live is closed: a receive yields ok==false promptly.
	if _, ok := recvWithin(t, sub.Live); ok {
		t.Fatal("Live delivered an event after Cancel, want closed channel")
	}
	// The slot is gone from the fan-out set, so a later Publish never targets
	// it — the length dropped by one (white-box on the internal slice).
	if got := len(bus.subscribers); got != 0 {
		t.Fatalf("subscribers = %d after Cancel, want 0 (slot removed)", got)
	}
	// A subsequent Publish delivers to nobody and must not touch the cancelled
	// slot; it simply returns the next seq.
	if seq := bus.Publish(ready()); seq == 0 {
		t.Fatal("Publish returned seq 0")
	}
	// Cancel is a clean unsubscribe, not an overrun.
	if sub.Lagged() {
		t.Fatal("Lagged() = true after Cancel, want false (not an overrun)")
	}
}

// Cancel is idempotent: a second call — and a call after the bus itself closed
// the channel (here via Close) — is a safe no-op, never a double-close panic.
func TestCancelIsIdempotent(t *testing.T) {
	bus := NewBus[ev]()
	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe(0,0): %v", err)
	}

	sub.Cancel()
	sub.Cancel() // second call: no double-close panic, still a no-op.
	if got := len(bus.subscribers); got != 0 {
		t.Fatalf("subscribers = %d after double Cancel, want 0", got)
	}

	// Cancel after Close: the bus already closed this channel, so Cancel's
	// close() hits an already-fired sync.Once and stays safe.
	sub2, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("re-Subscribe(0,0): %v", err)
	}
	bus.Close()
	sub2.Cancel()
}

// After an overrun (Publish latched the subscriber lagged and closed its
// channel), Cancel is a safe no-op: the closeOnce has already fired and the
// slot is already dropped from the fan-out set.
func TestCancelAfterOverrunDoesNotPanic(t *testing.T) {
	bus := NewBus[ev]()
	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe(0,0): %v", err)
	}

	// Fill the buffer then one more: the last publish cannot place the event,
	// so it latches the subscriber lagged and closes its channel.
	for i := range liveBufferCapacity + 1 {
		bus.Publish(ev{n: i})
	}

	// Cancel after the overrun already closed the channel — a no-op, no panic.
	sub.Cancel()

	// Draining confirms the channel is closed (as the overrun left it).
	for {
		if _, ok := recvWithin(t, sub.Live); !ok {
			break
		}
	}
	if !sub.Lagged() {
		t.Fatal("Lagged() = false after overrun, want true")
	}
}

// After Close, Subscribe hands back a terminal stream: an already-closed Live,
// empty Replay, Lagged()==false, and a nil cancel so Cancel() is a no-op — a
// dead stream, not a registered subscriber.
func TestClosedBusSubscribeReturnsTerminalStream(t *testing.T) {
	bus := NewBus[ev]()
	bus.Publish(ready()) // ring holds an event, to prove Replay is still empty.
	bus.Close()

	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe(0,0) after Close: %v", err)
	}
	if _, ok := recvWithin(t, sub.Live); ok {
		t.Fatal("terminal Live delivered an event, want an already-closed channel")
	}
	if len(sub.Replay) != 0 {
		t.Fatalf("terminal Replay len = %d, want 0", len(sub.Replay))
	}
	if sub.Lagged() {
		t.Fatal("terminal Lagged() = true, want false")
	}
	// A terminal subscription carries a nil cancel: Cancel() is a no-op.
	sub.Cancel()
	// No subscriber was registered by the terminal Subscribe.
	if got := len(bus.subscribers); got != 0 {
		t.Fatalf("subscribers = %d after terminal Subscribe, want 0", got)
	}
}

// Close is sticky: every Subscribe after it returns a terminal (closed-Live)
// stream, not just the first.
func TestCloseIsStickyAcrossMultipleSubscribes(t *testing.T) {
	bus := NewBus[ev]()
	bus.Close()

	for i := range 3 {
		sub, err := bus.Subscribe(0, 0)
		if err != nil {
			t.Fatalf("Subscribe #%d after Close: %v", i, err)
		}
		if _, ok := recvWithin(t, sub.Live); ok {
			t.Fatalf("Subscribe #%d: Live delivered an event, want an already-closed channel", i)
		}
		if got := len(bus.subscribers); got != 0 {
			t.Fatalf("Subscribe #%d: subscribers = %d, want 0 (terminal, not registered)", i, got)
		}
	}
}

// The terminal-stream promise Close makes is unconditional: it does not exempt
// a positioned (nonzero) cursor. Subscribe checks b.closed first, ahead of
// every cursor gate (epoch, at/beyond nextSeq, below the evicted span), so a
// reconnecting client that hands a stale cursor to a shut-down server still
// gets one clean terminal stream (already-closed Live, empty Replay), not
// ErrBufferUnderflow forcing it to resync against a bus that is already dead.
// Each case below is a distinct invalid-cursor gate that, before the closed
// check was reordered ahead of them, wrongly won the ordering and underflowed;
// they now all resolve to the one terminal contract.
func TestClosedBusSubscribeWithInvalidCursorReturnsTerminalStream(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(bus *Bus[ev]) (sinceSeq, reqEpoch uint64)
	}{
		{
			// A positioned cursor from a prior boot: its epoch mismatches
			// this instance's — the epoch gate, an underflow on a live bus.
			name: "mismatched epoch",
			prepare: func(bus *Bus[ev]) (uint64, uint64) {
				bus.Publish(ready())
				bus.Publish(ready())
				bus.Publish(ready())
				return 1, bus.InstanceEpoch() + 1
			},
		},
		{
			// A cursor naming a seq the bus never emitted: three publishes
			// leave nextSeq at 4, so 8 trips the at/beyond-nextSeq gate.
			name: "at or beyond nextSeq",
			prepare: func(bus *Bus[ev]) (uint64, uint64) {
				bus.Publish(ready())
				bus.Publish(ready())
				bus.Publish(ready())
				return 8, bus.InstanceEpoch()
			},
		},
		{
			// A cursor whose replay span was evicted: seqs 1..=10 drop,
			// oldest retained is 11, so a cursor of 5 trips the evicted-span
			// gate.
			name: "below evicted span",
			prepare: func(bus *Bus[ev]) (uint64, uint64) {
				for range ringCapacity + 10 {
					bus.Publish(ready())
				}
				return 5, bus.InstanceEpoch()
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bus := NewBus[ev]()
			sinceSeq, reqEpoch := tc.prepare(bus)
			bus.Close()

			sub, err := bus.Subscribe(sinceSeq, reqEpoch)
			if err != nil {
				t.Fatalf("Subscribe(%d, %d) after Close: err = %v, want nil (terminal stream, not ErrBufferUnderflow)", sinceSeq, reqEpoch, err)
			}
			if _, ok := recvWithin(t, sub.Live); ok {
				t.Fatal("terminal Live delivered an event, want an already-closed channel")
			}
			if len(sub.Replay) != 0 {
				t.Fatalf("terminal Replay len = %d, want 0", len(sub.Replay))
			}
			if sub.Lagged() {
				t.Fatal("terminal Lagged() = true, want false")
			}
			// A terminal Subscribe registers no subscriber that Close would
			// otherwise never reach.
			if got := len(bus.subscribers); got != 0 {
				t.Fatalf("subscribers = %d after terminal Subscribe, want 0", got)
			}
		})
	}
}

// The replay->live handoff must lose and duplicate nothing across the boundary,
// even when publishes race the Subscribe call. Run under -race: registration
// and the ring snapshot happen under one lock, so every event lands in exactly
// one of Replay / Live.
func TestReplayLiveHandoffIsGapFreeUnderConcurrency(t *testing.T) {
	const n = 500 // < liveBufferCapacity, so the undrained live buffer never overruns
	bus := NewBus[ev]()

	done := make(chan struct{})
	go func() {
		for i := range n {
			bus.Publish(ev{n: i})
		}
		close(done)
	}()

	// Subscribe concurrently with the publisher, at an arbitrary interleaving.
	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("Subscribe(0,0): %v", err)
	}

	// Once the publisher has emitted all n, every event is either in Replay or
	// buffered on Live (n < buffer capacity, so none dropped).
	<-done

	seen := make(map[uint64]int, n)
	for _, e := range sub.Replay {
		seen[e.Seq]++
	}
	// Drain live until we have observed the final seq n; ordering guarantees no
	// gaps, so seeing n means we have seen the whole 1..n span.
	for seen[uint64(n)] == 0 {
		e, ok := recvWithin(t, sub.Live)
		if !ok {
			t.Fatal("Live closed before delivering the full 1..n span (event dropped across the handoff)")
		}
		seen[e.Seq]++
	}

	if len(seen) != n {
		t.Fatalf("observed %d distinct seqs, want %d", len(seen), n)
	}
	for s := uint64(1); s <= n; s++ {
		switch seen[s] {
		case 1:
			// exactly once — correct
		case 0:
			t.Fatalf("seq %d never delivered (dropped across the handoff)", s)
		default:
			t.Fatalf("seq %d delivered %d times (duplicated across the handoff)", s, seen[s])
		}
	}
}
