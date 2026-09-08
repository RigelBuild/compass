//go:build pgtest

package store

// The set_updated_at trigger convention (RIG-3495), proven against real
// Postgres. updated_at is maintained by ONE mechanism — the BEFORE UPDATE
// trigger 0001_init.sql installs on every table carrying the column — and no
// query file sets it by hand. These tests are the enforcement of that: they
// fail if the trigger is missing, which is exactly what happens if someone
// re-adds a table without an updated_at_tables entry, or drops the block.
//
// Three properties, each a distinct failure mode:
//
//  1. A plain UPDATE advances updated_at and leaves created_at alone. The
//     created_at half is not padding: it is what catches a BEFORE INSERT OR
//     UPDATE trigger, which would look correct on every update assertion while
//     silently making created_at meaningless.
//  2. The INSERT ... ON CONFLICT DO UPDATE conflict path fires it. This is the
//     case most likely to be wrong, since the trigger is declared on UPDATE and
//     an upsert reads as an insert. Driven through the real store method
//     (RecordAgentPlacement), not raw SQL, so the covered thing is the path
//     production takes.
//  3. secrets.updated_at is live — the rot this change fixes.
//
// now() is TRANSACTION time in Postgres, so two writes inside one transaction
// observe the identical value. Every write below is its own statement on the
// pool (its own implicit transaction), which is what makes a strict > correct.
// A time.Sleep would only mask a wrong assertion; there is none here.
//
// context.Background is the test root (the pgtest-suite convention, sibling
// forge_cursors_pgtest_test.go).

import (
	"context"
	"testing"
	"time"
)

// TestUpdatedAtTriggerAdvancesOnUpdate proves property 1 on
// forge_repo_subscriptions: SetForgeRepoSubscriptionEnabled is a plain UPDATE
// that no longer sets updated_at itself, so an advanced value can only have
// come from the trigger — and created_at must not move with it.
func TestUpdatedAtTriggerAdvancesOnUpdate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	sub := ForgeRepoSubscription{
		Provider: ForgeProviderGitHub,
		Host:     "github.com",
		Repo:     "a/b",
		Enabled:  true,
	}
	if err := s.EnsureForgeRepoSubscription(ctx, sub); err != nil {
		t.Fatalf("EnsureForgeRepoSubscription: %v", err)
	}
	createdBefore, updatedBefore := repoSubStamps(t, s, sub)
	if !updatedBefore.Equal(createdBefore) {
		t.Fatalf("on insert updated_at %v != created_at %v; both default now() in one statement",
			updatedBefore, createdBefore)
	}

	// Separate statement => separate transaction => a strictly later now().
	if err := s.SetForgeRepoSubscriptionEnabled(ctx, sub.Provider, sub.Host, sub.Repo, false); err != nil {
		t.Fatalf("SetForgeRepoSubscriptionEnabled: %v", err)
	}
	createdAfter, updatedAfter := repoSubStamps(t, s, sub)

	if !updatedAfter.After(updatedBefore) {
		t.Errorf("updated_at did not advance on UPDATE: before=%v after=%v (set_updated_at trigger missing?)",
			updatedBefore, updatedAfter)
	}
	if !createdAfter.Equal(createdBefore) {
		t.Errorf("created_at moved on UPDATE: before=%v after=%v (trigger fires on INSERT too?)",
			createdBefore, createdAfter)
	}
}

// TestUpdatedAtTriggerFiresOnUpsertConflict proves property 2: an
// INSERT ... ON CONFLICT DO UPDATE takes the UPDATE path on conflict and so
// fires the BEFORE UPDATE trigger. RecordAgentPlacement is that upsert, and its
// query no longer carries a hand-written updated_at.
func TestUpdatedAtTriggerFiresOnUpsertConflict(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "placement-owner")
	agent := mustAgent(t, s, owner.ID, "placed")

	if err := s.RecordAgentPlacement(ctx, agent.ID, "runner-1", "compass-agent-1"); err != nil {
		t.Fatalf("RecordAgentPlacement (insert): %v", err)
	}
	createdBefore, updatedBefore := placementStamps(t, s, agent.ID)

	// Same agent_account_id => the ON CONFLICT DO UPDATE arm.
	if err := s.RecordAgentPlacement(ctx, agent.ID, "runner-2", "compass-agent-1"); err != nil {
		t.Fatalf("RecordAgentPlacement (conflict): %v", err)
	}
	createdAfter, updatedAfter := placementStamps(t, s, agent.ID)

	if !updatedAfter.After(updatedBefore) {
		t.Errorf("updated_at did not advance on the upsert conflict path: before=%v after=%v",
			updatedBefore, updatedAfter)
	}
	if !createdAfter.Equal(createdBefore) {
		t.Errorf("created_at moved on the upsert conflict path: before=%v after=%v",
			createdBefore, createdAfter)
	}
}

