//go:build unix

package delivery

import (
	"context"

	comms "github.com/RigelBuild/compass/go/internal/comms"

	"go.opentelemetry.io/otel"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/store"
)

// OnSessionSettled is the hub's SettleSink hook (§2), called at the hub's
// deliverSession arm right after the LifecycleSink publish, from the hub's
// Deliver goroutine. It must NOT block that goroutine on store work and must NOT
// store the caller's ctx (the loop owns the serve ctx), so it only enqueues the
// edge and wakes the loop; the loop drains it under its own ctx. A settle to a
// non-terminal, non-READY state (STARTING) is ignored — only a SETTLED edge
// fires held delivers.
func (c *Consumer) OnSessionSettled(sessionID string, state compassv1.AgentSessionState) {
	if !firesHeldDelivers(state) {
		return
	}
	c.mu.Lock()
	c.settleQueue = append(c.settleQueue, settleEvent{sessionID: sessionID, state: state})
	// Recorded with the enqueue, so a hold that loses the race to the drain
	// still sees this settle.
	c.lastSettle[sessionID] = c.now().UnixMilli()
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

// firesHeldDelivers reports whether a settle to state fires an author's held
// delivers (design.md:148-168, 312-315):
//   - READY (agent_end, the normal WORKING->READY turn-end) fires from the
//     message's current (settled) blocks.
//   - STOPPED / ERRORED (agent-emitted terminal frames) fire from stored blocks.
//   - DISCONNECTED is NOT a settle — it is the bounded-reattach window; firing on
//     it would collapse DISCONNECTED to ERRORED, which the design forbids.
//   - STARTING / WORKING / UNSPECIFIED are not settle edges.
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

// drainSettles fires every queued author-settle edge under the loop's ctx. Each
// edge fires the messages held for that author session, in post order, from
// each message's CURRENT (settled) stored blocks (design.md:158-168), then
// clears the registry entry. An edge for a session with nothing held is a no-op.
//
// A no-frame author death never enqueues an edge. No-loss still holds: the
// sweeps skip only messages held for a LIVE author, so the cursor sweep
// delivers an entry stranded under a dead session (design.md:168-176).
func (c *Consumer) drainSettles(ctx context.Context) {
	for {
		c.mu.Lock()
		if len(c.settleQueue) == 0 {
			c.mu.Unlock()
			return
		}
		ev := c.settleQueue[0]
		c.settleQueue = c.settleQueue[1:]
		c.mu.Unlock()
		c.fireHeld(ctx, ev.sessionID)
	}
}

// drainStarts sweeps every queued session-start edge under the loop's ctx. Each
// edge redelivers the freshly-live session's owed messages via the EXISTING
// sweepSession (holding the recipient session's dispatch gate for the whole
// ordered re-dispatch, so live events for that session queue behind the
// sweep — design.md:220-225). The sweep is at-least-once; agent-side message_id
// dedup (T5) makes an already-acked message a no-op, and the contiguous+sparse
// cursor omits it from UndeliveredMessages, so a message is not re-swept after
// ack (design.md:360-365).
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

// sweepPins injects a freshly-live session's current pins — the session-start
// pin step (design.md T7), a sibling of sweepSession. For every channel the
// agent sweeps (SweepChannels, the D1 disjunct), each PinnedEntry's message is
// re-read and dispatched as a DeliverControl REGARDLESS of cursor position, so a
// pin below the delivery cursor (acked_seq ≥ its seq) still reaches a fresh
// session. All reads (SweepChannels, PinnedEntries, message re-reads) happen
// BEFORE the gate is taken; the recipient session's dispatch gate is then held
// only across the ordered dispatch of the pre-built ops, mirroring sweepSession,
// so live events for the session queue behind it (design.md:220-225).
//
// It does NOT change cursor-advance semantics: a pin-sweep deliver is acked like
// any deliver (an ack for an already-below-cursor seq is the existing no-op,
// design.md:338, :658-660). Per-session message_id dedup (agent-side, DL-073/T5)
// absorbs the overlap when the cursor sweep already delivered the same message
// this session, so no server-side dedup is applied here.
//
// Errors resolving the channel set are returned (the caller logs them); a single
// unreadable pin (its message vanished) is logged and skipped so the rest of the
// board still injects.
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
			ops = append(ops, pinOp{op: deliverOp(wire, c.authorHandle(ctx, wire), cn, tn, otelx.Traceparent(ctx)), messageID: entry.MessageID})
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

// sweepOwedMentions dispatches every owed mention for a freshly-live session as
// a STEER (OQ-4: a woken mention keeps its mention→steer semantics for the
// mention-gap population), regardless of subscription or cursor. Mirrors
// sweepPins: reads happen before the gate; the recipient session's dispatch gate
// is held only across the ordered dispatch. Unlike sweepPins' skip-and-log of an
// unreadable pin, a permanently-unreadable owed message is CLEAR-and-log — a
// vanished message is undeliverable by construction, so its owed row is cleared
// rather than re-logged on every start (an every-start re-log loop otherwise).
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
			ops = append(ops, steerOpEntry{op: steerOp(wire, c.authorHandle(ctx, wire), cn, tn, otelx.Traceparent(ctx)), messageID: m.ID})
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

// fireHeld dispatches every message held for authorSession, ascending, and
// clears the registry entry. Each message is re-read under its hold-time tenant,
// so the deliver carries the SETTLED blocks (design.md:158-161), and recipients
// are re-resolved against the then-current subscription + liveness.
func (c *Consumer) fireHeld(ctx context.Context, authorSession string) {
	c.mu.Lock()
	held := c.held[authorSession]
	delete(c.held, authorSession)
	c.mu.Unlock()

	for _, entry := range held {
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

// OnSessionsReaped drops the held-deliver and settle-time entries for sessions
// whose hub bindings were cleared at a Runner (re-)enroll, so a no-frame
// author death does not leak an entry (design.md:172-175). The cursor sweep
// still delivers what was held, since it skips only live authors' messages.
func (c *Consumer) OnSessionsReaped(sessionIDs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, sid := range sessionIDs {
		delete(c.held, sid)
		delete(c.lastSettle, sid)
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
func (c *Consumer) heldIDs() map[string]struct{} {
	live := c.liveSessionIDs() // resolver read stays outside c.mu
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := make(map[string]struct{})
	for sid, entries := range c.held {
		if _, ok := live[sid]; !ok {
			continue
		}
		for _, e := range entries {
			ids[e.messageID] = struct{}{}
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
			op := deliverOp(wire, c.authorHandle(ctx, wire), cn, tn, otelx.Traceparent(ctx))
			if err := c.dispatch.DispatchControl(ctx, sessionID, op); err != nil {
				c.log.WarnContext(ctx, "delivery: sweep dispatch failed, leaving to next sweep",
					"error", err, "session_id", sessionID, "message_id", string(msgs[i].ID))
			}
		}
	}
}
