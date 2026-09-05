package store

// Model-registry DOOR contracts (RIG-3122 P2), default gate — pure functions, no
// Postgres. ValidateModelRegistry is the fail-closed write validator (design.md
// §P2 L524-535): it enforces schema shape (every entry has a display_name and
// >=1 candidate) and candidate shape (provider/model_id non-empty, no
// whitespace). The profile-reference helpers (configBundleProfileBodies +
// profileModelSelectors + stableNameOfSelector) are the DB-free half of the
// orphan cross-check, exercised here over in-test bundles; the DB-backed CAS +
// orphan-rejection contracts live in the pgtest-tagged sibling.
import (
	"compress/gzip"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// validRegistry is a minimal well-formed registry: one stable name, one
// candidate, a display name. The shape ValidateModelRegistry must accept.
func validRegistry() ModelRegistry {
	return ModelRegistry{Entries: map[string]ModelRegistryEntry{
		"claude-opus": {
			DisplayName: "Claude Opus",
			Candidates:  []ModelCandidate{{Provider: "anthropic", ModelID: "claude-opus-4"}},
			Metadata:    ModelMetadata{ContextWindow: 200000, API: "anthropic-messages"},
		},
	}}
}

// TestValidateModelRegistryAcceptsValid pins the accept side: a well-formed
// registry (and the empty registry — a valid unconfigured/cleared state) pass.
func TestValidateModelRegistryAcceptsValid(t *testing.T) {
	if err := ValidateModelRegistry(validRegistry()); err != nil {
		t.Fatalf("valid registry rejected: %v", err)
	}
	if err := ValidateModelRegistry(ModelRegistry{}); err != nil {
		t.Fatalf("empty registry rejected: %v", err)
	}
	// Multiple ordered candidates are the point of the chain — accepted.
	multi := ModelRegistry{Entries: map[string]ModelRegistryEntry{
		"opus": {DisplayName: "Opus", Candidates: []ModelCandidate{
			{Provider: "anthropic", ModelID: "claude-opus-4"},
			{Provider: "openrouter", ModelID: "anthropic/claude-opus"},
		}},
	}}
	if err := ValidateModelRegistry(multi); err != nil {
		t.Fatalf("multi-candidate registry rejected: %v", err)
	}
}

// TestValidateModelRegistryRejectsMalformed is the RED-first schema/candidate
// battery: each case is a single malformed field, and every one must reject with
// ErrInvalidArgument (a bad input reddens). A regression that fails open on any
// one of these turns that case green here.
func TestValidateModelRegistryRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		reg  ModelRegistry
	}{
		{"empty stable name", ModelRegistry{Entries: map[string]ModelRegistryEntry{
			"": {DisplayName: "x", Candidates: []ModelCandidate{{Provider: "a", ModelID: "m"}}},
		}}},
		{"empty display_name", ModelRegistry{Entries: map[string]ModelRegistryEntry{
			"opus": {DisplayName: "", Candidates: []ModelCandidate{{Provider: "a", ModelID: "m"}}},
		}}},
		{"no candidates", ModelRegistry{Entries: map[string]ModelRegistryEntry{
			"opus": {DisplayName: "Opus", Candidates: nil},
		}}},
		{"empty candidate provider", ModelRegistry{Entries: map[string]ModelRegistryEntry{
			"opus": {DisplayName: "Opus", Candidates: []ModelCandidate{{Provider: "", ModelID: "m"}}},
		}}},
		{"empty candidate model_id", ModelRegistry{Entries: map[string]ModelRegistryEntry{
			"opus": {DisplayName: "Opus", Candidates: []ModelCandidate{{Provider: "a", ModelID: ""}}},
		}}},
		{"whitespace in provider", ModelRegistry{Entries: map[string]ModelRegistryEntry{
			"opus": {DisplayName: "Opus", Candidates: []ModelCandidate{{Provider: "an thropic", ModelID: "m"}}},
		}}},
		{"whitespace in model_id", ModelRegistry{Entries: map[string]ModelRegistryEntry{
			"opus": {DisplayName: "Opus", Candidates: []ModelCandidate{{Provider: "a", ModelID: "claude opus"}}},
		}}},
		{"newline in model_id", ModelRegistry{Entries: map[string]ModelRegistryEntry{
			"opus": {DisplayName: "Opus", Candidates: []ModelCandidate{{Provider: "a", ModelID: "claude\nopus"}}},
		}}},
		// L2: a non-ASCII whitespace rune (U+00A0 NBSP) must reject too — the
		// four-ASCII-rune predicate would have failed open on it.
		{"non-breaking space in model_id", ModelRegistry{Entries: map[string]ModelRegistryEntry{
			"opus": {DisplayName: "Opus", Candidates: []ModelCandidate{{Provider: "a", ModelID: "claude\u00a0opus"}}},
		}}},
		// M1: the stable-name KEY grammar the reverse lint's discriminators
		// depend on — a '/' key is indistinguishable from an escape hatch, a
		// ':' key is unreachable through the tier split, whitespace matches the
		// candidate-field strictness one field away. Each must reject at the door.
		{"slash in stable name", ModelRegistry{Entries: map[string]ModelRegistryEntry{
			"anthropic/claude": {DisplayName: "x", Candidates: []ModelCandidate{{Provider: "a", ModelID: "m"}}},
		}}},
		{"colon in stable name", ModelRegistry{Entries: map[string]ModelRegistryEntry{
			"gpt-5:turbo": {DisplayName: "x", Candidates: []ModelCandidate{{Provider: "a", ModelID: "m"}}},
		}}},
		{"whitespace in stable name", ModelRegistry{Entries: map[string]ModelRegistryEntry{
			"opus high": {DisplayName: "x", Candidates: []ModelCandidate{{Provider: "a", ModelID: "m"}}},
		}}},
		{"newline in stable name", ModelRegistry{Entries: map[string]ModelRegistryEntry{
			" opus\n": {DisplayName: "x", Candidates: []ModelCandidate{{Provider: "a", ModelID: "m"}}},
		}}},
		// L1: an oversized single string field rejects before the aggregate cap.
		{"oversized display_name", ModelRegistry{Entries: map[string]ModelRegistryEntry{
			"opus": {DisplayName: strings.Repeat("x", maxRegistryFieldBytes+1), Candidates: []ModelCandidate{{Provider: "a", ModelID: "m"}}},
		}}},
		{"oversized candidate model_id", ModelRegistry{Entries: map[string]ModelRegistryEntry{
			"opus": {DisplayName: "Opus", Candidates: []ModelCandidate{{Provider: "a", ModelID: strings.Repeat("m", maxRegistryFieldBytes+1)}}},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateModelRegistry(tc.reg)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("want ErrInvalidArgument for %s, got %v", tc.name, err)
			}
		})
	}
}

