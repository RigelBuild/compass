//go:build unix

package delivery

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
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
	c.hold(authorSession, messageID, otelx.Traceparent(ctx))
}

// hold registers messageID under its author's session for later firing at the
// author's settle edge (design.md:157-160). Kept in post order so a settle fires
// the held set ascending. traceparent is the origin trace captured at hold time
// (the bus-extracted author ctx); it is restamped at fireHeld so the settled
// deliver re-links to the publisher's trace across the settle goroutine boundary.
func (c *Consumer) hold(authorSession, messageID, traceparent string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held[authorSession] = append(c.held[authorSession], heldEntry{messageID: messageID, traceparent: traceparent})
}

// fanOut dispatches one settled message. It first routes any `@`-mentions to a
// steer for each mentioned channel agent member (D5, routeMentionsFor), then
// delivers the plain message to every live subscribed agent session, author
// excluded — but SKIPS any agent that was mentioned: the mentioned agent gets a
// steer only, never steer + deliver of the same message (steer-only precedence,
// OQ-3, design.md:537-546). A recipient with no live session is woken via the
// AgentWaker seam (OQ-6, best-effort resume) and otherwise skipped — the D2
// cursor sweep is the durable backstop that delivers on its next start
// (design.md:137,149), so no owed row is recorded on this arm. Folding mention
// routing here (not at the raw MessagePosted) parses at the author's settle edge
// for all three delivery paths at once, so a mention that streams in via a later
// MessageUpdated block is still seen (design.md:519-523).
func (c *Consumer) fanOut(ctx context.Context, channel store.ChannelID, author store.AccountID, msg *compassv1.Message) {
	mentioned := c.routeMentionsFor(ctx, channel, author, msg)
	// RIG-2257: an ask_answer message targets its asking agent when that agent is
	// outside the channel's sweep set (invisible to both the deliver loop below
	// and the reconnect cursor sweep). Shared with the recovery scan so a restart
	// in the commit→fanOut window still re-derives the owed row.
	c.routeAskAnswerFor(ctx, channel, msg)
	// Stamp the durable mention marker so the next recovery scan structurally
	// excludes this message: the live settle edge routed its mentions. A mark
	// failure is logged loud and swallowed — mention routing can never fail a
	// post (design.md:522-523).
	if err := c.st.MarkMentionsRouted(ctx, msg.GetId()); err != nil {
		c.log.ErrorContext(ctx, "delivery: mark mentions routed", "error", err, "message_id", msg.GetId())
	}
	recipients, err := c.st.SubscribedAgents(ctx, channel, author)
	if err != nil {
		c.log.ErrorContext(ctx, "delivery: resolve subscribers", "error", err, "channel", string(channel))
		return
	}
	fromHandle := c.authorHandle(ctx, msg)
	for _, agent := range recipients {
		if mentioned[agent] {
			continue // steer-only precedence: a mentioned agent never also gets a deliver
		}
		sessionID, live := c.resolver.SessionForAccount(agent)
		if !live {
			c.wake(ctx, agent) // best-effort resume; the D2 sweep is the durable backstop
			continue
		}
		c.dispatchTo(ctx, sessionID, msg, fromHandle)
	}
}

