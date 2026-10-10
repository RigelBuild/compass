//go:build pgtest && unix

package server

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/secrets"
	"github.com/RigelBuild/compass/go/internal/store"
)

type agentRepositoryLifecycleTest struct {
	t        *testing.T
	f        lifecycleFixture
	ctx      context.Context
	clock    *time.Time
	minter   *fakeGitCredentialMinter
	resolver *brokeredSecretResolver
	client   compassv1connect.AgentRepositoryServiceClient
	token    string
	signal   *recordingAgentRepoSignaler
}

func newAgentRepositoryLifecycleTest(t *testing.T) *agentRepositoryLifecycleTest {
	t.Helper()
	f := newLifecycleFixture(t)
	ctx := context.Background()
	if err := f.store.GrantForgeScope(ctx, store.ForgeScope{
		AccountID: f.ownerAdmin, Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/base",
	}); err != nil {
		t.Fatalf("GrantForgeScope(owner): %v", err)
	}

	clock := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	minter := &fakeGitCredentialMinter{
		tokens:    []string{"owner-only", "parent-workstream", "owner-after-revoke", "parent-workstream-after-time", "child-custom"},
		expiresAt: clock.Add(time.Hour),
	}
	broker := &gitCredentialBroker{
		host: "github.com", store: f.store, minter: minter, log: discardLogE2E(),
		clock: func() time.Time { return clock }, entries: make(map[string]gitCredentialEntry),
		negative: make(map[string]gitCredentialNegativeEntry), last: make(map[store.AccountID]string),
	}
	resolver := &brokeredSecretResolver{inner: &fakeAgentSecretResolver{}, broker: broker}
	token, err := auth.IssueAccountToken(ctx, f.store, f.ownerAdmin)
	if err != nil {
		t.Fatalf("IssueAccountToken(owner): %v", err)
	}
	signal := &recordingAgentRepoSignaler{}
	svc := newAgentRepositoryService(f.store, broker, signal)
	url := newAgentRepositoryH2CServer(t, svc, f.store, f.ownerAdmin)
	client := compassv1connect.NewAgentRepositoryServiceClient(newAgentRepositoryH2CClient(t), url)
	return &agentRepositoryLifecycleTest{
		t: t, f: f, ctx: ctx, clock: &clock, minter: minter, resolver: resolver, client: client, token: token, signal: signal,
	}
}

func (e *agentRepositoryLifecycleTest) resolveToken(agent store.AccountID) string {
	e.t.Helper()
	resolved, err := e.resolver.ResolveFor(e.ctx, agent, gitCredentialReason)
	if err != nil {
		e.t.Fatalf("ResolveFor(%s): %v", agent, err)
	}
	for _, secret := range resolved {
		if secret.Name == gitCredentialSecretName && secret.Kind == secrets.SecretGH {
			return secret.Value
		}
	}
	e.t.Fatalf("ResolveFor(%s) returned no GitHub credential: %+v", agent, resolved)
	return ""
}

func (e *agentRepositoryLifecycleTest) assertToken(agent store.AccountID, want, stage string) {
	e.t.Helper()
	if got := e.resolveToken(agent); got != want {
		e.t.Fatalf("%s token = %q, want %q", stage, got, want)
	}
}

func (e *agentRepositoryLifecycleTest) assertMintRepos(index int, want []string, stage string) {
	e.t.Helper()
	got := e.minter.mintRepos()
	if index >= len(got) || !slices.Equal(got[index], want) {
		e.t.Fatalf("%s mint repositories = %v, want %v at index %d", stage, got, want, index)
	}
}

func (e *agentRepositoryLifecycleTest) grant(handle, repository string) {
	e.t.Helper()
	resp, err := grantAgentRepository(e.client, e.ctx, e.token, handle, repository)
	if err != nil || !resp.Msg.GetAdded() {
		e.t.Fatalf("GrantAgentRepository(%q, %q) = (%v, %v), want added", handle, repository, resp, err)
	}
}

func (e *agentRepositoryLifecycleTest) revoke(handle, repository string) {
	e.t.Helper()
	resp, err := revokeAgentRepository(e.client, e.ctx, e.token, handle, repository)
	if err != nil || !resp.Msg.GetRemoved() {
		e.t.Fatalf("RevokeAgentRepository(%q, %q) = (%v, %v), want removed", handle, repository, resp, err)
	}
}

