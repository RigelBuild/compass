package gateway

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
)

// Runner control producer.
//
// AgentGateway.Control is the Runner's one way to deliver control ops to an
// agent. The session lifecycle writes typed AgentControl messages through
// ControlSender; this file stamps each with a Runner-assigned monotonic
// control_seq, retains it until the agent acks, and drains it to whichever
// subscription is currently bound.
//
// Three invariants carry the contract:
//
//   - Ordering. Wire order is apply order: a single per-session queue drained
//     by one goroutine, so Send order is delivery order.
//   - Retention. Send success means "durably queued until acked", not "handed
//     to a socket". Ops live in the retention buffer past the agent's
//     ControlAck cursor, so an agent or container replacement loses nothing
//     and needs no durable store of its own.
//   - Replay barrier. Live ops are held while a replay is in flight and
//     released when the agent's ReplayCompleteAck arrives (routed here from
//     the Publish stream).

// ErrNoAgent is returned by SendIfLive when no subscription is live, so a
// caller that would rather fail than queue for an absent agent can do so.
var ErrNoAgent = errors.New("gateway: no live control subscription for session")

// controlLane is the full control lane: the ack-routing seam plus the
// subscription drain. ControlRouter stays deliberately narrow — it is the
// ack-routing contract the Publish side tests against with its own fake, so the
// Gateway's control field keeps that type and any ControlRouter remains
// injectable. Control (the server stream) additionally needs the drain, so it
// upgrades the injected router to this superset at use: *controlProducer
// satisfies it, the no-op default does not, and a Gateway wired with an
// ack-only router therefore refuses the subscription honestly rather than
// accepting a stream nothing can ever write to.
type controlLane interface {
	ControlRouter
	serve(ctx context.Context, sessionID string, sink controlSink) error
}

// errNoControlLane is returned by Control when the injected router carries no
// drain — an ack-only router (the no-op default, or a test fake) can route
// acks but cannot serve a subscription.
var errNoControlLane = errors.New("gateway: no control lane wired for subscription")

// errCloneFailed guards the proto.Clone type assertion in send. Clone always
// returns the concrete type it was given, so this is unreachable in practice —
// but an unchecked assertion here would panic the Runner rather than fail one
// control op.
var errCloneFailed = errors.New("gateway: control op clone produced the wrong type")

// errEmptyControlVariant is the must-not-send rule. SteerControl,
// DeliverControl, TranscriptReplay and ConfigControl are empty shells on the
// wire (their payloads are not yet defined), so sending one carries no
// information and would reach the agent only to be counted as unmapped.
// Rejecting at the seam keeps that impossible-by-construction.
var errEmptyControlVariant = errors.New("gateway: control variant has no representable payload")

// maxRetainedOps bounds one session's retention buffer. The Runner is the
// durable side of this seam — the agent keeps no store of its own — so ops live
// here until acked, and an agent that stops acking would otherwise grow this
// without limit until the Runner dies. Past the cap Send FAILS
// (CodeResourceExhausted) rather than evicting: an evicted op was already
// reported "durably queued until acked", so dropping it would break that
// promise silently, while a rejection tells the caller to back off. Sized well
// past any plausible in-flight burst, so hitting it means the agent is wedged,
// not busy.
const maxRetainedOps = 4096

// errRetentionFull is the cap's rejection. It is backpressure, not a wedge: an
// ack frees room immediately, and the replay barrier's own release is exempt
// from the cap so it stays reachable even when retention is full of held ops
// the agent has not been able to ack.
var errRetentionFull = errors.New("gateway: control retention buffer is full for session")

// errNoBoundSession is returned when a caller names a session the Runner never
// bound, or has already retired. Bind and Retire are the only lifetime, so an
// unknown id is genuinely absent rather than merely not-yet-created.
var errNoBoundSession = errors.New("gateway: no control session bound for id")

// ControlSender is the seam the session lifecycle uses to deliver a control op.
// Send stamps control_seq, retains the op for redelivery, and queues it for the
// stream goroutine to drain.
//
// Send does NOT mutate the caller's message: it retains a clone, so the caller
// keeps ownership of the op it passed and may reuse the pointer.
type ControlSender interface {
	Send(sessionID string, op *compassv1internal.AgentControl) error
}

