//go:build pgtest

package store

// Secret names-registry contracts (RIG-1327 T3): the round-trip of a declared
// row with its delivery/kind/provider/host/actor intact and name-ordered, the
// UNIQUE conflict on a duplicate name, the door name-validation that rejects a
// bad name before any row is written, the declared_by FK on an unknown actor,
// and the delete path (found → gone, unknown → ErrNotFound). NEVER a value:
// the registry stores names only.

import (
	"context"
	"testing"
)

func TestDeclareSecretRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "declarer")

	// A generic file secret, a provider secret carrying a Provider id, and a gh
	// secret carrying a Host — the three routing classes, declared out of name
	// order to prove the read orders them.
	if err := s.DeclareSecret(ctx, actor.ID, "ZED_TOKEN", SecretDeliveryFile, SecretKindGeneric, "", ""); err != nil {
		t.Fatalf("declare generic: %v", err)
	}
	if err := s.DeclareSecret(ctx, actor.ID, "ANTHROPIC_KEY", SecretDeliveryEnv, SecretKindProvider, "anthropic", ""); err != nil {
		t.Fatalf("declare provider: %v", err)
	}
	if err := s.DeclareSecret(ctx, actor.ID, "GH_TOKEN", SecretDeliveryFile, SecretKindGH, "", "github.com"); err != nil {
		t.Fatalf("declare gh: %v", err)
	}

	got, err := s.DeclaredSecrets(ctx)
	if err != nil {
		t.Fatalf("DeclaredSecrets: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("DeclaredSecrets returned %d rows, want 3", len(got))
	}

	// Name-ordered: ANTHROPIC_KEY < GH_TOKEN < ZED_TOKEN.
	wantOrder := []string{"ANTHROPIC_KEY", "GH_TOKEN", "ZED_TOKEN"}
	for i, w := range wantOrder {
		if got[i].Name != w {
			t.Errorf("row %d name = %q, want %q (name-ordered)", i, got[i].Name, w)
		}
	}

	// Fields round-trip intact, keyed by name.
	byName := map[string]SecretDeclaration{}
	for _, d := range got {
		byName[d.Name] = d
	}
	if d := byName["ANTHROPIC_KEY"]; d.Delivery != SecretDeliveryEnv || d.Kind != SecretKindProvider || d.Provider != "anthropic" || d.DeclaredBy != actor.ID {
		t.Errorf("provider secret round-trip mismatch: %+v", d)
	}
	if d := byName["GH_TOKEN"]; d.Kind != SecretKindGH || d.Host != "github.com" {
		t.Errorf("gh secret round-trip mismatch: %+v", d)
	}
	if d := byName["ZED_TOKEN"]; d.Delivery != SecretDeliveryFile || d.Kind != SecretKindGeneric || d.Provider != "" || d.Host != "" {
		t.Errorf("generic secret round-trip mismatch: %+v", d)
	}
	// Never a value: the struct has no value field; timestamps are set.
	if byName["ZED_TOKEN"].CreatedAt.IsZero() {
		t.Error("CreatedAt not populated on round-trip")
	}
}

func TestDeclareSecretDuplicateConflict(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "declarer")

	if err := s.DeclareSecret(ctx, actor.ID, "API_KEY", SecretDeliveryEnv, SecretKindGeneric, "", ""); err != nil {
		t.Fatalf("first declare: %v", err)
	}
	err := s.DeclareSecret(ctx, actor.ID, "API_KEY", SecretDeliveryFile, SecretKindGeneric, "", "")
	sentinelIs(t, err, ErrConflict, "duplicate secret name")
}

func TestDeclareSecretInvalidNameRejected(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "declarer")

	for _, bad := range []string{"bad-name", "", "a/b", "1abc", "a b", "../x"} {
		err := s.DeclareSecret(ctx, actor.ID, bad, SecretDeliveryEnv, SecretKindGeneric, "", "")
		sentinelIs(t, err, ErrInvalidArgument, "invalid secret name "+bad)
	}

	// The door rejects before Postgres: no row was ever written.
	got, err := s.DeclaredSecrets(ctx)
	if err != nil {
		t.Fatalf("DeclaredSecrets: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a bad name reached the table: %d rows written, want 0", len(got))
	}
}

