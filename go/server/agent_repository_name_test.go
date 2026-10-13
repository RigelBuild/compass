package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

func TestAgentRepositoryNameLimits(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		ok        bool
	}{
		{"owner at 39", strings.Repeat("a", 39) + "/repo", true},
		{"owner over 39", strings.Repeat("a", 40) + "/repo", false},
		{"repository at 100", "org/" + strings.Repeat("r", 100), true},
		{"repository over 100", "org/" + strings.Repeat("r", 101), false},
		{"dot repository", "org/.", false},
		{"dot-dot repository", "org/..", false},
		{"leading hyphen owner", "-org/repo", false},
		{"dotted repository", "org/.github", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := agentRepository(tc.raw)
			if tc.ok && err != nil {
				t.Fatalf("agentRepository(%q) = %v, want ok", tc.raw, err)
			}
			if !tc.ok && !errors.Is(err, store.ErrInvalidArgument) {
				t.Fatalf("agentRepository(%q) = %v, want ErrInvalidArgument", tc.raw, err)
			}
		})
	}
}
