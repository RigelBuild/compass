//go:build pgtest && unix

package server

// SpawnAsAccount role/persona thread-through (RIG-2673 T4): a Manager-creating
// spawn carries role+persona set-at-creation. The values are stored via
// store.CreateAgent (source of record) and threaded to the Runner's Provision
// wire from the CREATED store account — so a spawn with role/persona set lands
// them in agent_accounts AND on the Provision wire, and an empty-field spawn is
// byte-identical to today (empty on both). Idempotent re-spawn keeps the stored
// values. Driven through newLifecycleFixture directly, as the other spawn tests.
// context.Background() is the test root (test-root ctx exemption).

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// TestSpawnStoresAndThreadsRole: a spawn with role "manager" and no persona
// lands the role in the created agent_accounts row AND on the Provision wire
// (threaded from the store account, the source of record), and stores an empty
// persona verbatim — pinning the PR's deliberate asymmetry: role is a closed
// server-validated set, persona is free-text and may be empty.
func TestSpawnStoresAndThreadsRole(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()

	resp, err := f.lc.SpawnAsAccount(ctx, f.agentID, &compassv1internal.SpawnPeerRequest{
		Handle:          "peer-role",
		DisplayName:     "Peer Role",
		ClientRequestId: "spawn-role",
		Role:            "manager",
	})
	if err != nil {
		t.Fatalf("SpawnAsAccount = %v, want success", err)
	}
	newID := store.AccountID(resp.GetAgentAccountId())

	acc, err := f.store.GetAccount(ctx, newID)
	if err != nil {
		t.Fatalf("GetAccount(spawned) = %v", err)
	}
	if got := acc.Agent.Role; got != "manager" {
		t.Fatalf("stored role = %q, want manager (set-at-creation from the spawn request)", got)
	}
	if got := f.runner.provisionRole(t); got != "manager" {
		t.Fatalf("Provision wire role = %q, want manager (threaded from the store account)", got)
	}
	if got := acc.Agent.Persona; got != "" {
		t.Fatalf("stored persona = %q, want empty (a role-only spawn leaves persona blank; persona is free-text and optional)", got)
	}
}

// TestSpawnStoresAndThreadsPersona: a spawn with a persona lands it in the
// created agent_accounts row AND on the Provision wire.
func TestSpawnStoresAndThreadsPersona(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()

	const wantPersona = "You are a diligent manager."
	resp, err := f.lc.SpawnAsAccount(ctx, f.agentID, &compassv1internal.SpawnPeerRequest{
		Handle:          "peer-persona",
		DisplayName:     "Peer Persona",
		ClientRequestId: "spawn-persona",
		Role:            "manager",
		Persona:         wantPersona,
	})
	if err != nil {
		t.Fatalf("SpawnAsAccount = %v, want success", err)
	}
	newID := store.AccountID(resp.GetAgentAccountId())

	acc, err := f.store.GetAccount(ctx, newID)
	if err != nil {
		t.Fatalf("GetAccount(spawned) = %v", err)
	}
	if got := acc.Agent.Persona; got != wantPersona {
		t.Fatalf("stored persona = %q, want %q (set-at-creation from the spawn request)", got, wantPersona)
	}
	if got := f.runner.provisionPersona(t); got != wantPersona {
		t.Fatalf("Provision wire persona = %q, want %q (threaded from the store account)", got, wantPersona)
	}
}

// TestSpawnEmptyRoleIsRejected: a spawn naming no role (empty) is refused
// CodeInvalidArgument and writes NO account row — every spawned node carries a
// role from the closed taxonomy. This pins the ROLE requirement only; the
// contrasting invariant that an empty PERSONA is accepted (persona is
// free-text, not a closed set) is pinned by TestSpawnStoresAndThreadsRole,
// which spawns a valid role with no persona and asserts the stored persona is
// empty.
func TestSpawnEmptyRoleIsRejected(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()

	_, err := f.lc.SpawnAsAccount(ctx, f.agentID, &compassv1internal.SpawnPeerRequest{
		Handle:          "peer-empty-role",
		DisplayName:     "Peer Empty Role",
		ClientRequestId: "spawn-empty-role",
		Persona:         "compass-agent lane",
	})
	if err == nil {
		t.Fatal("SpawnAsAccount with an empty role = nil error, want CodeInvalidArgument")
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("empty-role spawn code = %v, want CodeInvalidArgument", got)
	}
	if !errors.Is(err, errUnknownRole) {
		t.Fatalf("empty-role spawn cause = %v, want errUnknownRole", err)
	}
	if _, err := f.store.AgentByHandle(ctx, f.ownerAdmin, "peer-empty-role"); err == nil {
		t.Fatal("an account was created for an empty-role spawn, want none (rejected before CreateAgent)")
	} else if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("AgentByHandle(peer-empty-role) = %v, want ErrNotFound (no row written)", err)
	}
}

