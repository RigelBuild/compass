//go:build unix

package server

// Tail-sink contracts. Two seams:
//
//   - toPublicFrame: the internal→public repackaging. The typed_event pointer
//     and the state enum transfer faithfully, and session_id is ALWAYS stamped
//     from the routing key, never read from the frame body — a bug that read
//     session_id from the body would let a mislabeled frame route to the wrong
//     subscriber.
//   - sessionTail: the per-session live fan-out. Isolation across session ids,
//     N-way fan-out, lag-drop that never stalls a session's other subscribers,
//     unsubscribe that unblocks a receiver and is double-call safe, and a
//     no-subscriber relay that is a clean no-op. Exercised under -race.

import (
	"sync"
	"testing"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
)

// assistantEvent builds a public SessionEvent carrying one assistant-text body,
// the trace payload the tail fans out.
func assistantEvent(text string) *compassv1.SessionEvent {
	return &compassv1.SessionEvent{
		Event: &compassv1.SessionEvent_AssistantText{
			AssistantText: &compassv1.SessionAssistantText{Text: text},
		},
	}
}

// TestToPublicFrameRepackaging pins the internal→public mapping across the
// event/state matrix. The contract: typed_event transfers by pointer (same
// SessionEvent, no re-encode), state transfers as-is, and session_id is stamped
// from the arg — even when the frame carries no identity of its own. An empty
// field encodes "absent", so an event-only frame must leave state UNSPECIFIED
// and a state-only frame must leave the event nil.
func TestToPublicFrameRepackaging(t *testing.T) {
	ev := assistantEvent("hello")

	for _, tc := range []struct {
		name      string
		frame     *compassv1internal.SessionFrame
		wantEvent *compassv1.SessionEvent // exact pointer expected, or nil
		wantText  string                  // "" when no event
		wantState compassv1.AgentSessionState
	}{
		{
			name:      "event-only",
			frame:     &compassv1internal.SessionFrame{TypedEvent: ev},
			wantEvent: ev,
			wantText:  "hello",
			wantState: compassv1.AgentSessionState_AGENT_SESSION_STATE_UNSPECIFIED,
		},
		{
			name:      "state-only",
			frame:     &compassv1internal.SessionFrame{State: compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING},
			wantEvent: nil,
			wantState: compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING,
		},
		{
			name: "both-set",
			frame: &compassv1internal.SessionFrame{
				TypedEvent: ev,
				State:      compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING,
			},
			wantEvent: ev,
			wantText:  "hello",
			wantState: compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub := toPublicFrame("sess-routing", tc.frame)

			if pub.GetSessionId() != "sess-routing" {
				t.Fatalf("session_id = %q, want sess-routing (stamped from the routing key)", pub.GetSessionId())
			}
			// Pointer identity: the public type IS the internal envelope's
			// referenced type, so it transfers without a re-encode.
			if pub.GetEvent() != tc.wantEvent {
				t.Fatalf("event pointer = %p, want %p (transfer by pointer, no re-encode)", pub.GetEvent(), tc.wantEvent)
			}
			if got := pub.GetEvent().GetAssistantText().GetText(); got != tc.wantText {
				t.Fatalf("event text = %q, want %q", got, tc.wantText)
			}
			if pub.GetState() != tc.wantState {
				t.Fatalf("state = %v, want %v", pub.GetState(), tc.wantState)
			}
		})
	}
}

// TestToPublicFrameSessionIdFromArgNotBody is the routing-key invariant on its
// own: the public frame's session_id is the arg, unconditionally. The internal
// SessionFrame has no session_id field of its own — the routing key lives at the
// sink — so this pins that the public stream is labeled by where the tail
// delivered it, not by anything a frame body could spoof.
func TestToPublicFrameSessionIdFromArgNotBody(t *testing.T) {
	pub := toPublicFrame("real-session", &compassv1internal.SessionFrame{TypedEvent: assistantEvent("x")})
	if pub.GetSessionId() != "real-session" {
		t.Fatalf("session_id = %q, want real-session (always the arg)", pub.GetSessionId())
	}
}

// traceFrame is an internal SessionFrame carrying one assistant-text trace, the
// unit the fan-out relays.
func traceFrame(text string) *compassv1internal.SessionFrame {
	return &compassv1internal.SessionFrame{TypedEvent: assistantEvent(text)}
}

