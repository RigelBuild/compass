// Package events is the server event bus: a monotonic-seq ring buffer plus a
// per-subscriber live tail, generic over the payload it carries. It has a single
// owner regardless of event type: one structure backs the server's
// SubscribeEvents and (with the networked Server tier) the comms SubscribeComms,
// so the gap-free-resubscribe contract (snapshot the ring at sinceSeq = 0,
// replay only what follows a cursor, and signal a resync when the cursor
// predates the ring) has one implementation.
//
// Fan-out uses a per-subscriber buffered channel plus an overrun flag rather
// than a broadcast primitive: a subscriber that fills its buffer is marked
// lagged and its channel closed; the reader distinguishes that overrun (emit a
// terminal ResyncRequired) from a clean bus shutdown via Subscription.Lagged.
package events

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

// ringCapacity bounds replay memory; a subscriber that falls further behind
// than this recovers by re-snapshotting at sinceSeq = 0.
const ringCapacity = 1024

// liveBufferCapacity is the per-subscriber live-tail buffer depth. Matched to
// the ring so a subscriber lagging by less than the ring window can still
// recover its gap via sinceSeq replay after re-subscribing.
const liveBufferCapacity = ringCapacity

// Stamped is a published payload plus the ordering envelope the bus stamps onto
// it: the monotonic Seq, the wall-clock publish time, and the per-boot
// InstanceEpoch. The stream edge maps this onto the concrete compass.v1
// response message, so one bus backs every sequenced stream.
type Stamped[P any] struct {
	Seq           uint64
	AtUnixMS      int64
	InstanceEpoch uint64
	Payload       P
}

// Subscription is the outcome of Bus.Subscribe: drain Replay (oldest first),
// then read Live for the rest of the session. The bus guarantees every Replay
// event carries a lower Seq than anything delivered on Live after the call
// returns — no event is delivered twice and none is dropped across the handoff.
//
// When Live closes, call Lagged to distinguish the two reasons: an overrun (the
// subscriber fell more than the ring window behind — emit a terminal
// ResyncRequired so the client re-snapshots) from a clean bus shutdown (end the
// stream silently). Lagged is safe to read only after Live has closed.
type Subscription[P any] struct {
	Replay []Stamped[P]
	Live   <-chan Stamped[P]

	// Epoch is the bus's instance epoch, stamped onto a synthesized resync so
	// every response the stream emits carries the current epoch.
	Epoch uint64

	// HeadSeq is the consistency-point seq: the highest seq on the ring at
	// subscribe time (nextSeq - 1), captured under the same lock that fills
	// Replay, so it is the true replay boundary with no post-subscribe re-read
	// race. A state-snapshot consumer (SubscribeComms at sinceSeq = 0) stamps
	// its synthesized *Changed events with this seq and discards Replay; a fresh
	// bus (nothing published) yields 0, the correct "tail from the first live
	// event" cursor.
	HeadSeq uint64

	lagged *lagFlag

	// cancel removes this subscription's slot from the bus and closes its live
	// channel, idempotently. nil on a terminal subscription handed back after
	// Close (nothing to cancel). Call it (typically deferred) when the stream
	// handler returns, so a client disconnect frees the fan-out slot instead of
	// leaking it until an overrun or bus shutdown.
	cancel func()
}

// Lagged reports whether the subscriber's Live channel closed because it
// overran its buffer (true) rather than because the bus shut down (false). Read
// it only after Live has closed.
func (s Subscription[P]) Lagged() bool {
	return s.lagged.get()
}

// Cancel unsubscribes the stream: it removes the subscription's slot from the
// bus and closes its live channel. Idempotent and safe to call after the bus
// has closed the channel itself (overrun or shutdown); a terminal subscription
// handed back after Close has nothing to cancel. Stream handlers should defer
// it so a client disconnect frees the fan-out slot promptly.
func (s Subscription[P]) Cancel() {
	if s.cancel != nil {
		s.cancel()
	}
}

// lagFlag is a one-way overrun latch shared between the bus (setter, under the
// publish lock) and the subscriber (reader, after Live closes).
type lagFlag struct {
	mu     sync.Mutex
	lagged bool
}

func (f *lagFlag) set() {
	f.mu.Lock()
	f.lagged = true
	f.mu.Unlock()
}

func (f *lagFlag) get() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lagged
}

