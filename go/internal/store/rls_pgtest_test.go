//go:build pgtest

// Row-Level-Security tenant-isolation contracts (RIG-3106 / T2 of RIG-2861).
// These prove the enforcement half of managed multi-tenancy end to end against
// real Postgres: the request path (compass_app role + per-tx compass.tenant_id
// GUC) reads and writes ONLY its own tenant's rows; a connection with no GUC
// fails CLOSED (zero rows, no SQLSTATE escape); a transaction-mode pooler reuse
// carries no leftover scope; FORCE ROW LEVEL SECURITY is in effect for the
// table owner; and the forge-board + N7 handle keys carry tenant_id so two
// tenants never collide on a shared forge coordinate or a shared handle.
//
// An in-memory substitute could not prove any of this — RLS is a database
// property — so these run only under the pgtest build tag against the harness's
// real Postgres.

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// seedTenant inserts a fresh, non-bootstrap tenant row (bucket A: tenants is
// RLS-exempt, so the owner-pool insert is fine) and returns its id. A second
// tenant is the whole point of these tests — one to prove isolation against.
func seedTenant(t *testing.T, s *Store, slug string) TenantID {
	t.Helper()
	id := newID()
	if _, err := s.pool.Exec(context.Background(),
		"INSERT INTO tenants (id, slug, display_name, created_at_unix_ms) VALUES ($1, $2, $3, $4)",
		id, slug, slug, time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("seed tenant %q: %v", slug, err)
	}
	return TenantID(id)
}

