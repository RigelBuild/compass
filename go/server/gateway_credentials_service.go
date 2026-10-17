package server

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect/v2"

	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/gatewaycred"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/store"
)

type gatewayCredentialsService struct {
	compassv1internalconnect.UnimplementedGatewayCredentialsHandler
	tokens gatewayTokenVerifier
	creds  gatewaycred.CredentialStore
	pool   gatewaycred.PoolResolver
	log    *slog.Logger
}

var _ compassv1internalconnect.GatewayCredentialsHandler = (*gatewayCredentialsService)(nil)

func newGatewayCredentialsService(tokens gatewayTokenVerifier, creds gatewaycred.CredentialStore, pool gatewaycred.PoolResolver, log *slog.Logger) *gatewayCredentialsService {
	if log == nil {
		log = slog.Default()
	}
	return &gatewayCredentialsService{tokens: tokens, creds: creds, pool: pool, log: log}
}

func (s *gatewayCredentialsService) ListCredentialPool(ctx context.Context, req *compassv1internal.ListCredentialPoolRequest) (*compassv1internal.ListCredentialPoolResponse, error) {
	if req == nil || req == nil || req.GetAgentToken() == "" {
		return nil, gatewayCredentialsError(connect.CodeInvalidArgument, "agent_token is required")
	}
	ctx, agent, err := s.resolveCaller(ctx, req.GetAgentToken(), "ListCredentialPool")
	if err != nil {
		return nil, err
	}
	credentials, err := s.pool.Pool(ctx, agent)
	if err != nil {
		return nil, s.mapStoreError(ctx, "listing credential pool", err)
	}
	provider := req.GetProvider()
	out := make([]*compassv1internal.GatewayCredential, 0, len(credentials))
	for _, credential := range credentials {
		if provider != "" && credential.Provider != provider {
			continue
		}
		out = append(out, gatewayCredentialToProto(credential))
	}
	return &compassv1internal.ListCredentialPoolResponse{Credentials: out}, nil
}

func (s *gatewayCredentialsService) UpdateCredentialOAuth(ctx context.Context, req *compassv1internal.UpdateCredentialOAuthRequest) (*compassv1internal.UpdateCredentialOAuthResponse, error) {
	if req == nil || req == nil || req.GetAgentToken() == "" || req.GetId() == "" || req.GetExpectedVersion() == 0 || req.GetToken() == nil || req.GetToken().GetAccess() == "" {
		return nil, gatewayCredentialsError(connect.CodeInvalidArgument, "id, agent_token, expected_version, and token access are required")
	}
	if !gatewaycred.ValidOAuthClearFields(req.GetClearFields()) {
		return nil, gatewayCredentialsError(connect.CodeInvalidArgument, "clear_fields names a field that cannot be cleared")
	}
	ctx, agent, err := s.resolveCaller(ctx, req.GetAgentToken(), "UpdateCredentialOAuth")
	if err != nil {
		return nil, err
	}
	version, err := s.creds.UpdateOAuth(ctx, agent, req.GetId(), gatewayOAuthFromProto(req.GetToken()), req.GetExpectedVersion(), req.GetClearFields())
	if err != nil {
		return nil, s.mapStoreError(ctx, "updating OAuth credential", err)
	}
	return &compassv1internal.UpdateCredentialOAuthResponse{Version: version}, nil
}

func (s *gatewayCredentialsService) DisableCredential(ctx context.Context, req *compassv1internal.DisableCredentialRequest) (*compassv1internal.DisableCredentialResponse, error) {
	if req == nil || req == nil || req.GetAgentToken() == "" || req.GetId() == "" || req.GetExpectedVersion() == 0 {
		return nil, gatewayCredentialsError(connect.CodeInvalidArgument, "id, agent_token, and expected_version are required")
	}
	ctx, agent, err := s.resolveCaller(ctx, req.GetAgentToken(), "DisableCredential")
	if err != nil {
		return nil, err
	}
	if err := s.creds.Disable(ctx, agent, req.GetId(), req.GetCause(), req.GetExpectedVersion()); err != nil {
		return nil, s.mapStoreError(ctx, "disabling credential", err)
	}
	return &compassv1internal.DisableCredentialResponse{}, nil
}

func (s *gatewayCredentialsService) resolveCaller(ctx context.Context, agentToken, procedure string) (context.Context, store.AccountID, error) {
	subj, ok := auth.ServiceSubjectFrom(ctx)
	if !ok || subj.Kind != store.SubjectService || subj.ID != auth.LLMGatewayServiceID {
		return nil, "", gatewayCredentialsError(connect.CodeUnauthenticated, "unauthenticated")
	}
	caller, err := s.tokens.Verify(ctx, agentToken)
	if err != nil {
		if errors.Is(err, auth.ErrGatewayTokenInvalid) {
			return nil, "", gatewayCredentialsError(connect.CodeUnauthenticated, "invalid agent token")
		}
		s.log.ErrorContext(ctx, "gateway credential caller verification failed", "operation", procedure, "error", err)
		return nil, "", gatewayCredentialsError(connect.CodeInternal, "gateway credentials unavailable")
	}
	s.log.InfoContext(ctx, "gateway credential door: caller resolved",
		"service", subj.ID, "agent", caller.AgentAccountID, "owner", caller.OwnerUserID, "tenant", caller.TenantID, "procedure", procedure)
	return store.WithTenant(store.WithoutSystemRole(ctx), caller.TenantID), caller.AgentAccountID, nil
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

// errGatewayCredentialNotFound hides whether a credential is missing or out of pool.
var errGatewayCredentialNotFound = gatewayCredentialsError(connect.CodeNotFound, "not found")

func gatewayCredentialsError(code connect.Code, message string) error {
	return connect.NewError(code, message)
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
