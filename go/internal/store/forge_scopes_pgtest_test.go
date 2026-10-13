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
		{name: "wildcard organization", scope: ForgeScope{AccountID: agent.ID, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "*/repo"}},
		{name: "wildcard repository", scope: ForgeScope{AccountID: agent.ID, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "org/*"}},
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

func TestAgentForgeScopeRevokeAndList(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner, err := s.CreateUser(ctx, NewUser{Handle: "revoke-agent-scope-owner", DisplayName: "Owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "revoke-agent-scope-agent", DisplayName: "Agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	ownerScope := ForgeScope{AccountID: owner.ID, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "owner/grant"}
	if err := s.GrantForgeScope(ctx, ownerScope); err != nil {
		t.Fatalf("GrantForgeScope: %v", err)
	}
	agentScope := ForgeScope{AccountID: agent.ID, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "agent/repo"}
	added, err := s.GrantAgentForgeScope(ctx, agentScope)
	if err != nil || !added {
		t.Fatalf("GrantAgentForgeScope = (%t, %v), want (true, nil)", added, err)
	}

	ownRepos, err := s.ListAgentForgeScopeRepos(ctx, agent.ID, ForgeProviderGitHub, "github.com")
	if err != nil || len(ownRepos) != 1 || ownRepos[0] != "agent/repo" {
		t.Fatalf("ListAgentForgeScopeRepos = (%v, %v), want ([agent/repo], nil)", ownRepos, err)
	}
	allRepos, err := s.ListForgeScopeRepos(ctx, agent.ID, ForgeProviderGitHub, "github.com")
	if err != nil || len(allRepos) != 2 || allRepos[0] != "agent/repo" || allRepos[1] != "owner/grant" {
		t.Fatalf("ListForgeScopeRepos = (%v, %v), want ([agent/repo owner/grant], nil)", allRepos, err)
	}

	removed, err := s.RevokeAgentForgeScope(ctx, agentScope)
	if err != nil || !removed {
		t.Fatalf("RevokeAgentForgeScope first = (%t, %v), want (true, nil)", removed, err)
	}
	removed, err = s.RevokeAgentForgeScope(ctx, agentScope)
	if err != nil || removed {
		t.Fatalf("RevokeAgentForgeScope duplicate = (%t, %v), want (false, nil)", removed, err)
	}
	removed, err = s.RevokeAgentForgeScope(ctx, ownerScope)
	if err != nil || removed {
		t.Fatalf("RevokeAgentForgeScope user = (%t, %v), want (false, nil)", removed, err)
	}
	if got, err := s.HasForgeScope(ctx, agent.ID, ForgeProviderGitHub, "github.com", "owner/grant"); err != nil || !got {
		t.Fatalf("owner grant after agent revoke = (%t, %v), want (true, nil)", got, err)
	}

	if _, err := s.GrantAgentForgeScope(ctx, agentScope); err != nil {
		t.Fatalf("GrantAgentForgeScope before tenant check: %v", err)
	}
	tenantB := seedTenant(t, s, "revoke-agent-scope-tenant-b")
	ctxB := WithTenant(ctx, tenantB)
	removed, err = s.RevokeAgentForgeScope(ctxB, agentScope)
	if err != nil || removed {
		t.Fatalf("RevokeAgentForgeScope tenant B = (%t, %v), want (false, nil)", removed, err)
	}
	if repos, err := s.ListAgentForgeScopeRepos(ctx, agent.ID, ForgeProviderGitHub, "github.com"); err != nil || len(repos) != 1 || repos[0] != "agent/repo" {
		t.Fatalf("ListAgentForgeScopeRepos tenant A after tenant B revoke = (%v, %v), want ([agent/repo], nil)", repos, err)
	}
	if repos, err := s.ListAgentForgeScopeRepos(ctxB, agent.ID, ForgeProviderGitHub, "github.com"); err != nil || len(repos) != 0 {
		t.Fatalf("ListAgentForgeScopeRepos tenant B = (%v, %v), want ([], nil)", repos, err)
	}
}

func TestCreateAgentCopiesForgeScopesFromParent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner, err := s.CreateUser(ctx, NewUser{Handle: "copy-agent-scope-owner", DisplayName: "Owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	parent, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "copy-agent-scope-parent", DisplayName: "Parent"})
	if err != nil {
		t.Fatalf("CreateAgent parent: %v", err)
	}
	if err := s.GrantForgeScope(ctx, ForgeScope{
		AccountID: owner.ID, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "owner/not-copied",
	}); err != nil {
		t.Fatalf("GrantForgeScope owner: %v", err)
	}
	parentScope := ForgeScope{AccountID: parent.ID, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "parent/copied"}
	if _, err := s.GrantAgentForgeScope(ctx, parentScope); err != nil {
		t.Fatalf("GrantAgentForgeScope parent: %v", err)
	}
	child, err := s.CreateAgent(ctx, owner.ID, NewAgent{
		Handle: "copy-agent-scope-child", DisplayName: "Child", ParentAgentID: parent.ID,
	})
	if err != nil {
		t.Fatalf("CreateAgent child: %v", err)
	}
	childRepos, err := s.ListAgentForgeScopeRepos(ctx, child.ID, ForgeProviderGitHub, "github.com")
	if err != nil || len(childRepos) != 1 || childRepos[0] != "parent/copied" {
		t.Fatalf("child own repos = (%v, %v), want ([parent/copied], nil)", childRepos, err)
	}
	if got, err := s.HasForgeScope(ctx, child.ID, ForgeProviderGitHub, "github.com", "owner/not-copied"); err != nil || !got {
		t.Fatalf("child inherits owner grant = (%t, %v), want (true, nil)", got, err)
	}
	root, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "copy-agent-scope-root", DisplayName: "Root"})
	if err != nil {
		t.Fatalf("CreateAgent root: %v", err)
	}
	rootRepos, err := s.ListAgentForgeScopeRepos(ctx, root.ID, ForgeProviderGitHub, "github.com")
	if err != nil || len(rootRepos) != 0 {
		t.Fatalf("root own repos = (%v, %v), want ([], nil)", rootRepos, err)
	}
	otherOwner, err := s.CreateUser(ctx, NewUser{Handle: "copy-agent-scope-other-owner", DisplayName: "Other Owner"})
	if err != nil {
		t.Fatalf("CreateUser other owner: %v", err)
	}
	foreignChild, err := s.CreateAgent(ctx, otherOwner.ID, NewAgent{
		Handle: "copy-agent-scope-foreign-child", DisplayName: "Foreign Child", ParentAgentID: parent.ID,
	})
	if err != nil {
		t.Fatalf("CreateAgent with foreign parent: %v", err)
	}
	foreignChildRepos, err := s.ListAgentForgeScopeRepos(ctx, foreignChild.ID, ForgeProviderGitHub, "github.com")
	if err != nil || len(foreignChildRepos) != 0 {
		t.Fatalf("foreign child own repos = (%v, %v), want ([], nil)", foreignChildRepos, err)
	}
}
