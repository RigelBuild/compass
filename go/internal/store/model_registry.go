package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// The fleet MODEL REGISTRY store (RIG-3122 P2). One fleet-wide singleton row
// holds the stable-name registry: per name, a display name, an ordered
// candidate chain of {provider, model_id}, and listing metadata. The gateway
// resolver reads it to route a request's modelId.

// COMPARE-AND-SET on a monotonic version: a Put carries the version it read and
// only lands if the row still holds it. Delete retains the version so a later
// seed cannot reuse a cached version. ErrVersionConflict is its sentinel.

// The payload is validated fail-closed at the RPC boundary; the orphan
// cross-check (a removal stranding a published profile's models.* reference)
// needs the current config bundle, so it lives on the store methods
// (PutModelRegistry, DeleteModelRegistry), not the pure validator.

// ModelCandidate is one upstream (provider, model_id) in a stable name's ordered
// chain — the pair the gateway resolver tries in order until one has a usable
// credential. Both fields are opaque here (the SDK owns the selector grammar);
// the store validates only that each is non-empty and whitespace-free.
type ModelCandidate struct {
	Provider string `json:"provider"`
	ModelID  string `json:"model_id"`
}

// ModelMetadata is the listing shape a stable name carries, taken from its
// primary candidate (OQ-2). All fields optional. Money is integer micro-USD,
// never a float (Global Constraints).
type ModelMetadata struct {
	ContextWindow      int64  `json:"context_window,omitempty"`
	InputCostMicroUSD  int64  `json:"input_cost_micro_usd,omitempty"`
	OutputCostMicroUSD int64  `json:"output_cost_micro_usd,omitempty"`
	API                string `json:"api,omitempty"`
}

// ModelRegistryEntry is one stable name's entry: a display name, the ordered
// candidate chain, and listing metadata.
type ModelRegistryEntry struct {
	DisplayName string           `json:"display_name"`
	Candidates  []ModelCandidate `json:"candidates"`
	Metadata    ModelMetadata    `json:"metadata"`
}

// ModelRegistry is the fleet registry payload: the stable-name → entry map. The
// key is the stable name (the modelId the gateway resolver looks up).
type ModelRegistry struct {
	Entries map[string]ModelRegistryEntry `json:"entries"`
}

// Model-registry size caps bound both marshaling and the JSONB payload served to
// every authenticated account; violations return ErrInvalidArgument.
const (
	// maxRegistryEntries caps the roster size; hundreds is already generous.
	maxRegistryEntries = 512
	// maxCandidatesPerEntry caps each ordered fallback chain.
	maxCandidatesPerEntry = 32
	// maxRegistryBytes caps the serialized JSONB payload served on Get.
	maxRegistryBytes = 1 << 20 // 1 MiB
	// maxRegistryFieldBytes bounds each field before Marshal can make a large
	// transient allocation; entry and aggregate caps bound the complete payload.
	maxRegistryFieldBytes = 1024
)

