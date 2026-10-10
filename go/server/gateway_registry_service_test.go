package server

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/auth"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

type fakeGatewayRegistryStore struct {
	version    int64
	registry   store.ModelRegistry
	currentErr error
	versionErr error
}

func (s *fakeGatewayRegistryStore) CurrentModelRegistry(context.Context) (int64, store.ModelRegistry, error) {
	return s.version, s.registry, s.currentErr
}

func (s *fakeGatewayRegistryStore) ModelRegistryVersion(context.Context) (int64, error) {
	return s.version, s.versionErr
}

func newGatewayRegistryUnitService(st *fakeGatewayRegistryStore) *gatewayRegistryService {
	return newGatewayRegistryService(st, slog.Default())
}

type gatewayRegistryContextCapture struct {
	ctx context.Context
}

func (c *gatewayRegistryContextCapture) handle(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
	c.ctx = ctx
	return connect.NewResponse(&compassv1internal.GetGatewayModelRegistryResponse{}), nil
}

func gatewayRegistryServiceContext(t *testing.T, subject store.Subject) context.Context {
	t.Helper()
	const token = "gateway-registry-test-token"
	request := connect.NewRequest(&compassv1internal.GetGatewayModelRegistryRequest{})
	request.Header().Set("Authorization", "Bearer "+token)
	interceptor := auth.ServiceBearerInterceptor(func(_ context.Context, presented string, want store.SubjectKind) (store.Subject, error) {
		if presented != token || want != store.SubjectService {
			t.Fatalf("service resolver args = (%q, %v), want gateway token and SubjectService", presented, want)
		}
		return subject, nil
	}, subject.ID)
	var capture gatewayRegistryContextCapture
	_, err := interceptor(capture.handle)(t.Context(), request)
	if err != nil {
		t.Fatalf("authenticate service context: %v", err)
	}
	return capture.ctx
}

func TestGatewayRegistryServiceRequiresGatewayServiceSubject(t *testing.T) {
	svc := newGatewayRegistryUnitService(&fakeGatewayRegistryStore{})
	otherServiceCtx := gatewayRegistryServiceContext(t, store.Subject{Kind: store.SubjectService, ID: "other-service"})
	for _, tt := range []struct {
		name string
		ctx  context.Context
	}{
		{name: "missing subject", ctx: t.Context()},
		{name: "different service", ctx: otherServiceCtx},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.GetGatewayModelRegistry(tt.ctx, connect.NewRequest(&compassv1internal.GetGatewayModelRegistryRequest{}))
			if connect.CodeOf(err) != connect.CodeUnauthenticated {
				t.Fatalf("GetGatewayModelRegistry error = %v, want Unauthenticated", err)
			}
			_, err = svc.GetGatewayModelRegistryVersion(tt.ctx, connect.NewRequest(&compassv1internal.GetGatewayModelRegistryVersionRequest{}))
			if connect.CodeOf(err) != connect.CodeUnauthenticated {
				t.Fatalf("GetGatewayModelRegistryVersion error = %v, want Unauthenticated", err)
			}
		})
	}
}

func TestGatewayRegistryServiceUnconfiguredReads(t *testing.T) {
	st := &fakeGatewayRegistryStore{currentErr: store.ErrNotFound}
	svc := newGatewayRegistryUnitService(st)
	ctx := gatewayRegistryServiceContext(t, store.Subject{Kind: store.SubjectService, ID: auth.LLMGatewayServiceID})
	response, err := svc.GetGatewayModelRegistry(ctx, connect.NewRequest(&compassv1internal.GetGatewayModelRegistryRequest{}))
	if err != nil {
		t.Fatalf("GetGatewayModelRegistry: %v", err)
	}
	if response.Msg.GetVersion() != 0 || !proto.Equal(response.Msg.GetRegistry(), &compassv1.ModelRegistry{Entries: map[string]*compassv1.ModelRegistryEntry{}}) {
		t.Fatalf("unconfigured registry response = %v, want version 0 and empty registry", response.Msg)
	}
	version, err := svc.GetGatewayModelRegistryVersion(ctx, connect.NewRequest(&compassv1internal.GetGatewayModelRegistryVersionRequest{}))
	if err != nil {
		t.Fatalf("GetGatewayModelRegistryVersion: %v", err)
	}
	if version.Msg.GetVersion() != 0 {
		t.Fatalf("unconfigured version = %d, want 0", version.Msg.GetVersion())
	}
}

func TestGatewayRegistryServiceMapsStoreErrors(t *testing.T) {
	storeErr := errors.New("database unavailable")
	st := &fakeGatewayRegistryStore{currentErr: storeErr, versionErr: storeErr}
	svc := newGatewayRegistryUnitService(st)
	ctx := gatewayRegistryServiceContext(t, store.Subject{Kind: store.SubjectService, ID: auth.LLMGatewayServiceID})
	_, err := svc.GetGatewayModelRegistry(ctx, connect.NewRequest(&compassv1internal.GetGatewayModelRegistryRequest{}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("GetGatewayModelRegistry error = %v, want Internal", err)
	}
	_, err = svc.GetGatewayModelRegistryVersion(ctx, connect.NewRequest(&compassv1internal.GetGatewayModelRegistryVersionRequest{}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("GetGatewayModelRegistryVersion error = %v, want Internal", err)
	}
}

func TestGatewayRegistryServiceReturnsConfiguredRegistry(t *testing.T) {
	registry := store.ModelRegistry{Entries: map[string]store.ModelRegistryEntry{
		"claude-opus": {
			DisplayName: "Claude Opus",
			Candidates:  []store.ModelCandidate{{Provider: "anthropic", ModelID: "claude-opus-4"}},
			Metadata:    store.ModelMetadata{ContextWindow: 200000, InputCostMicroUSD: 15, OutputCostMicroUSD: 75, API: "anthropic-messages"},
		},
	}}
	st := &fakeGatewayRegistryStore{version: 9, registry: registry}
	svc := newGatewayRegistryUnitService(st)
	ctx := gatewayRegistryServiceContext(t, store.Subject{Kind: store.SubjectService, ID: auth.LLMGatewayServiceID})
	response, err := svc.GetGatewayModelRegistry(ctx, connect.NewRequest(&compassv1internal.GetGatewayModelRegistryRequest{}))
	if err != nil {
		t.Fatalf("GetGatewayModelRegistry: %v", err)
	}
	want := &compassv1.ModelRegistry{Entries: map[string]*compassv1.ModelRegistryEntry{
		"claude-opus": {
			DisplayName: "Claude Opus",
			Candidates:  []*compassv1.ModelCandidate{{Provider: "anthropic", ModelId: "claude-opus-4"}},
			Metadata:    &compassv1.ModelMetadata{ContextWindow: 200000, InputCostMicroUsd: 15, OutputCostMicroUsd: 75, Api: "anthropic-messages"},
		},
	}}
	if response.Msg.GetVersion() != 9 || !proto.Equal(response.Msg.GetRegistry(), want) {
		t.Fatalf("registry response = %v, want version 9 and payload %v", response.Msg, want)
	}
	version, err := svc.GetGatewayModelRegistryVersion(ctx, connect.NewRequest(&compassv1internal.GetGatewayModelRegistryVersionRequest{}))
	if err != nil {
		t.Fatalf("GetGatewayModelRegistryVersion: %v", err)
	}
	if version.Msg.GetVersion() != 9 {
		t.Fatalf("version-only response = %d, want 9", version.Msg.GetVersion())
	}
}
