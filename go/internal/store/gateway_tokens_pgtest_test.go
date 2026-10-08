//go:build pgtest && unix

package store

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestGatewayTokenRotateResolveRevoke(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner, err := s.CreateUser(ctx, NewUser{Handle: "gw-owner", DisplayName: "owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "gw-agent", DisplayName: "agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	first := sha256.Sum256([]byte("first"))
	if err := s.RotateGatewayToken(ctx, agent.ID, first); err != nil {
		t.Fatalf("RotateGatewayToken(first): %v", err)
	}
	got, err := s.ResolveGatewayToken(ctx, first)
	if err != nil {
		t.Fatalf("ResolveGatewayToken(first): %v", err)
	}
	if got.AgentAccountID != agent.ID || got.OwnerUserID != owner.ID {
		t.Fatalf("resolved %+v, want agent %q owner %q", got, agent.ID, owner.ID)
	}

	second := sha256.Sum256([]byte("second"))
	if err := s.RotateGatewayToken(ctx, agent.ID, second); err != nil {
		t.Fatalf("RotateGatewayToken(second): %v", err)
	}
	if _, err := s.ResolveGatewayToken(ctx, first); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotated-out token resolve err = %v, want ErrNotFound", err)
	}
	if _, err := s.ResolveGatewayToken(ctx, second); err != nil {
		t.Fatalf("ResolveGatewayToken(second): %v", err)
	}

	if err := s.RevokeGatewayToken(ctx, agent.ID); err != nil {
		t.Fatalf("RevokeGatewayToken: %v", err)
	}
	if _, err := s.ResolveGatewayToken(ctx, second); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked token resolve err = %v, want ErrNotFound", err)
	}
	if err := s.RevokeGatewayToken(ctx, agent.ID); err != nil {
		t.Fatalf("repeat RevokeGatewayToken must be a no-op, got %v", err)
	}
}

// A token row carries its agent's owner and tenant, so the gateway can resolve
// a token minted in any tenant; minting is still scoped to the caller's tenant.
func TestGatewayTokenTenantScope(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	tenantB := seedTenant(t, s, "gw-tenant-b")
	ctxB := WithTenant(ctx, tenantB)
	ownerB, err := s.CreateUser(ctxB, NewUser{Handle: "gw-owner-b", DisplayName: "owner b"})
	if err != nil {
		t.Fatalf("CreateUser(b): %v", err)
	}
	agentB, err := s.CreateAgent(ctxB, ownerB.ID, NewAgent{Handle: "gw-agent-b", DisplayName: "agent b"})
	if err != nil {
		t.Fatalf("CreateAgent(b): %v", err)
	}

	hash := sha256.Sum256([]byte("tenant-b"))
	if err := s.RotateGatewayToken(ctx, agentB.ID, hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mint for another tenant's agent err = %v, want ErrNotFound", err)
	}
	if err := s.RevokeGatewayToken(ctx, agentB.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke for another tenant's agent err = %v, want ErrNotFound", err)
	}
	if err := s.RotateGatewayToken(ctxB, agentB.ID, hash); err != nil {
		t.Fatalf("RotateGatewayToken(b): %v", err)
	}

	var tenant string
	if err := s.pool.QueryRow(ctx, `SELECT tenant_id FROM gateway_tokens WHERE hash = $1`, hash[:]).Scan(&tenant); err != nil {
		t.Fatalf("read token tenant: %v", err)
	}
	if TenantID(tenant) != tenantB {
		t.Fatalf("token tenant = %q, want %q", tenant, tenantB)
	}
	got, err := s.ResolveGatewayToken(ctx, hash)
	if err != nil {
		t.Fatalf("resolve tenant-b token from a bootstrap ctx: %v", err)
	}
	if got.AgentAccountID != agentB.ID || got.OwnerUserID != ownerB.ID {
		t.Fatalf("resolved %+v, want agent %q owner %q", got, agentB.ID, ownerB.ID)
	}
}