// The producer is the implementation behind both seams: the lifecycle's sender
// and, once wired, the ack router plus this package's subscription drain.
// Asserted at compile time so a refactor breaks the build rather than the seam.
var (
	_ ControlSender = (*controlProducer)(nil)
	_ controlLane   = (*controlProducer)(nil)
)

// retained is one op held for redelivery until the agent acks it.
type retained struct {
	seq uint64
	op  *compassv1internal.AgentControl
}

// controlSession is the per-session control state: the seq counter, the
// retention buffer, the replay barrier, and the currently-bound subscription.
type controlSession struct {
	mu sync.Mutex

	nextSeq uint64
	ops     []retained // ascending by seq; pruned on ack

	// held is set while a replay is in flight. Live ops still enter retention
	// and keep their seq, but are not drained until the barrier releases — so
	// ordering survives the barrier. Replay-path ops are exempt: the barrier's
	// own release depends on the agent receiving replay_complete.
	held bool

	// sub is the live subscription generation. A takeover increments it, which
	// is how the displaced drainer learns it has been replaced.
	sub uint64
	// wake belongs to the CURRENT subscription, not to the session. A shared
	// channel is a lost-wakeup hazard: signal is a 1-slot edge-trigger, so a
	// displaced drainer parked on it can absorb the token owed to the live one
	// and return without re-signalling, stranding a retained op with no error
	// anywhere. Rebinding installs a fresh channel and closes the old one, so a
	// stale drainer is structurally incapable of taking the live drainer's wake
	// and is retired immediately rather than cooperatively.
	wake chan struct{}
	// live reports whether a subscription is currently bound. It is distinct
	// from sub, which only ever increases: after the first agent disconnects,
	// sub is nonzero forever, so a liveness test written against it silently
	// stops working exactly when a caller most wants to fail fast.
	live bool
	// retired is the tombstone Retire leaves on a session it has detached from
	// the map. serve resolves the session under p.mu and releases it before
	// taking s.mu to bind, so a Retire can delete the session in that window;
	// the resolved pointer still refers to the now-orphaned object. Binding on
	// it would revive live=true state nothing retires again and park a drainer
	// on a wake the completed Retire already closed. serve re-checks this under
	// s.mu at the bind and refuses, so a session torn down mid-resolve is never
	// bound. Set once and never cleared: a retired id is dead for good, and a
	// reused id gets a fresh controlSession from Bind.
	retired bool
	cursor  uint64 // highest contiguously-acked seq
}

// controlProducer owns every session's control state for one Runner.
type controlProducer struct {
	mu       sync.Mutex
	sessions map[string]*controlSession
	// onCycle is a test seam: nil in production, and when set it is called by
	// a drainer at the end of every send loop, immediately before it parks,
	// with the size of that drainer's own out-of-order set at that instant.
	// The set is a drainer local, so nothing else can observe it — a watermark
	// jump that strands entries into it is invisible to every delivery
	// assertion.
	//
	// Guarded by p.mu on both sides — setOnCycle writes it, cycleHook reads it
	// — rather than by a set-before-bind convention. A drainer captures it once
	// at bind time, so a late set is merely ineffective for the subscriptions
	// already running, not a data race.
	onCycle func(aboveLen int)
	// afterResolve is a test seam: nil in production, and when set it is called
	// by serve immediately after it resolves the session and BEFORE it takes
	// s.mu to bind. It exists to make the serve-vs-Retire interleaving
	// deterministic — a full Retire fired here lands squarely in the window
	// between the resolve and the bind. Guarded by p.mu like onCycle, and read
	// once per serve call, so a late set cannot race a bind already past it.
	afterResolve func()
}

func newControlProducer() *controlProducer {
	return &controlProducer{sessions: make(map[string]*controlSession)}
}

