//go:build unix

// The Server-side repackaging that turns an internal agent→Runner SessionFrame
// into the public AgentSessionFrame streamed on SubscribeAgentSession. The
// Runner relay is payload-agnostic — it forwards whole AgentFrame lines — so
// the internal→public mapping happens here, at the tail sink, not on the relay
// path.
package server

import (
	"sync"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// tailBuffer is the per-subscriber queue depth. A subscriber that falls this
// far behind the live trace is dropped (its channel closed) rather than
// stalling RelaySessionFrame — the observation pane is a live tail, so dropping
// one slow subscriber must never block delivery to the session's others. A
// dropped client sees a clean stream end and recovers by re-subscribing from the
// current tail (there is no lag signal or replay this increment). Sized to
// absorb a normal render-loop hiccup without dropping.
const tailBuffer = 256

// sessionTail is the real SessionTailSink: a per-session live
// fan-out from the single RelaySessionFrame writer (the RunnerHub delivery path)
// to the N SubscribeAgentSession subscribers of that session. It is the
// live-tail wiring the frozen record scopes for this increment — no replay ring,
// no resync, no reattach window (those are the deferred daemon-lifecycle
// machinery, compass.proto AgentSessionState notes). A subscriber joins at the
// live head and receives frames until it disconnects, the session ends, or it
// lags past its buffer. Safe for concurrent use.
type sessionTail struct {
	mu   sync.Mutex
	subs map[string]map[*tailSub]struct{}
}

// tailSub is one SubscribeAgentSession subscriber's live queue: the buffered
// channel the fan-out sends on and the handler receives from. A full buffer
// drops the subscriber by closing this channel, which the handler sees as a
// clean stream end — a lag-drop is deliberately indistinguishable from any other
// end this increment (no lag signal until the deferred resync work lands).
type tailSub struct {
	ch chan *compassv1.AgentSessionFrame
}

func newSessionTail() *sessionTail {
	return &sessionTail{subs: make(map[string]map[*tailSub]struct{})}
}

// RelaySessionFrame is the SessionTailSink implementation: repackage the
// internal frame to its public form once and fan it out to every live
// subscriber of sessionID. The send is non-blocking — a subscriber whose buffer
// is full has its channel closed, dropping it rather than stalling delivery for
// its session's other subscribers; the dropped client sees a clean stream end
// and recovers by re-subscribing from the current tail. A session with no
// subscribers is a no-op.
//
// When the frame carries a terminal lifecycle state (STOPPED or ERRORED), the
// session has ended: after delivering it, close every remaining subscriber so a
// healthy client drains the final frame from its buffer and then sees EOF,
// rather than blocking forever on a stream that will never carry another frame.
// DISCONNECTED is NOT terminal — a dropped Runner link awaits reattach within a
// bounded window (compass.proto AgentSessionState), so those subscribers stay
// open.
//
// Concurrency preconditions (both upheld by the RunnerHub delivery path, the
// sole caller): (1) frames are single-writer per session — RelaySessionFrame is
// called serially for a given sessionID, so the per-call mutex gives frame
// ORDERING, not just per-call atomicity; a second concurrent relay for one
// session would interleave nondeterministically at subscribers. (2) The public
// frame (and the SessionEvent pointer it shares) is treated as immutable once
// relayed: one *AgentSessionFrame is fanned out to all subscribers and marshaled
// concurrently by their stream.Send goroutines, which is race-free only because
// nothing mutates the frame or its typed_event after this point.
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

// isTerminalState reports whether a lifecycle state ends the session's live
// tail. STOPPED (clean end) and ERRORED (unexpected exit; recovery is an
// explicit ReloadAgentSession, never an auto-reconnect) are terminal.
// DISCONNECTED is not: a dropped Runner link awaits reattach within a bounded
// window, so its subscribers keep waiting rather than seeing a false EOF.
func isTerminalState(s compassv1.AgentSessionState) bool {
	return s == compassv1.AgentSessionState_AGENT_SESSION_STATE_STOPPED ||
		s == compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED
}

// subscribe registers a live subscriber for sessionID and returns it. The caller
// (SubscribeAgentSession) MUST call unsubscribe when its stream ends, on every
// exit path, to free the fan-out slot.
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

// unsubscribe removes a subscriber and closes its channel, so a handler blocked
// on receive unblocks. Idempotent-safe for a given sub via the presence check.
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

// toPublicFrame maps an internal SessionFrame (the agent→Runner envelope) onto
// the public AgentSessionFrame the SubscribeAgentSession stream carries. The
// typed_event is already the PUBLIC SessionEvent type — the internal envelope
// references it across the internal→public import — so it transfers by pointer
// with no re-encoding. state transfers as-is: AGENT_SESSION_STATE_UNSPECIFIED
// means "no transition, trace only" and a nil typed_event means "pure lifecycle
// frame", so a frame carrying only one of the two maps faithfully (the other
// field stays at its zero value, which is exactly its "absent" encoding). The
// session_id is stamped from the tail sink's routing key, not the frame body.
func toPublicFrame(sessionID string, f *compassv1internal.SessionFrame) *compassv1.AgentSessionFrame {
	return &compassv1.AgentSessionFrame{
		SessionId: sessionID,
		Event:     f.GetTypedEvent(),
		State:     f.GetState(),
	}
}
