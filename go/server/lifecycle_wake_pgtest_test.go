//go:build pgtest && unix

package server

// RIG-1641 T3: lifecycleService.WakeAgent, the resume-based offline-agent wake,
// against a real Postgres AND a real Runner door (the fake recordingRunner that
// records every relayed command, so "a Start was pushed" / "no Start was pushed"
// / "the resume body rode the internal envelope" are observed wire facts, not
// mock expectations). WakeAgent is an INTERNAL seam (delivery.AgentWaker), not a
// wire RPC, so these drive newLifecycleService(store, hub).WakeAgent directly
// under a resolved agent AccountID — the same way the delivery consumer's wake
// seam calls it — rather than through the connect client.
//
// The chain each case pins: not-live pre-check (a live agent is a no-op), a
// prior-session system-authorized internal resume (BindLifetime +
// ReconstructSessionBody + StartResume, SKIPPING the caller-subscriber gate), a
// never-started fresh hub.Start fallback, a no-placement logged no-op, and the
// per-agent singleflight coalescing N concurrent wakes onto exactly one start.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

// newWakeFixture builds the placement fixture and a lifecycleService over its
// real store + hub — the seam WakeAgent is a method on. The fixture agent
// (f.agentID) is the wake target; the attach probe is already forgotten by
// newPlacementFixture's caller path, but the wake tests seed their own state, so
// each drops the probe explicitly before asserting.
func newWakeFixture(t *testing.T) (placementFixture, *lifecycleService) {
	t.Helper()
	pf := newPlacementFixture(t)
	return pf, newLifecycleService(pf.store, pf.hub, nil)
}

// TestWakeAgentLiveIsNoOp pins the not-live pre-check: an agent with a LIVE
// session is already awake, so WakeAgent pushes no Start and does no resume.
//
// Mutation: dropping the SessionForAccount not-live guard would run the resume
// chain against a live agent — a redundant start — reddening the "no Start"
// assertion.
func TestWakeAgentLiveIsNoOp(t *testing.T) {
	ctx := context.Background() // test root
	f, lc := newWakeFixture(t)

	// Make the agent LIVE through the real Provision->Start promotion path the
	// hub drives: bindContainer then promoteSession bind (agent -> live session).
	if _, _, err := f.hub.Provision(ctx, "prov-live", &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: string(f.agentID)}); err != nil {
		t.Fatalf("Provision = %v, want success", err)
	}
	if _, err := f.hub.Start(ctx, "start-live", &compassv1.StartAgentSessionRequest{ContainerName: fakeContainer}); err != nil {
		t.Fatalf("Start = %v, want success", err)
	}
	if _, live := f.hub.SessionForAccount(f.agentID); !live {
		t.Fatal("precondition: agent should be live after Provision+Start")
	}
	f.runner.forget() // drop the setup commands; assert only on the wake

	lc.WakeAgent(ctx, f.agentID)

	if n := f.runner.startCount(); n != 0 {
		t.Fatalf("WakeAgent(live agent) pushed %d Start(s), want 0 (a live agent is nothing to wake); commands: %v", n, f.runner.commands())
	}
}

// TestWakeAgentPriorSessionResumes pins the system-authorized internal resume: an
// OFFLINE agent with a recorded prior session (and a placement + a stored
// transcript) is woken by RESUMING that session — BindLifetime +
// ReconstructSessionBody + StartResume — NOT a fresh Start. Proven on the wire:
// the Start command carries the reconstructed resume_body (the internal envelope
// only StartResume attaches), and no second session row is recorded (resume
// reuses the logical id).
//
// Mutation: routing a prior-session agent through the fresh hub.Start path (no
// StartResume) leaves resume_body empty, reddening the body assertion; skipping
// BindLifetime leaves base_entry_seq unbound.
func TestWakeAgentPriorSessionResumes(t *testing.T) {
	ctx := context.Background() // test root
	f, lc := newWakeFixture(t)

	const logical = "sess-wake-resume"
	if err := f.store.RecordAgentSession(ctx, logical, f.agentID); err != nil {
		t.Fatalf("RecordAgentSession: %v", err)
	}
	if err := f.store.RecordAgentPlacement(ctx, f.agentID, fakeRunnerID, fakeContainer); err != nil {
		t.Fatalf("RecordAgentPlacement: %v", err)
	}
	// A checkpoint + one delta: the PG hot-tail normal resume set.
	if err := f.store.AppendTranscriptEntry(ctx, logical, 1, true, `{"header":true}`, "k1"); err != nil {
		t.Fatalf("append checkpoint: %v", err)
	}
	if err := f.store.AppendTranscriptEntry(ctx, logical, 2, false, `{"d":2}`, "k2"); err != nil {
		t.Fatalf("append delta: %v", err)
	}
	f.runner.forget()

	lc.WakeAgent(ctx, f.agentID)

	body, sawStart := relayedStartResumeBody(t, f.runner)
	if !sawStart {
		t.Fatalf("WakeAgent(prior session) pushed no Start (commands: %v)", f.runner.commands())
	}
	if n := f.runner.startCount(); n != 1 {
		t.Fatalf("WakeAgent(prior session) pushed %d Starts, want exactly 1 (one resume)", n)
	}
	want := "{\"header\":true}\n{\"d\":2}"
	if body != want {
		t.Fatalf("resume relayed resume_body = %q, want %q (a resume attaches the reconstructed body; a fresh Start attaches nothing)", body, want)
	}
	// A resume reuses the logical id: no NEW session row is recorded.
	if got := sessionRowCount(t, ctx, f.dsn, logical); got != 1 {
		t.Fatalf("session rows for %q = %d, want 1 (resume reuses the logical id, never records a second)", logical, got)
	}
	// BindLifetime snapshotted the rebase base as the stored max (2).
	if base := boundBase(t, ctx, f.dsn, logical); base != 2 {
		t.Fatalf("resume bound base = %d, want 2 (BindLifetime ran on the resume path)", base)
	}
}

