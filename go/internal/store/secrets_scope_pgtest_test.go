//go:build pgtest && unix

// T2 of the compass-user-secret-store record: the scoped, encrypted user-secret
// store. These prove the A9 scope model (tenant/user/agent, most-specific-wins)
// and the A1 value columns against real Postgres — the composite-PK resolution
// collapse, cross-scope isolation, tenant sharing, the upsert value-rewrite vs
// second-scope-insert distinction, the door-side scope-shape + reserved-prefix
// validation (and that the CHECK backs the door if bypassed), scope-addressed
// delete, and that a stored ciphertext never contains its plaintext.

package store

import (
	"bytes"
	"context"
	"testing"

	"github.com/RigelBuild/compass/go/internal/envelope"
)

// testKey is a fixed 32-byte AES key for the ciphertext round-trips. The value
// is irrelevant — only that Encrypt/Decrypt use the same one.
func testKey(t *testing.T) envelope.Key {
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

// upsertValue encrypts value under the row's AAD and upserts it at the given
// scope. tenantID is "" here: the store resolves the bootstrap tenant for the
// row's tenant_id, and the AAD binds whatever the caller passes — the tests only
// need self-consistency between encrypt and the later decrypt, so they bind "".
func upsertValue(t *testing.T, s *Store, key envelope.Key, actor AccountID, name string, scopeKind int16, scopeID, value string) {
	t.Helper()
	ctx := context.Background()
	aad := envelope.UserSecretAAD("", scopeKind, scopeID, name, 1)
	nonce, ct, err := key.Encrypt([]byte(value), aad)
	if err != nil {
		t.Fatalf("encrypt %s@%d/%s: %v", name, scopeKind, scopeID, err)
	}
	if err := s.UpsertSecret(ctx, actor, name, scopeKind, scopeID,
		SecretDeliveryEnv, SecretKindGeneric, "", "", ct, nonce, 1); err != nil {
		t.Fatalf("UpsertSecret %s@%d/%s: %v", name, scopeKind, scopeID, err)
	}
}

// recordsByName indexes an agent's resolved records by name, failing on a
// duplicate — the DISTINCT ON contract is exactly one row per name.
func recordsByName(t *testing.T, recs []SecretRecord) map[string]SecretRecord {
	t.Helper()
	out := map[string]SecretRecord{}
	for _, r := range recs {
		if _, dup := out[r.Name]; dup {
			t.Fatalf("SecretRecordsForAgent returned two rows for name %q — DISTINCT ON broken", r.Name)
		}
		out[r.Name] = r
	}
	return out
}

// TestSecretScopePrecedence proves the A9 most-specific-wins collapse: a name at
// all three tiers resolves to the agent row; at tenant+user to the user row;
// tenant-only to the tenant row — exactly one row per name for the caller.
func TestSecretScopePrecedence(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	key := testKey(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	// THREE: present at tenant, user, and agent scope.
	upsertValue(t, s, key, owner.ID, "THREE", SecretScopeTenant, "", "tenant-three")
	upsertValue(t, s, key, owner.ID, "THREE", SecretScopeUser, string(owner.ID), "user-three")
	upsertValue(t, s, key, agent.ID, "THREE", SecretScopeAgent, string(agent.ID), "agent-three")
	// TWO: tenant + user only.
	upsertValue(t, s, key, owner.ID, "TWO", SecretScopeTenant, "", "tenant-two")
	upsertValue(t, s, key, owner.ID, "TWO", SecretScopeUser, string(owner.ID), "user-two")
	// ONE: tenant only.
	upsertValue(t, s, key, owner.ID, "ONE", SecretScopeTenant, "", "tenant-one")

	recs, err := s.SecretRecordsForAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("SecretRecordsForAgent: %v", err)
	}
	byName := recordsByName(t, recs)
	if len(byName) != 3 {
		t.Fatalf("resolved %d names, want 3: %+v", len(byName), byName)
	}

	// Decrypt each winner under its own scope AAD and check it is the expected
	// tier — a wrong tier would either decrypt to the wrong value or fail AAD.
	for name, want := range map[string]struct {
		scope int16
		id    string
		value string
	}{
		"THREE": {SecretScopeAgent, string(agent.ID), "agent-three"},
		"TWO":   {SecretScopeUser, string(owner.ID), "user-two"},
		"ONE":   {SecretScopeTenant, "", "tenant-one"},
	} {
		r := byName[name]
		if r.ScopeKind != want.scope || r.ScopeID != want.id {
			t.Errorf("%s: resolved scope (%d,%q), want (%d,%q)", name, r.ScopeKind, r.ScopeID, want.scope, want.id)
		}
		aad := envelope.UserSecretAAD("", r.ScopeKind, r.ScopeID, r.Name, r.KeyVersion)
		pt, err := key.Decrypt(r.ValueNonce, r.ValueCiphertext, aad)
		if err != nil {
			t.Fatalf("%s: decrypt winner: %v", name, err)
		}
		if string(pt) != want.value {
			t.Errorf("%s: decrypted %q, want %q", name, pt, want.value)
		}
	}
}

// TestSecretScopeIsolation proves a user-scoped row of another user and an
// agent-scoped row of another agent are NOT returned to the calling agent.
func TestSecretScopeIsolation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	key := testKey(t)
	owner := mustUser(t, s, "owner")
	other := mustUser(t, s, "other")
	agent := mustAgent(t, s, owner.ID, "agent")
	otherAgent := mustAgent(t, s, other.ID, "other-agent")

	// Rows the calling agent must NOT see: another user's user row and another
	// agent's agent row, both under a name the caller has no own row for.
	upsertValue(t, s, key, other.ID, "FOREIGN_USER", SecretScopeUser, string(other.ID), "other-user-val")
	upsertValue(t, s, key, otherAgent.ID, "FOREIGN_AGENT", SecretScopeAgent, string(otherAgent.ID), "other-agent-val")
	// A tenant row the caller SHOULD see, to prove the query returns anything.
	upsertValue(t, s, key, owner.ID, "SHARED", SecretScopeTenant, "", "shared-val")

	recs, err := s.SecretRecordsForAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("SecretRecordsForAgent: %v", err)
	}
	byName := recordsByName(t, recs)
	if _, ok := byName["FOREIGN_USER"]; ok {
		t.Error("another user's user-scoped row leaked to the calling agent")
	}
	if _, ok := byName["FOREIGN_AGENT"]; ok {
		t.Error("another agent's agent-scoped row leaked to the calling agent")
	}
	if _, ok := byName["SHARED"]; !ok {
		t.Error("tenant-scoped row not resolved for the calling agent")
	}
}

