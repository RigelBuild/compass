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
// PutModelRegistryRequest, passes store validation, and holds the expected chains.
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

	// The docs page tables list these chains; keep the two in step by hand.
	want := map[string][]store.ModelCandidate{
		"claude-opus-5-5": {
			{Provider: "anthropic", ModelID: "claude-opus-5-5"},
			{Provider: "openrouter", ModelID: "anthropic/claude-opus-5.5"},
			{Provider: "amazon-bedrock", ModelID: "global.anthropic.claude-opus-5-5"},
		},
		"gpt-6-luna": {
			{Provider: "openai-codex", ModelID: "gpt-6-luna"},
			{Provider: "openai", ModelID: "gpt-6-luna"},
			{Provider: "openrouter", ModelID: "openai/gpt-6-luna"},
			{Provider: "amazon-bedrock", ModelID: "global.openai.gpt-6-luna"},
		},
		"gpt-6-sol": {
			{Provider: "openai-codex", ModelID: "gpt-6-sol"},
			{Provider: "openai", ModelID: "gpt-6-sol"},
			{Provider: "openrouter", ModelID: "openai/gpt-6-sol"},
			{Provider: "amazon-bedrock", ModelID: "global.openai.gpt-6-sol"},
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