// TestWakeAgentNeverStartedFreshStarts pins the no-prior-session fallback: an
// OFFLINE agent that has NEVER had a session recorded, but DOES have a placement,
// is woken by a FRESH hub.Start (no resume body) and its new session is recorded.
//
// Mutation: attempting a resume with no prior session (a StartResume /
// ReconstructSessionBody against a nonexistent session) would error and log
// outcome=failed, pushing no Start — reddening the "one fresh Start" assertion.
func TestWakeAgentNeverStartedFreshStarts(t *testing.T) {
	ctx := context.Background() // test root
	f, lc := newWakeFixture(t)

	// A placement but NO recorded session: the never-started-with-placement case.
	if err := f.store.RecordAgentPlacement(ctx, f.agentID, fakeRunnerID, fakeContainer); err != nil {
		t.Fatalf("RecordAgentPlacement: %v", err)
	}
	f.runner.forget()

	lc.WakeAgent(ctx, f.agentID)

	body, sawStart := relayedStartResumeBody(t, f.runner)
	if !sawStart {
		t.Fatalf("WakeAgent(never started) pushed no Start (commands: %v)", f.runner.commands())
	}
	if n := f.runner.startCount(); n != 1 {
		t.Fatalf("WakeAgent(never started) pushed %d Starts, want exactly 1 (one fresh start)", n)
	}
	if body != "" {
		t.Fatalf("fresh start relayed resume_body = %q, want empty (a fresh start attaches no resume body)", body)
	}
	// The fresh start recorded its new session (the minted fakeSessionID).
	if got := sessionRowCount(t, ctx, f.dsn, fakeSessionID); got != 1 {
		t.Fatalf("session rows for the fresh session %q = %d, want 1 (fresh start records ownership)", fakeSessionID, got)
	}
}

// TestWakeAgentNoPlacementIsNoOp pins the no-placement logged no-op: an OFFLINE
// agent with no recorded session AND no placement (never provisioned) cannot be
// woken — the owed row / cursor waits for a future natural start — so WakeAgent
// pushes no Start.
//
// Mutation: treating a placement miss as anything but a clean no-op (e.g.
// panicking on the ErrNotFound, or pushing a Start with an empty container) would
// redden this.
func TestWakeAgentNoPlacementIsNoOp(t *testing.T) {
	ctx := context.Background() // test root
	f, lc := newWakeFixture(t)
	f.runner.forget()

	lc.WakeAgent(ctx, f.agentID) // no session, no placement

	if n := f.runner.startCount(); n != 0 {
		t.Fatalf("WakeAgent(no placement) pushed %d Start(s), want 0 (nothing to wake); commands: %v", n, f.runner.commands())
	}
}

// TestWakeAgentPriorSessionNoPlacementIsBenignNoPlacement pins the placement-miss
// symmetry (RIG-1641 T3, §Decisions OQ-7): an OFFLINE agent with a RECORDED prior
// session but NO current placement — the despawned-then-mentioned state, since
// despawn deletes the placement while agent_sessions rows are never deleted — is
// a BENIGN no-placement no-op, exactly like the never-provisioned agent. It
// pushes no Start AND logs outcome=no-placement, NOT the ERROR outcome=failed a
// real fault gets: the owed row waits for the agent's next natural start.
//
// Mutation: the pre-fix code wrapped PlacementForAgent's ErrNotFound on the
// resume path into a returned error, so wakeOnce logged ERROR + outcome=failed
// here (while freshStart mapped the identical miss to no-placement). Reverting
// the errWakeNoPlacement sentinel reddens the outcome=no-placement assertion (the
// log would carry outcome=failed) — mis-severity that trips error-rate alerting
// on a routine path.
func TestWakeAgentPriorSessionNoPlacementIsBenignNoPlacement(t *testing.T) {
	ctx := context.Background() // test root
	f, lc := newWakeFixture(t)

	// A recorded prior session but NO placement: the despawned-agent state.
	const logical = "sess-wake-noplace"
	if err := f.store.RecordAgentSession(ctx, logical, f.agentID); err != nil {
		t.Fatalf("RecordAgentSession: %v", err)
	}
	f.runner.forget()

	// Capture the process-global slog default for the wake so the outcome is an
	// observed fact. Owns the global logger for its duration — no t.Parallel().
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	sb := &syncBuffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(sb, nil)))

	lc.WakeAgent(ctx, f.agentID)

	slog.SetDefault(prev) // stop capturing before asserting on the buffer
	if n := f.runner.startCount(); n != 0 {
		t.Fatalf("WakeAgent(prior session, no placement) pushed %d Start(s), want 0 (a despawned agent is nothing to wake); commands: %v", n, f.runner.commands())
	}
	logs := sb.String()
	if !strings.Contains(logs, "outcome=no-placement") {
		t.Fatalf("wake of a prior-sessioned agent with no placement logged %q, want outcome=no-placement (a despawned agent is a benign no-op, not a fault)", logs)
	}
	if strings.Contains(logs, "outcome=failed") {
		t.Fatalf("wake of a prior-sessioned agent with no placement logged outcome=failed (%q); a placement miss is benign, not an ERROR that trips alerting", logs)
	}
}