// TestValidateModelRegistryRejectsOverCap is the M1 RED battery: an over-cap
// entry count, an over-cap per-entry candidate count, and an over-cap serialized
// byte size each reject with ErrInvalidArgument. A regression dropping any cap
// turns its case green here. The caps are fail-closed size bounds on the payload
// the store marshals into a JSONB row and serves to every authenticated account.
func TestValidateModelRegistryRejectsOverCap(t *testing.T) {
	t.Run("over entry count cap", func(t *testing.T) {
		entries := make(map[string]ModelRegistryEntry, maxRegistryEntries+1)
		for i := range maxRegistryEntries + 1 {
			name := fmt.Sprintf("name-%d", i)
			entries[name] = ModelRegistryEntry{
				DisplayName: name,
				Candidates:  []ModelCandidate{{Provider: "anthropic", ModelID: "claude-" + name}},
			}
		}
		if err := ValidateModelRegistry(ModelRegistry{Entries: entries}); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("want ErrInvalidArgument over entry cap, got %v", err)
		}
	})

	t.Run("over per-entry candidate cap", func(t *testing.T) {
		cands := make([]ModelCandidate, maxCandidatesPerEntry+1)
		for i := range cands {
			cands[i] = ModelCandidate{Provider: "anthropic", ModelID: fmt.Sprintf("claude-%d", i)}
		}
		reg := ModelRegistry{Entries: map[string]ModelRegistryEntry{
			"opus": {DisplayName: "Opus", Candidates: cands},
		}}
		if err := ValidateModelRegistry(reg); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("want ErrInvalidArgument over candidate cap, got %v", err)
		}
	})

	t.Run("over serialized byte cap", func(t *testing.T) {
		// One entry within the entry/candidate caps but whose display_name alone
		// pushes the serialized payload past maxRegistryBytes.
		big := strings.Repeat("x", maxRegistryBytes+1)
		reg := ModelRegistry{Entries: map[string]ModelRegistryEntry{
			"opus": {DisplayName: big, Candidates: []ModelCandidate{{Provider: "anthropic", ModelID: "claude-opus"}}},
		}}
		if err := ValidateModelRegistry(reg); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("want ErrInvalidArgument over byte cap, got %v", err)
		}
	})

	t.Run("at entry cap accepted", func(t *testing.T) {
		entries := make(map[string]ModelRegistryEntry, maxRegistryEntries)
		for i := range maxRegistryEntries {
			name := fmt.Sprintf("name-%d", i)
			entries[name] = ModelRegistryEntry{
				DisplayName: name,
				Candidates:  []ModelCandidate{{Provider: "anthropic", ModelID: "claude-" + name}},
			}
		}
		if err := ValidateModelRegistry(ModelRegistry{Entries: entries}); err != nil {
			t.Fatalf("registry at the entry cap rejected: %v", err)
		}
	})
}

