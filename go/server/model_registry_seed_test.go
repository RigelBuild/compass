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

// TestDay1ModelRegistrySeedPassesDoor pins the committed seed as a Connect-JSON
// PutModelRegistry body the door accepts, so the documented curl keeps working.
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
	if err := store.ValidateModelRegistry(registryFromProto(req.GetRegistry())); err != nil {
		t.Fatalf("day-1 model registry seed rejected at the door: %v", err)
	}

	// The chains the docs page tables promise; update both together.
	want := map[string][]string{
		"claude-opus-4-8": {"anthropic/claude-opus-4-8", "openrouter/anthropic/claude-opus-4.8", "amazon-bedrock/global.anthropic.claude-opus-4-8"},
		"gpt-5-5":         {"openai-codex/gpt-5.5", "openai/gpt-5.5", "openrouter/openai/gpt-5.5"},
		"gemini-3-1-pro":  {"google/gemini-3.1-pro-preview", "openrouter/google/gemini-3.1-pro-preview"},
	}
	entries := req.GetRegistry().GetEntries()
	if len(entries) != len(want) {
		t.Errorf("seed has %d entries, want %d", len(entries), len(want))
	}
	for name, chain := range want {
		var got []string
		for _, c := range entries[name].GetCandidates() {
			got = append(got, c.GetProvider()+"/"+c.GetModelId())
		}
		if !slices.Equal(got, chain) {
			t.Errorf("entry %q candidates = %v, want %v", name, got, chain)
		}
	}
}
