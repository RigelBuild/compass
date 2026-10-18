//go:build pgtest && unix

package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/pgtest"
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
	if got, minted, err := issueAndWriteAdminToken(ctx, st, adminID, dir, ""); err != nil || got != path || !minted {
		t.Fatalf("first boot = %q, minted=%v, %v; want %q, minted, nil", got, minted, err, path)
	}
	first := readAdminToken(t, dir)
	setup(t, path, first)
	var before os.FileInfo
	if wantReuse {
		before = lstat(t, path)
	}

	_, minted, err := issueAndWriteAdminToken(ctx, st, adminID, dir, "")
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

func TestIssueAndWriteAdminTokenRegistersSuppliedToken(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	member, err := st.CreateUser(ctx, store.NewUser{Handle: "member", DisplayName: "member"})
	if err != nil {
		t.Fatalf("CreateUser(member): %v", err)
	}
	token := newSuppliedAdminToken(t)
	tokenFile := filepath.Join(t.TempDir(), "operator-token")
	writeAdminTokenFile(t, tokenFile, token)
	stateDir := t.TempDir()

	path, minted, err := issueAndWriteAdminToken(ctx, st, admin.ID, stateDir, tokenFile)
	if err != nil || minted || path != filepath.Join(stateDir, adminTokenFile) {
		t.Fatalf("first supplied-token boot = (%q, minted=%v, %v), want state path and minted=false", path, minted, err)
	}
	if got := readAdminToken(t, stateDir); got != token {
		t.Fatal("state-dir token file does not match supplied token")
	}
	if err := assertAdminTokenAuthenticates(ctx, st, admin.ID, token); err != nil {
		t.Fatal(err)
	}
	before := lstat(t, filepath.Join(stateDir, adminTokenFile))
	if _, minted, err := issueAndWriteAdminToken(ctx, st, admin.ID, stateDir, tokenFile); err != nil || minted {
		t.Fatalf("restart supplied token = minted=%v, %v; want minted=false", minted, err)
	}
	after := lstat(t, filepath.Join(stateDir, adminTokenFile))
	if !os.SameFile(before, after) {
		t.Fatal("restart rewrote identical state-dir token file")
	}
	var rows int
	if err := countTokenRows(ctx, t, dsn, &rows); err != nil {
		t.Fatalf("count token rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("token rows = %d, want one", rows)
	}
	restarted, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(restarted.Close)
	restartedAdmin, err := restarted.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin after restart: %v", err)
	}
	if _, minted, err := issueAndWriteAdminToken(ctx, restarted, restartedAdmin.ID, stateDir, tokenFile); err != nil || minted {
		t.Fatalf("second boot with fresh Store = minted=%v, %v; want minted=false", minted, err)
	}
	if err := assertAdminTokenAuthenticates(ctx, restarted, restartedAdmin.ID, token); err != nil {
		t.Fatal(err)
	}

	// A fresh isolated schema models database recreation: the operator file seeds the new admin binding.
	recreatedDSN := pgtest.RequireDSN(t)
	recreated, err := store.Open(ctx, recreatedDSN)
	if err != nil {
		t.Fatalf("open recreated database: %v", err)
	}
	t.Cleanup(recreated.Close)
	recreatedAdmin, err := recreated.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin after database recreate: %v", err)
	}
	if _, minted, err := issueAndWriteAdminToken(ctx, recreated, recreatedAdmin.ID, stateDir, tokenFile); err != nil || minted {
		t.Fatalf("boot after database recreate = minted=%v, %v; want minted=false", minted, err)
	}
	if err := assertAdminTokenAuthenticates(ctx, recreated, recreatedAdmin.ID, token); err != nil {
		t.Fatal(err)
	}

	if err := auth.RevokeToken(ctx, st, token); err != nil {
		t.Fatalf("revoke supplied token: %v", err)
	}
	if _, _, err := issueAndWriteAdminToken(ctx, st, admin.ID, stateDir, tokenFile); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("issueAndWriteAdminToken for revoked token = %v, want startup failure naming revoke", err)
	}

	memberToken := newSuppliedAdminToken(t)
	if err := auth.RegisterAccountToken(ctx, st, member.ID, memberToken); err != nil {
		t.Fatalf("register member token: %v", err)
	}
	memberFile := filepath.Join(t.TempDir(), "member-token")
	writeAdminTokenFile(t, memberFile, memberToken)
	if _, _, err := issueAndWriteAdminToken(ctx, st, admin.ID, stateDir, memberFile); err == nil || !strings.Contains(err.Error(), "another subject") {
		t.Fatalf("issueAndWriteAdminToken for member token = %v, want startup failure naming subject mismatch", err)
	}
}

