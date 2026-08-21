//go:build unix

package delivery

import (
	"context"
	"errors"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
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
	author := store.AccountID(msg.GetAuthorAccountId())
	messageID := msg.GetId()
	if msg.GetTopicId() == "" || messageID == "" {
		return
	}
	authorIsAgent, err := c.st.IsAgentAccount(ctx, author)
	if err != nil {
		c.log.ErrorContext(ctx, "delivery: resolve author kind", "error", err, "message_id", messageID)
		return
	}

	if !authorIsAgent {
		// Human-authored: settled at post, deliver immediately from posted blocks.
		// A wire message carries only its topic; resolve the channel it lives in
		// through topics.channel_id (the frozen record's topic->channel resolution).
		channel, err := c.st.MessageChannel(ctx, messageID)
		if err != nil {
			c.log.ErrorContext(ctx, "delivery: resolve message channel", "error", err, "message_id", messageID)
			return
		}
		c.fanOut(ctx, channel, author, msg)
		return
	}

	// Agent-authored. If the author has a live session, HOLD until it settles;
	// otherwise deliver now, re-reading the settled blocks from the store (no
	// live turn to wait on) — mirroring fireHeld, never the posted (possibly
	// partial) wire message (design.md:177-178, :306).
	authorSession, live := c.resolver.SessionForAccount(author)
	if !live {
		wire, channel, author, err := c.storeMessageToWire(ctx, messageID)
		if err != nil {
			// The message vanished between post and deliver (unexpected): skip it;
			// the cursor never advanced, so the sweep still redelivers.
			c.log.ErrorContext(ctx, "delivery: re-read message for dead-author deliver", "error", err, "message_id", messageID)
			return
		}
		c.fanOut(ctx, channel, author, wire)
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

// fanOut dispatches one settled message. It first routes any `@`-mentions to a
// steer for each mentioned channel agent member (D5, routeMentions), then
// delivers the plain message to every live subscribed agent session, author
// excluded — but SKIPS any agent that was mentioned: the mentioned agent gets a
// steer only, never steer + deliver of the same message (steer-only precedence,
// OQ-3, design.md:537-546). A recipient with no live session is skipped — the D2
// sweep delivers on its next start (design.md:137,149). Folding mention routing
// here (not at the raw MessagePosted) parses at the author's settle edge for all
// three delivery paths at once, so a mention that streams in via a later
// MessageUpdated block is still seen (design.md:519-523).
func (c *Consumer) fanOut(ctx context.Context, channel store.ChannelID, author store.AccountID, msg *compassv1.Message) {
	mentioned := c.routeMentions(ctx, channel, author, msg)
	recipients, err := c.st.SubscribedAgents(ctx, channel, author)
	if err != nil {
		c.log.ErrorContext(ctx, "delivery: resolve subscribers", "error", err, "channel", string(channel))
		return
	}
	for _, agent := range recipients {
		if mentioned[agent] {
			continue // steer-only precedence: a mentioned agent never also gets a deliver
		}
		sessionID, live := c.resolver.SessionForAccount(agent)
		if !live {
			continue // no live session: the reconnect sweep delivers on next start
		}
		c.dispatchTo(ctx, sessionID, msg)
	}
}

// routeMentions parses `@`-mentions from msg's settled text blocks and steers
// every mentioned channel agent member with a LIVE session (D5). It returns the
// full set of mentioned agent members (live or not) so fanOut can exclude them
// from the plain deliver — steer-only precedence (OQ-3). A mentioned agent with
// no live session is skipped: a steer is a mid-turn interrupt and there is no
// turn to interrupt, and it is NOT added to the deliver fan-out either. A
// subscribed-or-home member picks the message up on its next start via the D2
// sweep (design.md:546-548). An UNSUBSCRIBED non-home member has no sweep
// redelivery (UndeliveredMessages is subscription-gated,
// store/delivery_cursors.go:212), so a mention reaches it only while it is live:
// offline + unsubscribed = nothing this cycle, by design (SEA-1641 tracks
// whether that gap should become recoverable). A mention routing failure is
// logged and the mention(s) dropped, never returned up: a mention must never
// fail a post (design.md:522-523).
func (c *Consumer) routeMentions(ctx context.Context, channel store.ChannelID, author store.AccountID, msg *compassv1.Message) map[store.AccountID]bool {
	handles := mentionHandles(msg)
	if len(handles) == 0 {
		return nil
	}
	mentioned := c.resolveMentioned(ctx, channel, author, handles)
	for agent := range mentioned {
		sessionID, live := c.resolver.SessionForAccount(agent)
		if !live {
			continue // no live turn to interrupt; redelivery only if subscribed-or-home (SEA-1641)
		}
		c.dispatchSteerTo(ctx, sessionID, msg)
	}
	return mentioned
}

// resolveMentioned resolves parsed handles to the set of agent members of
// channel to steer, author excluded throughout (design.md:525-535). Reserved
// pings (@everyone/@agents) expand to the channel's agent members; @users
// expands to human members, which have no session to steer, so they add nothing
// (design.md:532-534). A non-reserved handle resolves via AgentByHandle: an
// unknown or human handle is store.ErrNotFound — a no-op; a resolved agent that
// is not a channel member is also a no-op (design.md:526-527,534-535). The
// channel agent-member set is read once and reused both for reserved expansion
// and the per-handle membership check. Any store error is logged and the mention
// dropped (never fails the post).
func (c *Consumer) resolveMentioned(ctx context.Context, channel store.ChannelID, author store.AccountID, handles []string) map[store.AccountID]bool {
	members, err := c.st.ChannelAgentMembers(ctx, channel, author)
	if err != nil {
		c.log.ErrorContext(ctx, "delivery: resolve channel agent members for mention routing", "error", err, "channel", string(channel))
		return nil // drop all mentions; the post still delivers normally
	}
	memberSet := make(map[store.AccountID]bool, len(members))
	for _, m := range members {
		memberSet[m] = true
	}

	mentioned := map[store.AccountID]bool{}
	for _, h := range handles {
		if reservedMentions[h] {
			if h == "everyone" || h == "agents" {
				for m := range memberSet {
					mentioned[m] = true // author already excluded by ChannelAgentMembers
				}
			}
			// @users expands to human members only: no agent session to steer.
			continue
		}
		acc, err := c.st.AgentByHandle(ctx, h)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // unknown or human handle: a no-op
			}
			c.log.ErrorContext(ctx, "delivery: resolve mention handle", "error", err, "handle", h)
			continue // never fail the post
		}
		if acc.ID == author {
			continue // author never steers itself (self-mention)
		}
		if !memberSet[acc.ID] {
			continue // resolved agent is not a channel member: a no-op
		}
		mentioned[acc.ID] = true
	}
	return mentioned
}

