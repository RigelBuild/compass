//go:build unix

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/store"
)

type installationResolverFunc func(context.Context, string) (int64, error)

func (f installationResolverFunc) InstallationFor(ctx context.Context, owner string) (int64, error) {
	return f(ctx, owner)
}

// DefaultInstallation is the "owner" installation in these tests.
func (f installationResolverFunc) DefaultInstallation() int64 { return 159353126 }

type agentRepositoryScopeStore struct {
	grants []string
}

func (s agentRepositoryScopeStore) ListForgeScopeRepos(context.Context, store.AccountID, store.ForgeProvider, string) ([]string, error) {
	return s.grants, nil
}

func TestAgentRepositoryServiceCheckOrgInstallations(t *testing.T) {
	resolver := installationResolverFunc(func(_ context.Context, owner string) (int64, error) {
		switch strings.ToLower(owner) {
		case "owner":
			return 159353126, nil
		case "another":
			return 168669446, nil
		default:
			return 0, fmt.Errorf("owner %q: %w", owner, forge.ErrNoInstallation)
		}
	})
	svc := &agentRepositoryService{
		scopes: agentRepositoryScopeStore{grants: []string{"owner/base"}},
		host:   "github.com", installations: resolver,
	}

	if err := svc.checkOrg(context.Background(), "agent", "owner/workstream"); err != nil {
		t.Fatalf("first installed owner: %v", err)
	}
	if err := svc.checkOrg(context.Background(), "agent", "another/repository"); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "repository org differs") {
		t.Fatalf("second installed owner error = %v, want FailedPrecondition for grant-owner mismatch", err)
	}
	svc.scopes = agentRepositoryScopeStore{grants: []string{"*"}}
	if err := svc.checkOrg(context.Background(), "agent", "missing/repository"); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), `no installation for owner "missing"`) {
		t.Fatalf("uninstalled wildcard owner error = %v, want FailedPrecondition naming owner", err)
	}
	svc.scopes = agentRepositoryScopeStore{grants: []string{"*"}}
	if err := svc.checkOrg(context.Background(), "agent", "owner/wildcard-repository"); err != nil {
		t.Fatalf("wildcard grant with the default installation owner: %v", err)
	}
	if err := svc.checkOrg(context.Background(), "agent", "another/wildcard-repository"); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "wildcard grant covers only the default") {
		t.Fatalf("wildcard grant with a non-default installed owner = %v, want FailedPrecondition", err)
	}
	if err := svc.checkOrg(context.Background(), "agent", "missing/wildcard-repository"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("wildcard grant for uninstalled owner = %v, want FailedPrecondition", err)
	}
}
func TestAgentRepositoryServiceCheckOrgFirstGrantRequiresInstallation(t *testing.T) {
	resolver := installationResolverFunc(func(context.Context, string) (int64, error) {
		return 0, fmt.Errorf("%w", forge.ErrNoInstallation)
	})
	svc := &agentRepositoryService{
		scopes: agentRepositoryScopeStore{}, host: "github.com", installations: resolver,
	}

	err := svc.checkOrg(context.Background(), "agent", "uninstalled/repository")
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), `no installation for owner "uninstalled"`) {
		t.Fatalf("first uninstalled owner error = %v, want FailedPrecondition naming owner", err)
	}
}

func TestAgentRepositoryServiceCheckOrgCredentialError(t *testing.T) {
	svc := &agentRepositoryService{
		scopes: agentRepositoryScopeStore{}, host: "github.com",
		installations: installationResolverFunc(func(context.Context, string) (int64, error) {
			return 0, &forge.StatusError{Status: http.StatusUnauthorized}
		}),
	}
	err := svc.checkOrg(context.Background(), "agent", "owner/repository")
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("credential error code = %v (%v), want Internal", got, err)
	}
}

func TestAgentRepositoryServiceCheckOrgResolverTransportError(t *testing.T) {
	svc := &agentRepositoryService{
		scopes: agentRepositoryScopeStore{}, host: "github.com",
		installations: installationResolverFunc(func(context.Context, string) (int64, error) {
			return 0, errors.New("transport failed")
		}),
	}

	err := svc.checkOrg(context.Background(), "agent", "owner/repository")
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("resolver transport error code = %v (%v), want Unavailable", got, err)
	}
}
