//go:build pgtest && unix

package store

import (
	"context"
	"errors"
	"testing"
)

func TestT0ServerSecretsShape(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, tbl := range []string{"server_secrets", "server_key_state"} {
		var n int
		if err := s.pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_class c JOIN pg_namespace ns ON ns.oid=c.relnamespace
			  WHERE ns.nspname=current_schema() AND c.relname=$1 AND c.relkind='r'`, tbl).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("table %s not created", tbl)
		}
		var rls bool
		if err := s.pool.QueryRow(ctx,
			`SELECT c.relrowsecurity FROM pg_class c JOIN pg_namespace ns ON ns.oid=c.relnamespace
			  WHERE ns.nspname=current_schema() AND c.relname=$1`, tbl).Scan(&rls); err != nil {
			t.Fatal(err)
		}
		if rls {
			t.Fatalf("%s: RLS enabled, want bucket-A (disabled)", tbl)
		}
		var hasTenant int
		if err := s.pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.columns
			  WHERE table_schema=current_schema() AND table_name=$1 AND column_name='tenant_id'`, tbl).Scan(&hasTenant); err != nil {
			t.Fatal(err)
		}
		if hasTenant != 0 {
			t.Fatalf("%s: has tenant_id, want none (bucket A)", tbl)
		}
	}

	// The load-bearing half: grants are NOT inherited from 0001's snapshot, so
	// each table needs its own. Both tables are asserted with their OWN expected
	// privilege set, and server_key_state's withheld DELETE is asserted ABSENT —
	// that omission is a deliberate least-privilege choice (the tripwire digest
	// must not be droppable), so it is pinned, not left to chance.
	for _, tc := range []struct {
		tbl     string
		granted []string
		denied  []string
	}{
		{"server_secrets", []string{"SELECT", "INSERT", "UPDATE", "DELETE"}, nil},
		{"server_key_state", []string{"SELECT", "INSERT", "UPDATE"}, []string{"DELETE"}},
	} {
		for _, role := range []string{"compass_app", "compass_system"} {
			for _, priv := range tc.granted {
				var ok bool
				if err := s.pool.QueryRow(ctx,
					`SELECT has_table_privilege($1, (current_schema()||'.'||$2)::regclass, $3)`,
					role, tc.tbl, priv).Scan(&ok); err != nil {
					t.Fatal(err)
				}
				if !ok {
					t.Fatalf("%s: %s lacks %s", tc.tbl, role, priv)
				}
			}
			for _, priv := range tc.denied {
				var ok bool
				if err := s.pool.QueryRow(ctx,
					`SELECT has_table_privilege($1, (current_schema()||'.'||$2)::regclass, $3)`,
					role, tc.tbl, priv).Scan(&ok); err != nil {
					t.Fatal(err)
				}
				if ok {
					t.Fatalf("%s: %s has %s, which is deliberately withheld", tc.tbl, role, priv)
				}
			}
		}
	}

	// declared_by must be NULLABLE (server-provisioned rows carry no actor).
	var nullable string
	if err := s.pool.QueryRow(ctx,
		`SELECT is_nullable FROM information_schema.columns
		  WHERE table_schema=current_schema() AND table_name='server_secrets' AND column_name='declared_by'`).Scan(&nullable); err != nil {
		t.Fatal(err)
	}
	if nullable != "YES" {
		t.Fatalf("declared_by is_nullable=%s, want YES", nullable)
	}

	// server_key_state is single-row by construction.
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO server_key_state (id, key_fingerprint, fingerprint_salt) VALUES (1, '\x00', '\x01')`); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO server_key_state (id, key_fingerprint, fingerprint_salt) VALUES (2, '\x00', '\x01')`); err == nil {
		t.Fatal("id=2 accepted, want CHECK (id = 1) rejection")
	}
}

