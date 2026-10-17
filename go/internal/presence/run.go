//go:build unix

package presence

import (
	"context"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/store"
)

// OnSessionLifecycle is the hub's PresenceSink lifecycle hook after the
// LifecycleSink publish in deliverSession (design.md:472-479). It must not block
// on store work or retain caller ctx; it enqueues the edge, and the loop restores
// its traceparent on its serve ctx before recomputing and publishing presence.
func (p *Publisher) OnSessionLifecycle(ctx context.Context, account store.AccountID, sessionID string, state compassv1.AgentSessionState) {
	p.enqueue(hubEdge{kind: edgeLifecycle, account: account, sessionID: sessionID, state: state, traceparent: otelx.Traceparent(ctx)})
}

// OnSessionPromoted is the hub's promoteSession reconciliation hook
// (design.md:494-503); re-enroll clears bindings and re-promotes sessions, so
// the loop rebuilds presence from lifecycle state and open-ask overlay. The hook
// only queues; it retains no ctx, and the loop restores its traceparent on ctx.
func (p *Publisher) OnSessionPromoted(ctx context.Context, account store.AccountID, sessionID string) {
	p.enqueue(hubEdge{kind: edgePromoted, account: account, sessionID: sessionID, traceparent: otelx.Traceparent(ctx)})
}

// enqueue appends a hub-fed edge and wakes the loop without blocking the caller.
func (p *Publisher) enqueue(edge hubEdge) {
	p.mu.Lock()
	p.queue = append(p.queue, edge)
	p.mu.Unlock()
	// Coalescing wakeup: a full buffer already signals a pending drain, so a
	// dropped send loses nothing (the loop drains the whole queue).
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

// Run tails the comms bus (the ask arm) and drains hub-fed edges (the lifecycle
// + reconciliation arms) until ctx is cancelled (serve shutdown) or the bus
// closes. It mirrors delivery.Consumer.Run: drain the replay snapshot
// oldest-first, then select on ctx, the live tail, and the edge-queue notify. On
// the live channel closing, sub.Lagged() distinguishes an overrun — re-subscribe
// and carry on (presence is derived, so a dropped bus event is a missed ask
// recompute the next event or the reconciliation edge repairs; there is no owed
// set to sweep) — from a clean bus shutdown (end silently). ctx threads from the
// serve group into every store read and publish below; the loop never re-roots it.
func (p *Publisher) Run(ctx context.Context) error {
	sub, err := p.bus.Subscribe(0, p.bus.InstanceEpoch())
	if err != nil {
		// A fresh subscription at since_seq=0 on a live bus cannot underflow; any
		// error here is a genuine subscribe fault the caller should see.
		return err
	}
	// Closure over sub (not defer sub.Cancel()): the lagged branch reassigns sub
	// to a fresh subscription, and only a closure cancels whichever one is
	// current at return.
	defer func() { sub.Cancel() }()

	for _, event := range sub.Replay {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		p.handleEvent(otelx.ContextWithTraceparent(otelx.WithoutSpan(ctx), event.Traceparent), event.Payload)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-p.notify:
			p.drainEdges(ctx)
		case event, ok := <-sub.Live:
			if !ok {
				if sub.Lagged() {
					// Overrun: bus events were dropped. Presence is a derived
					// projection with no owed set — re-subscribe and carry on; the
					// next ask event or the reconciliation edge repairs any missed
					// recompute. Re-drain of the retained ring is deliberately
					// avoided (it would republish nothing new under
					// publish-on-change anyway).
					fresh, err := p.bus.Subscribe(0, p.bus.InstanceEpoch())
					if err != nil {
						p.log.ErrorContext(ctx, "presence: re-subscribe after bus-lag overrun", "error", err)
						return err
					}
					sub.Cancel()
					sub = fresh
					continue
				}
				return nil
			}
			p.handleEvent(otelx.ContextWithTraceparent(otelx.WithoutSpan(ctx), event.Traceparent), event.Payload)
		}
	}
}

// handleEvent routes one comms bus payload for the ask arm. A MessagePosted
// carrying an ask (Ask-open) and a MessageUpdated (Ask-answered flips
// Ask.answered) both recompute + publish-on-change for the AUTHORING agent; a
// MessagePosted with no ask, and every other variant, is ignored (only the ask
// overlay flips on a comms event). The authoring agent's current lifecycle state
// for the recompute comes from the in-memory last-known lifecycle, or OFFLINE if
// unknown (design.md:483-485).
func (p *Publisher) handleEvent(ctx context.Context, resp *compassv1.SubscribeCommsResponse) {
	var author string
	switch {
	case resp.GetMessagePosted() != nil:
		msg := resp.GetMessagePosted().GetMessage()
		if !hasAsk(msg) {
			return
		}
		author = msg.GetAuthorAccountId()
	case resp.GetMessageUpdated() != nil:
		// An update grows/flips an agent-authored message's blocks — an answer
		// flips Ask.answered. Recompute unconditionally against the store, which
		// is the source of truth for whether ANY authored ask is still open (the
		// update may answer one of several, so the wire message alone can't decide).
		author = resp.GetMessageUpdated().GetMessage().GetAuthorAccountId()
	default:
		return
	}
	if author == "" {
		return
	}
	p.recomputeFromStore(ctx, store.AccountID(author))
}

