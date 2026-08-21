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
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
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

// TestTerminalLifecycleEdgeGoesOfflineThenDedups is M1 on the presence side: a
// bound agent driven to WORKING, then a terminal (DISCONNECTED) lifecycle edge —
// the edge the hub's teardown paths (unbindSession/enroll) now fire — publishes
// exactly one OFFLINE; a repeat terminal edge publishes nothing (publish-on-
// change dedup). RED against pre-fix: pre-fix the hub never fired a terminal edge
// on teardown, so presence stayed WORKING; this test drives the edge directly
// into the publisher, so it reddens only if presenceFor stops mapping
// DISCONNECTED→OFFLINE or dedup breaks. Its runnerhub sibling proves the hub
// actually fires it.
func TestTerminalLifecycleEdgeGoesOfflineThenDedups(t *testing.T) {
	p, _, _, bus := newTestPublisher(t)
	rec := startRecorder(t, bus)
	startPublisher(t, p)
	const agent store.AccountID = "agent-a"

	p.OnSessionLifecycle(agent, "sess-a", compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING)
	rec.waitForPublishes(t, 1)
	if got := rec.snapshot()[0]; got.presence != compassv1.AgentPresence_AGENT_PRESENCE_WORKING {
		t.Fatalf("first publish = %+v, want WORKING", got)
	}

	// Terminal edge from teardown → OFFLINE (one publish).
	p.OnSessionLifecycle(agent, "sess-a", compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED)
	rec.waitForPublishes(t, 2)
	if got := rec.snapshot()[1]; got.presence != compassv1.AgentPresence_AGENT_PRESENCE_OFFLINE {
		t.Fatalf("terminal-edge publish = %+v, want OFFLINE", got)
	}

	// A second terminal edge recomputes OFFLINE again — no change, so no publish.
	// A following distinct edge (WORKING) proves nothing published in between.
	p.OnSessionLifecycle(agent, "sess-a", compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED)
	p.OnSessionLifecycle(agent, "sess-a", compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING)
	rec.waitForPublishes(t, 3)

	got := rec.snapshot()
	if len(got) != 3 {
		t.Fatalf("publishes = %d, want 3 (WORKING, OFFLINE, WORKING; the repeat DISCONNECTED must not republish)", len(got))
	}
	if got[2].presence != compassv1.AgentPresence_AGENT_PRESENCE_WORKING {
		t.Fatalf("third publish = %+v, want WORKING", got[2])
	}
}

// TestAskFromUnknownAuthorPublishesNothing is M2: an ask authored by an account
// with NO recorded lifecycle state (a human ask author never receives a
// lifecycle edge, so is never in lastState) publishes NOTHING — the ask overlay
// applies only to agents the lifecycle arm has seen. RED against pre-fix: the
// pre-fix recomputeFromStore read lastState with a plain map read, so an absent
// key yielded UNSPECIFIED → presenceFor(UNSPECIFIED, open)=OFFLINE and published
// one spurious AgentPresenceChanged naming the human OFFLINE. The gate flips that
// to zero. Contrast: an agent WITH a recorded state authoring an ask DOES publish
// WAITING (the TestAskOpen... test above).
func TestAskFromUnknownAuthorPublishesNothing(t *testing.T) {
	p, reads, _, bus := newTestPublisher(t)
	rec := startRecorder(t, bus)
	startPublisher(t, p)
	const human store.AccountID = "human-a"
	const agent store.AccountID = "agent-a"

	// The human has an open ask in the store but NO lifecycle state recorded.
	reads.setOpen(human, true)
	bus.Publish(askPosted(human))

	// Drive a real agent ask right after; when THAT publish lands, a spurious
	// human-triggered publish (if any) would already be recorded before it. The
	// agent is established live READY (→ IDLE) first so it has a lastState entry.
	p.OnSessionLifecycle(agent, "sess-a", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	rec.waitForPublishes(t, 1)
	reads.setOpen(agent, true)
	bus.Publish(askPosted(agent))
	rec.waitForPublishes(t, 2)

	got := rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("publishes = %d, want 2 (agent IDLE then WAITING; the human ask must not publish): %+v", len(got), got)
	}
	for _, r := range got {
		if r.account == human {
			t.Fatalf("published presence for a human ask author %+v; a non-agent must never appear in the projection", r)
		}
	}
	if got[1].account != agent || got[1].presence != compassv1.AgentPresence_AGENT_PRESENCE_WAITING {
		t.Fatalf("second publish = %+v, want {agent-a, WAITING}", got[1])
	}
}

// TestBlockingResolveDoesNotWedgeFollowingEdges is M3: a wedged Runner Status
// resolve on a promotion edge must not freeze the single loop goroutine and
// starve the ask arm. With a blocking resolver and a SHORT statusTimeout, a
// promotion degrades to OFFLINE at the deadline (ok=false), and a concurrently
// enqueued ask event for a live agent is still serviced. RED against pre-fix:
// pre-fix applyPromoted called SessionState(ctx, ...) with the unbounded serve
// ctx, so the resolve never returned and drainEdges (hence the loop) blocked
// forever — the ask publish would never land and this test times out. The
// bounded resolve unblocks the loop.
func TestBlockingResolveDoesNotWedgeFollowingEdges(t *testing.T) {
	blocker := newBlockingStatus(compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	p, reads, bus := newTestPublisherWithStatus(t, blocker)
	p.statusTimeout = 20 * time.Millisecond // a DEADLINE, not a sync device; short so the wedge degrades fast
	rec := startRecorder(t, bus)
	startPublisher(t, p)
	const wedged store.AccountID = "agent-wedged"
	const live store.AccountID = "agent-live"

	// Establish the live agent so its ask has a recorded lifecycle state to layer
	// on (M2 gate); this publish lands before the wedged promotion is enqueued.
	p.OnSessionLifecycle(live, "sess-live", compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	rec.waitForPublishes(t, 1)

	// Enqueue the wedged promotion; the loop enters the blocking resolve.
	p.OnSessionPromoted(wedged, "sess-wedged")
	select {
	case <-blocker.entered:
	case <-time.After(testTimeout):
		t.Fatal("resolver never entered; the promotion edge was not applied")
	}

	// The wedged resolve degrades to OFFLINE at the deadline (one publish), and
	// the loop is free again — a following ask edge for the live agent is
	// serviced (→ WAITING). If the loop were frozen (pre-fix), neither lands.
	reads.setOpen(live, true)
	bus.Publish(askPosted(live))
	rec.waitForPublishes(t, 3)

	var sawWedgedOffline, sawLiveWaiting bool
	for _, r := range rec.snapshot() {
		if r.account == wedged && r.presence == compassv1.AgentPresence_AGENT_PRESENCE_OFFLINE {
			sawWedgedOffline = true
		}
		if r.account == live && r.presence == compassv1.AgentPresence_AGENT_PRESENCE_WAITING {
			sawLiveWaiting = true
		}
	}
	if !sawWedgedOffline {
		t.Fatalf("wedged promotion did not degrade to OFFLINE at the deadline: %+v", rec.snapshot())
	}
	if !sawLiveWaiting {
		t.Fatalf("live agent's ask was not serviced — the loop was wedged by the blocking resolve: %+v", rec.snapshot())
	}

	close(blocker.release) // let any late resolve return cleanly before teardown
}
