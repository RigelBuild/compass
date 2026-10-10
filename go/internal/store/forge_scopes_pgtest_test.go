//go:build pgtest

package store

import (
	"context"
	"errors"
	"testing"
)

func TestForgeScopeGrantInheritanceWildcardAndRevoke(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner, err := s.CreateUser(ctx, NewUser{Handle: "scope-owner", DisplayName: "Scope Owner"})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "scope-agent", DisplayName: "Scope Agent"})
	if err != nil {
		t.Fatal(err)
	}
	otherOwner, err := s.CreateUser(ctx, NewUser{Handle: "other-owner", DisplayName: "Other Owner"})
	if err != nil {
		t.Fatal(err)
	}
	otherAgent, err := s.CreateAgent(ctx, otherOwner.ID, NewAgent{Handle: "other-agent", DisplayName: "Other Agent"})
	if err != nil {
		t.Fatal(err)
	}

	exact := ForgeScope{AccountID: owner.ID, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "Owner/Repo"}
	if err := s.GrantForgeScope(ctx, exact); err != nil {
		t.Fatalf("GrantForgeScope exact: %v", err)
	}
	if err := s.GrantForgeScope(ctx, exact); err != nil {
		t.Fatalf("GrantForgeScope idempotent: %v", err)
	}
	for _, tc := range []struct {
		name     string
		account  AccountID
		provider ForgeProvider
		host     string
		repo     string
		want     bool
	}{
		{name: "owner exact grant", account: owner.ID, provider: ForgeProviderGitHub, host: "github.com", repo: "OWNER/REPO", want: true},
		{name: "agent inherits owner exact grant", account: agent.ID, provider: ForgeProviderGitHub, host: "github.com", repo: "owner/repo", want: true},
		{name: "other repo denied", account: agent.ID, provider: ForgeProviderGitHub, host: "github.com", repo: "owner/another", want: false},
		{name: "other owner denied", account: otherAgent.ID, provider: ForgeProviderGitHub, host: "github.com", repo: "owner/repo", want: false},
		{name: "other host denied", account: agent.ID, provider: ForgeProviderGitHub, host: "ghe.example", repo: "owner/repo", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.HasForgeScope(ctx, tc.account, tc.provider, tc.host, tc.repo)
			if err != nil {
				t.Fatalf("HasForgeScope: %v", err)
			}
			if got != tc.want {
				t.Fatalf("HasForgeScope = %t, want %t", got, tc.want)
			}
		})
	}

	wildcard := ForgeScope{AccountID: owner.ID, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "*"}
	if err := s.GrantForgeScope(ctx, wildcard); err != nil {
		t.Fatalf("GrantForgeScope wildcard: %v", err)
	}
	if allowed, err := s.HasForgeScope(ctx, agent.ID, ForgeProviderGitHub, "github.com", "new/repo"); err != nil || !allowed {
		t.Fatalf("HasForgeScope wildcard = (%t, %v), want (true, nil)", allowed, err)
	}
	repos, err := s.ListForgeScopeRepos(ctx, agent.ID, ForgeProviderGitHub, "github.com")
	if err != nil {
		t.Fatalf("ListForgeScopeRepos: %v", err)
	}
	if len(repos) != 2 || repos[0] != "*" || repos[1] != "owner/repo" {
		t.Fatalf("ListForgeScopeRepos = %v, want [* owner/repo]", repos)
	}
	if err := s.RevokeForgeScope(ctx, exact); err != nil {
		t.Fatalf("RevokeForgeScope exact: %v", err)
	}
	if allowed, err := s.HasForgeScope(ctx, agent.ID, ForgeProviderGitHub, "github.com", "owner/repo"); err != nil || !allowed {
		t.Fatalf("wildcard after exact revoke = (%t, %v), want (true, nil)", allowed, err)
	}
	if err := s.RevokeForgeScope(ctx, wildcard); err != nil {
		t.Fatalf("RevokeForgeScope wildcard: %v", err)
	}
	if allowed, err := s.HasForgeScope(ctx, agent.ID, ForgeProviderGitHub, "github.com", "owner/repo"); err != nil || allowed {
		t.Fatalf("HasForgeScope after revoke = (%t, %v), want (false, nil)", allowed, err)
	}
}

