//go:build unix

package presence

// The presence projection's acceptance cases (SEA-1569 T8, design record D4,
// design.md:880-890), RED-first. Each drives the publisher through the real
// events bus + hand-written fakes and gates on the recorder's observed
// AgentPresenceChanged publishes — never a sleep, never a retry
// (rule://no-retries). context.Background() is the test root
// (rule://go-thread-context exemption for _test.go); it is threaded into Run via
// startPublisher and never re-rooted below.

import (
	"testing"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// TestPresenceForMapsD4Table is the pure mapper against D4's exact table
// (design.md:864-867): WORKING→WORKING; STARTING/READY→IDLE (or WAITING with an
// open ask, WAITING > IDLE); terminal/unknown→OFFLINE (open ask irrelevant).
func TestPresenceForMapsD4Table(t *testing.T) {
	tests := []struct {
		name    string
		state   compassv1.AgentSessionState
		openAsk bool
		want    compassv1.AgentPresence
	}{
		{"working", compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING, false, compassv1.AgentPresence_AGENT_PRESENCE_WORKING},
		{"working with open ask stays working", compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING, true, compassv1.AgentPresence_AGENT_PRESENCE_WORKING},
		{"starting idle", compassv1.AgentSessionState_AGENT_SESSION_STATE_STARTING, false, compassv1.AgentPresence_AGENT_PRESENCE_IDLE},
		{"ready idle", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, false, compassv1.AgentPresence_AGENT_PRESENCE_IDLE},
		{"starting with open ask waits", compassv1.AgentSessionState_AGENT_SESSION_STATE_STARTING, true, compassv1.AgentPresence_AGENT_PRESENCE_WAITING},
		{"ready with open ask waits (WAITING > IDLE)", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, true, compassv1.AgentPresence_AGENT_PRESENCE_WAITING},
		{"stopped offline", compassv1.AgentSessionState_AGENT_SESSION_STATE_STOPPED, false, compassv1.AgentPresence_AGENT_PRESENCE_OFFLINE},
		{"errored offline", compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED, false, compassv1.AgentPresence_AGENT_PRESENCE_OFFLINE},
		{"disconnected offline", compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED, false, compassv1.AgentPresence_AGENT_PRESENCE_OFFLINE},
		{"disconnected with open ask still offline", compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED, true, compassv1.AgentPresence_AGENT_PRESENCE_OFFLINE},
		{"unspecified offline", compassv1.AgentSessionState_AGENT_SESSION_STATE_UNSPECIFIED, false, compassv1.AgentPresence_AGENT_PRESENCE_OFFLINE},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := presenceFor(tc.state, tc.openAsk); got != tc.want {
				t.Fatalf("presenceFor(%v, openAsk=%v) = %v, want %v", tc.state, tc.openAsk, got, tc.want)
			}
		})
	}
}

