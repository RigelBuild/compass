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
)

type installationResolverFunc func(context.Context, string) (int64, error)

func (f installationResolverFunc) InstallationFor(ctx context.Context, owner string) (int64, error) {
	return f(ctx, owner)
}

func TestAgentRepositoryServiceCheckOrgAllowsMixedOwners(t *testing.T) {
	resolver := installationResolverFunc(func(_ context.Context, owner string) (int64, error) {
		switch strings.ToLower(owner) {
		case "owner-a":
			return 159353126, nil
		case "owner-b":
			return 168669446, nil
		default:
			return 0, fmt.Errorf("owner %q: %w", owner, forge.ErrNoInstallation)
		}
	})
	svc := &agentRepositoryService{host: "github.com", installations: resolver}

	if err := svc.checkOrg(context.Background(), "agent", "owner-b/two"); err != nil {
		t.Fatalf("grant under second installed owner: %v", err)
	}
}

func TestAgentRepositoryServiceCheckOrgRequiresPrimaryInstallation(t *testing.T) {
	svc := &agentRepositoryService{
		host: "github.com",
		installations: installationResolverFunc(func(context.Context, string) (int64, error) {
			return 0, fmt.Errorf("%w", forge.ErrNoInstallation)
		}),
	}

	err := svc.checkOrg(context.Background(), "agent", "owner-b/two")
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), `GitHub App has no installation for owner "owner-b"`) {
		t.Fatalf("missing primary installation error = %v, want FailedPrecondition naming GitHub App and owner-b", err)
	}
}

func TestAgentRepositoryServiceCheckOrgRequiresReviewerInstallation(t *testing.T) {
	svc := &agentRepositoryService{
		host: "github.com",
		installations: installationResolverFunc(func(context.Context, string) (int64, error) {
			return 168669446, nil
		}),
		reviewerInstallations: installationResolverFunc(func(context.Context, string) (int64, error) {
			return 0, fmt.Errorf("%w", forge.ErrNoInstallation)
		}),
	}

	err := svc.checkOrg(context.Background(), "agent", "owner-b/two")
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "reviewer GitHub App") {
		t.Fatalf("missing reviewer installation error = %v, want FailedPrecondition naming reviewer GitHub App", err)
	}
}

func TestAgentRepositoryServiceCheckOrgWildcardAllowsMixedOwner(t *testing.T) {
	svc := &agentRepositoryService{
		host: "github.com",
		installations: installationResolverFunc(func(context.Context, string) (int64, error) {
			return 168669446, nil
		}),
	}

	if err := svc.checkOrg(context.Background(), "agent", "owner-b/two"); err != nil {
		t.Fatalf("wildcard grant under another installed owner: %v", err)
	}
}

func TestAgentRepositoryServiceCheckOrgDoesNotRequireUnconfiguredReviewer(t *testing.T) {
	svc := &agentRepositoryService{
		host: "github.com",
		installations: installationResolverFunc(func(context.Context, string) (int64, error) {
			return 159353126, nil
		}),
	}
	if err := svc.checkOrg(context.Background(), "agent", "owner/repository"); err != nil {
		t.Fatalf("grant with unconfigured reviewer App: %v", err)
	}
}

func TestAgentRepositoryServiceCheckOrgFirstGrantRequiresInstallation(t *testing.T) {
	resolver := installationResolverFunc(func(context.Context, string) (int64, error) {
		return 0, fmt.Errorf("%w", forge.ErrNoInstallation)
	})
	svc := &agentRepositoryService{host: "github.com", installations: resolver}

	err := svc.checkOrg(context.Background(), "agent", "uninstalled/repository")
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), `no installation for owner "uninstalled"`) {
		t.Fatalf("first uninstalled owner error = %v, want FailedPrecondition naming owner", err)
	}
}

func TestAgentRepositoryServiceCheckOrgCredentialError(t *testing.T) {
	svc := &agentRepositoryService{
		host: "github.com",
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
		host: "github.com",
		installations: installationResolverFunc(func(context.Context, string) (int64, error) {
			return 0, errors.New("transport failed")
		}),
	}

	err := svc.checkOrg(context.Background(), "agent", "owner/repository")
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("resolver transport error code = %v (%v), want Unavailable", got, err)
	}
}
