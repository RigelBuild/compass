//go:build pgtest && unix

package store

import (
	"crypto/sha256"
	"errors"
	"testing"
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
