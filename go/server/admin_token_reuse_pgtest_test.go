//go:build pgtest && unix

package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/store"
)

// A restart must reuse the admin-token file while it is a private regular file
// whose token still resolves to the bootstrap admin, and mint a fresh one in
// every other case. Each case starts from its own first boot.
func TestIssueAndWriteAdminTokenReusesValidToken(t *testing.T) {
	ctx := context.Background()
	st, adminID, memberID := newNetworkStore(t)

	cases := []struct {
		name      string
		wantReuse bool
		setup     func(t *testing.T, path, tok string) // tok is the first boot's token
	}{
		{"valid file", true, func(*testing.T, string, string) {}},
		{"trailing newline", true, func(t *testing.T, path, tok string) {
			t.Helper()
			writeAdminTokenFile(t, path, tok+"\n")
		}},
		{"revoked token", false, func(t *testing.T, _, tok string) {
			t.Helper()
			if err := auth.RevokeToken(ctx, st, tok); err != nil {
				t.Fatal(err)
			}
		}},
		{"another account's token", false, func(t *testing.T, path, _ string) {
			t.Helper()
			other, err := auth.IssueAccountToken(ctx, st, memberID)
			if err != nil {
				t.Fatal(err)
			}
			writeAdminTokenFile(t, path, other)
		}},
		{"unknown token", false, func(t *testing.T, path, _ string) {
			t.Helper()
			writeAdminTokenFile(t, path, "not-a-token")
		}},
		{"empty file", false, func(t *testing.T, path, _ string) {
			t.Helper()
			writeAdminTokenFile(t, path, "")
		}},
		{"missing file", false, func(t *testing.T, path, _ string) {
			t.Helper()
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
		{"group-readable file", false, func(t *testing.T, path, _ string) {
			t.Helper()
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink to a valid token", false, func(t *testing.T, path, tok string) {
			t.Helper()
			elsewhere := filepath.Join(t.TempDir(), "elsewhere")
			writeAdminTokenFile(t, elsewhere, tok)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkAdminTokenRestart(t, st, adminID, tc.wantReuse, tc.setup)
		})
	}
}

// checkAdminTokenRestart runs a first boot, applies setup, restarts, and checks
// the file was reused untouched or replaced by a fresh 0600 admin token.
func checkAdminTokenRestart(t *testing.T, st *store.Store, adminID store.AccountID, wantReuse bool, setup func(t *testing.T, path, tok string)) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, adminTokenFile)
	if got, minted, err := issueAndWriteAdminToken(ctx, st, adminID, dir); err != nil || got != path || !minted {
		t.Fatalf("first boot = %q, minted=%v, %v; want %q, minted, nil", got, minted, err, path)
	}
	first := readAdminToken(t, dir)
	setup(t, path, first)
	var before os.FileInfo
	if wantReuse {
		before = lstat(t, path)
	}

	_, minted, err := issueAndWriteAdminToken(ctx, st, adminID, dir)
	if err != nil {
		t.Fatal(err)
	}
	after := lstat(t, path)
	if wantReuse {
		if minted || !os.SameFile(before, after) {
			t.Fatalf("minted=%v sameFile=%v; want the existing file reused untouched", minted, os.SameFile(before, after))
		}
		return
	}
	if !minted || !after.Mode().IsRegular() || after.Mode().Perm() != 0o600 {
		t.Fatalf("minted=%v mode=%v; want a fresh 0600 regular file", minted, after.Mode())
	}
	got := readAdminToken(t, dir)
	if got == first {
		t.Fatal("admin token was not re-minted")
	}
	subj, err := auth.ResolveToken(ctx, st, got, store.SubjectAccount)
	if err != nil || subj.ID != string(adminID) {
		t.Fatalf("re-minted token resolves to %+v, %v; want the admin", subj, err)
	}
}

func writeAdminTokenFile(t *testing.T, path, tok string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
		t.Fatal(err)
	}
}

func lstat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return info
}
