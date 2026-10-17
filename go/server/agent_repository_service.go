//go:build unix

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"connectrpc.com/connect/v2"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/store"
)

// agentSecretsSignaler narrows the hub dependency to one account refresh.
type agentSecretsSignaler interface {
	SignalSecretsVersionFor(ctx context.Context, account store.AccountID) error
}

type agentRepositoryService struct {
	store  *store.Store
	host   string
	broker *gitCredentialBroker
	signal agentSecretsSignaler
}

func newAgentRepositoryService(st *store.Store, broker *gitCredentialBroker, signal agentSecretsSignaler) *agentRepositoryService {
	svc := &agentRepositoryService{store: st, broker: broker, signal: signal}
	if broker != nil {
		svc.host = broker.host
	}
	return svc
}

var _ compassv1connect.AgentRepositoryServiceHandler = (*agentRepositoryService)(nil)

// agentRepository accepts one exact GitHub org/name and stores it in lowercase.
func agentRepository(raw string) (string, error) {
	repo := strings.ToLower(raw)
	org, name, ok := strings.Cut(repo, "/")
	if !ok || org == "" || name == "" || strings.Contains(name, "/") || !githubOwnerPart(org) || !githubRepositoryPart(name) {
		return "", fmt.Errorf("%w: repository must be an exact GitHub org/name", store.ErrInvalidArgument)
	}
	return repo, nil
}

// githubOwnerPart follows GitHub's login rule: 1-39 alphanumerics or hyphens,
// not starting with a hyphen.
func githubOwnerPart(part string) bool {
	if len(part) > 39 || part[0] == '-' {
		return false
	}
	for _, c := range part {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			continue
		}
		return false
	}
	return true
}

// githubRepositoryPart follows GitHub's repository rule: up to 100 of
// [a-z0-9._-], never "." or "..".
func githubRepositoryPart(part string) bool {
	if len(part) > 100 || part == "." || part == ".." {
		return false
	}
	for _, c := range part {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			continue
		}
		return false
	}
	return true
}

// resolveOwnedAgent resolves a caller-owned agent and conceals every miss.
func resolveOwnedAgent(ctx context.Context, st *store.Store, caller store.AccountID, raw string) (store.Account, error) {
	qh := store.ParseQualifiedHandle(raw)
	var agent store.Account
	var err error
	if qh.Qualified() {
		agent, err = resolveQualifiedAgent(ctx, st, raw)
	} else {
		agent, err = st.AgentByHandle(ctx, caller, raw)
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidArgument) || connect.CodeOf(err) == connect.CodeNotFound {
			return store.Account{}, handleNotFound(raw)
		}
		wrappedErr := fmt.Errorf("resolving agent handle %q: %w", raw, err)
		return store.Account{}, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}
	if agent.Agent == nil || agent.Agent.OwnerUserID != caller {
		return store.Account{}, handleNotFound(raw)
	}
	return agent, nil
}

func (s *agentRepositoryService) GrantAgentRepository(
	ctx context.Context,
	req *compassv1.GrantAgentRepositoryRequest,
) (*compassv1.GrantAgentRepositoryResponse, error) {
	caller, err := s.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireHost(); err != nil {
		return nil, err
	}
	if req.GetAgentHandle() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "agent_handle is required")
	}
	repo, err := agentRepository(req.GetRepository())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	agent, err := resolveOwnedAgent(ctx, s.store, caller, req.GetAgentHandle())
	if err != nil {
		return nil, err
	}
	if err := s.checkOrg(ctx, agent.ID, repo); err != nil {
		return nil, err
	}
	added, err := s.store.GrantAgentForgeScope(ctx, store.ForgeScope{
		AccountID: agent.ID, Provider: store.ForgeProviderGitHub, Host: s.host, Repo: repo,
	})
	if err != nil {
		return nil, s.mapStoreError("granting agent repository", err)
	}
	if added {
		if s.broker != nil {
			s.broker.grantsChanged(ctx, agent.ID)
		}
		s.signalSecretsVersion(ctx, agent.ID)
	}
	return &compassv1.GrantAgentRepositoryResponse{Added: added}, nil
}

