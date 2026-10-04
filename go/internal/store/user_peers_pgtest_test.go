//go:build pgtest

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RigelBuild/compass/go/internal/pgtest"
)

func TestApprovePeerIdempotentAndListStates(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := mustUser(t, s, "peer-a")
	b := mustUser(t, s, "peer-b")

	inserted, err := s.ApprovePeer(ctx, a.ID, b.ID)
	if err != nil || !inserted {
		t.Fatalf("ApprovePeer(a, b) = (%v, %v), want (true, nil)", inserted, err)
	}
	inserted, err = s.ApprovePeer(ctx, a.ID, b.ID)
	if err != nil || inserted {
		t.Fatalf("second ApprovePeer(a, b) = (%v, %v), want (false, nil)", inserted, err)
	}

	assertPeerings := func(user AccountID, wantID AccountID, wantHandle string, wantState PeeringState) {
		t.Helper()
		peers, err := s.ListPeerings(ctx, user)
		if err != nil {
			t.Fatalf("ListPeerings(%q): %v", user, err)
		}
		if len(peers) != 1 || peers[0].PeerID != wantID || peers[0].Handle != wantHandle || peers[0].State != wantState {
			t.Fatalf("ListPeerings(%q) = %+v, want one %q/%q state %v", user, peers, wantID, wantHandle, wantState)
		}
	}
	assertPeerings(a.ID, b.ID, b.Handle, PeeringPendingOutgoing)
	assertPeerings(b.ID, a.ID, a.Handle, PeeringPendingIncoming)

	inserted, err = s.ApprovePeer(ctx, b.ID, a.ID)
	if err != nil || !inserted {
		t.Fatalf("ApprovePeer(b, a) = (%v, %v), want (true, nil)", inserted, err)
	}
	assertPeerings(a.ID, b.ID, b.Handle, PeeringApproved)
	assertPeerings(b.ID, a.ID, a.Handle, PeeringApproved)
}

func TestRevokePeerTransitions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := mustUser(t, s, "revoke-a")
	b := mustUser(t, s, "revoke-b")
	for _, edge := range [][2]AccountID{{a.ID, b.ID}, {b.ID, a.ID}} {
		if _, err := s.ApprovePeer(ctx, edge[0], edge[1]); err != nil {
			t.Fatalf("ApprovePeer(%q, %q): %v", edge[0], edge[1], err)
		}
	}

	deleted, err := s.RevokePeer(ctx, a.ID, b.ID)
	if err != nil || !deleted {
		t.Fatalf("RevokePeer(a, b) = (%v, %v), want (true, nil)", deleted, err)
	}
	for user, wantState := range map[AccountID]PeeringState{
		a.ID: PeeringPendingIncoming,
		b.ID: PeeringPendingOutgoing,
	} {
		peers, err := s.ListPeerings(ctx, user)
		if err != nil {
			t.Fatalf("ListPeerings(%q): %v", user, err)
		}
		if len(peers) != 1 || peers[0].State != wantState {
			t.Errorf("ListPeerings(%q) = %+v, want one state %v", user, peers, wantState)
		}
	}
	deleted, err = s.RevokePeer(ctx, a.ID, b.ID)
	if err != nil || deleted {
		t.Fatalf("second RevokePeer(a, b) = (%v, %v), want (false, nil)", deleted, err)
	}
}

func TestApprovePeerRejectsSelfAndInvalidPeer(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	user := mustUser(t, s, "invalid-peer-user")
	agent := mustAgent(t, s, user.ID, "invalid-peer-agent")

	if _, err := s.ApprovePeer(ctx, user.ID, user.ID); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("ApprovePeer(self) error = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.ApprovePeer(ctx, user.ID, agent.ID); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("ApprovePeer(agent id) error = %v, want ErrInvalidArgument", err)
	}
}