// TestWakeAgentSingleflightCoalescesToOneStart is the load-bearing cost-control
// test (§Decisions OQ-2): N concurrent WakeAgent calls for the SAME offline agent
// must produce EXACTLY ONE underlying start, not N. The fake Runner blocks the
// leader's Start in-flight on a gate while the other N-1 wakes pile onto the
// singleflight; releasing the gate lets the one start complete. Counting Starts
// on the wire is what makes "coalesced to one" an observed fact.
//
// Mutation: dropping the per-agent singleflight (wakeGroup.Do) makes each
// concurrent wake push its own Start — the start-storm this control prevents —
// so startCount would be N, reddening the assertion.
func TestWakeAgentSingleflightCoalescesToOneStart(t *testing.T) {
	ctx := context.Background() // test root
	f, lc := newWakeFixture(t)

	// A prior session + placement + transcript, so each wake takes the resume
	// path (one deterministic start shape to count).
	const logical = "sess-wake-sf"
	if err := f.store.RecordAgentSession(ctx, logical, f.agentID); err != nil {
		t.Fatalf("RecordAgentSession: %v", err)
	}
	if err := f.store.RecordAgentPlacement(ctx, f.agentID, fakeRunnerID, fakeContainer); err != nil {
		t.Fatalf("RecordAgentPlacement: %v", err)
	}
	if err := f.store.AppendTranscriptEntry(ctx, logical, 1, true, `{"header":true}`, "k1"); err != nil {
		t.Fatalf("append checkpoint: %v", err)
	}
	// Bind the container in the hub (Provision's container->account binding) so
	// the leader's StartResume->promoteSession actually promotes the agent LIVE.
	// That closes the only residual race: a follower that reaches wakeGroup.Do
	// just AFTER the leader releases finds the agent live at the not-live
	// pre-check and no-ops, so it never starts a second session either.
	if _, _, err := f.hub.Provision(ctx, "prov-sf", &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: string(f.agentID)}); err != nil {
		t.Fatalf("Provision (bind container): %v", err)
	}
	f.runner.forget()

	// Gate the leader's Start in-flight: its StartResume blocks in the fake
	// Runner until we close the gate, holding the singleflight key busy the whole
	// time so every caller that reaches wakeGroup.Do while it is held coalesces.
	gate := make(chan struct{})
	f.runner.setStartGate(gate)

	// The LEADER: launched alone and confirmed in-flight before any follower, so
	// the singleflight key is provably held busy when the followers arrive.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		lc.WakeAgent(ctx, f.agentID)
	}()
	waitForOneInflightStart(t, f.runner)

	// The FOLLOWERS: launched only now, while the leader still blocks inside Do.
	// Each signals ready right before calling WakeAgent; once all have signalled
	// and the key is still held (gate un-closed), every one of them joins the
	// in-flight leader rather than starting its own.
	const followers = 7
	ready := make(chan struct{}, followers)
	wg.Add(followers)
	for range followers {
		go func() {
			defer wg.Done()
			ready <- struct{}{}
			lc.WakeAgent(ctx, f.agentID)
		}()
	}
	for range followers {
		<-ready
	}

	// Every caller is now fanned in against the still-in-flight leader; release
	// the gate so the one start completes and all callers return.
	close(gate)
	wg.Wait()

	if got := f.runner.startCount(); got != 1 {
		t.Fatalf("%d concurrent WakeAgent for one agent pushed %d Starts, want exactly 1 (per-agent singleflight must coalesce them); commands: %v", followers+1, got, f.runner.commands())
	}
}

// waitForOneInflightStart spins until the fake Runner has recorded one Start (the
// gated leader), so the test launches followers only once the leader is provably
// in-flight and holding the singleflight key. Bounded by the suite timeout: a
// genuinely stuck wake fails fast rather than hanging.
func waitForOneInflightStart(t *testing.T, r *recordingRunner) {
	t.Helper()
	deadline := timeAfter()
	for {
		if r.startCount() >= 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("no Start reached the Runner; the leader wake never dispatched (commands: %v)", r.commands())
		default:
		}
	}
}