// seedChannelWithMessage creates a user, a channel, and one posted message, all
// under tenant tenantCtx. It returns the channel and the message id — the two
// handles a cross-tenant read test asserts against. Every store call routes
// through the tenant-scoping path (compass_app + GUC), so the rows land stamped
// with the context tenant exactly as a real request would.
func seedChannelWithMessage(t *testing.T, s *Store, tenantCtx context.Context, handle, body string) (ChannelID, MessageID, AccountID) {
	t.Helper()
	owner, err := s.CreateUser(tenantCtx, NewUser{Handle: handle, DisplayName: handle})
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", handle, err)
	}
	ch, err := s.CreateChannel(tenantCtx, owner.ID, NewChannel{
		Name:             handle + "-room",
		Kind:             ChannelKindChannel,
		MemberAccountIDs: []AccountID{owner.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel(%s): %v", handle, err)
	}
	msg, _, err := s.AppendMessage(tenantCtx,
		Message{AuthorAccountID: owner.ID, Blocks: []MessageBlock{textBlock(body)}},
		string(ch.ID), TopicRef{Name: "general", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage(%s): %v", handle, err)
	}
	return ch.ID, msg.ID, owner.ID
}

// TestCrossTenantReadReturnsZeroRows proves case 1: tenant B, running the
// request path, cannot see tenant A's channel or messages — the isolation is
// the RLS policy, not an application-layer WHERE. Both tenants exist in the same
// physical tables; only the per-tx GUC differs between the two contexts.
func TestCrossTenantReadReturnsZeroRows(t *testing.T) {
	s := newTestStore(t)
	tenantB := seedTenant(t, s, "tenant-b")
	ctxA := context.Background() // no tenant set → bootstrap tenant (tenant A)
	ctxB := WithTenant(context.Background(), tenantB)

	chA, msgA, ownerA := seedChannelWithMessage(t, s, ctxA, "alice", "secret to A")

	// Tenant B lists channels visible to A's owner id: RLS filters channels to
	// B's tenant, so A's channel is invisible regardless of the visibility
	// predicate — the row simply is not in B's view.
	bChannels, err := s.ListChannels(ctxB, ownerA)
	if err != nil {
		t.Fatalf("ListChannels(B): %v", err)
	}
	for _, c := range bChannels {
		if c.ID == chA {
			t.Fatalf("tenant B saw tenant A's channel %q — cross-tenant read leak", chA)
		}
	}

	// Tenant B reads A's channel messages directly by id: the channel row is
	// invisible under B's GUC, so the message list is empty (or errors closed),
	// never A's message.
	bMsgs, err := s.ListMessages(ctxB, ListMessagesQuery{Actor: ownerA, ChannelID: chA})
	if err == nil {
		for _, m := range bMsgs {
			if m.ID == msgA {
				t.Fatalf("tenant B read tenant A's message %q — cross-tenant read leak", msgA)
			}
		}
	}

	// Control: tenant A DOES see its own channel — proving the policy is not
	// just globally hiding everything.
	aChannels, err := s.ListChannels(ctxA, ownerA)
	if err != nil {
		t.Fatalf("ListChannels(A): %v", err)
	}
	if !channelsContain(aChannels, chA) {
		t.Fatalf("tenant A cannot see its OWN channel %q — policy over-blocks", chA)
	}
}

// TestCrossTenantWriteLandsUnderWriterTenant proves case 2: a write issued under
// tenant B's context is stamped with B, never A — even when it reuses an id/name
// that also exists under A. The WITH CHECK arm plus the tenant_id GUC DEFAULT
// make a request-path INSERT land in the acting tenant, so a compromised or
// confused caller cannot write into a foreign tenant.
func TestCrossTenantWriteLandsUnderWriterTenant(t *testing.T) {
	s := newTestStore(t)
	tenantB := seedTenant(t, s, "tenant-b")
	ctxA := context.Background()
	ctxB := WithTenant(context.Background(), tenantB)

	// Same handle "matt" created under each tenant: N7 allows it, and the write
	// must be stamped with the acting tenant each time.
	uA, err := s.CreateUser(ctxA, NewUser{Handle: "matt", DisplayName: "Matt A"})
	if err != nil {
		t.Fatalf("CreateUser(A): %v", err)
	}
	uB, err := s.CreateUser(ctxB, NewUser{Handle: "matt", DisplayName: "Matt B"})
	if err != nil {
		t.Fatalf("CreateUser(B): %v", err)
	}
	if uA.ID == uB.ID {
		t.Fatalf("two tenants minted the same account id %q", uA.ID)
	}
	if got := tenantOf(t, s, uA.ID); got != string(s.bootstrapTenantID) {
		t.Fatalf("tenant A's user stamped %q, want bootstrap %q", got, s.bootstrapTenantID)
	}
	if got := tenantOf(t, s, uB.ID); got != string(tenantB) {
		t.Fatalf("tenant B's user stamped %q, want %q", got, tenantB)
	}
}

// TestUnsetGUCFailsClosed proves case 3: a request-path (compass_app) query on a
// connection whose compass.tenant_id GUC was never set returns ZERO rows, never
// all rows and never a SQLSTATE 42704 (unrecognized GUC) escape. This is the
// policy's non-empty guard + missing_ok=true current_setting read. It is probed
// at the raw connection level because the store API always sets a GUC (it
// resolves to the bootstrap tenant), so the unset case only exists below it.
func TestUnsetGUCFailsClosed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Seed one row as the bootstrap tenant so "all rows" is distinguishable
	// from "zero rows".
	if _, err := s.CreateUser(ctx, NewUser{Handle: "seed", DisplayName: "Seed"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	// Enter the request role WITHOUT setting the tenant GUC. A session-scoped
	// SET ROLE is fine here — we release (and reset) the conn after.
	if _, err := conn.Exec(ctx, "SET ROLE compass_app"); err != nil {
		t.Fatalf("SET ROLE compass_app: %v", err)
	}
	var count int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM accounts").Scan(&count); err != nil {
		// A clean zero is the contract; a real error (e.g. 42704) is a FAIL —
		// the policy must fail closed, not throw.
		t.Fatalf("unset-GUC count must succeed with zero, got error: %v", err)
	}
	if count != 0 {
		t.Fatalf("unset-GUC read saw %d rows, want 0 (fail-closed) — RLS not enforced", count)
	}
	// Reset the session role before the pooled conn goes back.
	if _, err := conn.Exec(ctx, "RESET ROLE"); err != nil {
		t.Fatalf("RESET ROLE: %v", err)
	}
}

// TestPooledConnReuseCarriesNoScope proves case 4: a physical connection scoped
// to tenant A inside one transaction, then reused for a second transaction that
// sets no scope, does NOT see A's rows — the SET LOCAL is transaction-scoped, so
// nothing leaks across the checkout boundary. A real transaction-mode PgBouncer
// was not available in the harness (the pgtest harness hands back a direct pgx
// pool DSN, no bouncer); this simulates the pooler's connection-reuse hazard by
// pinning ONE physical *pgxpool.Conn and running two sequential transactions on
// it with A-scope then no-scope, which is exactly the reuse a transaction-mode
// pooler creates.
func TestPooledConnReuseCarriesNoScope(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// One row under the bootstrap tenant (tenant A).
	if _, err := s.CreateUser(ctx, NewUser{Handle: "poolseed", DisplayName: "Pool Seed"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	tenantA := string(s.bootstrapTenantID)

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	// Txn 1 on this physical conn: scope to A, confirm A's row is visible.
	tx1, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	if _, err := tx1.Exec(ctx, "SET LOCAL ROLE compass_app"); err != nil {
		t.Fatalf("tx1 set role: %v", err)
	}
	if _, err := tx1.Exec(ctx, "SELECT set_config('compass.tenant_id', $1, true)", tenantA); err != nil {
		t.Fatalf("tx1 set_config: %v", err)
	}
	var scoped int
	if err := tx1.QueryRow(ctx, "SELECT count(*) FROM accounts").Scan(&scoped); err != nil {
		t.Fatalf("tx1 count: %v", err)
	}
	if scoped == 0 {
		t.Fatalf("tx1 scoped to A saw 0 rows, want A's seeded row — scope not applied")
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit tx1: %v", err)
	}

	// Txn 2 on the SAME physical conn: enter the app role but set NO tenant GUC
	// — the transaction-mode-pooler reuse hazard. If SET LOCAL leaked from tx1,
	// this would still see A's rows. It must not.
	tx2, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	if _, err := tx2.Exec(ctx, "SET LOCAL ROLE compass_app"); err != nil {
		t.Fatalf("tx2 set role: %v", err)
	}
	var leaked int
	if err := tx2.QueryRow(ctx, "SELECT count(*) FROM accounts").Scan(&leaked); err != nil {
		t.Fatalf("tx2 count must fail closed (zero), got error: %v", err)
	}
	if leaked != 0 {
		t.Fatalf("tx2 (reused conn, no scope) saw %d rows, want 0 — SET LOCAL leaked across checkout", leaked)
	}
	if err := tx2.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx2: %v", err)
	}
}

// TestNonOwnerRoleScopedByRLS proves case 5: a request-path query running as the
// non-owner compass_app role (entered via SET LOCAL ROLE from the owner
// connection, exactly as the store's tenant-tx path does) is constrained by RLS
// to a single tenant — it sees exactly the scoped tenant's rows, never another
// tenant's. This is the enforcement the request path actually relies on: every
// request statement drops to compass_app, which has neither owner-bypass nor
// BYPASSRLS, so ENABLE ROW LEVEL SECURITY binds it.
//
// It deliberately does NOT try to prove FORCE: the pgtest harness (like
// production) connects as a SUPERUSER owner, and a superuser bypasses even FORCE
// ROW LEVEL SECURITY, so FORCE is unobservable through any query on this
// connection. The FORCE flag — defense-in-depth for a future non-superuser owner
// (design.md:703-708) — is asserted deterministically from the catalog in
// TestRLSCatalogEnabledAndForced.
func TestNonOwnerRoleScopedByRLS(t *testing.T) {
	s := newTestStore(t)
	tenantB := seedTenant(t, s, "tenant-b")
	ctxA := context.Background()
	ctxB := WithTenant(context.Background(), tenantB)

	// One user per tenant.
	if _, err := s.CreateUser(ctxA, NewUser{Handle: "ownera", DisplayName: "Owner A"}); err != nil {
		t.Fatalf("CreateUser(A): %v", err)
	}
	if _, err := s.CreateUser(ctxB, NewUser{Handle: "ownerb", DisplayName: "Owner B"}); err != nil {
		t.Fatalf("CreateUser(B): %v", err)
	}

	conn, err := s.pool.Acquire(ctx0())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	ctx := ctx0()

	// The pooled conn is the OWNER (the migrating role). Enter a transaction,
	// SET LOCAL ROLE compass_app, scope to B, and count: FORCE means the owner's
	// bypass is irrelevant once it has assumed the app role, so it sees exactly
	// B's one row, never A's.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE compass_app"); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('compass.tenant_id', $1, true)", string(tenantB)); err != nil {
		t.Fatalf("set_config: %v", err)
	}
	var bCount int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM user_accounts").Scan(&bCount); err != nil {
		t.Fatalf("scoped count: %v", err)
	}
	if bCount != 1 {
		t.Fatalf("owner-as-app scoped to B saw %d user rows, want exactly 1 — FORCE RLS not in effect (owner bypassing)", bCount)
	}
}

// TestForgeBoardTwoTenantsSameCoordinate proves case 6: two tenants watching the
// SAME forge coordinate (provider/host/repo/number) each get their OWN issues
// row with no unique-key collision — because tenant_id is now IN the coordinate
// key (issues_coordinate_key). Each tenant reads back only its own row.
func TestForgeBoardTwoTenantsSameCoordinate(t *testing.T) {
	s := newTestStore(t)
	tenantB := seedTenant(t, s, "tenant-b")
	ctxA := context.Background()
	ctxB := WithTenant(context.Background(), tenantB)

	coord := IssueForgeFields{
		ForgeProvider: ForgeProviderGitHub,
		ForgeHost:     "github.com",
		Repo:          "acme/widgets",
		Number:        42,
		Title:         "shared coordinate",
		ForgeState:    "open",
		ForgeAccount:  "octocat",
		URL:           "https://github.com/acme/widgets/issues/42",
	}
	aCoord := coord
	aCoord.Title = "A's view of #42"
	bCoord := coord
	bCoord.Title = "B's view of #42"

	idA, err := s.UpsertIssueForgeFields(ctxA, aCoord)
	if err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	idB, err := s.UpsertIssueForgeFields(ctxB, bCoord)
	if err != nil {
		t.Fatalf("upsert B (same coordinate, different tenant) must NOT collide: %v", err)
	}
	if idA == idB {
		t.Fatalf("two tenants collided on one issues row id %q — tenant_id not in coordinate key", idA)
	}

	// Each tenant reads back only its own row for that coordinate.
	gotA, err := s.GetIssue(ctxA, idA)
	if err != nil {
		t.Fatalf("GetIssue(A): %v", err)
	}
	if gotA.Title != "A's view of #42" {
		t.Fatalf("tenant A read title %q, want its own", gotA.Title)
	}
	// Tenant A cannot read B's row id (RLS hides it).
	if _, err := s.GetIssue(ctxA, idB); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant A reading B's issue id got err=%v, want ErrNotFound (cross-tenant hidden)", err)
	}
	gotB, err := s.GetIssue(ctxB, idB)
	if err != nil {
		t.Fatalf("GetIssue(B): %v", err)
	}
	if gotB.Title != "B's view of #42" {
		t.Fatalf("tenant B read title %q, want its own", gotB.Title)
	}
}

