package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/forgecred"
)

func TestGHOwnerTokensScriptWritesOwnersAndLastDefault(t *testing.T) {
	home := t.TempDir()
	script, err := GHOwnerTokensScript(home, []GHCredentials{
		{Host: "github.com", Owner: "Owner-A", Token: "token-a"},
		{Host: "github.com", Owner: "Owner-B", Token: "token-b"},
		{Host: "github.com", Owner: "Owner-A", Token: "token-a-last"},
	})
	if err != nil {
		t.Fatalf("GHOwnerTokensScript: %v", err)
	}
	runScript(t, script)

	for _, tc := range []struct {
		owner string
		want  string
	}{
		{owner: "owner-a", want: "token-a-last"},
		{owner: "owner-b", want: "token-b"},
	} {
		path := forgecred.TokenPath(home, "github.com", tc.owner)
		got, err := os.ReadFile(path)
		if err != nil || string(got) != tc.want {
			t.Fatalf("owner token %q = (%q, %v), want %q", tc.owner, got, err, tc.want)
		}
		assert0600(t, path)
	}
	owner, ok, err := forgecred.DefaultOwner(home, "github.com")
	if err != nil || !ok || owner != "owner-a" {
		t.Fatalf("default owner = (%q, %t, %v), want (owner-a, true, nil)", owner, ok, err)
	}
}

func TestGHOwnerTokensScriptRemovesStaleOwners(t *testing.T) {
	home := t.TempDir()
	oldPath := forgecred.TokenPath(home, "github.com", "owner-old")
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := GHOwnerTokensScript(home, []GHCredentials{{Host: "github.com", Owner: "owner-new", Token: "token-new"}})
	if err != nil {
		t.Fatalf("GHOwnerTokensScript: %v", err)
	}
	runScript(t, script)
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("stale owner file stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(forgecred.TokenPath(home, "github.com", "owner-new")); err != nil {
		t.Fatalf("new owner token file: %v", err)
	}
}

func TestGHOwnerTokensScriptEmptySetRemovesOwnersAndMarker(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".compass", "forge", "github.com")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "owner-old"), forgecred.DefaultOwnerPath(home, "github.com")} {
		if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script, err := GHOwnerTokensScript(home, nil)
	if err != nil {
		t.Fatalf("GHOwnerTokensScript: %v", err)
	}
	runScript(t, script)
	if _, err := os.Stat(filepath.Join(home, ".compass", "forge")); !os.IsNotExist(err) {
		t.Fatalf("forge directory after empty cleanup = %v, want not exist", err)
	}
}

func TestGHOwnerTokensScriptEmptySetToleratesMissingForgeDirectory(t *testing.T) {
	home := t.TempDir()
	script, err := GHOwnerTokensScript(home, nil)
	if err != nil {
		t.Fatalf("GHOwnerTokensScript: %v", err)
	}
	runScript(t, script)
	if _, err := os.Stat(filepath.Join(home, ".compass", "forge")); !os.IsNotExist(err) {
		t.Fatalf("forge directory after empty cleanup = %v, want not exist", err)
	}
}

func TestGHOwnerTokensScriptKeepsSecretOutOfScript(t *testing.T) {
	token := "token-$(touch /tmp/not-run)\nend"
	script, err := GHOwnerTokensScript(t.TempDir(), []GHCredentials{{Host: "github.com", Owner: "owner-a", Token: token}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, token) || strings.Contains(script, "not-run") {
		t.Fatal("script contains secret bytes")
	}
}
