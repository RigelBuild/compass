//go:build unix

// Package presence is the Server-side agent-presence projection (SEA-1569 T8,
// design record D4). It publishes an AgentPresenceChanged onto the comms fan-out
// bus whenever a live agent's presence changes, where presence is a 4-state
// projection (WORKING / IDLE / WAITING / OFFLINE) of TWO sources: the agent
// session's lifecycle state and an unanswered-authored-ask overlay (WAITING >
// IDLE). The 4-state model is ratified (OQ-1, Matt 2026-07-29) — a 3-state MVP
// was rejected, and WAITING is a server-side overlay, not a session enum value.
//
// It mirrors delivery.Consumer EXACTLY (the established precedent for a
// bus-tailing server component fed by the hub): Run(ctx) roots a long-lived loop
// on the serve-scoped ctx that tails the comms bus (the ask arm) and drains a
// queue of hub-fed edges (the lifecycle + reconciliation arms), while the hub
// sinks only enqueue + notify — they never block the hub's Deliver goroutine on
// store work and never store the caller's ctx (rule://go-thread-context: the ctx
// passed to Run IS the loop's root; the sinks hand their work back to the loop
// rather than storing a ctx).
//
// Presence is published from TWO arms because the two sources flip on different
// signals (D4, design.md:472-485):
//   - the LIFECYCLE arm (OnSessionLifecycle, fed from the hub's deliverSession)
//     recomputes on each session lifecycle transition, and
//   - the ASK arm (the bus tail in Run) recomputes on an Ask opening
//     (MessagePosted carrying an ask) or answering (MessageUpdated flipping
//     Ask.answered) — the overlay flips on COMMS events, so the lifecycle arm
//     alone would never republish a READY agent as WAITING or back.
//
// Restart reconciliation (OnSessionPromoted, fed from the hub's promoteSession)
// reconstructs presence at Runner re-enrollment: a restarted Server knows
// nothing, and a long-WORKING agent may emit no lifecycle frame for hours, so on
// each session promotion the component resolves the live lifecycle state (via the
// hub Status relay) AND the open-ask overlay (from the store) and publishes the
// reconstructed presence (design.md:494-503).
//
// Publish-on-change: the component keeps the last-published presence per agent
// and publishes AgentPresenceChanged ONLY when the newly-computed value differs
// (design.md:481-485). This is what makes "a transition publishes EXACTLY ONE
// AgentPresenceChanged" hold. Visibility is enforced at the SUBSCRIBE edge
// (comms/subscribe.go, the shared-channel rule), not here — the publisher just
// publishes onto the bus.
package presence

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// PresenceReads is the store surface the publisher reads: the unanswered-
// authored-ask overlay input. *store.Store implements it (presence_reads.go).
type PresenceReads interface {
	// AgentHasOpenAsk reports whether the agent has authored an ask block that is
	// not yet answered — the WAITING overlay input. The store's implementation is
	// a JSONB path-existence probe (an unanswered ask OMITS the answered field, so
	// a naive containment misses it).
	AgentHasOpenAsk(ctx context.Context, agent store.AccountID) (bool, error)
}

// LifecycleStatusResolver resolves a live session's lifecycle state through the
// Runner Status relay (GetAgentStatus) — the reconciliation input a restart
// rebuilds presence from. runnerhub.Hub implements it (SessionState). ok=false
// when no live status exists for the session (the reconstruction falls to
// OFFLINE).
type LifecycleStatusResolver interface {
	SessionState(ctx context.Context, sessionID string) (compassv1.AgentSessionState, bool)
}

// presenceStatusTimeout bounds the remote Runner Status resolve at a session
// promotion (applyPromoted). It runs on the single presence loop goroutine, so a
// wedged Runner must not freeze the loop: past this deadline the resolve returns
// ok=false and the session degrades to OFFLINE. A DEGRADE bound, not a retry
// (rule://no-retries) — one bounded call per promotion, no loop, no backoff.
const presenceStatusTimeout = 5 * time.Second

// edgeKind tags a hub-fed edge queued for the loop to drain under its own ctx.
type edgeKind int

const (
	// edgeLifecycle is a session lifecycle transition (OnSessionLifecycle): the
	// state is carried on the edge; the loop recomputes with the store overlay.
	edgeLifecycle edgeKind = iota
	// edgePromoted is a session promotion (OnSessionPromoted, the reconciliation
	// edge): the loop resolves the live state via the Status relay + the overlay.
	edgePromoted
)

// hubEdge is one queued hub-fed edge handed from the hub's Deliver / promote
// goroutine to the publisher's ctx-rooted loop.
type hubEdge struct {
	kind      edgeKind
	account   store.AccountID
	sessionID string
	state     compassv1.AgentSessionState // set for edgeLifecycle only
}