// TestN7HandleAcrossTwoTenants proves case 7: @matt can exist under two tenants
// with no collision (the org-scoped partial-unique index carries tenant_id), and
// a handle resolver returns only the caller's-tenant account. Both the global
// tier (a human @matt) and the resolver's tenant-scoping (via RLS on
// account_handles) are exercised.
func TestN7HandleAcrossTwoTenants(t *testing.T) {
	s := newTestStore(t)
	tenantB := seedTenant(t, s, "tenant-b")
	ctxA := context.Background()
	ctxB := WithTenant(context.Background(), tenantB)

	uA, err := s.CreateUser(ctxA, NewUser{Handle: "matt", DisplayName: "Matt A"})
	if err != nil {
		t.Fatalf("CreateUser @matt (A): %v", err)
	}
	uB, err := s.CreateUser(ctxB, NewUser{Handle: "matt", DisplayName: "Matt B"})
	if err != nil {
		t.Fatalf("CreateUser @matt (B) must not collide across tenants: %v", err)
	}

	// The resolver returns the caller's-tenant @matt, not the other tenant's.
	resA, err := s.UserByHandle(ctxA, "matt")
	if err != nil {
		t.Fatalf("UserByHandle(A): %v", err)
	}
	if resA.ID != uA.ID {
		t.Fatalf("resolver under A returned %q, want A's @matt %q", resA.ID, uA.ID)
	}
	resB, err := s.UserByHandle(ctxB, "matt")
	if err != nil {
		t.Fatalf("UserByHandle(B): %v", err)
	}
	if resB.ID != uB.ID {
		t.Fatalf("resolver under B returned %q, want B's @matt %q", resB.ID, uB.ID)
	}
	if resA.ID == resB.ID {
		t.Fatalf("resolver returned the same account for both tenants — N7 not tenant-scoped")
	}
}

