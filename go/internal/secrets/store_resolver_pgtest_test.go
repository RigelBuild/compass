//go:build pgtest && unix

// T3 of the compass-user-secret-store record: the DB-backed StoreResolver over
// the real pgtest store. These prove the crypto seam end-to-end THROUGH the
// resolver, not just the store: the Upsert→ResolveFor round-trip with the A9
// most-specific-wins precedence observed, cross-scope isolation, the AAD tamper
// property (a row whose coordinate is altered behind the resolver's back fails to
// decrypt rather than leaking a value), NUL-field refusal on the write door, and
// the read-side authz gate for a non-agent principal.
//
// context.Background() is the test root (rule://go-thread-context _test.go
// exemption); it is threaded into store.Open and every resolver call below.

package secrets

import (
	"context"
	"errors"
	"testing"

	"github.com/RigelBuild/compass/go/internal/envelope"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

// resolverFixture opens a real store against a fresh schema and builds a
// StoreResolver over it with a fixed key. The RequireDSN call is FIRST so the
// suite skips cleanly in a container-less sandbox before any guard runs (record
// A8: fail-closed guards must come after the store fixture).
func resolverFixture(t *testing.T) (*store.Store, *StoreResolver) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, pgtest.RequireDSN(t))
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	return st, NewStoreResolver(st, fixtureKey(t), 1)
}

// fixtureKey is a fixed 32-byte AES key. The value is irrelevant — only that one
// resolver's encrypt and decrypt use the same key.
func fixtureKey(t *testing.T) envelope.Key {
	t.Helper()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	k, err := envelope.NewKey(raw)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	return k
}

// mustUser creates a human account through the public door or fails the test.
func mustUser(t *testing.T, s *store.Store, handle string) store.Account {
	t.Helper()
	u, err := s.CreateUser(context.Background(), store.NewUser{Handle: handle, DisplayName: handle})
	if err != nil {
		t.Fatalf("CreateUser %q: %v", handle, err)
	}
	return u
}

// mustAgent creates an owned agent account through the public door or fails.
func mustAgent(t *testing.T, s *store.Store, owner store.AccountID, handle string) store.Account {
	t.Helper()
	a, err := s.CreateAgent(context.Background(), owner, store.NewAgent{Handle: handle, DisplayName: handle})
	if err != nil {
		t.Fatalf("CreateAgent %q: %v", handle, err)
	}
	return a
}

// resolvedByName indexes ResolveFor output by name, failing on a duplicate — the
// A9 collapse returns exactly one row per name.
func resolvedByName(t *testing.T, rs []ResolvedSecret) map[string]ResolvedSecret {
	t.Helper()
	out := map[string]ResolvedSecret{}
	for _, r := range rs {
		if _, dup := out[r.Name]; dup {
			t.Fatalf("ResolveFor returned two rows for name %q — precedence collapse broken", r.Name)
		}
		out[r.Name] = r
	}
	return out
}

// TestResolveForRoundTripPrecedence proves the Upsert→ResolveFor round-trip AND
// the A9 most-specific-wins precedence observed end to end through the resolver:
// a name at all three tiers resolves to the agent value, at tenant+user to the
// user value, tenant-only to the tenant value.
func TestResolveForRoundTripPrecedence(t *testing.T) {
	ctx := context.Background()
	st, r := resolverFixture(t)
	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")

	// ALL3 exists at every tier; USR at tenant+user; TEN at tenant only.
	mustUpsert(t, r, owner.ID, "ALL3", store.SecretScopeTenant, "", "tenant-all3")
	mustUpsert(t, r, owner.ID, "ALL3", store.SecretScopeUser, string(owner.ID), "user-all3")
	mustUpsert(t, r, owner.ID, "ALL3", store.SecretScopeAgent, string(agent.ID), "agent-all3")
	mustUpsert(t, r, owner.ID, "USR", store.SecretScopeTenant, "", "tenant-usr")
	mustUpsert(t, r, owner.ID, "USR", store.SecretScopeUser, string(owner.ID), "user-usr")
	mustUpsert(t, r, owner.ID, "TEN", store.SecretScopeTenant, "", "tenant-ten")

	got, err := r.ResolveFor(ctx, agent.ID, "test")
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	by := resolvedByName(t, got)
	for name, want := range map[string]string{
		"ALL3": "agent-all3",
		"USR":  "user-usr",
		"TEN":  "tenant-ten",
	} {
		if by[name].Value != want {
			t.Errorf("ResolveFor %q = %q, want %q (precedence)", name, by[name].Value, want)
		}
		// Version is the content hash of the decrypted value, computed per resolve.
		if by[name].Version != Version(want) {
			t.Errorf("ResolveFor %q version = %q, want Version(%q)", name, by[name].Version, want)
		}
	}
}

