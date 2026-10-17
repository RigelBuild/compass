//go:build pgtest && unix

package server

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/otelconnect"
	"google.golang.org/protobuf/proto"

	"github.com/RigelBuild/compass/go/internal/auth"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

type gatewayRegistryContextStore struct {
	*store.Store
	currentCalled         bool
	currentUsedSystemRole bool
	versionCalled         bool
	versionUsedSystemRole bool
}

func (s *gatewayRegistryContextStore) GatewayModelRegistry(ctx context.Context) (int64, store.ModelRegistry, error) {
	s.currentCalled = true
	s.currentUsedSystemRole = store.IsSystemRole(ctx)
	return s.Store.GatewayModelRegistry(ctx)
}

func (s *gatewayRegistryContextStore) ModelRegistryVersion(ctx context.Context) (int64, error) {
	s.versionCalled = true
	s.versionUsedSystemRole = store.IsSystemRole(ctx)
	return s.Store.ModelRegistryVersion(ctx)
}

func gatewayRegistryRequest(ctx context.Context, token string) (context.Context, *compassv1internal.GetGatewayModelRegistryRequest) {
	ctx, info := connect.NewClientContext(ctx)
	info.RequestHeader().Set("Authorization", "Bearer "+token)
	return ctx, &compassv1internal.GetGatewayModelRegistryRequest{}
}

func gatewayRegistryVersionRequest(ctx context.Context, token string) (context.Context, *compassv1internal.GetGatewayModelRegistryVersionRequest) {
	ctx, info := connect.NewClientContext(ctx)
	info.RequestHeader().Set("Authorization", "Bearer "+token)
	return ctx, &compassv1internal.GetGatewayModelRegistryVersionRequest{}
}

func assertGatewayRegistryVersion(t *testing.T, response *compassv1internal.GetGatewayModelRegistryVersionResponse, want int64) {
	t.Helper()
	if response.GetVersion() != want {
		t.Fatalf("version-only read = %d, want %d", response.GetVersion(), want)
	}
}

func assertGatewayRegistryQueryContext(t *testing.T, queryStore *gatewayRegistryContextStore) {
	t.Helper()
	if !queryStore.currentCalled || queryStore.currentUsedSystemRole {
		t.Fatalf("GatewayModelRegistry calls = %t with system role %t; want no system role",
			queryStore.currentCalled, queryStore.currentUsedSystemRole)
	}
	if !queryStore.versionCalled || queryStore.versionUsedSystemRole {
		t.Fatalf("ModelRegistryVersion calls = %t with system role %t; want no system role",
			queryStore.versionCalled, queryStore.versionUsedSystemRole)
	}
}

func assertGatewayRegistryAllowlist(t *testing.T, ctx context.Context, client compassv1internalconnect.GatewayRegistryClient, token string) {
	t.Helper()
	ctx, request := gatewayRegistryRequest(ctx, token)
	if _, err := client.GetGatewayModelRegistry(ctx, request); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("GetGatewayModelRegistry error = %v, want Unauthenticated", err)
	}
	ctx, versionRequest := gatewayRegistryVersionRequest(ctx, token)
	if _, err := client.GetGatewayModelRegistryVersion(ctx, versionRequest); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("GetGatewayModelRegistryVersion error = %v, want Unauthenticated", err)
	}
}

type gatewayRegistryDoorFixture struct {
	ctx          context.Context
	store        *store.Store
	operator     store.Account
	accountToken string
	serviceToken string
	otherService string
	client       compassv1internalconnect.GatewayRegistryClient
	queryStore   *gatewayRegistryContextStore
}

