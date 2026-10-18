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
	"context"
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"

	otelx "github.com/RigelBuild/compass/go/internal/otel"
)

// RingCapacity bounds replay memory; a subscriber that falls further behind
// than this recovers by re-snapshotting at sinceSeq = 0. Exported so tests in
// other packages can derive an overrun count from it instead of hardcoding a
// literal that silently stops overrunning if this grows.
const RingCapacity = 1024

// liveBufferCapacity is the per-subscriber live-tail buffer depth. Matched to
// the ring so a subscriber lagging by less than the ring window can still
// recover its gap via sinceSeq replay after re-subscribing.
const liveBufferCapacity = RingCapacity

// Stamped is a published payload plus the ordering envelope the bus stamps onto
// it: the monotonic Seq, the wall-clock publish time, and the per-boot
// InstanceEpoch. The stream edge maps this onto the concrete compass.v1
// response message, so one bus backs every sequenced stream.
type Stamped[P any] struct {
	Seq           uint64
	AtUnixMS      int64
	InstanceEpoch uint64
	Payload       P
	// Traceparent is the W3C trace-context of the span active when the payload
	// was published (empty when published without an active span). It rides the
	// envelope so the delivery consumer can re-link dispatch to the publisher's
	// trace across the bus boundary.
	Traceparent string
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

	// HeadSeq is the highest seq on the ring at subscribe time, captured under
	// the same lock as Replay to provide a race-free snapshot boundary.
	// State-snapshot consumers use it for synthesized Changed events and discard
	// Replay; a fresh bus yields 0.
	HeadSeq uint64

	lagged *lagFlag

	// cancel removes the subscription's slot and closes Live, idempotently. nil
	// for terminal subscriptions after Close; defer it to free slots on disconnect.
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
		ring:          make([]Stamped[P], 0, RingCapacity),
	}
}

// Publish stamps the next seq, a timestamp, and ctx's traceparent (empty with no
// span), rings the event, and fans it out; it returns the assigned seq. A
// subscriber whose buffer is full is marked lagged and dropped; the ring keeps
// the event for a re-subscribe.
func (b *Bus[P]) Publish(ctx context.Context, payload P) uint64 {
	return b.publish(otelx.Traceparent(ctx), payload)
}

// publish stamps the next seq, a wall-clock timestamp, and traceparent onto
// payload, then fans out to live subscribers.
func (b *Bus[P]) publish(traceparent string, payload P) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	seq := b.nextSeq
	b.nextSeq++
	event := Stamped[P]{
		Seq:           seq,
		AtUnixMS:      epochMS(),
		InstanceEpoch: b.instanceEpoch,
		Payload:       payload,
		Traceparent:   traceparent,
	}

	if len(b.ring) == RingCapacity {
		copy(b.ring, b.ring[1:])
		b.ring[len(b.ring)-1] = event
	} else {
		b.ring = append(b.ring, event)
	}

	// Non-blocking sends bound each live buffer; a full buffer marks that
	// subscriber lagged and closes it, while the ring preserves replay.
	// The no-drop fast path leaves the slice untouched; on first drop, rebuild it
	// so the discarded backing array does not retain dead subscribers.
	var live []*subscriber[P] // nil until the first drop forces a rebuild
	for i, sub := range b.subscribers {
		drop := sub.lagged.get()
		if !drop {
			select {
			case sub.ch <- event:
			default:
				sub.lagged.set()
				sub.close()
				drop = true
			}
		}
		switch {
		case drop && live == nil:
			// First drop: seed the replacement with everyone kept so far.
			live = make([]*subscriber[P], 0, len(b.subscribers))
			live = append(live, b.subscribers[:i]...)
		case !drop && live != nil:
			live = append(live, sub)
		}
	}
	if live != nil {
		b.subscribers = live
	}

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

	// After Close, return an unregistered terminal stream. Check closed before
	// validating the cursor so even stale cursors end silently, as promised.
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

	// A positioned cursor must match this instance's epoch; otherwise its seq
	// space belongs to a prior boot (or the client never received an epoch).
	// Snapshots at sinceSeq=0 ignore the epoch.
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

// epochNonce mints a random per-boot epoch; unlike a timestamp it cannot repeat
// after a clock reset or VM snapshot restore. It retries on 0, the wire's
// "unset" sentinel.
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