// representable reports whether the op carries a payload worth sending. The
// four parked variants are empty shells; everything else is representable.
func representable(op *compassv1internal.AgentControl) bool {
	switch op.GetControl().(type) {
	case *compassv1internal.AgentControl_Steer,
		*compassv1internal.AgentControl_Deliver,
		*compassv1internal.AgentControl_Replay,
		*compassv1internal.AgentControl_Config:
		return false
	case nil:
		return false
	default:
		return true
	}
}

// replayPath reports whether an op belongs to the replay sequence rather than
// to live traffic. The barrier holds LIVE ops only ("Replay frames first; live
// ops held until the agent's replay ack arrives"): the release is driven by the
// agent's ReplayCompleteAck, and the agent emits that only after RECEIVING
// replay_complete — so holding replay-path traffic behind the barrier would
// hold the one op whose ack is the only thing that can lift it, killing the
// session's control stream outright.
//
// TranscriptReplay is listed for when its payload is defined; it is an empty
// shell today, so `representable` rejects it before it can be sent.
func replayPath(op *compassv1internal.AgentControl) bool {
	switch op.GetControl().(type) {
	case *compassv1internal.AgentControl_ReplayComplete,
		*compassv1internal.AgentControl_Replay:
		return true
	default:
		return false
	}
}

// Send stamps, retains and queues an op. It succeeds whether or not a
// subscription is live: retention is what makes "queued until acked" true.
func (p *controlProducer) Send(sessionID string, op *compassv1internal.AgentControl) error {
	return p.send(sessionID, op, false)
}

// SendIfLive is Send for a caller that would rather fail than have its op sit
// unseen: it returns ErrNoAgent when no subscription is bound, instead of
// queueing for one that may never arrive.
//
// It is a LIVENESS gate, not a retention opt-out. An op accepted here is
// retained exactly as Send's is — the agent was live, so the op was genuinely
// handed over, and at-least-once still owes it redelivery if that agent dies
// before acking. The only thing this method decides is whether to fail when
// nobody is listening.
func (p *controlProducer) SendIfLive(sessionID string, op *compassv1internal.AgentControl) error {
	return p.send(sessionID, op, true)
}

// Retire drops a session's control state. It is Bind's counterpart, and the
// pair is the whole lifetime: every other retirement here is ack-driven or
// takeover-driven, which prunes ops WITHIN a session but never the session
// entry itself. The Runner reuses one container — and so one producer —
// across Stop/Start, minting a fresh session id each time, so
// without this the map grows by one controlSession per cycle for the life of
// the process, each pinning up to maxRetainedOps retained ops.
//
// Retiring a session with a live subscription closes its wake channel, which
// unparks the drainer; it observes the generation change and returns, exactly
// as it does on a takeover. Unknown or already-retired ids are a no-op, so this
// matches Stop's idempotent semantics.
func (p *controlProducer) Retire(sessionID string) {
	p.mu.Lock()
	s, ok := p.sessions[sessionID]
	delete(p.sessions, sessionID)
	p.mu.Unlock()
	if !ok {
		return
	}
	s.mu.Lock()
	// Advance the generation so a drainer that is mid-batch retires instead of
	// writing to a stream whose session no longer exists.
	s.sub++
	s.live = false
	// Tombstone the detached object so a serve that resolved this session
	// before the delete refuses the bind rather than reviving it.
	s.retired = true
	s.ops = nil
	if s.wake != nil {
		close(s.wake)
		s.wake = nil
	}
	s.mu.Unlock()
}

// Bind creates a session's control state. It is Retire's counterpart, and the
// pair is the whole lifetime: the Runner starts a session, so the Runner
// creates its state, and nothing else does.
//
// That is what lets every agent-driven path refuse an id it does not know
// rather than mint one. Creating on demand from the agent side cannot
// distinguish "not started yet" from "already retired", so a subscribe or an
// ack racing Stop rebuilt an entry nothing would ever retire again — the
// lifecycle spends its one Stop for that id, and the next cycle mints a fresh
// one. Ownership here makes that unrepresentable instead of guarded against.
//
// Idempotent: re-binding a live id is a no-op, so a retried Start cannot
// discard retained ops.
func (p *controlProducer) Bind(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.sessions[sessionID]; !ok {
		p.sessions[sessionID] = &controlSession{}
	}
}

