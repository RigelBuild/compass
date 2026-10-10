package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/gatewaycred"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

const (
	gatewayTestAgent = store.AccountID("agent-gateway-test")
	gatewayTestOwner = store.AccountID("owner-gateway-test")
)

type fakeAgentTenantResolver struct {
	tenant store.TenantID
	err    error
	calls  int
	agent  store.AccountID
}

func (f *fakeAgentTenantResolver) GatewayAgentTenant(_ context.Context, agent store.AccountID) (store.TenantID, error) {
	f.calls++
	f.agent = agent
	return f.tenant, f.err
}

type recordingGatewayPool struct {
	inner   gatewaycred.PoolResolver
	tenants []store.TenantID
}

func (p *recordingGatewayPool) Pool(ctx context.Context, agent store.AccountID) ([]gatewaycred.Credential, error) {
	tenant, _ := store.TenantFromContext(ctx)
	p.tenants = append(p.tenants, tenant)
	return p.inner.Pool(ctx, agent)
}

type recordingGatewayStore struct {
	gatewaycred.CredentialStore
	updateTenants  []store.TenantID
	disableTenants []store.TenantID
	calls          int
}

func (s *recordingGatewayStore) UpdateOAuth(ctx context.Context, agent store.AccountID, id string, token gatewaycred.OAuthToken, expected int64) (int64, error) {
	tenant, _ := store.TenantFromContext(ctx)
	s.updateTenants = append(s.updateTenants, tenant)
	s.calls++
	return s.CredentialStore.UpdateOAuth(ctx, agent, id, token, expected)
}

func (s *recordingGatewayStore) Disable(ctx context.Context, agent store.AccountID, id, cause string, expected int64) error {
	tenant, _ := store.TenantFromContext(ctx)
	s.disableTenants = append(s.disableTenants, tenant)
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
	scope  *fakeAgentTenantResolver
	creds  *recordingGatewayStore
	pool   *recordingGatewayPool
}

func newGatewayServiceFixture(t *testing.T) gatewayServiceTestFixture {
	t.Helper()
	memory := gatewaycred.NewMemory()
	memory.SetAgentOwner(gatewayTestAgent, gatewayTestOwner)
	scope := &fakeAgentTenantResolver{tenant: "tenant-resolved"}
	creds := &recordingGatewayStore{CredentialStore: memory}
	pool := &recordingGatewayPool{inner: memory}
	return gatewayServiceTestFixture{
		svc: newGatewayCredentialsService(scope, creds, pool, nil), memory: memory,
		scope: scope, creds: creds, pool: pool,
	}
}

func gatewayServiceContext(t *testing.T) context.Context {
	t.Helper()
	token := "service-token-for-gateway-tests"
	request := connect.NewRequest(&compassv1internal.ListCredentialPoolRequest{})
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
		return connect.NewResponse(&compassv1internal.ListCredentialPoolResponse{}), nil
	})(t.Context(), request)
	if err != nil {
		t.Fatalf("authenticate test request: %v", err)
	}
	if _, ok := auth.ServiceSubjectFrom(captured.ctx); !ok {
		t.Fatal("service subject missing from authenticated context")
	}
	return captured.ctx
}

func TestGatewayCredentialsResolveTenantRequiresGatewayServiceID(t *testing.T) {
	fixture := newGatewayServiceFixture(t)
	token := "other-service-token"
	request := connect.NewRequest(&compassv1internal.ListCredentialPoolRequest{})
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
		return connect.NewResponse(&compassv1internal.ListCredentialPoolResponse{}), nil
	})(t.Context(), request)
	if err != nil {
		t.Fatalf("authenticate other service: %v", err)
	}
	_, err = fixture.svc.resolveTenant(capture.ctx, gatewayTestAgent, "ListCredentialPool")
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("resolveTenant error = %v, want Unauthenticated", err)
	}
	if fixture.scope.calls != 0 {
		t.Fatalf("tenant lookup called for another service: %d", fixture.scope.calls)
	}
}