// TestSecretScopeTenantSharing proves the ruling that motivated the whole scope
// model: ONE tenant-scoped row resolves for two different agents under two
// different owning users.
func TestSecretScopeTenantSharing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	key := testKey(t)
	userA := mustUser(t, s, "user-a")
	userB := mustUser(t, s, "user-b")
	agentA := mustAgent(t, s, userA.ID, "agent-a")
	agentB := mustAgent(t, s, userB.ID, "agent-b")

	upsertValue(t, s, key, userA.ID, "SHARED", SecretScopeTenant, "", "one-shared-value")

	for _, ag := range []Account{agentA, agentB} {
		recs, err := s.SecretRecordsForAgent(ctx, ag.ID)
		if err != nil {
			t.Fatalf("SecretRecordsForAgent(%s): %v", ag.ID, err)
		}
		byName := recordsByName(t, recs)
		r, ok := byName["SHARED"]
		if !ok {
			t.Fatalf("agent %s did not resolve the shared tenant row", ag.ID)
		}
		if r.ScopeKind != SecretScopeTenant {
			t.Errorf("agent %s resolved SHARED at scope %d, want tenant", ag.ID, r.ScopeKind)
		}
		aad := envelope.UserSecretAAD("", r.ScopeKind, r.ScopeID, r.Name, r.KeyVersion)
		pt, err := key.Decrypt(r.ValueNonce, r.ValueCiphertext, aad)
		if err != nil || string(pt) != "one-shared-value" {
			t.Errorf("agent %s: shared value = %q,%v, want one-shared-value", ag.ID, pt, err)
		}
	}
}

