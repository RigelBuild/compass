package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/gatewaycred"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

const (
	gatewayTestAgent = store.AccountID("agent-gateway-test")
	gatewayTestOwner = store.AccountID("owner-gateway-test")
)

type fakeGatewayCredentialsVerifier struct {
	err   error
	calls int
}

func (f *fakeGatewayCredentialsVerifier) Verify(_ context.Context, token string) (store.GatewayCaller, error) {
	f.calls++
	if f.err != nil {
		return store.GatewayCaller{}, f.err
	}
	if token == f.validAgentToken() {
		return store.GatewayCaller{AgentAccountID: gatewayTestAgent, OwnerUserID: gatewayTestOwner, TenantID: "tenant-resolved"}, nil
	}
	if token == "valid-other-agent-token" {
		return store.GatewayCaller{AgentAccountID: "different-agent", OwnerUserID: "different-owner", TenantID: "tenant-resolved"}, nil
	}
	return store.GatewayCaller{}, auth.ErrGatewayTokenInvalid
}
func (f *fakeGatewayCredentialsVerifier) validAgentToken() string {
	return "distinctive-agent-token-never-log-this"
}

type recordingGatewayPool struct {
	inner      gatewaycred.PoolResolver
	tenants    []store.TenantID
	systemRole []bool
}

func (p *recordingGatewayPool) Pool(ctx context.Context, agent store.AccountID) ([]gatewaycred.Credential, error) {
	tenant, _ := store.TenantFromContext(ctx)
	p.tenants = append(p.tenants, tenant)
	p.systemRole = append(p.systemRole, store.IsSystemRole(ctx))
	return p.inner.Pool(ctx, agent)
}

type recordingGatewayStore struct {
	gatewaycred.CredentialStore
	updateTenants      []store.TenantID
	updateSystemRoles  []bool
	disableTenants     []store.TenantID
	disableSystemRoles []bool
	calls              int
}

func (s *recordingGatewayStore) UpdateOAuth(ctx context.Context, agent store.AccountID, id string, token gatewaycred.OAuthToken, expected int64, clearFields []string) (int64, error) {
	tenant, _ := store.TenantFromContext(ctx)
	s.updateTenants = append(s.updateTenants, tenant)
	s.updateSystemRoles = append(s.updateSystemRoles, store.IsSystemRole(ctx))
	s.calls++
	return s.CredentialStore.UpdateOAuth(ctx, agent, id, token, expected, clearFields)
}

func (s *recordingGatewayStore) Disable(ctx context.Context, agent store.AccountID, id, cause string, expected int64) error {
	tenant, _ := store.TenantFromContext(ctx)
	s.disableTenants = append(s.disableTenants, tenant)
	s.disableSystemRoles = append(s.disableSystemRoles, store.IsSystemRole(ctx))
	s.calls++
	return s.CredentialStore.Disable(ctx, agent, id, cause, expected)
}

type gatewayServiceContextCapture struct {
	ctx context.Context
}

func (c *gatewayServiceContextCapture) capture(ctx context.Context) {
	c.ctx = ctx
}

type gatewayServiceTestFixture struct {
	svc    *gatewayCredentialsService
	memory *gatewaycred.Memory
	tokens *fakeGatewayCredentialsVerifier
	creds  *recordingGatewayStore
	pool   *recordingGatewayPool
}

func newGatewayServiceFixture(t *testing.T) gatewayServiceTestFixture {
	t.Helper()
	memory := gatewaycred.NewMemory()
	memory.SetAgentOwner(gatewayTestAgent, gatewayTestOwner)
	tokens := &fakeGatewayCredentialsVerifier{}
	creds := &recordingGatewayStore{CredentialStore: memory}
	pool := &recordingGatewayPool{inner: memory}
	return gatewayServiceTestFixture{
		svc: newGatewayCredentialsService(tokens, creds, pool, nil), memory: memory,
		tokens: tokens, creds: creds, pool: pool,
	}
}

