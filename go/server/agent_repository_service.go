//go:build unix

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"connectrpc.com/connect"

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
	signal agentSecretsSignaler
}

func newAgentRepositoryService(st *store.Store, broker *gitCredentialBroker, signal agentSecretsSignaler) *agentRepositoryService {
	svc := &agentRepositoryService{store: st, signal: signal}
	if broker != nil {
		svc.host = broker.host
	}
	return svc
}

var _ compassv1connect.AgentRepositoryServiceHandler = (*agentRepositoryService)(nil)

// agentRepository accepts one exact org/name and stores it in lowercase.
func agentRepository(raw string) (string, error) {
	repo := strings.ToLower(raw)
	org, name, ok := strings.Cut(repo, "/")
	if !ok || org == "" || name == "" || strings.Contains(name, "/") || repo == "*" {
		return "", fmt.Errorf("%w: repository must be an exact org/name", store.ErrInvalidArgument)
	}
	return repo, nil
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
		return store.Account{}, connect.NewError(connect.CodeInternal, fmt.Errorf("resolving agent handle %q: %w", raw, err))
	}
	if agent.Agent == nil || agent.Agent.OwnerUserID != caller {
		return store.Account{}, handleNotFound(raw)
	}
	return agent, nil
}

func (s *agentRepositoryService) GrantAgentRepository(
	ctx context.Context,
	req *connect.Request[compassv1.GrantAgentRepositoryRequest],
) (*connect.Response[compassv1.GrantAgentRepositoryResponse], error) {
	caller, err := s.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireHost(); err != nil {
		return nil, err
	}
	if req.Msg.GetAgentHandle() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("agent_handle is required"))
	}
	repo, err := agentRepository(req.Msg.GetRepository())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	agent, err := resolveOwnedAgent(ctx, s.store, caller, req.Msg.GetAgentHandle())
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
		s.signalSecretsVersion(ctx, agent.ID)
	}
	return connect.NewResponse(&compassv1.GrantAgentRepositoryResponse{Added: added}), nil
}

func (s *agentRepositoryService) RevokeAgentRepository(
	ctx context.Context,
	req *connect.Request[compassv1.RevokeAgentRepositoryRequest],
) (*connect.Response[compassv1.RevokeAgentRepositoryResponse], error) {
	caller, err := s.requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireHost(); err != nil {
		return nil, err
	}
	if req.Msg.GetAgentHandle() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("agent_handle is required"))
	}
	repo, err := agentRepository(req.Msg.GetRepository())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	agent, err := resolveOwnedAgent(ctx, s.store, caller, req.Msg.GetAgentHandle())
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
		s.signalSecretsVersion(ctx, agent.ID)
	}
	return connect.NewResponse(&compassv1.RevokeAgentRepositoryResponse{Removed: removed}), nil
}

func (s *agentRepositoryService) ListAgentRepositories(
	ctx context.Context,
	req *connect.Request[compassv1.ListAgentRepositoriesRequest],
) (*connect.Response[compassv1.ListAgentRepositoriesResponse], error) {
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
		if req.Msg.GetAgentHandle() != "" {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("agents may list only their own repositories"))
		}
		agent = account
	case account.User != nil:
		if req.Msg.GetAgentHandle() == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("agent_handle is required"))
		}
		agent, err = resolveOwnedAgent(ctx, s.store, caller, req.Msg.GetAgentHandle())
		if err != nil {
			return nil, err
		}
	default:
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("agent repository access is unavailable to this account"))
	}
	repos, err := s.store.ListAgentForgeScopeRepos(ctx, agent.ID, store.ForgeProviderGitHub, s.host)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("listing agent repositories: %w", err))
	}
	return connect.NewResponse(&compassv1.ListAgentRepositoriesResponse{Repositories: repos}), nil
}

func (s *agentRepositoryService) requireCaller(ctx context.Context) (store.AccountID, error) {
	caller, ok := auth.CallerFrom(ctx)
	if !ok {
		return "", connect.NewError(connect.CodeUnauthenticated, errNoCaller)
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
		return "", connect.NewError(connect.CodePermissionDenied, errors.New("agent repository changes are user-only"))
	}
	return caller, nil
}

func (s *agentRepositoryService) callerLookupError(caller store.AccountID, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("caller account %q not found", caller))
	}
	return connect.NewError(connect.CodeInternal, fmt.Errorf("resolving caller account: %w", err))
}

func (s *agentRepositoryService) requireHost() error {
	if s.host == "" {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("GitHub App not configured"))
	}
	return nil
}

func (s *agentRepositoryService) checkOrg(ctx context.Context, agent store.AccountID, repo string) error {
	grants, err := s.store.ListForgeScopeRepos(ctx, agent, store.ForgeProviderGitHub, s.host)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("checking agent credential org: %w", err))
	}
	org, _, _ := strings.Cut(repo, "/")
	mismatch := false
	for _, grant := range grants {
		if grant == "*" {
			return nil
		}
		grantOrg, _, ok := strings.Cut(strings.ToLower(grant), "/")
		if ok && grantOrg != org {
			mismatch = true
		}
	}
	if mismatch {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("repository org differs from the agent's credential org"))
	}
	return nil
}

func (s *agentRepositoryService) mapStoreError(operation string, err error) error {
	if errors.Is(err, store.ErrInvalidArgument) {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewError(connect.CodeInternal, fmt.Errorf("%s: %w", operation, err))
}

func (s *agentRepositoryService) signalSecretsVersion(ctx context.Context, agent store.AccountID) {
	if s.signal == nil {
		return
	}
	if err := s.signal.SignalSecretsVersionFor(ctx, agent); err != nil {
		slog.WarnContext(ctx, "emitting agent repository secrets signal", "agent", agent, "error", err)
	}
}
