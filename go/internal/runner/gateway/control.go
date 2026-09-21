package gateway

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// Runner control producer. AgentGateway.Control is the Runner's one way to deliver
// control ops to an agent: it stamps each with a monotonic control_seq, retains it until
// acked, and drains it to the bound subscription. Three invariants — ordering (one queue,
// one goroutine), retention (durably queued until acked), and a replay barrier.

// ErrNoAgent is returned by SendIfLive when no subscription is live, so a
// caller that would rather fail than queue for an absent agent can do so.
var ErrNoAgent = errors.New("gateway: no live control subscription for session")

// controlLane is the full control lane: the ack-routing seam plus the subscription drain.
// ControlRouter stays narrow (the Publish side tests it with a fake); Control also needs
// the drain, so it upgrades the injected router to this superset at use. An ack-only
// router refuses the subscription rather than accepting an unwritable stream.
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

// errEmptyControlVariant is the must-not-send rule. TranscriptReplay and ConfigControl
// are empty shells on the wire, so sending one carries no information and would reach
// the agent only to be counted as unmapped. SteerControl/DeliverControl each carry a
// defined compass.v1.Message, so both are representable and sent.
var errEmptyControlVariant = errors.New("gateway: control variant has no representable payload")

// maxRetainedOps bounds one session's retention buffer. The Runner is the durable side,
// so ops live here until acked; an agent that stops acking would otherwise grow this
// without limit. Past the cap Send FAILS (CodeResourceExhausted) rather than evicting a
// durably-queued op. Sized well past any plausible in-flight burst.
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
	// wake belongs to the CURRENT subscription, not the session. A shared channel is a
	// lost-wakeup hazard: signal is a 1-slot edge-trigger, so a displaced drainer could
	// absorb the live one's token. Rebinding installs a fresh channel and closes the old,
	// so a stale drainer cannot take the live drainer's wake.
	wake chan struct{}
	// live reports whether a subscription is currently bound. It is distinct
	// from sub, which only ever increases: after the first agent disconnects,
	// sub is nonzero forever, so a liveness test written against it silently
	// stops working exactly when a caller most wants to fail fast.
	live bool
	// retired is the tombstone Retire leaves on a session detached from the map. serve
	// resolves under p.mu and releases before taking s.mu to bind, so a Retire can delete
	// the session in that window; binding on the orphaned pointer would revive it. serve
	// re-checks this under s.mu at the bind and refuses. Set once, never cleared.
	retired bool
	cursor  uint64 // highest contiguously-acked seq
}

// controlProducer owns every session's control state for one Runner.
type controlProducer struct {
	mu       sync.Mutex
	sessions map[string]*controlSession
	// onCycle is a test seam: nil in production; when set, a drainer calls it at the end
	// of every send loop, before parking, with its own out-of-order set size. The set is
	// a drainer local, invisible to delivery assertions. Guarded by p.mu on both sides;
	// a drainer captures it once at bind, so a late set is merely ineffective, not a race.
	onCycle func(aboveLen int)
	// afterResolve is a test seam: nil in production; when set, serve calls it right after
	// resolving the session and BEFORE taking s.mu to bind, making the serve-vs-Retire
	// interleaving deterministic. Guarded by p.mu like onCycle, read once per serve call.
	afterResolve func()
}

func newControlProducer() *controlProducer {
	return &controlProducer{sessions: make(map[string]*controlSession)}
}

// representable reports whether the op carries a payload worth sending. Replay and Config
// are empty shells; Steer and Deliver each carry a defined compass.v1.Message, so they
// are representable and everything else is too.
func representable(op *compassv1internal.AgentControl) bool {
	switch op.GetControl().(type) {
	case *compassv1internal.AgentControl_Replay,
		*compassv1internal.AgentControl_Config:
		return false
	case nil:
		return false
	default:
		return true
	}
}

