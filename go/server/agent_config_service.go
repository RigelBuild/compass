//go:build unix

// The fleet agent-config declaration handlers — the operator-facing write path on
// CompassService (RIG-1625 T2). PutAgentConfig / DeleteAgentConfig are admin-gated
// on the network door (admin_gate.go classifies them adminOnly); GetAgentConfigInfo
// is value-free (names only) and classified authenticatedOpen. They sit on the same
// service struct as the rest of CompassService (service.go) rather than a separate
// service like SecretsService — these RPCs are ON CompassService.
//
// A successful Put/Delete emits a ConfigVersion signal (a fire-and-forget hub push
// to every live session, configSignaler) so live Runners re-fetch the bundle. The
// signal carries the store's canonical content version on Put and the empty string
// on Delete (the fleet-cleared-to-empty marker). The bundle is credential-free by
// rule — GetAgentConfigInfo returns member NAMES only, never content.
package server

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/store"
)

// configSignaler is the narrow hub surface the config write handlers need to emit
// a ConfigVersion signal after a bundle write. *runnerhub.Hub satisfies it; the
// service depends on the interface (not the concrete hub) so a test drives the
// emit path with a recorder, mirroring secretsService's secretsSignaler.
type configSignaler interface {
	// SignalConfigVersion pushes a ConfigVersion signal carrying the given
	// version to every live session (best-effort). An empty version marks the
	// fleet cleared to no config.
	SignalConfigVersion(version string) error
}

// PutAgentConfig declares the fleet config bundle: it validates + hashes + upserts
// the singleton at the store door, then signals every live session with the new
// version so Runners re-fetch. Admin-gated on the network door (adminOnly); the
// authenticated caller is the operator-scoped writer the store records. A bundle
// that fails the door's validation (not a gzip tarball, path escapes, size/count
// caps, invalid mcp JSON) is CodeInvalidArgument; the bundle bytes are never
// logged.
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
	req *connect.Request[compassv1.PutAgentConfigRequest],
) (*connect.Response[compassv1.PutAgentConfigResponse], error) {
	caller, err := s.requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	// Best-effort dedupe: read the currently-stored version so an identical
	// re-Put can skip the redundant signal. A read failure (or an unconfigured
	// fleet, ErrNotFound) degrades to an empty current version so the write
	// still commits and a safe redundant signal fires — never let this
	// optimization's read block the primary config declaration.
	currentVersion, _, err := s.store.CurrentAgentConfig(ctx)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "reading current config version for signal dedupe; signalling anyway", "err", err)
		}
		currentVersion = ""
	}
	version, err := s.store.PutAgentConfig(ctx, caller, req.Msg.GetBundle())
	if err != nil {
		if errors.Is(err, store.ErrInvalidArgument) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if version != currentVersion {
		s.signalConfigVersion(ctx, version)
	}
	return connect.NewResponse(&compassv1.PutAgentConfigResponse{Version: version}), nil
}

// GetAgentConfigInfo reports the current bundle's version and its member NAMES,
// bucketed by top dir — names only, never content (record §525-526). An
// unconfigured fleet (store ErrNotFound) is a valid state, NOT an error: it
// returns an empty-but-valid response (empty version, empty name lists), the same
// value-free posture the resolve/fetch path treats as materialize-empty.
func (s *service) GetAgentConfigInfo(
	ctx context.Context,
	_ *connect.Request[compassv1.GetAgentConfigInfoRequest],
) (*connect.Response[compassv1.GetAgentConfigInfoResponse], error) {
	info, err := s.store.AgentConfigInfo(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return connect.NewResponse(&compassv1.GetAgentConfigInfoResponse{}), nil
		}
		// Unreachable-by-construction for a Put-validated bundle: the Info walk
		// (configBundleMemberNames) applies a strict SUBSET of the Put door's
		// validation (validateAndHashConfigBundle), so a bundle already stored via
		// Put can never fail it. Kept as defense-in-depth, not a reachable path.
		if errors.Is(err, store.ErrInvalidArgument) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&compassv1.GetAgentConfigInfoResponse{
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
	}), nil
}

// DeleteAgentConfig clears the fleet config bundle back to unconfigured, then
// signals every live session with an EMPTY version so Runners re-materialize the
// empty config dir. Admin-gated (adminOnly). Idempotent at the store: deleting an
// already-empty fleet succeeds.
func (s *service) DeleteAgentConfig(
	ctx context.Context,
	_ *connect.Request[compassv1.DeleteAgentConfigRequest],
) (*connect.Response[compassv1.DeleteAgentConfigResponse], error) {
	if err := s.store.DeleteAgentConfig(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Empty version: the fleet-cleared marker (record §514-519) — Runners
	// re-fetch and materialize an empty dir.
	s.signalConfigVersion(ctx, "")
	return connect.NewResponse(&compassv1.DeleteAgentConfigResponse{}), nil
}

// requireCaller returns the authenticated caller id, or CodeUnauthenticated when
// none is in context (a door-wiring bug: an interceptor must attach one on every
// door — fail closed, mirroring SubscribeAgentSession and secretsService).
func (s *service) requireCaller(ctx context.Context) (store.AccountID, error) {
	callerID, ok := auth.CallerFrom(ctx)
	if !ok {
		return "", connect.NewError(connect.CodeUnauthenticated, errNoCaller)
	}
	return callerID, nil
}

// signalConfigVersion emits the ConfigVersion signal after a successful config
// write. Best-effort: a nil signaler (socket-only server, no Runner door) is a
// no-op, and a push failure is logged, never surfaced to the caller — the write
// already committed, and the Runner re-fetches on reconnect regardless (the
// signal is only a "re-fetch now" prod, not the source of truth), the same
// posture as bumpSecretsVersion.
func (s *service) signalConfigVersion(ctx context.Context, version string) {
	if s.signaler == nil {
		return
	}
	if err := s.signaler.SignalConfigVersion(version); err != nil {
		slog.WarnContext(ctx, "emitting config version signal", "err", err)
	}
}