func TestIssueAndWriteAdminTokenReplacesSuppliedStateFiles(t *testing.T) {
	ctx := context.Background()
	st, adminID, _ := newNetworkStore(t)
	cases := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{"different valid token", func(t *testing.T, path string) {
			t.Helper()
			token, err := auth.IssueAccountToken(ctx, st, adminID)
			if err != nil {
				t.Fatalf("issue pre-existing admin token: %v", err)
			}
			writeAdminTokenFile(t, path, token)
		}},
		{"symlink", func(t *testing.T, path string) {
			t.Helper()
			target := filepath.Join(t.TempDir(), "target")
			writeAdminTokenFile(t, target, newSuppliedAdminToken(t))
			if err := os.Symlink(target, path); err != nil {
				t.Fatalf("create state token symlink: %v", err)
			}
		}},
		{"group readable", func(t *testing.T, path string) {
			t.Helper()
			writeAdminTokenFile(t, path, newSuppliedAdminToken(t))
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatalf("chmod pre-existing state token: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := newSuppliedAdminToken(t)
			tokenFile := filepath.Join(t.TempDir(), "operator-token")
			writeAdminTokenFile(t, tokenFile, token)
			stateDir := t.TempDir()
			statePath := filepath.Join(stateDir, adminTokenFile)
			tc.setup(t, statePath)

			if _, minted, err := issueAndWriteAdminToken(ctx, st, adminID, stateDir, tokenFile); err != nil || minted {
				t.Fatalf("issueAndWriteAdminToken = minted=%v, %v; want minted=false", minted, err)
			}
			info, err := os.Lstat(statePath)
			if err != nil {
				t.Fatalf("lstat resulting state token: %v", err)
			}
			if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
				t.Fatalf("state token mode = %v, want regular file mode 0600", info.Mode())
			}
			if got := readAdminToken(t, stateDir); got != token {
				t.Fatal("state token file does not contain supplied token")
			}
		})
	}
}

func TestIssueAndWriteAdminTokenRejectsMalformedSuppliedFiles(t *testing.T) {
	ctx := context.Background()
	st, adminID, _ := newNetworkStore(t)
	cases := []struct {
		name    string
		mode    os.FileMode
		body    string
		fifo    bool
		symlink bool
	}{
		{name: "malformed", mode: 0o600, body: "not-base64!"},
		{name: "embedded newline", mode: 0o600, body: "AAAAAAAAAAAAAAAAAAAA\nAAAAAAAAAAAAAAAAAAAAAAA"},
		{name: "wrong length", mode: 0o600, body: base64.RawURLEncoding.EncodeToString([]byte("short"))},
		{name: "group writable", mode: 0o660, body: newSuppliedAdminToken(t)},
		{name: "other readable", mode: 0o604, body: newSuppliedAdminToken(t)},
		{name: "fifo", fifo: true},
		{name: "symlink to fifo", fifo: true, symlink: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tokenFile := filepath.Join(t.TempDir(), "operator-token")
			if tc.fifo {
				makeSuppliedTokenFIFO(t, tokenFile, tc.symlink)
			} else {
				writeSuppliedTokenFile(t, tokenFile, tc.body, tc.mode)
			}

			check := func() error {
				_, _, err := issueAndWriteAdminToken(ctx, st, adminID, t.TempDir(), tokenFile)
				return err
			}
			var err error
			if tc.fifo {
				err = checkSuppliedAdminTokenFIFO(t, tokenFile, check)
				if err == nil || !strings.Contains(err.Error(), "must be a private regular file") {
					t.Fatalf("issueAndWriteAdminToken error = %v, want private regular-file failure for %q", err, tokenFile)
				}
				return
			}
			err = check()
			if err == nil || !strings.Contains(err.Error(), tokenFile) {
				t.Fatalf("issueAndWriteAdminToken error = %v, want failure naming path %q", err, tokenFile)
			}
		})
	}
}

func makeSuppliedTokenFIFO(t *testing.T, tokenFile string, symlink bool) {
	t.Helper()
	target := tokenFile
	if symlink {
		target = filepath.Join(t.TempDir(), "fifo-token")
	}
	if err := syscall.Mkfifo(target, 0o600); err != nil {
		t.Fatalf("create supplied token FIFO: %v", err)
	}
	if !symlink {
		return
	}
	if err := os.Symlink(target, tokenFile); err != nil {
		t.Fatalf("create supplied token symlink: %v", err)
	}
}

