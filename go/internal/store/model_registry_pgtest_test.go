//go:build pgtest && unix

package store

// Model-registry store contracts (RIG-3122 P2), pgtest gate — the CAS write
// discipline and the fail-closed orphan cross-check against a real Postgres via
// the shared harness (newTestStore/mustUser/mkBundle only exist under this tag).
// The pure payload validation is proven in the default-gate sibling; here we
// prove what only a real DB can: the compare-and-set on the whole-registry
// version (a stale/racing expected_version → ErrVersionConflict; the correct
// version → a bump), the seed-then-replace round trip, ErrNotFound on an
// unconfigured store, and that a removal/clear stranding a published profile
// reference fails closed. Behind `pgtest && unix` (SKIP when no runtime).

import (
	"context"
	"errors"
	"testing"
)

// reg1 builds a single-entry registry keyed on the given stable name.
func reg1(name string) ModelRegistry {
	return ModelRegistry{Entries: map[string]ModelRegistryEntry{
		name: {
			DisplayName: name,
			Candidates:  []ModelCandidate{{Provider: "anthropic", ModelID: "claude-" + name}},
		},
	}}
}

// TestPutModelRegistrySeedThenReadRoundTrip: seeding at expected version 0 lands
// version 1, and CurrentModelRegistry reads back that version and payload.
func TestPutModelRegistrySeedThenReadRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "operator")

	v, err := s.PutModelRegistry(ctx, actor.ID, reg1("opus"), 0)
	if err != nil {
		t.Fatalf("seed PutModelRegistry: %v", err)
	}
	if v != 1 {
		t.Fatalf("seed version = %d, want 1", v)
	}
	gotV, gotReg, err := s.CurrentModelRegistry(ctx)
	if err != nil {
		t.Fatalf("CurrentModelRegistry: %v", err)
	}
	if gotV != 1 {
		t.Errorf("read version = %d, want 1", gotV)
	}
	if _, ok := gotReg.Entries["opus"]; !ok {
		t.Errorf("read registry missing 'opus' entry: %+v", gotReg)
	}
}

// TestCurrentModelRegistryEmptyNotFound: an unconfigured store reports ErrNotFound.
func TestCurrentModelRegistryEmptyNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, _, err := s.CurrentModelRegistry(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound on empty store, got %v", err)
	}
}

// TestPutModelRegistryEmptyActorInvalid: an empty writer id is rejected before
// any row write.
func TestPutModelRegistryEmptyActorInvalid(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.PutModelRegistry(ctx, "", reg1("opus"), 0); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument for empty actor, got %v", err)
	}
}

// TestPutModelRegistryCASBumpsOnCorrectVersion: a replace at the current version
// lands and bumps the version; the payload is the new one.
func TestPutModelRegistryCASBumpsOnCorrectVersion(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "operator")

	v1, err := s.PutModelRegistry(ctx, actor.ID, reg1("opus"), 0)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	v2, err := s.PutModelRegistry(ctx, actor.ID, reg1("sonnet"), v1)
	if err != nil {
		t.Fatalf("replace at correct version: %v", err)
	}
	if v2 != v1+1 {
		t.Fatalf("replace version = %d, want %d", v2, v1+1)
	}
	_, gotReg, err := s.CurrentModelRegistry(ctx)
	if err != nil {
		t.Fatalf("CurrentModelRegistry: %v", err)
	}
	if _, ok := gotReg.Entries["sonnet"]; !ok {
		t.Errorf("current registry did not take the replace: %+v", gotReg)
	}
}

// TestPutModelRegistryStaleVersionConflict is the CAS heart: a write carrying a
// stale expected_version (the value read BEFORE a racing write bumped it) matches
// no row and returns ErrVersionConflict — the racing operator write is never
// clobbered.
func TestPutModelRegistryStaleVersionConflict(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "operator")

	v1, err := s.PutModelRegistry(ctx, actor.ID, reg1("opus"), 0)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A first operator advances the version.
	if _, err := s.PutModelRegistry(ctx, actor.ID, reg1("sonnet"), v1); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	// A second operator still holding the stale v1 tries to write — CAS refuses.
	if _, err := s.PutModelRegistry(ctx, actor.ID, reg1("haiku"), v1); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale-version write: want ErrVersionConflict, got %v", err)
	}
	// The losing write left the winner in place.
	_, gotReg, err := s.CurrentModelRegistry(ctx)
	if err != nil {
		t.Fatalf("CurrentModelRegistry: %v", err)
	}
	if _, ok := gotReg.Entries["sonnet"]; !ok {
		t.Errorf("stale write clobbered the winner: %+v", gotReg)
	}
}

// TestPutModelRegistrySeedConflictWhenAlreadyExists: a seed (expected 0) after a
// registry already exists is a stale CAS, not a silent overwrite.
func TestPutModelRegistrySeedConflictWhenAlreadyExists(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "operator")

	if _, err := s.PutModelRegistry(ctx, actor.ID, reg1("opus"), 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.PutModelRegistry(ctx, actor.ID, reg1("sonnet"), 0); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("re-seed over an existing registry: want ErrVersionConflict, got %v", err)
	}
}

