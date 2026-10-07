package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// ForgeScope grants a user account access to one repo or an entire provider host.
type ForgeScope struct {
	AccountID AccountID
	Provider  ForgeProvider
	Host      string
	Repo      string
}

func (scope ForgeScope) normalized() (ForgeScope, error) {
	if scope.AccountID == "" {
		return ForgeScope{}, fmt.Errorf("%w: scope account id is required", ErrInvalidArgument)
	}
	if scope.Provider == ForgeProviderUnspecified {
		return ForgeScope{}, fmt.Errorf("%w: forge provider is required", ErrInvalidArgument)
	}
	if scope.Host == "" {
		return ForgeScope{}, fmt.Errorf("%w: forge host is required", ErrInvalidArgument)
	}
	if scope.Repo == "" {
		return ForgeScope{}, fmt.Errorf("%w: repo is required", ErrInvalidArgument)
	}
	if scope.Provider == ForgeProviderGitHub && scope.Repo != "*" {
		scope.Repo = strings.ToLower(scope.Repo)
	}
	return scope, nil
}

// GrantForgeScope adds a user-owned grant. Agents inherit grants from their owner.
func (s *Store) GrantForgeScope(ctx context.Context, scope ForgeScope) error {
	scope, err := scope.normalized()
	if err != nil {
		return err
	}
	if err := s.q.GrantForgeScope(ctx, db.GrantForgeScopeParams{
		AccountID:     string(scope.AccountID),
		ForgeProvider: int16(scope.Provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum, always within int16.
		ForgeHost:     scope.Host,
		Repo:          scope.Repo,
	}); err != nil {
		if pgErrIs(err, pgForeignKeyViolation) {
			return fmt.Errorf("%w: scope account %q is not a user", ErrInvalidArgument, scope.AccountID)
		}
		return fmt.Errorf("store: grant forge scope: %w", err)
	}
	return nil
}

// RevokeForgeScope removes one user grant; a missing grant is a no-op.
func (s *Store) RevokeForgeScope(ctx context.Context, scope ForgeScope) error {
	scope, err := scope.normalized()
	if err != nil {
		return err
	}
	if err := s.q.RevokeForgeScope(ctx, db.RevokeForgeScopeParams{
		AccountID:     string(scope.AccountID),
		ForgeProvider: int16(scope.Provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum, always within int16.
		ForgeHost:     scope.Host,
		Repo:          scope.Repo,
	}); err != nil {
		return fmt.Errorf("store: revoke forge scope: %w", err)
	}
	return nil
}

// HasForgeScope checks an exact repo or provider-host wildcard for an account and its owner.
func (s *Store) HasForgeScope(ctx context.Context, accountID AccountID, provider ForgeProvider, host, repo string) (bool, error) {
	scope, err := (ForgeScope{AccountID: accountID, Provider: provider, Host: host, Repo: repo}).normalized()
	if err != nil {
		return false, err
	}
	allowed, err := s.q.HasForgeScope(ctx, db.HasForgeScopeParams{
		AccountID:     string(scope.AccountID),
		ForgeProvider: int16(scope.Provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum, always within int16.
		ForgeHost:     scope.Host,
		Repo:          scope.Repo,
	})
	if err != nil {
		return false, fmt.Errorf("store: check forge scope: %w", err)
	}
	return allowed, nil
}

// ListForgeScopeRepos returns the exact repo grants and wildcard for an account and its owner.
func (s *Store) ListForgeScopeRepos(ctx context.Context, accountID AccountID, provider ForgeProvider, host string) ([]string, error) {
	if accountID == "" {
		return nil, fmt.Errorf("%w: scope account id is required", ErrInvalidArgument)
	}
	if provider == ForgeProviderUnspecified {
		return nil, fmt.Errorf("%w: forge provider is required", ErrInvalidArgument)
	}
	if host == "" {
		return nil, fmt.Errorf("%w: forge host is required", ErrInvalidArgument)
	}
	repos, err := s.q.ListForgeScopeRepos(ctx, db.ListForgeScopeReposParams{
		AccountID:     string(accountID),
		ForgeProvider: int16(provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum, always within int16.
		ForgeHost:     host,
	})
	if err != nil {
		return nil, fmt.Errorf("store: list forge scope repos: %w", err)
	}
	return repos, nil
}
