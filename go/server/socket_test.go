//go:build unix

package server

// Tests for the socket-door helpers in socket.go: private-parent creation,
// stale-socket handling, inode-checked cleanup, and parentDir. Hermetic: every
// path is under t.TempDir().

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParentDir(t *testing.T) {
	tests := []struct {
		name       string
		socketPath string
		want       string
	}{
		{"bare filename has no parent to create", "compass.sock", ""},
		{"nested path returns its directory", "/run/compass/compass.sock", "/run/compass"},
		{"relative nested path returns its directory", "sub/compass.sock", "sub"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parentDir(tc.socketPath); got != tc.want {
				t.Fatalf("parentDir(%q) = %q, want %q", tc.socketPath, got, tc.want)
			}
		})
	}
}

func TestDefaultSocketPath(t *testing.T) {
	// Every path is built with filepath.Join so the expectations match the
	// impl's own construction and stay OS-path-correct. Env is set per-subtest
	// with t.Setenv (auto-restored); because t.Setenv is used, no t.Parallel().
	tests := []struct {
		name    string
		xdg     string
		home    string
		want    string
		wantErr bool
	}{
		{
			// XDG_RUNTIME_DIR set non-empty wins over HOME (XDG precedence): the
			// impl returns from the XDG branch before ever reading HOME.
			name: "xdg set wins over home",
			xdg:  "/run/user/1000",
			home: "/home/x",
			want: filepath.Join("/run/user/1000", "compass", "server.sock"),
		},
		{
			// LOAD-BEARING INVARIANT: an empty XDG_RUNTIME_DIR counts as unset and
			// falls back to HOME, rather than resolving a bogus /compass/... path.
			// If the impl's `!= ""` check were weakened to a mere presence check
			// (e.g. LookupEnv truthiness), an empty XDG would bind
			// /compass/server.sock and this case's want (the HOME path) would
			// fail — that is the tooth here.
			name: "empty xdg counts as unset and falls back to home",
			xdg:  "",
			home: "/home/x",
			want: filepath.Join("/home/x", ".compass", "server.sock"),
		},
		{
			// XDG "unset": Go's t.Setenv cannot truly unset a var, but the impl
			// treats empty as the fallback trigger, so setting "" is the
			// equivalent path — same fallback branch, same expected result.
			name: "xdg unset falls back to home",
			xdg:  "",
			home: "/home/x",
			want: filepath.Join("/home/x", ".compass", "server.sock"),
		},
		{
			// Neither XDG nor HOME available: the impl surfaces an error and an
			// empty path.
			name:    "both unset errors on missing home",
			xdg:     "",
			home:    "",
			wantErr: true,
		},
		{
			// LOAD-BEARING INVARIANT: a non-empty but RELATIVE XDG_RUNTIME_DIR is
			// not absolute, so it counts as unset and falls back to HOME — a
			// relative value would otherwise resolve a cwd-dependent socket path,
			// the footgun the impl guards against. If the impl weakened its
			// filepath.IsAbs check to a mere `!= ""`, this would bind
			// "runrel/compass/server.sock" (cwd-relative) and this case's want
			// (the HOME path) would fail — that is the tooth.
			name: "relative xdg counts as unset and falls back to home",
			xdg:  "runrel",
			home: "/home/x",
			want: filepath.Join("/home/x", ".compass", "server.sock"),
		},
		{
			// A non-empty but RELATIVE HOME is not absolute either: rather than
			// resolving a cwd-relative ".compass/server.sock", the impl surfaces
			// an error. Same absolute-path filter as XDG, applied to HOME.
			name:    "relative home errors rather than resolving cwd-relative path",
			xdg:     "",
			home:    "homerel",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", tc.xdg)
			t.Setenv("HOME", tc.home)
			got, err := DefaultSocketPath()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("DefaultSocketPath() error = nil, want non-nil")
				}
				if !strings.Contains(err.Error(), "HOME") {
					t.Fatalf("DefaultSocketPath() error = %q, want it to mention HOME", err)
				}
				if got != "" {
					t.Fatalf("DefaultSocketPath() path = %q, want empty on error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("DefaultSocketPath() unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("DefaultSocketPath() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEnsurePrivateDirCreatesMissingAncestorsAt0700(t *testing.T) {
	root := t.TempDir()
	// Three not-yet-existing levels: each one ensurePrivateDir creates must be
	// 0700, so a socket placed inside is never briefly world-traversable.
	target := filepath.Join(root, "a", "b", "c")
	if err := ensurePrivateDir(target); err != nil {
		t.Fatalf("ensurePrivateDir: %v", err)
	}
	for _, dir := range []string{
		filepath.Join(root, "a"),
		filepath.Join(root, "a", "b"),
		filepath.Join(root, "a", "b", "c"),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("%s mode = %o, want 0700", dir, perm)
		}
	}
}

func TestEnsurePrivateDirLeavesExistingDirModeUntouched(t *testing.T) {
	root := t.TempDir()
	// An operator-provisioned parent at a deliberately looser mode.
	existing := filepath.Join(root, "shared")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatalf("pre-create: %v", err)
	}
	// ensurePrivateDir on the existing dir must not rewrite its mode...
	if err := ensurePrivateDir(existing); err != nil {
		t.Fatalf("ensurePrivateDir(existing): %v", err)
	}
	if perm := statPerm(t, existing); perm != 0o755 {
		t.Fatalf("existing dir mode = %o, want 0755 (untouched)", perm)
	}
	// ...even when creating a fresh child beneath it: the child is 0700, the
	// pre-existing parent keeps 0755.
	child := filepath.Join(existing, "child")
	if err := ensurePrivateDir(child); err != nil {
		t.Fatalf("ensurePrivateDir(child): %v", err)
	}
	if perm := statPerm(t, existing); perm != 0o755 {
		t.Fatalf("parent mode after child create = %o, want 0755 (untouched)", perm)
	}
	if perm := statPerm(t, child); perm != 0o700 {
		t.Fatalf("child mode = %o, want 0700", perm)
	}
}

func TestEnsurePrivateDirPinsModeUnderRestrictiveUmask(t *testing.T) {
	// umask is process-global and not goroutine-safe, so this test must not run
	// in parallel with any other umask-touching test (no t.Parallel).
	// Create the tempdir root before tightening the umask — t.TempDir() itself
	// mkdirs under this umask, and 0o777 would leave it inaccessible.
	root := t.TempDir()
	// The pathological umask: 0o777 masks off every permission bit, so a plain
	// os.Mkdir(_, 0o700) would land 0000. ensurePrivateDir's explicit chmod
	// after each mkdir must pin 0700 regardless.
	prev := syscall.Umask(0o777)
	defer syscall.Umask(prev)

	target := filepath.Join(root, "a", "b", "c")
	if err := ensurePrivateDir(target); err != nil {
		t.Fatalf("ensurePrivateDir: %v", err)
	}
	for _, dir := range []string{
		filepath.Join(root, "a"),
		filepath.Join(root, "a", "b"),
		filepath.Join(root, "a", "b", "c"),
	} {
		if perm := statPerm(t, dir); perm != 0o700 {
			t.Fatalf("%s mode = %o, want 0700 (chmod must pin past the umask)", dir, perm)
		}
	}
}

func TestListenUnixPrivateBirthsSocket0600UnderPermissiveUmask(t *testing.T) {
	// umask is process-global and not goroutine-safe: no t.Parallel.
	// A fully permissive umask (0): a plain net.Listen would create the socket
	// 0666 & ^0 == 0666, connectable by other users before any chmod tightens
	// it. listenUnixPrivate binds under a temporary 0177 umask so the socket is
	// born 0600 with no such window.
	prev := syscall.Umask(0)
	defer syscall.Umask(prev)

	path := filepath.Join(t.TempDir(), "compass.sock")
	l, err := listenUnixPrivate(path)
	if err != nil {
		t.Fatalf("listenUnixPrivate: %v", err)
	}
	defer l.Close()

	// Assert the birth mode immediately — before any external chmod — so this
	// proves the umask-guarded bind, not a later tightening step.
	if perm := statPerm(t, path); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 0600 (born owner-only under the guarded umask)", perm)
	}
}

func TestListenUnixPrivateRetainsSocketOnClose(t *testing.T) {
	// listenUnixPrivate disables Go's default unlink-on-close on the returned
	// *net.UnixListener, so the socket file is removed ONLY through the
	// inode-guarded cleanupSocket. If unlink-on-close were left at Go's default
	// true, Close() would unconditionally delete the path and clobber a successor
	// server's rebound socket when this server drains, defeating the inode guard.
	path := filepath.Join(t.TempDir(), "s.sock")
	l, err := listenUnixPrivate(path)
	if err != nil {
		t.Fatalf("listenUnixPrivate: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket file missing right after bind: %v", err)
	}

	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The regression assertion: with unlink-on-close disabled, Close leaves the
	// socket file on disk. This fails if the SetUnlinkOnClose(false) call is
	// dropped or set back to true.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket file removed by Close (%v); listenUnixPrivate must disable unlink-on-close so removal is cleanupSocket's job alone", err)
	}

	// Removal is normally cleanupSocket's job; clean up here so temp-dir
	// teardown is quiet.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("cleanup remove: %v", err)
	}
}

