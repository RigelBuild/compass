//go:build unix

package delivery

import (
	"context"
	"math"
	"slices"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/store"
)

// onMessagePosted resolves recipients once and splits delivery by author state.
// Human posts and agent posts without a live author dispatch now; live-author posts wait for settlement.
// Held entries re-resolve recipients when they fire so subscription changes apply.
// Reads that can fail run before hold/dispatch; fanOut read faults are logged because retries could repeat steers.
func (c *Consumer) onMessagePosted(ctx context.Context, msg *compassv1.Message) error {
	if msg == nil {
		return nil
	}
	author := store.AccountID(msg.GetAuthorAccountId())
	messageID := msg.GetId()
	if msg.GetTopicId() == "" || messageID == "" {
		return nil
	}
	authorIsAgent, err := c.st.IsAgentAccount(ctx, author)
	if err != nil {
		return c.readFailure(ctx, "resolve author kind", messageID, err)
	}

	if !authorIsAgent {
		// Human-authored: settled at post, deliver immediately from posted blocks.
		// A wire message carries only its topic; resolve the channel it lives in
		// through topics.channel_id (the design record's topic->channel resolution).
		channel, err := c.st.MessageChannel(ctx, messageID)
		if err != nil {
			return c.readFailure(ctx, "resolve message channel", messageID, err)
		}
		c.fanOut(ctx, channel, author, msg)
		return nil
	}

	// Agent-authored. If the author has a live session, HOLD until it settles;
	// otherwise deliver now, re-reading the settled blocks from the store —
	// mirroring fireHeld, never the posted (possibly partial) wire message
	// (design.md:177-178, :306).
	authorSession, live := c.resolver.SessionForAccount(ctx, author)
	if !live {
		wire, channel, storedAuthor, err := c.storeMessageToWire(ctx, messageID)
		if err != nil {
			return c.readFailure(ctx, "re-read message for stored-block deliver", messageID, err)
		}
		c.fanOut(ctx, channel, storedAuthor, wire)
		return nil
	}
	c.hold(ctx, authorSession, messageID, msg.GetAtUnixMs(), msg.GetTurnSequence())
	return nil
}

// hold registers messageID under its author's session, ordered by commit time,
// for firing at the author's settle edge. A late hold replays its observed settle.
func (c *Consumer) hold(ctx context.Context, authorSession, messageID string, atUnixMs int64, turnSequence uint64) {
	entry := heldEntry{messageID: messageID, traceparent: otelx.Traceparent(ctx), atUnixMs: atUnixMs, turnSequence: turnSequence}
	if tenant, ok := store.TenantFromContext(ctx); ok {
		entry.tenant = tenant
	}
	c.mu.Lock()
	entry.generation = c.generation[authorSession]
	// Committed before the restart (same server clock as the insert): the old
	// identity's turn, whose settle won't come; any new-identity settle frees it.
	oldIdentity := false
	start, started := c.generationStart[authorSession]
	if started && atUnixMs < start {
		entry.turnSequence = 0
		turnSequence = 0
		oldIdentity = true
	}
	entries := c.held[authorSession]
	// First index strictly after atUnixMs: equal stamps keep arrival order.
	i, _ := slices.BinarySearchFunc(entries, atUnixMs, func(e heldEntry, at int64) int {
		if e.atUnixMs <= at {
			return -1
		}
		return 1
	})
	c.held[authorSession] = slices.Insert(entries, i, entry)
	settled, ok := c.lastSettle[authorSession]
	settledSequence := c.lastSettleSequence[authorSession]
	legacySettle := c.lastSettleLegacy[authorSession]
	// Compare sequences when both sides have one; fall back to commit time only
	// after a legacy (zero-sequence) settle.
	sequenceObserved := turnSequence > 0 && settledSequence >= turnSequence
	eligible := ok && (sequenceObserved || oldIdentity || legacySettle && settled >= atUnixMs)
	if eligible {
		upTo := int64(math.MaxInt64)
		replaySequence := turnSequence
		if oldIdentity {
			upTo = start - 1
			replaySequence = 0
		} else if turnSequence == 0 || legacySettle {
			upTo = settled
			replaySequence = 0
		}
		c.settleQueue = append(c.settleQueue, settleEvent{
			generation:   entry.generation,
			sessionID:    authorSession,
			state:        compassv1.AgentSessionState_AGENT_SESSION_STATE_READY,
			turnSequence: replaySequence,
			upTo:         upTo,
		})
	}
	c.mu.Unlock()
	if eligible {
		select {
		case c.notify <- struct{}{}:
		default:
		}
	}
}

