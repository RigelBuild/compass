//go:build unix

package runner

// Per-container transition-lock tests for the production agentHost
// (docs/designs/infra/runtime/compass-runner-concurrent-dispatch/design.md, Approach
// (d) / Plan T4). Concurrent dispatch makes concurrent SessionHost callers
// reachable, so the lifecycle ops on ONE container must serialize while ops on
// DIFFERENT containers still overlap — the per-container granularity that keeps
// a slow Provision/Start of one container off the latency path of every other,
// and closes the Start TOCTOU (host.go — two concurrent Starts for one container
// both passing the existing-session check).
//
// T1 harness choice (per the record's T1 note — implementer's choice): these
// drive the REAL agentHost (NewSessionHost via newConfigRefreshFixture), not a
// fake, because the transition lock lives in agentHost and a fake would assert
// nothing about it. The launch is parked on the engine's execGate/execEntered
// channels so a test can hold one Start in flight while it drives another,
// deterministically — no sleeps, no polling. Where a test must prove an op was
// SUPPRESSED (produces no event by construction), it asserts on the authoritative
// post-release OUTCOME (which fails fast the instant both goroutines complete,
// never via the failsafe ceiling); the testTimeout ceiling only converts a
// pathological hang into a descriptive t.Fatal, mirroring
// TestCloseJoinsConcurrentTeardowns (host_test.go).

import (
	"context"
	"errors"
	"sync"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// startResult is one concurrent Start's outcome, carried back over a channel so
// the test can collect both without racing the goroutines.
type startResult struct {
	sessionID string
	err       error
}

// Two concurrent Starts for ONE container serialize through the container's
// transition lock: exactly one wins (a live session), the other is rejected with
// errAlreadyRunning, and the agent is launched exactly once. This is the
// documented Start TOCTOU (host.go: the existing-session check releases h.mu
// before the slow launch and re-acquires to record, so two Starts can both pass
// the check in that window). The per-container lock closes it.
//
// Deterministic construction: the first Start is parked mid-launch (execEntered
// fires, execGate holds). The second Start is then launched. The gate is
// released and BOTH Starts are joined, then the authoritative outcome is
// asserted — this fails FAST (the instant both goroutines complete), so the RED
// signal is the outcome assertion, never the failsafe ceiling.
//
//   - RED (no container lock): the second Start also passes the empty-session
//     check (the first has not recorded yet — it is parked past the check), so it
//     ALSO enters the launch (execEntered fires twice) and records a SECOND
//     session against the one container. Two sessions / two successes → the TOCTOU
//     is open.
//   - GREEN (per-container lock): the second Start blocks on the container lock
//     the first holds, never reaching the launch (execEntered fires once); once
//     the first records and releases, the second acquires the lock, re-checks, and
//     returns errAlreadyRunning.
func TestStartSameContainerSerializesClosingTOCTOU(t *testing.T) {
	host, engine, _ := newConfigRefreshFixture(t)
	ctx := context.Background()
	// Reap both children (RED leaves two live sessions on one container) on exit.
	t.Cleanup(func() { host.Close(context.Background()) })

	name, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "a"})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}

	// Park every agent launch until released, signalling entry so the test gates
	// on the real "reached the launch" event (execEntered is buffered so a parked
	// launch never blocks the send). Released on every exit path so a failing
	// assertion cannot hang the suite.
	gate := make(chan struct{})
	entered := make(chan runtime.ContainerID, 2)
	engine.mu.Lock()
	engine.execGate = gate
	engine.execEntered = entered
	engine.mu.Unlock()
	release := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)

	// First Start enters the launch and parks there, holding the container lock
	// (once T4 lands).
	first := make(chan startResult, 1)
	go func() {
		id, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "")
		first <- startResult{id, err}
	}()
	select {
	case <-entered:
	case <-timeAfter():
		t.Fatal("the first Start never reached the agent launch")
	}

	// Second Start against the SAME container, launched while the first is parked.
	second := make(chan startResult, 1)
	go func() {
		id, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "")
		second <- startResult{id, err}
	}()

	// Release both and join. RED fails fast on the outcome the instant both
	// complete; the ceiling only guards a pathological hang.
	release()
	var got [2]startResult
	for i, ch := range [2]chan startResult{first, second} {
		select {
		case got[i] = <-ch:
		case <-timeAfter():
			t.Fatal("a concurrent Start did not complete after the launch gate released")
		}
	}

	// Exactly one Start won and one was rejected with errAlreadyRunning — the
	// per-container lock serialized them. RED: both succeeded (two sessions minted
	// against one container — the open TOCTOU).
	var wins, rejects int
	for _, r := range got {
		switch {
		case r.err == nil && r.sessionID != "":
			wins++
		case errors.Is(r.err, errAlreadyRunning):
			rejects++
		default:
			t.Fatalf("unexpected Start outcome: sessionID=%q err=%v", r.sessionID, r.err)
		}
	}
	if wins != 1 || rejects != 1 {
		t.Fatalf("concurrent Starts on one container: %d won, %d rejected with errAlreadyRunning; want 1 and 1 (the per-container lock must let exactly one win and reject the other — the Start TOCTOU is open)", wins, rejects)
	}
	// The agent was launched exactly once — the loser never reached the launch.
	if got := engine.launchCount(name); got != 1 {
		t.Fatalf("agent launched %d times for one container, want 1 (the serialized loser must be rejected before the launch)", got)
	}
	// Exactly one live session survives on the container.
	statuses, err := host.Status(ctx, "")
	if err != nil {
		t.Fatalf("Status(all) = %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("live sessions on one container = %d, want 1 (two would be the TOCTOU minting a duplicate)", len(statuses))
	}
}