// replayPath reports whether an op belongs to the replay sequence rather than live
// traffic. The barrier holds LIVE ops only: it releases on the agent's ReplayCompleteAck,
// which it emits only after RECEIVING replay_complete — so holding replay-path traffic
// would hold the one op whose ack lifts it. TranscriptReplay: representable rejects it today.
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
// entry itself. The Runner reuses one container — and so one producer — across
// Stop/Start, receiving a fresh server-issued session id each time, so without
// this the map grows by one controlSession per cycle for the life of the process,
// each pinning up to maxRetainedOps retained ops.
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
// lifecycle spends its one Stop for that id, and the next cycle receives a fresh
// server-issued id. Ownership here makes that unrepresentable instead of guarded
// against.
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

	// An ack naming nothing prunes nothing, the only shape production emits today, so the
	// snapshot is skipped. When there IS something to intersect: snapshot the retained
	// seqs, then scan the agent-sized field OUTSIDE the lock — a stale snapshot only keeps
	// a since-retired seq, and the prune re-checks live state, so it prunes nothing.
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
	// The filter reuses the backing array, so the tail still holds pointers to the pruned
	// messages. Clear it or they stay reachable for as long as the session lives.
	for i := len(kept); i < len(s.ops); i++ {
		s.ops[i] = retained{}
	}
	s.ops = kept
}

// boundedAboveSet builds the applied-above set, keeping only seqs that can prune, which
// bounds what one ack can allocate. The agent sizes this field with no ordering, so
// relevance is the order-independent bound: intersect against retention itself, not the
// unbounded (cursor, nextSeq] window — capping the result at retention's size.
func boundedAboveSet(appliedAbove []uint64, retainedSeqs map[uint64]struct{}) map[uint64]struct{} {
	above := make(map[uint64]struct{})
	for _, seq := range appliedAbove {
		if _, retained := retainedSeqs[seq]; retained {
			above[seq] = struct{}{}
		}
	}
	return above
}

// HoldForReplay raises the replay barrier: live ops queue and retain but are not drained
// until the agent's ReplayCompleteAck releases them. Raised by the lifecycle when it
// restarts an agent into an existing session, before enqueuing the transcript replay. No
// production caller yet: exercised only by tests until the socket cutover lands.
func (p *controlProducer) HoldForReplay(sessionID string) {
	s, ok := p.existingSession(sessionID)
	if !ok {
		return
	}
	s.mu.Lock()
	s.held = true
	s.mu.Unlock()
}

// ReleaseReplayBarrier is the ack-routing entry point for the replay barrier: the agent's
// ReplayCompleteAck arrived on Publish, so held live ops may flow.
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

// existingSession resolves a session without creating one. The agent drives the
// ack/barrier entry points and can emit after the lifecycle retired the session (Stop
// ends the child but not the socket/stream). A creating lookup would rebuild the entry
// and nothing would retire it — the unbounded growth Retire exists to stop. Gone = no-op.
func (p *controlProducer) existingSession(sessionID string) (*controlSession, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[sessionID]
	if !ok || s == nil {
		return nil, false
	}
	return s, true
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

	// Clone before stamping: the producer owns what it retains. Stamping the caller's
	// message in place would mutate its argument AND race the drainer (which reads the
	// retained pointer outside the lock); a caller reusing one pointer for two Sends would
	// leave both retention entries behind a single message carrying only the last seq.
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
	// Check the cap BEFORE stamping: a rejected Send must consume no seq and leave no
	// partial state. Replay-path ops are exempt: under a raised barrier live ops retain but
	// don't deliver, so capping replay_complete would reject the barrier's own release — a
	// permanent wedge. Whoever defines TranscriptReplay's payload must bound admission here.
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

// absorbContiguous advances a subscription's contiguous watermark from `from`, consuming
// the run of already-delivered seqs above it and removing them from `above`. The walk is
// strictly upward, so it is correct ONLY for a contiguous advance; when the watermark
// JUMPS, entries stranded below it are unreachable — absorbJump handles that.
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

// absorbJump advances the watermark to `from` across a GAP, dropping what the move leaves
// behind. Entries at or below the new watermark are delivered ops the agent can never ask
// for again, so retaining them is pure leak. It lives here, not in absorbContiguous,
// because the O(len(above)) sweep is not needed on the contiguous path.
func absorbJump(from uint64, above map[uint64]struct{}) uint64 {
	for seq := range above {
		if seq <= from {
			delete(above, seq)
		}
	}
	return absorbContiguous(from, above)
}

// collectBatch walks the retention window for one drain cycle and returns the ops owed to
// the subscription: seq > sent, not held by the barrier, not already in `above`. Caller
// MUST hold s.mu. It binary-searches the ascending, front-pruned s.ops rather than
// walking it — a non-acking agent would otherwise make delivering n ops O(n²).
func (p *controlProducer) collectBatch(s *controlSession, sent uint64, above map[uint64]struct{}) []retained {
	if s.ops == nil {
		return nil
	}
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
		// The barrier holds LIVE ops only. Replay-path ops must pass it: the release is the
		// agent's ack of replay_complete, so holding that op holds its own release. A live
		// op arriving mid-replay sits at a lower seq, so this SKIPS rather than stops.
		if s.held && !replayPath(r.op) {
			continue
		}
		batch = append(batch, r)
	}
	return batch
}