// fanOut routes mentions and delivers the message to reachable channel agents.
// A member out of reach is absent from both recipient sets.
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
	fromHandle := msg.GetAuthorHandle()
	for _, agent := range recipients {
		if mentioned[agent] {
			continue // steer-only precedence: a mentioned agent never also gets a deliver
		}
		sessionID, live := c.resolver.SessionForAccount(ctx, agent)
		if !live {
			c.wake(ctx, agent) // best-effort resume; the D2 sweep is the durable backstop
			continue
		}
		c.dispatchTo(ctx, sessionID, msg, fromHandle)
	}
}

// routeMentionsFor is the single mention policy shared by live settle and recovery paths.
// It returns all mentioned channel agents for steer-only precedence; offline recipients are woken, not delivered.
// Out-of-sweep recipients get an owed row before wake, then a session recheck closes the record/wake race.
// Faults are logged and swallowed so mentions never fail a post.
func (c *Consumer) routeMentionsFor(ctx context.Context, channel store.ChannelID, author store.AccountID, msg *compassv1.Message) map[store.AccountID]bool {
	handles := mentionHandles(msg)
	if len(handles) == 0 {
		return nil
	}
	mentioned := c.resolveMentioned(ctx, channel, author, handles)
	fromHandle := msg.GetAuthorHandle()
	for agent := range mentioned {
		sessionID, live := c.resolver.SessionForAccount(ctx, agent)
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
			} else if sessionID, live := c.resolver.SessionForAccount(ctx, agent); live {
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

// routeAskAnswerFor is the shared ask_answer policy for live and recovery paths, preventing committed answers from stranding askers.
// Out-of-sweep askers get an owed row first, then a live-session steer closes the record/start race.
// The owed row is the offline backstop; the direct steer is the latency path.
// Record faults are logged and swallowed so targeting never fails a post.
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
	// Now-live between the sweep-set check and the record: steer directly, closing the
	// record-vs-start race (the owed row is the offline backstop, this is the latency
	// path). The owed sweep also dispatches as a STEER, so both render through the T6
	// ask_answer arm and dedup by msg.id absorbs any overlap.
	if sessionID, live := c.resolver.SessionForAccount(ctx, asker); live {
		c.dispatchSteerTo(ctx, sessionID, msg, msg.GetAuthorHandle())
	}
}

// resolveMentioned returns mentioned channel agents the author may reach, with a
// bare handle scoped to the posting author's owner namespace.
func (c *Consumer) resolveMentioned(ctx context.Context, channel store.ChannelID, author store.AccountID, handles []string) map[store.AccountID]bool {
	members, err := c.st.ChannelAgentMembers(ctx, channel, author)
	if err != nil {
		c.log.ErrorContext(ctx, "delivery: resolve channel agent members for mention routing", "error", err, "channel", string(channel))
		return nil
	}
	authorOwner, err := c.st.ResolveOwner(ctx, author)
	if err != nil {
		c.log.ErrorContext(ctx, "delivery: resolve author owner for mention routing", "error", err, "author", string(author))
		return nil // drop all mentions; the post still delivers normally
	}
	mentioned := map[store.AccountID]bool{}
	for _, handle := range handles {
		if reservedMentions[handle] {
			// @users expands to human members only: no agent session to steer.
			if handle == "everyone" || handle == "agents" {
				for _, member := range members {
					mentioned[member.ID] = true
				}
			}
			continue
		}
		qh := store.ParseQualifiedHandle(handle)
		for _, member := range members {
			if member.Handle != qh.Handle {
				continue
			}
			if qh.Qualified() && member.OwnerHandle == qh.Owner || !qh.Qualified() && member.OwnerUserID == authorOwner {
				mentioned[member.ID] = true
			}
		}
	}
	return mentioned
}

// dispatchTo queues a deliver behind the recipient's sweep; a refusal leaves the cursor unadvanced for D2 recovery.
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

// wake best-effort resumes an offline recipient; the owed row or cursor sweep remains the durable backstop.
// Nil is a no-op.
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
