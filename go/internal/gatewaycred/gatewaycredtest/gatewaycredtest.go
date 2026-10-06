// Package gatewaycredtest is the contract suite every credential backend must pass.
package gatewaycredtest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/RigelBuild/compass/go/internal/gatewaycred"
	"github.com/RigelBuild/compass/go/internal/store"
)

const (
	ownAccessToken         = "own"
	replacementAccessToken = "replacement"
	oldAccessToken         = "old"
)

// Harness adapts a credential store, agent provisioning, and tenant contexts.
type Harness struct {
	New   func(t *testing.T) (gatewaycred.CredentialStore, gatewaycred.PoolResolver)
	Ctx   func(t *testing.T, tenant int) context.Context
	Agent func(t *testing.T, ctx context.Context, owner string) (agent, ownerID store.AccountID)
}

// Run runs the credential contract against the backend h adapts.
func Run(t *testing.T, h Harness) {
	t.Helper()
	t.Run("own_before_shared_per_provider", ownBeforeShared(h))
	t.Run("disabled_own_falls_back_to_shared", disabledOwnFallsBack(h))
	t.Run("provider_filter", providerFilter(h))
	t.Run("cross_agent_writes_are_not_found", crossAgentWritesAreNotFound(h))
	t.Run("shadowed_shared_write_is_not_found", shadowedSharedWriteIsNotFound(h))
	t.Run("unknown_agent_is_not_found", unknownAgentIsNotFound(h))
	t.Run("cas_race_has_one_winner", casRace(h))
	t.Run("stale_version_is_conflict", staleVersion(h))
	t.Run("invalid_arguments_are_rejected", invalidArguments(h))
	t.Run("api_key_update_is_precondition_failure", apiKeyUpdate(h))
	t.Run("oauth_update_keeps_unset_fields", oauthMerge(h))
	t.Run("disable_removes_credential", disable(h))
	t.Run("tenants_are_isolated", tenantsAreIsolated(h))
}

func ownBeforeShared(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, pool := h.New(t)
		ctx := h.Ctx(t, 0)
		agent, owner := h.Agent(t, ctx, "owner")
		create(t, ctx, s, own("anthropic", owner, gatewaycred.OAuthToken{Access: ownAccessToken}))
		create(t, ctx, s, shared("anthropic", gatewaycred.OAuthToken{Access: "shared-anthropic"}))
		create(t, ctx, s, shared("openai", gatewaycred.OAuthToken{Access: "shared-openai"}))

		got, err := pool.Pool(ctx, agent)
		if err != nil {
			t.Fatalf("Pool: %v", err)
		}
		if len(got) != 2 || got[0].Provider != "anthropic" || got[0].Scope != gatewaycred.ScopeOwn || got[1].Provider != "openai" || got[1].Scope != gatewaycred.ScopeShared {
			t.Fatalf("Pool() = %#v, want own anthropic and shared openai", got)
		}
	}
}

func disabledOwnFallsBack(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, pool := h.New(t)
		ctx := h.Ctx(t, 0)
		agent, owner := h.Agent(t, ctx, "owner")
		create(t, ctx, s, shared("anthropic", gatewaycred.OAuthToken{Access: "shared"}))
		ownCred := create(t, ctx, s, own("anthropic", owner, gatewaycred.OAuthToken{Access: ownAccessToken}))
		if err := s.Disable(ctx, agent, ownCred.ID, "rotated", ownCred.Version); err != nil {
			t.Fatalf("Disable own: %v", err)
		}
		got, err := pool.Pool(ctx, agent)
		if err != nil {
			t.Fatalf("Pool: %v", err)
		}
		if len(got) != 1 || got[0].Scope != gatewaycred.ScopeShared {
			t.Fatalf("Pool() = %#v, want enabled shared fallback", got)
		}
	}
}

func providerFilter(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, _ := h.New(t)
		ctx := h.Ctx(t, 0)
		_, owner := h.Agent(t, ctx, "owner")
		create(t, ctx, s, own("anthropic", owner, gatewaycred.OAuthToken{Access: "a"}))
		create(t, ctx, s, own("openai", owner, gatewaycred.OAuthToken{Access: "b"}))
		got, err := s.List(ctx, owner, "openai")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 || got[0].Provider != "openai" {
			t.Fatalf("List(provider=openai) = %#v", got)
		}
	}
}

