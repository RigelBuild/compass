//go:build pgtest && unix

package runnerhub

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

// A Runner is shared across tenants and its door carries none. A lost session of a
// non-bootstrap tenant, read cold from the table, must still be released durably and
// woken: the hub resolves the session's tenant before it reads or deletes.
func TestDropLostSessionScopesToTheSessionTenant(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "admin"}); err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}

	tenantB := seedTenantRow(t, ctx, dsn, "tenant-b")
	ctxB := store.WithTenant(ctx, tenantB)
	owner, err := st.CreateUser(ctxB, store.NewUser{Handle: "owner-b", DisplayName: "owner-b"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctxB, owner.ID, store.NewAgent{Handle: "agent-b", DisplayName: "agent-b"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if _, err := st.RecordSessionBinding(ctxB, "sess-b", agent.ID, "runner-1"); err != nil {
		t.Fatalf("RecordSessionBinding: %v", err)
	}

	// A fresh hub: its cache is cold, as after a Server restart.
	hub := newHubOnly()
	hub.SetSessionBindingStore(st)
	sink := &recordingLostSink{}
	hub.SetSessionLostSink(sink)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	// enroll's reap ran unscoped and cannot see tenant B's row; re-record it so the
	// test does not depend on that.
	if _, err := st.RecordSessionBinding(ctxB, "sess-b", agent.ID, "runner-1"); err != nil {
		t.Fatalf("RecordSessionBinding after enroll: %v", err)
	}

	hub.dropLostSession(ctx, "runner-1", "sess-b")

	if len(sink.lost) != 1 || sink.lost[0] != agent.ID {
		t.Fatalf("lost = %v, want [%s]: the tenant-B session was not resolved, so no wake", sink.lost, agent.ID)
	}
	if _, _, err := st.ResolveSessionBinding(ctxB, "sess-b"); err == nil {
		t.Fatal("tenant B's durable binding survived the drop; a cache miss would resurrect it")
	}
}

// seedTenantRow inserts a non-bootstrap tenant. tenants is RLS-exempt, so a plain
// connection may write it.
func seedTenantRow(t *testing.T, ctx context.Context, dsn, slug string) store.TenantID {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgx.Connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() // test cleanup; nothing actionable remains
	id := slug + "-id"
	if _, err := conn.Exec(ctx,
		"INSERT INTO tenants (id, slug, display_name, created_at_unix_ms) VALUES ($1, $2, $3, $4)",
		id, slug, slug, time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed tenant %q: %v", slug, err)
	}
	return store.TenantID(id)
}