// TestSpawnOffTaxonomyRoleIsRejected: a spawn naming a role outside the closed
// taxonomy is refused CodeInvalidArgument and writes NO account row.
func TestSpawnOffTaxonomyRoleIsRejected(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()

	_, err := f.lc.SpawnAsAccount(ctx, f.agentID, &compassv1internal.SpawnPeerRequest{
		Handle:          "peer-director",
		DisplayName:     "Peer Director",
		ClientRequestId: "spawn-director",
		Role:            "director",
		Persona:         "compass-agent lane",
	})
	if err == nil {
		t.Fatal("SpawnAsAccount with an off-taxonomy role = nil error, want CodeInvalidArgument")
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("off-taxonomy-role spawn code = %v, want CodeInvalidArgument", got)
	}
	if !errors.Is(err, errUnknownRole) {
		t.Fatalf("off-taxonomy-role spawn cause = %v, want errUnknownRole", err)
	}
	if _, err := f.store.AgentByHandle(ctx, f.ownerAdmin, "peer-director"); err == nil {
		t.Fatal("an account was created for an off-taxonomy-role spawn, want none (rejected before CreateAgent)")
	} else if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("AgentByHandle(peer-director) = %v, want ErrNotFound (no row written)", err)
	}
}

// TestSpawnAcceptsEachTaxonomyRole: all three roles are spawnable (supervisor
// stays spawnable — a spawned supervisor is parented and permitted), and each
// lands in the created row AND on the Provision wire threaded from the store.
func TestSpawnAcceptsEachTaxonomyRole(t *testing.T) {
	for _, role := range []string{"supervisor", "owner", "manager"} {
		t.Run(role, func(t *testing.T) {
			f := newLifecycleFixture(t)
			ctx := context.Background()

			resp, err := f.lc.SpawnAsAccount(ctx, f.agentID, &compassv1internal.SpawnPeerRequest{
				Handle:          "peer-" + role,
				DisplayName:     "Peer " + role,
				ClientRequestId: "spawn-" + role,
				Role:            role,
				Persona:         "compass-agent lane",
			})
			if err != nil {
				t.Fatalf("SpawnAsAccount(role=%q) = %v, want success", role, err)
			}
			acc, err := f.store.GetAccount(ctx, store.AccountID(resp.GetAgentAccountId()))
			if err != nil {
				t.Fatalf("GetAccount(spawned) = %v", err)
			}
			if acc.Agent.Role != role {
				t.Fatalf("stored role = %q, want %q", acc.Agent.Role, role)
			}
			if got := f.runner.provisionRole(t); got != role {
				t.Fatalf("Provision wire role = %q, want %q (threaded from the store account)", got, role)
			}
		})
	}
}

// TestSpawnIdempotentReSpawnKeepsStoredRolePersona: a second spawn for the SAME
// handle+client_request_id, once the first is live, returns the SAME peer under
// the STORED role/persona — the re-spawn never rewrites them (set-at-creation).
func TestSpawnIdempotentReSpawnKeepsStoredRolePersona(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()

	const (
		wantRole    = "manager"
		wantPersona = "You are a diligent manager."
	)
	first, err := f.lc.SpawnAsAccount(ctx, f.agentID, &compassv1internal.SpawnPeerRequest{
		Handle:          "peer-dup-rp",
		ClientRequestId: "spawn-dup-rp",
		Role:            wantRole,
		Persona:         wantPersona,
	})
	if err != nil {
		t.Fatalf("first SpawnAsAccount = %v, want success", err)
	}

	// A retry naming a DIFFERENT (but still valid-taxonomy) role/persona must not
	// rewrite the stored values. "owner" is a valid member — the point is that the
	// resume ignores the retry's values, not that the role is rejected.
	second, err := f.lc.SpawnAsAccount(ctx, f.agentID, &compassv1internal.SpawnPeerRequest{
		Handle:          "peer-dup-rp",
		ClientRequestId: "spawn-dup-rp",
		Role:            "owner",
		Persona:         "different",
	})
	if err != nil {
		t.Fatalf("retry SpawnAsAccount = %v, want idempotent success", err)
	}
	if second.GetAgentAccountId() != first.GetAgentAccountId() {
		t.Fatalf("retry returned a different agent id %q, want the first %q", second.GetAgentAccountId(), first.GetAgentAccountId())
	}

	acc, err := f.store.GetAccount(ctx, store.AccountID(first.GetAgentAccountId()))
	if err != nil {
		t.Fatalf("GetAccount(spawned) = %v", err)
	}
	if got := acc.Agent.Role; got != wantRole {
		t.Fatalf("stored role after re-spawn = %q, want the first-spawn %q (set-at-creation, never rewritten)", got, wantRole)
	}
	if got := acc.Agent.Persona; got != wantPersona {
		t.Fatalf("stored persona after re-spawn = %q, want the first-spawn %q (set-at-creation, never rewritten)", got, wantPersona)
	}
}