func crossAgentWritesAreNotFound(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, _ := h.New(t)
		ctx := h.Ctx(t, 0)
		firstAgent, owner := h.Agent(t, ctx, "owner-a")
		secondAgent, _ := h.Agent(t, ctx, "owner-b")
		cred := create(t, ctx, s, own("anthropic", owner, gatewaycred.OAuthToken{Access: "secret"}))
		randomID := "no-such-credential"

		_, foreignUpdate := s.UpdateOAuth(ctx, secondAgent, cred.ID, gatewaycred.OAuthToken{Access: replacementAccessToken}, cred.Version)
		_, randomUpdate := s.UpdateOAuth(ctx, secondAgent, randomID, gatewaycred.OAuthToken{Access: replacementAccessToken}, cred.Version)
		if !errors.Is(foreignUpdate, gatewaycred.ErrNotFound) || foreignUpdate.Error() != randomUpdate.Error() {
			t.Fatalf("foreign UpdateOAuth error = %v, random id = %v", foreignUpdate, randomUpdate)
		}
		foreignDisable := s.Disable(ctx, secondAgent, cred.ID, "x", cred.Version)
		randomDisable := s.Disable(ctx, secondAgent, randomID, "x", cred.Version)
		if !errors.Is(foreignDisable, gatewaycred.ErrNotFound) || foreignDisable.Error() != randomDisable.Error() {
			t.Fatalf("foreign Disable error = %v, random id = %v", foreignDisable, randomDisable)
		}
		if _, err := s.UpdateOAuth(ctx, firstAgent, cred.ID, gatewaycred.OAuthToken{Access: replacementAccessToken}, cred.Version); err != nil {
			t.Fatalf("owner update after rejected probes: %v", err)
		}
	}
}

func shadowedSharedWriteIsNotFound(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, _ := h.New(t)
		ctx := h.Ctx(t, 0)
		agent, owner := h.Agent(t, ctx, "owner")
		create(t, ctx, s, shared("anthropic", gatewaycred.OAuthToken{Access: "shared"}))
		ownCred := create(t, ctx, s, own("anthropic", owner, gatewaycred.OAuthToken{Access: ownAccessToken}))
		sharedCreds, err := s.List(ctx, owner, "anthropic")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		var sharedID string
		for _, c := range sharedCreds {
			if c.Scope == gatewaycred.ScopeShared {
				sharedID = c.ID
			}
		}
		if sharedID == "" {
			t.Fatal("List did not expose shared credential")
		}
		if _, err := s.UpdateOAuth(ctx, agent, sharedID, gatewaycred.OAuthToken{Access: replacementAccessToken}, 1); !errors.Is(err, gatewaycred.ErrNotFound) {
			t.Fatalf("UpdateOAuth shadowed shared: %v", err)
		}
		if err := s.Disable(ctx, agent, sharedID, "x", 1); !errors.Is(err, gatewaycred.ErrNotFound) {
			t.Fatalf("Disable shadowed shared: %v", err)
		}
		if _, err := s.UpdateOAuth(ctx, agent, ownCred.ID, gatewaycred.OAuthToken{Access: replacementAccessToken}, ownCred.Version); err != nil {
			t.Fatalf("update own credential: %v", err)
		}
	}
}

func unknownAgentIsNotFound(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, pool := h.New(t)
		ctx := h.Ctx(t, 0)
		unknown := store.AccountID("unknown-agent")
		agent, owner := h.Agent(t, ctx, "owner")
		credential := create(t, ctx, s, own("anthropic", owner, gatewaycred.OAuthToken{Access: "known-row"}))
		if _, err := pool.Pool(ctx, unknown); !errors.Is(err, gatewaycred.ErrNotFound) {
			t.Fatalf("Pool unknown agent: %v", err)
		}
		if _, err := s.UpdateOAuth(ctx, unknown, credential.ID, gatewaycred.OAuthToken{Access: "x"}, credential.Version); !errors.Is(err, gatewaycred.ErrNotFound) {
			t.Fatalf("UpdateOAuth unknown agent: %v", err)
		}
		if err := s.Disable(ctx, unknown, credential.ID, "x", credential.Version); !errors.Is(err, gatewaycred.ErrNotFound) {
			t.Fatalf("Disable unknown agent: %v", err)
		}
		if _, err := s.UpdateOAuth(ctx, agent, credential.ID, gatewaycred.OAuthToken{Access: "x"}, credential.Version); err != nil {
			t.Fatalf("UpdateOAuth known agent: %v", err)
		}
	}
}

func casRace(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, _ := h.New(t)
		ctx := h.Ctx(t, 0)
		agent, owner := h.Agent(t, ctx, "owner")
		cred := create(t, ctx, s, own("anthropic", owner, gatewaycred.OAuthToken{Access: oldAccessToken}))
		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		for _, access := range []string{"one", "two"} {
			wg.Add(1)
			go func(access string) {
				defer wg.Done()
				<-start
				_, err := s.UpdateOAuth(ctx, agent, cred.ID, gatewaycred.OAuthToken{Access: access}, cred.Version)
				results <- err
			}(access)
		}
		close(start)
		wg.Wait()
		close(results)
		var wins, conflicts int
		for err := range results {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, gatewaycred.ErrVersionConflict):
				conflicts++
			default:
				t.Fatalf("UpdateOAuth race error: %v", err)
			}
		}
		if wins != 1 || conflicts != 1 {
			t.Fatalf("race outcomes: %d wins, %d conflicts", wins, conflicts)
		}
		got, err := s.List(ctx, owner, "anthropic")
		if err != nil {
			t.Fatalf("List final: %v", err)
		}
		if len(got) != 1 || got[0].Version != cred.Version+1 {
			t.Fatalf("final credentials = %#v, want version %d", got, cred.Version+1)
		}
	}
}