// TestT0KeyspacePartition is the F1 structural guard (design record D6): the two
// secret doors partition the keyspace by NAME, so a reserved-prefix name can
// only ever live in server_secrets and an unprefixed name only in secrets.
// Without both halves a user-path declare could mint a shadow `secrets` row
// under a server-secret name, which the inject-all container path then delivers
// into every agent container.
func TestT0KeyspacePartition(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	actor := mustUser(t, s, "declarer").ID

	// Server door REQUIRES a reserved prefix.
	if err := s.DeclareServerSecret(ctx, actor, "PLAIN_NAME"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unprefixed server secret: want ErrInvalidArgument, got %v", err)
	}
	for _, name := range []string{"SERVER_LINEAR_FORGE_CLIENT_SECRET", "GATEWAY_CREDENTIALS_MASTER_KEY"} {
		if err := s.DeclareServerSecret(ctx, actor, name); err != nil {
			t.Fatalf("declare %s: %v", name, err)
		}
	}

	// User door REJECTS those same prefixes.
	for _, name := range []string{"SERVER_LINEAR_FORGE_CLIENT_SECRET", "GATEWAY_CREDENTIALS_MASTER_KEY"} {
		err := s.DeclareSecret(ctx, actor, name, SecretDeliveryEnv, SecretKindGeneric, "", "")
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("user declare of reserved name %s: want ErrInvalidArgument, got %v", name, err)
		}
	}

	// The server registry is INVISIBLE to the container-delivery read path.
	// A legitimate user secret is declared first so this check has something to
	// iterate: without it the loop ran zero times and would have passed even if
	// both registries shared one table.
	if err := s.DeclareSecret(ctx, actor, "PLAIN_USER_TOKEN", SecretDeliveryEnv, SecretKindGeneric, "", ""); err != nil {
		t.Fatalf("declare user secret: %v", err)
	}
	userRows, err := s.DeclaredSecrets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(userRows) == 0 {
		t.Fatal("user registry is empty, so the leak check below cannot fail — fix the fixture")
	}
	for _, r := range userRows {
		if HasServerSecretPrefix(r.Name) {
			t.Fatalf("server secret %q leaked into the user registry", r.Name)
		}
	}
	// And the converse: the two server secrets declared above are present in the
	// server registry, so the isolation is genuinely two-way rather than one
	// empty set.
	serverRows, err := s.DeclaredServerSecrets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(serverRows) != 2 {
		t.Fatalf("server registry has %d rows, want the 2 declared above", len(serverRows))
	}
	for _, r := range serverRows {
		if !HasServerSecretPrefix(r.Name) {
			t.Fatalf("unprefixed name %q in the server registry", r.Name)
		}
	}

	// Duplicate is a conflict; delete is idempotent-checked via ErrNotFound.
	if err := s.DeclareServerSecret(ctx, actor, "SERVER_LINEAR_FORGE_CLIENT_SECRET"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate: want ErrConflict, got %v", err)
	}
	if err := s.DeleteServerSecretDeclaration(ctx, "SERVER_ABSENT"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent delete: want ErrNotFound, got %v", err)
	}
}

// TestT0ServerProvisionedRowHasNoActor pins the NULL declared_by contract: the
// boot provisioner declares the master key with no human actor, and that must
// round-trip as an empty AccountID rather than a falsified attribution.
func TestT0ServerProvisionedRowHasNoActor(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.DeclareServerSecret(ctx, "", "GATEWAY_CREDENTIALS_MASTER_KEY"); err != nil {
		t.Fatalf("server-provisioned declare: %v", err)
	}
	rows, err := s.DeclaredServerSecrets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].DeclaredBy != "" {
		t.Fatalf("want one row with empty DeclaredBy, got %+v", rows)
	}

	// And the resolver view maps it to a name-only declaration.
	view := ServerDeclaredSecrets{Store: s}
	decls, err := view.DeclaredSecrets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(decls) != 1 || decls[0].Name != "GATEWAY_CREDENTIALS_MASTER_KEY" || decls[0].Kind != SecretKindGeneric {
		t.Fatalf("view mapping: %+v", decls)
	}
}

// TestT0PrefixCheckSurvivesADoorBypass proves the F1 partition holds even for a
// writer that never calls DeclareServerSecret. The Go guard is the actionable
// error path; this CHECK is what keeps the invariant true if a future writer
// (a backfill, a repair path, a new in-package query) reaches the table
// directly. Note the escapes matter: `_` is a LIKE wildcard, so an unescaped
// pattern would admit "SERVERX_Y".
func TestT0PrefixCheckSurvivesADoorBypass(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, name := range []string{"PLAIN_NAME", "SERVERX_Y", "GATEWAY_CREDENTIALSX_K", "server_lowercase"} {
		_, err := s.pool.Exec(ctx, `INSERT INTO server_secrets (name) VALUES ($1)`, name)
		if err == nil {
			t.Fatalf("raw insert of %q succeeded — the reserved-prefix CHECK is not enforcing", name)
		}
	}
	for _, name := range []string{"SERVER_APP_PEM", "GATEWAY_CREDENTIALS_MASTER_KEY"} {
		if _, err := s.pool.Exec(ctx, `INSERT INTO server_secrets (name) VALUES ($1)`, name); err != nil {
			t.Fatalf("raw insert of legitimate %q rejected: %v", name, err)
		}
	}
}