// TestPutModelRegistryRemovalOrphaningPublishedProfileFailsClosed: a replace that
// REMOVES a stable name still referenced by a published profile's models.* map
// fails closed (ErrInvalidArgument). The profile lives in the config bundle, so
// a bundle is published first, then a registry holding the referenced name, then
// a replace dropping it is rejected.
func TestPutModelRegistryRemovalOrphaningPublishedProfileFailsClosed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "operator")

	// Seed a registry holding "opus" FIRST — the reverse bundle-door lint rejects
	// publishing a profile that pins a name absent from the registry.
	v1, err := s.PutModelRegistry(ctx, actor.ID, reg1("opus"), 0)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Publish a config bundle whose profile pins the stable name "opus".
	if _, err := s.PutAgentConfig(ctx, actor.ID, mkBundle(t, map[string]string{
		"profiles/candidate/profile.yml": "models:\n  manager: opus\n",
	})); err != nil {
		t.Fatalf("PutAgentConfig: %v", err)
	}
	_, err = s.PutModelRegistry(ctx, actor.ID, reg1("sonnet"), v1) // drops "opus"
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("orphaning removal: want ErrInvalidArgument, got %v", err)
	}
	// A replace that KEEPS "opus" (adds alongside) is allowed.
	both := ModelRegistry{Entries: map[string]ModelRegistryEntry{
		"opus":   reg1("opus").Entries["opus"],
		"sonnet": reg1("sonnet").Entries["sonnet"],
	}}
	if _, err := s.PutModelRegistry(ctx, actor.ID, both, v1); err != nil {
		t.Fatalf("non-orphaning replace rejected: %v", err)
	}
}

// TestDeleteModelRegistryRoundTrip: Delete on a registry not referenced by any
// profile clears it so CurrentModelRegistry reports ErrNotFound.
func TestDeleteModelRegistryRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "operator")

	if _, err := s.PutModelRegistry(ctx, actor.ID, reg1("opus"), 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.DeleteModelRegistry(ctx); err != nil {
		t.Fatalf("DeleteModelRegistry: %v", err)
	}
	if _, _, err := s.CurrentModelRegistry(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CurrentModelRegistry after delete: want ErrNotFound, got %v", err)
	}
}

// TestDeleteModelRegistryIdempotent: deleting an already-unconfigured registry is
// a no-op success.
func TestDeleteModelRegistryIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.DeleteModelRegistry(ctx); err != nil {
		t.Fatalf("DeleteModelRegistry on empty store: want nil, got %v", err)
	}
}

// TestDeleteModelRegistryOrphaningPublishedProfileFailsClosed: clearing a
// registry whose stable name is still referenced by a published profile fails
// closed (ErrInvalidArgument), leaving the registry in place.
func TestDeleteModelRegistryOrphaningPublishedProfileFailsClosed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "operator")

	// Seed the registry FIRST so the reverse bundle-door lint admits the profile.
	if _, err := s.PutModelRegistry(ctx, actor.ID, reg1("opus"), 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.PutAgentConfig(ctx, actor.ID, mkBundle(t, map[string]string{
		"profiles/candidate/profile.yml": "models:\n  manager: opus\n",
	})); err != nil {
		t.Fatalf("PutAgentConfig: %v", err)
	}
	if err := s.DeleteModelRegistry(ctx); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("orphaning delete: want ErrInvalidArgument, got %v", err)
	}
	if _, _, err := s.CurrentModelRegistry(ctx); err != nil {
		t.Fatalf("registry cleared despite orphaning delete: %v", err)
	}
}

// TestPutAgentConfigReverseLintRejectsUnknownStableName is the M2 reverse
// bundle-door lint (design.md §P2 L530-532): publishing a profile that pins a
// bare stable name absent from the current registry fails closed
// (ErrInvalidArgument), rather than stranding a reference that would fail only at
// gateway resolve. Here the registry holds "opus" but the profile pins "sonnet".
func TestPutAgentConfigReverseLintRejectsUnknownStableName(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "operator")

	if _, err := s.PutModelRegistry(ctx, actor.ID, reg1("opus"), 0); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	_, err := s.PutAgentConfig(ctx, actor.ID, mkBundle(t, map[string]string{
		"profiles/candidate/profile.yml": "models:\n  manager: sonnet\n",
	}))
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("profile pinning an unknown stable name: want ErrInvalidArgument, got %v", err)
	}
}

// TestPutAgentConfigReverseLintAcceptsEscapeHatchSelector: a profile using an
// explicit provider/id escape-hatch selector (contains "/") names no registry
// entry and is accepted even against an empty/unconfigured registry — the
// escape hatch is the sanctioned way to reference a model outside the registry.
func TestPutAgentConfigReverseLintAcceptsEscapeHatchSelector(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "operator")

	if _, err := s.PutAgentConfig(ctx, actor.ID, mkBundle(t, map[string]string{
		"profiles/candidate/profile.yml": "models:\n  manager: openrouter/anthropic/claude\n",
	})); err != nil {
		t.Fatalf("escape-hatch selector rejected: %v", err)
	}
}

