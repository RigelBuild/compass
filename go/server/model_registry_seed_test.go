//go:build unix

package server

import (
	"os"
	"slices"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
	"google.golang.org/protobuf/encoding/protojson"
)

// TestDay1ModelRegistrySeedPassesDoor checks that the committed seed parses as a
// PutModelRegistryRequest, passes store validation, and matches the docs tables.
func TestDay1ModelRegistrySeedPassesDoor(t *testing.T) {
	content, err := os.ReadFile("../../docs/model-registry/day-1.json")
	if err != nil {
		t.Fatalf("reading day-1 model registry seed: %v", err)
	}

	var req compassv1.PutModelRegistryRequest
	if err := protojson.Unmarshal(content, &req); err != nil {
		t.Fatalf("unmarshaling day-1 model registry seed: %v", err)
	}
	if req.GetExpectedVersion() != 0 {
		t.Fatalf("expected_version = %d, want 0 for first seed", req.GetExpectedVersion())
	}
	reg := registryFromProto(req.GetRegistry())
	if err := store.ValidateModelRegistry(reg); err != nil {
		t.Fatalf("day-1 model registry seed rejected at the door: %v", err)
	}

	// The chains the docs page tables promise; update both together.
	want := map[string][]store.ModelCandidate{
		"claude-opus-4-8": {
			{Provider: "anthropic", ModelID: "claude-opus-4-8"},
			{Provider: "openrouter", ModelID: "anthropic/claude-opus-4.8"},
			{Provider: "amazon-bedrock", ModelID: "global.anthropic.claude-opus-4-8"},
		},
		"gpt-5-5": {
			{Provider: "openai-codex", ModelID: "gpt-5.5"},
			{Provider: "openai", ModelID: "gpt-5.5"},
			{Provider: "openrouter", ModelID: "openai/gpt-5.5"},
		},
		"gemini-3-1-pro": {
			{Provider: "google", ModelID: "gemini-3.1-pro-preview"},
			{Provider: "openrouter", ModelID: "google/gemini-3.1-pro-preview"},
		},
	}
	if len(reg.Entries) != len(want) {
		t.Errorf("seed has %d entries, want %d", len(reg.Entries), len(want))
	}
	for name, chain := range want {
		if got := reg.Entries[name].Candidates; !slices.Equal(got, chain) {
			t.Errorf("entry %q candidates = %+v, want %+v", name, got, chain)
		}
	}
}