// TestSecretUpsertRewriteVsSecondScope proves the composite-PK contract: the
// same (name, scope) rewrites its value; the SAME name at a DIFFERENT scope
// inserts a second row rather than overwriting the first.
func TestSecretUpsertRewriteVsSecondScope(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	key := testKey(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	// Fresh tenant insert, then rewrite the same coordinate.
	upsertValue(t, s, key, owner.ID, "DUP", SecretScopeTenant, "", "first")
	upsertValue(t, s, key, owner.ID, "DUP", SecretScopeTenant, "", "second")
	// Same name at agent scope: a distinct row, not an overwrite.
	upsertValue(t, s, key, agent.ID, "DUP", SecretScopeAgent, string(agent.ID), "agent-val")

	// Two physical rows for DUP: tenant + agent.
	var count int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM secrets WHERE name = 'DUP'`).Scan(&count); err != nil {
		t.Fatalf("count DUP rows: %v", err)
	}
	if count != 2 {
		t.Fatalf("DUP has %d rows, want 2 (tenant rewrite + agent insert)", count)
	}

	// The tenant row holds "second" (rewrite landed), the agent row "agent-val".
	tenantVal := decryptRow(t, s, key, "DUP", SecretScopeTenant, "")
	if tenantVal != "second" {
		t.Errorf("tenant DUP = %q, want second (rewrite)", tenantVal)
	}
	agentVal := decryptRow(t, s, key, "DUP", SecretScopeAgent, string(agent.ID))
	if agentVal != "agent-val" {
		t.Errorf("agent DUP = %q, want agent-val", agentVal)
	}
}

// decryptRow reads one row's ciphertext straight from the table and decrypts it
// under its scope AAD — the direct read the value-distinction tests assert on.
func decryptRow(t *testing.T, s *Store, key envelope.Key, name string, scopeKind int16, scopeID string) string {
	t.Helper()
	var ct, nonce []byte
	var kv int16
	if err := s.pool.QueryRow(context.Background(),
		`SELECT value_ciphertext, value_nonce, key_version FROM secrets
		  WHERE name = $1 AND scope_kind = $2 AND scope_id = $3`,
		name, scopeKind, scopeID).Scan(&ct, &nonce, &kv); err != nil {
		t.Fatalf("read row %s@%d/%s: %v", name, scopeKind, scopeID, err)
	}
	aad := envelope.UserSecretAAD("", scopeKind, scopeID, name, kv)
	pt, err := key.Decrypt(nonce, ct, aad)
	if err != nil {
		t.Fatalf("decrypt row %s@%d/%s: %v", name, scopeKind, scopeID, err)
	}
	return string(pt)
}

// TestSecretUpsertScopeShapeDoorValidation proves the store door rejects
// scope-shape violations with ErrInvalidArgument, not a raw Postgres error.
func TestSecretUpsertScopeShapeDoorValidation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")

	// Tenant scope with a non-empty scope_id.
	err := s.UpsertSecret(ctx, owner.ID, "BAD", SecretScopeTenant, string(owner.ID),
		SecretDeliveryEnv, SecretKindGeneric, "", "", []byte("ct"), []byte("nonce"), 1)
	sentinelIs(t, err, ErrInvalidArgument, "tenant scope with non-empty id")

	// User scope with an empty scope_id.
	err = s.UpsertSecret(ctx, owner.ID, "BAD", SecretScopeUser, "",
		SecretDeliveryEnv, SecretKindGeneric, "", "", []byte("ct"), []byte("nonce"), 1)
	sentinelIs(t, err, ErrInvalidArgument, "user scope with empty id")
}

// TestSecretScopeShapeCheckBacksTheDoor proves the CHECK is real, not merely
// mirrored in Go: a direct bad-shape insert is rejected by Postgres.
func TestSecretScopeShapeCheckBacksTheDoor(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")

	// A tenant row (scope_kind 0) carrying a non-empty scope_id violates
	// secrets_scope_shape. declared_by must be a real account (FK), so use owner.
	_, err := s.pool.Exec(ctx,
		`INSERT INTO secrets (name, scope_kind, scope_id, delivery, kind, declared_by)
		 VALUES ('DIRECT_BAD', 0, $1, 1, 0, $1)`, string(owner.ID))
	if err == nil {
		t.Fatal("direct bad-shape insert succeeded — secrets_scope_shape CHECK not enforcing")
	}
}

// TestSecretUpsertUnknownScopeAccount proves a user/agent write naming a scope_id
// that is not an account of that subtype is rejected at the door.
func TestSecretUpsertUnknownScopeAccount(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	// User scope naming an id that is not a user account (it is an agent).
	err := s.UpsertSecret(ctx, owner.ID, "UNK", SecretScopeUser, string(agent.ID),
		SecretDeliveryEnv, SecretKindGeneric, "", "", []byte("ct"), []byte("nonce"), 1)
	sentinelIs(t, err, ErrInvalidArgument, "user scope naming a non-user account")

	// Agent scope naming an id that is not an agent account (it is a user).
	err = s.UpsertSecret(ctx, owner.ID, "UNK", SecretScopeAgent, string(owner.ID),
		SecretDeliveryEnv, SecretKindGeneric, "", "", []byte("ct"), []byte("nonce"), 1)
	sentinelIs(t, err, ErrInvalidArgument, "agent scope naming a non-agent account")
}

// TestSecretUpsertReservedPrefixRejected proves the user door rejects a
// SERVER_-prefixed name AND a case-folded near-miss, exercising the case-folding
// ShadowsServerSecretPrefix predicate distinctly from the byte-exact admit check.
func TestSecretUpsertReservedPrefixRejected(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")

	// Exact-case reserved prefix.
	err := s.UpsertSecret(ctx, owner.ID, "SERVER_THING", SecretScopeTenant, "",
		SecretDeliveryEnv, SecretKindGeneric, "", "", []byte("ct"), []byte("nonce"), 1)
	sentinelIs(t, err, ErrInvalidArgument, "exact reserved prefix")

	// Case-folded near-miss: byte-exact HasServerSecretPrefix would ADMIT this,
	// so this row asserts the case-folding reject predicate is genuinely used.
	if HasServerSecretPrefix("server_thing") {
		t.Fatal("precondition broken: HasServerSecretPrefix should not match lowercase")
	}
	err = s.UpsertSecret(ctx, owner.ID, "server_thing", SecretScopeTenant, "",
		SecretDeliveryEnv, SecretKindGeneric, "", "", []byte("ct"), []byte("nonce"), 1)
	sentinelIs(t, err, ErrInvalidArgument, "case-folded reserved prefix near-miss")
}

// TestSecretDeleteScopeAddressed proves deleting at one scope leaves a
// same-named row at another scope intact.
func TestSecretDeleteScopeAddressed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	key := testKey(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	upsertValue(t, s, key, owner.ID, "KEEP", SecretScopeTenant, "", "tenant-keep")
	upsertValue(t, s, key, agent.ID, "KEEP", SecretScopeAgent, string(agent.ID), "agent-gone")

	// Delete only the agent-scoped row.
	if err := s.DeleteSecretDeclaration(ctx, owner.ID, "KEEP", SecretScopeAgent, string(agent.ID)); err != nil {
		t.Fatalf("delete agent-scoped KEEP: %v", err)
	}

	// The tenant row survives; the agent row is gone.
	if got := decryptRow(t, s, key, "KEEP", SecretScopeTenant, ""); got != "tenant-keep" {
		t.Errorf("tenant KEEP after agent delete = %q, want tenant-keep", got)
	}
	var count int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM secrets WHERE name = 'KEEP' AND scope_kind = $1 AND scope_id = $2`,
		SecretScopeAgent, string(agent.ID)).Scan(&count); err != nil {
		t.Fatalf("count agent KEEP: %v", err)
	}
	if count != 0 {
		t.Errorf("agent-scoped KEEP still present after scoped delete (count=%d)", count)
	}
}