func TestClearStaleSocketNoopWhenNothingAtPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compass.sock")
	if err := clearStaleSocket(path); err != nil {
		t.Fatalf("clearStaleSocket on missing path = %v, want nil", err)
	}
}

func TestClearStaleSocketRefusesNonSocketFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-socket")
	if err := os.WriteFile(path, []byte("regular file"), 0o600); err != nil {
		t.Fatalf("write regular file: %v", err)
	}
	if err := clearStaleSocket(path); err == nil {
		t.Fatal("clearStaleSocket on a regular file = nil, want refusal error")
	}
	// It refused rather than deleted: the file must survive.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("regular file was removed (%v); clearStaleSocket must refuse, not delete", err)
	}
}

func TestClearStaleSocketRemovesStaleSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compass.sock")
	// Bind a listener, then close it WITHOUT unlinking so the socket inode is
	// deterministically left on disk with no server behind it — a genuinely
	// stale socket a connect would refuse. net.Listen("unix") defaults to
	// unlink-on-close, which would usually remove the file and leave this
	// test's removal path unexercised; SetUnlinkOnClose(false) pins the stale
	// state so the assertion always runs.
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	// The stale socket must still be on disk now — that is the precondition
	// the removal path is meant to handle.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale socket not retained after close (%v); SetUnlinkOnClose(false) should keep it", err)
	}
	if err := clearStaleSocket(path); err != nil {
		t.Fatalf("clearStaleSocket on stale socket = %v, want nil", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale socket still present after clear (stat err = %v)", err)
	}
}