// TestStableNameOfSelector pins the split-on-last-colon grammar the orphan check
// applies to a bare profile selector: a trailing reasoning tier is stripped so
// the registry key is the pre-last-colon segment; a colon-free selector is its
// own name.
func TestStableNameOfSelector(t *testing.T) {
	cases := map[string]string{
		"claude-opus":          "claude-opus",
		"claude-opus-4-8:high": "claude-opus-4-8",
		"name:with:two:colons": "name:with:two",
		"trailingcolon:":       "trailingcolon",
	}
	for sel, want := range cases {
		if got := stableNameOfSelector(sel); got != want {
			t.Errorf("stableNameOfSelector(%q) = %q, want %q", sel, got, want)
		}
	}
}

// TestValidateStableNameGrammar pins the M1 key grammar directly: the P5 day-1
// stable names (hyphenated, no '/' or ':') pass, and each of the three
// discriminator-breaking shapes the reverse lint depends on ('/', ':',
// whitespace) rejects with ErrInvalidArgument. The '/' and ':' rejections are
// what make stableNameOfSelector's split and the escape-hatch discriminator
// total over any stored key (design.md §P2 L530-532, L583-585).
func TestValidateStableNameGrammar(t *testing.T) {
	for _, ok := range []string{"claude-opus-4-8", "gpt-5-5", "gemini-3-1-pro"} {
		if err := validateStableName(ok); err != nil {
			t.Errorf("validateStableName(%q) rejected a valid P5 name: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "anthropic/claude", "gpt-5:turbo", "opus high", " opus\n", strings.Repeat("x", maxRegistryFieldBytes+1)} {
		if err := validateStableName(bad); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("validateStableName(%q) = %v, want ErrInvalidArgument", bad, err)
		}
	}
}

// TestProfileModelSelectors pins selector collection from a parsed profile: the
// manager selector and every models.agents value are returned; absent/non-string
// axes contribute nothing.
func TestProfileModelSelectors(t *testing.T) {
	mapping := map[string]any{
		"models": map[string]any{
			"manager": "claude-opus:high",
			"agents": map[string]any{
				"impl":   "claude-sonnet",
				"review": "gpt-5",
			},
		},
	}
	got := profileModelSelectors(mapping)
	slices.Sort(got)
	want := []string{"claude-opus:high", "claude-sonnet", "gpt-5"}
	if !slices.Equal(got, want) {
		t.Fatalf("profileModelSelectors = %v, want %v", got, want)
	}
	// No models axis → no selectors.
	if s := profileModelSelectors(map[string]any{"corpus": map[string]any{}}); len(s) != 0 {
		t.Fatalf("profileModelSelectors with no models axis = %v, want empty", s)
	}
}

// TestConfigBundleProfileModelRefsExtraction proves the DB-free half of the
// orphan check end-to-end over a real bundle: a bundle whose profile references
// a bare stable name (`claude-opus`) surfaces that name, keyed to the profile,
// while an explicit provider/id escape-hatch selector (contains "/") is NOT a
// registry reference and is skipped. This is the exact discrimination the
// fail-closed delete/removal guard relies on (design.md §P2 L530-532).
func TestConfigBundleProfileModelRefsExtraction(t *testing.T) {
	// A profile pinning a bare stable name for the manager and an escape-hatch
	// provider/id selector for an agent.
	bundle := buildBundle(t, gzip.DefaultCompression, time.Unix(1000, 0),
		tarEntry{name: "agents/impl.md", content: "---\nname: impl\n---\nx"},
		tarEntry{name: "profiles/candidate/profile.yml", content: "models:\n  manager: claude-opus:high\n  agents:\n    impl: openrouter/anthropic/claude\n"},
	)
	bodies, err := configBundleProfileBodies(bundle)
	if err != nil {
		t.Fatalf("configBundleProfileBodies: %v", err)
	}
	body, ok := bodies["candidate"]
	if !ok {
		t.Fatalf("profile 'candidate' not found in %v", bodies)
	}
	mapping, err := parseYAMLMapping(body, "profiles/candidate/profile.yml")
	if err != nil {
		t.Fatalf("parse profile: %v", err)
	}
	var bareRefs, escapeRefs []string
	for _, sel := range profileModelSelectors(mapping) {
		if selectorHasSlash(sel) {
			escapeRefs = append(escapeRefs, sel)
		} else {
			bareRefs = append(bareRefs, stableNameOfSelector(sel))
		}
	}
	if !slices.Equal(bareRefs, []string{"claude-opus"}) {
		t.Fatalf("bare stable-name refs = %v, want [claude-opus]", bareRefs)
	}
	if !slices.Equal(escapeRefs, []string{"openrouter/anthropic/claude"}) {
		t.Fatalf("escape-hatch refs = %v, want the provider/id selector (skipped by the orphan check)", escapeRefs)
	}
}

// selectorHasSlash mirrors the orphan check's escape-hatch discriminator, kept
// local to the test so a change to the production predicate is caught here.
func selectorHasSlash(sel string) bool {
	for _, r := range sel {
		if r == '/' {
			return true
		}
	}
	return false
}
