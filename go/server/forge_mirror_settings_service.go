//go:build unix

package server

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/store"
)

func (s *service) ListForgeMirrorSettings(
	ctx context.Context,
	_ *compassv1.ListForgeMirrorSettingsRequest,
) (*compassv1.ListForgeMirrorSettingsResponse, error) {
	caller, ok := auth.CallerFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errNoCaller.Error()).WithCause(errNoCaller)
	}
	account, err := s.store.GetAccount(ctx, caller)
	if err != nil {
		wrappedErr := fmt.Errorf("resolve mirror settings caller: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}
	settings, err := s.store.ListForgeMirrorSettings(ctx)
	if err != nil {
		wrappedErr := fmt.Errorf("list forge mirror settings: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}
	out := make([]*compassv1.ForgeMirrorSetting, 0, len(settings))
	for _, setting := range settings {
		mode, err := mirrorModeToProto(setting.Mode)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
		}
		out = append(out, &compassv1.ForgeMirrorSetting{
			Forge: &compassv1.ForgeRef{Provider: compassv1.ForgeProvider(setting.Provider), Host: setting.Host},
			Repo:  setting.Repo,
			Mode:  mode,
		})
	}
	canEdit := account.User != nil && account.User.Role == store.UserRoleAdmin
	return &compassv1.ListForgeMirrorSettingsResponse{Settings: out, CanEdit: canEdit}, nil
}

func (s *service) SetForgeMirrorSetting(
	ctx context.Context,
	req *compassv1.SetForgeMirrorSettingRequest,
) (*compassv1.SetForgeMirrorSettingResponse, error) {
	caller, ok := auth.CallerFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errNoCaller.Error()).WithCause(errNoCaller)
	}
	account, err := s.store.GetAccount(ctx, caller)
	if err != nil {
		wrappedErr := fmt.Errorf("resolve mirror settings caller: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}
	if account.User == nil || account.User.Role != store.UserRoleAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, "setting forge mirror mode requires a tenant admin")
	}
	wire := req.GetSetting()
	if wire == nil || wire.GetForge() == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "forge and setting are required")
	}
	mode, err := mirrorModeFromProto(wire.GetMode())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	provider := wire.GetForge().GetProvider()
	if provider < compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB || provider > compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "unknown forge provider %d", provider)
	}
	coordinate := (store.ForgeCoord{Provider: store.ForgeProvider(provider), Host: wire.GetForge().GetHost(), Repo: wire.GetRepo()}).Normalized()
	enabled, err := s.store.ListForgeMirrorSettings(ctx)
	if err != nil {
		wrappedErr := fmt.Errorf("list enabled forge repositories: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}
	found := false
	for _, candidate := range enabled {
		if candidate.Provider == coordinate.Provider && candidate.Host == coordinate.Host && candidate.Repo == coordinate.Repo {
			found = true
			break
		}
	}
	if !found {
		return nil, connect.NewError(connect.CodeNotFound, store.ErrNotFound.Error()).WithCause(store.ErrNotFound)
	}
	setting := store.ForgeMirrorSetting{Provider: coordinate.Provider, Host: coordinate.Host, Repo: coordinate.Repo, Mode: mode}
	if err := s.store.SetForgeMirrorSetting(ctx, caller, setting); err != nil {
		return nil, mapForgeMirrorStoreError(err)
	}
	protoMode, err := mirrorModeToProto(mode)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &compassv1.SetForgeMirrorSettingResponse{Setting: &compassv1.ForgeMirrorSetting{
		Forge: &compassv1.ForgeRef{Provider: compassv1.ForgeProvider(coordinate.Provider), Host: coordinate.Host},
		Repo:  coordinate.Repo,
		Mode:  protoMode,
	}}, nil
}

func mirrorModeFromProto(mode compassv1.ForgeMirrorMode) (store.ForgeMirrorMode, error) {
	switch mode {
	case compassv1.ForgeMirrorMode_FORGE_MIRROR_MODE_OFF:
		return store.ForgeMirrorOff, nil
	case compassv1.ForgeMirrorMode_FORGE_MIRROR_MODE_MANUAL:
		return store.ForgeMirrorManual, nil
	case compassv1.ForgeMirrorMode_FORGE_MIRROR_MODE_AUTO:
		return store.ForgeMirrorAuto, nil
	default:
		return store.ForgeMirrorOff, fmt.Errorf("unknown forge mirror mode %d", mode)
	}
}

func mirrorModeToProto(mode store.ForgeMirrorMode) (compassv1.ForgeMirrorMode, error) {
	switch mode {
	case store.ForgeMirrorOff:
		return compassv1.ForgeMirrorMode_FORGE_MIRROR_MODE_OFF, nil
	case store.ForgeMirrorManual:
		return compassv1.ForgeMirrorMode_FORGE_MIRROR_MODE_MANUAL, nil
	case store.ForgeMirrorAuto:
		return compassv1.ForgeMirrorMode_FORGE_MIRROR_MODE_AUTO, nil
	default:
		return compassv1.ForgeMirrorMode_FORGE_MIRROR_MODE_UNSPECIFIED, fmt.Errorf("unknown stored forge mirror mode %d", mode)
	}
}

func mapForgeMirrorStoreError(err error) error {
	switch {
	case errors.Is(err, store.ErrInvalidArgument):
		return connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err.Error()).WithCause(err)
	default:
		return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
}
