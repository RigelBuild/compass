//go:build unix

// The fleet model-registry handlers — the operator-facing write path on
// CompassService (RIG-3122 P2). PutModelRegistry / DeleteModelRegistry are
// admin-gated on the network door (admin_gate.go classifies them adminOnly:
// operator-scoped only, agents never author); GetModelRegistry is value-free of
// credentials (the registry names providers/models, never holds keys) and
// classified authenticatedOpen, mirroring GetAgentConfigInfo. They sit on the
// same service struct as the rest of CompassService (service.go).
//
// The write is a COMPARE-AND-SET on the whole-registry version: the caller
// carries the version it read (0 to seed), the store bumps only if the row still
// holds it, and a stale version maps to CodeAborted — the connect/gRPC
// convention for a failed CAS ("re-read and retry"), never CodeInvalidArgument.
// A malformed payload or an orphaning removal is CodeInvalidArgument (fail
// closed at the door). No version signal is wired here: the gateway read surface
// is a later PR (deliverable 4), so there is no live consumer to prod yet.
package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// PutModelRegistry declares the fleet model registry under a compare-and-set on
// the version the caller read. Admin-gated on the network door (adminOnly); the
// authenticated caller is the operator-scoped writer the store records. A
// payload that fails validation (empty display_name, no candidates, malformed
// candidate) or a removal that would orphan a published profile reference is
// CodeInvalidArgument; a stale expected_version is CodeAborted (a racing
// operator write landed first).
func (s *service) PutModelRegistry(
	ctx context.Context,
	req *connect.Request[compassv1.PutModelRegistryRequest],
) (*connect.Response[compassv1.PutModelRegistryResponse], error) {
	caller, err := s.requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	version, err := s.store.PutModelRegistry(ctx, caller, registryFromProto(req.Msg.GetRegistry()), req.Msg.GetExpectedVersion())
	if err != nil {
		return nil, mapModelRegistryErr(err)
	}
	return connect.NewResponse(&compassv1.PutModelRegistryResponse{Version: version}), nil
}

// GetModelRegistry reports the current registry version and payload. An
// unconfigured fleet (store ErrNotFound) is a valid state, NOT an error: it
// returns an empty-but-valid response (version 0, empty registry), the same
// value-free posture GetAgentConfigInfo takes on an unconfigured fleet.
func (s *service) GetModelRegistry(
	ctx context.Context,
	_ *connect.Request[compassv1.GetModelRegistryRequest],
) (*connect.Response[compassv1.GetModelRegistryResponse], error) {
	version, reg, err := s.store.CurrentModelRegistry(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return connect.NewResponse(&compassv1.GetModelRegistryResponse{
				Registry: registryToProto(store.ModelRegistry{}),
			}), nil
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&compassv1.GetModelRegistryResponse{
		Version:  version,
		Registry: registryToProto(reg),
	}), nil
}

// DeleteModelRegistry clears the fleet model registry back to unconfigured.
// Admin-gated (adminOnly). Fails closed (CodeInvalidArgument) if the registry
// being cleared holds a stable name still referenced by a published profile.
// Idempotent at the store: clearing an already-empty registry succeeds.
func (s *service) DeleteModelRegistry(
	ctx context.Context,
	_ *connect.Request[compassv1.DeleteModelRegistryRequest],
) (*connect.Response[compassv1.DeleteModelRegistryResponse], error) {
	if err := s.store.DeleteModelRegistry(ctx); err != nil {
		return nil, mapModelRegistryErr(err)
	}
	return connect.NewResponse(&compassv1.DeleteModelRegistryResponse{}), nil
}

// mapModelRegistryErr maps a store error to its connect status code: a malformed
// payload or an orphaning removal (store.ErrInvalidArgument) is
// CodeInvalidArgument; a failed CAS (store.ErrVersionConflict) is CodeAborted
// (the connect/gRPC convention for compare-and-set contention — the caller
// re-reads and retries); anything else is CodeInternal.
func mapModelRegistryErr(err error) error {
	switch {
	case errors.Is(err, store.ErrInvalidArgument):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, store.ErrVersionConflict):
		return connect.NewError(connect.CodeAborted, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}

// registryFromProto converts the wire registry into the store domain type. A nil
// message (an omitted registry field) is the empty registry — a valid input the
// door then validates (an empty registry has no entries to fail schema shape).
func registryFromProto(msg *compassv1.ModelRegistry) store.ModelRegistry {
	entries := make(map[string]store.ModelRegistryEntry, len(msg.GetEntries()))
	for name, e := range msg.GetEntries() {
		candidates := make([]store.ModelCandidate, 0, len(e.GetCandidates()))
		for _, c := range e.GetCandidates() {
			candidates = append(candidates, store.ModelCandidate{
				Provider: c.GetProvider(),
				ModelID:  c.GetModelId(),
			})
		}
		entries[name] = store.ModelRegistryEntry{
			DisplayName: e.GetDisplayName(),
			Candidates:  candidates,
			Metadata: store.ModelMetadata{
				ContextWindow:      e.GetMetadata().GetContextWindow(),
				InputCostMicroUSD:  e.GetMetadata().GetInputCostMicroUsd(),
				OutputCostMicroUSD: e.GetMetadata().GetOutputCostMicroUsd(),
				API:                e.GetMetadata().GetApi(),
			},
		}
	}
	return store.ModelRegistry{Entries: entries}
}

// registryToProto converts the store domain registry into the wire message. Each
// entry carries a Metadata message (never nil) so a round-trip is shape-stable.
func registryToProto(reg store.ModelRegistry) *compassv1.ModelRegistry {
	entries := make(map[string]*compassv1.ModelRegistryEntry, len(reg.Entries))
	for name, e := range reg.Entries {
		candidates := make([]*compassv1.ModelCandidate, 0, len(e.Candidates))
		for _, c := range e.Candidates {
			candidates = append(candidates, &compassv1.ModelCandidate{
				Provider: c.Provider,
				ModelId:  c.ModelID,
			})
		}
		entries[name] = &compassv1.ModelRegistryEntry{
			DisplayName: e.DisplayName,
			Candidates:  candidates,
			Metadata: &compassv1.ModelMetadata{
				ContextWindow:      e.Metadata.ContextWindow,
				InputCostMicroUsd:  e.Metadata.InputCostMicroUSD,
				OutputCostMicroUsd: e.Metadata.OutputCostMicroUSD,
				Api:                e.Metadata.API,
			},
		}
	}
	return &compassv1.ModelRegistry{Entries: entries}
}