func gatewayServiceContext(t *testing.T) context.Context {
	t.Helper()
	token := "service-token-for-gateway-tests"
	request := &compassv1internal.ListCredentialPoolRequest{}
	request.Header().Set("Authorization", "Bearer "+token)
	interceptor := auth.ServiceBearerInterceptor(func(_ context.Context, presented string, want store.SubjectKind) (store.Subject, error) {
		if presented != token || want != store.SubjectService {
			t.Fatalf("service resolver args = (%q, %v), want gateway token and SubjectService", presented, want)
		}
		return store.Subject{Kind: store.SubjectService, ID: auth.LLMGatewayServiceID}, nil
	}, auth.LLMGatewayServiceID)
	var captured gatewayServiceContextCapture
	_, err := interceptor(func(requestCtx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		captured.capture(requestCtx)
		return &compassv1internal.ListCredentialPoolResponse{}, nil
	})(t.Context(), request)
	if err != nil {
		t.Fatalf("authenticate test request: %v", err)
	}
	if _, ok := auth.ServiceSubjectFrom(captured.ctx); !ok {
		t.Fatal("service subject missing from authenticated context")
	}
	return store.WithSystemRole(captured.ctx)
}

func TestGatewayCredentialsResolveCallerRequiresGatewayServiceID(t *testing.T) {
	fixture := newGatewayServiceFixture(t)
	token := "other-service-token"
	request := &compassv1internal.ListCredentialPoolRequest{}
	request.Header().Set("Authorization", "Bearer "+token)
	interceptor := auth.ServiceBearerInterceptor(func(_ context.Context, presented string, want store.SubjectKind) (store.Subject, error) {
		if presented != token || want != store.SubjectService {
			t.Fatalf("service resolver args = (%q, %v), want other-service token and SubjectService", presented, want)
		}
		return store.Subject{Kind: store.SubjectService, ID: "other-service"}, nil
	}, "other-service")
	var capture gatewayServiceContextCapture
	_, err := interceptor(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		capture.capture(ctx)
		return &compassv1internal.ListCredentialPoolResponse{}, nil
	})(t.Context(), request)
	if err != nil {
		t.Fatalf("authenticate other service: %v", err)
	}
	_, _, err = fixture.svc.resolveCaller(capture.ctx, fixture.tokens.validAgentToken(), "ListCredentialPool")
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("resolveCaller error = %v, want Unauthenticated", err)
	}
	if fixture.tokens.calls != 0 {
		t.Fatalf("token verifier called for another service: %d", fixture.tokens.calls)
	}
}

type gatewayInvalidArgumentCase struct {
	name string
	call func(*gatewayCredentialsService, context.Context) error
}

func TestGatewayCredentialsServiceInvalidArguments(t *testing.T) {
	tests := []gatewayInvalidArgumentCase{
		{
			name: "list empty agent token",
			call: func(s *gatewayCredentialsService, ctx context.Context) error {
				_, err := s.ListCredentialPool(ctx, &compassv1internal.ListCredentialPoolRequest{})
				return err
			},
		},
		{
			name: "disable empty agent token",
			call: func(s *gatewayCredentialsService, ctx context.Context) error {
				_, err := s.DisableCredential(ctx, &compassv1internal.DisableCredentialRequest{Id: "id", ExpectedVersion: 1})
				return err
			},
		},
		{
			name: "disable empty id",
			call: func(s *gatewayCredentialsService, ctx context.Context) error {
				_, err := s.DisableCredential(ctx, &compassv1internal.DisableCredentialRequest{
					AgentToken: "distinctive-agent-token-never-log-this", ExpectedVersion: 1,
				})
				return err
			},
		},
		{
			name: "disable zero version",
			call: func(s *gatewayCredentialsService, ctx context.Context) error {
				_, err := s.DisableCredential(ctx, &compassv1internal.DisableCredentialRequest{
					Id: "id", AgentToken: "distinctive-agent-token-never-log-this",
				})
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assertGatewayInvalidArgument(t, tt) })
	}
}

func TestGatewayCredentialsServiceInvalidUpdateArguments(t *testing.T) {
	for _, tt := range invalidGatewayUpdateCases() {
		t.Run(tt.name, func(t *testing.T) { assertGatewayInvalidArgument(t, tt) })
	}
}

func assertGatewayInvalidArgument(t *testing.T, tt gatewayInvalidArgumentCase) {
	t.Helper()
	fixture := newGatewayServiceFixture(t)
	err := tt.call(fixture.svc, gatewayServiceContext(t))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("error code = %v, want InvalidArgument (err %v)", connect.CodeOf(err), err)
	}
	if fixture.tokens.calls != 0 || fixture.creds.calls != 0 {
		t.Fatalf("invalid request reached dependencies: verifier calls=%d credential calls=%d", fixture.tokens.calls, fixture.creds.calls)
	}
}