// serve binds a subscription and drains the session's retained ops until the context ends
// or a takeover displaces it. Binding is a TAKEOVER: the generation increments and the
// predecessor's wake channel is CLOSED. Each subscription's high-water mark starts at the
// ack cursor, so a fresh one re-sends past the cursor while a steady-state one only advances.
func (p *controlProducer) serve(ctx context.Context, sessionID string, sink controlSink) error {
	s, ok := p.existingSession(sessionID)
	if !ok {
		// The Runner never bound this id, or already retired it. Minting one here can't tell
		// those apart, and nothing would retire what this created. Refuse honestly.
		return connect.NewError(connect.CodeNotFound, errNoBoundSession)
	}

	// Captured once, under the producer lock, so the drainer never races a concurrent write
	// to the seam on its hot loop.
	onCycle := p.cycleHook()

	// Resolve-window seam (nil in production): fires after the session is resolved and
	// before the bind takes s.mu — the exact interleaving a concurrent Retire can exploit.
	if hook := p.afterResolveHook(); hook != nil {
		hook()
	}

	s.mu.Lock()
	// Re-check the tombstone under s.mu: a Retire may have deleted this session between the
	// resolve above and this lock. Binding on a detached object would revive live=true state
	// nothing retires again and park a drainer on a closed wake — the serve-vs-Retire leak.
	if s.retired {
		s.mu.Unlock()
		return connect.NewError(connect.CodeNotFound, errNoBoundSession)
	}
	s.sub++
	mine := s.sub
	// Retire the predecessor: closing its channel unparks its drainer at once, and it owns
	// that channel, so this cannot disturb the new subscription.
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
		// Advance past seqs retention no longer holds. The agent is untrusted, so it may ack
		// a seq it never received; that prunes the op and the gap at sent+1 can never fill,
		// so without this `above` grows unbounded. absorbJump, not absorbContiguous: a JUMP
		// strands entries already recorded at or below the new watermark, and only this drops them.
		if sent < s.cursor {
			sent = absorbJump(s.cursor, above)
		}
		// collectBatch does the retention-window walk (binary-searched, ops>sent,
		// barrier-filtered) under the lock we already hold; extracted so serve's
		// cognitive complexity stays under the gate.
		batch := p.collectBatch(s, sent, above)
		s.mu.Unlock()

		// Send OUTSIDE the lock, deliberately: sink.Send is network I/O, and holding the
		// mutex across it would stall every Send and ack behind one slow socket. The cost is
		// an ack landing mid-batch can leave an acked op in this snapshot, so the agent may
		// see a duplicate — the at-least-once contract covers it via seq-dedup.
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
			// Same cursor-plus-set shape the agent's ack uses: a contiguous watermark plus
			// seqs delivered above it while the barrier held something lower. Absorb as the
			// gap fills so the set stays small and empties once the barrier lifts.
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

// subscriptionGeneration reports the highest subscription generation bound so far.
// Monotonic WITHIN a session's lifetime, NOT a liveness signal (`live` is that). A
// displaced subscription is one whose generation is no longer current — how a takeover
// retires the stale drainer. An unknown/retired id reads 0 rather than minting a session.
func (p *controlProducer) subscriptionGeneration(sessionID string) uint64 {
	s, ok := p.existingSession(sessionID)
	if !ok {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sub
}
