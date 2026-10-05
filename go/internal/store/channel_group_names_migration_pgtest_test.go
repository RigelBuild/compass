//go:build pgtest

package store

import (
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
			('group-m', 'tenant-name', 'owner-f', 'boot-2', NULL)`); err != nil {
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
	}
	for id, name := range want {
		if got[id] != name {
			t.Errorf("group %s name = %q, want %q", id, got[id], name)
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
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin read: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // read-only tx; rollback is cleanup only
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+systemRole); err != nil {
		t.Fatalf("set read role: %v", err)
	}
	rows, err := tx.Query(ctx, `SELECT id, name FROM channel_groups`)
	if err != nil {
		t.Fatalf("read repaired groups: %v", err)
	}
	defer rows.Close()
	got := make(map[string]string)
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatalf("scan repaired group: %v", err)
		}
		got[id] = name
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate repaired groups: %v", err)
	}
	return got
}