func invalidGatewayUpdateCases() []gatewayInvalidArgumentCase {
	return []gatewayInvalidArgumentCase{
		{
			name: "update empty id",
			call: func(s *gatewayCredentialsService, ctx context.Context) error {
				req := &compassv1internal.UpdateCredentialOAuthRequest{
					AgentToken: "distinctive-agent-token-never-log-this", ExpectedVersion: 1,
					Token: &compassv1internal.GatewayOAuthToken{Access: "access"},
				}
				_, err := s.UpdateCredentialOAuth(ctx, req)
				return err
			},
		},
		{
			name: "update zero version",
			call: func(s *gatewayCredentialsService, ctx context.Context) error {
				req := &compassv1internal.UpdateCredentialOAuthRequest{
					Id: "id", AgentToken: "distinctive-agent-token-never-log-this",
					Token: &compassv1internal.GatewayOAuthToken{Access: "access"},
				}
				_, err := s.UpdateCredentialOAuth(ctx, req)
				return err
			},
		},
		{
			name: "update empty agent token",
			call: func(s *gatewayCredentialsService, ctx context.Context) error {
				req := &compassv1internal.UpdateCredentialOAuthRequest{
					Id: "id", ExpectedVersion: 1,
					Token: &compassv1internal.GatewayOAuthToken{Access: "access"},
				}
				_, err := s.UpdateCredentialOAuth(ctx, req)
				return err
			},
		},
		{
			name: "update missing token",
			call: func(s *gatewayCredentialsService, ctx context.Context) error {
				req := &compassv1internal.UpdateCredentialOAuthRequest{
					Id: "id", AgentToken: "distinctive-agent-token-never-log-this", ExpectedVersion: 1,
				}
				_, err := s.UpdateCredentialOAuth(ctx, req)
				return err
			},
		},
		{
			name: "update missing access",
			call: func(s *gatewayCredentialsService, ctx context.Context) error {
				req := &compassv1internal.UpdateCredentialOAuthRequest{
					Id: "id", AgentToken: "distinctive-agent-token-never-log-this", ExpectedVersion: 1,
					Token: &compassv1internal.GatewayOAuthToken{Refresh: "refresh"},
				}
				_, err := s.UpdateCredentialOAuth(ctx, req)
				return err
			},
		},
		{
			name: "update unknown clear field",
			call: func(s *gatewayCredentialsService, ctx context.Context) error {
				req := &compassv1internal.UpdateCredentialOAuthRequest{
					Id: "id", AgentToken: "distinctive-agent-token-never-log-this", ExpectedVersion: 1,
					Token: &compassv1internal.GatewayOAuthToken{Access: "access"}, ClearFields: []string{"org_id", "nope"},
				}
				_, err := s.UpdateCredentialOAuth(ctx, req)
				return err
			},
		},
		{
			name: "update clears access",
			call: func(s *gatewayCredentialsService, ctx context.Context) error {
				req := &compassv1internal.UpdateCredentialOAuthRequest{
					Id: "id", AgentToken: "distinctive-agent-token-never-log-this", ExpectedVersion: 1,
					Token: &compassv1internal.GatewayOAuthToken{Access: "access"}, ClearFields: []string{"access"},
				}
				_, err := s.UpdateCredentialOAuth(ctx, req)
				return err
			},
		},
		{
			name: "update clears refresh",
			call: func(s *gatewayCredentialsService, ctx context.Context) error {
				req := &compassv1internal.UpdateCredentialOAuthRequest{
					Id: "id", AgentToken: "distinctive-agent-token-never-log-this", ExpectedVersion: 1,
					Token: &compassv1internal.GatewayOAuthToken{Access: "access"}, ClearFields: []string{"refresh"},
				}
				_, err := s.UpdateCredentialOAuth(ctx, req)
				return err
			},
		},
		{
			name: "update clears expires_unix_ms",
			call: func(s *gatewayCredentialsService, ctx context.Context) error {
				req := &compassv1internal.UpdateCredentialOAuthRequest{
					Id: "id", AgentToken: "distinctive-agent-token-never-log-this", ExpectedVersion: 1,
					Token: &compassv1internal.GatewayOAuthToken{Access: "access"}, ClearFields: []string{"expires_unix_ms"},
				}
				_, err := s.UpdateCredentialOAuth(ctx, req)
				return err
			},
		},
	}
}