// TestPutAgentConfigReverseLintFailsClosedOnUnconfiguredRegistry (L3b) exercises
// the load-bearing nil-map path the doc comment calls out: with NO registry ever
// declared, CurrentModelRegistry returns ErrNotFound, reg stays the zero value,
// and every bare stable-name reference misses the empty entry map and rejects.
// TestPutAgentConfigReverseLintRejectsUnknownStableName seeds a registry first,
// so it never reaches this branch; a regression making ErrNotFound fail OPEN
// would stay green without this test.
func TestPutAgentConfigReverseLintFailsClosedOnUnconfiguredRegistry(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "operator")

	// No PutModelRegistry call at all — the registry is unconfigured.
	_, err := s.PutAgentConfig(ctx, actor.ID, mkBundle(t, map[string]string{
		"profiles/candidate/profile.yml": "models:\n  manager: opus\n",
	}))
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bare stable name against unconfigured registry: want ErrInvalidArgument (fail closed), got %v", err)
	}
}

// TestPutAgentConfigReverseLintAcceptsKnownStableName: a profile pinning a bare
// stable name that IS present in the registry is accepted.
func TestPutAgentConfigReverseLintAcceptsKnownStableName(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "operator")

	if _, err := s.PutModelRegistry(ctx, actor.ID, reg1("opus"), 0); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	if _, err := s.PutAgentConfig(ctx, actor.ID, mkBundle(t, map[string]string{
		"profiles/candidate/profile.yml": "models:\n  manager: opus\n",
	})); err != nil {
		t.Fatalf("profile pinning a known stable name rejected: %v", err)
	}
}

// TestPutModelRegistryNegativeVersionInvalid is the L1 guard: an expected_version
// below zero is rejected as ErrInvalidArgument before any row write.
func TestPutModelRegistryNegativeVersionInvalid(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "operator")

	if _, err := s.PutModelRegistry(ctx, actor.ID, reg1("x"), -1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative expected_version: want ErrInvalidArgument, got %v", err)
	}
}

// TestPutModelRegistryMetadataRoundTrip is the M3 metadata proto<->store<->JSONB
// round trip and the L4 candidate-order assertion. registryFromProto/ToProto
// hand-map four transposition-prone metadata fields; no other test populates
// Metadata, so a cost-field swap or a dropped field would ship green. Here a
// fully-populated entry with DISTINCT non-zero values for each field is seeded
// and read back, asserting each field equals what was written (a transposition
// reddens because the values differ), and a 2-candidate chain's read-back order
// equals the written order (L4).
func TestPutModelRegistryMetadataRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actor := mustUser(t, s, "operator")

	// Distinct non-zero values so a field swap cannot pass silently.
	want := ModelRegistryEntry{
		DisplayName: "Claude Opus",
		Candidates: []ModelCandidate{
			{Provider: "anthropic", ModelID: "claude-opus-4"},   // primary, order index 0
			{Provider: "openrouter", ModelID: "anthropic/opus"}, // fallback, order index 1
		},
		Metadata: ModelMetadata{
			ContextWindow:      200000,
			InputCostMicroUSD:  15,
			OutputCostMicroUSD: 75,
			API:                "anthropic-messages",
		},
	}
	reg := ModelRegistry{Entries: map[string]ModelRegistryEntry{"opus": want}}
	if _, err := s.PutModelRegistry(ctx, actor.ID, reg, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, got, err := s.CurrentModelRegistry(ctx)
	if err != nil {
		t.Fatalf("CurrentModelRegistry: %v", err)
	}
	entry, ok := got.Entries["opus"]
	if !ok {
		t.Fatalf("read registry missing 'opus': %+v", got)
	}
	if entry.Metadata.ContextWindow != want.Metadata.ContextWindow {
		t.Errorf("context_window = %d, want %d", entry.Metadata.ContextWindow, want.Metadata.ContextWindow)
	}
	if entry.Metadata.InputCostMicroUSD != want.Metadata.InputCostMicroUSD {
		t.Errorf("input_cost_micro_usd = %d, want %d", entry.Metadata.InputCostMicroUSD, want.Metadata.InputCostMicroUSD)
	}
	if entry.Metadata.OutputCostMicroUSD != want.Metadata.OutputCostMicroUSD {
		t.Errorf("output_cost_micro_usd = %d, want %d", entry.Metadata.OutputCostMicroUSD, want.Metadata.OutputCostMicroUSD)
	}
	if entry.Metadata.API != want.Metadata.API {
		t.Errorf("api = %q, want %q", entry.Metadata.API, want.Metadata.API)
	}
	// L4: candidate order is the resolver's try order — read-back must preserve it.
	if len(entry.Candidates) != len(want.Candidates) {
		t.Fatalf("candidate count = %d, want %d", len(entry.Candidates), len(want.Candidates))
	}
	for i := range want.Candidates {
		if entry.Candidates[i] != want.Candidates[i] {
			t.Errorf("candidate[%d] = %+v, want %+v (order must be preserved)", i, entry.Candidates[i], want.Candidates[i])
		}
	}
}