// Publisher tails the comms bus and publishes AgentPresenceChanged onto it as a
// live agent's presence changes. Safe for concurrent use: last, lastState, the
// edge queue, and the notify channel mutate under mu; the bus loop, the hub
// sinks, and PresenceSnapshot all touch them.
type Publisher struct {
	bus    *events.Bus[*compassv1.SubscribeCommsResponse]
	st     PresenceReads
	status LifecycleStatusResolver
	log    *slog.Logger

	mu sync.Mutex
	// last is the last-published presence per agent — the publish-on-change
	// registry AND the snapshot source (PresenceSnapshot). An agent absent here
	// has never had a presence published; the zero value UNSPECIFIED is never a
	// COMPUTED presence (presenceFor only ever returns IDLE/WORKING/WAITING/
	// OFFLINE), so the first computed value for an agent always differs and
	// publishes.
	last map[store.AccountID]compassv1.AgentPresence
	// lastState is the last-known lifecycle state per agent, the state input the
	// ask arm recomputes against (the ask overlay flips without a lifecycle
	// transition, so it needs the last-known lifecycle to layer WAITING on). An
	// agent absent here is UNSPECIFIED, which projects OFFLINE (design.md:132).
	lastState map[store.AccountID]compassv1.AgentSessionState
	// queue buffers hub-fed edges (lifecycle + promotion) the sinks enqueue,
	// drained by the loop under its ctx. A slice (never lost) plus a buffered
	// notify channel (coalescing wakeups): a sink appends and signals without
	// blocking the hub goroutine.
	queue []hubEdge
	// notify wakes the loop when queue grows. Buffered(1) with a non-blocking
	// send, so many edges between drains collapse to one wakeup and no sink ever
	// blocks.
	notify chan struct{}
	// statusTimeout bounds the remote Status resolve in applyPromoted. Defaults
	// to presenceStatusTimeout; an unexported field so a test can shorten it to
	// exercise the degrade-to-OFFLINE-on-timeout path without a real wait. Never
	// exposed publicly.
	statusTimeout time.Duration
}

// NewPublisher constructs the presence publisher over the comms bus (it both
// tails and publishes onto it), the store's open-ask read surface, and the hub's
// Status relay for reconciliation. The hub sinks are wired the other way, via
// hub.SetPresenceSink(publisher) AFTER both exist — the post-construction setter
// that breaks the construction cycle, mirroring delivery's SetSettleSink. A nil
// log falls back to slog.Default.
func NewPublisher(
	bus *events.Bus[*compassv1.SubscribeCommsResponse],
	st PresenceReads,
	status LifecycleStatusResolver,
	log *slog.Logger,
) *Publisher {
	if log == nil {
		log = slog.Default()
	}
	return &Publisher{
		bus:           bus,
		st:            st,
		status:        status,
		log:           log,
		last:          make(map[store.AccountID]compassv1.AgentPresence),
		lastState:     make(map[store.AccountID]compassv1.AgentSessionState),
		notify:        make(chan struct{}, 1),
		statusTimeout: presenceStatusTimeout,
	}
}

// presenceFor maps a lifecycle state plus the open-ask overlay onto the 4-state
// presence projection (D4, design.md:864-867). WAITING overrides IDLE: a live
// STARTING/READY session whose agent has an open ask shows WAITING, not IDLE. A
// WORKING agent is "actively running" and stays WORKING even with an open ask.
// Every terminal/unknown state (STOPPED/ERRORED/DISCONNECTED/UNSPECIFIED, i.e.
// no live session) is OFFLINE.
func presenceFor(state compassv1.AgentSessionState, openAsk bool) compassv1.AgentPresence {
	switch state {
	case compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING:
		return compassv1.AgentPresence_AGENT_PRESENCE_WORKING
	case compassv1.AgentSessionState_AGENT_SESSION_STATE_STARTING,
		compassv1.AgentSessionState_AGENT_SESSION_STATE_READY:
		if openAsk {
			return compassv1.AgentPresence_AGENT_PRESENCE_WAITING
		}
		return compassv1.AgentPresence_AGENT_PRESENCE_IDLE
	default:
		// STOPPED / ERRORED / DISCONNECTED / UNSPECIFIED, or no recorded session.
		return compassv1.AgentPresence_AGENT_PRESENCE_OFFLINE
	}
}

// PresenceSnapshot returns the last-published presence per agent — the snapshot
// surface a future ListAccounts reader will consume (presence is ephemeral;
// there is no durable table). No such reader is wired yet. A copy under the
// lock, so the caller iterates without holding publisher state.
func (p *Publisher) PresenceSnapshot() map[store.AccountID]compassv1.AgentPresence {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[store.AccountID]compassv1.AgentPresence, len(p.last))
	maps.Copy(out, p.last)
	return out
}

// publishIfChanged publishes an AgentPresenceChanged for account only when
// presence differs from the last published for it (design.md:481-485) — the
// exactly-one-per-change guarantee. It records the new value under the lock and
// publishes outside it, so the bus Publish never runs under mu.
func (p *Publisher) publishIfChanged(account store.AccountID, presence compassv1.AgentPresence) {
	p.mu.Lock()
	if prev, ok := p.last[account]; ok && prev == presence {
		p.mu.Unlock()
		return
	}
	p.last[account] = presence
	p.mu.Unlock()

	p.bus.Publish(&compassv1.SubscribeCommsResponse{
		Payload: &compassv1.SubscribeCommsResponse_AgentPresenceChanged{
			AgentPresenceChanged: &compassv1.AgentPresenceChanged{
				AgentAccountId: string(account),
				Presence:       presence,
			},
		},
	})
}