// ValidateModelRegistry is the pure, DB-free door check the write path runs
// before any row write (RIG-3122 P2, design.md §P2 fail-closed). It enforces the
// registry SCHEMA SHAPE and each candidate's SHAPE; it does NOT do the orphan
// cross-check (that needs the current config bundle and lives on the store
// methods). Cross-family / model-choice judgment stays ADVISORY, not enforced
// (design.md §P2 L533-535). Rejections are %w-wrapped ErrInvalidArgument so the
// RPC edge maps them to CodeInvalidArgument.
//
//   - the entry count, per-entry candidate count, and serialized byte size are
//     within the fail-closed size caps (M1);
//   - every stable-name KEY is non-empty, within the per-field byte cap, and
//     contains no '/', ':', or whitespace — the grammar the reverse lint's
//     escape-hatch ('/') and split-on-last-colon (':') discriminators depend
//     on, rejected at the door that mints the vocabulary (design.md §P2
//     L530-532, L583-585);
//   - every entry has a non-empty display_name and at least one candidate;
//   - every candidate's provider and model_id are non-empty and whitespace-free
//     (no existing model-id grammar in the tree — the SDK owns split-on-last-
//     colon — so the store enforces only the minimal non-empty/no-whitespace
//     shape, per the brief).
func ValidateModelRegistry(reg ModelRegistry) error {
	if len(reg.Entries) > maxRegistryEntries {
		return fmt.Errorf("%w: model registry has %d entries, exceeds the cap of %d", ErrInvalidArgument, len(reg.Entries), maxRegistryEntries)
	}
	for name, entry := range reg.Entries {
		if err := validateStableName(name); err != nil {
			return err
		}
		if entry.DisplayName == "" {
			return fmt.Errorf("%w: model registry entry %q has an empty display_name", ErrInvalidArgument, name)
		}
		if len(entry.DisplayName) > maxRegistryFieldBytes {
			return fmt.Errorf("%w: model registry entry %q display_name is %d bytes, exceeds the field cap of %d", ErrInvalidArgument, name, len(entry.DisplayName), maxRegistryFieldBytes)
		}
		if len(entry.Metadata.API) > maxRegistryFieldBytes {
			return fmt.Errorf("%w: model registry entry %q metadata api is %d bytes, exceeds the field cap of %d", ErrInvalidArgument, name, len(entry.Metadata.API), maxRegistryFieldBytes)
		}
		if len(entry.Candidates) == 0 {
			return fmt.Errorf("%w: model registry entry %q has no candidates (>=1 required)", ErrInvalidArgument, name)
		}
		if len(entry.Candidates) > maxCandidatesPerEntry {
			return fmt.Errorf("%w: model registry entry %q has %d candidates, exceeds the cap of %d", ErrInvalidArgument, name, len(entry.Candidates), maxCandidatesPerEntry)
		}
		for i, c := range entry.Candidates {
			if err := validateCandidateField(name, i, "provider", c.Provider); err != nil {
				return err
			}
			if err := validateCandidateField(name, i, "model_id", c.ModelID); err != nil {
				return err
			}
		}
	}
	// Serialized-size cap: bound the bytes actually written to the JSONB row and
	// served on Get. Marshalling here mirrors the store's own json.Marshal(reg)
	// at the write, so this catches an over-cap payload before any row write.
	payload, err := json.Marshal(reg)
	if err != nil {
		return fmt.Errorf("%w: model registry is not serializable: %w", ErrInvalidArgument, err)
	}
	if len(payload) > maxRegistryBytes {
		return fmt.Errorf("%w: model registry serializes to %d bytes, exceeds the cap of %d", ErrInvalidArgument, len(payload), maxRegistryBytes)
	}
	return nil
}

// validateStableName rejects slash and colon because they conflict with the
// escape-hatch and reasoning-tier selector grammars. It also rejects whitespace
// and fields over the byte cap.
func validateStableName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: model registry has an empty stable name", ErrInvalidArgument)
	}
	if len(name) > maxRegistryFieldBytes {
		return fmt.Errorf("%w: model registry stable name %q is %d bytes, exceeds the field cap of %d", ErrInvalidArgument, name, len(name), maxRegistryFieldBytes)
	}
	if strings.ContainsRune(name, '/') {
		return fmt.Errorf("%w: model registry stable name %q contains '/' (reserved for the provider/id escape-hatch selector — a slash-bearing name is unreachable)", ErrInvalidArgument, name)
	}
	if strings.ContainsRune(name, ':') {
		return fmt.Errorf("%w: model registry stable name %q contains ':' (reserved for the reasoning-tier suffix — a colon-bearing name is unreachable)", ErrInvalidArgument, name)
	}
	if strings.ContainsFunc(name, unicode.IsSpace) {
		return fmt.Errorf("%w: model registry stable name %q contains whitespace", ErrInvalidArgument, name)
	}
	return nil
}

// validateCandidateField rejects empty values and all Unicode whitespace; an
// ASCII-only check would admit malformed selectors with exotic spacing.
func validateCandidateField(name string, idx int, field, value string) error {
	if value == "" {
		return fmt.Errorf("%w: model registry entry %q candidate %d has an empty %s", ErrInvalidArgument, name, idx, field)
	}
	if len(value) > maxRegistryFieldBytes {
		return fmt.Errorf("%w: model registry entry %q candidate %d %s is %d bytes, exceeds the field cap of %d", ErrInvalidArgument, name, idx, field, len(value), maxRegistryFieldBytes)
	}
	if strings.ContainsFunc(value, unicode.IsSpace) {
		return fmt.Errorf("%w: model registry entry %q candidate %d %s %q contains whitespace", ErrInvalidArgument, name, idx, field, value)
	}
	return nil
}