// TestSpawnResumeReprovisionThreadsStoredRolePersona covers the UNPLACED-resume
// branch (lifecycle.go resumeOrReject -> provisionAndStart with the EXISTING
// account's stored persona/role, design.md:215): a spawn that crashed after
// CreateAgent leaves the account unplaced, and a re-spawn of the same handle
// re-provisions it. This path threads the STORED role/persona to the Runner, so
// a retry naming DIFFERENT values must still provision under the first-spawn's
// stored values — never the retry's. Without this test a regression passing
// req.GetRole()/GetPersona() on the resume branch (the exact mistake T4 fixes on
// the create branch) would ship green: the stored row stays correct while an
// injected role reaches the Runner on re-provision.
func TestSpawnResumeReprovisionThreadsStoredRolePersona(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()

	const (
		wantRole    = "manager"
		wantPersona = "You are a diligent manager."
	)
	// First spawn crashes mid-chain: the Runner refuses Start, so provisionAndStart
	// rolls the container back and leaves the account UNPLACED with no recorded
	// session — the genuine crashed-after-CreateAgent state the resume path exists
	// to recover (mirrors lifecycle_pgtest_test.go's rollback-then-resume).
	f.runner.setFailStart(true)
	_, err := f.lc.SpawnAsAccount(ctx, f.agentID, &compassv1internal.SpawnPeerRequest{
		Handle:          "peer-resume-rp",
		ClientRequestId: "spawn-resume-rp-1",
		Role:            wantRole,
		Persona:         wantPersona,
	})
	if err == nil {
		t.Fatal("first SpawnAsAccount = nil, want a mid-chain failure (Runner refused Start)")
	}
	// The account exists (CreateAgent committed) with its stored role/persona, but
	// is unplaced. Confirm the stored values are the first-spawn's.
	created, err := f.store.AgentByHandle(ctx, f.ownerAdmin, "peer-resume-rp")
	if err != nil {
		t.Fatalf("AgentByHandle(peer-resume-rp) after crash = %v, want the created-but-unplaced account", err)
	}
	if created.Agent.Role != wantRole || created.Agent.Persona != wantPersona {
		t.Fatalf("stored (role,persona) = (%q,%q), want (%q,%q)", created.Agent.Role, created.Agent.Persona, wantRole, wantPersona)
	}
	f.runner.setFailStart(false) // the Runner accepts Start again: the resume can complete
	f.runner.forget()            // drop the failed first attempt; assert only on the resume

	// Re-spawn the same handle under a DISTINCT client_request_id (so it reaches
	// CreateAgent, conflicts on the handle, and resumes the unplaced account)
	// naming a DIFFERENT (still valid-taxonomy) role/persona — the injection
	// attempt the resume must ignore. "owner" is a valid member; the resume must
	// re-provision under the stored first-spawn values, never the retry's.
	second, err := f.lc.SpawnAsAccount(ctx, f.agentID, &compassv1internal.SpawnPeerRequest{
		Handle:          "peer-resume-rp",
		ClientRequestId: "spawn-resume-rp-2",
		Role:            "owner",
		Persona:         "different",
	})
	if err != nil {
		t.Fatalf("resume SpawnAsAccount = %v, want success", err)
	}
	if second.GetAgentAccountId() != string(created.ID) {
		t.Fatalf("resume returned a different agent id %q, want the first %q (a resume, not a second account)", second.GetAgentAccountId(), created.ID)
	}

	// The load-bearing assertion: the Provision wire on the RE-provision carries
	// the STORED first-spawn values, not the retry's injected ones.
	if got := f.runner.provisionRole(t); got != wantRole {
		t.Fatalf("re-provision wire role = %q, want the stored %q (never the retry's injected role)", got, wantRole)
	}
	if got := f.runner.provisionPersona(t); got != wantPersona {
		t.Fatalf("re-provision wire persona = %q, want the stored %q (never the retry's injected persona)", got, wantPersona)
	}
}
