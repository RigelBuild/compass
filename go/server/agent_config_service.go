//go:build unix

// The fleet agent-config declaration handlers on CompassService (RIG-1625 T2).
// PutAgentConfig / DeleteAgentConfig are admin-gated on the network door;
// GetAgentConfigInfo is value-free (names only) and authenticatedOpen. A Put/Delete
// nudges live Runners to re-fetch the credential-free bundle.
package server

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect/v2"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/store"
)

// configSignaler is the narrow hub surface the config write handlers need to emit
// a ConfigVersion nudge after a bundle write. *runnerhub.Hub satisfies it; the
// service depends on the interface (not the concrete hub) so a test drives the
// emit path with a recorder, mirroring secretsService's secretsSignaler.
type configSignaler interface {
	// SignalConfigChanged nudges every live session to re-fetch the fleet config.
	SignalConfigChanged(ctx context.Context) error
}

// PutAgentConfig declares the fleet config bundle: it validates + hashes + upserts
// the singleton at the store door, then nudges live Runners to re-fetch. Admin-gated
// on the network door (adminOnly); the authenticated caller is the operator-scoped
// writer the store records. A bundle that fails the door's validation (not a gzip
// tarball, path escapes, size/count caps, invalid mcp JSON) is CodeInvalidArgument;
// the bundle bytes are never logged.
//
// The signal fires only when the written version differs from the one already
// stored. The store is content-hash-versioned, so a re-Put of byte-identical
// content yields the same version and replaces the row in place — the fleet
// already holds it, so re-signalling would be a redundant re-fetch prod.
// Concurrent Put is last-writer-wins with no compare-and-set (design record),
// so the read-before-write version compare is a best-effort dedupe, not a lock:
// a read failure degrades to signalling (never blocks the declaration), because
// the signal is itself best-effort — a redundant one coalesces and a missed one
// self-heals on the Runner's reconnect version reconciliation (design record).
func (s *service) PutAgentConfig(
	ctx context.Context,
	req *compassv1.PutAgentConfigRequest,
) (*compassv1.PutAgentConfigResponse, error) {
	caller, err := s.requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	// Best-effort dedupe: read the stored version so an identical re-Put can skip
	// the redundant signal. A read failure (or unconfigured fleet) degrades to an
	// empty current version so the write still commits and a safe redundant signal
	// fires — never let this optimization block the primary config declaration.
	currentVersion, _, err := s.store.CurrentAgentConfig(ctx)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "reading current config version for signal dedupe; signalling anyway", "err", err)
		}
		currentVersion = ""
	}
	version, err := s.store.PutAgentConfig(ctx, caller, req.GetBundle())
	if err != nil {
		if errors.Is(err, store.ErrInvalidArgument) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
		}
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if version != currentVersion {
		s.signalConfigChanged(ctx)
	}
	return &compassv1.PutAgentConfigResponse{Version: version}, nil
}

// GetAgentConfigInfo reports the current bundle's version and its member NAMES,
// bucketed by top dir — names only, never content (record §525-526). An
// unconfigured fleet (store ErrNotFound) is a valid state, NOT an error: it
// returns an empty-but-valid response (empty version, empty name lists), the same
// value-free posture the resolve/fetch path treats as materialize-empty.
func (s *service) GetAgentConfigInfo(
	ctx context.Context,
	_ *compassv1.GetAgentConfigInfoRequest,
) (*compassv1.GetAgentConfigInfoResponse, error) {
	info, err := s.store.AgentConfigInfo(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return &compassv1.GetAgentConfigInfoResponse{}, nil
		}
		// Unreachable-by-construction for a Put-validated bundle: the Info walk
		// (configBundleMemberNames) applies a strict SUBSET of the Put door's
		// validation (validateAndHashConfigBundle), so a bundle already stored via
		// Put can never fail it. Kept as defense-in-depth, not a reachable path.
		if errors.Is(err, store.ErrInvalidArgument) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
		}
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &compassv1.GetAgentConfigInfoResponse{
		Version:     info.Version,
		Skills:      info.Skills,
		Extensions:  info.Extensions,
		McpServers:  info.McpServers,
		HasSettings: info.HasSettings,
		HasAgentsMd: info.HasAgentsMD,
		Rules:       info.Rules,
		Subagents:   info.Subagents,
		HasModels:   info.HasModels,
		Prompts:     info.Prompts,
	}, nil
}

// DeleteAgentConfig clears the fleet config bundle back to unconfigured, then
// nudges live sessions to re-fetch and re-materialize it. Admin-gated (adminOnly).
// Idempotent at the store: deleting an already-empty fleet succeeds.
func (s *service) DeleteAgentConfig(
	ctx context.Context,
	_ *compassv1.DeleteAgentConfigRequest,
) (*compassv1.DeleteAgentConfigResponse, error) {
	if err := s.store.DeleteAgentConfig(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	s.signalConfigChanged(ctx)
	return &compassv1.DeleteAgentConfigResponse{}, nil
}

// requireCaller returns the authenticated caller id, or CodeUnauthenticated when
// none is in context (a door-wiring bug: an interceptor must attach one on every
// door — fail closed, mirroring SubscribeAgentSession and secretsService).
func (s *service) requireCaller(ctx context.Context) (store.AccountID, error) {
	callerID, ok := auth.CallerFrom(ctx)
	if !ok {
		return "", connect.NewError(connect.CodeUnauthenticated, errNoCaller.Error()).WithCause(errNoCaller)
	}
	return callerID, nil
}

// signalConfigChanged nudges the Runner after a successful config write.
// Best-effort: a nil signaler (socket-only server, no Runner door) is a no-op,
// and a push failure is logged, never surfaced after the write commits.
func (s *service) signalConfigChanged(ctx context.Context) {
	if s.signaler == nil {
		return
	}
	if err := s.signaler.SignalConfigChanged(ctx); err != nil {
		slog.WarnContext(ctx, "emitting config changed signal", "err", err)
	}
}