func staleVersion(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, _ := h.New(t)
		ctx := h.Ctx(t, 0)
		agent, owner := h.Agent(t, ctx, "owner")
		cred := create(t, ctx, s, own("anthropic", owner, gatewaycred.OAuthToken{Access: oldAccessToken}))
		if _, err := s.UpdateOAuth(ctx, agent, cred.ID, gatewaycred.OAuthToken{Access: "new"}, cred.Version+1); !errors.Is(err, gatewaycred.ErrVersionConflict) {
			t.Fatalf("stale UpdateOAuth: %v", err)
		}
	}
}

func invalidArguments(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, _ := h.New(t)
		ctx := h.Ctx(t, 0)
		agent, owner := h.Agent(t, ctx, "owner")
		cred := create(t, ctx, s, own("anthropic", owner, gatewaycred.OAuthToken{Access: oldAccessToken}))
		for _, tc := range []struct {
			name    string
			agent   store.AccountID
			id      string
			token   gatewaycred.OAuthToken
			version int64
		}{
			{name: "empty id", agent: agent, id: "", token: gatewaycred.OAuthToken{Access: "x"}, version: 1},
			{name: "empty agent", agent: "", id: cred.ID, token: gatewaycred.OAuthToken{Access: "x"}, version: 1},
			{name: "zero version", agent: agent, id: cred.ID, token: gatewaycred.OAuthToken{Access: "x"}},
			{name: "empty access", agent: agent, id: cred.ID, version: 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := s.UpdateOAuth(ctx, tc.agent, tc.id, tc.token, tc.version); !errors.Is(err, gatewaycred.ErrInvalidArgument) {
					t.Fatalf("UpdateOAuth: %v", err)
				}
			})
		}
		for _, tc := range []struct {
			name    string
			agent   store.AccountID
			id      string
			version int64
		}{
			{name: "empty id", agent: agent, version: 1},
			{name: "empty agent", id: cred.ID, version: 1},
			{name: "zero version", agent: agent, id: cred.ID},
		} {
			t.Run("disable "+tc.name, func(t *testing.T) {
				if err := s.Disable(ctx, tc.agent, tc.id, "x", tc.version); !errors.Is(err, gatewaycred.ErrInvalidArgument) {
					t.Fatalf("Disable: %v", err)
				}
			})
		}
	}
}

func apiKeyUpdate(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, _ := h.New(t)
		ctx := h.Ctx(t, 0)
		agent, owner := h.Agent(t, ctx, "owner")
		cred := create(t, ctx, s, gatewaycred.NewAPIKeyCredential(gatewaycred.Credential{Provider: "anthropic", Scope: gatewaycred.ScopeOwn, OwnerUserID: owner}, "secret"))
		if _, err := s.UpdateOAuth(ctx, agent, cred.ID, gatewaycred.OAuthToken{Access: replacementAccessToken}, cred.Version); !errors.Is(err, gatewaycred.ErrFailedPrecondition) {
			t.Fatalf("UpdateOAuth api key: %v", err)
		}
	}
}

func oauthMerge(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, _ := h.New(t)
		ctx := h.Ctx(t, 0)
		agent, owner := h.Agent(t, ctx, "owner")
		cred := create(t, ctx, s, own("anthropic", owner, gatewaycred.OAuthToken{
			Access: "old-access", Refresh: "old-refresh", ExpiresUnixMs: 55,
			EnterpriseURL: "https://enterprise", ProjectID: "project", Email: "user@example.test",
			AccountID: "account", APIEndpoint: "https://api", OrgID: "org", OrgName: "organization",
			AuthorizedAtUnixMs: 44, Region: "region", InferenceRegion: "inference", ActiveOrganizationID: "active-org",
		}))
		if _, err := s.UpdateOAuth(ctx, agent, cred.ID, gatewaycred.OAuthToken{Access: "new-access"}, cred.Version); err != nil {
			t.Fatalf("UpdateOAuth: %v", err)
		}
		got, err := s.List(ctx, owner, "anthropic")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		tok, ok := got[0].OAuth()
		if !ok || tok.Access != "new-access" || tok.Refresh != "old-refresh" || tok.ExpiresUnixMs != 55 || tok.EnterpriseURL != "https://enterprise" || tok.ProjectID != "project" || tok.Email != "user@example.test" || tok.AccountID != "account" || tok.APIEndpoint != "https://api" || tok.OrgID != "org" || tok.OrgName != "organization" || tok.AuthorizedAtUnixMs != 44 || tok.Region != "region" || tok.InferenceRegion != "inference" || tok.ActiveOrganizationID != "active-org" {
			t.Fatalf("merged OAuth token = %#v", tok)
		}
	}
}

