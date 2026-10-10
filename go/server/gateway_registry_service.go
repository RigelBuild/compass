//go:build unix

package server

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/internal/auth"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/store"
)

type gatewayRegistryReader interface {
	GatewayModelRegistry(ctx context.Context) (int64, store.ModelRegistry, error)
	ModelRegistryVersion(ctx context.Context) (int64, error)
}

type gatewayRegistryService struct {
	compassv1internalconnect.UnimplementedGatewayRegistryHandler
	registry gatewayRegistryReader
	log      *slog.Logger
}

var _ compassv1internalconnect.GatewayRegistryHandler = (*gatewayRegistryService)(nil)

func newGatewayRegistryService(registry gatewayRegistryReader, log *slog.Logger) *gatewayRegistryService {
	if log == nil {
		log = slog.Default()
	}
	return &gatewayRegistryService{registry: registry, log: log}
}

func (s *gatewayRegistryService) GetGatewayModelRegistry(
	ctx context.Context,
	_ *connect.Request[compassv1internal.GetGatewayModelRegistryRequest],
) (*connect.Response[compassv1internal.GetGatewayModelRegistryResponse], error) {
	if err := s.requireGatewaySubject(ctx); err != nil {
		return nil, err
	}
	version, registry, err := s.registry.GatewayModelRegistry(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "gateway model registry read failed", "error", err)
		return nil, connect.NewError(connect.CodeInternal, errGatewayRegistryUnavailable)
	}
	return connect.NewResponse(&compassv1internal.GetGatewayModelRegistryResponse{
		Version:  version,
		Registry: registryToProto(registry),
	}), nil
}

func (s *gatewayRegistryService) GetGatewayModelRegistryVersion(
	ctx context.Context,
	_ *connect.Request[compassv1internal.GetGatewayModelRegistryVersionRequest],
) (*connect.Response[compassv1internal.GetGatewayModelRegistryVersionResponse], error) {
	if err := s.requireGatewaySubject(ctx); err != nil {
		return nil, err
	}
	version, err := s.registry.ModelRegistryVersion(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "gateway model registry version read failed", "error", err)
		return nil, connect.NewError(connect.CodeInternal, errGatewayRegistryUnavailable)
	}
	return connect.NewResponse(&compassv1internal.GetGatewayModelRegistryVersionResponse{Version: version}), nil
}

func (s *gatewayRegistryService) requireGatewaySubject(ctx context.Context) error {
	subject, ok := auth.ServiceSubjectFrom(ctx)
	if !ok || subject.Kind != store.SubjectService || subject.ID != auth.LLMGatewayServiceID {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))
	}
	return nil
}

var errGatewayRegistryUnavailable = errors.New("gateway registry unavailable")