// TestResolveForCrossScopeIsolation proves another user's user-scoped row and
// another agent's agent-scoped row are absent from ResolveFor for the caller.
func TestResolveForCrossScopeIsolation(t *testing.T) {
	ctx := context.Background()
	st, r := resolverFixture(t)
	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")
	other := mustUser(t, st, "other")
	otherAgent := mustAgent(t, st, other.ID, "otheragent")

	mustUpsert(t, r, other.ID, "OTHER_USER", store.SecretScopeUser, string(other.ID), "other-user-val")
	mustUpsert(t, r, other.ID, "OTHER_AGENT", store.SecretScopeAgent, string(otherAgent.ID), "other-agent-val")

	got, err := r.ResolveFor(ctx, agent.ID, "test")
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ResolveFor leaked %d cross-scope rows: %v", len(got), got)
	}
}

// TestResolveForTamperFailsDecrypt proves the AAD binding: a row whose scope_id
// (or name) is altered in the DB behind the resolver's back fails to decrypt
// rather than returning a value. The ciphertext is sealed under coordinate A's
// AAD, then planted at coordinate B through the store's ciphertext door — the
// exact row-substitution the AAD exists to defeat.
func TestResolveForTamperFailsDecrypt(t *testing.T) {
	ctx := context.Background()
	st, r := resolverFixture(t)
	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")
	key := fixtureKey(t)

	// Seal a value under a DIFFERENT name's AAD (the "wrong" coordinate) but the
	// SAME tenant the row lands with, so the name is the only mismatched field —
	// then store the ciphertext at the agent-scoped row named MOVED. The stored
	// row's own coordinate (name=MOVED) no longer matches the AAD the ciphertext
	// was sealed under (name=ORIGINAL), so ResolveFor's row-bound AAD
	// authentication must fail.
	tenant := string(st.EffectiveTenant(ctx))
	wrongAAD, err := envelope.UserSecretAAD(tenant, store.SecretScopeAgent, string(agent.ID), "ORIGINAL", 1)
	if err != nil {
		t.Fatalf("wrong AAD: %v", err)
	}
	nonce, ct, err := key.Encrypt([]byte("moved-value"), wrongAAD)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := st.UpsertSecret(ctx, owner.ID, "MOVED", store.SecretScopeAgent, string(agent.ID),
		store.SecretDeliveryEnv, store.SecretKindGeneric, "", "", ct, nonce, 1); err != nil {
		t.Fatalf("plant tampered row: %v", err)
	}

	_, err = r.ResolveFor(ctx, agent.ID, "test")
	if !errors.Is(err, envelope.ErrDecrypt) {
		t.Fatalf("ResolveFor over a coordinate-mismatched row = %v, want ErrDecrypt", err)
	}
}

// TestResolveForForeignTenantFailsDecrypt proves the AAD's tenant field is live:
// a ciphertext sealed under one tenant fails to decrypt when planted in a row
// that belongs to a different tenant. This is the cross-tenant row-substitution
// RLS is the OUTER boundary for — every other AAD field (scope_kind, scope_id,
// name, key_version) can be identical across tenants, so the tenant is the only
// thing that distinguishes tenant A's row from tenant B's copy of it.
//
// It is the non-vacuity proof for the fix: the ciphertext is sealed under the
// foreign tenant "" while the planted row lands under this store's real
// (bootstrap) tenant. With the old inert "" binding the read path also used ""
// on every row, so the sealed "" matched and the foreign ciphertext decrypted —
// the tamper went undetected. Now the read binds the row's real tenant, "" no
// longer matches, and decrypt fails closed.
func TestResolveForForeignTenantFailsDecrypt(t *testing.T) {
	ctx := context.Background()
	st, r := resolverFixture(t)
	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")
	key := fixtureKey(t)

	// Sanity: the row's real tenant is a non-empty bootstrap id, distinct from the
	// foreign "" the ciphertext is sealed under — otherwise the mismatch is a
	// no-op and the test proves nothing.
	rowTenant := string(st.EffectiveTenant(ctx))
	if rowTenant == "" {
		t.Fatalf("bootstrap tenant is empty; foreign-tenant mismatch would be a no-op")
	}

	// Seal under tenant "" — a tenant the row does NOT belong to — but with the
	// row's exact scope/name/key_version, so tenant is the ONLY mismatched field.
	foreignAAD, err := envelope.UserSecretAAD("", store.SecretScopeAgent, string(agent.ID), "CROSS", 1)
	if err != nil {
		t.Fatalf("foreign AAD: %v", err)
	}
	nonce, ct, err := key.Encrypt([]byte("foreign-tenant-value"), foreignAAD)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := st.UpsertSecret(ctx, owner.ID, "CROSS", store.SecretScopeAgent, string(agent.ID),
		store.SecretDeliveryEnv, store.SecretKindGeneric, "", "", ct, nonce, 1); err != nil {
		t.Fatalf("plant foreign-tenant row: %v", err)
	}

	_, err = r.ResolveFor(ctx, agent.ID, "test")
	if !errors.Is(err, envelope.ErrDecrypt) {
		t.Fatalf("ResolveFor over a foreign-tenant ciphertext = %v, want ErrDecrypt", err)
	}
}