// ctx0 is a readability wrapper for a fresh background context in the owner-probe
// test, which uses several.
func ctx0() context.Context { return context.Background() }

// channelsContain reports whether chs includes want.
func channelsContain(chs []Channel, want ChannelID) bool {
	for _, c := range chs {
		if c.ID == want {
			return true
		}
	}
	return false
}

// TestRLSCatalogEnabledAndForced proves the migration left RLS both ENABLED and
// FORCED on every one of the 26 tenant-owned tables, and left the three
// infrastructure tables (bucket A) exempt. It reads the pg_class catalog flags
// directly rather than probing behavior because the harness connects as a
// superuser (which bypasses even FORCE), so FORCE cannot be proven by a query
// returning zero rows — only by the flag. This is the test that fails if a
// future edit drops FORCE, ENABLE, or a table from 0002_rls.sql: TestCrossTenant*
// prove the non-owner request path is scoped (which ENABLE alone gives), but only
// this catalog check defends the owner-binding FORCE guarantee the design
// requires, and the bucket-A exemption auth depends on.
func TestRLSCatalogEnabledAndForced(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// The 26 tenant-owned tables the migration's policy loop covers — kept in
	// sync with 0002_rls.sql's tenant_tables array. A table missing here (or
	// there) surfaces as a catalog-flag mismatch.
	tenantOwned := []string{
		"accounts",
		"user_accounts", "agent_accounts", "system_accounts", "account_handles",
		"channel_groups", "channels", "channel_members", "agent_workspaces",
		"topics", "messages", "channel_pins", "secrets",
		"agent_sessions", "agent_placements",
		"agent_session_transcript_entries", "agent_session_archive_segments",
		"agent_delivery_cursors", "owed_mentions", "agent_activity",
		"agent_forge_subscriptions", "forge_authored_artifacts",
		"linear_agent_sessions",
		"issues", "forge_repo_subscriptions", "forge_artifact_cursors",
	}
	for _, tbl := range tenantOwned {
		var enabled, forced bool
		if err := s.pool.QueryRow(ctx,
			`SELECT relrowsecurity, relforcerowsecurity
			   FROM pg_class
			  WHERE oid = format('%I.%I', current_schema(), $1::text)::regclass`,
			tbl,
		).Scan(&enabled, &forced); err != nil {
			t.Fatalf("read RLS flags for %q: %v", tbl, err)
		}
		if !enabled {
			t.Errorf("%s: ENABLE ROW LEVEL SECURITY missing (relrowsecurity=false)", tbl)
		}
		if !forced {
			t.Errorf("%s: FORCE ROW LEVEL SECURITY missing (relforcerowsecurity=false) — a non-superuser owner would bypass RLS", tbl)
		}
	}

	// Bucket A (infrastructure) must stay RLS-exempt: gating tokens breaks auth
	// (token resolution establishes the tenant before any GUC exists), and gating
	// tenants / agent_config_bundle breaks bootstrap and the fleet-config
	// singleton. An accidental ENABLE here is a fail-closed outage, not a leak —
	// so it is worth catching deterministically too.
	for _, tbl := range []string{"tenants", "tokens", "agent_config_bundle"} {
		var enabled bool
		if err := s.pool.QueryRow(ctx,
			`SELECT relrowsecurity
			   FROM pg_class
			  WHERE oid = format('%I.%I', current_schema(), $1::text)::regclass`,
			tbl,
		).Scan(&enabled); err != nil {
			t.Fatalf("read RLS flag for %q: %v", tbl, err)
		}
		if enabled {
			t.Errorf("%s: infrastructure table must NOT have RLS enabled (bucket A)", tbl)
		}
	}
}