// The partial unique index is the backstop if a writer skips the revoke.
func TestGatewayTokenOneLivePerAgent(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner, err := s.CreateUser(ctx, NewUser{Handle: "gw-live-owner", DisplayName: "owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "gw-live-agent", DisplayName: "agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := s.RotateGatewayToken(ctx, agent.ID, sha256.Sum256([]byte("a"))); err != nil {
		t.Fatalf("RotateGatewayToken: %v", err)
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO gateway_tokens (hash, agent_account_id, owner_user_id, tenant_id)
		 SELECT $1, account_id, owner_user_id, tenant_id FROM agent_accounts WHERE account_id = $2`,
		[]byte("raw-second"), string(agent.ID))
	if !pgErrIs(err, pgUniqueViolation) {
		t.Fatalf("second live token insert err = %v, want unique violation", err)
	}
}

// A Rotate that meets an in-flight rotation waits on the agent row, then
// replaces that rotation's token, so exactly one token stays live.
func TestGatewayTokenConcurrentRotatesSerialize(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner, err := s.CreateUser(ctx, NewUser{Handle: "gw-race-owner", DisplayName: "owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "gw-race-agent", DisplayName: "agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	gate, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin gate tx: %v", err)
	}
	defer func() {
		if err := gate.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("rollback gate: %v", err)
		}
	}()
	// The gate is a rotation in flight: it holds the agent-row lock and an uncommitted live token.
	if _, err := gate.Exec(ctx, `SELECT 1 FROM agent_accounts WHERE account_id = $1 FOR NO KEY UPDATE`, string(agent.ID)); err != nil {
		t.Fatalf("gate agent-row lock: %v", err)
	}
	first := sha256.Sum256([]byte("race-first"))
	if _, err := gate.Exec(ctx,
		`INSERT INTO gateway_tokens (hash, agent_account_id, owner_user_id, tenant_id)
		 SELECT $1, account_id, owner_user_id, tenant_id FROM agent_accounts WHERE account_id = $2`,
		first[:], string(agent.ID)); err != nil {
		t.Fatalf("gate insert token: %v", err)
	}

	second := sha256.Sum256([]byte("race-second"))
	done := make(chan error, 1)
	go func() { done <- s.RotateGatewayToken(ctx, agent.ID, second) }()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		// A row-lock waiter holds the tuple lock on the agent row while it waits on the gate's xid.
		var waiters int
		if err := gate.QueryRow(ctx,
			`SELECT count(*) FROM pg_locks w JOIN pg_locks r ON r.pid = w.pid
			 WHERE w.locktype = 'transactionid' AND NOT w.granted
			   AND w.transactionid = (SELECT transactionid FROM pg_locks
			                          WHERE pid = pg_backend_pid() AND locktype = 'transactionid')
			   AND r.locktype = 'tuple' AND r.relation = 'agent_accounts'::regclass`,
		).Scan(&waiters); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if waiters >= 1 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Rotate finished (err=%v) while the gate held the agent row", err)
		case <-deadline:
			t.Fatal("Rotate never waited on the agent-row lock")
		case <-tick.C:
		}
	}
	if err := gate.Commit(ctx); err != nil {
		t.Fatalf("release gate: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("RotateGatewayToken after release: %v", err)
	}

	rows, err := s.pool.Query(ctx, `SELECT hash FROM gateway_tokens WHERE agent_account_id = $1 AND revoked_at IS NULL`, string(agent.ID))
	if err != nil {
		t.Fatalf("read live tokens: %v", err)
	}
	live, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatalf("scan live tokens: %v", err)
	}
	if len(live) != 1 || string(live[0]) != string(second[:]) {
		t.Fatalf("live tokens = %x, want only the second rotation's hash", live)
	}
}

// gateway_tokens follows the tokens posture: RLS-exempt because verification
// precedes any tenant, with explicit grants since 0001's grant predates it.
func TestGatewayTokensGrantsAndRLS(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	var rls bool
	if err := s.pool.QueryRow(ctx,
		`SELECT relrowsecurity FROM pg_class WHERE oid = format('%I.%I', current_schema(), 'gateway_tokens')::regclass`,
	).Scan(&rls); err != nil {
		t.Fatalf("read RLS flag: %v", err)
	}
	if rls {
		t.Fatal("gateway_tokens: RLS enabled; verification would fail closed before a tenant exists")
	}
	for _, role := range []string{"compass_app", "compass_system"} {
		for _, priv := range []string{"SELECT", "INSERT", "UPDATE"} {
			if !hasTablePrivilege(t, s, role, "gateway_tokens", priv) {
				t.Fatalf("gateway_tokens: %s lacks %s", role, priv)
			}
		}
	}
}