// TestSecretCiphertextAtRest proves a stored value_ciphertext never contains the
// plaintext bytes — the whole point of encryption at rest.
func TestSecretCiphertextAtRest(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	key := testKey(t)
	owner := mustUser(t, s, "owner")

	const plaintext = "super-secret-database-url-value"
	upsertValue(t, s, key, owner.ID, "AT_REST", SecretScopeTenant, "", plaintext)

	var ct []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT value_ciphertext FROM secrets WHERE name = 'AT_REST' AND scope_kind = 0 AND scope_id = ''`).
		Scan(&ct); err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	if bytes.Contains(ct, []byte(plaintext)) {
		t.Fatal("value_ciphertext contains the plaintext bytes — not encrypted at rest")
	}
	if len(ct) == 0 {
		t.Fatal("value_ciphertext is empty")
	}
}

// TestShadowsServerSecretPrefix is a pure predicate unit test (no Postgres, but
// it rides the pgtest build with its neighbors): the case-folding reject side
// matches every case variant while the byte-exact admit side does not.
func TestShadowsServerSecretPrefix(t *testing.T) {
	reject := []string{"SERVER_X", "server_x", "SeRvEr_x", "GATEWAY_CREDENTIALS_K", "gateway_credentials_k"}
	for _, n := range reject {
		if !ShadowsServerSecretPrefix(n) {
			t.Errorf("ShadowsServerSecretPrefix(%q) = false, want true", n)
		}
	}
	admit := []string{"PLAIN", "SERVERX_Y", "server", "GATEWAY_CREDENTIAL"}
	for _, n := range admit {
		if ShadowsServerSecretPrefix(n) {
			t.Errorf("ShadowsServerSecretPrefix(%q) = true, want false", n)
		}
	}
	// The two predicates differ: lowercase is admitted byte-exact, rejected fold.
	if HasServerSecretPrefix("server_x") || !ShadowsServerSecretPrefix("server_x") {
		t.Error("admit/reject predicates should differ on lowercase server_x")
	}
}
