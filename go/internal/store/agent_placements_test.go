//go:build pgtest

package store

// Agent placement: the durable record of which Runner an agent is on and the
// container name it runs under. Two things must hold for the SEA-1516 reattach
// work that reads it — a placement is SINGULAR per agent (a re-provision
// replaces it, never accumulates), and the container -> agent mapping it
// exposes is exclusive. Both are database invariants (the PK and the unique
// index), so both are pgtest-backed; a mock would only re-assert the Go code.

import (
	"testing"
)

// TestRecordAgentPlacementRoundTrips pins the base contract both reads depend
// on: what Provision wrote is what Start and reattach read back — the account
// resolvable from the container name, and the placement listed under its Runner
// carrying the container name re-driving Provision needs.
func TestRecordAgentPlacementRoundTrips(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	if err := s.RecordAgentPlacement(ctx, agent.ID, "runner-1", "compass-agent-"+string(agent.ID)); err != nil {
		t.Fatalf("RecordAgentPlacement: %v", err)
	}

	got, err := s.AgentForContainer(ctx, "compass-agent-"+string(agent.ID))
	if err != nil {
		t.Fatalf("AgentForContainer: %v", err)
	}
	if got != agent.ID {
		t.Fatalf("AgentForContainer = %q, want the placed agent %q", got, agent.ID)
	}

	placements, err := s.ListAgentPlacementsForRunner(ctx, "runner-1")
	if err != nil {
		t.Fatalf("ListAgentPlacementsForRunner: %v", err)
	}
	if len(placements) != 1 {
		t.Fatalf("runner-1 placements = %d, want 1", len(placements))
	}
	want := AgentPlacement{AgentAccountID: agent.ID, RunnerID: "runner-1", ContainerName: "compass-agent-" + string(agent.ID)}
	if placements[0] != want {
		t.Fatalf("placement = %+v, want %+v", placements[0], want)
	}
}

// TestRecordAgentPlacementReplacesOnRePlacement is the invariant reattach hangs
// on: an agent is on AT MOST ONE Runner. Re-provisioning it onto a different
// Runner must REPLACE its row — updating runner_id and container_name TOGETHER —
// not add a second. If placements accumulated, reattach on the OLD Runner would
// re-drive Provision for an agent that has already moved; if the two columns
// updated independently, a row would pair the new Runner with the old container
// name and reattach would try to recover a container that is not there.
func TestRecordAgentPlacementReplacesOnRePlacement(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	if err := s.RecordAgentPlacement(ctx, agent.ID, "runner-1", "cont-old"); err != nil {
		t.Fatalf("first RecordAgentPlacement: %v", err)
	}
	if err := s.RecordAgentPlacement(ctx, agent.ID, "runner-2", "cont-new"); err != nil {
		t.Fatalf("re-placement onto runner-2: %v", err)
	}

	// The old Runner must no longer claim it — otherwise a reattach pass on
	// runner-1 re-drives Provision for an agent that has moved away.
	old, err := s.ListAgentPlacementsForRunner(ctx, "runner-1")
	if err != nil {
		t.Fatalf("ListAgentPlacementsForRunner(runner-1): %v", err)
	}
	if len(old) != 0 {
		t.Fatalf("runner-1 still holds %+v after the agent moved, want no placements", old)
	}

	current, err := s.ListAgentPlacementsForRunner(ctx, "runner-2")
	if err != nil {
		t.Fatalf("ListAgentPlacementsForRunner(runner-2): %v", err)
	}
	want := AgentPlacement{AgentAccountID: agent.ID, RunnerID: "runner-2", ContainerName: "cont-new"}
	if len(current) != 1 || current[0] != want {
		t.Fatalf("runner-2 placements = %+v, want exactly [%+v]", current, want)
	}

	// The superseded container name resolves to nothing: a Start arriving late
	// for the old container cannot record a session against a placement that no
	// longer exists.
	if _, err := s.AgentForContainer(ctx, "cont-old"); err == nil {
		t.Fatal("AgentForContainer(cont-old) = nil error, want ErrNotFound (the old placement was replaced)")
	} else {
		sentinelIs(t, err, ErrNotFound, "superseded container lookup")
	}
}