// TestSecretsUpdatedAtIsLive proves property 3 — the specific rot RIG-3495
// fixes. secrets.updated_at is declared, read by DeclaredSecrets, and surfaced
// on SecretDeclaration.UpdatedAt, but queries/secrets.sql has only an INSERT and
// a DELETE: no write path ever set the column, so its value could never differ
// from created_at and every reader was reading a lie.
//
// The store therefore still has no update method for a secret, and this test
// does NOT invent one. It asserts what the store's own surface can show (a
// freshly declared row has updated_at == created_at), and then drives a bare
// UPDATE on the table to prove the trigger is ARMED on secrets — so the column
// becomes correct for free the moment a re-declare/rotate path is added, rather
// than needing whoever adds it to remember.
func TestSecretsUpdatedAtIsLive(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "secrets-owner")

	if err := s.DeclareSecret(ctx, actor.ID, "DATABASE_URL", SecretDeliveryEnv, SecretKindGeneric, "", ""); err != nil {
		t.Fatalf("DeclareSecret: %v", err)
	}
	createdBefore, updatedBefore := secretStamps(t, s, "DATABASE_URL")
	if !updatedBefore.Equal(createdBefore) {
		t.Fatalf("on declare updated_at %v != created_at %v; both default now() in one statement",
			updatedBefore, createdBefore)
	}

	// A mutation of the row's own data, in its own transaction. The statement
	// deliberately does NOT mention updated_at: only the trigger can move it.
	if _, err := s.pool.Exec(ctx,
		`UPDATE secrets SET delivery = $2 WHERE name = $1`,
		"DATABASE_URL", int32(SecretDeliveryFile),
	); err != nil {
		t.Fatalf("update secret delivery: %v", err)
	}
	createdAfter, updatedAfter := secretStamps(t, s, "DATABASE_URL")

	if !updatedAfter.After(updatedBefore) {
		t.Errorf("secrets.updated_at did not advance on UPDATE: before=%v after=%v (the rot is not fixed)",
			updatedBefore, updatedAfter)
	}
	if !createdAfter.Equal(createdBefore) {
		t.Errorf("secrets.created_at moved on UPDATE: before=%v after=%v",
			createdBefore, createdAfter)
	}
}

// repoSubStamps reads a forge repo subscription's (created_at, updated_at).
func repoSubStamps(t *testing.T, s *Store, sub ForgeRepoSubscription) (created, updated time.Time) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(),
		`SELECT created_at, updated_at FROM forge_repo_subscriptions
		  WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3`,
		int16(sub.Provider), sub.Host, sub.Repo,
	).Scan(&created, &updated); err != nil {
		t.Fatalf("read forge_repo_subscriptions stamps: %v", err)
	}
	return created, updated
}

// placementStamps reads an agent placement's (created_at, updated_at).
func placementStamps(t *testing.T, s *Store, agent AccountID) (created, updated time.Time) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(),
		`SELECT created_at, updated_at FROM agent_placements WHERE agent_account_id = $1`,
		string(agent),
	).Scan(&created, &updated); err != nil {
		t.Fatalf("read agent_placements stamps: %v", err)
	}
	return created, updated
}

// secretStamps reads a declared secret's (created_at, updated_at).
func secretStamps(t *testing.T, s *Store, name string) (created, updated time.Time) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(),
		`SELECT created_at, updated_at FROM secrets WHERE name = $1`, name,
	).Scan(&created, &updated); err != nil {
		t.Fatalf("read secrets stamps: %v", err)
	}
	return created, updated
}