func newGatewayRegistryDoorFixture(t *testing.T) *gatewayRegistryDoorFixture {
	t.Helper()
	ctx := t.Context()
	st, err := store.Open(ctx, pgtest.RequireDSN(t))
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	operator, err := st.CreateUser(ctx, store.NewUser{Handle: "gateway-registry-operator", DisplayName: "Gateway Registry Operator"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	accountToken, err := auth.IssueAccountToken(ctx, st, operator.ID)
	if err != nil {
		t.Fatalf("IssueAccountToken: %v", err)
	}
	const serviceToken = "gateway-registry-service-token"
	if err := st.PutTokenHash(ctx, sha256.Sum256([]byte(serviceToken)), store.Subject{Kind: store.SubjectService, ID: auth.LLMGatewayServiceID}); err != nil {
		t.Fatalf("PutTokenHash(gateway service): %v", err)
	}
	const otherServiceToken = "gateway-registry-other-service-token"
	if err := st.PutTokenHash(ctx, sha256.Sum256([]byte(otherServiceToken)), store.Subject{Kind: store.SubjectService, ID: "other-service"}); err != nil {
		t.Fatalf("PutTokenHash(other service): %v", err)
	}
	queryStore := &gatewayRegistryContextStore{Store: st}
	registryService := newGatewayRegistryService(queryStore, nil)
	resolve := func(ctx context.Context, presented string, want store.SubjectKind) (store.Subject, error) {
		return auth.ResolveToken(ctx, st, presented, want)
	}
	runnerResolve := newRunnerResolve(resolve, nil)
	otelIC, err := otelconnect.NewServerInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewServerInterceptor: %v", err)
	}
	mux := http.NewServeMux()
	mountGatewayServices(mux, gatewayServices{registry: registryService}, otelIC, runnerResolve)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &gatewayRegistryDoorFixture{
		ctx: ctx, store: st, operator: operator, accountToken: accountToken,
		serviceToken: serviceToken, otherService: otherServiceToken,
		client: compassv1internalconnect.NewGatewayRegistryClient(connect.NewClient(connecthttp.NewTransport(http.DefaultClient, server.URL, connecthttp.WithReadMaxBytes(0)))), queryStore: queryStore,
	}
}

func TestGatewayRegistryDoorServiceBearerAllowlist(t *testing.T) {
	fixture := newGatewayRegistryDoorFixture(t)
	ctx, st, operator := fixture.ctx, fixture.store, fixture.operator
	serviceToken := fixture.serviceToken
	queryStore, client := fixture.queryStore, fixture.client

	ctx, request := gatewayRegistryRequest(ctx, serviceToken)
	unconfigured, err := client.GetGatewayModelRegistry(ctx, request)
	if err != nil {
		t.Fatalf("GetGatewayModelRegistry on unconfigured fleet: %v", err)
	}
	if unconfigured.GetVersion() != 0 || len(unconfigured.GetRegistry().GetEntries()) != 0 {
		t.Fatalf("unconfigured response = %v, want version 0 and empty registry", unconfigured)
	}
	ctx, versionRequest := gatewayRegistryVersionRequest(ctx, serviceToken)
	unconfiguredVersion, err := client.GetGatewayModelRegistryVersion(ctx, versionRequest)
	if err != nil {
		t.Fatalf("GetGatewayModelRegistryVersion on unconfigured fleet: %v", err)
	}
	if unconfiguredVersion.GetVersion() != 0 {
		t.Fatalf("unconfigured version = %d, want 0", unconfiguredVersion.GetVersion())
	}

	assertGatewayRegistryQueryContext(t, queryStore)
	wantRegistry := store.ModelRegistry{Entries: map[string]store.ModelRegistryEntry{
		"claude-opus": {
			DisplayName: "Claude Opus",
			Candidates:  []store.ModelCandidate{{Provider: "anthropic", ModelID: "claude-opus-4"}},
			Metadata:    store.ModelMetadata{ContextWindow: 200000, InputCostMicroUSD: 15, OutputCostMicroUSD: 75},
		},
	}}
	seededVersion, err := st.PutModelRegistry(ctx, operator.ID, wantRegistry, 0)
	if err != nil {
		t.Fatalf("PutModelRegistry seed: %v", err)
	}
	if seededVersion != 1 {
		t.Fatalf("seed version = %d, want 1", seededVersion)
	}
	ctx, request = gatewayRegistryRequest(ctx, serviceToken)
	seeded, err := client.GetGatewayModelRegistry(ctx, request)
	if err != nil {
		t.Fatalf("GetGatewayModelRegistry after seed: %v", err)
	}
	if seeded.GetVersion() != 1 || !proto.Equal(seeded.GetRegistry(), registryToProto(wantRegistry)) {
		t.Fatalf("seeded response = %v, want version 1 and registry %v", seeded, registryToProto(wantRegistry))
	}
	ctx, versionRequest = gatewayRegistryVersionRequest(ctx, serviceToken)
	seededVersionOnly, err := client.GetGatewayModelRegistryVersion(ctx, versionRequest)
	if err != nil {
		t.Fatalf("GetGatewayModelRegistryVersion after seed: %v", err)
	}
	assertGatewayRegistryVersion(t, seededVersionOnly, 1)
	assertGatewayRegistryQueryContext(t, queryStore)
	updatedRegistry := store.ModelRegistry{Entries: map[string]store.ModelRegistryEntry{
		"claude-sonnet": {
			DisplayName: "Claude Sonnet",
			Candidates:  []store.ModelCandidate{{Provider: "anthropic", ModelID: "claude-sonnet-4"}},
		},
	}}
	updatedVersion, err := st.PutModelRegistry(ctx, operator.ID, updatedRegistry, seededVersion)
	if err != nil {
		t.Fatalf("PutModelRegistry update: %v", err)
	}
	if updatedVersion != 2 {
		t.Fatalf("updated version = %d, want 2", updatedVersion)
	}
	ctx, versionRequest = gatewayRegistryVersionRequest(ctx, serviceToken)
	observedVersion, err := client.GetGatewayModelRegistryVersion(ctx, versionRequest)
	if err != nil {
		t.Fatalf("GetGatewayModelRegistryVersion after update: %v", err)
	}
	assertGatewayRegistryVersion(t, observedVersion, 2)
	ctx, request = gatewayRegistryRequest(ctx, serviceToken)
	updatedRead, err := client.GetGatewayModelRegistry(ctx, request)
	if err != nil {
		t.Fatalf("GetGatewayModelRegistry after update: %v", err)
	}
	if updatedRead.GetVersion() != 2 || !proto.Equal(updatedRead.GetRegistry(), registryToProto(updatedRegistry)) {
		t.Fatalf("updated registry response = %v, want version 2 and payload %v", updatedRead, registryToProto(updatedRegistry))
	}
	if err := st.DeleteModelRegistry(ctx); err != nil {
		t.Fatalf("DeleteModelRegistry: %v", err)
	}
	ctx, request = gatewayRegistryRequest(ctx, serviceToken)
	deleted, err := client.GetGatewayModelRegistry(ctx, request)
	if err != nil {
		t.Fatalf("GetGatewayModelRegistry after delete: %v", err)
	}
	ctx, versionRequest = gatewayRegistryVersionRequest(ctx, serviceToken)
	deletedVersion, err := client.GetGatewayModelRegistryVersion(ctx, versionRequest)
	if err != nil {
		t.Fatalf("GetGatewayModelRegistryVersion after delete: %v", err)
	}
	if deleted.GetVersion() == 0 || deleted.GetVersion() != deletedVersion.GetVersion() || len(deleted.GetRegistry().GetEntries()) != 0 {
		t.Fatalf("deleted full read = %v, version-only = %d; want same non-zero version and empty registry", deleted, deletedVersion.GetVersion())
	}
	for _, tt := range []struct {
		name  string
		token string
	}{
		{name: "account bearer", token: fixture.accountToken},
		{name: "different service", token: fixture.otherService},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertGatewayRegistryAllowlist(t, ctx, client, tt.token)
		})
	}
}