// signal nudges the CURRENT subscription's drainer without blocking; the
// channel is a 1-slot edge-trigger, so a burst of Sends collapses into one
// wake. Callers must hold s.mu: the channel is swapped on rebind, and signaling
// a retired channel would be exactly the lost wakeup this design prevents.
func (s *controlSession) signalLocked() {
	if s.wake == nil {
		return // no subscription bound; a later bind drains from the cursor
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// AckControl retires retained ops through the contiguous cursor and drops any
// op individually applied above it.
//
// The ack comes from the agent, which is the untrusted side of this seam, so
// the cursor is clamped to the highest seq actually issued. An unclamped cursor
// past nextSeq would make every future op invisible to every future
// subscription — a silent, permanent wedge — because a subscription starts
// draining at the cursor.
//
// applied_above is untrusted the same way, and in the other direction: it is a
// repeated field the agent sizes. The proto calls it a small window but nothing
// enforces that, and the only ceiling on the wire is the 16MiB read cap — which
// packed varints turn into millions of entries, each amplified into a map slot.
// So the set is bounded here; see boundedAboveSet.
func (p *controlProducer) AckControl(sessionID string, ackedSeq uint64, appliedAbove []uint64) {
	s, ok := p.existingSession(sessionID)
	if !ok {
		return // retired: no retention to prune, no barrier to lift
	}

	// An ack that names nothing prunes nothing by relevance, and that is the
	// only shape production emits today — so the snapshot below is skipped
	// entirely rather than charged to every ControlAck frame. It costs a walk
	// of retention under s.mu, which is exclusive with every Send and with the
	// drainer, and the agent sets the frame rate.
	//
	// When there IS something to intersect: snapshot the retained seqs, then
	// scan the agent-sized field OUTSIDE the lock. The snapshot can only go
	// stale toward keeping a seq a concurrent ack has since retired, and the
	// prune below re-checks the live state, so a stale entry prunes nothing.
	// A seq appended after the snapshot is correctly absent: it did not exist
	// when the agent built this field, so this ack cannot have applied it.
	var above map[uint64]struct{}
	if len(appliedAbove) > 0 {
		s.mu.Lock()
		retainedSeqs := make(map[uint64]struct{}, len(s.ops))
		for _, r := range s.ops {
			retainedSeqs[r.seq] = struct{}{}
		}
		s.mu.Unlock()

		above = boundedAboveSet(appliedAbove, retainedSeqs)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if ackedSeq > s.nextSeq {
		ackedSeq = s.nextSeq
	}
	if ackedSeq > s.cursor {
		s.cursor = ackedSeq
	}

	kept := s.ops[:0]
	for _, r := range s.ops {
		if r.seq <= s.cursor {
			continue
		}
		if _, applied := above[r.seq]; applied {
			continue
		}
		kept = append(kept, r)
	}
	// The filter reuses the backing array, so the tail still holds pointers to
	// the pruned messages. Clear it or they stay reachable — and un-collectable
	// — for as long as the session lives.
	for i := len(kept); i < len(s.ops); i++ {
		s.ops[i] = retained{}
	}
	s.ops = kept
}

// boundedAboveSet builds the applied-above set, keeping only the seqs that can
// prune something and thereby bounding what one ack can allocate.
//
// The agent sizes this field and the proto specifies no ordering, so POSITION
// CARRIES NO MEANING: a positional bound — first N, or stop once N distinct
// are seen — keeps or drops in-window seqs according to how the agent happened
// to encode them. An agent listing its set ascending, the natural encoding,
// puts the seqs inside the live retention window LAST, so a positional bound
// discards exactly the real ones and keeps the noise.
//
// Relevance is the order-independent bound, and retention is what relevance
// means here: an entry prunes a retained op only if that op is retained. So
// the set is intersected against retention itself, not against the seq window
// (cursor, nextSeq] — that window is NOT bounded by retention and grows
// without limit over a session's life, because an agent that prunes purely by
// applied_above holds the contiguous cursor at 0 while nextSeq climbs, so
// retention never fills and the cap at Send never trips. Intersecting against
// the retained seqs caps the result at retention's own size, for any input of
// any size in any order, and drops nothing that could have pruned. Retention
// is what is Runner-driven here, not the constant: send exempts replay-path
// ops from maxRetainedOps so the barrier's own release stays reachable, so
// s.ops — and this set with it — can exceed the cap by that finite sequence.
func boundedAboveSet(appliedAbove []uint64, retainedSeqs map[uint64]struct{}) map[uint64]struct{} {
	above := make(map[uint64]struct{})
	for _, seq := range appliedAbove {
		if _, retained := retainedSeqs[seq]; retained {
			above[seq] = struct{}{}
		}
	}
	return above
}

// HoldForReplay raises the replay barrier: live ops queue and retain but are
// not drained until the agent's ReplayCompleteAck releases them.
//
// Raised by the session lifecycle when it restarts an agent into an existing
// session, immediately before it enqueues the transcript replay — so the agent
// sees its history before any live op that arrived while it was down. No
// production caller exists yet: the restart path lands with the socket
// cutover, and until then the barrier and the out-of-order set it produces are
// exercised only by tests. Not dead code; not yet reachable.
func (p *controlProducer) HoldForReplay(sessionID string) {
	s, ok := p.existingSession(sessionID)
	if !ok {
		return
	}
	s.mu.Lock()
	s.held = true
	s.mu.Unlock()
}

// ReleaseReplayBarrier is the ack-routing entry point for the replay barrier:
// the agent's ReplayCompleteAck arrived on Publish, so held live ops may flow.
//
// Agent-driven, so it must not create: see existingSession.
func (p *controlProducer) ReleaseReplayBarrier(sessionID string) {
	s, ok := p.existingSession(sessionID)
	if !ok {
		return
	}
	s.mu.Lock()
	s.held = false
	s.signalLocked()
	s.mu.Unlock()
}

// existingSession resolves a session without creating one, reporting whether
// it is still live state.
//
// The agent drives the ack and barrier entry points, and it decides WHEN to
// emit — so an ack can arrive after the lifecycle retired the session. Stop
// ends the agent's child process; it does not close the socket, the HTTP
// server, or the agent's Publish stream, so a frame already in flight lands
// after the retirement. Resolving that through the creating lookup would
// rebuild the entry, and nothing would ever retire it again: the lifecycle
// already ran its one Stop for that id and the next cycle mints a fresh one.
// The map would then grow by one resurrected session per Stop/Start — exactly
// the unbounded growth Retire was added to stop, reinstated by the untrusted
// side.
//
// An ack for a session that no longer exists is correctly a no-op: there is no
// retention left to prune and no barrier left to lift.
func (p *controlProducer) existingSession(sessionID string) (*controlSession, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[sessionID]
	return s, ok
}

// cycleHook reads the test seam under the lock, so a drainer captures it once
// at bind time instead of racing a concurrent write on every cycle.
func (p *controlProducer) cycleHook() func(int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.onCycle
}

// setOnCycle installs the test seam under the lock — the write side of the
// guarantee cycleHook's locked read makes.
func (p *controlProducer) setOnCycle(hook func(int)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onCycle = hook
}

// afterResolveHook reads the resolve-window test seam under the lock, matching
// cycleHook's discipline: serve captures it once so a concurrent set cannot
// race the read.
func (p *controlProducer) afterResolveHook() func() {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.afterResolve
}

// setAfterResolve installs the resolve-window seam under the lock.
func (p *controlProducer) setAfterResolve(hook func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.afterResolve = hook
}

// send is the shared body. requireLive is the SendIfLive gate: fail rather than
// queue when no subscription is bound. It does not affect retention — an op
// that clears the gate is retained like any other.
func (p *controlProducer) send(sessionID string, op *compassv1internal.AgentControl, requireLive bool) error {
	if !representable(op) {
		return connect.NewError(connect.CodeInvalidArgument, errEmptyControlVariant)
	}

	// Clone before stamping: the producer owns what it retains. Stamping the
	// caller's message in place would mutate an argument the caller still owns
	// AND race the drainer, which reads the retained pointer outside the lock;
	// worse, a caller reusing one pointer for two Sends would leave both
	// retention entries behind a single message carrying only the last seq.
	stamped, ok := proto.Clone(op).(*compassv1internal.AgentControl)
	if !ok {
		return connect.NewError(connect.CodeInternal, errCloneFailed)
	}

	s, ok := p.existingSession(sessionID)
	if !ok {
		return connect.NewError(connect.CodeNotFound, errNoBoundSession)
	}
	s.mu.Lock()
	if requireLive && !s.live {
		s.mu.Unlock()
		return ErrNoAgent
	}
	// Check the cap BEFORE stamping: a rejected Send must consume no seq and
	// leave no partial state, or the seq space gains a hole the agent's
	// contiguous ack cursor can never cross.
	//
	// Replay-path ops are exempt, for the same reason they bypass the hold
	// below. Under a raised barrier live ops are retained but NOT delivered, so
	// the agent can ack none of them and retention fills with undeliverable
	// work. Applying the cap to replay_complete there would reject the barrier's
	// own release: the agent never receives it, never emits ReplayCompleteAck,
	// and nothing can ever lower the barrier or free room — a permanent, silent
	// wedge.
	//
	// The exemption is bounded because the replay sequence is finite and
	// Runner-driven, not agent-driven. That bound is NOT structural, though: it
	// holds only while replay_complete is the sole representable replay-path
	// variant. replayPath also covers TranscriptReplay, which `representable`
	// rejects today as an empty shell — so defining its payload turns this into
	// an uncapped admission path. Whoever fills it in must bound replay-path
	// admission here.
	if len(s.ops) >= maxRetainedOps && !replayPath(stamped) {
		s.mu.Unlock()
		return connect.NewError(connect.CodeResourceExhausted, errRetentionFull)
	}

	s.nextSeq++
	stamped.ControlSeq = s.nextSeq
	s.ops = append(s.ops, retained{seq: s.nextSeq, op: stamped})
	if !s.held || replayPath(stamped) {
		s.signalLocked()
	}
	s.mu.Unlock()
	return nil
}

// controlSink is the stream side of a subscription — the connect ServerStream
// in production, a recorder in tests.
type controlSink interface {
	Send(op *compassv1internal.AgentControl) error
}

// absorbContiguous advances a subscription's contiguous watermark from `from`,
// consuming the run of seqs already delivered above it and removing them from
// the set. `above` holds seqs delivered out of order — past the watermark while
// the barrier held something lower — so the run drains as its gap fills, which
// is what keeps the set small and empties it once the barrier lifts.
//
// The walk is strictly upward, so it never examines an entry at or below
// `from`. That makes it correct ONLY for a contiguous advance. When the
// watermark JUMPS — an untrusted ack moves the cursor past seqs this
// subscription already recorded — those entries are stranded below it and no
// later walk can reach them; absorbJump is the entry point for that case.
func absorbContiguous(from uint64, above map[uint64]struct{}) uint64 {
	sent := from
	for {
		next := sent + 1
		if _, ok := above[next]; !ok {
			return sent
		}
		delete(above, next)
		sent = next
	}
}

// absorbJump advances the watermark to `from` across a GAP, dropping what the
// move leaves behind. Entries at or below the new watermark are delivered ops
// the agent can never ask for again, so retaining them is pure leak: the
// upward walk cannot reach them, and the guard that calls this does not
// re-fire once the watermark catches the cursor.
//
// The sweep is O(len(above)) and lives here rather than in absorbContiguous
// precisely because it is not needed on the contiguous path — folding it in
// would make steady-state delivery O(n) per op for no gain.
func absorbJump(from uint64, above map[uint64]struct{}) uint64 {
	for seq := range above {
		if seq <= from {
			delete(above, seq)
		}
	}
	return absorbContiguous(from, above)
}

// serve binds a subscription and drains the session's retained ops to it until
// the context ends or a takeover displaces it.
//
// Binding is a TAKEOVER: the generation counter increments and the predecessor's
// wake channel is CLOSED, which retires its drainer at its next park instead of
// leaving it to discover the change cooperatively. Each subscription owns its
// wake channel, so a lingering drainer can never absorb the token owed to the
// live one.
//
// Each subscription tracks its own high-water mark, starting at the ack cursor
// rather than at the newest op. That single choice gives both required
// behaviors from one mechanism: a fresh subscription re-sends everything past
// the cursor (transfer on takeover, redelivery on reconnect), while a
// steady-state subscription only ever advances, so a live agent never sees a
// duplicate it already received.
func (p *controlProducer) serve(ctx context.Context, sessionID string, sink controlSink) error {
	s, ok := p.existingSession(sessionID)
	if !ok {
		// The Runner never bound this id, or already retired it. Minting one
		// here cannot tell those apart, and either way nothing would retire
		// what this created. Refuse honestly: a stream to state the lifecycle
		// does not know is one nothing will ever write to.
		return connect.NewError(connect.CodeNotFound, errNoBoundSession)
	}

	// Captured once, under the producer lock, so the drainer never races a
	// concurrent write to the seam on its hot loop.
	onCycle := p.cycleHook()

	// Resolve-window seam (nil in production): fires after the session is
	// resolved and before the bind takes s.mu, the exact interleaving a
	// concurrent Retire can exploit. A test installs a Retire here to make
	// that race deterministic.
	if hook := p.afterResolveHook(); hook != nil {
		hook()
	}

	s.mu.Lock()
	// Re-check the tombstone under s.mu: a Retire may have deleted this session
	// from the map between the resolve above and this lock. Binding on a
	// detached object would revive live=true state nothing retires again and
	// park a drainer on a wake the completed Retire already closed — the
	// serve-vs-Retire leak. Refuse honestly, as the initial resolve does for an
	// id the Runner never bound.
	if s.retired {
		s.mu.Unlock()
		return connect.NewError(connect.CodeNotFound, errNoBoundSession)
	}
	s.sub++
	mine := s.sub
	// Retire the predecessor: closing its channel unparks its drainer at once,
	// and it owns that channel, so this cannot disturb the new subscription.
	if s.wake != nil {
		close(s.wake)
	}
	wake := make(chan struct{}, 1)
	s.wake = wake
	s.live = true
	// Start from the acked cursor: everything retained above it is owed to
	// this subscription, whether it was never sent or sent to a predecessor
	// that never acked.
	sent := s.cursor
	// above holds seqs delivered past `sent` while the barrier held a lower
	// live op. Per-subscription, like `sent`: a fresh subscription re-derives
	// both from the ack cursor.
	above := make(map[uint64]struct{})
	s.signalLocked()
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		// Only the CURRENT subscription clears liveness: a displaced drainer
		// exiting must not mark its replacement dead.
		if s.sub == mine {
			s.live = false
		}
		s.mu.Unlock()
	}()

	for {
		s.mu.Lock()
		if s.sub != mine {
			s.mu.Unlock()
			return nil // displaced by a takeover
		}
		// Advance past seqs retention no longer holds. The agent is untrusted
		// (see AckControl), so it may ack a seq it never received; that prunes
		// the op, and the gap at sent+1 can then never be filled by a delivery.
		// Without this, the absorb walk never fires again and every later seq
		// accumulates in `above` for the life of the subscription — growth
		// bounded by neither maxRetainedOps nor the ack cursor. Ops at or below
		// the cursor are gone for good, so skipping them loses nothing.
		//
		// absorbJump, not absorbContiguous: this is a JUMP, so entries `above`
		// already recorded at or below the new watermark are stranded by it.
		// The walk only steps upward, and this guard does not re-fire once
		// sent catches the cursor, so nothing else would ever drop them.
		if sent < s.cursor {
			sent = absorbJump(s.cursor, above)
		}
		// Binary-search the prefix rather than walking it. s.ops is ascending
		// by seq and pruned only from the front, so every entry at or below
		// `sent` is a contiguous prefix — and retention only shrinks on ack,
		// which the agent is under no obligation to do promptly. Walking it
		// would cost O(len(s.ops)) per cycle under the session mutex, so an
		// agent that receives without acking makes delivering n ops O(n²) and
		// stalls every concurrent Send behind the scan. That is the same rule
		// absorbJump's doc states one screen up: the untrusted side must not be
		// able to make a per-op cost scale.
		first, _ := slices.BinarySearchFunc(s.ops, sent, func(r retained, target uint64) int {
			return cmp.Compare(r.seq, target)
		})
		var batch []retained
		for _, r := range s.ops[first:] {
			if r.seq <= sent {
				continue // BinarySearch lands ON an equal seq when one is present
			}
			if _, done := above[r.seq]; done {
				continue
			}
			// The barrier holds LIVE ops only. Replay-path ops must pass it:
			// the release is the agent's ack of replay_complete, so holding
			// that op holds its own release. A live op arriving mid-replay
			// sits at a lower seq than the replay frames behind it, so this
			// SKIPS rather than stops — otherwise one held live op wedges the
			// whole replay sequence and the barrier never lifts.
			if s.held && !replayPath(r.op) {
				continue
			}
			batch = append(batch, r)
		}
		s.mu.Unlock()

		// Send OUTSIDE the lock, deliberately: sink.Send is network I/O, and
		// holding the session mutex across it would stall every Send and ack
		// for the session behind one slow socket. The cost is that an ack
		// landing mid-batch can leave an already-acked op in this snapshot, so
		// the agent may see a duplicate — which the at-least-once contract
		// covers via its seq-dedup. Do not "fix" that by widening the lock.
		for _, r := range batch {
			s.mu.Lock()
			displaced := s.sub != mine
			s.mu.Unlock()
			if displaced {
				return nil
			}
			if err := sink.Send(r.op); err != nil {
				return err
			}
			// Same cursor-plus-set shape the agent's ack uses: a contiguous
			// watermark, plus the seqs delivered above it while the barrier
			// held something lower. Absorb into the watermark as the gap fills
			// so the set stays small and empties once the barrier lifts.
			if r.seq == sent+1 {
				sent = absorbContiguous(r.seq, above)
				continue
			}
			above[r.seq] = struct{}{}
		}

		if onCycle != nil {
			onCycle(len(above))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
			// A closed channel yields immediately; the generation check at the
			// loop top then retires this drainer.
		}
	}
}

// Control registers the agent's subscription and drains the per-session send
// queue to the stream. A second Control call is a takeover: the stale
// subscription is cancelled, the new one bound, and all ops past the ack cursor
// transferred to it. Returns when the session ends or is taken over.
func (g *Gateway) Control(
	ctx context.Context,
	_ *connect.Request[compassv1internal.ControlSubscribeRequest],
	stream *connect.ServerStream[compassv1internal.AgentControl],
) error {
	sessionID, ok := g.sessions.Session(g.containerName)
	if !ok || sessionID == "" {
		return connect.NewError(connect.CodePermissionDenied, errNoSessionForContainer)
	}
	lane, ok := g.control.(controlLane)
	if !ok {
		return connect.NewError(connect.CodeUnimplemented, errNoControlLane)
	}
	return lane.serve(ctx, sessionID, stream)
}

// subscriptionGeneration reports the highest subscription generation bound so
// far. Monotonic WITHIN a session's lifetime, and NOT a liveness signal: it
// stays nonzero after the agent disconnects (`live` is the liveness flag). A
// displaced subscription is one whose generation is no longer current — that
// is how a takeover retires the stale drainer.
//
// An observer must not create what it observes, so an unknown or retired id
// reads 0 rather than minting a session — which is why the monotonicity is
// qualified: the count does not survive a Retire.
func (p *controlProducer) subscriptionGeneration(sessionID string) uint64 {
	s, ok := p.existingSession(sessionID)
	if !ok {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sub
}