func TestClearStaleSocketRefusesLivePeer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compass.sock")
	// A real listener answering connects: clearStaleSocket must refuse to start
	// on top of it and must NOT remove it.
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer l.Close()
	// Accept loop so the connect in clearStaleSocket succeeds.
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	if err := clearStaleSocket(path); err == nil {
		t.Fatal("clearStaleSocket over a live peer = nil, want refusal error")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("live socket was removed (%v); clearStaleSocket must refuse, not delete", err)
	}
}

func TestClearStaleSocketPropagatesNonRefusedProbeError(t *testing.T) {
	// A connect probe that fails with anything OTHER than ECONNREFUSED/ENOENT
	// (here EACCES) proves nothing about staleness, so clearStaleSocket must
	// propagate the error and leave the socket file untouched — never unlink a
	// socket whose liveness is uncertain. Mechanism: bind a real Unix listener,
	// then chmod the socket 0000 so the kernel denies connect() with EACCES
	// (permission is checked before any refused/accepted decision), while the
	// listener stays live underneath.
	if os.Geteuid() == 0 {
		t.Skip("root bypasses socket permission checks, so a 0000 connect is not denied with EACCES")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "compass.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer l.Close()
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatalf("chmod 0000: %v", err)
	}

	// Confirm the environment actually denies connect with a non-refused error;
	// if some kernel/uid still permits it, the contract under test cannot be
	// exercised, so skip rather than assert a false result.
	if c, derr := net.DialTimeout("unix", path, 250*time.Millisecond); derr == nil {
		c.Close()
		t.Skip("connect to 0000 socket unexpectedly succeeded; cannot induce a non-refused probe error here")
	} else if errors.Is(derr, syscall.ECONNREFUSED) || errors.Is(derr, syscall.ENOENT) {
		t.Skipf("connect to 0000 socket gave a stale-signaling error (%v); cannot induce a non-refused probe error here", derr)
	}

	if err := clearStaleSocket(path); err == nil {
		t.Fatal("clearStaleSocket over a non-refused probe error = nil, want the probe error propagated")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket was removed (%v) on a non-refused probe error; clearStaleSocket must propagate, not delete", err)
	}
}

func TestCleanupSocketRemovesOwnSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compass.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer l.Close()

	inode, ok := socketInode(path)
	if !ok {
		t.Fatal("socketInode failed right after bind")
	}
	cleanupSocket(path, inode, true)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("own socket not removed by cleanup (stat err = %v)", err)
	}
}

func TestCleanupSocketLeavesSuccessorRebindIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compass.sock")

	// Bind, record the inode, then simulate our socket disappearing and a
	// successor server rebinding the same path to a DIFFERENT inode.
	l1, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("bind #1: %v", err)
	}
	boundInode, ok := socketInode(path)
	if !ok {
		t.Fatal("socketInode failed after bind #1")
	}
	l1.Close()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove #1: %v", err)
	}

	l2, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("bind #2 (successor): %v", err)
	}
	defer l2.Close()
	successorInode, ok := socketInode(path)
	if !ok {
		t.Fatal("socketInode failed after successor bind")
	}
	if successorInode == boundInode {
		t.Skip("rebind reused the same inode; cannot distinguish successor on this fs")
	}

	// Our cleanup, pinned to the old inode, must NOT delete the successor's live
	// socket.
	cleanupSocket(path, boundInode, true)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("successor socket was removed (%v); cleanup must guard on inode match", err)
	}
}

func TestCleanupSocketNoopWhenInodeNeverPinned(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compass.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer l.Close()

	// boundOK=false models socketInode having failed right after bind: cleanup
	// must leave the file alone rather than delete on an unproven inode.
	cleanupSocket(path, 0, false)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket removed despite unpinned inode (%v); cleanup must no-op", err)
	}
}

func statPerm(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}