func TestGatewayCredentialsServiceInvalidArguments(t *testing.T) {
	tests := []struct {
		name string
		call func(*gatewayCredentialsService, context.Context) error
	}{
		{name: "list empty agent", call: func(s *gatewayCredentialsService, ctx context.Context) error {
			_, err := s.ListCredentialPool(ctx, connect.NewRequest(&compassv1internal.ListCredentialPoolRequest{}))
			return err
		}},
		{name: "update empty agent", call: func(s *gatewayCredentialsService, ctx context.Context) error {
			_, err := s.UpdateCredentialOAuth(ctx, connect.NewRequest(&compassv1internal.UpdateCredentialOAuthRequest{Id: "id", ExpectedVersion: 1, Token: &compassv1internal.GatewayOAuthToken{Access: "access"}}))
			return err
		}},
		{name: "update empty id", call: func(s *gatewayCredentialsService, ctx context.Context) error {
			_, err := s.UpdateCredentialOAuth(ctx, connect.NewRequest(&compassv1internal.UpdateCredentialOAuthRequest{AgentAccountId: string(gatewayTestAgent), ExpectedVersion: 1, Token: &compassv1internal.GatewayOAuthToken{Access: "access"}}))
			return err
		}},
		{name: "update zero version", call: func(s *gatewayCredentialsService, ctx context.Context) error {
			_, err := s.UpdateCredentialOAuth(ctx, connect.NewRequest(&compassv1internal.UpdateCredentialOAuthRequest{Id: "id", AgentAccountId: string(gatewayTestAgent), Token: &compassv1internal.GatewayOAuthToken{Access: "access"}}))
			return err
		}},
		{name: "update missing token", call: func(s *gatewayCredentialsService, ctx context.Context) error {
			_, err := s.UpdateCredentialOAuth(ctx, connect.NewRequest(&compassv1internal.UpdateCredentialOAuthRequest{Id: "id", AgentAccountId: string(gatewayTestAgent), ExpectedVersion: 1}))
			return err
		}},
		{name: "update missing access", call: func(s *gatewayCredentialsService, ctx context.Context) error {
			_, err := s.UpdateCredentialOAuth(ctx, connect.NewRequest(&compassv1internal.UpdateCredentialOAuthRequest{Id: "id", AgentAccountId: string(gatewayTestAgent), ExpectedVersion: 1, Token: &compassv1internal.GatewayOAuthToken{Refresh: "refresh"}}))
			return err
		}},
		{name: "disable empty agent", call: func(s *gatewayCredentialsService, ctx context.Context) error {
			_, err := s.DisableCredential(ctx, connect.NewRequest(&compassv1internal.DisableCredentialRequest{Id: "id", ExpectedVersion: 1}))
			return err
		}},
		{name: "disable empty id", call: func(s *gatewayCredentialsService, ctx context.Context) error {
			_, err := s.DisableCredential(ctx, connect.NewRequest(&compassv1internal.DisableCredentialRequest{AgentAccountId: string(gatewayTestAgent), ExpectedVersion: 1}))
			return err
		}},
		{name: "disable zero version", call: func(s *gatewayCredentialsService, ctx context.Context) error {
			_, err := s.DisableCredential(ctx, connect.NewRequest(&compassv1internal.DisableCredentialRequest{Id: "id", AgentAccountId: string(gatewayTestAgent)}))
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newGatewayServiceFixture(t)
			err := tt.call(fixture.svc, gatewayServiceContext(t))
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("error code = %v, want InvalidArgument (err %v)", connect.CodeOf(err), err)
			}
			if fixture.scope.calls != 0 || fixture.creds.calls != 0 {
				t.Fatalf("invalid request reached dependencies: scope calls=%d credential calls=%d", fixture.scope.calls, fixture.creds.calls)
			}
		})
	}
}