func TestGatewayCredentialsServiceInvalidAgentToken(t *testing.T) {
	tests := []struct {
		name string
		call func(*gatewayCredentialsService, context.Context) error
	}{
		{name: "list", call: func(s *gatewayCredentialsService, ctx context.Context) error {
			_, err := s.ListCredentialPool(ctx, &compassv1internal.ListCredentialPoolRequest{AgentToken: "unknown-token"})
			return err
		}},
		{name: "update", call: func(s *gatewayCredentialsService, ctx context.Context) error {
			_, err := s.UpdateCredentialOAuth(ctx, &compassv1internal.UpdateCredentialOAuthRequest{Id: "id", AgentToken: "unknown-token", ExpectedVersion: 1, Token: &compassv1internal.GatewayOAuthToken{Access: "access"}})
			return err
		}},
		{name: "disable", call: func(s *gatewayCredentialsService, ctx context.Context) error {
			_, err := s.DisableCredential(ctx, &compassv1internal.DisableCredentialRequest{Id: "id", AgentToken: "unknown-token", ExpectedVersion: 1})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newGatewayServiceFixture(t)
			err := tt.call(fixture.svc, gatewayServiceContext(t))
			if connect.CodeOf(err) != connect.CodeUnauthenticated || err.Error() != "unauthenticated: invalid agent token" {
				t.Fatalf("token error = %v, want Unauthenticated invalid agent token", err)
			}
			if fixture.creds.calls != 0 || len(fixture.pool.tenants) != 0 || fixture.tokens.calls != 1 {
				t.Fatalf("dependencies after token error: verifier calls=%d credential calls=%d pool calls=%d", fixture.tokens.calls, fixture.creds.calls, len(fixture.pool.tenants))
			}
		})
	}
}

func TestGatewayCredentialsServiceMapsCredentialErrorsAndHidesIDs(t *testing.T) {
	fixture := newGatewayServiceFixture(t)
	svc, memory := fixture.svc, fixture.memory
	ctx := store.WithTenant(gatewayServiceContext(t), "tenant-resolved")
	apiKey, err := memory.Create(ctx, gatewaycred.NewAPIKeyCredential(gatewaycred.Credential{Provider: "anthropic", Scope: gatewaycred.ScopeOwn, OwnerUserID: gatewayTestOwner}, "key"))
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	oauth, err := memory.Create(ctx, gatewaycred.NewOAuthCredential(gatewaycred.Credential{Provider: "openai", Scope: gatewaycred.ScopeOwn, OwnerUserID: gatewayTestOwner}, gatewaycred.OAuthToken{Access: "access", Refresh: "refresh"}))
	if err != nil {
		t.Fatalf("create OAuth: %v", err)
	}
	memory.SetAgentOwner("different-agent", "different-owner")
	var foreignMessage string
	for _, tt := range []struct {
		name string
		id   string
	}{
		{name: "foreign credential", id: apiKey.ID},
		{name: "random credential", id: "random-missing-id"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			request := &compassv1internal.DisableCredentialRequest{Id: tt.id, AgentToken: "valid-other-agent-token", ExpectedVersion: 1}
			_, err := svc.DisableCredential(gatewayServiceContext(t), request)
			if connect.CodeOf(err) != connect.CodeNotFound {
				t.Fatalf("credential error = %v, want NotFound", err)
			}
			if tt.name == "foreign credential" {
				foreignMessage = err.Error()
			} else if err.Error() != foreignMessage {
				t.Fatalf("random id error = %q, foreign id error = %q", err.Error(), foreignMessage)
			}
		})
	}
	if len(fixture.creds.disableTenants) != 2 || fixture.creds.disableTenants[0] != "tenant-resolved" || fixture.creds.disableTenants[1] != "tenant-resolved" {
		t.Fatalf("disable tenant contexts = %v, want resolved tenant for each call", fixture.creds.disableTenants)
	}
	if len(fixture.creds.disableSystemRoles) != 2 || fixture.creds.disableSystemRoles[0] || fixture.creds.disableSystemRoles[1] {
		t.Fatalf("disable system role contexts = %v, want false for each call", fixture.creds.disableSystemRoles)
	}

	_, stale := svc.UpdateCredentialOAuth(ctx, &compassv1internal.UpdateCredentialOAuthRequest{Id: oauth.ID, AgentToken: fixture.tokens.validAgentToken(), ExpectedVersion: 2, Token: &compassv1internal.GatewayOAuthToken{Access: "next"}})
	if connect.CodeOf(stale) != connect.CodeAborted {
		t.Fatalf("stale update code = %v, want Aborted (err %v)", connect.CodeOf(stale), stale)
	}
	_, wrongKind := svc.UpdateCredentialOAuth(ctx, &compassv1internal.UpdateCredentialOAuthRequest{Id: apiKey.ID, AgentToken: fixture.tokens.validAgentToken(), ExpectedVersion: 1, Token: &compassv1internal.GatewayOAuthToken{Access: "next"}})
	if connect.CodeOf(wrongKind) != connect.CodeFailedPrecondition {
		t.Fatalf("api-key update code = %v, want FailedPrecondition (err %v)", connect.CodeOf(wrongKind), wrongKind)
	}
}