func (s *agentRepositoryService) RevokeAgentRepository(
	ctx context.Context,
	req *compassv1.RevokeAgentRepositoryRequest,
) (*compassv1.RevokeAgentRepositoryResponse, error) {
	caller, err := s.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireHost(); err != nil {
		return nil, err
	}
	if req.GetAgentHandle() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "agent_handle is required")
	}
	repo, err := agentRepository(req.GetRepository())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	agent, err := resolveOwnedAgent(ctx, s.store, caller, req.GetAgentHandle())
	if err != nil {
		return nil, err
	}
	removed, err := s.store.RevokeAgentForgeScope(ctx, store.ForgeScope{
		AccountID: agent.ID, Provider: store.ForgeProviderGitHub, Host: s.host, Repo: repo,
	})
	if err != nil {
		return nil, s.mapStoreError("revoking agent repository", err)
	}
	if removed {
		if s.broker != nil {
			s.broker.grantsChanged(ctx, agent.ID)
		}
		s.signalSecretsVersion(ctx, agent.ID)
	}
	return &compassv1.RevokeAgentRepositoryResponse{Removed: removed}, nil
}

func (s *agentRepositoryService) ListAgentRepositories(
	ctx context.Context,
	req *compassv1.ListAgentRepositoriesRequest,
) (*compassv1.ListAgentRepositoriesResponse, error) {
	caller, err := s.requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireHost(); err != nil {
		return nil, err
	}
	account, err := s.store.GetAccount(ctx, caller)
	if err != nil {
		return nil, s.callerLookupError(caller, err)
	}
	var agent store.Account
	switch {
	case account.Agent != nil:
		if req.GetAgentHandle() != "" {
			return nil, connect.NewError(connect.CodePermissionDenied, "agents may list only their own repositories")
		}
		agent = account
	case account.User != nil:
		if req.GetAgentHandle() == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, "agent_handle is required")
		}
		agent, err = resolveOwnedAgent(ctx, s.store, caller, req.GetAgentHandle())
		if err != nil {
			return nil, err
		}
	default:
		return nil, connect.NewError(connect.CodePermissionDenied, "agent repository access is unavailable to this account")
	}
	repos, err := s.store.ListAgentForgeScopeRepos(ctx, agent.ID, store.ForgeProviderGitHub, s.host)
	if err != nil {
		wrappedErr := fmt.Errorf("listing agent repositories: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}
	return &compassv1.ListAgentRepositoriesResponse{Repositories: repos}, nil
}

func (s *agentRepositoryService) requireCaller(ctx context.Context) (store.AccountID, error) {
	caller, ok := auth.CallerFrom(ctx)
	if !ok {
		return "", connect.NewError(connect.CodeUnauthenticated, errNoCaller.Error()).WithCause(errNoCaller)
	}
	return caller, nil
}

func (s *agentRepositoryService) requireUser(ctx context.Context) (store.AccountID, error) {
	caller, err := s.requireCaller(ctx)
	if err != nil {
		return "", err
	}
	account, err := s.store.GetAccount(ctx, caller)
	if err != nil {
		return "", s.callerLookupError(caller, err)
	}
	if account.User == nil {
		return "", connect.NewError(connect.CodePermissionDenied, "agent repository changes are user-only")
	}
	return caller, nil
}

func (s *agentRepositoryService) callerLookupError(caller store.AccountID, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return connect.Errorf(connect.CodePermissionDenied, "caller account %q not found", caller)
	}
	wrappedErr := fmt.Errorf("resolving caller account: %w", err)
	return connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
}

func (s *agentRepositoryService) requireHost() error {
	if s.host == "" {
		return connect.NewError(connect.CodeFailedPrecondition, "GitHub App not configured")
	}
	return nil
}

func (s *agentRepositoryService) checkOrg(ctx context.Context, agent store.AccountID, repo string) error {
	grants, err := s.store.ListForgeScopeRepos(ctx, agent, store.ForgeProviderGitHub, s.host)
	if err != nil {
		wrappedErr := fmt.Errorf("checking agent credential org: %w", err)
		return connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}
	org, _, _ := strings.Cut(repo, "/")
	wildcard := false
	mismatch := false
	for _, grant := range grants {
		if grant == "*" {
			wildcard = true
			continue
		}
		grantOrg, _, ok := strings.Cut(strings.ToLower(grant), "/")
		if ok && grantOrg != org {
			mismatch = true
		}
	}
	if mismatch && !wildcard {
		return connect.NewError(connect.CodeFailedPrecondition, "repository org differs from the agent's credential org")
	}
	return nil
}

func (s *agentRepositoryService) mapStoreError(operation string, err error) error {
	if errors.Is(err, store.ErrInvalidArgument) {
		return connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	wrappedErr := fmt.Errorf("%s: %w", operation, err)
	return connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
}

func (s *agentRepositoryService) signalSecretsVersion(ctx context.Context, agent store.AccountID) {
	if s.signal == nil {
		return
	}
	if err := s.signal.SignalSecretsVersionFor(ctx, agent); err != nil {
		slog.WarnContext(ctx, "emitting agent repository secrets signal", "agent", agent, "error", err)
	}
}
