//go:build unix

// SessionTail repackages internal Runner frames as public AgentSessionFrame
// values at the subscription sink; the relay stays payload-agnostic.
package server

import (
	"sync"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// tailBuffer is the per-subscriber queue depth. Full queues close only that
// subscriber rather than stalling other consumers.
const tailBuffer = 256

// sessionTail fans Runner frames to live subscribers. Subscribers join at the
// live head and end on disconnect, session end, or lag; there is no replay.
type sessionTail struct {
	mu   sync.Mutex
	subs map[string]map[*tailSub]struct{}
}

// tailSub holds one subscriber queue; a full buffer closes its stream without
// distinguishing lag from other ends.
type tailSub struct {
	ch chan *compassv1.AgentSessionFrame
}

func newSessionTail() *sessionTail {
	return &sessionTail{subs: make(map[string]map[*tailSub]struct{})}
}

// RelaySessionFrame repackages frames and fans them out without blocking; a full
// buffer drops only that subscriber. Terminal frames close subscribers, but
// DISCONNECTED stays open during the bounded reattach window. The hub serializes
// writes per session, so subscribers can marshal shared frames concurrently.
func (t *sessionTail) RelaySessionFrame(sessionID string, frame *compassv1internal.SessionFrame) {
	public := toPublicFrame(sessionID, frame)
	t.mu.Lock()
	defer t.mu.Unlock()
	for sub := range t.subs[sessionID] {
		select {
		case sub.ch <- public:
		default:
			// Buffer full: drop this subscriber rather than block the writer.
			delete(t.subs[sessionID], sub)
			close(sub.ch)
		}
	}
	if isTerminalState(frame.GetState()) {
		// Session ended: close every subscriber that still holds the buffered
		// terminal frame so it drains that frame and then reads EOF. Deleting
		// from the map before close keeps the handler's deferred unsubscribe a
		// no-op (its presence check fails), so the channel is never double-closed.
		for sub := range t.subs[sessionID] {
			delete(t.subs[sessionID], sub)
			close(sub.ch)
		}
	}
	if len(t.subs[sessionID]) == 0 {
		delete(t.subs, sessionID)
	}
}

// STOPPED and ERRORED end live tails; ERRORED requires explicit recovery.
// DISCONNECTED waits for bounded Runner reattach and must not appear as EOF.
func isTerminalState(s compassv1.AgentSessionState) bool {
	return s == compassv1.AgentSessionState_AGENT_SESSION_STATE_STOPPED ||
		s == compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED
}

// subscribe registers a live subscriber; the handler MUST always unsubscribe
// when its stream ends to free the fan-out slot.
func (t *sessionTail) subscribe(sessionID string) *tailSub {
	sub := &tailSub{ch: make(chan *compassv1.AgentSessionFrame, tailBuffer)}
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.subs[sessionID]
	if m == nil {
		m = make(map[*tailSub]struct{})
		t.subs[sessionID] = m
	}
	m[sub] = struct{}{}
	return sub
}

// unsubscribe removes a subscriber and closes its channel to unblock receives.
// Repeated calls are safe because removal checks membership.
func (t *sessionTail) unsubscribe(sessionID string, sub *tailSub) {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.subs[sessionID]
	if m == nil {
		return
	}
	if _, ok := m[sub]; ok {
		delete(m, sub)
		close(sub.ch)
	}
	if len(m) == 0 {
		delete(t.subs, sessionID)
	}
}

// toPublicFrame maps the internal envelope to the public stream frame. The
// public typed_event transfers by pointer; state and nil-event zero values retain
// their meanings. The session ID comes from the tail's routing key.
func toPublicFrame(sessionID string, f *compassv1internal.SessionFrame) *compassv1.AgentSessionFrame {
	return &compassv1.AgentSessionFrame{
		SessionId:    sessionID,
		Event:        f.GetTypedEvent(),
		State:        f.GetState(),
		TurnSequence: f.GetTurnSequence(),
	}
}