// recv pulls the next frame from a subscriber, failing on a closed channel — a
// received frame proves delivery; a close proves a lag-drop or unsubscribe.
func recv(t *testing.T, sub *tailSub) *compassv1.AgentSessionFrame {
	t.Helper()
	f, ok := <-sub.ch
	if !ok {
		t.Fatal("subscriber channel closed, want a frame")
	}
	return f
}

// TestFanOutIsolatesBySessionID pins per-session isolation: a frame relayed for
// one session reaches only that session's subscriber, never a subscriber on a
// different session. A routing bug that fanned to all sessions would leak one
// agent's trace onto another's stream.
func TestFanOutIsolatesBySessionID(t *testing.T) {
	tail := newSessionTail()
	subA := tail.subscribe("sess-a")
	subB := tail.subscribe("sess-b")

	tail.RelaySessionFrame("sess-a", traceFrame("for-a"))

	got := recv(t, subA)
	if got.GetEvent().GetAssistantText().GetText() != "for-a" {
		t.Fatalf("sess-a got %q, want for-a", got.GetEvent().GetAssistantText().GetText())
	}
	// subB must NOT have received the sess-a frame.
	select {
	case f := <-subB.ch:
		t.Fatalf("sess-b received %v, want nothing (fan-out is per session id)", f)
	default:
	}
}

// TestFanOutToAllSubscribers pins N-way fan-out: every live subscriber on a
// session receives the one relayed frame. A bug that delivered to only the
// first (or a random one) would silently starve the other observers.
func TestFanOutToAllSubscribers(t *testing.T) {
	tail := newSessionTail()
	const n = 5
	subs := make([]*tailSub, n)
	for i := range subs {
		subs[i] = tail.subscribe("sess-1")
	}

	tail.RelaySessionFrame("sess-1", traceFrame("broadcast"))

	for i, sub := range subs {
		got := recv(t, sub)
		if got.GetEvent().GetAssistantText().GetText() != "broadcast" {
			t.Fatalf("subscriber %d got %q, want broadcast", i, got.GetEvent().GetAssistantText().GetText())
		}
	}
}

// TestLagDropDoesNotBlockOtherSubscribers pins the lag-drop contract: a
// subscriber that fills past tailBuffer is dropped (its channel closed) while a
// healthy subscriber on the same session keeps receiving — the relay never
// blocks on the full one. A bug that blocked the writer on a full buffer would
// stall the whole session's delivery behind one slow client.
func TestLagDropDoesNotBlockOtherSubscribers(t *testing.T) {
	tail := newSessionTail()
	slow := tail.subscribe("sess-1") // never drains
	fast := tail.subscribe("sess-1") // drained in lockstep below

	// Overfill: relay tailBuffer+1 frames. The slow subscriber's buffer fills
	// and the (buffer+1)-th send drops it. The fast subscriber is drained each
	// iteration so it never fills and stays live throughout.
	for i := 0; i <= tailBuffer; i++ {
		tail.RelaySessionFrame("sess-1", traceFrame("f"))
		// Drain the fast subscriber so it does not itself lag.
		got := recv(t, fast)
		if got.GetEvent().GetAssistantText().GetText() != "f" {
			t.Fatalf("fast subscriber frame %d = %q, want f (relay must keep flowing to the healthy sub)", i, got.GetEvent().GetAssistantText().GetText())
		}
	}

	// The slow subscriber was lag-dropped: its channel is closed. Drain the
	// buffered frames first, then the close sentinel arrives and ends the range.
	drained := 0
	for range slow.ch {
		drained++
	}
	if drained != tailBuffer {
		t.Fatalf("slow subscriber buffered %d frames before drop, want %d (tailBuffer)", drained, tailBuffer)
	}
}

// TestUnsubscribeClosesChannelAndUnblocksReceiver pins unsubscribe: it closes
// the subscriber's channel so a receiver blocked on it unblocks (the handler's
// exit signal), and a second unsubscribe of the same sub is a safe no-op — the
// presence guard prevents a double-close panic on a handler that unsubscribes
// twice on overlapping exit paths.
func TestUnsubscribeClosesChannelAndUnblocksReceiver(t *testing.T) {
	tail := newSessionTail()
	sub := tail.subscribe("sess-1")

	blocked := make(chan struct{})
	go func() {
		// Blocks until the channel is closed by unsubscribe.
		<-sub.ch
		close(blocked)
	}()

	tail.unsubscribe("sess-1", sub)
	<-blocked // a receive on the closed channel returns, unblocking the handler

	// Double unsubscribe is safe (presence-guarded, no double close panic).
	tail.unsubscribe("sess-1", sub)
}

