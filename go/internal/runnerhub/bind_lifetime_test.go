//go:build unix

package runnerhub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// recordingBinder is a LifetimeBinder that records the ctx each call ran under,
// so a test can observe the role and tenant the hub chose.
type recordingBinder struct {
	tenant        store.TenantID
	tenantCtx     context.Context
	tenantAccount store.AccountID
	bindCtx       context.Context
	bindCalls     int
	bindAcct      store.AccountID
	bindSessID    string
	tenantErr     error
}

func (b *recordingBinder) AccountTenant(ctx context.Context, account store.AccountID) (store.TenantID, error) {
	b.tenantCtx = ctx
	b.tenantAccount = account
	return b.tenant, b.tenantErr
}

func wireRecordingBinder(hub *Hub) *recordingBinder {
	binder := &recordingBinder{tenant: "tenant-test"}
	hub.SetLifetimeBinder(binder)
	return binder
}

func (b *recordingBinder) BindLifetime(ctx context.Context, sessionID string, account store.AccountID) (uint64, error) {
	b.bindCtx = ctx
	b.bindCalls++
	b.bindAcct = account
	b.bindSessID = sessionID
	return 0, nil
}

func TestHubBindLifetimeNoBinderUnavailable(t *testing.T) {
	hub := newHubOnly()
	hub.enroll(t.Context(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("cont-1", testAgentAccount, "runner-1")

	err := hub.BindLifetime(t.Context(), "runner-1", "cont-1", "sess-1")
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("BindLifetime with no binder: code = %v (%v), want Unavailable", got, err)
	}
}

// The tenant is read under the system role, but the write runs tenant-scoped
// under the account's tenant with the system role cleared.
func TestHubBindLifetimeWritesUnderAccountTenant(t *testing.T) {
	hub := newHubOnly()
	binder := &recordingBinder{tenant: "tenant-b"}
	hub.SetLifetimeBinder(binder)
	hub.enroll(t.Context(), "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("cont-1", testAgentAccount, "runner-1")

	if err := hub.BindLifetime(store.WithSystemRole(t.Context()), "runner-1", "cont-1", "sess-1"); err != nil {
		t.Fatalf("BindLifetime = %v", err)
	}
	if !store.IsSystemRole(binder.tenantCtx) {
		t.Fatal("AccountTenant ran without the system role; the door carries no tenant to scope it")
	}
	if binder.bindCalls != 1 || binder.bindAcct != testAgentAccount || binder.bindSessID != "sess-1" {
		t.Fatalf("bind calls = %d (%q, %q), want 1 for (%q, sess-1)", binder.bindCalls, binder.bindAcct, binder.bindSessID, testAgentAccount)
	}
	if store.IsSystemRole(binder.bindCtx) {
		t.Fatal("BindLifetime write ran under the system role, want tenant-scoped")
	}
	if got, ok := store.TenantFromContext(binder.bindCtx); !ok || got != "tenant-b" {
		t.Fatalf("bind tenant = %q (set=%v), want tenant-b", got, ok)
	}
}