func disable(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, pool := h.New(t)
		ctx := h.Ctx(t, 0)
		agent, owner := h.Agent(t, ctx, "owner")
		cred := create(t, ctx, s, own("anthropic", owner, gatewaycred.OAuthToken{Access: oldAccessToken}))
		if err := s.Disable(ctx, agent, cred.ID, "operator", cred.Version); err != nil {
			t.Fatalf("Disable: %v", err)
		}
		got, err := pool.Pool(ctx, agent)
		if err != nil {
			t.Fatalf("Pool after Disable: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("Pool after Disable = %#v, want empty", got)
		}
		if err := s.Disable(ctx, agent, cred.ID, "operator", cred.Version+1); !errors.Is(err, gatewaycred.ErrNotFound) {
			t.Fatalf("second Disable: %v", err)
		}
	}
}

func tenantsAreIsolated(h Harness) func(*testing.T) {
	return func(t *testing.T) {
		s, pool := h.New(t)
		ctxA, ctxB := h.Ctx(t, 0), h.Ctx(t, 1)
		agentA, ownerA := h.Agent(t, ctxA, "same-owner")
		agentB, ownerB := h.Agent(t, ctxB, "same-owner")
		create(t, ctxA, s, own("anthropic", ownerA, gatewaycred.OAuthToken{Access: "tenant-a"}))
		create(t, ctxB, s, own("anthropic", ownerB, gatewaycred.OAuthToken{Access: "tenant-b"}))
		create(t, ctxA, s, shared("openai", gatewaycred.OAuthToken{Access: "tenant-a-shared"}))
		credentialB := create(t, ctxB, s, shared("openai", gatewaycred.OAuthToken{Access: "tenant-b-shared"}))
		gotA, err := pool.Pool(ctxA, agentA)
		if err != nil {
			t.Fatalf("Pool tenant A: %v", err)
		}
		gotB, err := pool.Pool(ctxB, agentB)
		if err != nil {
			t.Fatalf("Pool tenant B: %v", err)
		}
		if len(gotA) != 2 || len(gotB) != 2 {
			t.Fatalf("tenant pools: A=%#v B=%#v", gotA, gotB)
		}
		accesses := func(credentials []gatewaycred.Credential) map[string]bool {
			values := make(map[string]bool, len(credentials))
			for _, credential := range credentials {
				token, ok := credential.OAuth()
				if !ok {
					t.Fatalf("pool returned non-OAuth credential: %v", credential)
				}
				values[token.Access] = true
			}
			return values
		}
		if values := accesses(gotA); !values["tenant-a"] || !values["tenant-a-shared"] || values["tenant-b"] || values["tenant-b-shared"] {
			t.Fatalf("tenant A pool crossed tenants: %v", values)
		}
		if values := accesses(gotB); !values["tenant-b"] || !values["tenant-b-shared"] || values["tenant-a"] || values["tenant-a-shared"] {
			t.Fatalf("tenant B pool crossed tenants: %v", values)
		}
		if _, err := s.UpdateOAuth(ctxA, agentA, credentialB.ID, gatewaycred.OAuthToken{Access: "cross-tenant"}, credentialB.Version); !errors.Is(err, gatewaycred.ErrNotFound) {
			t.Fatalf("UpdateOAuth tenant A with tenant B credential: %v", err)
		}
		if err := s.Disable(ctxA, agentA, credentialB.ID, "cross-tenant", credentialB.Version); !errors.Is(err, gatewaycred.ErrNotFound) {
			t.Fatalf("Disable tenant A with tenant B credential: %v", err)
		}
	}
}

func create(t *testing.T, ctx context.Context, s gatewaycred.CredentialStore, c gatewaycred.Credential) gatewaycred.Credential {
	t.Helper()
	created, err := s.Create(ctx, c)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == "" || created.Version != 1 {
		t.Fatalf("Create returned id=%q version=%d", created.ID, created.Version)
	}
	return created
}

func own(provider string, owner store.AccountID, tok gatewaycred.OAuthToken) gatewaycred.Credential {
	return gatewaycred.NewOAuthCredential(gatewaycred.Credential{Provider: provider, Scope: gatewaycred.ScopeOwn, OwnerUserID: owner}, tok)
}

func shared(provider string, tok gatewaycred.OAuthToken) gatewaycred.Credential {
	return gatewaycred.NewOAuthCredential(gatewaycred.Credential{Provider: provider, Scope: gatewaycred.ScopeShared}, tok)
}