func TestGatewayCredentialsServiceListPoolAndMapsValues(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	memory := gatewaycred.NewMemory()
	memory.SetAgentOwner(gatewayTestAgent, gatewayTestOwner)
	tokens := &fakeGatewayCredentialsVerifier{}
	creds := &recordingGatewayStore{CredentialStore: memory}
	pool := &recordingGatewayPool{inner: memory}
	svc := newGatewayCredentialsService(tokens, creds, pool, logger)
	ctx := store.WithTenant(gatewayServiceContext(t), "tenant-resolved")

	sharedAnthropic, err := memory.Create(ctx, gatewaycred.NewAPIKeyCredential(gatewaycred.Credential{Provider: "anthropic", Scope: gatewaycred.ScopeShared}, "shared-key"))
	if err != nil {
		t.Fatalf("create shared anthropic: %v", err)
	}
	ownAnthropic, err := memory.Create(ctx, gatewaycred.NewOAuthCredential(gatewaycred.Credential{Provider: "anthropic", Scope: gatewaycred.ScopeOwn, OwnerUserID: gatewayTestOwner}, gatewaycred.OAuthToken{
		Access: "own-access", Refresh: "own-refresh", ExpiresUnixMs: 123, EnterpriseURL: "enterprise", ProjectID: "project", Email: "mail", AccountID: "account", APIEndpoint: "endpoint", OrgID: "org", OrgName: "name", AuthorizedAtUnixMs: 456, Region: "eu", InferenceRegion: "global", ActiveOrganizationID: "active-org",
	}))
	if err != nil {
		t.Fatalf("create own anthropic: %v", err)
	}
	sharedOpenAI, err := memory.Create(ctx, gatewaycred.NewOAuthCredential(gatewaycred.Credential{Provider: "openai", Scope: gatewaycred.ScopeShared}, gatewaycred.OAuthToken{Access: "shared-openai-access", Refresh: "shared-openai-refresh"}))
	if err != nil {
		t.Fatalf("create shared openai: %v", err)
	}
	apiKeyCredential, err := memory.Create(ctx, gatewaycred.NewAPIKeyCredential(gatewaycred.Credential{Provider: "gemini", Scope: gatewaycred.ScopeShared}, "gemini-key"))
	if err != nil {
		t.Fatalf("create shared gemini: %v", err)
	}

	response, err := svc.ListCredentialPool(ctx, &compassv1internal.ListCredentialPoolRequest{AgentToken: tokens.validAgentToken()})
	if err != nil {
		t.Fatalf("ListCredentialPool: %v", err)
	}
	assertGatewayCredentialPoolResponse(t, response, ownAnthropic, apiKeyCredential, sharedOpenAI, sharedAnthropic)
	assertGatewayCredentialPoolFilterAndAudit(t, svc, ctx, ownAnthropic, pool, tokens, &logs)
}