func writeSuppliedTokenFile(t *testing.T, tokenFile, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(tokenFile, []byte(body), 0o600); err != nil {
		t.Fatalf("write supplied token file: %v", err)
	}
	if err := os.Chmod(tokenFile, mode); err != nil {
		t.Fatalf("chmod supplied token file: %v", err)
	}
}

func checkSuppliedAdminTokenFIFO(t *testing.T, tokenFile string, check func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- check()
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		writer, err := os.OpenFile(tokenFile, os.O_WRONLY, 0)
		if err != nil {
			t.Errorf("open FIFO to release timed-out reader: %v", err)
			return errors.New("supplied admin token FIFO read exceeded 5 seconds")
		}
		if err := writer.Close(); err != nil {
			t.Errorf("close FIFO writer: %v", err)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("supplied admin token FIFO reader did not stop after release")
		}
		return errors.New("supplied admin token FIFO read exceeded 5 seconds")
	}
}

func TestIssueAndWriteAdminTokenRegistersSecretVolumeToken(t *testing.T) {
	cases := []struct {
		name string
		gid  func(t *testing.T) int
	}{
		{name: "effective gid", gid: func(*testing.T) int { return os.Getegid() }},
		{name: "supplementary gid", gid: supplementaryGID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertSecretVolumeTokenBoots(t, tc.gid(t))
		})
	}
}

// supplementaryGID returns a held group other than the effective one, as
// kubelet's fsGroup is when it differs from runAsGroup.
func supplementaryGID(t *testing.T) int {
	t.Helper()
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatalf("read supplementary groups: %v", err)
	}
	for _, gid := range groups {
		if gid != os.Getegid() {
			return gid
		}
	}
	t.Skip("process holds no supplementary group besides its effective gid")
	return 0
}

func assertSecretVolumeTokenBoots(t *testing.T, gid int) {
	t.Helper()
	ctx := context.Background()
	st, adminID, _ := newNetworkStore(t)
	volumeDir := t.TempDir()
	dataDir := filepath.Join(volumeDir, "..data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("create projected Secret data dir: %v", err)
	}
	token := newSuppliedAdminToken(t)
	target := filepath.Join(dataDir, "token")
	if err := os.WriteFile(target, []byte(token), 0o440); err != nil {
		t.Fatalf("write projected Secret token: %v", err)
	}
	if err := os.Chown(target, -1, gid); err != nil {
		t.Fatalf("set projected Secret group: %v", err)
	}
	if err := os.Symlink(filepath.Join("..data", "token"), filepath.Join(volumeDir, "token")); err != nil {
		t.Fatalf("create projected Secret symlink: %v", err)
	}

	stateDir := t.TempDir()
	path, minted, err := issueAndWriteAdminToken(ctx, st, adminID, stateDir, filepath.Join(volumeDir, "token"))
	if err != nil || minted || path != filepath.Join(stateDir, adminTokenFile) {
		t.Fatalf("Secret-volume boot = (%q, minted=%v, %v), want state path and minted=false", path, minted, err)
	}
	if err := assertAdminTokenAuthenticates(ctx, st, adminID, token); err != nil {
		t.Fatalf("projected Secret token does not authenticate as bootstrap admin: %v", err)
	}
	info := lstat(t, path)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("persisted token mode = %v, want regular 0600 file", info.Mode())
	}
	if got := readAdminToken(t, stateDir); got != token {
		t.Fatal("state-dir token file does not match projected Secret token")
	}
}

func assertAdminTokenAuthenticates(ctx context.Context, st *store.Store, adminID store.AccountID, token string) error {
	subj, err := auth.ResolveToken(ctx, st, token, store.SubjectAccount)
	if err != nil {
		return err
	}
	if subj.ID != string(adminID) {
		return fmt.Errorf("resolved account = %q, want bootstrap admin %q", subj.ID, adminID)
	}
	return nil
}

func countTokenRows(ctx context.Context, t *testing.T, dsn string, rows *int) error {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open token-count pool: %w", err)
	}
	t.Cleanup(pool.Close)
	return pool.QueryRow(ctx, "SELECT count(*) FROM tokens").Scan(rows)
}

func newSuppliedAdminToken(t *testing.T) string {
	t.Helper()
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("generate supplied token: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:])
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
