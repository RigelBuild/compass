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
	st, agent, ctxB := openTenantBSession(t, ctx)

	// A fresh hub: its cache is cold, as after a Server restart.
	hub := newHubOnly()
	hub.SetSessionBindingStore(st)
	sink := newRecordingLostSink()
	hub.SetSessionLostSink(sink)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	// Enroll reaps every binding on this Runner; re-record so the drop reads a live row cold.
	if _, _, err := st.RecordSessionBinding(ctxB, "sess-b", agent.ID, "runner-1"); err != nil {
		t.Fatalf("RecordSessionBinding after enroll: %v", err)
	}

	hub.dropLostSession(ctx, "runner-1", "sess-b", nil, false)

	if lost, _ := sink.waitOne(t); lost != agent.ID {
		t.Fatalf("lost = %s, want %s: the tenant-B session was not resolved, so no wake", lost, agent.ID)
	}
	if _, _, _, err := st.ResolveSessionBinding(ctxB, "sess-b"); err == nil {
		t.Fatal("tenant B's durable binding survived the drop; a cache miss would resurrect it")
	}
}

// An enrolling Runner spans tenants, so its system-role sweep must reap tenant B's
// binding, record its end event with tenant B's id, and archive it under tenant B.
func TestEnrollReapsSessionBindingAcrossTenants(t *testing.T) {
	ctx := context.Background()
	st, agent, ctxB := openTenantBSession(t, ctx)
	tenantB, _ := store.TenantFromContext(ctxB)

	hub := newHubOnly()
	hub.SetSessionBindingStore(st)
	ended := tenantRecordingEndSink{ended: make(chan endedArchive, 1)}
	hub.SetSessionEndSink(ended)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

	var remaining, ends int
	if err := st.WithTx(ctxB, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctxB, "SELECT count(*) FROM session_bindings WHERE session_id = 'sess-b'").Scan(&remaining); err != nil {
			return err
		}
		return tx.QueryRow(ctxB, `SELECT count(*) FROM compute_usage_events
			WHERE agent_account_id = $1 AND session_id = 'sess-b' AND kind = 'end'`, string(agent.ID)).Scan(&ends)
	}); err != nil {
		t.Fatalf("read tenant B after enroll reap: %v", err)
	}
	if remaining != 0 || ends != 1 {
		t.Fatalf("tenant B binding remaining = %d, end events = %d; want reaped binding and one end", remaining, ends)
	}
	select {
	case got := <-ended.ended:
		if got != (endedArchive{tenant: tenantB, sessionID: "sess-b"}) {
			t.Fatalf("archived %+v, want sess-b under %q", got, tenantB)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no session-end archive within 10s")
	}
}

// openTenantBSession opens a store with a non-bootstrap tenant B whose agent holds
// sess-b on runner-1.
func openTenantBSession(t *testing.T, ctx context.Context) (*store.Store, store.Account, context.Context) {
	t.Helper()
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
	if _, _, err := st.RecordSessionBinding(ctxB, "sess-b", agent.ID, "runner-1"); err != nil {
		t.Fatalf("RecordSessionBinding: %v", err)
	}
	return st, agent, ctxB
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