func assertGatewayCredentialPoolResponse(
	t *testing.T,
	response *compassv1internal.ListCredentialPoolResponse,
	ownAnthropic, apiKeyCredential, sharedOpenAI, sharedAnthropic gatewaycred.Credential,
) {
	t.Helper()
	if len(response.GetCredentials()) != 3 {
		t.Fatalf("pool contains %d credentials, want own anthropic, shared openai and shared gemini", len(response.GetCredentials()))
	}
	if got := response.GetCredentials()[0]; got.GetId() != ownAnthropic.ID || got.GetScope() != compassv1internal.GatewayCredentialScope_GATEWAY_CREDENTIAL_SCOPE_OWN {
		t.Fatalf("first pool credential = %+v, want own anthropic", got)
	} else if token := got.GetOauth(); token == nil || token.GetAccess() != "own-access" || token.GetRefresh() != "own-refresh" || token.GetExpiresUnixMs() != 123 || token.GetEnterpriseUrl() != "enterprise" || token.GetProjectId() != "project" || token.GetEmail() != "mail" || token.GetAccountId() != "account" || token.GetApiEndpoint() != "endpoint" || token.GetOrgId() != "org" || token.GetOrgName() != "name" || token.GetAuthorizedAtUnixMs() != 456 || token.GetRegion() != "eu" || token.GetInferenceRegion() != "global" || token.GetActiveOrganizationId() != "active-org" {
		t.Fatalf("OAuth response fields = %+v, want explicit full-field conversion", token)
	}
	if got := response.GetCredentials()[1]; got.GetId() != apiKeyCredential.ID || got.GetScope() != compassv1internal.GatewayCredentialScope_GATEWAY_CREDENTIAL_SCOPE_SHARED || got.GetApiKey() != "gemini-key" {
		t.Fatalf("second pool credential = %+v, want shared gemini api key", got)
	}
	if got := response.GetCredentials()[2]; got.GetId() != sharedOpenAI.ID || got.GetScope() != compassv1internal.GatewayCredentialScope_GATEWAY_CREDENTIAL_SCOPE_SHARED || got.GetOauth().GetAccess() != "shared-openai-access" {
		t.Fatalf("third pool credential = %+v, want shared openai OAuth", got)
	}
	for _, credential := range response.GetCredentials() {
		if credential.GetId() == sharedAnthropic.ID {
			t.Fatal("shared anthropic must be suppressed by the owner's enabled credential")
		}
	}
}

func assertGatewayCredentialPoolFilterAndAudit(
	t *testing.T,
	svc *gatewayCredentialsService,
	ctx context.Context,
	ownAnthropic gatewaycred.Credential,
	pool *recordingGatewayPool,
	tokens *fakeGatewayCredentialsVerifier,
	logs *bytes.Buffer,
) {
	t.Helper()
	filtered, err := svc.ListCredentialPool(ctx, &compassv1internal.ListCredentialPoolRequest{AgentToken: tokens.validAgentToken(), Provider: "anthropic"})
	if err != nil {
		t.Fatalf("filtered ListCredentialPool: %v", err)
	}
	if len(filtered.GetCredentials()) != 1 || filtered.GetCredentials()[0].GetId() != ownAnthropic.ID {
		t.Fatalf("anthropic pool filter = %+v, want own anthropic only", filtered.GetCredentials())
	}
	if len(pool.tenants) != 2 || pool.tenants[0] != store.TenantID("tenant-resolved") || pool.tenants[1] != store.TenantID("tenant-resolved") {
		t.Fatalf("Pool tenant contexts = %v, want tenant-resolved for each call", pool.tenants)
	}
	if len(pool.systemRole) != 2 || pool.systemRole[0] || pool.systemRole[1] {
		t.Fatalf("Pool system role contexts = %v, want false for each call", pool.systemRole)
	}
	if strings.Count(logs.String(), "gateway credential door: caller resolved") != 2 {
		t.Fatalf("audit logs = %q, want one line for each successful RPC", logs.String())
	}
	if strings.Contains(logs.String(), tokens.validAgentToken()) {
		t.Fatal("agent token appeared in successful audit logs")
	}
}