func TestDeclareSecretUnknownActorInvalid(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// A well-formed name but an actor account that was never created → the
	// declared_by FK yields ErrInvalidArgument.
	err := s.DeclareSecret(ctx, AccountID("acct-never-created"), "API_KEY", SecretDeliveryEnv, SecretKindGeneric, "", "")
	sentinelIs(t, err, ErrInvalidArgument, "unknown declaring account")
}

func TestDeleteSecretDeclaration(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "declarer")

	if err := s.DeclareSecret(ctx, actor.ID, "API_KEY", SecretDeliveryEnv, SecretKindGeneric, "", ""); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if err := s.DeleteSecretDeclaration(ctx, actor.ID, "API_KEY"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	got, err := s.DeclaredSecrets(ctx)
	if err != nil {
		t.Fatalf("DeclaredSecrets: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("secret still listed after delete: %+v", got)
	}
}

func TestDeleteUnknownSecretNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "declarer")

	err := s.DeleteSecretDeclaration(ctx, actor.ID, "NEVER_DECLARED")
	sentinelIs(t, err, ErrNotFound, "delete unknown secret")
}

func TestDeclareSecretKindRoutingRejected(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "declarer")

	// Each row is a valid name + real actor (both validated BEFORE the kind
	// guard) paired with an out-of-invariant kind↔provider/host combo, so the
	// kind guard is the only thing that can trip. Every one must surface
	// ErrInvalidArgument at the door — never reaching a row.
	cases := []struct {
		name     string
		kind     SecretKind
		provider string
		host     string
	}{
		{"provider kind, empty provider", SecretKindProvider, "", ""},
		{"provider kind, non-empty host", SecretKindProvider, "anthropic", "github.com"},
		{"gh kind, empty host", SecretKindGH, "", ""},
		{"gh kind, non-empty provider", SecretKindGH, "anthropic", "github.com"},
		{"generic kind, non-empty provider", SecretKindGeneric, "anthropic", ""},
		{"generic kind, non-empty host", SecretKindGeneric, "", "github.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.DeclareSecret(ctx, actor.ID, "API_KEY", SecretDeliveryEnv, tc.kind, tc.provider, tc.host)
			sentinelIs(t, err, ErrInvalidArgument, tc.name)
		})
	}

	// The door rejects before Postgres: no out-of-invariant row was written.
	got, err := s.DeclaredSecrets(ctx)
	if err != nil {
		t.Fatalf("DeclaredSecrets: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("an out-of-invariant row reached the table: %d rows written, want 0", len(got))
	}
}

func TestDeclareSecretKindRoutingAccepted(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "declarer")

	// The three in-invariant combos: provider+provider-id, gh+host,
	// generic+neither. Asserts the guard is not simply rejecting everything.
	cases := []struct {
		name     string
		kind     SecretKind
		provider string
		host     string
	}{
		{"PROVIDER_KEY", SecretKindProvider, "anthropic", ""},
		{"GH_KEY", SecretKindGH, "", "github.com"},
		{"GENERIC_KEY", SecretKindGeneric, "", ""},
	}
	for _, tc := range cases {
		if err := s.DeclareSecret(ctx, actor.ID, tc.name, SecretDeliveryEnv, tc.kind, tc.provider, tc.host); err != nil {
			t.Errorf("%s: valid combo rejected: %v", tc.name, err)
		}
	}

	got, err := s.DeclaredSecrets(ctx)
	if err != nil {
		t.Fatalf("DeclaredSecrets: %v", err)
	}
	if len(got) != len(cases) {
		t.Errorf("valid combos not all accepted: %d rows written, want %d", len(got), len(cases))
	}
}
