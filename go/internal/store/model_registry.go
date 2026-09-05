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
// (model_registry, 0001_init.sql) holds the stable-name registry: per name, a
// display name, an ordered candidate chain of {provider, model_id}, and listing
// metadata. The gateway resolver reads it to route a request's modelId
// (compass-stable-name-routing §P1/P2).
//
// UNLIKE agent_config_bundle (a content-hash upsert, current-only), the write
// path is COMPARE-AND-SET on a monotonic whole-registry version: a Put carries
// the version it read and only lands if the row still holds it, so a racing
// operator write is never clobbered (the versioned-CAS discipline the sibling
// gateway_credentials store also uses, compass-server-llm-gateway/design.md:
// 324-329). The version keys the gateway resolver's in-memory ref (P1). This is
// the first CAS-on-version writer in the store, so the discipline is built fresh
// here; ErrVersionConflict (errors.go) is its sentinel.
//
// The payload is validated fail-closed at the RPC boundary via the pure
// ValidateModelRegistry (schema shape + candidate shape); the orphan cross-check
// (a removal that would strand a published profile's models.* reference) needs
// the current config bundle, so it lives on the store methods (PutModelRegistry
// for removals, DeleteModelRegistry for a full clear), not the pure validator.

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

// Fail-closed size caps on the registry payload (M1). The registry is small
// operator config — a fleet names on the order of dozens of stable names, each
// a short ordered candidate chain — but nothing downstream bounds it: the store
// marshals whatever it is handed into a single JSONB row (PutModelRegistry) and
// GetModelRegistry serves the whole payload to every authenticated account
// (authenticatedOpen), so an unbounded registry is both an unbounded-allocation
// write and a read-amplification vector. These caps mirror the config-bundle
// door's posture (agent_config.go maxDecompressedBytes / maxFileCount): generous
// headroom over any realistic registry while still bounding a single Put's
// memory and the served row. The write is admin-gated, so this is defense in
// depth, not an untrusted-input gate — but a fail-closed bound belongs at the
// door regardless. Each breach is a %w-wrapped ErrInvalidArgument.
const (
	// maxRegistryEntries caps the number of stable-name entries. A fleet's
	// stable-name set is operator-curated and small; hundreds is already far
	// beyond any realistic roster.
	maxRegistryEntries = 512
	// maxCandidatesPerEntry caps one stable name's ordered candidate chain. The
	// chain is the fallback list the resolver tries in order; a handful is
	// typical, tens is generous.
	maxCandidatesPerEntry = 32
	// maxRegistryBytes caps the serialized (json.Marshal) payload size — the
	// bytes actually written to the JSONB row and served on Get. 1 MiB is a low-
	// MB bound comfortably above the entry/candidate caps' worst case of short
	// text fields, while keeping the singleton row and every Get response small.
	maxRegistryBytes = 1 << 20 // 1 MiB
	// maxRegistryFieldBytes caps a single string field (stable name, display
	// name, candidate provider/model_id, metadata api). Checked in the per-entry
	// loop BEFORE the aggregate json.Marshal, so no single field forces an
	// unbounded transient allocation inside Marshal ahead of the byte cap (L1).
	// This composes with the entry/candidate caps to bound the pre-marshal
	// content at maxRegistryEntries * (3 + 2*maxCandidatesPerEntry) *
	// maxRegistryFieldBytes (~= 35 MB worst case, not near maxRegistryBytes); the
	// aggregate maxRegistryBytes check on the marshaled payload is the final,
	// tight gate. Generous over any real name.
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

// validateStableName enforces the stable-name KEY grammar the registry's two
// downstream discriminators depend on. The bundle-door reverse lint reads a
// profile selector as an escape-hatch provider/id when it contains "/"
// (checkBundleProfileRefsAgainstRegistry) and strips a trailing reasoning-tier
// by splitting on the LAST ":" (stableNameOfSelector), so a key containing "/"
// is UNREACHABLE (only ever read as an escape hatch) and a key containing ":"
// is UNREACHABLE (the tier split always truncates it). Either breaks the
// design's own discriminators (design.md §P2 L530-532, L583-585), so the door
// that mints the vocabulary rejects both, plus whitespace (matching
// validateCandidateField's candidate-field strictness one field away) and the
// per-field byte cap (L1).
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

// validateCandidateField enforces the minimal candidate-field shape: non-empty
// and containing no whitespace (a whitespace-bearing provider/model_id is a
// malformed selector, never a real upstream name). Whitespace is the full
// Unicode set (unicode.IsSpace), not just ASCII: a selector bearing exotic
// whitespace (NBSP, vertical tab, U+2028) is as malformed as one with a space,
// and enumerating only the four ASCII runes would fail open on the rest.
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

// CurrentModelRegistry returns the current registry and its monotonic version.
// ErrNotFound when no registry has been declared — a valid state downstream (the
// gateway resolver treats an absent registry as an empty one), but the store
// still reports the absence; the caller decides empty-is-ok, mirroring
// CurrentAgentConfig.
func (s *Store) CurrentModelRegistry(ctx context.Context) (version int64, reg ModelRegistry, err error) {
	row, err := s.q.CurrentModelRegistry(ctx)
	if err != nil {
		if noRows(err) {
			return 0, ModelRegistry{}, fmt.Errorf("%w: no model registry declared", ErrNotFound)
		}
		return 0, ModelRegistry{}, fmt.Errorf("store: read model registry: %w", err)
	}
	if err := json.Unmarshal(row.Registry, &reg); err != nil {
		return 0, ModelRegistry{}, fmt.Errorf("store: unmarshal model registry: %w", err)
	}
	return row.Version, reg, nil
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
//   - 0 seeds the FIRST registry — lands only if the singleton is still absent
//     (ON CONFLICT DO NOTHING); a racing seed that lost gets ErrVersionConflict.
//   - N>0 replaces an existing registry — lands only if the row still holds N,
//     bumping to N+1; a stale/racing version gets ErrVersionConflict.
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
	//
	// TOCTOU: this cross-check spans TWO stores non-transactionally. It reads the
	// config bundle (CurrentAgentConfig, via checkNoOrphanedProfileRefs) here,
	// then CAS-writes the registry row below; the version CAS protects only the
	// registry row, never the separate config-bundle row. A PutAgentConfig that
	// publishes a new profile reference in the window between this read and the
	// write is unseen, so this removal can still strand that just-published ref.
	// The reverse bundle-door lint (checkBundleProfileRefsAgainstRegistry, called from
	// PutAgentConfig) has the symmetric window against a concurrent registry
	// write. This is the ACCEPTED, self-healing degradation: both writers are
	// admin-gated and low-frequency, a stranded ref surfaces only at gateway
	// resolve, and the operator re-runs the write to clear it. We deliberately do
	// NOT take a cross-store lock or span both rows in one transaction — a heavier
	// guarantee judged unnecessary for admin-gated, low-frequency operator writes.
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
				// ON CONFLICT DO NOTHING matched an existing row: a registry
				// already exists, so a seed (expected 0) is a stale CAS.
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

// DeleteModelRegistry clears the fleet model registry, returning to the
// unconfigured state. Fails closed if any stable name in the registry being
// cleared is still referenced by a published profile (clearing it would strand
// that reference). Idempotent: deleting an already-absent registry is a no-op
// success (mirroring DeleteAgentConfig), and there is nothing to orphan.
func (s *Store) DeleteModelRegistry(ctx context.Context) error {
	_, reg, err := s.CurrentModelRegistry(ctx)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil // already unconfigured — idempotent, nothing to orphan
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
	if err := s.q.DeleteModelRegistry(ctx); err != nil {
		return fmt.Errorf("store: delete model registry: %w", err)
	}
	return nil
}

// checkBundleProfileRefsAgainstRegistry is the REVERSE orphan guard mandated by
// the frozen design (compass-stable-name-routing/design.md §P2 L530-532): the
// bundle-door profile lint. PutAgentConfig calls it before the bundle row write,
// so a config bundle whose profile pins a stable name absent from the current
// model registry fails closed (ErrInvalidArgument) rather than publishing a
// stranded reference that would fail only at gateway resolve.
//
// Every profile models.* value must be EITHER a known stable name present in the
// current registry OR an explicit escape-hatch selector (a "provider/id" form,
// which always contains a "/"). An escape-hatch selector names no registry entry
// and is accepted unconditionally. A bare selector is stripped to its stable
// name (split-on-last-colon, stableNameOfSelector) and must be a registry key.
// Cross-family / model-choice judgment stays ADVISORY (design.md §P2 L533-535,
// RIG-2936 posture): this lint enforces only known-name presence, never whether
// a chosen model is appropriate.
//
// An unconfigured registry (CurrentModelRegistry ErrNotFound) has NO known
// names, so any bare stable-name reference fails closed — the operator must
// declare the registry before publishing a profile that pins a name from it.
//
// TOCTOU: this reads the registry then the caller CAS-writes the bundle row,
// two stores with no shared transaction — symmetric to the removal-side window
// documented on PutModelRegistry. A concurrent PutModelRegistry removing a name
// in this window is unseen; the accepted, self-healing degradation is the same
// (admin-gated low-frequency writers, a stranded ref surfaces at resolve, the
// operator re-runs). No cross-store lock is taken.
func (s *Store) checkBundleProfileRefsAgainstRegistry(ctx context.Context, bundle []byte) error {
	profiles, err := configBundleProfileBodies(bundle)
	if err != nil {
		return err
	}
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

// checkNoOrphanedProfileRefs fails closed (ErrInvalidArgument) if any name in
// the given set is still referenced by a published profile's models.* map. The
// published profiles live in the current config bundle (agent_config_bundle);
// an unconfigured fleet (no bundle) references nothing. An empty name set is a
// no-op. This is the fail-closed orphan guard of design.md §P2 L528-532: a
// registry removal must not strand a profile that pins a stable name.
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

// publishedProfileModelRefs reads the current config bundle and returns the set
// of stable names its profiles reference through models.* selectors, mapped to
// the profile name that references each (for a precise error). Only STABLE-NAME
// references count: an explicit provider/id escape-hatch selector always
// contains a "/" (design.md §P2 L530-532) and names no registry entry, so it is
// skipped. An unconfigured fleet (no bundle) references nothing.
//
// It reuses the same tar-walk + YAML-mapping grammar the config-bundle door
// enforced at Put, so it reads only already-validated, trusted content.
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

// profileModelSelectors returns every models.* string selector declared in a
// parsed profile mapping: models.manager (when a string) and every string value
// under models.agents. It mirrors validateProfileModelSelectors' traversal (the
// door already proved these are string-shaped), collecting the values rather
// than shape-checking them. A non-string or absent axis contributes nothing.
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

// stableNameOfSelector returns the stable-name portion of a bare (no "/")
// profile selector: the SDK's selector grammar splits a trailing reasoning-tier
// on the LAST colon (e.g. "claude-opus-4-8:high" → name "claude-opus-4-8"), so
// the registry key is the pre-last-colon segment. A selector with no colon is
// its own stable name. This is the ONE place the split-on-last-colon grammar is
// applied on the server, and only to fail CLOSED: matching the stripped name
// against the registry catches an orphaning removal that a literal-only match
// would miss. This split is sound only because a registry stable name may
// contain neither ":" nor "/" (validateStableName enforces that grammar at the
// registry door; design.md §P2 L530-532, L583-585): a ":"-bearing key would be
// unreachable through its own selector, a "/"-bearing key indistinguishable
// from an escape hatch.
func stableNameOfSelector(sel string) string {
	if i := strings.LastIndexByte(sel, ':'); i >= 0 {
		return sel[:i]
	}
	return sel
}