// subscriber is one live-tail delivery endpoint the bus fans events out to. The
// id identifies it for removal by Subscription.Cancel; closeOnce guards its
// channel so the three close sites (Publish's lagged-drop, Close, Cancel) never
// double-close.
type subscriber[P any] struct {
	id        uint64
	ch        chan Stamped[P]
	lagged    *lagFlag
	closeOnce sync.Once
}

// close shuts the subscriber's live channel exactly once, whichever site
// (overrun, bus shutdown, or explicit cancel) reaches it first.
func (s *subscriber[P]) close() {
	s.closeOnce.Do(func() { close(s.ch) })
}

// Bus is a monotonic-seq ring buffer plus per-subscriber live tail, generic
// over the payload P it carries. Cheap to share by pointer; every method is
// safe for concurrent use.
type Bus[P any] struct {
	// instanceEpoch is a per-boot nonce (see epochNonce), minted once at
	// construction.
	instanceEpoch uint64

	mu          sync.Mutex
	nextSeq     uint64
	nextSubID   uint64
	ring        []Stamped[P]
	subscribers []*subscriber[P]
	// closed is sticky: once Close runs, every later Subscribe returns an
	// already-closed live channel (a terminal stream) instead of registering a
	// subscriber that would never be closed.
	closed bool
}

// NewBus constructs an empty bus with a fresh per-boot instance epoch.
func NewBus[P any]() *Bus[P] {
	return &Bus[P]{
		instanceEpoch: epochNonce(),
		nextSeq:       1,
		ring:          make([]Stamped[P], 0, ringCapacity),
	}
}

// Publish stamps the next seq and a wall-clock timestamp onto payload, pushes it
// into the ring (evicting the oldest at capacity), and forwards it to every live
// subscriber. Returns the seq assigned to this event. A subscriber whose buffer
// is full is marked lagged and dropped — the ring still retains the event, so a
// re-subscribe within the ring window recovers it.
func (b *Bus[P]) Publish(payload P) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	seq := b.nextSeq
	b.nextSeq++
	event := Stamped[P]{
		Seq:           seq,
		AtUnixMS:      epochMS(),
		InstanceEpoch: b.instanceEpoch,
		Payload:       payload,
	}

	if len(b.ring) == ringCapacity {
		copy(b.ring, b.ring[1:])
		b.ring[len(b.ring)-1] = event
	} else {
		b.ring = append(b.ring, event)
	}

	// Fan out to live subscribers. A non-blocking send bounds each subscriber's
	// buffer: if it is full the subscriber has fallen more than the ring window
	// behind, so latch it lagged and close its channel (the reader sees the
	// closed channel + the lag flag and emits a terminal resync). Retain only the
	// still-live subscribers.
	live := b.subscribers[:0]
	for _, sub := range b.subscribers {
		if sub.lagged.get() {
			continue
		}
		select {
		case sub.ch <- event:
			live = append(live, sub)
		default:
			sub.lagged.set()
			sub.close()
		}
	}
	// Zero out the dropped tail so retained-but-moved pointers don't leak.
	for i := len(live); i < len(b.subscribers); i++ {
		b.subscribers[i] = nil
	}
	b.subscribers = live

	return seq
}