func TestForgeScopeLinearKeysRemainVerbatim(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner, err := s.CreateUser(ctx, NewUser{Handle: "linear-scope-owner", DisplayName: "Linear Scope Owner"})
	if err != nil {
		t.Fatal(err)
	}
	scope := ForgeScope{AccountID: owner.ID, Provider: ForgeProviderLinear, Host: "linear.app", Repo: "Team-Key"}
	if err := s.GrantForgeScope(ctx, scope); err != nil {
		t.Fatalf("GrantForgeScope: %v", err)
	}
	for _, tc := range []struct {
		name string
		repo string
		want bool
	}{
		{name: "exact team key", repo: "Team-Key", want: true},
		{name: "case is significant", repo: "team-key", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.HasForgeScope(ctx, owner.ID, ForgeProviderLinear, "linear.app", tc.repo)
			if err != nil {
				t.Fatalf("HasForgeScope: %v", err)
			}
			if got != tc.want {
				t.Fatalf("HasForgeScope = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestForgeScopeRejectsAgentGrantAndSeparatesTenants(t *testing.T) {
	ctxA := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "scope-rls")
	if err := s.GrantForgeScope(ctxA, ForgeScope{AccountID: agent, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("GrantForgeScope(agent) = %v, want ErrInvalidArgument", err)
	}

	tenantB := seedTenant(t, s, "scope-tenant-b")
	ctxB := WithTenant(context.Background(), tenantB)
	// A tenant-B caller cannot grant to a tenant-A user, even though the FK sees it.
	if err := s.GrantForgeScope(ctxB, ForgeScope{AccountID: owner, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("cross-tenant GrantForgeScope = %v, want ErrInvalidArgument", err)
	}
	if got, err := s.HasForgeScope(ctxB, agent, ForgeProviderGitHub, "github.com", "owner/repo"); err != nil || got {
		t.Fatalf("cross-tenant HasForgeScope = (%t, %v), want (false, nil)", got, err)
	}
	if repos, err := s.ListForgeScopeRepos(ctxB, agent, ForgeProviderGitHub, "github.com"); err != nil || len(repos) != 0 {
		t.Fatalf("cross-tenant ListForgeScopeRepos = (%v, %v), want ([], nil)", repos, err)
	}
	if err := s.GrantForgeScope(ctxA, ForgeScope{AccountID: owner, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo"}); err != nil {
		t.Fatalf("GrantForgeScope owner: %v", err)
	}
	if got, err := s.HasForgeScope(ctxA, agent, ForgeProviderGitHub, "github.com", "owner/repo"); err != nil || !got {
		t.Fatalf("same-tenant HasForgeScope = (%t, %v), want (true, nil)", got, err)
	}
	if got, err := s.HasForgeScope(ctxB, agent, ForgeProviderGitHub, "github.com", "owner/repo"); err != nil || got {
		t.Fatalf("cross-tenant HasForgeScope after grant = (%t, %v), want (false, nil)", got, err)
	}
}

func TestForgeScopeValidationAndNormalization(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope ForgeScope
		want  ForgeScope
		bad   bool
	}{
		{name: "GitHub lowercases repo", scope: ForgeScope{AccountID: "u", Provider: ForgeProviderGitHub, Host: "github.com", Repo: "Owner/Repo"}, want: ForgeScope{AccountID: "u", Provider: ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo"}},
		{name: "wildcard preserved", scope: ForgeScope{AccountID: "u", Provider: ForgeProviderGitHub, Host: "github.com", Repo: "*"}, want: ForgeScope{AccountID: "u", Provider: ForgeProviderGitHub, Host: "github.com", Repo: "*"}},
		{name: "Linear key preserved", scope: ForgeScope{AccountID: "u", Provider: ForgeProviderLinear, Host: "linear.app", Repo: "Team-Key"}, want: ForgeScope{AccountID: "u", Provider: ForgeProviderLinear, Host: "linear.app", Repo: "Team-Key"}},
		{name: "empty account invalid", scope: ForgeScope{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b"}, bad: true},
		{name: "empty repo invalid", scope: ForgeScope{AccountID: "u", Provider: ForgeProviderGitHub, Host: "github.com"}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.scope.normalized()
			if tc.bad {
				if !errors.Is(err, ErrInvalidArgument) {
					t.Fatalf("normalized error = %v, want ErrInvalidArgument", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("normalized = (%+v, %v), want (%+v, nil)", got, err, tc.want)
			}
		})
	}
}
func TestGrantAgentForgeScope(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner, err := s.CreateUser(ctx, NewUser{Handle: "agent-scope-owner", DisplayName: "Owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "agent-scope-agent", DisplayName: "Agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	sibling, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "agent-scope-sibling", DisplayName: "Sibling"})
	if err != nil {
		t.Fatalf("CreateAgent sibling: %v", err)
	}
	if err := s.GrantForgeScope(ctx, ForgeScope{
		AccountID: owner.ID, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "Owner/Base",
	}); err != nil {
		t.Fatalf("GrantForgeScope owner: %v", err)
	}

	scope := ForgeScope{
		AccountID: agent.ID, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "Workstream/Repo",
	}
	added, err := s.GrantAgentForgeScope(ctx, scope)
	if err != nil || !added {
		t.Fatalf("GrantAgentForgeScope first = (%t, %v), want (true, nil)", added, err)
	}
	added, err = s.GrantAgentForgeScope(ctx, scope)
	if err != nil || added {
		t.Fatalf("GrantAgentForgeScope duplicate = (%t, %v), want (false, nil)", added, err)
	}

	for _, tc := range []struct {
		name    string
		account AccountID
		want    bool
	}{
		{name: "agent", account: agent.ID, want: true},
		{name: "owner", account: owner.ID},
		{name: "sibling", account: sibling.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.HasForgeScope(ctx, tc.account, ForgeProviderGitHub, "github.com", "workstream/repo")
			if err != nil || got != tc.want {
				t.Fatalf("HasForgeScope = (%t, %v), want (%t, nil)", got, err, tc.want)
			}
		})
	}
	repos, err := s.ListForgeScopeRepos(ctx, agent.ID, ForgeProviderGitHub, "github.com")
	if err != nil {
		t.Fatalf("ListForgeScopeRepos(agent): %v", err)
	}
	if len(repos) != 2 || repos[0] != "owner/base" || repos[1] != "workstream/repo" {
		t.Fatalf("ListForgeScopeRepos(agent) = %v, want [owner/base workstream/repo]", repos)
	}

	for _, tc := range []struct {
		name  string
		scope ForgeScope
	}{
		{name: "wildcard", scope: ForgeScope{AccountID: agent.ID, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "*"}},
		{name: "user account", scope: ForgeScope{AccountID: owner.ID, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "owner/repo"}},
		{name: "missing slash", scope: ForgeScope{AccountID: agent.ID, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "repo"}},
		{name: "empty organization", scope: ForgeScope{AccountID: agent.ID, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "/repo"}},
		{name: "empty repository", scope: ForgeScope{AccountID: agent.ID, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "org/"}},
		{name: "multiple slashes", scope: ForgeScope{AccountID: agent.ID, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "org/repo/extra"}},
	} {
		t.Run("reject "+tc.name, func(t *testing.T) {
			if _, err := s.GrantAgentForgeScope(ctx, tc.scope); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("GrantAgentForgeScope error = %v, want ErrInvalidArgument", err)
			}
		})
	}

	if err := s.RevokeForgeScope(ctx, scope); err != nil {
		t.Fatalf("RevokeForgeScope agent row: %v", err)
	}
	if got, err := s.HasForgeScope(ctx, agent.ID, ForgeProviderGitHub, "github.com", "workstream/repo"); err != nil || got {
		t.Fatalf("HasForgeScope after agent-row revoke = (%t, %v), want (false, nil)", got, err)
	}

	tenantB := seedTenant(t, s, "agent-scope-tenant-b")
	ctxB := WithTenant(ctx, tenantB)
	if _, err := s.GrantAgentForgeScope(ctxB, scope); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("GrantAgentForgeScope from tenant B = %v, want ErrInvalidArgument", err)
	}
	if repos, err := s.ListForgeScopeRepos(ctxB, agent.ID, ForgeProviderGitHub, "github.com"); err != nil || len(repos) != 0 {
		t.Fatalf("ListForgeScopeRepos from tenant B = (%v, %v), want ([], nil)", repos, err)
	}
}