func (e *agentRepositoryLifecycleTest) spawn(requestID string) store.AccountID {
	e.t.Helper()
	resp, err := e.f.lc.SpawnAsAccount(e.ctx, e.f.agentID, &compassv1internal.SpawnPeerRequest{
		Handle: "repo-child", DisplayName: "Repository Child", ClientRequestId: requestID, Role: "manager",
	})
	if err != nil {
		e.t.Fatalf("SpawnAsAccount(%q): %v", requestID, err)
	}
	child := store.AccountID(resp.GetAgentAccountId())
	if child == "" || child == e.f.agentID {
		e.t.Fatalf("spawned child id = %q, want a fresh agent account", child)
	}
	return child
}

func (e *agentRepositoryLifecycleTest) assertRows(agent store.AccountID, want []string, stage string) {
	e.t.Helper()
	got, err := e.f.store.ListAgentForgeScopeRepos(e.ctx, agent, store.ForgeProviderGitHub, "github.com")
	if err != nil || !slices.Equal(got, want) {
		e.t.Fatalf("%s rows = %v, %v, want %v", stage, got, err, want)
	}
}

func (e *agentRepositoryLifecycleTest) despawn(handle string) {
	e.t.Helper()
	if _, err := e.f.lc.DespawnAsAccount(e.ctx, e.f.agentID, &compassv1internal.DespawnPeerRequest{AgentHandle: handle}); err != nil {
		e.t.Fatalf("DespawnAsAccount(%s): %v", handle, err)
	}
}

func (e *agentRepositoryLifecycleTest) assertSignals(want []store.AccountID) {
	e.t.Helper()
	if !slices.Equal(e.signal.accounts, want) {
		e.t.Fatalf("targeted signals = %v, want %v", e.signal.accounts, want)
	}
}

func TestAgentRepositoryGrantRevokeInheritanceAndDespawn(t *testing.T) {
	e := newAgentRepositoryLifecycleTest(t)
	e.assertToken(e.f.agentID, "owner-only", "initial agent")
	e.assertMintRepos(0, []string{"base"}, "initial")
	e.grant("atlas", "OWNER/WORKSTREAM")
	e.assertToken(e.f.agentID, "parent-workstream", "parent after grant")
	e.assertMintRepos(1, []string{"base", "workstream"}, "parent after grant")
	e.revoke("atlas", "owner/workstream")
	*e.clock = e.clock.Add(46 * time.Minute)
	e.minter.mu.Lock()
	e.minter.expiresAt = e.clock.Add(time.Hour)
	e.minter.mu.Unlock()
	e.assertToken(e.f.agentID, "owner-after-revoke", "parent after revoke")
	e.assertMintRepos(2, []string{"base"}, "parent after revoke")

	e.grant("atlas", "owner/workstream")
	e.f.runner.setContainerNames("child-container-1", "child-container-2")
	e.f.runner.setStartIDs("child-session-1", "child-session-2")
	child := e.spawn("repo-child-first")
	e.assertRows(child, []string{"owner/workstream"}, "child inherited")
	e.assertToken(child, "parent-workstream-after-time", "inherited child")
	e.assertMintRepos(3, []string{"base", "workstream"}, "inherited child")

	e.revoke("admin/repo-child", "owner/workstream")
	e.assertToken(child, "owner-after-revoke", "child after revoke")
	if got := e.spawn("repo-child-resume"); got != child {
		t.Fatalf("same-handle spawn id = %q, want existing child %q", got, child)
	}
	e.assertRows(child, nil, "child after resume")

	e.grant("admin/repo-child", "owner/child-only")
	e.assertToken(child, "child-custom", "child after custom grant")
	e.despawn("repo-child")
	e.assertRows(child, []string{"owner/child-only"}, "child after despawn")
	if got := e.spawn("repo-child-respawn"); got != child {
		t.Fatalf("respawn id = %q, want existing child %q", got, child)
	}
	e.assertToken(child, "child-custom", "child after respawn")
	e.assertMintRepos(4, []string{"base", "child-only"}, "child after grant")
	e.assertSignals([]store.AccountID{e.f.agentID, e.f.agentID, e.f.agentID, child, child})
}
