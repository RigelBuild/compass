//go:build pgtest

package store

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RigelBuild/compass/go/internal/pgtest"
)

// TestChannelGroupNamesMigrationRepairsExistingRows replays the channel-group-names
// migration over pre-constraint rows: slashes and duplicate siblings are renamed,
// a valid original keeps its name, and reserved top-level names stay untouched.
func TestChannelGroupNamesMigrationRepairsExistingRows(t *testing.T) {
	ctx := t.Context()
	dsn := pgtest.RequireDSN(t)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	// Found by name: the slot is renumbered whenever a concurrent migration lands first.
	target := slices.IndexFunc(migs, func(m migration) bool { return strings.HasSuffix(m.name, "_channel_group_names.sql") })
	if target < 0 {
		t.Fatal("channel-group-names migration not embedded")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire migration connection: %v", err)
	}
	defer conn.Release()
	// 0001 edits cluster-global roles, so hold the lock migrate holds or a parallel package's Open races it.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		t.Fatalf("acquire migration lock: %v", err)
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockKey); err != nil {
			t.Errorf("release migration lock: %v", err)
		}
	}()
	if err := ensureMigrationsTable(ctx, conn); err != nil {
		t.Fatalf("ensure migrations table: %v", err)
	}
	for _, m := range migs[:target] {
		if err := applyMigration(ctx, conn, m); err != nil {
			t.Fatalf("apply v%d: %v", m.version, err)
		}
	}

	// The owner connection skips RLS only as superuser, so seed as the system role.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit; a failed seed already fails the test
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+systemRole); err != nil {
		t.Fatalf("set seed role: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO tenants (id, slug, display_name, created_at_unix_ms)
		VALUES ('boot', 'default', 'Default', 1), ('boot-2', 'second', 'Second', 2);
		INSERT INTO accounts (id, handle, display_name, tenant_id) VALUES
			('owner-k', 'owner-k', 'Owner K', 'boot'),
			('agent-k1', 'agent-k1', 'Agent K1', 'boot'),
			('agent-k2', 'agent-k2', 'Agent K2', 'boot');
		INSERT INTO user_accounts (account_id, tenant_id) VALUES ('owner-k', 'boot');
		INSERT INTO agent_accounts (account_id, owner_user_id, tenant_id) VALUES
			('agent-k1', 'owner-k', 'boot'), ('agent-k2', 'owner-k', 'boot');
		INSERT INTO channel_groups (id, name, owner_user_id, tenant_id, parent_group_id) VALUES
			('group-a', 'duplicate', 'owner-a', 'boot', NULL),
			('group-b', 'duplicate', 'owner-a', 'boot', NULL),
			('group-c', 'duplicate-2', 'owner-a', 'boot', NULL),
			('group-d', 'path/name', 'owner-b', 'boot', NULL),
			('group-e', '__dm__', 'owner-c', 'boot', NULL),
			('group-f', '__dm__', 'owner-c', 'boot', NULL),
			('group-g', 'a/b', 'owner-d', 'boot', NULL),
			('group-h', 'a-b', 'owner-d', 'boot', NULL),
			('group-i', 'parent', 'owner-e', 'boot', NULL),
			('group-j', 'nested', 'owner-e', 'boot', 'group-i'),
			('group-k', 'nested', 'owner-e', 'boot', 'group-i'),
			('group-l', 'tenant-name', 'owner-f', 'boot', NULL),
			('group-m', 'tenant-name', 'owner-f', 'boot-2', NULL),
			('group-n', '__dm__', 'owner-e', 'boot', 'group-i'),
			('group-o', '__dm__', 'owner-e', 'boot', 'group-i'),
			('group-p', 'team', 'owner-k', 'boot', NULL),
			('group-q', 'team', 'agent-k1', 'boot', NULL),
			('group-r', 'svc', 'agent-k1', 'boot', 'group-p'),
			('group-s', 'svc', 'agent-k2', 'boot', 'group-p'),
			('group-t', 'pub', 'owner-g', 'boot', NULL),
			('group-u', 'infra', 'owner-g', 'boot', 'group-t'),
			('group-v', 'infra', 'owner-h', 'boot', 'group-t')`); err != nil {
		t.Fatalf("seed pre-migration groups: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	if err := applyMigration(ctx, conn, migs[target]); err != nil {
		t.Fatalf("apply channel-group-names migration: %v", err)
	}

	got := readGroupNamesAsSystem(t, pool)
	want := map[string]string{
		"group-a": "duplicate",     // lowest id keeps the name
		"group-b": "duplicate-3",   // -2 is taken by group-c
		"group-c": "duplicate-2",   // a pre-existing valid name is never clobbered
		"group-d": "path-name",     // slash rewritten
		"group-e": "__dm__",        // reserved top-level names are exempt
		"group-f": "__dm__",        // reserved top-level names are exempt
		"group-g": "a-b-2",         // a rewritten name yields to the valid original
		"group-h": "a-b",           // the valid original keeps its name
		"group-i": "parent",        // nested parent untouched
		"group-j": "nested",        // nested siblings dedupe too
		"group-k": "nested-2",      // nested siblings dedupe too
		"group-l": "tenant-name",   // owner ids are global, so tenants share a namespace
		"group-m": "tenant-name-2", // owner ids are global, so tenants share a namespace
		"group-n": "__dm__",        // the reserved exemption is top-level only
		"group-o": "__dm__-2",      // the reserved exemption is top-level only
		"group-p": "team",          // an agent's group shares its owner's namespace
		"group-q": "team-2",        // an agent's group shares its owner's namespace
		"group-r": "svc",           // two agents of one owner share a namespace
		"group-s": "svc-2",         // two agents of one owner share a namespace
		"group-u": "infra",         // nested names are unique per parent
		"group-v": "infra-2",       // nested names are unique per parent, across namespaces
	}
	for id, name := range want {
		if got[id] != name {
			t.Errorf("group %s name = %q, want %q", id, got[id], name)
		}
	}

	namespaces := readGroupNamespacesAsSystem(t, pool)
	for id, owner := range map[string]string{"group-a": "owner-a", "group-p": "owner-k", "group-q": "owner-k", "group-s": "owner-k"} {
		if namespaces[id] != owner {
			t.Errorf("group %s namespace = %q, want %q", id, namespaces[id], owner)
		}
	}

	var indexExists, checkExists bool
	if err := pool.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = current_schema()
			AND indexname = 'channel_groups_owner_parent_name_key'),
		EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'channel_groups_name_no_slash'
			AND conrelid = 'channel_groups'::regclass AND convalidated)`).Scan(&indexExists, &checkExists); err != nil {
		t.Fatalf("inspect constraints: %v", err)
	}
	if !indexExists {
		t.Error("sibling unique index does not exist")
	}
	if !checkExists {
		t.Error("validated no-slash check does not exist")
	}
}

func readGroupNamesAsSystem(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	return readGroupColumnAsSystem(t, pool, "name")
}

func readGroupNamespacesAsSystem(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	return readGroupColumnAsSystem(t, pool, "namespace_owner_id")
}

// readGroupColumnAsSystem maps each group id to one text column; column is a
// test constant, never input.
func readGroupColumnAsSystem(t *testing.T, pool *pgxpool.Pool, column string) map[string]string {
	t.Helper()
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin read: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // read-only tx; rollback is cleanup only
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+systemRole); err != nil {
		t.Fatalf("set read role: %v", err)
	}
	rows, err := tx.Query(ctx, "SELECT id, "+column+" FROM channel_groups")
	if err != nil {
		t.Fatalf("read repaired groups: %v", err)
	}
	defer rows.Close()
	got := make(map[string]string)
	for rows.Next() {
		var id, value string
		if err := rows.Scan(&id, &value); err != nil {
			t.Fatalf("scan repaired group: %v", err)
		}
		got[id] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate repaired groups: %v", err)
	}
	return got
}
