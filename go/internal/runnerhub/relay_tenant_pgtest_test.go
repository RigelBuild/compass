//go:build pgtest && unix

package runnerhub

import (
	"context"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

type tenantCommsCaller struct {
	*fakeCommsCaller
	ctx context.Context
}

func (c *tenantCommsCaller) PostAsAccountByName(ctx context.Context, account store.AccountID, req *compassv1.PostMessageRequest) (*compassv1.PostMessageResponse, error) {
	c.ctx = ctx
	return c.fakeCommsCaller.PostAsAccountByName(ctx, account, req)
}

type tenantLifecycleCaller struct {
	fakeLifecycleCaller
	ctx context.Context
}

func (c *tenantLifecycleCaller) SpawnAsAccount(ctx context.Context, caller store.AccountID, req *compassv1internal.SpawnPeerRequest) (*compassv1internal.SpawnPeerResponse, error) {
	c.ctx = ctx
	return c.fakeLifecycleCaller.SpawnAsAccount(ctx, caller, req)
}

type tenantBoardCaller struct {
	fakeBoardCaller
	ctx context.Context
}

func (c *tenantBoardCaller) SetIssueStateAsAccount(ctx context.Context, caller store.AccountID, req *compassv1internal.SetIssueStateRequest) (*compassv1internal.SetIssueStateResponse, error) {
	c.ctx = ctx
	return c.fakeBoardCaller.SetIssueStateAsAccount(ctx, caller, req)
}

// Runners are shared across tenants and their door carries none, so each relay
// must hand its service a ctx scoped to the session's tenant.
func TestRunnerRelaysRunUnderTheSessionTenant(t *testing.T) {
	ctx := context.Background()
	st, agent, ctxB := openTenantBSession(t, ctx)
	tenantB, _ := store.TenantFromContext(ctxB)
	const sessionID = "sess-b"
	if err := st.RecordAgentSession(ctxB, sessionID, agent.ID); err != nil {
		t.Fatalf("RecordAgentSession: %v", err)
	}

	comms := &tenantCommsCaller{fakeCommsCaller: &fakeCommsCaller{postResp: &compassv1.PostMessageResponse{}}}
	hub := NewHub(&fakeLifecycleSink{}, &fakeTailSink{}, comms, discardLogger())
	lifecycle := &tenantLifecycleCaller{fakeLifecycleCaller: fakeLifecycleCaller{spawnResp: &compassv1internal.SpawnPeerResponse{}}}
	board := &tenantBoardCaller{fakeBoardCaller: fakeBoardCaller{resp: &compassv1internal.SetIssueStateResponse{}}}
	hub.SetLifecycleCaller(lifecycle)
	hub.SetBoardCaller(board)
	hub.SetSessionBindingStore(st)
	hub.SetTranscriptStore(st)
	if _, err := hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED); err != nil {
		t.Fatalf("enroll Runner: %v", err)
	}
	if _, _, err := st.RecordSessionBinding(ctxB, sessionID, agent.ID, "runner-1"); err != nil {
		t.Fatalf("RecordSessionBinding after enroll: %v", err)
	}

	if _, err := hub.RelayCommsCall(ctx, "runner-1", relayPost(sessionID, "c-1", &compassv1.PostMessageRequest{})); err != nil {
		t.Fatalf("RelayCommsCall: %v", err)
	}
	if _, err := hub.RelayLifecycleCall(ctx, "runner-1", relaySpawn(sessionID, "l-1", &compassv1internal.SpawnPeerRequest{})); err != nil {
		t.Fatalf("RelayLifecycleCall: %v", err)
	}
	if _, err := hub.RelayBoardCall(ctx, "runner-1", relaySetIssueState(sessionID, "b-1", &compassv1internal.SetIssueStateRequest{})); err != nil {
		t.Fatalf("RelayBoardCall: %v", err)
	}
	for name, got := range map[string]context.Context{"comms": comms.ctx, "lifecycle": lifecycle.ctx, "board": board.ctx} {
		if tenant, ok := store.TenantFromContext(got); !ok || tenant != tenantB {
			t.Errorf("%s relay tenant = %q (set %t), want %q", name, tenant, ok, tenantB)
		}
	}

	// The transcript row is a tenant-B row; under the bootstrap tenant its
	// session FK misses and the commit fails.
	if _, err := hub.CommitConversationFrame(ctx, "runner-1", transcriptReq(sessionID, "k-1", 1, false, `{"e":1}`)); err != nil {
		t.Fatalf("CommitConversationFrame: %v", err)
	}
}