// TestLifecycleTransitionPublishesExactlyOne: a lifecycle transition publishes
// EXACTLY ONE AgentPresenceChanged (publish-on-change); a repeat of the same
// state publishes zero more.
func TestLifecycleTransitionPublishesExactlyOne(t *testing.T) {
	p, _, _, bus := newTestPublisher(t)
	rec := startRecorder(t, bus)
	startPublisher(t, p)
	const agent store.AccountID = "agent-a"

	// READY (no open ask) → IDLE: one publish.
	p.OnSessionLifecycle(agent, "sess-a", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	rec.waitForPublishes(t, 1)
	if got := rec.snapshot(); got[0].account != agent || got[0].presence != compassv1.AgentPresence_AGENT_PRESENCE_IDLE {
		t.Fatalf("first publish = %+v, want {agent-a, IDLE}", got[0])
	}

	// A repeat of the same lifecycle state recomputes IDLE again — no change, so
	// no second publish. WORKING then differs and publishes.
	p.OnSessionLifecycle(agent, "sess-a", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	p.OnSessionLifecycle(agent, "sess-a", compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING)
	rec.waitForPublishes(t, 2)

	got := rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("publishes = %d, want 2 (the repeated READY must not republish)", len(got))
	}
	if got[1].presence != compassv1.AgentPresence_AGENT_PRESENCE_WORKING {
		t.Fatalf("second publish = %+v, want WORKING", got[1])
	}
}

// TestAskOpenPublishesWaitingThenAnsweredPublishesPrior is the comms-bus ask
// arm: with the agent live READY (already IDLE), opening an ask publishes exactly
// one AgentPresenceChanged (→ WAITING), and answering it publishes exactly one
// (→ the prior state, IDLE). No concurrent lifecycle transition.
func TestAskOpenPublishesWaitingThenAnsweredPublishesPrior(t *testing.T) {
	p, reads, _, bus := newTestPublisher(t)
	rec := startRecorder(t, bus)
	startPublisher(t, p)
	const agent store.AccountID = "agent-a"

	// Establish the agent as live READY → IDLE (one publish), and record its
	// last-known lifecycle so the ask arm layers WAITING on READY, not OFFLINE.
	p.OnSessionLifecycle(agent, "sess-a", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	rec.waitForPublishes(t, 1)

	// Open an ask: the store now reports an open ask; a MessagePosted carrying an
	// ask drives the ask arm to recompute → WAITING.
	reads.setOpen(agent, true)
	bus.Publish(askPosted(agent))
	rec.waitForPublishes(t, 2)
	if got := rec.snapshot()[1]; got.presence != compassv1.AgentPresence_AGENT_PRESENCE_WAITING {
		t.Fatalf("ask-open publish = %+v, want WAITING", got)
	}

	// Answer it: the store now reports no open ask; a MessageUpdated drives the
	// arm to recompute → back to IDLE (the prior state).
	reads.setOpen(agent, false)
	bus.Publish(askAnsweredUpdate(agent))
	rec.waitForPublishes(t, 3)

	got := rec.snapshot()
	if len(got) != 3 {
		t.Fatalf("publishes = %d, want 3 (IDLE, WAITING, IDLE)", len(got))
	}
	if got[2].presence != compassv1.AgentPresence_AGENT_PRESENCE_IDLE {
		t.Fatalf("ask-answered publish = %+v, want IDLE (return to prior state)", got[2])
	}
}

// TestNonAskPostedDoesNotPublish: a MessagePosted with only a text block is not
// an ask-open trigger, so it publishes nothing (the ask arm ignores it).
func TestNonAskPostedDoesNotPublish(t *testing.T) {
	p, reads, _, bus := newTestPublisher(t)
	rec := startRecorder(t, bus)
	startPublisher(t, p)
	const agent store.AccountID = "agent-a"

	p.OnSessionLifecycle(agent, "sess-a", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	rec.waitForPublishes(t, 1)

	// A text post while the store (hypothetically) has an open ask must STILL not
	// trigger the arm — only an ask-carrying post is a trigger.
	reads.setOpen(agent, true)
	bus.Publish(textPosted(agent))
	// Drive a real ask post right after; when THAT publish lands, a spurious
	// text-triggered publish (if any) would already be recorded before it.
	bus.Publish(askPosted(agent))
	rec.waitForPublishes(t, 2)

	got := rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("publishes = %d, want 2 (IDLE then WAITING; the text post must not publish)", len(got))
	}
	if got[1].presence != compassv1.AgentPresence_AGENT_PRESENCE_WAITING {
		t.Fatalf("second publish = %+v, want WAITING (from the ask post, not the text post)", got[1])
	}
}

// TestReconcileReadyWithOpenAskRebuildsWaiting: restart reconciliation via
// OnSessionPromoted with the Status relay reporting a live READY session and the
// store reporting an open ask reconstructs WAITING (NOT IDLE) — GetAgentStatus
// returns only lifecycle, so without the store overlay pass a WAITING agent
// would rebuild as IDLE across a restart (design.md:883-884).
func TestReconcileReadyWithOpenAskRebuildsWaiting(t *testing.T) {
	p, reads, status, bus := newTestPublisher(t)
	rec := startRecorder(t, bus)
	startPublisher(t, p)
	const agent store.AccountID = "agent-a"

	status.set("sess-a", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	reads.setOpen(agent, true)

	p.OnSessionPromoted(agent, "sess-a")
	rec.waitForPublishes(t, 1)

	if got := rec.snapshot()[0]; got.account != agent || got.presence != compassv1.AgentPresence_AGENT_PRESENCE_WAITING {
		t.Fatalf("reconstructed presence = %+v, want {agent-a, WAITING} (READY + open ask, not IDLE)", got)
	}
}

// TestReconcileWorkingRebuildsWithoutTransition: a long-WORKING agent that emits
// no lifecycle frame reconstructs WORKING at promotion, from the Status relay
// alone — the restart case a lifecycle-only projection would miss.
func TestReconcileWorkingRebuildsWithoutTransition(t *testing.T) {
	p, _, status, bus := newTestPublisher(t)
	rec := startRecorder(t, bus)
	startPublisher(t, p)
	const agent store.AccountID = "agent-a"

	status.set("sess-a", compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING)

	p.OnSessionPromoted(agent, "sess-a")
	rec.waitForPublishes(t, 1)

	if got := rec.snapshot()[0]; got.presence != compassv1.AgentPresence_AGENT_PRESENCE_WORKING {
		t.Fatalf("reconstructed presence = %+v, want WORKING (rebuilt from Status without a transition)", got)
	}
}

// TestSnapshotReturnsLastPublished: PresenceSnapshot returns the last-published
// state per agent.
func TestSnapshotReturnsLastPublished(t *testing.T) {
	p, reads, _, bus := newTestPublisher(t)
	rec := startRecorder(t, bus)
	startPublisher(t, p)
	const agentA, agentB store.AccountID = "agent-a", "agent-b"

	// agentB has an open ask, so its READY session projects WAITING — this also
	// exercises setOpen for an agent other than agent-a.
	reads.setOpen(agentB, true)
	p.OnSessionLifecycle(agentA, "sess-a", compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING)
	p.OnSessionLifecycle(agentB, "sess-b", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	rec.waitForPublishes(t, 2)

	snap := p.PresenceSnapshot()
	if snap[agentA] != compassv1.AgentPresence_AGENT_PRESENCE_WORKING {
		t.Fatalf("snapshot[agent-a] = %v, want WORKING", snap[agentA])
	}
	if snap[agentB] != compassv1.AgentPresence_AGENT_PRESENCE_WAITING {
		t.Fatalf("snapshot[agent-b] = %v, want WAITING (READY + open ask)", snap[agentB])
	}
	// The snapshot is a copy: mutating it does not affect the publisher's state.
	snap[agentA] = compassv1.AgentPresence_AGENT_PRESENCE_OFFLINE
	if p.PresenceSnapshot()[agentA] != compassv1.AgentPresence_AGENT_PRESENCE_WORKING {
		t.Fatalf("snapshot mutation leaked into publisher state")
	}
}