func TestGatewayCredentialsServiceUnknownAgentFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		call func(*gatewayCredentialsService, context.Context) error
	}{
		{name: "list", call: func(s *gatewayCredentialsService, ctx context.Context) error {
			_, err := s.ListCredentialPool(ctx, connect.NewRequest(&compassv1internal.ListCredentialPoolRequest{AgentAccountId: string(gatewayTestAgent)}))
			return err
		}},
		{name: "update", call: func(s *gatewayCredentialsService, ctx context.Context) error {
			_, err := s.UpdateCredentialOAuth(ctx, connect.NewRequest(&compassv1internal.UpdateCredentialOAuthRequest{Id: "id", AgentAccountId: string(gatewayTestAgent), ExpectedVersion: 1, Token: &compassv1internal.GatewayOAuthToken{Access: "access"}}))
			return err
		}},
		{name: "disable", call: func(s *gatewayCredentialsService, ctx context.Context) error {
			_, err := s.DisableCredential(ctx, connect.NewRequest(&compassv1internal.DisableCredentialRequest{Id: "id", AgentAccountId: string(gatewayTestAgent), ExpectedVersion: 1}))
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newGatewayServiceFixture(t)
			fixture.scope.err = store.ErrNotFound
			err := tt.call(fixture.svc, gatewayServiceContext(t))
			if connect.CodeOf(err) != connect.CodeNotFound {
				t.Fatalf("error code = %v, want NotFound (err %v)", connect.CodeOf(err), err)
			}
			if fixture.creds.calls != 0 || len(fixture.pool.tenants) != 0 {
				t.Fatalf("dependencies called after unknown agent: credential calls=%d pool calls=%d", fixture.creds.calls, len(fixture.pool.tenants))
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
			request := connect.NewRequest(&compassv1internal.DisableCredentialRequest{Id: tt.id, AgentAccountId: "different-agent", ExpectedVersion: 1})
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

	_, stale := svc.UpdateCredentialOAuth(ctx, connect.NewRequest(&compassv1internal.UpdateCredentialOAuthRequest{Id: oauth.ID, AgentAccountId: string(gatewayTestAgent), ExpectedVersion: 2, Token: &compassv1internal.GatewayOAuthToken{Access: "next"}}))
	if connect.CodeOf(stale) != connect.CodeAborted {
		t.Fatalf("stale update code = %v, want Aborted (err %v)", connect.CodeOf(stale), stale)
	}
	_, wrongKind := svc.UpdateCredentialOAuth(ctx, connect.NewRequest(&compassv1internal.UpdateCredentialOAuthRequest{Id: apiKey.ID, AgentAccountId: string(gatewayTestAgent), ExpectedVersion: 1, Token: &compassv1internal.GatewayOAuthToken{Access: "next"}}))
	if connect.CodeOf(wrongKind) != connect.CodeFailedPrecondition {
		t.Fatalf("api-key update code = %v, want FailedPrecondition (err %v)", connect.CodeOf(wrongKind), wrongKind)
	}
}

func TestGatewayCredentialsServiceListPoolAndMapsValues(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	memory := gatewaycred.NewMemory()
	memory.SetAgentOwner(gatewayTestAgent, gatewayTestOwner)
	scope := &fakeAgentTenantResolver{tenant: "tenant-resolved"}
	creds := &recordingGatewayStore{CredentialStore: memory}
	pool := &recordingGatewayPool{inner: memory}
	svc := newGatewayCredentialsService(scope, creds, pool, logger)
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

	response, err := svc.ListCredentialPool(ctx, connect.NewRequest(&compassv1internal.ListCredentialPoolRequest{AgentAccountId: string(gatewayTestAgent)}))
	if err != nil {
		t.Fatalf("ListCredentialPool: %v", err)
	}
	assertGatewayCredentialPoolResponse(t, response, ownAnthropic, apiKeyCredential, sharedOpenAI, sharedAnthropic)
	assertGatewayCredentialPoolFilterAndAudit(t, svc, ctx, ownAnthropic, pool, scope, &logs)
}

func assertGatewayCredentialPoolResponse(
	t *testing.T,
	response *connect.Response[compassv1internal.ListCredentialPoolResponse],
	ownAnthropic, apiKeyCredential, sharedOpenAI, sharedAnthropic gatewaycred.Credential,
) {
	t.Helper()
	if len(response.Msg.GetCredentials()) != 3 {
		t.Fatalf("pool contains %d credentials, want own anthropic, shared openai and shared gemini", len(response.Msg.GetCredentials()))
	}
	if got := response.Msg.GetCredentials()[0]; got.GetId() != ownAnthropic.ID || got.GetScope() != compassv1internal.GatewayCredentialScope_GATEWAY_CREDENTIAL_SCOPE_OWN {
		t.Fatalf("first pool credential = %+v, want own anthropic", got)
	} else if token := got.GetOauth(); token == nil || token.GetAccess() != "own-access" || token.GetRefresh() != "own-refresh" || token.GetExpiresUnixMs() != 123 || token.GetEnterpriseUrl() != "enterprise" || token.GetProjectId() != "project" || token.GetEmail() != "mail" || token.GetAccountId() != "account" || token.GetApiEndpoint() != "endpoint" || token.GetOrgId() != "org" || token.GetOrgName() != "name" || token.GetAuthorizedAtUnixMs() != 456 || token.GetRegion() != "eu" || token.GetInferenceRegion() != "global" || token.GetActiveOrganizationId() != "active-org" {
		t.Fatalf("OAuth response fields = %+v, want explicit full-field conversion", token)
	}
	if got := response.Msg.GetCredentials()[1]; got.GetId() != apiKeyCredential.ID || got.GetScope() != compassv1internal.GatewayCredentialScope_GATEWAY_CREDENTIAL_SCOPE_SHARED || got.GetApiKey() != "gemini-key" {
		t.Fatalf("second pool credential = %+v, want shared gemini api key", got)
	}
	if got := response.Msg.GetCredentials()[2]; got.GetId() != sharedOpenAI.ID || got.GetScope() != compassv1internal.GatewayCredentialScope_GATEWAY_CREDENTIAL_SCOPE_SHARED || got.GetOauth().GetAccess() != "shared-openai-access" {
		t.Fatalf("third pool credential = %+v, want shared openai OAuth", got)
	}
	if sharedAnthropic.ID == response.Msg.GetCredentials()[0].GetId() {
		t.Fatal("shared anthropic must be suppressed by the owner's enabled credential")
	}
}

func assertGatewayCredentialPoolFilterAndAudit(
	t *testing.T,
	svc *gatewayCredentialsService,
	ctx context.Context,
	ownAnthropic gatewaycred.Credential,
	pool *recordingGatewayPool,
	scope *fakeAgentTenantResolver,
	logs *bytes.Buffer,
) {
	t.Helper()
	filtered, err := svc.ListCredentialPool(ctx, connect.NewRequest(&compassv1internal.ListCredentialPoolRequest{AgentAccountId: string(gatewayTestAgent), Provider: "anthropic"}))
	if err != nil {
		t.Fatalf("filtered ListCredentialPool: %v", err)
	}
	if len(filtered.Msg.GetCredentials()) != 1 || filtered.Msg.GetCredentials()[0].GetId() != ownAnthropic.ID {
		t.Fatalf("anthropic pool filter = %+v, want own anthropic only", filtered.Msg.GetCredentials())
	}
	if len(pool.tenants) != 2 || pool.tenants[0] != scope.tenant || pool.tenants[1] != scope.tenant {
		t.Fatalf("Pool tenant contexts = %v, want %q for each call", pool.tenants, scope.tenant)
	}
	if strings.Count(logs.String(), "gateway credential door: cross-tenant agent lookup") != 2 {
		t.Fatalf("audit logs = %q, want one line for each successful RPC", logs.String())
	}
}

func TestGatewayCredentialsServiceWriteTenantAndAuditLog(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	fixture := newGatewayServiceFixture(t)
	svc, memory, scope, creds := fixture.svc, fixture.memory, fixture.scope, fixture.creds
	svc.log = logger
	ctx := store.WithTenant(gatewayServiceContext(t), scope.tenant)
	credential, err := memory.Create(ctx, gatewaycred.NewOAuthCredential(gatewaycred.Credential{Provider: "openai", Scope: gatewaycred.ScopeOwn, OwnerUserID: gatewayTestOwner}, gatewaycred.OAuthToken{Access: "old"}))
	if err != nil {
		t.Fatalf("create OAuth credential: %v", err)
	}
	updated, err := svc.UpdateCredentialOAuth(gatewayServiceContext(t), connect.NewRequest(&compassv1internal.UpdateCredentialOAuthRequest{
		Id: credential.ID, AgentAccountId: string(gatewayTestAgent), ExpectedVersion: 1,
		Token: &compassv1internal.GatewayOAuthToken{Access: "new"},
	}))
	if err != nil {
		t.Fatalf("UpdateCredentialOAuth: %v", err)
	}
	if updated.Msg.GetVersion() != 2 {
		t.Fatalf("updated version = %d, want 2", updated.Msg.GetVersion())
	}
	if len(creds.updateTenants) != 1 || creds.updateTenants[0] != scope.tenant {
		t.Fatalf("update tenant contexts = %v, want %q", creds.updateTenants, scope.tenant)
	}
	if got := logs.String(); strings.Count(got, "gateway credential door: cross-tenant agent lookup") != 1 || !strings.Contains(got, "service=llm-gateway") || !strings.Contains(got, "agent="+string(gatewayTestAgent)) || !strings.Contains(got, "tenant=tenant-resolved") || !strings.Contains(got, "procedure=UpdateCredentialOAuth") {
		t.Fatalf("audit log missing required lookup fields or count: %s", got)
	}
}

func TestGatewayCredentialsServiceMapsInternalErrorsWithoutDetails(t *testing.T) {
	fixture := newGatewayServiceFixture(t)
	svc, scope, creds := fixture.svc, fixture.scope, fixture.creds
	scope.err = errors.New("database includes secret detail")
	_, err := svc.ListCredentialPool(gatewayServiceContext(t), connect.NewRequest(&compassv1internal.ListCredentialPoolRequest{AgentAccountId: string(gatewayTestAgent)}))
	if connect.CodeOf(err) != connect.CodeInternal || strings.Contains(err.Error(), "secret detail") {
		t.Fatalf("internal error = %v, want fixed Internal error without underlying detail", err)
	}
	if creds.calls != 0 {
		t.Fatalf("credential store called after failed tenant lookup: %d", creds.calls)
	}
}