// dispatchTo relays one deliver for msg to a recipient session under that
// session's dispatch gate — the per-session serialization (design.md:212-225): a
// live deliver takes the gate per message, so it queues BEHIND an in-flight
// sweep for the same session and never interleaves ahead of the sweep's ordered
// re-dispatch. A synchronous refusal (no live stream) is not fatal: the cursor
// was never advanced on send, so the D2 sweep redelivers.
func (c *Consumer) dispatchTo(ctx context.Context, sessionID string, msg *compassv1.Message) {
	c.gatedDispatch(ctx, sessionID, deliverOp(msg), msg.GetId())
}

// dispatchSteerTo relays one steer for msg to a mentioned recipient's session,
// through the same per-session gate as a deliver so a steer and a deliver for the
// same session never interleave. A synchronous refusal falls to the sweep
// exactly as a deliver does (design.md:546-548).
func (c *Consumer) dispatchSteerTo(ctx context.Context, sessionID string, msg *compassv1.Message) {
	c.gatedDispatch(ctx, sessionID, steerOp(msg), msg.GetId())
}

// gatedDispatch sends one control op to a recipient session under that session's
// dispatch gate (design.md:212-225), shared by the deliver and steer paths. A
// synchronous refusal (no live stream / immediate push failure) is treated as
// "no live session": the cursor is unadvanced, so the D2 sweep redelivers.
func (c *Consumer) gatedDispatch(ctx context.Context, sessionID string, op *compassv1internal.AgentControl, messageID string) {
	gate := c.gateFor(sessionID)
	if c.beforeGate != nil {
		c.beforeGate(sessionID)
	}
	gate.Lock()
	defer gate.Unlock()
	if err := c.dispatch.DispatchControl(ctx, sessionID, op); err != nil {
		c.log.WarnContext(ctx, "delivery: dispatch to session failed, leaving to sweep",
			"error", err, "session_id", sessionID, "message_id", messageID)
	}
}
