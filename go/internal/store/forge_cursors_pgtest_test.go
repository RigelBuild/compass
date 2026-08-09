//go:build pgtest

package store

// Forge poll-driver store contracts (SEA-1810 T2, design
// docs/designs/product/compass-forge-poll-driver/design.md §T2 test cycle): the
// migration 0016 four-table shape (PKs, provider CHECK domain 1..4, page/kind
// CHECKs), the repo-LIST fetch cursor (read/upsert/prune with coordinate
// isolation), and the board's repo-subscription CRUD (ensure-insert idempotent,
// list-enabled ascending, soft-disable, unknown->ErrNotFound). context.Background
// is the test root (the pgtest-suite convention, sibling issues_pgtest_test.go).

import (
	"context"
	"testing"
	"time"
)

// ── Test 1: migration applies + all four tables exist with PKs/CHECKs ─────────

// TestMigration0016TablesExist proves the migration applied (newTestStore runs
// it) and every forge table is present by inserting the minimal legal row into
// each. The sequential migration harness proves both the empty-db and
// from-0015-db paths — newTestStore opens a freshly-migrated database, applying
// 0001..0016 in order.
func TestMigration0016TablesExist(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// forge_repo_subscriptions
	mustExec(t, s, `INSERT INTO forge_repo_subscriptions (forge_provider, forge_host, repo) VALUES (1, 'github.com', 'a/b')`)
	// forge_list_cursors
	mustExec(t, s, `INSERT INTO forge_list_cursors (forge_provider, forge_host, repo, page) VALUES (1, 'github.com', 'a/b', 1)`)
	// forge_artifact_cursors
	mustExec(t, s, `INSERT INTO forge_artifact_cursors (forge_provider, forge_host, repo, kind, number) VALUES (1, 'github.com', 'a/b', 1, 7)`)
	// agent_forge_subscriptions needs a real agent_account_id (FK); seed one.
	owner := mustUser(t, s, "forge-owner")
	agent := mustAgent(t, s, owner.ID, "forge-agent")
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO agent_forge_subscriptions (id, agent_account_id, forge_provider, forge_host, repo, kind, number)
		 VALUES ('afs-1', $1, 1, 'github.com', 'a/b', 1, 7)`, string(agent.ID)); err != nil {
		t.Fatalf("insert agent_forge_subscriptions: %v", err)
	}
}

// TestMigration0016ChecksReject proves the page CHECK rejects 0 and the kind
// CHECKs reject 0 and 3.
func TestMigration0016ChecksReject(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.pool.Exec(ctx,
		`INSERT INTO forge_list_cursors (forge_provider, forge_host, repo, page) VALUES (1, 'github.com', 'a/b', 0)`); err == nil {
		t.Fatal("page 0 accepted, want CHECK rejection")
	}
	for _, kind := range []int{0, 3} {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO forge_artifact_cursors (forge_provider, forge_host, repo, kind, number) VALUES (1, 'github.com', 'a/b', $1, 7)`, kind); err == nil {
			t.Fatalf("forge_artifact_cursors kind %d accepted, want CHECK rejection", kind)
		}
	}
}

// ── Test 2: provider CHECK domain IN (1,2,3,4) on both driver tables ──────────

// TestProviderCheckDomain proves providers 1..4 insert cleanly and 0/5 are each
// rejected, on both forge_repo_subscriptions and forge_list_cursors (OQ-D2).
func TestProviderCheckDomain(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	for _, p := range []int{1, 2, 3, 4} {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO forge_repo_subscriptions (forge_provider, forge_host, repo) VALUES ($1, 'h', 'r')`, p); err != nil {
			t.Fatalf("forge_repo_subscriptions provider %d rejected, want accepted: %v", p, err)
		}
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO forge_list_cursors (forge_provider, forge_host, repo, page) VALUES ($1, 'h', 'r', 1)`, p); err != nil {
			t.Fatalf("forge_list_cursors provider %d rejected, want accepted: %v", p, err)
		}
	}
	for _, p := range []int{0, 5} {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO forge_repo_subscriptions (forge_provider, forge_host, repo) VALUES ($1, 'h2', 'r2')`, p); err == nil {
			t.Fatalf("forge_repo_subscriptions provider %d accepted, want CHECK rejection", p)
		}
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO forge_list_cursors (forge_provider, forge_host, repo, page) VALUES ($1, 'h2', 'r2', 1)`, p); err == nil {
			t.Fatalf("forge_list_cursors provider %d accepted, want CHECK rejection", p)
		}
	}
}

