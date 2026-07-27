//go:build unix

// Package board is the Server-side Bridge board: the authoritative projection of
// agent workstream state. It consumes the agent-session lifecycle transitions the
// RunnerHub delivers (runnerhub.LifecycleSink) and exposes them two ways off one
// source of truth: a point-in-time snapshot for GetAgentStatus, and a live
// fan-out on SubscribeEvents. The board is a projection — it reflects workstream
// state, so it reuses the frozen AgentSessionStatus payload (compass.proto:133)
// and adds no proto surface.
//
// Recording each transition (not only publishing it onto the bus) lets
// GetAgentStatus serve a snapshot without reconstructing state from the event
// ring, which evicts past its window. One session key = one workstream in the
// single-agent-per-container MVP.
package board

import (
	"sort"
	"sync"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
)

// busPayload is the SubscribeEvents bus event type: the whole wire response with
// only its Payload oneof set, the bus stamping Seq/AtUnixMs/InstanceEpoch onto a
// copy at the stream edge (mirrors server/service.go's busPayload alias). The
// projection publishes the AgentSessionStatus variant.
type busPayload = *compassv1.SubscribeEventsResponse

// Projection is the Bridge board: the per-session aggregate of the latest
// AgentSessionState, plus the SubscribeEvents fan-out. Safe for concurrent use —
// PublishSessionStatus (the RunnerHub delivery goroutine) and Snapshot (the
// GetAgentStatus RPC goroutine) run concurrently.
type Projection struct {
	bus *events.Bus[busPayload]

	mu sync.RWMutex
	// sessions is session_id -> the latest state observed for it, terminal states
	// (STOPPED/ERRORED) included. A session is retained after it terminates so a
	// single-id GetAgentStatus(session_id) still answers a finished or crashed
	// agent's final state ("did my agent finish?"); the all-sessions Snapshot("")
	// filters terminal states out, since GetAgentStatus unfiltered contracts to
	// return every *live* session (compass.proto:49,283). Retain-all is bounded by
	// the single-Runner agent count for the MVP; lifecycle GC (eviction on the
	// reattach/expiry state machine, compass.proto:149-153) is deferred.
	sessions map[string]compassv1.AgentSessionState
}

// NewProjection constructs an empty board over the SubscribeEvents bus it fans
// lifecycle transitions onto.
func NewProjection(bus *events.Bus[busPayload]) *Projection {
	return &Projection{
		bus:      bus,
		sessions: make(map[string]compassv1.AgentSessionState),
	}
}

// PublishSessionStatus records the transition as the session's latest state and
// fans it onto SubscribeEvents, both under the write lock. It satisfies
// runnerhub.LifecycleSink (structurally, so board takes no runnerhub dependency):
// the RunnerHub extracts one AgentSessionStatus per session frame carrying a
// non-UNSPECIFIED state and hands it here.
//
// Record and publish are atomic under p.mu: a Snapshot reader can never observe a
// recorded state whose transition has not yet reached the bus, nor a bus event
// past the recorded state — so GetAgentStatus can never disagree with the last
// AgentSessionStatus a SubscribeEvents client saw, even under concurrent writers.
// Holding the lock across the fan-out is cheap and deadlock-free: events.Bus.Publish
// is non-blocking (a per-subscriber select/default, events.go:195-201) under its
// own distinct bus.mu, and no path takes bus.mu before p.mu, so there is no lock
// inversion.
//
// A nil status, an empty session_id, or an UNSPECIFIED state is ignored: the
// RunnerHub only synthesizes a status for a real session with a set state, so any
// of these is a contract violation, not a board entry — recording one would
// create a phantom row (keyed on "" or holding the zero state). The state guard
// is defense-in-depth: the hub already skips UNSPECIFIED frames upstream, but the
// board enforces its own recorded-state invariant rather than trusting the caller.
func (p *Projection) PublishSessionStatus(status *compassv1.AgentSessionStatus) {
	if status == nil || status.GetSessionId() == "" ||
		status.GetState() == compassv1.AgentSessionState_AGENT_SESSION_STATE_UNSPECIFIED {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions[status.GetSessionId()] = status.GetState()

	p.bus.Publish(&compassv1.SubscribeEventsResponse{
		Payload: &compassv1.SubscribeEventsResponse_AgentSessionStatus{
			AgentSessionStatus: status,
		},
	})
}

// Snapshot returns the board state for GetAgentStatus. With sessionID set it
// returns that one session — including a terminal (STOPPED/ERRORED) one, so a
// caller polling a known id still learns a finished or crashed agent's final
// state — or an empty slice when the board has never seen it. With sessionID
// empty it returns every *live* session (terminal states filtered out), honoring
// GetAgentStatus's unfiltered contract to list live sessions (compass.proto:49,
// 283); the result is sorted by session_id so the response is deterministic
// regardless of map iteration order. Each entry is a fresh AgentSessionStatus the
// caller owns.
func (p *Projection) Snapshot(sessionID string) []*compassv1.AgentSessionStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if sessionID != "" {
		state, ok := p.sessions[sessionID]
		if !ok {
			return nil
		}
		return []*compassv1.AgentSessionStatus{statusOf(sessionID, state)}
	}

	out := make([]*compassv1.AgentSessionStatus, 0, len(p.sessions))
	for id, state := range p.sessions {
		if isTerminal(state) {
			continue
		}
		out = append(out, statusOf(id, state))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].GetSessionId() < out[j].GetSessionId()
	})
	return out
}

// isTerminal reports whether a session has reached a terminal state — STOPPED (a
// clean finish) or ERRORED (a crash). Terminal sessions are retained in the map
// (queryable by single id) but excluded from the all-sessions Snapshot("").
// DISCONNECTED is deliberately NOT terminal: the owning Runner dropped its link
// but the session awaits reattach within the bounded window (compass.proto:161-163),
// so it stays a live board entry until the expiry state machine (deferred)
// falls it to ERRORED.
func isTerminal(state compassv1.AgentSessionState) bool {
	return state == compassv1.AgentSessionState_AGENT_SESSION_STATE_STOPPED ||
		state == compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED
}

// statusOf builds one board entry.
func statusOf(sessionID string, state compassv1.AgentSessionState) *compassv1.AgentSessionStatus {
	return &compassv1.AgentSessionStatus{SessionId: sessionID, State: state}
}
