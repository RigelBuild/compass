package server

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/gatewaycred"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/store"
)

type agentTenantResolver interface {
	GatewayAgentTenant(ctx context.Context, agent store.AccountID) (store.TenantID, error)
}

type gatewayCredentialsService struct {
	compassv1internalconnect.UnimplementedGatewayCredentialsHandler
	scope agentTenantResolver
	creds gatewaycred.CredentialStore
	pool  gatewaycred.PoolResolver
	log   *slog.Logger
}

var _ compassv1internalconnect.GatewayCredentialsHandler = (*gatewayCredentialsService)(nil)

func newGatewayCredentialsService(scope agentTenantResolver, creds gatewaycred.CredentialStore, pool gatewaycred.PoolResolver, log *slog.Logger) *gatewayCredentialsService {
	if log == nil {
		log = slog.Default()
	}
	return &gatewayCredentialsService{scope: scope, creds: creds, pool: pool, log: log}
}

func (s *gatewayCredentialsService) ListCredentialPool(ctx context.Context, req *connect.Request[compassv1internal.ListCredentialPoolRequest]) (*connect.Response[compassv1internal.ListCredentialPoolResponse], error) {
	if req == nil || req.Msg == nil || req.Msg.GetAgentAccountId() == "" {
		return nil, gatewayCredentialsError(connect.CodeInvalidArgument, "agent_account_id is required")
	}
	agent := store.AccountID(req.Msg.GetAgentAccountId())
	ctx, err := s.resolveTenant(ctx, agent, "ListCredentialPool")
	if err != nil {
		return nil, err
	}
	credentials, err := s.pool.Pool(ctx, agent)
	if err != nil {
		return nil, s.mapStoreError(ctx, "listing credential pool", err)
	}
	provider := req.Msg.GetProvider()
	out := make([]*compassv1internal.GatewayCredential, 0, len(credentials))
	for _, credential := range credentials {
		if provider != "" && credential.Provider != provider {
			continue
		}
		out = append(out, gatewayCredentialToProto(credential))
	}
	return connect.NewResponse(&compassv1internal.ListCredentialPoolResponse{Credentials: out}), nil
}

func (s *gatewayCredentialsService) UpdateCredentialOAuth(ctx context.Context, req *connect.Request[compassv1internal.UpdateCredentialOAuthRequest]) (*connect.Response[compassv1internal.UpdateCredentialOAuthResponse], error) {
	if req == nil || req.Msg == nil || req.Msg.GetAgentAccountId() == "" || req.Msg.GetId() == "" || req.Msg.GetExpectedVersion() == 0 || req.Msg.GetToken() == nil || req.Msg.GetToken().GetAccess() == "" {
		return nil, gatewayCredentialsError(connect.CodeInvalidArgument, "id, agent_account_id, expected_version, and token access are required")
	}
	agent := store.AccountID(req.Msg.GetAgentAccountId())
	ctx, err := s.resolveTenant(ctx, agent, "UpdateCredentialOAuth")
	if err != nil {
		return nil, err
	}
	version, err := s.creds.UpdateOAuth(ctx, agent, req.Msg.GetId(), gatewayOAuthFromProto(req.Msg.GetToken()), req.Msg.GetExpectedVersion())
	if err != nil {
		return nil, s.mapStoreError(ctx, "updating OAuth credential", err)
	}
	return connect.NewResponse(&compassv1internal.UpdateCredentialOAuthResponse{Version: version}), nil
}

func (s *gatewayCredentialsService) DisableCredential(ctx context.Context, req *connect.Request[compassv1internal.DisableCredentialRequest]) (*connect.Response[compassv1internal.DisableCredentialResponse], error) {
	if req == nil || req.Msg == nil || req.Msg.GetAgentAccountId() == "" || req.Msg.GetId() == "" || req.Msg.GetExpectedVersion() == 0 {
		return nil, gatewayCredentialsError(connect.CodeInvalidArgument, "id, agent_account_id, and expected_version are required")
	}
	agent := store.AccountID(req.Msg.GetAgentAccountId())
	ctx, err := s.resolveTenant(ctx, agent, "DisableCredential")
	if err != nil {
		return nil, err
	}
	if err := s.creds.Disable(ctx, agent, req.Msg.GetId(), req.Msg.GetCause(), req.Msg.GetExpectedVersion()); err != nil {
		return nil, s.mapStoreError(ctx, "disabling credential", err)
	}
	return connect.NewResponse(&compassv1internal.DisableCredentialResponse{}), nil
}