func TestGatewayCredentialsServiceWriteTenantAndAuditLog(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	fixture := newGatewayServiceFixture(t)
	svc, memory, creds := fixture.svc, fixture.memory, fixture.creds
	svc.log = logger
	ctx := store.WithTenant(gatewayServiceContext(t), store.TenantID("tenant-resolved"))
	credential, err := memory.Create(ctx, gatewaycred.NewOAuthCredential(gatewaycred.Credential{Provider: "openai", Scope: gatewaycred.ScopeOwn, OwnerUserID: gatewayTestOwner}, gatewaycred.OAuthToken{Access: "old"}))
	if err != nil {
		t.Fatalf("create OAuth credential: %v", err)
	}
	updated, err := svc.UpdateCredentialOAuth(gatewayServiceContext(t), &compassv1internal.UpdateCredentialOAuthRequest{Id: credential.ID, AgentToken: fixture.tokens.validAgentToken(), ExpectedVersion: 1, Token: &compassv1internal.GatewayOAuthToken{Access: "new"}})
	if err != nil {
		t.Fatalf("UpdateCredentialOAuth: %v", err)
	}
	if updated.GetVersion() != 2 {
		t.Fatalf("updated version = %d, want 2", updated.GetVersion())
	}
	if len(creds.updateTenants) != 1 || creds.updateTenants[0] != store.TenantID("tenant-resolved") {
		t.Fatalf("update tenant contexts = %v, want tenant-resolved", creds.updateTenants)
	}
	if len(creds.updateSystemRoles) != 1 || creds.updateSystemRoles[0] {
		t.Fatalf("update system role contexts = %v, want false", creds.updateSystemRoles)
	}
	if got := logs.String(); strings.Count(got, "gateway credential door: caller resolved") != 1 || !strings.Contains(got, "service=llm-gateway") || !strings.Contains(got, "agent="+string(gatewayTestAgent)) || !strings.Contains(got, "tenant=tenant-resolved") || !strings.Contains(got, "procedure=UpdateCredentialOAuth") {
		t.Fatalf("audit log missing required lookup fields or count: %s", got)
	}
	if strings.Contains(logs.String(), fixture.tokens.validAgentToken()) {
		t.Fatal("agent token appeared in update audit logs")
	}
}

func TestGatewayCredentialsServiceUpdateClearsNamedFields(t *testing.T) {
	fixture := newGatewayServiceFixture(t)
	ctx := store.WithTenant(gatewayServiceContext(t), store.TenantID("tenant-resolved"))
	credential, err := fixture.memory.Create(ctx, gatewaycred.NewOAuthCredential(gatewaycred.Credential{Provider: "openai", Scope: gatewaycred.ScopeOwn, OwnerUserID: gatewayTestOwner}, gatewaycred.OAuthToken{Access: "old", OrgID: "org", Region: "region"}))
	if err != nil {
		t.Fatalf("create OAuth credential: %v", err)
	}
	if _, err := fixture.svc.UpdateCredentialOAuth(gatewayServiceContext(t), &compassv1internal.UpdateCredentialOAuthRequest{Id: credential.ID, AgentToken: fixture.tokens.validAgentToken(), ExpectedVersion: 1, Token: &compassv1internal.GatewayOAuthToken{Access: "new"}, ClearFields: []string{"org_id"}}); err != nil {
		t.Fatalf("UpdateCredentialOAuth: %v", err)
	}
	got, err := fixture.memory.List(ctx, gatewayTestOwner, "openai")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if tok, ok := got[0].OAuth(); !ok || tok.OrgID != "" || tok.Region != "region" {
		t.Fatalf("token after clear = %#v, want org_id cleared and region kept", tok)
	}
}

func TestGatewayCredentialsServiceMapsInternalErrorsWithoutDetails(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	fixture := newGatewayServiceFixture(t)
	fixture.svc.log = logger
	fixture.tokens.err = errors.New("database includes secret detail")
	agentToken := fixture.tokens.validAgentToken()
	_, err := fixture.svc.ListCredentialPool(gatewayServiceContext(t), &compassv1internal.ListCredentialPoolRequest{AgentToken: agentToken})
	if connect.CodeOf(err) != connect.CodeInternal || strings.Contains(err.Error(), "secret detail") || strings.Contains(err.Error(), agentToken) {
		t.Fatalf("internal error = %v, want fixed Internal error without token or underlying detail", err)
	}
	if strings.Contains(logs.String(), agentToken) {
		t.Fatal("agent token appeared in verifier outage logs")
	}
	if fixture.creds.calls != 0 {
		t.Fatalf("credential store called after failed token verification: %d", fixture.creds.calls)
	}
}