// TestUpsertRefusesNULField proves a NUL in a bound AAD field is refused by
// Upsert, propagating envelope.ErrAADField, before any row is written. scope_id
// is the reachable NUL-bearing field (name grammar forbids NUL at the store
// door; here the AAD boundary catches it first at the resolver).
func TestUpsertRefusesNULField(t *testing.T) {
	ctx := context.Background()
	st, r := resolverFixture(t)
	owner := mustUser(t, st, "owner")

	err := r.Upsert(ctx, owner.ID, "NULSEC", store.SecretScopeUser, "id\x00evil", "val",
		DeliveryEnv, SecretGeneric, "", "")
	if !errors.Is(err, envelope.ErrAADField) {
		t.Fatalf("Upsert with NUL scope_id = %v, want ErrAADField", err)
	}
}

// TestResolveForUnknownPrincipalEmpty proves the read-side authz gate observed
// through the resolver: a non-agent (a user) principal resolves zero rows even
// against a tenant-scoped secret every real agent would see — the T2 INNER JOIN
// gate, now surfaced by ResolveFor.
func TestResolveForUnknownPrincipalEmpty(t *testing.T) {
	ctx := context.Background()
	st, r := resolverFixture(t)
	owner := mustUser(t, st, "owner")

	// Bait: a tenant-scoped row any real agent would resolve.
	mustUpsert(t, r, owner.ID, "SHARED", store.SecretScopeTenant, "", "tenant-val")

	for _, principal := range []store.AccountID{owner.ID, store.AccountID("acc_nope")} {
		got, err := r.ResolveFor(ctx, principal, "test")
		if err != nil {
			t.Fatalf("ResolveFor(%s): %v", principal, err)
		}
		if len(got) != 0 {
			t.Errorf("ResolveFor(%s) = %d rows, want 0 — authz gate leaked", principal, len(got))
		}
	}
}

// TestRemoveScopeAddressed proves Remove deletes at the addressed coordinate
// (gone from ResolveFor) and reports ErrNotFound for an absent coordinate.
func TestRemoveScopeAddressed(t *testing.T) {
	ctx := context.Background()
	st, r := resolverFixture(t)
	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")

	mustUpsert(t, r, owner.ID, "GONE", store.SecretScopeTenant, "", "bye")
	if err := r.Remove(ctx, owner.ID, "GONE", store.SecretScopeTenant, ""); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got, err := r.ResolveFor(ctx, agent.ID, "test")
	if err != nil {
		t.Fatalf("ResolveFor after remove: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ResolveFor still returns %d rows after Remove", len(got))
	}
	if err := r.Remove(ctx, owner.ID, "GONE", store.SecretScopeTenant, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Remove absent coordinate = %v, want ErrNotFound", err)
	}
}

// mustUpsert writes value at the given scope through the resolver or fails.
func mustUpsert(t *testing.T, r *StoreResolver, actor store.AccountID, name string, scopeKind int16, scopeID, value string) {
	t.Helper()
	if err := r.Upsert(context.Background(), actor, name, scopeKind, scopeID, value,
		DeliveryEnv, SecretGeneric, "", ""); err != nil {
		t.Fatalf("Upsert %s@%d/%s: %v", name, scopeKind, scopeID, err)
	}
}
