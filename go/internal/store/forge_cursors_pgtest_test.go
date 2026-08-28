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
// each. newTestStore applies 0001..0016 in order on a fresh database, so 0016
// is exercised landing on top of the full 0001..0015 schema.
func TestMigration0016TablesExist(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// forge_repo_subscriptions — including the RIG-2883 T4 columns
	// (swept_updated_at watermark + list_etag), proving they applied.
	mustExec(t, s, `INSERT INTO forge_repo_subscriptions (forge_provider, forge_host, repo, swept_updated_at, list_etag) VALUES (1, 'github.com', 'a/b', now(), '"e"')`)
	// forge_list_cursors
	mustExec(t, s, `INSERT INTO forge_list_cursors (forge_provider, forge_host, repo, page) VALUES (1, 'github.com', 'a/b', 1)`)
	// forge_artifact_cursors — both legal kind values (1=issue, 2=pull_request)
	// accept, proving the CHECK IN (1,2) upper bound, symmetric to the
	// provider-domain 1..4 accept test.
	mustExec(t, s, `INSERT INTO forge_artifact_cursors (forge_provider, forge_host, repo, kind, number) VALUES (1, 'github.com', 'a/b', 1, 7)`)
	mustExec(t, s, `INSERT INTO forge_artifact_cursors (forge_provider, forge_host, repo, kind, number) VALUES (1, 'github.com', 'a/b', 2, 8)`)
	// issues — the RIG-2883 T4 forge_updated_at column is present (INERT this
	// slice; T4a threads its write path). Proven present by inserting it.
	mustExec(t, s, `INSERT INTO issues (id, forge_provider, forge_host, repo, number, forge_updated_at) VALUES ('i-1', 1, 'github.com', 'a/b', 7, now())`)
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
	// ascending repo, nil for none. Seed two enabled github.com repos INSERTED
	// out of lexical order (a/c before a/a) plus a foreign host, so the returned
	// slice being ascending proves the ORDER BY repo ASC clause (a dropped or
	// DESC order goes RED here, not silently green on a single row).
	if err := s.EnsureForgeRepoSubscription(ctx, ForgeRepoSubscription{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/c", Enabled: true}); err != nil {
		t.Fatalf("ensure a/c: %v", err)
	}
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
	// a/b is disabled; a/a and a/c are enabled on github.com, returned ascending.
	gotRepos := make([]string, len(list))
	for i, r := range list {
		gotRepos[i] = r.Repo
		if !r.Enabled {
			t.Fatalf("list enabled contains a disabled row: %+v", r)
		}
	}
	if len(list) != 2 || gotRepos[0] != "a/a" || gotRepos[1] != "a/c" {
		t.Fatalf("list enabled = %v, want [a/a a/c] ascending", gotRepos)
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

// ── Test 8: forge_repo_subscriptions watermark round-trip (RIG-2883 T4) ───────

// TestForgeRepoWatermarkRoundTrip proves the swept_updated_at + list_etag
// watermark persists: a never-swept row reads zero/empty, an unknown coordinate
// also reads zero/empty (not an error — the reconciler treats "no row" and
// "never swept" identically), a store then reads back, and a store on an unknown
// coordinate is ErrNotFound (the subscription must exist first).
func TestForgeRepoWatermarkRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	sub := ForgeRepoSubscription{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b", Enabled: true}
	if err := s.EnsureForgeRepoSubscription(ctx, sub); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// Never-swept row: zero time, empty etag.
	mark, etag, err := s.LoadForgeRepoWatermark(ctx, ForgeProviderGitHub, "github.com", "a/b")
	if err != nil {
		t.Fatalf("load never-swept: %v", err)
	}
	if !mark.IsZero() || etag != "" {
		t.Fatalf("never-swept = (%v, %q), want (zero, \"\")", mark, etag)
	}

	// Unknown coordinate (no row) also reads zero/empty, not an error.
	mark, etag, err = s.LoadForgeRepoWatermark(ctx, ForgeProviderGitLab, "gitlab.com", "x/y")
	if err != nil {
		t.Fatalf("load unknown coordinate: %v", err)
	}
	if !mark.IsZero() || etag != "" {
		t.Fatalf("unknown coordinate = (%v, %q), want (zero, \"\")", mark, etag)
	}

	// Store then round-trip.
	want := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	if err := s.StoreForgeRepoWatermark(ctx, ForgeProviderGitHub, "github.com", "a/b", want, `"v1"`); err != nil {
		t.Fatalf("store watermark: %v", err)
	}
	mark, etag, err = s.LoadForgeRepoWatermark(ctx, ForgeProviderGitHub, "github.com", "a/b")
	if err != nil {
		t.Fatalf("load after store: %v", err)
	}
	if !mark.Equal(want) {
		t.Fatalf("watermark = %v, want %v", mark, want)
	}
	if etag != `"v1"` {
		t.Fatalf("etag = %q, want %q", etag, `"v1"`)
	}

	// Storing on an unknown coordinate -> ErrNotFound (the subscription must exist).
	err = s.StoreForgeRepoWatermark(ctx, ForgeProviderGitHub, "github.com", "no/such", want, "")
	sentinelIs(t, err, ErrNotFound, "store watermark on unknown coordinate")
}

// ── Test 9: watermark coordinate isolation across (provider, host) ────────────

func TestForgeRepoWatermarkCoordinateIsolation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	const repo = "a/b"
	subs := []struct {
		provider ForgeProvider
		host     string
		mark     time.Time
		etag     string
	}{
		{ForgeProviderGitHub, "github.com", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), `"gh"`},
		{ForgeProviderGitHub, "ghe.example.com", time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC), `"ghe"`},
		{ForgeProviderGitLab, "github.com", time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC), `"gl"`},
	}
	for _, c := range subs {
		if err := s.EnsureForgeRepoSubscription(ctx, ForgeRepoSubscription{Provider: c.provider, Host: c.host, Repo: repo, Enabled: true}); err != nil {
			t.Fatalf("ensure (%d,%q): %v", c.provider, c.host, err)
		}
		if err := s.StoreForgeRepoWatermark(ctx, c.provider, c.host, repo, c.mark, c.etag); err != nil {
			t.Fatalf("store (%d,%q): %v", c.provider, c.host, err)
		}
	}
	// Each coordinate reads back exactly its own watermark and etag.
	for _, c := range subs {
		mark, etag, err := s.LoadForgeRepoWatermark(ctx, c.provider, c.host, repo)
		if err != nil {
			t.Fatalf("load (%d,%q): %v", c.provider, c.host, err)
		}
		if !mark.Equal(c.mark) || etag != c.etag {
			t.Fatalf("coordinate (%d,%q) = (%v, %q), want (%v, %q)", c.provider, c.host, mark, etag, c.mark, c.etag)
		}
	}
}