func TestUserPeerRenameAndReclaim(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := mustUser(t, s, "peer-rename-a")
	b := mustUser(t, s, "peer-rename-b")
	if _, err := s.ApprovePeer(ctx, a.ID, b.ID); err != nil {
		t.Fatalf("ApprovePeer: %v", err)
	}
	if _, err := s.ApprovePeer(ctx, b.ID, a.ID); err != nil {
		t.Fatalf("ApprovePeer reverse: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		"UPDATE account_handles SET handle = $2 WHERE account_id = $1", string(b.ID), "peer-rename-b-new"); err != nil {
		t.Fatalf("rename peer: %v", err)
	}
	peers, err := s.ListPeerings(ctx, a.ID)
	if err != nil || len(peers) != 1 || peers[0].PeerID != b.ID || peers[0].Handle != "peer-rename-b-new" || peers[0].State != PeeringApproved {
		t.Fatalf("peer after rename = %+v, %v; want original account, renamed handle, approved", peers, err)
	}

	if _, err := s.pool.Exec(ctx,
		"UPDATE account_handles SET handle = $2 WHERE account_id = $1", string(b.ID), "peer-rename-b-old"); err != nil {
		t.Fatalf("free peer handle: %v", err)
	}
	reclaimed, err := s.CreateUser(ctx, NewUser{Handle: "peer-rename-b", DisplayName: "Reclaimed"})
	if err != nil {
		t.Fatalf("reclaim peer handle: %v", err)
	}
	peers, err = s.ListPeerings(ctx, a.ID)
	if err != nil || len(peers) != 1 || peers[0].PeerID != b.ID || peers[0].Handle != "peer-rename-b-old" || peers[0].State != PeeringApproved {
		t.Fatalf("peer after reclaim = %+v, %v; want original id approved", peers, err)
	}
	if inserted, err := s.ApprovePeer(ctx, a.ID, reclaimed.ID); err != nil || !inserted {
		t.Fatalf("ApprovePeer(reclaimed account) = (%v, %v), want fresh row", inserted, err)
	}
	peers, err = s.ListPeerings(ctx, a.ID)
	if err != nil || len(peers) != 2 {
		t.Fatalf("ListPeerings after fresh approval = %+v, %v; want two distinct accounts", peers, err)
	}
}

func TestUserPeerCrossTenantIsolation(t *testing.T) {
	ctxA := context.Background()
	s := newTestStore(t)
	tenantB := seedTenant(t, s, "peer-tenant-b")
	ctxB := WithTenant(context.Background(), tenantB)
	a := mustUser(t, s, "peer-tenant-a-user")
	b, err := s.CreateUser(ctxB, NewUser{Handle: "peer-tenant-b-user", DisplayName: "Tenant B"})
	if err != nil {
		t.Fatalf("CreateUser(B): %v", err)
	}

	if _, err := s.ApprovePeer(ctxA, a.ID, b.ID); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("cross-tenant ApprovePeer error = %v, want ErrInvalidArgument", err)
	}
	peers, err := s.ListPeerings(ctxA, a.ID)
	if err != nil || len(peers) != 0 {
		t.Fatalf("tenant A peerings = %+v, %v; want none", peers, err)
	}

	other := mustUser(t, s, "peer-tenant-a-peer")
	if _, err := s.ApprovePeer(ctxA, a.ID, other.ID); err != nil {
		t.Fatalf("same-tenant ApprovePeer: %v", err)
	}
	peers, err = s.ListPeerings(ctxB, b.ID)
	if err != nil || len(peers) != 0 {
		t.Fatalf("tenant B peerings = %+v, %v; want no tenant A rows", peers, err)
	}
}

func TestOpenUpgradesPreviousMigrationToUserPeers(t *testing.T) {
	ctx := t.Context()
	dsn := pgtest.RequireDSN(t)
	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if len(migs) < 2 {
		t.Fatalf("migration count = %d, need prior migration and user_peers", len(migs))
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect before upgrade: %v", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		pool.Close()
		t.Fatalf("acquire before upgrade: %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		conn.Release()
		pool.Close()
		t.Fatalf("acquire migration lock: %v", err)
	}
	if err := ensureMigrationsTable(ctx, conn); err != nil {
		t.Fatalf("ensure migrations table: %v", err)
	}
	for _, migration := range migs[:len(migs)-1] {
		if err := applyMigration(ctx, conn, migration); err != nil {
			t.Fatalf("apply prior migration %q: %v", migration.name, err)
		}
	}
	// Users created before the upgrade must be peerable after it.
	const tenant TenantID = "upgrade-peer-tenant"
	if _, err := conn.Exec(ctx, "INSERT INTO tenants (id, slug, display_name, created_at_unix_ms) VALUES ($1, $1, $1, 0)", string(tenant)); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	for _, u := range []string{"upgrade-peer-a", "upgrade-peer-b"} {
		for _, q := range []string{
			"INSERT INTO accounts (id, handle, display_name, tenant_id) VALUES ($1, $1, $1, $2)",
			"INSERT INTO user_accounts (account_id, tenant_id) VALUES ($1, $2)",
			"INSERT INTO account_handles (account_id, handle, tenant_id) VALUES ($1, $1, $2)",
		} {
			if _, err := conn.Exec(ctx, q, u, string(tenant)); err != nil {
				t.Fatalf("seed user %s: %v", u, err)
			}
		}
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", migrationLockKey); err != nil {
		t.Fatalf("release migration lock: %v", err)
	}
	conn.Release()
	pool.Close()

	upgraded, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open upgrades prior schema: %v", err)
	}
	defer upgraded.Close()
	tctx := WithTenant(ctx, tenant)
	for _, edge := range [][2]AccountID{{"upgrade-peer-a", "upgrade-peer-b"}, {"upgrade-peer-b", "upgrade-peer-a"}} {
		if _, err := upgraded.ApprovePeer(tctx, edge[0], edge[1]); err != nil {
			t.Fatalf("ApprovePeer(%s, %s) after upgrade: %v", edge[0], edge[1], err)
		}
	}
	peers, err := upgraded.ListPeerings(tctx, "upgrade-peer-a")
	if err != nil || len(peers) != 1 || peers[0].State != PeeringApproved {
		t.Fatalf("peerings after upgrade = %+v, %v; want one approved", peers, err)
	}
}