// routeMentionsFor parses `@`-mentions from msg's settled text blocks and steers
// every mentioned channel agent member with a LIVE session (D5). It is the ONE
// implementation of the per-message mention policy, shared verbatim by the live
// settle path (fanOut) and the RIG-2490 recovery scan (scanMissedMentions) — a
// second mention-routing body is a second correctness surface and is prohibited.
// It returns the full set of mentioned agent members (live or not) so fanOut can
// exclude them from the plain deliver — steer-only precedence (OQ-3). A mentioned
// agent with no live session is handled by the offline arm rather than dropped:
// it is NOT added to the deliver fan-out (steer-only precedence still holds), but
// the mention is made recoverable. An out-of-sweep-set member (unsubscribed,
// non-home, non-mandatory) has no cursor-sweep backstop, so a durable
// owed_mentions row is recorded first (T1; a record failure is logged loud and
// the mention continues — a mention must never fail a post); after a successful
// record the session is re-checked and, if now live, the owed mention is steered
// directly, closing the record-vs-wake race. Then EVERY offline mentioned member
// is woken via the AgentWaker seam (OQ-3 wake-all, incl. broadcast-expanded
// members OQ-6), which resumes it to sweep the owed row. Order is record THEN
// wake — the durable row is written before the wake so a wake that
// resumes-and-sweeps finds it (durability-first). A mention routing failure is
// logged and the mention(s) dropped, never returned up: a mention must never
// fail a post (design.md:522-523).
func (c *Consumer) routeMentionsFor(ctx context.Context, channel store.ChannelID, author store.AccountID, msg *compassv1.Message) map[store.AccountID]bool {
	handles := mentionHandles(msg)
	if len(handles) == 0 {
		return nil
	}
	mentioned := c.resolveMentioned(ctx, channel, author, handles)
	fromHandle := c.authorHandle(ctx, msg)
	for agent := range mentioned {
		sessionID, live := c.resolver.SessionForAccount(agent)
		if live {
			c.dispatchSteerTo(ctx, sessionID, msg, fromHandle)
			continue
		}
		// Offline mentioned member: record (durable, if outside the sweep set)
		// then wake (durability-first). The owed row is the no-loss backstop; the
		// wake is the latency path.
		inSweep, err := c.st.InSweepSet(ctx, agent, channel)
		if err != nil {
			c.log.ErrorContext(ctx, "delivery: in-sweep-set check for offline mention", "error", err,
				"agent", string(agent), "channel", string(channel), "message_id", msg.GetId())
		} else if !inSweep {
			if err := c.st.RecordOwedMention(ctx, agent, channel, msg.GetId()); err != nil {
				// The no-loss edge: an owed mention that fails to record is lost.
				c.log.ErrorContext(ctx, "delivery: record owed mention for offline out-of-sweep-set member", "error", err,
					"agent", string(agent), "channel", string(channel), "message_id", msg.GetId())
			} else if sessionID, live := c.resolver.SessionForAccount(agent); live {
				// Now-live between the first resolve and the record: steer directly,
				// closing the record-vs-wake race.
				c.dispatchSteerTo(ctx, sessionID, msg, fromHandle)
			}
		}
		c.wake(ctx, agent)
	}
	return mentioned
}

// askAnswerTarget reports the asking agent an ask_answer message targets. The
// answer message carries a single server-owned ask_answer block whose
// asker_account_id is the agent that posted the original ask (denormalized at
// AnswerAsk time). A message with no ask_answer block returns ok=false.
func askAnswerTarget(msg *compassv1.Message) (store.AccountID, bool) {
	for _, b := range msg.GetBlocks() {
		if aa := b.GetAskAnswer(); aa != nil {
			if asker := aa.GetAskerAccountId(); asker != "" {
				return store.AccountID(asker), true
			}
		}
	}
	return "", false
}