func (s *gatewayCredentialsService) resolveTenant(ctx context.Context, agent store.AccountID, procedure string) (context.Context, error) {
	subj, ok := auth.ServiceSubjectFrom(ctx)
	if !ok || subj.Kind != store.SubjectService || subj.ID != auth.LLMGatewayServiceID {
		return nil, gatewayCredentialsError(connect.CodeUnauthenticated, "unauthenticated")
	}
	tenant, err := s.scope.GatewayAgentTenant(ctx, agent)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errGatewayCredentialNotFound
		}
		s.log.ErrorContext(ctx, "gateway credential agent tenant lookup failed", "operation", procedure, "error", err)
		return nil, gatewayCredentialsError(connect.CodeInternal, "gateway credentials unavailable")
	}
	s.log.InfoContext(ctx, "gateway credential door: cross-tenant agent lookup",
		"service", subj.ID, "agent", agent, "tenant", tenant, "procedure", procedure)
	return store.WithTenant(store.WithoutSystemRole(ctx), tenant), nil
}

func (s *gatewayCredentialsService) mapStoreError(ctx context.Context, operation string, err error) error {
	switch {
	case errors.Is(err, gatewaycred.ErrInvalidArgument):
		return gatewayCredentialsError(connect.CodeInvalidArgument, "invalid credential request")
	case errors.Is(err, gatewaycred.ErrNotFound):
		return errGatewayCredentialNotFound
	case errors.Is(err, gatewaycred.ErrFailedPrecondition):
		return gatewayCredentialsError(connect.CodeFailedPrecondition, "credential cannot be updated")
	case errors.Is(err, gatewaycred.ErrVersionConflict):
		return gatewayCredentialsError(connect.CodeAborted, "credential version conflict")
	default:
		s.log.ErrorContext(ctx, operation, "error", err)
		return gatewayCredentialsError(connect.CodeInternal, "gateway credentials unavailable")
	}
}

// errGatewayCredentialNotFound is shared by an unknown agent and a missing or
// out-of-pool credential, so a caller cannot tell which ids exist.
var errGatewayCredentialNotFound = gatewayCredentialsError(connect.CodeNotFound, "not found")

func gatewayCredentialsError(code connect.Code, message string) error {
	return connect.NewError(code, errors.New(message))
}

func gatewayOAuthFromProto(token *compassv1internal.GatewayOAuthToken) gatewaycred.OAuthToken {
	return gatewaycred.OAuthToken{
		Access: token.GetAccess(), Refresh: token.GetRefresh(), ExpiresUnixMs: token.GetExpiresUnixMs(),
		EnterpriseURL: token.GetEnterpriseUrl(), ProjectID: token.GetProjectId(), Email: token.GetEmail(),
		AccountID: token.GetAccountId(), APIEndpoint: token.GetApiEndpoint(), OrgID: token.GetOrgId(),
		OrgName: token.GetOrgName(), AuthorizedAtUnixMs: token.GetAuthorizedAtUnixMs(), Region: token.GetRegion(),
		InferenceRegion: token.GetInferenceRegion(), ActiveOrganizationID: token.GetActiveOrganizationId(),
	}
}

func gatewayOAuthToProto(token gatewaycred.OAuthToken) *compassv1internal.GatewayOAuthToken {
	return &compassv1internal.GatewayOAuthToken{
		Access: token.Access, Refresh: token.Refresh, ExpiresUnixMs: token.ExpiresUnixMs,
		EnterpriseUrl: token.EnterpriseURL, ProjectId: token.ProjectID, Email: token.Email,
		AccountId: token.AccountID, ApiEndpoint: token.APIEndpoint, OrgId: token.OrgID,
		OrgName: token.OrgName, AuthorizedAtUnixMs: token.AuthorizedAtUnixMs, Region: token.Region,
		InferenceRegion: token.InferenceRegion, ActiveOrganizationId: token.ActiveOrganizationID,
	}
}

func gatewayCredentialToProto(credential gatewaycred.Credential) *compassv1internal.GatewayCredential {
	scope := compassv1internal.GatewayCredentialScope_GATEWAY_CREDENTIAL_SCOPE_UNSPECIFIED
	switch credential.Scope {
	case gatewaycred.ScopeOwn:
		scope = compassv1internal.GatewayCredentialScope_GATEWAY_CREDENTIAL_SCOPE_OWN
	case gatewaycred.ScopeShared:
		scope = compassv1internal.GatewayCredentialScope_GATEWAY_CREDENTIAL_SCOPE_SHARED
	}
	out := &compassv1internal.GatewayCredential{
		Id: credential.ID, Provider: credential.Provider, Scope: scope, Version: credential.Version,
	}
	if apiKey, ok := credential.APIKey(); ok {
		out.Value = &compassv1internal.GatewayCredential_ApiKey{ApiKey: apiKey}
	} else if token, ok := credential.OAuth(); ok {
		out.Value = &compassv1internal.GatewayCredential_Oauth{Oauth: gatewayOAuthToProto(token)}
	}
	return out
}
