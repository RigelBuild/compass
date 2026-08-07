//go:build podman

package e2e

import (
	"context"
	"os/exec"
	"testing"

	"github.com/sealedsecurity/compass/go/internal/runner"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// TestLegSixTeardownIdempotence is the full leg-6 scenario over the real stack
// (design.md:690-706, Approach A6 :383-402): a container is provisioned, the
// stack is torn Down (the container LEAKS — Down closes only the runner's
// sockets, host.go:203-210, so the podman container survives), then the stack is
// restarted against the SAME persisted postgres so the same handle resolves to
// the SAME account (hence the SAME deterministic container name), and a preflight
// exact-name `podman rm -f` lets the second Provision succeed where without it it
// would collide on `podman create --name`.
//
// Unlike legs 2-5, this leg goes GREEN on the bare stack TODAY: its red→green is
// at the Provision/`podman create` boundary (the A6 preflight), independent of
// the unmerged native-agent lane — no agent turn is driven, so no canned backend
// is needed. podmanUsable() SKIPs it in a container-less sandbox. Every wait is a
// bounded RPC (rpcTimeout, threaded from ctx) — no sleeps, no polling, no
// retries.
func TestLegSixTeardownIdempotence(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman cannot run compass-agent:latest here; skipping the real-stack e2e")
	}

	ctx := context.Background() // test root, threaded into NewFixture + every primitive

	// The persistent substrate: one root/stateDir/ports shared by both Ups, so
	// run2's Up re-attaches the postgres cluster run1 initialized (the DB data
	// dir lives under stateDir). Its single end-of-test RemoveAll is owned here,
	// NOT per-Up, so run1's Down cannot delete the DB before run2.
	site := newPersistentSite(t)

	// A FIXED handle across both runs. CreateAgent is create-only (a duplicate
	// handle is an already_exists conflict, not find-or-create), so run1 MINTS
	// the account under this handle and run2 RESOLVES that same persisted row by
	// the handle over the re-attached DB (see run2 below) — same account id ⇒
	// same deterministic container name across the restart.
	const handle = "leg6-idempotence"
	const displayName = "Leg Six Idempotence"

	// ── RUN 1: fresh Up, provision the container, then Down (it leaks) ──

	f1 := NewFixture(ctx, t, WithSite(site))

	accountID, err := f1.CreateAgent(ctx, handle, displayName)
	if err != nil {
		t.Fatalf("CreateAgent (run1): %v", err)
	}
	if accountID == "" {
		t.Fatal("CreateAgent (run1) returned an empty account id")
	}

	// The container name is deterministic: NamePrefix (compass-agent-, run.go:48)
	// + the account id (spec.go:85). Known only after CreateAgent — so register
	// the exact-name reap right here, BEFORE run1 provisions, so a mid-test
	// t.Fatal still sweeps the leaked container. EXACT-NAME only (rule://
	// process-safety); best-effort — teardown, not an assertion.
	containerName := runner.AgentContainerNamePrefix + accountID
	t.Cleanup(func() {
		_ = podmanRemoveForce(ctx, containerName) // best-effort end-of-test reap of the leaked container; a failure here is not actionable during cleanup
	})

	container1, err := f1.Provision(ctx, accountID, "leg6-run1-provision")
	if err != nil {
		t.Fatalf("Provision (run1): %v", err)
	}
	if container1 != containerName {
		t.Fatalf("Provision (run1) returned container %q, want the deterministic name %q (NamePrefix + accountID); the name derivation drifted from spec.go:85", container1, containerName)
	}
	// The container is actually PRESENT in podman's runtime set by that exact
	// name — the observable proof it came up, probed with a dependency-free
	// `podman container exists` (the same check legthreefour_test.go:170 and the
	// runtime suite use, the same direct podman-shelling podmanUsable relies on).
	if exec.Command("podman", "container", "exists", containerName).Run() != nil {
		t.Fatalf("container %q is not present after run1 Provision — the first lifetime never created it, so the leak/collision premise cannot be exercised", containerName)
	}

	// Tear run1's stack Down HERE, between the two runs (the fixture's own
	// t.Cleanup Downs it too, but that fires at test end — too late to free the
	// ports for run2's Up). Down is safe to call twice; the registered cleanup
	// no-ops idempotently.
	if err := f1.Stack().Down(ctx); err != nil {
		t.Fatalf("Stack().Down (run1): %v", err)
	}

	// THE LEAK (the RED's precondition, mechanism-link #2): the container SURVIVES
	// Down — Down closes only the runner's agent sockets (host.go:203-210) and
	// drains the stack children (stack.go:141-151), orphaning the podman
	// container. If a future change made Down reap containers, this reddens and
	// the whole leg is moot (there would be nothing to collide with in run2), so
	// this assertion is load-bearing, not decoration.
	if exec.Command("podman", "container", "exists", containerName).Run() != nil {
		t.Fatalf("container %q was reaped by run1's Stack().Down — the leak the A6 preflight guards against no longer happens, so the idempotence scenario is moot (mechanism-link #2, host.go:203-210)", containerName)
	}

	// ── RUN 2: restart over the SAME site, preflight-sweep, re-provision ──

	// SAME site ⇒ the postgres cluster re-attaches (serve runs initdb only when
	// PG_VERSION is absent, compass-postgres/main.go:165-168), so the persisted
	// account survives.
	f2 := NewFixture(ctx, t, WithSite(site))

	// RESOLVE the persisted account by handle rather than re-creating it:
	// CreateAgent is create-only (a duplicate handle is an already_exists
	// conflict, NOT find-or-create — verified firsthand: run2 CreateAgent returns
	// `already_exists: handle "leg6-idempotence" already taken`), so a second
	// CreateAgent would fail on the persisted row. Reading the row back by handle
	// over the re-attached DB is the stronger persistence proof anyway: the
	// account the FIRST run minted is still there after the restart.
	st, err := store.Open(ctx, f2.DSN())
	if err != nil {
		t.Fatalf("store.Open (run2): %v", err)
	}
	defer st.Close()
	persisted, err := st.AgentByHandle(ctx, handle)
	if err != nil {
		t.Fatalf("AgentByHandle(%q) (run2): %v — the postgres cluster did not re-attach across the restart, so the account run1 minted did not survive and the deterministic-name collision premise is gone", handle, err)
	}
	accountID2 := string(persisted.ID)
	// The DB PERSISTED across the restart: same handle → SAME account id. This is
	// load-bearing — a non-persisted DB would hold no such row (AgentByHandle
	// above would be ErrNotFound), and even a fresh account would carry a
	// different random id, so the container name would differ and there would be
	// no collision to fix (the whole scenario's premise would vanish silently).
	if accountID2 != accountID {
		t.Fatalf("persisted account id %q != the run1 account id %q — the re-attached DB resolved a different account for the same handle, so the deterministic-name collision premise is gone", accountID2, accountID)
	}

	// THE A6 FIX UNDER TEST: preflight force-remove the leaked run1 container by
	// its EXACT name before re-provisioning.
	//
	// WITHOUT this preflight, the second Provision below FAILS: createAndStart
	// issues `podman create --name <containerName>` with no happy-path pre-create
	// sweep (go/internal/runtime/agent.go:245-267; the only Removes there are
	// error-recovery of that same call's partial container), so it collides with
	// the leaked run1 container that survived Down above. The in-memory
	// client_request_id dedup is also gone (run2 is a fresh runner process), so
	// run2 genuinely re-issues the create. This preflight is the A6 fix; the
	// green run2 Provision below is its proof.
	if err := podmanRemoveForce(ctx, containerName); err != nil {
		t.Fatalf("podmanRemoveForce preflight (run2): %v — the A6 exact-name sweep of the leaked container failed", err)
	}

	container2, err := f2.Provision(ctx, accountID2, "leg6-run2-provision")
	if err != nil {
		t.Fatalf("Provision (run2): %v — the second Provision failed despite the A6 preflight sweep of the leaked run1 container", err)
	}
	if container2 != containerName {
		t.Fatalf("Provision (run2) returned container %q, want the same deterministic name %q as run1", container2, containerName)
	}
	// The re-provisioned container is PRESENT by that exact name — the GREEN: with
	// the preflight, the second Provision succeeds despite the run1 leak.
	if exec.Command("podman", "container", "exists", containerName).Run() != nil {
		t.Fatalf("container %q is not present after run2 Provision — the idempotent re-provision did not bring the container back up", containerName)
	}
}