// ── Test 10: enabled-repo enumeration + point membership (RIG-2883 T4) ────────

func TestListAndIsEnabledForgeRepos(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// No rows -> nil, and the point check is false.
	repos, err := s.ListEnabledForgeRepos(ctx)
	if err != nil {
		t.Fatalf("list enabled repos empty: %v", err)
	}
	if repos != nil {
		t.Fatalf("empty = %v, want nil", repos)
	}
	ok, err := s.IsEnabledForgeRepo(ctx, "a/b")
	if err != nil {
		t.Fatalf("is-enabled empty: %v", err)
	}
	if ok {
		t.Fatal("is-enabled = true on empty, want false")
	}

	// Seed enabled repos out of lexical order across coordinates, plus one
	// disabled — enumeration is ascending and excludes the disabled repo.
	seed := []ForgeRepoSubscription{
		{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "z/z", Enabled: true},
		{Provider: ForgeProviderGitLab, Host: "gitlab.com", Repo: "a/a", Enabled: true},
		{Provider: ForgeProviderGitHub, Host: "ghe.example.com", Repo: "m/m", Enabled: true},
	}
	for _, sub := range seed {
		if err := s.EnsureForgeRepoSubscription(ctx, sub); err != nil {
			t.Fatalf("ensure %+v: %v", sub, err)
		}
	}
	if err := s.EnsureForgeRepoSubscription(ctx, ForgeRepoSubscription{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "d/d", Enabled: false}); err != nil {
		t.Fatalf("ensure disabled: %v", err)
	}

	repos, err = s.ListEnabledForgeRepos(ctx)
	if err != nil {
		t.Fatalf("list enabled repos: %v", err)
	}
	if len(repos) != 3 || repos[0] != "a/a" || repos[1] != "m/m" || repos[2] != "z/z" {
		t.Fatalf("list enabled repos = %v, want [a/a m/m z/z] ascending", repos)
	}

	// Point check: an enabled repo is true, the disabled repo is false.
	ok, err = s.IsEnabledForgeRepo(ctx, "m/m")
	if err != nil {
		t.Fatalf("is-enabled m/m: %v", err)
	}
	if !ok {
		t.Fatal("is-enabled m/m = false, want true")
	}
	ok, err = s.IsEnabledForgeRepo(ctx, "d/d")
	if err != nil {
		t.Fatalf("is-enabled d/d: %v", err)
	}
	if ok {
		t.Fatal("is-enabled d/d (disabled) = true, want false")
	}
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
