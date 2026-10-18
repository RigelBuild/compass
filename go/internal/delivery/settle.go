//go:build unix

package delivery

import (
	"context"
	"math"
	"time"

	comms "github.com/RigelBuild/compass/go/internal/comms"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/store"
)

// OnSessionSettled is the hub's SettleSink hook. It runs on the hub's Deliver
// goroutine, so it only enqueues the edge and wakes the loop. STARTING fires
// nothing but starts a new identity generation (a fresh agent restarts at 1).
func (c *Consumer) OnSessionSettled(sessionID string, state compassv1.AgentSessionState, turnSequence uint64) {
	if state == compassv1.AgentSessionState_AGENT_SESSION_STATE_STARTING {
		c.startGeneration(sessionID)
		return
	}
	if !firesHeldDelivers(state) {
		return
	}
	c.mu.Lock()
	c.settleQueue = append(c.settleQueue, settleEvent{sessionID: sessionID, state: state, turnSequence: turnSequence, upTo: math.MaxInt64, generation: c.generation[sessionID]})
	c.lastSettle[sessionID] = c.now().UnixMilli()
	if turnSequence > c.lastSettleSequence[sessionID] {
		c.lastSettleSequence[sessionID] = turnSequence
		c.lastSettleLegacy[sessionID] = false
	}
	if turnSequence == 0 {
		c.lastSettleLegacy[sessionID] = true
	}
	c.mu.Unlock()
	// Coalescing wakeup: a full buffer already signals a pending drain, so a
	// dropped send loses nothing (the loop drains the whole queue).
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// OnSessionStarted is the hub's SessionStartSink hook (RIG-1569 T6), called from
// promoteSession right after the hub binds account->session at StartAgentSession
// (and, in the single-Runner MVP, on the re-promotion each session takes after a
// Runner re-enroll clears the bindings). Like OnSessionSettled it must NOT block
// the hub's Start goroutine on store work and must NOT store the caller's ctx
// (the loop owns the serve ctx), so it only enqueues the start edge and wakes the
// loop; the loop drains it under its own ctx by running the reconnect sweep for
// the freshly-live session. A fresh-start session needs no replay barrier — it
// has no in-flight replay to hold delivers behind — so the MVP path is safe
// without HoldForReplay (design.md:370-372); the barrier is a gated cross-lane
// dependency (control.go:426-429, no production caller yet), out of T6's scope.
func (c *Consumer) OnSessionStarted(sessionID string, account store.AccountID) {
	if sessionID == "" || account == "" {
		return
	}
	c.mu.Lock()
	c.startQueue = append(c.startQueue, startEvent{sessionID: sessionID, account: account})
	c.mu.Unlock()
	// Coalescing wakeup, shared with the settle queue: a full buffer already
	// signals a pending drain, so a dropped send loses nothing (the loop drains
	// both queues on every wakeup).
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// firesHeldDelivers fires READY from settled blocks and STOPPED/ERRORED from stored blocks.
// DISCONNECTED is not a settle; firing then would collapse the bounded reattach window to ERRORED.
// STARTING, WORKING, and UNSPECIFIED are not settle edges.
func firesHeldDelivers(state compassv1.AgentSessionState) bool {
	switch state {
	case compassv1.AgentSessionState_AGENT_SESSION_STATE_READY,
		compassv1.AgentSessionState_AGENT_SESSION_STATE_STOPPED,
		compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED:
		return true
	default:
		return false
	}
}

// drainSettles handles each queued settle edge under the loop's ctx. Numbered
// edges select by turn sequence; zero-sequence edges keep the legacy replay bound.
func (c *Consumer) drainSettles(ctx context.Context) {
	for {
		c.mu.Lock()
		if len(c.settleQueue) == 0 {
			c.mu.Unlock()
			return
		}
		ev := c.settleQueue[0]
		c.settleQueue = c.settleQueue[1:]
		logFallback := ev.turnSequence == 0
		if logFallback {
			if _, logged := c.fallbackLogged[ev.sessionID]; logged {
				logFallback = false
			} else {
				c.fallbackLogged[ev.sessionID] = struct{}{}
			}
		}
		c.mu.Unlock()
		if logFallback {
			c.log.WarnContext(ctx, "delivery: zero turn sequence settle; using legacy fire-all fallback", "session_id", ev.sessionID)
		}
		c.fireHeld(ctx, ev.sessionID, ev.upTo, ev.turnSequence, ev.generation)
	}
}

// drainStarts replays each new session's owed messages under its dispatch gate, so live events queue behind the sweep.
// Agent-side message-ID dedup makes retries safe; acked messages are absent from UndeliveredMessages.
func (c *Consumer) drainStarts(ctx context.Context) {
	for {
		c.mu.Lock()
		if len(c.startQueue) == 0 {
			c.mu.Unlock()
			return
		}
		ev := c.startQueue[0]
		c.startQueue = c.startQueue[1:]
		c.mu.Unlock()
		// Skip messages held for a live author: fireHeld re-resolves recipients
		// at settle, so this session still gets them with their settled blocks.
		c.sweepSession(ctx, ev.account, ev.sessionID, true)
		if err := c.sweepPins(ctx, ev.account, ev.sessionID); err != nil {
			c.log.ErrorContext(ctx, "delivery: sweep pins on session start", "error", err,
				"account", string(ev.account), "session_id", ev.sessionID)
		}
		if err := c.sweepOwedMentions(ctx, ev.account, ev.sessionID); err != nil {
			c.log.ErrorContext(ctx, "delivery: sweep owed mentions on session start", "error", err,
				"account", string(ev.account), "session_id", ev.sessionID)
		}
	}
}

// sweepPins injects current pins regardless of cursor, including already-acked messages, on session start.
// It reads channels, pins, and messages before taking the gate; live events then queue behind ordered dispatch.
// Normal ack semantics apply; agent-side message-ID dedup absorbs overlap with the cursor sweep.
// Channel-set failures return; unreadable pins log and skip without blocking the rest.
func (c *Consumer) sweepPins(ctx context.Context, agent store.AccountID, sessionID string) error {
	// Root a per-pass span: the sweep runs on the settle/start drain ctx, which
	// carries no live span, so the swept delivers link to this span's trace.
	ctx, span := otel.Tracer(instrumentationScope).Start(ctx, "delivery.sweep.pins")
	defer span.End()
	channels, err := c.st.SweepChannels(ctx, agent)
	if err != nil {
		return err
	}
	type pinOp struct {
		op        *compassv1internal.AgentControl
		messageID store.MessageID
	}
	var ops []pinOp
	for _, channel := range channels {
		entries, err := c.st.PinnedEntries(ctx, channel)
		if err != nil {
			c.log.ErrorContext(ctx, "delivery: read pinned board for pin sweep", "error", err,
				"channel", string(channel))
			continue
		}
		for _, entry := range entries {
			wire, _, _, err := c.storeMessageToWire(ctx, string(entry.MessageID))
			if err != nil {
				c.log.ErrorContext(ctx, "delivery: re-read pinned message for pin sweep", "error", err,
					"channel", string(channel), "message_id", string(entry.MessageID))
				continue
			}
			cn, tn := c.sourceNames(ctx, wire)
			ops = append(ops, pinOp{op: deliverOp(wire, wire.GetAuthorHandle(), cn, tn, otelx.Traceparent(ctx)), messageID: entry.MessageID})
		}
	}
	gate := c.gateFor(sessionID)
	gate.Lock()
	defer gate.Unlock()
	for _, po := range ops {
		if err := c.dispatch.DispatchControl(ctx, sessionID, po.op); err != nil {
			c.log.WarnContext(ctx, "delivery: pin sweep dispatch failed, leaving to next sweep",
				"error", err, "session_id", sessionID, "message_id", string(po.messageID))
		}
	}
	return nil
}

// sweepOwedMentions sends owed mentions as STEERs regardless of subscription or cursor.
// Reads precede the recipient dispatch gate, held only for ordered sends.
// Missing messages are clear-and-log, avoiding a permanent error on every session start.
func (c *Consumer) sweepOwedMentions(ctx context.Context, agent store.AccountID, sessionID string) error {
	// Root a per-pass span (see sweepPins): the swept steers link to its trace.
	ctx, span := otel.Tracer(instrumentationScope).Start(ctx, "delivery.sweep.owedMentions")
	defer span.End()
	owed, err := c.st.OwedMentions(ctx, agent)
	if err != nil {
		return err
	}
	type steerOpEntry struct {
		op        *compassv1internal.AgentControl
		messageID store.MessageID
	}
	var ops []steerOpEntry
	for channel, msgs := range owed {
		for _, m := range msgs {
			wire, _, _, err := c.storeMessageToWire(ctx, string(m.ID))
			if err != nil {
				// Permanently unreadable (message vanished): clear the owed row so
				// it stops re-logging every start, then log once. Do NOT dispatch.
				if cerr := c.st.ClearOwedMention(ctx, agent, string(m.ID)); cerr != nil {
					c.log.ErrorContext(ctx, "delivery: clear unreadable owed mention", "error", cerr,
						"agent", string(agent), "channel", string(channel), "message_id", string(m.ID))
				}
				c.log.WarnContext(ctx, "delivery: owed mention unreadable, cleared", "error", err,
					"agent", string(agent), "channel", string(channel), "message_id", string(m.ID))
				continue
			}
			cn, tn := c.sourceNames(ctx, wire)
			ops = append(ops, steerOpEntry{op: steerOp(wire, wire.GetAuthorHandle(), cn, tn, otelx.Traceparent(ctx)), messageID: m.ID})
		}
	}
	if len(ops) > 0 {
		c.log.InfoContext(ctx, "delivery: sweeping owed mentions on session start",
			"agent", string(agent), "session_id", sessionID, "count", len(ops))
	}
	gate := c.gateFor(sessionID)
	gate.Lock()
	defer gate.Unlock()
	for _, oe := range ops {
		if err := c.dispatch.DispatchControl(ctx, sessionID, oe.op); err != nil {
			c.log.WarnContext(ctx, "delivery: owed-mention sweep dispatch failed, leaving to next sweep",
				"error", err, "session_id", sessionID, "message_id", string(oe.messageID))
		}
	}
	return nil
}

// fireHeld dispatches entries at or before both the commit-time and sequence
// bounds, re-reading each under its hold-time tenant before dispatch.
func (c *Consumer) fireHeld(ctx context.Context, authorSession string, upTo int64, turnSequence, generation uint64) {
	c.mu.Lock()
	var fire, keep []heldEntry
	for _, e := range c.held[authorSession] {
		withinTime := e.atUnixMs <= upTo
		// Legacy entries have no turn identity, so they never wait on a sequence.
		// An edge queued before a restart cannot release the new identity's posts.
		withinTurn := e.generation <= generation &&
			(turnSequence == 0 || e.turnSequence == 0 || e.turnSequence <= turnSequence)
		if withinTime && withinTurn {
			fire = append(fire, e)
		} else {
			keep = append(keep, e)
		}
	}
	if len(keep) == 0 {
		delete(c.held, authorSession)
	} else {
		c.held[authorSession] = keep
	}
	c.mu.Unlock()

	for _, entry := range fire {
		// The drain ctx carries no tenant; re-read under the one captured at hold.
		tctx := store.WithTenant(ctx, entry.tenant)
		wire, channel, author, err := c.storeMessageToWire(tctx, entry.messageID)
		if err != nil {
			// The message vanished between hold and fire (unexpected): skip it;
			// the cursor never advanced, so the sweep still redelivers.
			c.log.ErrorContext(tctx, "delivery: re-read held message", "error", err, "message_id", entry.messageID)
			continue
		}
		// Restamp the origin trace captured at hold onto this bare drain ctx (no
		// live span here — the settle goroutine boundary), so the settled deliver
		// re-links to the publisher's trace. Empty origin ⇒ ctx unchanged ⇒ empty
		// on the wire (invariant holds).
		mctx := otelx.ContextWithTraceparent(tctx, entry.traceparent)
		c.fanOut(mctx, channel, author, wire)
	}
}

// OnSessionsReaped drops all per-session delivery state after the hub clears
// the session binding. The cursor sweep still delivers what was held.
func (c *Consumer) OnSessionsReaped(sessionIDs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, sid := range sessionIDs {
		delete(c.held, sid)
		delete(c.lastSettle, sid)
		delete(c.lastSettleSequence, sid)
		delete(c.lastSettleLegacy, sid)
		delete(c.generation, sid)
		delete(c.generationStart, sid)
		delete(c.fallbackLogged, sid)
	}
}

// startGeneration begins a new identity generation for sessionID. Settle marks
// reset, and old-identity posts (held now or held later with an earlier commit
// time) drop their turn sequence, so the new identity's first settle frees them.
func (c *Consumer) startGeneration(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	gen := c.generation[sessionID] + 1
	c.generation[sessionID] = gen
	c.generationStart[sessionID] = c.now().UnixMilli()
	delete(c.lastSettle, sessionID)
	delete(c.lastSettleSequence, sessionID)
	delete(c.lastSettleLegacy, sessionID)
	for i := range c.held[sessionID] {
		c.held[sessionID][i].turnSequence = 0
		c.held[sessionID][i].generation = gen
	}
}

// sweepAllLive is the recovery pass a fabric reconnect or the floor tick runs:
// it redelivers every owed message to every live agent session, skipping those
// held for a live author so partial blocks never go out ahead of fireHeld
// (message_id dedup would drop the settled deliver).
func (c *Consumer) sweepAllLive(ctx context.Context) {
	for account, sessionID := range c.resolver.LiveAgentSessions() {
		c.sweepSession(ctx, account, sessionID, true)
	}
}

// heldIDs snapshots the ids held for LIVE author sessions. An entry stranded
// under a dead session has no settle coming, so the sweeps must deliver it.
// Liveness is read after the held snapshot, so a session that went live in
// between is still skipped; a stale-live one also stays skipped, the safe side.
func (c *Consumer) heldIDs() map[string]struct{} {
	c.mu.Lock()
	bySession := make(map[string][]string, len(c.held))
	for sid, entries := range c.held {
		for _, e := range entries {
			bySession[sid] = append(bySession[sid], e.messageID)
		}
	}
	c.mu.Unlock()
	live := c.liveSessionIDs() // resolver read stays outside c.mu
	ids := make(map[string]struct{})
	for sid, msgIDs := range bySession {
		if _, ok := live[sid]; !ok {
			continue
		}
		for _, id := range msgIDs {
			ids[id] = struct{}{}
		}
	}
	return ids
}

// liveSessionIDs snapshots the set of live agent session ids.
func (c *Consumer) liveSessionIDs() map[string]struct{} {
	bound := c.resolver.LiveAgentSessions()
	live := make(map[string]struct{}, len(bound))
	for _, sid := range bound {
		live[sid] = struct{}{}
	}
	return live
}

// sweepSession redelivers one agent's owed messages in seq order under the
// session's gate held for the whole pass, so live delivers queue behind it
// (design.md:220-225). skipHeld leaves messages held at owed-read time to fireHeld.
func (c *Consumer) sweepSession(ctx context.Context, account store.AccountID, sessionID string, skipHeld bool) {
	// Root a per-pass span (see sweepPins): the swept delivers link to its trace.
	ctx, span := otel.Tracer(instrumentationScope).Start(ctx, "delivery.sweep.session")
	defer span.End()
	owed, err := c.st.UndeliveredMessages(ctx, account)
	if err != nil {
		c.log.ErrorContext(ctx, "delivery: sweep undelivered", "error", err, "account", string(account))
		return
	}
	var skip map[string]struct{}
	if skipHeld {
		// Snapshot after the owed read, so the window is one read, not one pass.
		skip = c.heldIDs()
	}
	gate := c.gateFor(sessionID)
	gate.Lock()
	defer gate.Unlock()
	for _, msgs := range owed {
		for i := range msgs {
			if _, held := skip[string(msgs[i].ID)]; held {
				continue
			}
			wire := comms.MessageToWire(msgs[i])
			cn, tn := c.sourceNames(ctx, wire)
			op := deliverOp(wire, wire.GetAuthorHandle(), cn, tn, otelx.Traceparent(ctx))
			if err := c.dispatch.DispatchControl(ctx, sessionID, op); err != nil {
				c.log.WarnContext(ctx, "delivery: sweep dispatch failed, leaving to next sweep",
					"error", err, "session_id", sessionID, "message_id", string(msgs[i].ID))
			}
		}
	}
}

// OnSessionLost is the hub's SessionLostSink. It only enqueues: the loop wakes the
// account, which re-provisions the container.
func (c *Consumer) OnSessionLost(sessionID string, account store.AccountID, errored bool) {
	if sessionID == "" || account == "" {
		return
	}
	c.enqueueLost(lostEvent{account: account, errored: errored})
}

func (c *Consumer) enqueueLost(ev lostEvent) {
	c.mu.Lock()
	c.lostQueue = append(c.lostQueue, ev)
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// drainLost wakes each lost account. An ERRORED loss wakes only with owed work, and
// repeat ERRORED wakes back off, so a poisoned message re-wakes on a capped delay.
func (c *Consumer) drainLost(ctx context.Context) {
	for {
		c.mu.Lock()
		if len(c.lostQueue) == 0 {
			c.mu.Unlock()
			return
		}
		ev := c.lostQueue[0]
		c.lostQueue = c.lostQueue[1:]
		c.mu.Unlock()
		if ev.errored && !ev.deferred && c.erroredWakePending(ev.account) {
			continue
		}
		if ev.errored && !c.hasOwedWork(ctx, ev.account) {
			c.log.InfoContext(ctx, "delivery: errored session owes nothing, not waking", "account", string(ev.account))
			if ev.deferred {
				c.clearPendingErroredWake(ev.account)
			}
			continue
		}
		if ev.errored && !ev.deferred {
			if delay := c.nextErroredWakeDelay(ev.account); delay > 0 {
				c.log.WarnContext(ctx, "delivery: repeat errored session, delaying wake",
					"account", string(ev.account), "delay", delay)
				c.afterFunc(delay, func() {
					c.enqueueLost(lostEvent{account: ev.account, errored: true, deferred: true})
				})
				continue
			}
		}
		if ev.deferred {
			c.clearPendingErroredWake(ev.account)
		}
		c.wake(ctx, ev.account)
	}
}

// erroredWakeDelay is the wait before the wake that follows strikes earlier
// ERRORED losses in one window: zero, then doubling from the base to the cap.
func erroredWakeDelay(strikes int) time.Duration {
	if strikes == 0 {
		return 0
	}
	if strikes > 10 {
		return erroredWakeMaxDelay
	}
	return min(erroredWakeBaseDelay<<(strikes-1), erroredWakeMaxDelay)
}

func (c *Consumer) erroredWakePending(account store.AccountID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.erroredWakes[account]
	return b != nil && b.pending
}

// nextErroredWakeDelay records one ERRORED loss for account and returns its wake
// delay; a nonzero delay marks a deferred wake pending.
func (c *Consumer) nextErroredWakeDelay(account store.AccountID) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.erroredWakes[account]
	now := c.now()
	if b == nil || now.Sub(b.last) >= erroredWakeResetAfter {
		b = &erroredBackoff{}
		c.erroredWakes[account] = b
	}
	delay := erroredWakeDelay(b.strikes)
	b.strikes++
	b.last = now
	b.pending = delay > 0
	return delay
}

func (c *Consumer) clearPendingErroredWake(account store.AccountID) {
	c.mu.Lock()
	if b := c.erroredWakes[account]; b != nil {
		b.pending = false
	}
	c.mu.Unlock()
}

// pruneErroredWakes drops backoff state past its reset window. Caller holds mu.
func (c *Consumer) pruneErroredWakes() {
	now := c.now()
	for account, b := range c.erroredWakes {
		if !b.pending && now.Sub(b.last) >= erroredWakeResetAfter {
			delete(c.erroredWakes, account)
		}
	}
}

// hasOwedWork reports whether a start sweep would deliver anything to account. A
// read error counts as owed: a spurious wake is cheaper than a lost message.
func (c *Consumer) hasOwedWork(ctx context.Context, account store.AccountID) bool {
	undelivered, err := c.st.UndeliveredMessages(ctx, account)
	if err != nil || len(undelivered) > 0 {
		return true
	}
	mentions, err := c.st.OwedMentions(ctx, account)
	return err != nil || len(mentions) > 0
}

// OnRunnerReady is called when a Runner's command stream attaches. A mention made
// while no Runner could serve it left an owed row and a failed wake; the loop now
// wakes every owed agent. WakeAgent skips live agents and coalesces duplicates.
func (c *Consumer) OnRunnerReady() {
	c.mu.Lock()
	c.owedRewake = true
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// drainOwedRewake wakes every agent with an owed mention, once per OnRunnerReady.
func (c *Consumer) drainOwedRewake(ctx context.Context) {
	c.mu.Lock()
	run := c.owedRewake
	c.owedRewake = false
	c.mu.Unlock()
	if !run {
		return
	}
	accounts, err := c.st.OwedMentionAccounts(ctx)
	if err != nil {
		c.log.WarnContext(ctx, "delivery: list owed mention accounts on runner ready", "error", err)
		return
	}
	// Off the loop: each wake is a Runner round trip, and a restart can owe many.
	go func() {
		for _, account := range accounts {
			if ctx.Err() != nil {
				return
			}
			c.rewake(ctx, account)
		}
	}()
}

// rewake roots one owed-mention wake: a Runner coming ready carries no trace.
func (c *Consumer) rewake(ctx context.Context, account store.AccountID) {
	ctx, span := otel.Tracer(instrumentationScope).Start(ctx, "delivery.rewake", trace.WithNewRoot())
	defer span.End()
	c.wake(ctx, account)
}