// TestRelayNoSubscribersIsNoOp pins that relaying to a session nobody is
// subscribed to is a clean no-op — no panic on the empty (or absent) subscriber
// map. The delivery path runs for every frame whether or not an observer is
// attached.
func TestRelayNoSubscribersIsNoOp(t *testing.T) {
	tail := newSessionTail()
	tail.RelaySessionFrame("ghost-session", traceFrame("into-the-void"))

	// A subscriber to a different session confirms the no-op did not corrupt the
	// map: it can still subscribe and receive.
	sub := tail.subscribe("sess-1")
	tail.RelaySessionFrame("sess-1", traceFrame("live"))
	if got := recv(t, sub); got.GetEvent().GetAssistantText().GetText() != "live" {
		t.Fatalf("post-noop relay = %q, want live", got.GetEvent().GetAssistantText().GetText())
	}
}

// TestConcurrentSubscribeRelayUnsubscribe exercises the fan-out under concurrent
// subscribe / relay / unsubscribe on overlapping sessions, so `-race` can catch
// an unsynchronized map access or a send-on-closed-channel race. It asserts the
// tail survives the churn: a fresh subscriber after the storm still receives a
// relayed frame (the map is intact and consistent).
func TestConcurrentSubscribeRelayUnsubscribe(t *testing.T) {
	tail := newSessionTail()
	sessions := []string{"s0", "s1", "s2"}

	var wg sync.WaitGroup
	for _, sid := range sessions {
		for range 8 {
			wg.Add(1)
			go func(sid string) {
				defer wg.Done()
				sub := tail.subscribe(sid)
				// Drain concurrently so a relay never blocks us during the race.
				done := make(chan struct{})
				go func() {
					for range sub.ch {
					}
					close(done)
				}()
				tail.RelaySessionFrame(sid, traceFrame("x"))
				tail.unsubscribe(sid, sub)
				<-done
			}(sid)
		}
	}
	// Concurrent relayers with no coordination against the subscribers above.
	for _, sid := range sessions {
		wg.Add(1)
		go func(sid string) {
			defer wg.Done()
			for range 16 {
				tail.RelaySessionFrame(sid, traceFrame("y"))
			}
		}(sid)
	}
	wg.Wait()

	// After the churn the tail is still consistent: a new subscriber receives.
	sub := tail.subscribe("after")
	tail.RelaySessionFrame("after", traceFrame("survived"))
	if got := recv(t, sub); got.GetEvent().GetAssistantText().GetText() != "survived" {
		t.Fatalf("post-churn relay = %q, want survived (tail intact after concurrent churn)", got.GetEvent().GetAssistantText().GetText())
	}
}

// stateFrame is an internal SessionFrame carrying only a lifecycle state (no
// trace event), the pure lifecycle frame RelaySessionFrame classifies as
// terminal or not via isTerminalState.
func stateFrame(s compassv1.AgentSessionState) *compassv1internal.SessionFrame {
	return &compassv1internal.SessionFrame{State: s}
}

// recvClosed asserts the subscriber's channel is closed with nothing left to
// drain — the EOF a healthy client reads after the terminal frame. A frame still
// buffered (ok == true) or a still-open channel both fail here.
func recvClosed(t *testing.T, sub *tailSub) {
	t.Helper()
	f, ok := <-sub.ch
	if ok {
		t.Fatalf("channel yielded %v, want closed (EOF after the terminal frame)", f)
	}
}