// TestRecordAgentPlacementRejectsAContainerClaimedByAnotherAgent pins the
// exclusivity of the container -> agent mapping. StartAgentSession resolves the
// session's OWNER through this mapping, and that owner picks the home channel
// the authz JOIN authorizes against — so a second agent claiming a live
// container name would let a session be recorded under the wrong owner and
// hand a foreign channel's members the right to watch it. The unique index
// refuses it as ErrConflict rather than letting the write land.
func TestRecordAgentPlacementRejectsAContainerClaimedByAnotherAgent(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	first := mustAgent(t, s, owner.ID, "first")
	second := mustAgent(t, s, owner.ID, "second")

	if err := s.RecordAgentPlacement(ctx, first.ID, "runner-1", "cont-shared"); err != nil {
		t.Fatalf("first placement: %v", err)
	}

	err := s.RecordAgentPlacement(ctx, second.ID, "runner-1", "cont-shared")
	sentinelIs(t, err, ErrConflict, "second agent claiming a live container name")

	// The first agent keeps the container: the refused write changed nothing.
	got, err := s.AgentForContainer(ctx, "cont-shared")
	if err != nil {
		t.Fatalf("AgentForContainer after the refused claim: %v", err)
	}
	if got != first.ID {
		t.Fatalf("cont-shared resolves to %q, want the original owner %q", got, first.ID)
	}
}

// TestRecordAgentPlacementUnknownAgentIsInvalidArgument pins the FK: a placement
// for an account that is not an agent cannot land. Without it a row could name a
// nonexistent agent and reattach would re-drive Provision for nobody.
func TestRecordAgentPlacementUnknownAgentIsInvalidArgument(t *testing.T) {
	s := newTestStore(t)

	err := s.RecordAgentPlacement(t.Context(), "no-such-agent", "runner-1", "cont-1")
	sentinelIs(t, err, ErrInvalidArgument, "placement for an unknown agent")
}

// TestAgentForContainerUnplacedIsNotFound pins Start's fail-closed path: a
// container this Server never placed resolves no owner, so no session row can be
// recorded against it. Distinct from a conflict — nothing is wrong with the
// request, the container is simply not ours.
func TestAgentForContainerUnplacedIsNotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.AgentForContainer(t.Context(), "never-provisioned")
	sentinelIs(t, err, ErrNotFound, "unplaced container lookup")
}

// TestListAgentPlacementsForRunnerScopesToItsRunner is the reattach read itself:
// a recovery pass must get EXACTLY the agents on the restarted Runner. Too few
// strands an agent; too many re-drives Provision for agents on a healthy Runner,
// disrupting live work. Ordering is asserted because a recovery pass should be
// deterministic and its logs diffable across runs.
func TestListAgentPlacementsForRunnerScopesToItsRunner(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "agent-a")
	b := mustAgent(t, s, owner.ID, "agent-b")
	elsewhere := mustAgent(t, s, owner.ID, "agent-elsewhere")

	for _, p := range []AgentPlacement{
		{a.ID, "runner-1", "cont-a"},
		{b.ID, "runner-1", "cont-b"},
		{elsewhere.ID, "runner-2", "cont-elsewhere"},
	} {
		if err := s.RecordAgentPlacement(ctx, p.AgentAccountID, p.RunnerID, p.ContainerName); err != nil {
			t.Fatalf("RecordAgentPlacement(%+v): %v", p, err)
		}
	}

	got, err := s.ListAgentPlacementsForRunner(ctx, "runner-1")
	if err != nil {
		t.Fatalf("ListAgentPlacementsForRunner: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("runner-1 placements = %+v, want exactly the 2 agents on it", got)
	}
	for _, p := range got {
		if p.AgentAccountID == elsewhere.ID {
			t.Fatalf("runner-1 listing included %q, which is placed on runner-2", elsewhere.ID)
		}
		if p.ContainerName == "" {
			t.Fatalf("placement %+v carries no container name; reattach cannot re-drive Provision without it", p)
		}
	}
	if got[0].AgentAccountID > got[1].AgentAccountID {
		t.Fatalf("placements = %+v, want ascending agent order (a recovery pass must be deterministic)", got)
	}
}

