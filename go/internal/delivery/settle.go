//go:build unix

package delivery

import (
	"context"

	comms "github.com/sealedsecurity/compass/go/internal/comms"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
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
	c.mu.Unlock()
	// Coalescing wakeup: a full buffer already signals a pending drain, so a
	// dropped send loses nothing (the loop drains the whole queue).
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
// edge fires the messages held for that author session, in post order, from each
// message's CURRENT (settled) stored blocks (design.md:158-168), then clears the
// registry entry — a no-frame author death never enqueues an edge, so its held
// entry is left in place. It is NOT reaped in-process (no next-enroll reap is
// wired: hub.enroll clears only the hub's own session maps, never Consumer.held),
// so the entry persists until process restart. No-loss is unaffected — the
// reconnect sweep still delivers the message (design.md:168-176): the design's
// "no-loss, not no-leak" guarantee.
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

// fireHeld dispatches every message held for authorSession, ascending, and
// clears the registry entry. Each held message is re-read from the store so the
// deliver carries the author's SETTLED block set at fire time, not the initial
// posted blocks (design.md:158-161). Recipients are re-resolved per message
// against the then-current subscription + liveness.
func (c *Consumer) fireHeld(ctx context.Context, authorSession string) {
	c.mu.Lock()
	held := c.held[authorSession]
	delete(c.held, authorSession)
	c.mu.Unlock()

	for _, messageID := range held {
		wire, m, err := c.storeMessageToWire(ctx, messageID)
		if err != nil {
			// The message vanished between hold and fire (unexpected): skip it;
			// the cursor never advanced, so the sweep still redelivers.
			c.log.ErrorContext(ctx, "delivery: re-read held message", "error", err, "message_id", messageID)
			continue
		}
		c.fanOut(ctx, m.Container.ChannelID, m.AuthorAccountID, wire)
	}
}

// sweepAllLive redelivers every owed message to every live agent session — the
// resync->sweep fallback the bus-lag overrun triggers (design.md:227-231). The
// cursor defines exactly what each agent is owed, so a dropped bus event is a
// latency blip, never a loss. Each session's re-dispatch runs under that
// session's gate, so it serializes against any concurrent live deliver for the
// same session (design.md:220-225).
func (c *Consumer) sweepAllLive(ctx context.Context) {
	for account, sessionID := range c.resolver.LiveAgentSessions() {
		c.sweepSession(ctx, account, sessionID)
	}
}

// sweepSession redelivers every message owed to one agent, ascending seq per
// channel, under the recipient session's dispatch gate held for the WHOLE ordered
// re-dispatch — so live bus events for that session queue behind the sweep and
// drain after it (design.md:220-225), never interleaving ahead of the sweep's
// ordered set.
func (c *Consumer) sweepSession(ctx context.Context, account store.AccountID, sessionID string) {
	owed, err := c.st.UndeliveredMessages(ctx, account)
	if err != nil {
		c.log.ErrorContext(ctx, "delivery: sweep undelivered", "error", err, "account", string(account))
		return
	}
	gate := c.gateFor(sessionID)
	gate.Lock()
	defer gate.Unlock()
	for _, msgs := range owed {
		for i := range msgs {
			op := deliverOp(comms.MessageToWire(msgs[i]))
			if err := c.dispatch.DispatchControl(ctx, sessionID, op); err != nil {
				c.log.WarnContext(ctx, "delivery: sweep dispatch failed, leaving to next sweep",
					"error", err, "session_id", sessionID, "message_id", string(msgs[i].ID))
			}
		}
	}
}