// routeAskAnswerFor targets the asking agent of an ask_answer message when it
// falls OUTSIDE the answer channel's sweep set (RIG-2257 T5). The deliver set
// (SubscribedAgents) and the sweep set (InSweepSet) are the same disjunct
// (subscribed OR home OR mandatory), so an asker that is a channel member but
// unsubscribed, non-home, and non-mandatory is invisible to BOTH the live
// deliver loop AND the reconnect cursor sweep — its answer would never arrive
// without this arm. A subscribed/home/mandatory asker needs neither arm: the
// normal deliver + cursor sweep already covers it.
//
// It is the ONE implementation of the ask_answer targeting policy, shared
// verbatim by the live settle path (fanOut) and the RIG-2490 recovery scan
// (scanMissedMentions) — so a consumer restart between AnswerAsk's commit and
// fanOut re-derives the owed row on the next recovery scan (the answer message
// is committed mentions_routed_at IS NULL, so it reappears in
// UnroutedMentionMessages) rather than stranding the out-of-sweep asker. A
// second targeting body would be a second correctness surface and is prohibited.
//
// For an out-of-sweep asker: (1) RECORD the durable owed row (the offline
// backstop, drained by the OwedMentions start-edge sweep, which dispatches it as
// a STEER); then (2) re-check the session and, if now live, dispatch the answer
// directly as a steer — the latency path, closing the record-vs-start race.
// Owed row and live dispatch are complements (durability vs latency), not
// either/or: without the live dispatch an unsubscribed asker with a live session
// strands until its next restart. A record failure is logged loud and swallowed
// — delivery targeting must never fail a post.
func (c *Consumer) routeAskAnswerFor(ctx context.Context, channel store.ChannelID, msg *compassv1.Message) {
	asker, ok := askAnswerTarget(msg)
	if !ok {
		return
	}
	inSweep, err := c.st.InSweepSet(ctx, asker, channel)
	if err != nil {
		c.log.ErrorContext(ctx, "delivery: in-sweep-set check for ask_answer asker", "error", err,
			"agent", string(asker), "channel", string(channel), "message_id", msg.GetId())
		return
	}
	if inSweep {
		return // the normal deliver + cursor sweep already reaches a swept asker
	}
	if err := c.st.RecordOwedMention(ctx, asker, channel, msg.GetId()); err != nil {
		// The no-loss edge: an owed answer that fails to record is lost.
		c.log.ErrorContext(ctx, "delivery: record owed ask_answer for out-of-sweep-set asker", "error", err,
			"agent", string(asker), "channel", string(channel), "message_id", msg.GetId())
		return
	}
	// Now-live between the sweep-set check and the record: steer directly,
	// closing the record-vs-start race (the owed row is the offline backstop,
	// this is the latency path). The owed sweep dispatches as a STEER, so the
	// direct dispatch matches — both render through the same T6 ask_answer arm
	// and dedup by msg.id absorbs any overlap.
	if sessionID, live := c.resolver.SessionForAccount(asker); live {
		c.dispatchSteerTo(ctx, sessionID, msg, c.authorHandle(ctx, msg))
	}
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
	// A bare mention resolves in the POSTING AUTHOR's owner namespace (RIG-2751:
	// agent handles are per-owner; a mention carries no owner qualifier, so the
	// author's own namespace is the resolution scope). Resolve it once for the
	// per-handle lookups below.
	authorOwner, err := c.st.ResolveOwner(ctx, author)
	if err != nil {
		c.log.ErrorContext(ctx, "delivery: resolve author owner for mention routing", "error", err, "author", string(author))
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
		acc, err := c.st.AgentByHandle(ctx, authorOwner, h)
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
func (c *Consumer) dispatchTo(ctx context.Context, sessionID string, msg *compassv1.Message, fromHandle string) {
	channelName, topicName := c.sourceNames(ctx, msg)
	c.gatedDispatch(ctx, sessionID, deliverOp(msg, fromHandle, channelName, topicName, otelx.Traceparent(ctx)), msg.GetId())
}

// dispatchSteerTo relays one steer for msg to a mentioned recipient's session,
// through the same per-session gate as a deliver so a steer and a deliver for the
// same session never interleave. A synchronous refusal falls to the sweep
// exactly as a deliver does (design.md:546-548).
func (c *Consumer) dispatchSteerTo(ctx context.Context, sessionID string, msg *compassv1.Message, fromHandle string) {
	channelName, topicName := c.sourceNames(ctx, msg)
	c.gatedDispatch(ctx, sessionID, steerOp(msg, fromHandle, channelName, topicName, otelx.Traceparent(ctx)), msg.GetId())
}

// wake best-effort resumes an offline recipient via the AgentWaker seam (T3), so
// an owed mention or subscribed deliver reaches it promptly. Nil-safe: a
// consumer with no waker wired (the default, and every unit test that does not
// set one) is a no-op — the durable owed row / cursor sweep is the backstop, so
// a missing wake never loses a message.
func (c *Consumer) wake(ctx context.Context, agent store.AccountID) {
	if c.agentWaker != nil {
		c.agentWaker.WakeAgent(ctx, agent)
	}
}

// gatedDispatch sends one control op to a recipient session under that session's
// dispatch gate (design.md:212-225), shared by the deliver and steer paths. A
// synchronous refusal (no live stream / immediate push failure) is treated as
// "no live session": the cursor is unadvanced, so the D2 sweep redelivers.
func (c *Consumer) gatedDispatch(ctx context.Context, sessionID string, op *compassv1internal.AgentControl, messageID string) {
	// op kind (steer|deliver), read once from the op oneof, is the only attribute
	// on both the hop span and the dispatch metric — never per-session/channel/
	// message (cardinality).
	opKind := "deliver"
	if op.GetSteer() != nil {
		opKind = "steer"
	}
	// One hop span per dispatch, re-linking to the publisher's trace carried on
	// ctx (empty ctx ⇒ a root span; the trace machinery never blocks a delivery).
	ctx, span := otel.Tracer(instrumentationScope).Start(ctx, "delivery.dispatch")
	defer span.End()
	span.SetAttributes(
		attribute.String("compass.message.id", messageID),
		attribute.String("compass.session.id", sessionID),
		attribute.String("compass.op.kind", opKind),
	)
	// One dispatch counted per gatedDispatch, labelled only by op kind. Nil-guard:
	// a failed counter construction disables the metric, never a delivery.
	if c.dispatched != nil {
		c.dispatched.Add(ctx, 1, metric.WithAttributes(attribute.String("compass.op.kind", opKind)))
	}
	gate := c.gateFor(sessionID)
	if c.beforeGate != nil {
		c.beforeGate(sessionID)
	}
	gate.Lock()
	defer gate.Unlock()
	if err := c.dispatch.DispatchControl(ctx, sessionID, op); err != nil {
		span.SetStatus(codes.Error, "dispatch refused")
		c.log.WarnContext(ctx, "delivery: dispatch to session failed, leaving to sweep",
			"error", err, "session_id", sessionID, "message_id", messageID)
	}
}
