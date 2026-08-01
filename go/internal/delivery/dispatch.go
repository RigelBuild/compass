//go:build unix

package delivery

import (
	"context"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// onMessagePosted is the delivery trigger for one posted message. It resolves
// the subscribed agent recipients (D1's one query, author excluded, home-channel
// disjunct) and applies the author split (design.md:139-168):
//
//   - HUMAN-authored (settled at post — does not stream): deliver now, from the
//     posted blocks.
//   - AGENT-authored WITH a live author session: HOLD keyed by the author's
//     session id; the deliver fires on that session's settle edge, re-reading
//     the then-settled blocks.
//   - AGENT-authored with NO live author session (author already stopped at
//     post): deliver now from stored blocks — there is no live turn to wait on
//     (design.md:177-178, :306).
//
// The recipient set is resolved once here and captured with the held entry via a
// re-resolve at fire time (the settle path re-resolves recipients against the
// then-current subscription + liveness), so a subscription change between post
// and settle is honored.
func (c *Consumer) onMessagePosted(ctx context.Context, msg *compassv1.Message) {
	if msg == nil {
		return
	}
	channel := store.ChannelID(msg.GetChannelId())
	author := store.AccountID(msg.GetAuthorAccountId())
	messageID := msg.GetId()
	if channel == "" || messageID == "" {
		return
	}

	authorIsAgent, err := c.st.IsAgentAccount(ctx, author)
	if err != nil {
		c.log.ErrorContext(ctx, "delivery: resolve author kind", "error", err, "message_id", messageID)
		return
	}

	if !authorIsAgent {
		// Human-authored: settled at post, deliver immediately from posted blocks.
		c.fanOut(ctx, channel, author, msg)
		return
	}

	// Agent-authored. If the author has a live session, HOLD until it settles;
	// otherwise deliver now, re-reading the settled blocks from the store (no
	// live turn to wait on) — mirroring fireHeld, never the posted (possibly
	// partial) wire message (design.md:177-178, :306).
	authorSession, live := c.resolver.SessionForAccount(author)
	if !live {
		wire, m, err := c.storeMessageToWire(ctx, messageID)
		if err != nil {
			// The message vanished between post and deliver (unexpected): skip it;
			// the cursor never advanced, so the sweep still redelivers.
			c.log.ErrorContext(ctx, "delivery: re-read message for dead-author deliver", "error", err, "message_id", messageID)
			return
		}
		c.fanOut(ctx, m.Container.ChannelID, m.AuthorAccountID, wire)
		return
	}
	c.hold(authorSession, messageID)
}

// hold registers messageID under its author's session for later firing at the
// author's settle edge (design.md:157-160). Kept in post order so a settle fires
// the held set ascending.
func (c *Consumer) hold(authorSession, messageID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held[authorSession] = append(c.held[authorSession], messageID)
}

// fanOut dispatches one message (in wire form) to every live subscribed agent
// session, author excluded, each through the recipient's per-session gate so
// dispatch order is preserved. A recipient with no live session is skipped — the
// D2 sweep delivers on its next start (design.md:137,149).
func (c *Consumer) fanOut(ctx context.Context, channel store.ChannelID, author store.AccountID, msg *compassv1.Message) {
	recipients, err := c.st.SubscribedAgents(ctx, channel, author)
	if err != nil {
		c.log.ErrorContext(ctx, "delivery: resolve subscribers", "error", err, "channel", string(channel))
		return
	}
	for _, agent := range recipients {
		sessionID, live := c.resolver.SessionForAccount(agent)
		if !live {
			continue // no live session: the reconnect sweep delivers on next start
		}
		c.dispatchTo(ctx, sessionID, msg)
	}
}

// dispatchTo relays one deliver for msg to a recipient session under that
// session's dispatch gate — the per-session serialization (design.md:212-225): a
// live deliver takes the gate per message, so it queues BEHIND an in-flight
// sweep for the same session and never interleaves ahead of the sweep's ordered
// re-dispatch. A synchronous refusal (no live stream) is not fatal: the cursor
// was never advanced on send, so the D2 sweep redelivers.
func (c *Consumer) dispatchTo(ctx context.Context, sessionID string, msg *compassv1.Message) {
	gate := c.gateFor(sessionID)
	if c.beforeGate != nil {
		c.beforeGate(sessionID)
	}
	gate.Lock()
	defer gate.Unlock()
	if err := c.dispatch.DispatchControl(ctx, sessionID, deliverOp(msg)); err != nil {
		// Synchronous refusal — no live stream / immediate push failure. Treated
		// as "no live session"; the cursor is unadvanced, the sweep redelivers.
		c.log.WarnContext(ctx, "delivery: dispatch to session failed, leaving to sweep",
			"error", err, "session_id", sessionID, "message_id", msg.GetId())
	}
}
