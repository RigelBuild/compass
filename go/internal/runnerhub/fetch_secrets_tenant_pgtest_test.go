//go:build pgtest && unix

package runnerhub

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/envelope"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/secrets"
	"github.com/RigelBuild/compass/go/internal/store"
)

// grantedRepoResolver adds one secret per forge grant to the stored secrets, so a
// test sees both RLS-scoped reads the real brokered resolver makes.
type grantedRepoResolver struct {
	st    *store.Store
	inner *secrets.StoreResolver
}

func (r grantedRepoResolver) ResolveFor(ctx context.Context, agent store.AccountID, reason string) ([]secrets.ResolvedSecret, error) {
	resolved, err := r.inner.ResolveFor(ctx, agent, reason)
	if err != nil {
		return nil, err
	}
	repos, err := r.st.ListForgeScopeRepos(ctx, agent, store.ForgeProviderGitHub, "github.com")
	if err != nil {
		return nil, err
	}
	for _, repo := range repos {
		resolved = append(resolved, secrets.ResolvedSecret{Name: "GRANT", Value: repo})
	}
	return resolved, nil
}

// The Runner door carries no tenant, so FetchSecrets must read a tenant-B agent's
// secrets and forge grants under tenant B, not the bootstrap tenant.
func TestFetchSecretsResolvesUnderTheAgentTenant(t *testing.T) {
	ctx := context.Background()
	st, agent, ctxB := openTenantBSession(t, ctx)
	key, err := envelope.NewKey(make([]byte, 32))
	if err != nil {
		t.Fatalf("envelope.NewKey: %v", err)
	}
	resolver := secrets.NewStoreResolver(st, key, 1)
	if err := resolver.Upsert(ctxB, agent.Agent.OwnerUserID, "TENANT_B_SECRET", store.SecretScopeAgent, string(agent.ID),
		"b-value", secrets.DeliveryEnv, secrets.SecretGeneric, "", ""); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := st.GrantForgeScope(ctxB, store.ForgeScope{
		AccountID: agent.Agent.OwnerUserID, Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "rigelbuild/tenant-b",
	}); err != nil {
		t.Fatalf("GrantForgeScope: %v", err)
	}

	hub := newHubOnly()
	hub.SetLifetimeBinder(st)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("cont-b", agent.ID, "runner-1")
	url := newMountedH2CServerWith(t, hub, runnerResolverForFetch().resolve, grantedRepoResolver{st: st, inner: resolver}, nil)
	client := newRawRunnerClient(t, url, "runner-tok")

	resp, err := client.FetchSecrets(ctx, connect.NewRequest(&compassv1internal.FetchSecretsRequest{ContainerName: "cont-b"}))
	if err != nil {
		t.Fatalf("FetchSecrets: %v", err)
	}
	got := map[string]string{}
	for _, s := range resp.Msg.GetSecrets() {
		got[s.GetName()] = s.GetValue()
	}
	if got["TENANT_B_SECRET"] != "b-value" || got["GRANT"] != "rigelbuild/tenant-b" {
		t.Fatalf("FetchSecrets for a tenant-B agent = %v, want its secret and its forge grant", got)
	}
}