// Two Starts on DIFFERENT containers overlap fully — the lock is per-container,
// not one global lifecycle mutex. Both reach the agent launch concurrently, so
// the test gates on BOTH entry events (pure positive gating: different containers
// never contend, so both events fire in every correct world). A global lock
// would let only one enter while the other blocked — the second entry event
// would never fire and this reddens at the ceiling.
func TestStartDifferentContainersOverlap(t *testing.T) {
	host, engine, _ := newConfigRefreshFixture(t)
	ctx := context.Background()
	t.Cleanup(func() { host.Close(context.Background()) })

	nameA, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "a"})
	if err != nil {
		t.Fatalf("Provision(a) = %v", err)
	}
	nameB, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "b"})
	if err != nil {
		t.Fatalf("Provision(b) = %v", err)
	}

	gate := make(chan struct{})
	entered := make(chan runtime.ContainerID, 2)
	engine.mu.Lock()
	engine.execGate = gate
	engine.execEntered = entered
	engine.mu.Unlock()
	release := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)

	done := make(chan startResult, 2)
	for _, name := range []string{nameA, nameB} {
		go func() {
			id, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "")
			done <- startResult{id, err}
		}()
	}

	// Both launches must be reached concurrently: a per-container lock lets them
	// overlap, so both entry events fire while both are parked on the shared gate.
	// A GLOBAL lock would admit only one; the second event never fires and this
	// times out on the ceiling.
	seen := map[runtime.ContainerID]bool{}
	for range 2 {
		select {
		case id := <-entered:
			seen[id] = true
		case <-timeAfter():
			t.Fatalf("only %d of 2 different-container Starts reached the launch concurrently; a per-container lock must let different containers overlap (a global lock would serialize them)", len(seen))
		}
	}
	if len(seen) != 2 {
		t.Fatalf("the two concurrent launches were the same container id %v; want two distinct containers overlapping", seen)
	}

	release()
	for range 2 {
		select {
		case r := <-done:
			if r.err != nil {
				t.Fatalf("Start on a distinct container returned %v, want success", r.err)
			}
		case <-timeAfter():
			t.Fatal("a different-container Start did not complete after the gate released")
		}
	}
}

// A ConfigVersion-driven RefreshConfig pass does not self-deadlock, and a
// subsequent dispatch-driven Stop of that container still completes. The config
// worker's fan-out leg calls into the container's Reload while (once T4 lands)
// holding that container's transition lock; if Reload re-took the SAME
// non-reentrant lock, the worker would wedge forever and every later Stop/Reload/
// Remove of the container would wedge behind it. This pins the reloadLocked split
// (Approach (d)): the fan-out leg takes the lock and calls reloadLocked, the
// dispatch path calls the public Reload wrapper that takes the lock — never both.
//
// A deadlock produces NO event by construction, so the only possible RED signal
// is a hang the failsafe ceiling converts to a descriptive t.Fatal. The wedge is
// guaranteed (not probabilistic) in the single-non-reentrant-lock world, so this
// is deterministic. GREEN: RefreshConfig returns and the follow-up Stop returns.
func TestRefreshConfigReloadDoesNotSelfDeadlock(t *testing.T) {
	host, engine, pub := newConfigRefreshFixture(t)
	ctx := context.Background()

	pub.setConfigBundle(configBundleAt(t, "v-1"))
	name := provisionAndStart(t, host, "a")
	sessionID, ok := host.Session(name) // resolve the live session id for the later Stop
	if !ok {
		t.Fatalf("no live session bound to container %q after Start", name)
	}

	// Move the fleet version and give the container a live label, so the pass
	// actually Reloads (not the version-unchanged no-op).
	pub.setConfigBundle(configBundleAt(t, "v-2"))
	engine.labels[name] = "system_u:object_r:container_file_t:s0:c10,c20"
	stubRelabelAnyRoot(t)

	// The pass must complete — a self-deadlock wedges the config worker's leg
	// inside Reload forever.
	refreshed := make(chan error, 1)
	go func() { refreshed <- host.RefreshConfig(ctx) }()
	select {
	case err := <-refreshed:
		if err != nil {
			t.Fatalf("RefreshConfig = %v, want nil", err)
		}
	case <-timeAfter():
		t.Fatal("RefreshConfig self-deadlocked: the fan-out leg and Reload took the same non-reentrant container lock (the reloadLocked split is missing)")
	}

	// A subsequent dispatch-driven Stop of the same container completes — it did
	// not wedge behind a lock the deadlocked worker never released.
	stopped := make(chan error, 1)
	go func() { stopped <- host.Stop(ctx, sessionID) }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop after RefreshConfig = %v, want nil", err)
		}
	case <-timeAfter():
		t.Fatal("Stop wedged after RefreshConfig: a dispatch-driven Stop queued behind a container lock the self-deadlocked config worker never released")
	}
}