// CurrentModelRegistry returns live registries; tombstones are ErrNotFound.
func (s *Store) CurrentModelRegistry(ctx context.Context) (version int64, reg ModelRegistry, err error) {
	row, err := s.q.CurrentModelRegistry(ctx)
	if err != nil {
		if noRows(err) {
			return 0, ModelRegistry{}, fmt.Errorf("%w: no model registry declared", ErrNotFound)
		}
		return 0, ModelRegistry{}, fmt.Errorf("store: read model registry: %w", err)
	}
	if string(row.Registry) == "null" {
		return 0, ModelRegistry{}, fmt.Errorf("%w: no model registry declared", ErrNotFound)
	}
	if err := json.Unmarshal(row.Registry, &reg); err != nil {
		return 0, ModelRegistry{}, fmt.Errorf("store: unmarshal model registry: %w", err)
	}
	return row.Version, reg, nil
}

// GatewayModelRegistry returns version 0 when never seeded, and the retained
// version with an empty registry after a delete.
func (s *Store) GatewayModelRegistry(ctx context.Context) (version int64, reg ModelRegistry, err error) {
	row, err := s.q.CurrentModelRegistry(ctx)
	if err != nil {
		if noRows(err) {
			return 0, ModelRegistry{}, nil
		}
		return 0, ModelRegistry{}, fmt.Errorf("store: read gateway model registry: %w", err)
	}
	if string(row.Registry) == "null" {
		return row.Version, ModelRegistry{}, nil
	}
	if err := json.Unmarshal(row.Registry, &reg); err != nil {
		return 0, ModelRegistry{}, fmt.Errorf("store: unmarshal gateway model registry: %w", err)
	}
	return row.Version, reg, nil
}

