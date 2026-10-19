package forgecred

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTokenPath(t *testing.T) {
	got := TokenPath("/home/agent", "github.com", "OwNeR-A")
	want := filepath.Join("/home/agent", ".compass", "forge", "github.com", "owner-a")
	if got != want {
		t.Fatalf("TokenPath() = %q, want %q", got, want)
	}
}

func TestDefaultOwnerPath(t *testing.T) {
	got := DefaultOwnerPath("/home/agent", "github.com")
	want := filepath.Join("/home/agent", ".compass", "forge", "github.com", ".default-owner")
	if got != want {
		t.Fatalf("DefaultOwnerPath() = %q, want %q", got, want)
	}
}

func TestLookupToken(t *testing.T) {
	home := t.TempDir()
	path := TokenPath(home, "github.com", "Owner-A")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("token-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok, err := LookupToken(home, "github.com", "owner-a")
	if err != nil || !ok || got != "token-a" {
		t.Fatalf("LookupToken() = (%q, %t, %v), want (token-a, true, nil)", got, ok, err)
	}
	if _, ok, err := LookupToken(home, "github.com", "missing"); err != nil || ok {
		t.Fatalf("LookupToken(missing) = (_, %t, %v), want (_, false, nil)", ok, err)
	}
}

func TestDefaultOwner(t *testing.T) {
	home := t.TempDir()
	if owner, ok, err := DefaultOwner(home, "github.com"); err != nil || ok {
		t.Fatalf("DefaultOwner(absent) = (%q, %t, %v), want empty, false, nil", owner, ok, err)
	}
	path := DefaultOwnerPath(home, "github.com")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("Owner-A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner, ok, err := DefaultOwner(home, "github.com")
	if err != nil || !ok || owner != "owner-a" {
		t.Fatalf("DefaultOwner() = (%q, %t, %v), want (owner-a, true, nil)", owner, ok, err)
	}
}

func TestOwnerFromCredentialPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
		ok   bool
	}{
		{name: "repo path", path: "owner-a/repo", want: "owner-a", ok: true},
		{name: "git suffix", path: "owner-b/repo.git", want: "owner-b", ok: true},
		{name: "no owner", path: "repo", ok: false},
		{name: "empty", ok: false},
		{name: "trailing slash", path: "owner-a/", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := OwnerFromCredentialPath(tt.path)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("OwnerFromCredentialPath(%q) = (%q, %t), want (%q, %t)", tt.path, got, ok, tt.want, tt.ok)
			}
		})
	}
}
