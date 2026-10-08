//go:build unix

package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// TestSpawnableRolesHaveShippedPrompt: if the shipped default bundle loses
// config/prompts/<role>/SYSTEM.md, a spawn of that role degrades to the default
// block-0 with only a warn, so a rename or delete must red here instead.
func TestSpawnableRolesHaveShippedPrompt(t *testing.T) {
	for role := range store.ManagerRoles() {
		t.Run(role, func(t *testing.T) {
			path := filepath.Join("..", "..", "config", "prompts", role, "SYSTEM.md")
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("shipped prompt for spawnable role %q: %v", role, err)
			}
			if !info.Mode().IsRegular() || info.Size() == 0 {
				t.Fatalf("shipped prompt %s is not a non-empty regular file (mode %v, size %d)", path, info.Mode(), info.Size())
			}
		})
	}
}