// ModelRegistryVersion reads the retained version without loading the payload.
// It returns 0 only when the singleton has never been seeded.
func (s *Store) ModelRegistryVersion(ctx context.Context) (int64, error) {
	version, err := s.q.ModelRegistryVersion(ctx)
	if err != nil {
		if noRows(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: read model registry version: %w", err)
	}
	return version, nil
}

// PutModelRegistry declares the fleet model registry under a compare-and-set on
// the whole-registry version, returning the new version. It validates the
// payload shape (ValidateModelRegistry) and, for a write that REMOVES a stable
// name, fails closed if a published profile still references that name (the
// orphan cross-check). actor is the operator-scoped writer (empty →
// ErrInvalidArgument); the singleton has no per-actor column, but the param
// documents the operator-scoped write and matches the record.
//
// CAS semantics keyed on expectedVersion:
//   - 0 seeds a never-created registry at 1, or revives a tombstone at its
//     retained version + 1. A live row returns ErrVersionConflict.
//   - N>0 replaces a live registry only when the row still holds N, bumping it
//     to N+1; a stale or deleted version returns ErrVersionConflict.
func (s *Store) PutModelRegistry(ctx context.Context, actor AccountID, reg ModelRegistry, expectedVersion int64) (version int64, err error) {
	if actor == "" {
		return 0, fmt.Errorf("%w: model-registry writer account id is required", ErrInvalidArgument)
	}
	if expectedVersion < 0 {
		return 0, fmt.Errorf("%w: expected_version must be >= 0, got %d", ErrInvalidArgument, expectedVersion)
	}
	if err := ValidateModelRegistry(reg); err != nil {
		return 0, err
	}

	// Orphan cross-check for a REPLACE: any stable name present in the prior
	// registry but absent from the new one is being removed, and a removal that
	// strands a published profile's models.* reference fails closed. A seed
	// (expectedVersion 0) has no prior registry, so nothing is removed.

	// TOCTOU: this cross-check spans TWO stores non-transactionally — it reads
	// the config bundle here, then CAS-writes the registry row (the CAS guards
	// only that row). A concurrent PutAgentConfig can still strand a ref;
	// ACCEPTED, self-healing — both writers are admin-gated and re-runnable.
	if expectedVersion > 0 {
		priorVersion, prior, rerr := s.CurrentModelRegistry(ctx)
		if rerr != nil && !errors.Is(rerr, ErrNotFound) {
			return 0, rerr
		}
		// Only run the removal check against the version the caller intends to
		// replace; a divergent version will fail the CAS below regardless.
		if rerr == nil && priorVersion == expectedVersion {
			if err := s.checkNoOrphanedProfileRefs(ctx, removedNames(prior, reg)); err != nil {
				return 0, err
			}
		}
	}

	payload, err := json.Marshal(reg)
	if err != nil {
		return 0, fmt.Errorf("store: marshal model registry: %w", err)
	}

	if expectedVersion == 0 {
		version, err = s.q.InsertModelRegistry(ctx, payload)
		if err != nil {
			if noRows(err) {
				// A live-row conflict or stale concurrent seed returns no version.
				return 0, fmt.Errorf("%w: a model registry already exists (seed expected none)", ErrVersionConflict)
			}
			return 0, fmt.Errorf("store: seed model registry: %w", err)
		}
		return version, nil
	}

	version, err = s.q.UpdateModelRegistry(ctx, db.UpdateModelRegistryParams{
		Registry: payload,
		Version:  expectedVersion,
	})
	if err != nil {
		if noRows(err) {
			// No row held expectedVersion: either it was already bumped by a
			// racing write, or the registry is unconfigured. Either way the
			// caller's read is stale — CAS conflict.
			return 0, fmt.Errorf("%w: model registry not at version %d", ErrVersionConflict, expectedVersion)
		}
		return 0, fmt.Errorf("store: update model registry: %w", err)
	}
	return version, nil
}

// DeleteModelRegistry tombstones the registry while retaining its version. It
// fails closed on a referenced stable name, and conflicts if a write lands mid-delete.
func (s *Store) DeleteModelRegistry(ctx context.Context) error {
	version, reg, err := s.CurrentModelRegistry(ctx)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	names := make(map[string]bool, len(reg.Entries))
	for name := range reg.Entries {
		names[name] = true
	}
	if err := s.checkNoOrphanedProfileRefs(ctx, names); err != nil {
		return err
	}
	return s.tombstoneModelRegistry(ctx, version)
}

// tombstoneModelRegistry marks the registry deleted only if it still holds
// version. A concurrent delete that already landed counts as success.
func (s *Store) tombstoneModelRegistry(ctx context.Context, version int64) error {
	n, err := s.q.DeleteModelRegistry(ctx, version)
	if err != nil {
		return fmt.Errorf("store: delete model registry: %w", err)
	}
	if n > 0 {
		return nil
	}
	if _, _, err := s.CurrentModelRegistry(ctx); errors.Is(err, ErrNotFound) {
		return nil
	}
	return fmt.Errorf("%w: model registry changed during delete", ErrVersionConflict)
}

// checkBundleProfileRefsAgainstRegistry rejects bare selectors absent from the
// registry; provider/id selectors bypass lookup and suitability stays advisory.
// Unconfigured registries fail closed; concurrent removals may surface at resolve.
func (s *Store) checkBundleProfileRefsAgainstRegistry(ctx context.Context, profiles map[string][]byte) error {
	if len(profiles) == 0 {
		return nil // no profiles → no stable-name references to check
	}
	_, reg, err := s.CurrentModelRegistry(ctx)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	// ErrNotFound leaves reg the zero registry (no entries) — every bare
	// reference is then unknown and fails closed.
	for profileName, body := range profiles {
		mapping, err := parseYAMLMapping(body, "profiles/"+profileName+"/"+memberProfileYML)
		if err != nil {
			// The bundle was shape-validated at the door before this call, so a
			// parse failure here is a logic error; fail closed regardless.
			return err
		}
		for _, sel := range profileModelSelectors(mapping) {
			if strings.Contains(sel, "/") {
				continue // escape-hatch provider/id selector — accepted
			}
			name := stableNameOfSelector(sel)
			if name == "" {
				// A selector that strips to an empty stable name (e.g. ":high", a
				// bare tier with no name) is malformed, not exempt — fail closed
				// rather than admit it (M1). The registry door's own grammar
				// (validateStableName) guarantees no stored key is empty.
				return fmt.Errorf("%w: profile %q has a malformed model selector %q (empty stable name)", ErrInvalidArgument, profileName, sel)
			}
			if _, known := reg.Entries[name]; !known {
				return fmt.Errorf("%w: profile %q references unknown model stable name %q (declare it in the model registry or use a provider/id escape-hatch selector)", ErrInvalidArgument, profileName, name)
			}
		}
	}
	return nil
}

// removedNames returns the set of stable names present in prior but absent from
// next — the names a Put would REMOVE from the registry.
func removedNames(prior, next ModelRegistry) map[string]bool {
	removed := make(map[string]bool)
	for name := range prior.Entries {
		if _, kept := next.Entries[name]; !kept {
			removed[name] = true
		}
	}
	return removed
}

// checkNoOrphanedProfileRefs rejects removing stable names still used by
// published profiles. An unconfigured fleet or empty removal set is a no-op.
func (s *Store) checkNoOrphanedProfileRefs(ctx context.Context, names map[string]bool) error {
	if len(names) == 0 {
		return nil
	}
	refs, err := s.publishedProfileModelRefs(ctx)
	if err != nil {
		return err
	}
	for name := range names {
		if profile, referenced := refs[name]; referenced {
			return fmt.Errorf("%w: stable name %q is still referenced by published profile %q (remove the profile reference before removing the registry entry)", ErrInvalidArgument, name, profile)
		}
	}
	return nil
}

// publishedProfileModelRefs maps stable-name selectors to the profiles that use
// them. Provider/id escape hatches are excluded; the config bundle was validated
// when published, so this reuses its tar and YAML walk.
func (s *Store) publishedProfileModelRefs(ctx context.Context) (map[string]string, error) {
	_, bundle, err := s.CurrentAgentConfig(ctx)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return map[string]string{}, nil // no bundle → no published profiles
		}
		return nil, err
	}
	profiles, err := configBundleProfileBodies(bundle)
	if err != nil {
		return nil, err
	}
	refs := make(map[string]string)
	for profileName, body := range profiles {
		mapping, err := parseYAMLMapping(body, "profiles/"+profileName+"/"+memberProfileYML)
		if err != nil {
			// The bundle was validated at Put, so a parse failure here is a
			// logic error; fail closed regardless (never silently skip a
			// profile whose refs we could not read).
			return nil, err
		}
		for _, sel := range profileModelSelectors(mapping) {
			// An escape-hatch provider/id selector always carries a "/" and
			// names no registry entry; only a bare stable name (optionally
			// carrying a trailing :tier suffix, split-on-last-colon) is a
			// registry reference.
			if strings.Contains(sel, "/") {
				continue
			}
			name := stableNameOfSelector(sel)
			if name == "" {
				// Reads already-stored, door-validated content: the Put-side lint
				// (checkBundleProfileRefsAgainstRegistry) fails closed on an
				// empty-name selector, so a stored bundle cannot carry one. Skip
				// rather than error — this path must not fail on trusted content.
				continue
			}
			if _, seen := refs[name]; !seen {
				refs[name] = profileName
			}
		}
	}
	return refs, nil
}

// profileModelSelectors collects string selectors from models.manager and
// models.agents; the config-bundle door already validated their shape.
func profileModelSelectors(mapping map[string]any) []string {
	modelsRaw, present := mapping["models"]
	if !present || modelsRaw == nil {
		return nil
	}
	models, ok := modelsRaw.(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	if v, present := models["manager"]; present {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	if agentsRaw, present := models["agents"]; present && agentsRaw != nil {
		if agents, ok := agentsRaw.(map[string]any); ok {
			for _, v := range agents {
				if s, ok := v.(string); ok && s != "" {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// stableNameOfSelector strips the final reasoning tier from a bare selector.
// The registry rejects ':' and '/', so the result cannot hide an unreachable key
// or mistake a provider/id escape hatch for a stable-name reference.
func stableNameOfSelector(sel string) string {
	if i := strings.LastIndexByte(sel, ':'); i >= 0 {
		return sel[:i]
	}
	return sel
}