// TestTerminalFrameClosesHealthySubscribersAfterDelivery is the core regression:
// a terminal lifecycle frame (STOPPED / ERRORED) must be DELIVERED to every
// healthy subscriber AND then close their channels, so a client drains the final
// frame and reads EOF instead of blocking forever on a stream that will never
// carry another frame. Before the fix the second receive blocked. Commenting out
// the `if isTerminalState(...)` block in RelaySessionFrame reddens the recvClosed
// assertion (the channel stays open).
func TestTerminalFrameClosesHealthySubscribersAfterDelivery(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state compassv1.AgentSessionState
	}{
		{"stopped", compassv1.AgentSessionState_AGENT_SESSION_STATE_STOPPED},
		{"errored", compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tail := newSessionTail()
			const n = 2
			subs := make([]*tailSub, n)
			for i := range subs {
				subs[i] = tail.subscribe("sess-1")
			}

			tail.RelaySessionFrame("sess-1", stateFrame(tc.state))

			for i, sub := range subs {
				// The terminal frame is drainable first...
				got := recv(t, sub)
				if got.GetState() != tc.state {
					t.Fatalf("subscriber %d state = %v, want %v (terminal frame delivered before close)", i, got.GetState(), tc.state)
				}
				// ...then the channel is closed: the client sees EOF, not a block.
				recvClosed(t, sub)
			}
		})
	}
}

// TestDisconnectedFrameKeepsSubscribersOpen pins the DISCONNECTED exclusion:
// DISCONNECTED is NOT terminal — a dropped Runner link awaits reattach within a
// bounded window — so its subscribers receive the frame and STAY open, ready for
// the next frame. Deleting DISCONNECTED's exclusion from isTerminalState (making
// it terminal) reddens this: the follow-up frame would never arrive on a closed
// channel.
func TestDisconnectedFrameKeepsSubscribersOpen(t *testing.T) {
	tail := newSessionTail()
	sub := tail.subscribe("sess-1")

	tail.RelaySessionFrame("sess-1", stateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED))
	if got := recv(t, sub); got.GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED {
		t.Fatalf("state = %v, want DISCONNECTED", got.GetState())
	}

	// The channel stays open: a subsequent frame is still deliverable to the same
	// subscriber (proves DISCONNECTED did not close the stream).
	tail.RelaySessionFrame("sess-1", traceFrame("after-reattach"))
	if got := recv(t, sub); got.GetEvent().GetAssistantText().GetText() != "after-reattach" {
		t.Fatalf("post-disconnect frame = %q, want after-reattach (DISCONNECTED subscribers stay open)", got.GetEvent().GetAssistantText().GetText())
	}
}

// TestTerminalRelayThenUnsubscribeNoDoubleClose pins that the handler's deferred
// unsubscribe, which always runs on stream exit, is a safe no-op after a terminal
// relay already closed the subscriber. The fix deletes the sub from the map
// before closing, so unsubscribe's presence check skips the re-close — a second
// close would panic. Draining first mirrors a real handler that read the frame.
func TestTerminalRelayThenUnsubscribeNoDoubleClose(t *testing.T) {
	tail := newSessionTail()
	sub := tail.subscribe("sess-1")

	tail.RelaySessionFrame("sess-1", stateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_STOPPED))
	_ = recv(t, sub)   // drain the terminal frame
	recvClosed(t, sub) // channel already closed by the terminal relay

	// The deferred unsubscribe must not double-close (would panic); the sub is
	// gone from the map, so the presence check makes this a no-op.
	tail.unsubscribe("sess-1", sub)
}

// TestTerminalRelayCleansUpSessionMap pins that a terminal relay leaves the
// session with no live subscribers removed from the map: a follow-up relay to the
// same id is a clean no-op on an absent session (no panic, nothing delivered),
// and a fresh subscriber to that id starts empty. A leak would keep dead
// subscriber sets around indefinitely.
func TestTerminalRelayCleansUpSessionMap(t *testing.T) {
	tail := newSessionTail()
	old := tail.subscribe("sess-1")

	tail.RelaySessionFrame("sess-1", stateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_STOPPED))
	_ = recv(t, old)
	recvClosed(t, old)

	// A fresh subscriber to the same id joins an empty session and only sees
	// frames relayed after it subscribed — proving the terminal relay cleared the
	// old subscriber set rather than leaking it.
	fresh := tail.subscribe("sess-1")
	tail.RelaySessionFrame("sess-1", traceFrame("new-session"))
	if got := recv(t, fresh); got.GetEvent().GetAssistantText().GetText() != "new-session" {
		t.Fatalf("fresh subscriber frame = %q, want new-session (session map reusable after terminal cleanup)", got.GetEvent().GetAssistantText().GetText())
	}
	select {
	case f := <-fresh.ch:
		t.Fatalf("fresh subscriber saw an extra frame %v, want only new-session (no leaked delivery)", f)
	default:
	}
}
