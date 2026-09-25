//go:build unix

package server

import (
	"os"
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

	entries := req.GetRegistry().GetEntries()
	if len(entries) == 0 {
		t.Fatal("day-1 model registry seed has no entries")
	}
	for name, entry := range entries {
		candidates := entry.GetCandidates()
		if len(candidates) == 0 {
			t.Fatalf("entry %q has no candidates", name)
		}
		switch provider := candidates[0].GetProvider(); provider {
		case "anthropic", "openai", "openai-codex", "google":
		default:
			t.Errorf("entry %q first candidate provider = %q, want a day-1 provider", name, provider)
		}
	}
}