// Subscribe opens a subscription for events after sinceSeq, matching the
// client's last-seen reqEpoch against this instance's. sinceSeq = 0 replays the
// whole retained ring (the snapshot) before tailing live.
//
// The live channel is registered before the ring is snapshotted, both under the
// lock: an event published once the lock releases lands on Live (subscriber
// already registered) and is absent from Replay (ring already snapshotted), so
// the replay→live handoff neither duplicates nor drops an event.
//
// On a closed bus it returns the terminal stream unconditionally (see Close),
// ahead of any cursor check. Otherwise it returns ErrBufferUnderflow when the
// cursor cannot be served by a gap-free replay: a positioned cursor whose epoch
// belongs to a prior server instance, a cursor at or beyond the next seq the
// bus would assign, or a non-zero cursor older than the oldest retained event.
func (b *Bus[P]) Subscribe(sinceSeq, reqEpoch uint64) (Subscription[P], error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// After Close, hand back a terminal stream: an already-closed live channel
	// (no lag latch) and an empty replay, so a late subscriber observes the
	// same silent end an open subscriber would on shutdown, and no subscriber
	// is registered that would otherwise never be closed. This closed check
	// precedes cursor validation: Close promises every later Subscribe a
	// terminal stream unconditionally, so a stale cursor handed to a shut-down
	// bus still gets one clean end, never ErrBufferUnderflow.
	if b.closed {
		dead := make(chan Stamped[P])
		close(dead)
		return Subscription[P]{
			Live:    dead,
			Epoch:   b.instanceEpoch,
			HeadSeq: b.nextSeq - 1,
			lagged:  &lagFlag{},
		}, nil
	}

	// A positioned cursor is only meaningful against the instance that issued
	// it. A mismatched epoch — including 0, an old client that never saw one —
	// means the cursor came from a prior server instance whose seq space this
	// one does not share (the bus resets to seq 1 each boot), so force a resync.
	// sinceSeq = 0 always re-snapshots, so the epoch is only consulted for a
	// positioned cursor.
	if sinceSeq > 0 && reqEpoch != b.instanceEpoch {
		return Subscription[P]{}, ErrBufferUnderflow
	}

	// Same-instance sanity: a cursor at or beyond the next seq we would assign
	// refers to an event we never emitted, so it can't be tailed gap-free.
	if sinceSeq >= b.nextSeq {
		return Subscription[P]{}, ErrBufferUnderflow
	}

	// A non-zero cursor older than the oldest retained event means the span
	// between sinceSeq and our oldest was already evicted — the caller can't be
	// caught up by replay.
	if sinceSeq > 0 && len(b.ring) > 0 && b.ring[0].Seq > sinceSeq+1 {
		return Subscription[P]{}, ErrBufferUnderflow
	}

	b.nextSubID++
	sub := &subscriber[P]{id: b.nextSubID, ch: make(chan Stamped[P], liveBufferCapacity), lagged: &lagFlag{}}
	b.subscribers = append(b.subscribers, sub)

	// The replay boundary, captured under this same lock: the highest seq the
	// ring holds (nextSeq - 1), or 0 on a fresh bus.
	headSeq := b.nextSeq - 1
	var replay []Stamped[P]
	for _, event := range b.ring {
		if event.Seq > sinceSeq {
			replay = append(replay, event)
		}
	}

	return Subscription[P]{
		Replay:  replay,
		Live:    sub.ch,
		Epoch:   b.instanceEpoch,
		HeadSeq: headSeq,
		lagged:  sub.lagged,
		cancel:  func() { b.unsubscribe(sub) },
	}, nil
}

// Close shuts the bus down: it latches the sticky closed state, then closes
// every open subscriber's Live channel without the lag latch set, so readers
// end their streams silently (a clean drain, distinct from an overrun). Once
// closed, a later Subscribe returns an already-closed live channel rather than
// registering a subscriber that would never be closed. Idempotent.
func (b *Bus[P]) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for _, sub := range b.subscribers {
		sub.close()
	}
	b.subscribers = nil
}

// unsubscribe removes sub from the fan-out set and closes its live channel,
// idempotently: a subscriber already dropped (by an overrun in Publish or by
// Close) is simply absent, and its channel's close is guarded by a sync.Once,
// so a deferred Cancel after either is a safe no-op. Backs Subscription.Cancel.
func (b *Bus[P]) unsubscribe(sub *subscriber[P]) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, s := range b.subscribers {
		if s.id == sub.id {
			b.subscribers = append(b.subscribers[:i], b.subscribers[i+1:]...)
			break
		}
	}
	sub.close()
}

// InstanceEpoch is the per-boot instance epoch stamped on every response and
// matched against a reconnecting client's cursor. Distinct across restarts;
// never 0.
func (b *Bus[P]) InstanceEpoch() uint64 {
	return b.instanceEpoch
}

// epochMS is wall-clock time as Unix epoch milliseconds, stamped onto every
// event.
func epochMS() int64 {
	return time.Now().UnixMilli()
}

// epochNonce mints a per-boot instance epoch: a random uint64 from the OS,
// minted once at server start. A reconnecting client echoes it back so the
// server can tell a live cursor from a prior instance's; the guard only needs
// the value to differ across restarts, which a random nonce gives with
// negligible (~2⁻⁶⁴) collision odds — unlike a boot timestamp, which can repeat
// under a coarse clock, a clock reset, or VM snapshot/restore. Retried to never
// yield 0, since 0 is the "unset" sentinel on the wire (an old client that never
// received one).
func epochNonce() uint64 {
	var buf [8]byte
	for {
		if _, err := rand.Read(buf[:]); err != nil {
			// crypto/rand should never fail; if it does, a time-derived value
			// keeps the server starting rather than panicking on boot.
			return uint64(time.Now().UnixNano()) | 1
		}
		if n := binary.LittleEndian.Uint64(buf[:]); n != 0 {
			return n
		}
	}
}