// ── Test 3: ForgeListCursor on a never-polled repo → nil, no error ────────────

func TestForgeListCursorNeverPolled(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	got, err := s.ForgeListCursor(ctx, ForgeProviderGitHub, "github.com", "a/b")
	if err != nil {
		t.Fatalf("ForgeListCursor: %v", err)
	}
	if got != nil {
		t.Fatalf("never-polled repo = %v, want nil", got)
	}
}

// ── Test 4: upsert page 1, re-upsert with new ETag → one row, new ETag, ────────
// advanced_at advanced; rows come back ascending by page.

func TestUpsertForgeListCursorPageAdvances(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	base := ForgeListPageCursor{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b", Page: 1, ETag: `"v1"`, HasNext: true}
	if err := s.UpsertForgeListCursorPage(ctx, base); err != nil {
		t.Fatalf("upsert page 1: %v", err)
	}
	firstAdvanced := advancedAt(t, s, 1, "github.com", "a/b", 1)

	// Re-upsert the same page with a new ETag.
	base.ETag = `"v2"`
	base.HasNext = false
	if err := s.UpsertForgeListCursorPage(ctx, base); err != nil {
		t.Fatalf("re-upsert page 1: %v", err)
	}

	got, err := s.ForgeListCursor(ctx, ForgeProviderGitHub, "github.com", "a/b")
	if err != nil {
		t.Fatalf("ForgeListCursor: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("row count = %d, want 1 (upsert, not insert)", len(got))
	}
	if got[0].ETag != `"v2"` {
		t.Fatalf("etag = %q, want %q", got[0].ETag, `"v2"`)
	}
	if got[0].HasNext != false {
		t.Fatalf("has_next = %v, want false", got[0].HasNext)
	}
	secondAdvanced := advancedAt(t, s, 1, "github.com", "a/b", 1)
	if !secondAdvanced.After(firstAdvanced) {
		t.Fatalf("advanced_at not advanced: first=%v second=%v", firstAdvanced, secondAdvanced)
	}

	// Ascending page order across multiple pages.
	for _, p := range []int32{3, 2} {
		if err := s.UpsertForgeListCursorPage(ctx, ForgeListPageCursor{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b", Page: p}); err != nil {
			t.Fatalf("upsert page %d: %v", p, err)
		}
	}
	got, err = s.ForgeListCursor(ctx, ForgeProviderGitHub, "github.com", "a/b")
	if err != nil {
		t.Fatalf("ForgeListCursor: %v", err)
	}
	if len(got) != 3 || got[0].Page != 1 || got[1].Page != 2 || got[2].Page != 3 {
		t.Fatalf("pages = %v, want ascending [1 2 3]", pages(got))
	}
}

// ── Test 5: two hosts / two providers with the same repo do not collide ───────

func TestForgeListCursorCoordinateIsolation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	const repo = "a/b"
	rows := []ForgeListPageCursor{
		{Provider: ForgeProviderGitHub, Host: "github.com", Repo: repo, Page: 1, ETag: `"gh"`},
		{Provider: ForgeProviderGitHub, Host: "ghe.example.com", Repo: repo, Page: 1, ETag: `"ghe"`},
		{Provider: ForgeProviderGitLab, Host: "github.com", Repo: repo, Page: 1, ETag: `"gl"`},
	}
	for _, r := range rows {
		if err := s.UpsertForgeListCursorPage(ctx, r); err != nil {
			t.Fatalf("upsert %+v: %v", r, err)
		}
	}
	// Each coordinate reads back exactly its own single row and ETag.
	for _, r := range rows {
		got, err := s.ForgeListCursor(ctx, r.Provider, r.Host, r.Repo)
		if err != nil {
			t.Fatalf("ForgeListCursor %+v: %v", r, err)
		}
		if len(got) != 1 || got[0].ETag != r.ETag {
			t.Fatalf("coordinate (%d,%q,%q) = %v, want single row etag %q", r.Provider, r.Host, r.Repo, got, r.ETag)
		}
	}
}

// ── Test 6: PruneForgeListCursorPages(maxPage=2) leaves pages 1-2; prune of ────
// a never-polled repo is a no-op.

func TestPruneForgeListCursorPages(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	for _, p := range []int32{1, 2, 3, 4} {
		if err := s.UpsertForgeListCursorPage(ctx, ForgeListPageCursor{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b", Page: p}); err != nil {
			t.Fatalf("upsert page %d: %v", p, err)
		}
	}
	if err := s.PruneForgeListCursorPages(ctx, ForgeProviderGitHub, "github.com", "a/b", 2); err != nil {
		t.Fatalf("prune: %v", err)
	}
	got, err := s.ForgeListCursor(ctx, ForgeProviderGitHub, "github.com", "a/b")
	if err != nil {
		t.Fatalf("ForgeListCursor: %v", err)
	}
	if len(got) != 2 || got[0].Page != 1 || got[1].Page != 2 {
		t.Fatalf("pages after prune = %v, want [1 2]", pages(got))
	}
	// Pruning a never-polled repo is a no-op success.
	if err := s.PruneForgeListCursorPages(ctx, ForgeProviderGitHub, "github.com", "never/polled", 1); err != nil {
		t.Fatalf("prune never-polled: %v", err)
	}
}

// ── Test 7: agent_forge_subscriptions FK shape — unknown agent_account_id ──────
// fails (RESTRICT).

func TestAgentForgeSubscriptionFKRestrict(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.pool.Exec(ctx,
		`INSERT INTO agent_forge_subscriptions (id, agent_account_id, forge_provider, forge_host, repo, kind, number)
		 VALUES ('afs-x', 'no-such-agent', 1, 'github.com', 'a/b', 1, 7)`); err == nil {
		t.Fatal("unknown agent_account_id accepted, want FK RESTRICT rejection")
	}
}

// ── Test 8: repo-subscription CRUD ────────────────────────────────────────────

func TestForgeRepoSubscriptionCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	sub := ForgeRepoSubscription{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b", Enabled: true}

	// ensure-insert creates the row enabled.
	if err := s.EnsureForgeRepoSubscription(ctx, sub); err != nil {
		t.Fatalf("ensure-insert: %v", err)
	}
	// re-ensure of the same coordinate is idempotent (one row).
	if err := s.EnsureForgeRepoSubscription(ctx, sub); err != nil {
		t.Fatalf("re-ensure: %v", err)
	}
	if n := repoSubCount(t, s, 1, "github.com", "a/b"); n != 1 {
		t.Fatalf("row count after re-ensure = %d, want 1 (idempotent)", n)
	}

	// Soft-disable, then ensure-insert over the disabled row leaves it DISABLED
	// (ON CONFLICT DO NOTHING — the bootstrap-only semantic; goes RED against a
	// DO-UPDATE regression).
	if err := s.SetForgeRepoSubscriptionEnabled(ctx, ForgeProviderGitHub, "github.com", "a/b", false); err != nil {
		t.Fatalf("soft-disable: %v", err)
	}
	if err := s.EnsureForgeRepoSubscription(ctx, sub); err != nil { // sub.Enabled == true
		t.Fatalf("re-ensure over disabled: %v", err)
	}
	if enabled := repoSubEnabled(t, s, 1, "github.com", "a/b"); enabled {
		t.Fatal("ensure-insert re-enabled a soft-disabled row, want it left DISABLED (DO NOTHING)")
	}

	// ListEnabled returns only enabled rows for the asked (provider, host),
	// ascending repo, nil for none. Seed a second enabled repo + a foreign host.
	if err := s.EnsureForgeRepoSubscription(ctx, ForgeRepoSubscription{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/a", Enabled: true}); err != nil {
		t.Fatalf("ensure a/a: %v", err)
	}
	if err := s.EnsureForgeRepoSubscription(ctx, ForgeRepoSubscription{Provider: ForgeProviderGitHub, Host: "other.com", Repo: "z/z", Enabled: true}); err != nil {
		t.Fatalf("ensure foreign host: %v", err)
	}
	list, err := s.ListEnabledForgeRepoSubscriptions(ctx, ForgeProviderGitHub, "github.com")
	if err != nil {
		t.Fatalf("list enabled: %v", err)
	}
	// a/b is disabled; only a/a is enabled on github.com.
	if len(list) != 1 || list[0].Repo != "a/a" || !list[0].Enabled {
		t.Fatalf("list enabled = %+v, want [a/a enabled]", list)
	}

	// nil for none: a provider/host with no enabled rows.
	none, err := s.ListEnabledForgeRepoSubscriptions(ctx, ForgeProviderForgejo, "codeberg.org")
	if err != nil {
		t.Fatalf("list enabled none: %v", err)
	}
	if none != nil {
		t.Fatalf("no-enabled-rows = %v, want nil", none)
	}

	// Re-enable a/b and prove updated_at advances on the flip.
	before := repoSubUpdatedAt(t, s, 1, "github.com", "a/b")
	if err := s.SetForgeRepoSubscriptionEnabled(ctx, ForgeProviderGitHub, "github.com", "a/b", true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	after := repoSubUpdatedAt(t, s, 1, "github.com", "a/b")
	if !after.After(before) {
		t.Fatalf("updated_at not advanced on flip: before=%v after=%v", before, after)
	}
	// SetEnabled(false) removes the row from the enabled list WITHOUT deleting it.
	if err := s.SetForgeRepoSubscriptionEnabled(ctx, ForgeProviderGitHub, "github.com", "a/b", false); err != nil {
		t.Fatalf("disable again: %v", err)
	}
	if repoSubCount(t, s, 1, "github.com", "a/b") != 1 {
		t.Fatal("disable deleted the row, want it retained")
	}
	list, err = s.ListEnabledForgeRepoSubscriptions(ctx, ForgeProviderGitHub, "github.com")
	if err != nil {
		t.Fatalf("list enabled after disable: %v", err)
	}
	for _, r := range list {
		if r.Repo == "a/b" {
			t.Fatal("disabled a/b still in enabled list")
		}
	}

	// Unknown coordinate -> ErrNotFound.
	err = s.SetForgeRepoSubscriptionEnabled(ctx, ForgeProviderGitHub, "github.com", "no/such", true)
	sentinelIs(t, err, ErrNotFound, "set enabled on unknown coordinate")
}

// ── Test 9: invalid input → ErrInvalidArgument on every method ────────────────

func TestForgeCursorInvalidArgument(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// ForgeListCursor
	sentinelIs(t, mustErr(func() error { _, e := s.ForgeListCursor(ctx, ForgeProviderUnspecified, "h", "r"); return e }), ErrInvalidArgument, "list cursor zero provider")
	sentinelIs(t, mustErr(func() error { _, e := s.ForgeListCursor(ctx, ForgeProviderGitHub, "", "r"); return e }), ErrInvalidArgument, "list cursor empty host")
	sentinelIs(t, mustErr(func() error { _, e := s.ForgeListCursor(ctx, ForgeProviderGitHub, "h", ""); return e }), ErrInvalidArgument, "list cursor empty repo")

	// UpsertForgeListCursorPage
	sentinelIs(t, s.UpsertForgeListCursorPage(ctx, ForgeListPageCursor{Provider: 0, Host: "h", Repo: "r", Page: 1}), ErrInvalidArgument, "upsert zero provider")
	sentinelIs(t, s.UpsertForgeListCursorPage(ctx, ForgeListPageCursor{Provider: ForgeProviderGitHub, Host: "", Repo: "r", Page: 1}), ErrInvalidArgument, "upsert empty host")
	sentinelIs(t, s.UpsertForgeListCursorPage(ctx, ForgeListPageCursor{Provider: ForgeProviderGitHub, Host: "h", Repo: "", Page: 1}), ErrInvalidArgument, "upsert empty repo")
	sentinelIs(t, s.UpsertForgeListCursorPage(ctx, ForgeListPageCursor{Provider: ForgeProviderGitHub, Host: "h", Repo: "r", Page: 0}), ErrInvalidArgument, "upsert page 0")

	// PruneForgeListCursorPages
	sentinelIs(t, s.PruneForgeListCursorPages(ctx, 0, "h", "r", 1), ErrInvalidArgument, "prune zero provider")
	sentinelIs(t, s.PruneForgeListCursorPages(ctx, ForgeProviderGitHub, "", "r", 1), ErrInvalidArgument, "prune empty host")
	sentinelIs(t, s.PruneForgeListCursorPages(ctx, ForgeProviderGitHub, "h", "", 1), ErrInvalidArgument, "prune empty repo")
	sentinelIs(t, s.PruneForgeListCursorPages(ctx, ForgeProviderGitHub, "h", "r", 0), ErrInvalidArgument, "prune maxPage 0")

	// EnsureForgeRepoSubscription
	sentinelIs(t, s.EnsureForgeRepoSubscription(ctx, ForgeRepoSubscription{Provider: 0, Host: "h", Repo: "r"}), ErrInvalidArgument, "ensure zero provider")
	sentinelIs(t, s.EnsureForgeRepoSubscription(ctx, ForgeRepoSubscription{Provider: ForgeProviderGitHub, Host: "", Repo: "r"}), ErrInvalidArgument, "ensure empty host")
	sentinelIs(t, s.EnsureForgeRepoSubscription(ctx, ForgeRepoSubscription{Provider: ForgeProviderGitHub, Host: "h", Repo: ""}), ErrInvalidArgument, "ensure empty repo")

	// ListEnabledForgeRepoSubscriptions
	sentinelIs(t, mustErr(func() error { _, e := s.ListEnabledForgeRepoSubscriptions(ctx, 0, "h"); return e }), ErrInvalidArgument, "list enabled zero provider")
	sentinelIs(t, mustErr(func() error { _, e := s.ListEnabledForgeRepoSubscriptions(ctx, ForgeProviderGitHub, ""); return e }), ErrInvalidArgument, "list enabled empty host")

	// SetForgeRepoSubscriptionEnabled
	sentinelIs(t, s.SetForgeRepoSubscriptionEnabled(ctx, 0, "h", "r", true), ErrInvalidArgument, "set zero provider")
	sentinelIs(t, s.SetForgeRepoSubscriptionEnabled(ctx, ForgeProviderGitHub, "", "r", true), ErrInvalidArgument, "set empty host")
	sentinelIs(t, s.SetForgeRepoSubscriptionEnabled(ctx, ForgeProviderGitHub, "h", "", true), ErrInvalidArgument, "set empty repo")
}

// ── helpers ───────────────────────────────────────────────────────────────────

func mustExec(t *testing.T, s *Store, sql string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func mustErr(f func() error) error { return f() }

func pages(cs []ForgeListPageCursor) []int32 {
	out := make([]int32, len(cs))
	for i, c := range cs {
		out[i] = c.Page
	}
	return out
}

func advancedAt(t *testing.T, s *Store, provider int, host, repo string, page int) (ts time.Time) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(),
		`SELECT advanced_at FROM forge_list_cursors WHERE forge_provider=$1 AND forge_host=$2 AND repo=$3 AND page=$4`,
		provider, host, repo, page).Scan(&ts); err != nil {
		t.Fatalf("read advanced_at: %v", err)
	}
	return ts
}

func repoSubCount(t *testing.T, s *Store, provider int, host, repo string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM forge_repo_subscriptions WHERE forge_provider=$1 AND forge_host=$2 AND repo=$3`,
		provider, host, repo).Scan(&n); err != nil {
		t.Fatalf("count repo subs: %v", err)
	}
	return n
}

func repoSubEnabled(t *testing.T, s *Store, provider int, host, repo string) bool {
	t.Helper()
	var enabled bool
	if err := s.pool.QueryRow(context.Background(),
		`SELECT enabled FROM forge_repo_subscriptions WHERE forge_provider=$1 AND forge_host=$2 AND repo=$3`,
		provider, host, repo).Scan(&enabled); err != nil {
		t.Fatalf("read enabled: %v", err)
	}
	return enabled
}

func repoSubUpdatedAt(t *testing.T, s *Store, provider int, host, repo string) (ts time.Time) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(),
		`SELECT updated_at FROM forge_repo_subscriptions WHERE forge_provider=$1 AND forge_host=$2 AND repo=$3`,
		provider, host, repo).Scan(&ts); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	return ts
}