// TestListAgentPlacementsForRunnerEmptyForUnknownRunner pins that "nothing to
// reattach" is a normal outcome, not an error: a Runner enrolling for the first
// time holds no placements, and a recovery pass over it must proceed quietly
// rather than treat the empty set as a failure.
func TestListAgentPlacementsForRunnerEmptyForUnknownRunner(t *testing.T) {
	s := newTestStore(t)

	got, err := s.ListAgentPlacementsForRunner(t.Context(), "runner-never-seen")
	if err != nil {
		t.Fatalf("ListAgentPlacementsForRunner(unknown) = %v, want nil (no placements is not a failure)", err)
	}
	if len(got) != 0 {
		t.Fatalf("unknown runner placements = %+v, want empty", got)
	}
}

// TestDeleteAgentPlacementReleasesContainerName is the load-bearing property of
// the despawn release path: deleting a placement frees its unique container_name
// so a future spawn can reuse it. If DeleteAgentPlacement failed to remove the
// row (or left the unique name claimed), a re-spawn onto that container would hit
// ErrConflict and the name would be owned forever with no way back — exactly the
// leak RecordAgentPlacement's godoc warned a missing release path would cause.
func TestDeleteAgentPlacementReleasesContainerName(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "agent-a")
	b := mustAgent(t, s, owner.ID, "agent-b")

	if err := s.RecordAgentPlacement(ctx, a.ID, "runner-1", "cont-x"); err != nil {
		t.Fatalf("place agent A: %v", err)
	}
	if err := s.DeleteAgentPlacement(ctx, "cont-x"); err != nil {
		t.Fatalf("DeleteAgentPlacement: %v", err)
	}

	// The container resolves to no owner: the placement is gone.
	if _, err := s.AgentForContainer(ctx, "cont-x"); err == nil {
		t.Fatal("AgentForContainer(cont-x) = nil error after delete, want ErrNotFound")
	} else {
		sentinelIs(t, err, ErrNotFound, "released container lookup")
	}

	// The freed name is reusable: a DIFFERENT agent may now claim it without
	// hitting the unique-index conflict.
	if err := s.RecordAgentPlacement(ctx, b.ID, "runner-1", "cont-x"); err != nil {
		t.Fatalf("re-placing the freed container name: %v (want nil — the name was released)", err)
	}
}

// TestDeleteAgentPlacementAbsentRowSucceeds pins idempotency: despawn may be
// retried, so deleting a container that was never placed (or already removed)
// must succeed. If it errored on zero rows, a retried despawn would fail on its
// second pass and strand the teardown mid-way.
func TestDeleteAgentPlacementAbsentRowSucceeds(t *testing.T) {
	s := newTestStore(t)

	if err := s.DeleteAgentPlacement(t.Context(), "never-placed"); err != nil {
		t.Fatalf("DeleteAgentPlacement(never-placed) = %v, want nil (idempotent)", err)
	}
}

// TestPlacementForAgentRoundTrips pins the reverse read despawn depends on:
// given the agent id its authority check resolved, it returns the runner and
// container name the teardown must act on. A bug swapping the two columns, or
// reading the wrong row, would tear down the wrong container.
func TestPlacementForAgentRoundTrips(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	if err := s.RecordAgentPlacement(ctx, agent.ID, "runner-1", "cont-a"); err != nil {
		t.Fatalf("RecordAgentPlacement: %v", err)
	}

	runnerID, containerName, err := s.PlacementForAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("PlacementForAgent: %v", err)
	}
	if runnerID != "runner-1" || containerName != "cont-a" {
		t.Fatalf("PlacementForAgent = (%q, %q), want (runner-1, cont-a)", runnerID, containerName)
	}
}

// TestPlacementForAgentUnknownIsNotFound pins the fail-closed path: both an
// unknown agent and a known-but-unplaced agent resolve to ErrNotFound. Despawn
// treats the two alike — there is nothing to tear down either way — so the same
// sentinel must cover both rather than leaking a distinction the caller ignores.
func TestPlacementForAgentUnknownIsNotFound(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	_, _, err := s.PlacementForAgent(ctx, AccountID("ghost"))
	sentinelIs(t, err, ErrNotFound, "unknown agent placement lookup")

	owner := mustUser(t, s, "owner")
	unplaced := mustAgent(t, s, owner.ID, "unplaced")
	_, _, err = s.PlacementForAgent(ctx, unplaced.ID)
	sentinelIs(t, err, ErrNotFound, "known-but-unplaced agent placement lookup")
}