// hasAsk reports whether a wire message carries at least one ask block — the
// Ask-open trigger.
func hasAsk(msg *compassv1.Message) bool {
	for _, b := range msg.GetBlocks() {
		if b.GetAsk() != nil {
			return true
		}
	}
	return false
}

// recomputeFromStore recomputes an agent's presence from its last-known
// lifecycle state layered with the store's current open-ask overlay, and
// publishes-on-change. The ask arm uses it: the overlay flipped, the lifecycle
// did not, so the state comes from lastState.
//
// Gated on the account having a recorded lifecycle state: the ask overlay only
// applies to an agent the lifecycle arm has already seen. An agent records a
// lastState entry at its first lifecycle/promotion edge BEFORE it can author
// anything (it must be live to post), while a HUMAN ask author never receives a
// lifecycle edge and so has no entry. Skipping accounts with no recorded state
// keeps a human ask author out of this AGENT-presence projection — without the
// gate, presenceFor(UNSPECIFIED, openAsk) = OFFLINE would publish a spurious
// AgentPresenceChanged naming a non-agent. The store read is deferred to after
// the gate so a human ask author costs no query.
func (p *Publisher) recomputeFromStore(ctx context.Context, account store.AccountID) {
	p.mu.Lock()
	state, ok := p.lastState[account]
	p.mu.Unlock()
	if !ok {
		return // no lifecycle state ever recorded → not a live agent (e.g. a human ask author)
	}
	openAsk, err := p.st.AgentHasOpenAsk(ctx, account)
	if err != nil {
		p.log.ErrorContext(ctx, "presence: resolve open ask", "error", err, "account", string(account))
		return
	}
	p.publishIfChanged(ctx, account, presenceFor(state, openAsk))
}

// drainEdges fires every queued hub-fed edge under the loop's ctx.
func (p *Publisher) drainEdges(ctx context.Context) {
	for {
		p.mu.Lock()
		if len(p.queue) == 0 {
			p.mu.Unlock()
			return
		}
		edge := p.queue[0]
		p.queue = p.queue[1:]
		p.mu.Unlock()

		edgeCtx := otelx.ContextWithTraceparent(otelx.WithoutSpan(ctx), edge.traceparent)
		switch edge.kind {
		case edgeLifecycle:
			p.applyLifecycle(edgeCtx, edge.account, edge.state)
		case edgePromoted:
			p.applyPromoted(edgeCtx, edge.account, edge.sessionID)
		}
	}
}

// applyLifecycle records the agent's new lifecycle state and publishes-on-change
// the presence it projects with the store's current open-ask overlay. Recording
// lastState here is what lets a later ask event layer WAITING on the right base.
func (p *Publisher) applyLifecycle(ctx context.Context, account store.AccountID, state compassv1.AgentSessionState) {
	openAsk, err := p.st.AgentHasOpenAsk(ctx, account)
	if err != nil {
		p.log.ErrorContext(ctx, "presence: resolve open ask (lifecycle)", "error", err, "account", string(account))
		return
	}
	p.mu.Lock()
	p.lastState[account] = state
	p.mu.Unlock()
	p.publishIfChanged(ctx, account, presenceFor(state, openAsk))
}

// applyPromoted reconstructs presence at a session promotion (the restart-
// reconciliation edge): it resolves the session's live lifecycle state via the
// Status relay (GetAgentStatus returns only lifecycle, so WITHOUT the store pass
// a WAITING agent would rebuild as IDLE) and layers the store's open-ask overlay,
// then records the state and publishes-on-change. A session with no resolvable
// live status reconstructs OFFLINE.
//
// The Status resolve is a REMOTE Runner round-trip on the single loop goroutine,
// so it is bounded by a deadline (statusTimeout, default presenceStatusTimeout):
// a wedged Runner degrades this session to ok=false → OFFLINE rather than
// freezing the loop (and starving the ask arm) until the Runner answers or serve
// shuts down. A DEADLINE, not a retry — one bounded call, no loop, no backoff.
func (p *Publisher) applyPromoted(ctx context.Context, account store.AccountID, sessionID string) {
	rctx, cancel := context.WithTimeout(ctx, p.statusTimeout)
	state, ok := p.status.SessionState(rctx, sessionID)
	cancel()
	if !ok {
		state = compassv1.AgentSessionState_AGENT_SESSION_STATE_UNSPECIFIED
	}
	openAsk, err := p.st.AgentHasOpenAsk(ctx, account)
	if err != nil {
		p.log.ErrorContext(ctx, "presence: resolve open ask (promote)", "error", err, "account", string(account))
		return
	}
	p.mu.Lock()
	p.lastState[account] = state
	p.mu.Unlock()
	p.publishIfChanged(ctx, account, presenceFor(state, openAsk))
}
