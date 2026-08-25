//go:build unix

package gateway

// Hermetic suite for the Runner-side per-container agent socket listener
// (SEA-1351 T2). White-box (package gateway) so it can drive the unexported
// listenAgentSocket / reclaimStaleSocket and the runnerUID seam directly.
//
// The listener is a security boundary: the socket lives in an owner-only dir at
// mode 0600, and stale-socket recovery must reclaim ONLY a socket this Runner
// owns while never deleting any other inode that happens to sit at the path.
// Because the production code already exists, fail-first for the three security
// guards (cases 5/6/7) is shown by mutation-revert, recorded in the task report.
//
// No wall-clock sleeps: readiness and in-flight state are event-gated on
// channels; the one bounded deadline (case 9) is the production drain grace
// itself, injected via a short-deadline context, not a settle-sleep.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// testTimeout bounds every blocking wait so a wedged handler or goroutine fails
// the test fast instead of hanging the suite. It is a deadline safety net, never
// a synchronization device.
const testTimeout = 15 * time.Second

// socketPath returns a socket path under a fresh, not-yet-created "run" subdir
// of the test's temp dir, so listenAgentSocket exercises its MkdirAll + chmod of
// the parent (the tempdir root already exists at 0700).
func socketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "run", "agent.sock")
}

// stubHandler mounts the Unimplemented AgentGateway on a mux — a live door that
// answers every Comms with CodeUnimplemented, proving the server serves h rather
// than merely binding the socket.
func stubHandler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(compassv1internalconnect.NewAgentGatewayHandler(compassv1internalconnect.UnimplementedAgentGatewayHandler{}))
	return mux
}

// agentClient builds a real generated AgentGatewayClient that dials the unix
// socket at path over prior-knowledge h2c — the same cleartext-HTTP/2 door the
// listener serves — so the handler is exercised over the wire it ships on. The
// base URL is a placeholder; every dial is routed to the socket by DialContext.
func agentClient(t *testing.T, path string) compassv1internalconnect.AgentGatewayClient {
	t.Helper()
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{
		Protocols: p,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return compassv1internalconnect.NewAgentGatewayClient(&http.Client{Transport: tr}, "http://unix")
}

// callComms issues one Comms RPC over the socket and returns its error (nil on
// an unexpected success).
func callComms(ctx context.Context, c compassv1internalconnect.AgentGatewayClient) error {
	_, err := c.Comms(ctx, connect.NewRequest(&compassv1.CommsCallRequest{CallId: "probe"}))
	return err
}

// leaveOwnedSocket binds and then closes a unix listener at path with
// unlink-on-close disabled, so the socket file persists on disk owned by this
// process's uid — a faithful simulation of a socket a crashed Runner left
// behind. It fails the test if the leftover is not a socket we own.
func leaveOwnedSocket(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), socketDirMode); err != nil {
		t.Fatalf("pre-creating socket dir: %v", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("binding leftover socket: %v", err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("closing leftover listener: %v", err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("leftover socket did not persist on disk: %v", err)
	}
	if fi.Mode().Type() != os.ModeSocket {
		t.Fatalf("leftover object is %s, want a socket", fi.Mode().Type())
	}
}

// Case 1. A dial before the listener exists must fail: this proves the dial
// helper actually reaches the socket path, so a later green in case 3 is a real
// end-to-end success rather than a client that never connected.
func TestDialBeforeListenFails(t *testing.T) {
	path := socketPath(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := callComms(ctx, agentClient(t, path)); err == nil {
		t.Fatal("Comms to an unlistened socket path succeeded; the dial helper is not reaching the socket")
	}
}

// Case 2. The socket's whole lifecycle: absent before create; a 0600 socket in a
// 0700 dir after listen; absent again after Close. A regression that leaked the
// socket mode, the dir mode, or the file itself breaks exactly one of these.
func TestLifecycleAbsentPresentGone(t *testing.T) {
	path := socketPath(t)

	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("before listen: Lstat(%q) = %v, want ErrNotExist", path, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	l, err := listenAgentSocket(ctx, path, stubHandler(t), func() {})
	if err != nil {
		t.Fatalf("listenAgentSocket: %v", err)
	}

	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("after listen: Lstat(%q): %v", path, err)
	}
	if fi.Mode().Type() != os.ModeSocket {
		t.Fatalf("after listen: type = %s, want socket", fi.Mode().Type())
	}
	if perm := fi.Mode().Perm(); perm != socketFileMode {
		t.Fatalf("after listen: socket perm = %o, want %o", perm, socketFileMode)
	}
	dfi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("after listen: Stat(dir): %v", err)
	}
	if perm := dfi.Mode().Perm(); perm != socketDirMode {
		t.Fatalf("after listen: dir perm = %o, want %o", perm, socketDirMode)
	}

	if err := l.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("after Close: Lstat(%q) = %v, want ErrNotExist", path, err)
	}
}

// Case 3. The listener serves the mounted handler over the socket: a Comms call
// reaches the Unimplemented stub and comes back CodeUnimplemented. This is the
// GREEN end-to-end — the door is live and serving h, not merely bound.
func TestServesMountedHandler(t *testing.T) {
	path := socketPath(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	l, err := listenAgentSocket(ctx, path, stubHandler(t), func() {})
	if err != nil {
		t.Fatalf("listenAgentSocket: %v", err)
	}
	t.Cleanup(func() { _ = l.Close(context.Background()) })

	err = callComms(ctx, agentClient(t, path))
	if got := connect.CodeOf(err); got != connect.CodeUnimplemented {
		t.Fatalf("Comms over socket: code = %v (err %v), want CodeUnimplemented", got, err)
	}
}

// Case 4. A pre-existing socket dir carrying a looser mode is forced owner-only,
// so the 0600 socket inside is genuinely unreachable by other host users. A
// regression that trusted the existing dir mode leaves it world-traversable.
func TestDirModeForcedFromLooser(t *testing.T) {
	path := socketPath(t)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("pre-creating loose dir: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // MkdirAll honors umask; pin 0755 explicitly
		t.Fatalf("chmod loose dir: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	l, err := listenAgentSocket(ctx, path, stubHandler(t), func() {})
	if err != nil {
		t.Fatalf("listenAgentSocket: %v", err)
	}
	t.Cleanup(func() { _ = l.Close(context.Background()) })

	dfi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(dir): %v", err)
	}
	if perm := dfi.Mode().Perm(); perm != socketDirMode {
		t.Fatalf("dir perm after listen = %o, want %o (looser mode not tightened)", perm, socketDirMode)
	}
}

// Case 5. SECURITY CORE — a socket this Runner owns, left behind by a crash, is
// reclaimed: a second listen at the same path succeeds and serves. Mutation:
// making reclaimStaleSocket skip the os.Remove leaves the stale socket in place,
// so the second net.Listen fails EADDRINUSE and this test goes RED.
func TestReclaimsOwnedStaleSocket(t *testing.T) {
	path := socketPath(t)
	leaveOwnedSocket(t, path)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	l, err := listenAgentSocket(ctx, path, stubHandler(t), func() {})
	if err != nil {
		t.Fatalf("second listen over an owned stale socket must reclaim it, got: %v", err)
	}
	t.Cleanup(func() { _ = l.Close(context.Background()) })

	if err := callComms(ctx, agentClient(t, path)); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("reclaimed socket does not serve: code = %v (err %v)", connect.CodeOf(err), err)
	}
}

// Case 6. SECURITY CORE — any non-socket object squatting the path is a
// fail-closed error and is NEVER deleted. A regular file, a directory, and a
// symlink each stand for a path collision or partial op the listener must refuse
// to destroy. Mutation: dropping the ModeSocket guard makes the regular-file row
// os.Remove an unrelated file, so its still-exists assertion goes RED.
func TestRejectsNonSocketNeverDeletes(t *testing.T) {
	tests := []struct {
		name   string
		create func(t *testing.T, path string)
		// verify re-inspects the object after the refused listen; it must still
		// be there, unchanged in kind.
		verify func(t *testing.T, path string)
	}{
		{
			name: "regular file",
			create: func(t *testing.T, path string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), socketDirMode); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
					t.Fatalf("write file: %v", err)
				}
			},
			verify: func(t *testing.T, path string) {
				t.Helper()
				fi, err := os.Lstat(path)
				if err != nil {
					t.Fatalf("regular file was deleted by a refused listen: %v", err)
				}
				if !fi.Mode().IsRegular() {
					t.Fatalf("object is now %s, want a regular file", fi.Mode().Type())
				}
			},
		},
		{
			name: "directory",
			create: func(t *testing.T, path string) {
				t.Helper()
				if err := os.MkdirAll(path, socketDirMode); err != nil {
					t.Fatalf("mkdir at path: %v", err)
				}
			},
			verify: func(t *testing.T, path string) {
				t.Helper()
				fi, err := os.Lstat(path)
				if err != nil {
					t.Fatalf("directory was deleted by a refused listen: %v", err)
				}
				if !fi.IsDir() {
					t.Fatalf("object is now %s, want a directory", fi.Mode().Type())
				}
			},
		},
		{
			name: "symlink",
			create: func(t *testing.T, path string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), socketDirMode); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				// Target need not exist: the symlink must be rejected as itself,
				// never followed or removed.
				if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), path); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
			verify: func(t *testing.T, path string) {
				t.Helper()
				fi, err := os.Lstat(path)
				if err != nil {
					t.Fatalf("symlink was deleted by a refused listen: %v", err)
				}
				if fi.Mode().Type() != os.ModeSymlink {
					t.Fatalf("object is now %s, want a symlink (it was followed/removed)", fi.Mode().Type())
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := socketPath(t)
			tc.create(t, path)

			ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
			defer cancel()
			l, err := listenAgentSocket(ctx, path, stubHandler(t), func() {})
			if err == nil {
				_ = l.Close(context.Background())
				t.Fatalf("listen over a %s must fail closed, got nil error", tc.name)
			}
			tc.verify(t, path)
		})
	}
}

// Case 7. SECURITY CORE — a socket owned by another uid is foreign, not an
// abandoned Runner socket, so the listener fails closed and never deletes it.
// The wrong-owner branch can't be forged on disk without root, so we drive it
// through the runnerUID seam: an owned socket seen as if owned by a different
// uid. Mutation: flipping the ownership guard from != to == makes the impl
// remove the "foreign" socket, so the still-exists assertion goes RED.
func TestRejectsWrongOwnerSocketNeverDeletes(t *testing.T) {
	path := socketPath(t)
	leaveOwnedSocket(t, path)

	orig := runnerUID
	runnerUID = func() int { return os.Getuid() + 1 }
	defer func() { runnerUID = orig }()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	l, err := listenAgentSocket(ctx, path, stubHandler(t), func() {})
	if err == nil {
		_ = l.Close(context.Background())
		t.Fatal("listen over a wrong-owner socket must fail closed, got nil error")
	}

	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("wrong-owner socket was deleted by a refused listen: %v", err)
	}
	if fi.Mode().Type() != os.ModeSocket {
		t.Fatalf("object is now %s, want the untouched socket", fi.Mode().Type())
	}
}

// Case 7b. A path over the AF_UNIX sun_path limit is refused with an error that
// names the limit and the actual length, and nothing is created on disk; a path
// exactly at the limit still binds. The kernel enforces the cap at bind(2) with
// a bare EINVAL ("invalid argument"), which names neither number and reads as a
// permissions or path fault; the Runner's socket path is RuntimeDir plus a
// 69-byte tail at the store-minted 32-hex account id — a width fixed at its
// minting site, not validated at this hop — and RuntimeDir is operator-supplied
// with no bound anywhere, so this is a reachable misconfiguration, not a
// defensive check.
//
// Mutations, both RED: dropping the pre-check lets the over-long path reach bind,
// failing the message assertions and the never-created-dir assertion; tightening
// the bound to >= refuses the at-cap path the kernel accepts, failing the first
// subtest.
func TestRejectsPathOverSunPathLimit(t *testing.T) {
	// padTo returns a path under a fresh temp root of exactly n bytes, or fails
	// the subtest when the root alone already exceeds the budget (a very deep
	// TMPDIR).
	padTo := func(t *testing.T, n int) (dir, path string) {
		t.Helper()
		// A short fixed root, not t.TempDir(): the latter embeds the test name,
		// which lands these subtests at ~135 bytes under a darwin-shaped TMPDIR
		// (/var/folders/<2>/<hash>/T, ~49 bytes) and skips them — on the one
		// platform where sunPathMax differs from Linux's, and so the one platform
		// this test most needs to run. MkdirTemp("", "g") is bounded instead: the
		// random suffix is a uint32 in decimal, so at most 10 digits, giving <=16
		// bytes on Linux and <=61 under that darwin TMPDIR — both well under the
		// cap, and a bound rather than a measurement that could drift.
		root, err := os.MkdirTemp("", "g") //nolint:usetesting // t.TempDir embeds the test name and blows this test's own byte budget on darwin — see above
		if err != nil {
			t.Fatalf("MkdirTemp: %v", err)
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(root); err != nil {
				t.Errorf("removing %q: %v", root, err)
			}
		})
		dir = filepath.Join(root, "run")
		base := len(dir) + 1 // + separator
		// These two subtests are the only coverage of the boundary the guard
		// exists to enforce, so a deep TMPDIR must redden the run rather than
		// skip it — a skip here reports `ok` for a package that asserted
		// nothing, which is the failure mode this whole change is about.
		if base >= n {
			t.Fatalf("temp root %q is %d bytes, leaving no room for a %d-byte path; set TMPDIR to a short path", dir, base, n)
		}
		path = filepath.Join(dir, strings.Repeat("s", n-base))
		if len(path) != n {
			t.Fatalf("constructed path is %d bytes, want exactly %d", len(path), n)
		}
		return dir, path
	}

	t.Run("exactly at the cap binds", func(t *testing.T) {
		// The boundary case: the check must reject only what the kernel would.
		// An off-by-one (>= instead of >) refuses a path that binds fine, so this
		// is what keeps the guard from being over-tight.
		_, path := padTo(t, sunPathMax)
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		l, err := listenAgentSocket(ctx, path, stubHandler(t), func() {})
		if err != nil {
			t.Fatalf("listen on a %d-byte path (exactly the cap) = %v, want success", len(path), err)
		}
		if err := l.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	t.Run("one byte over is refused with a diagnostic", func(t *testing.T) {
		dir, path := padTo(t, sunPathMax+1)
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		l, err := listenAgentSocket(ctx, path, stubHandler(t), func() {})
		if err == nil {
			_ = l.Close(context.Background())
			t.Fatal("listen on an over-long path must fail, got nil error")
		}
		// The diagnostic is the whole point: the message must carry both numbers
		// and the flag to change, none of which the kernel's EINVAL does. Match
		// the surrounding phrases, not the bare digits — a temp path carries
		// random digits that could contain the cap by chance and pass a
		// substring check against a message that never mentioned it.
		msg := err.Error()
		for _, want := range []string{
			fmt.Sprintf("is %d bytes", len(path)),
			fmt.Sprintf("over the %d-byte", sunPathMax),
			"--runtime-dir",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q does not contain %q", msg, want)
			}
		}
		// A bare EINVAL is exactly what the check exists to replace, so the
		// message must not be one: that would mean the path reached bind.
		if strings.Contains(msg, "invalid argument") {
			t.Errorf("error %q reached bind; the pre-check must refuse it first", msg)
		}

		// Refused before anything was created: no directory left behind.
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Errorf("Lstat(%q) = %v, want the dir never to have been created", dir, err)
		}
	})
}

// Case 8. Close is idempotent and tolerates the socket file already being gone:
// a second Close returns nil (the os.Remove ErrNotExist is swallowed), and a
// Close after the socket was removed out from under the listener does not error
// on the missing file.
func TestCloseIdempotentAndTolerantOfMissingFile(t *testing.T) {
	t.Run("second close returns nil", func(t *testing.T) {
		path := socketPath(t)
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		l, err := listenAgentSocket(ctx, path, stubHandler(t), func() {})
		if err != nil {
			t.Fatalf("listenAgentSocket: %v", err)
		}
		if err := l.Close(ctx); err != nil {
			t.Fatalf("first Close: %v", err)
		}
		if err := l.Close(ctx); err != nil {
			t.Fatalf("second Close must be a no-op, got: %v", err)
		}
	})

	t.Run("close tolerates externally removed socket", func(t *testing.T) {
		path := socketPath(t)
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		l, err := listenAgentSocket(ctx, path, stubHandler(t), func() {})
		if err != nil {
			t.Fatalf("listenAgentSocket: %v", err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatalf("external remove: %v", err)
		}
		if err := l.Close(ctx); err != nil {
			t.Fatalf("Close over an already-removed socket must not error on the missing file, got: %v", err)
		}
	})
}

// blockingGateway is an AgentGateway whose Comms parks in-flight until either the
// request context is cancelled (the force-close path) or release is closed. It
// signals entry by closing started exactly once, so a test can event-gate on a
// live in-flight call with no sleep.
type blockingGateway struct {
	// Satisfy the grown AgentGateway interface (Publish/PostConversationFrame/
	// Control) with Unimplemented stubs; this double only exercises Comms.
	compassv1internalconnect.UnimplementedAgentGatewayHandler
	started chan struct{}
	release chan struct{}
}

func (b blockingGateway) Comms(ctx context.Context, _ *connect.Request[compassv1.CommsCallRequest]) (*connect.Response[compassv1.CommsCallResult], error) {
	close(b.started)
	select {
	case <-b.release:
		return connect.NewResponse(&compassv1.CommsCallResult{}), nil
	case <-ctx.Done():
		return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
	}
}

// Case 9. Close force-closes a handler still blocked past the drain grace: the
// in-flight call is torn down (the client sees a Connect error rather than
// hanging forever) and Close returns instead of wedging teardown. Deterministic:
// the in-flight state is event-gated on started, and the grace is the production
// drain deadline injected via a short-deadline context — not a settle-sleep.
func TestCloseForceClosesWedgedHandler(t *testing.T) {
	path := socketPath(t)
	h := blockingGateway{started: make(chan struct{}), release: make(chan struct{})}
	defer close(h.release) // release the handler if it survives the force-close

	mux := http.NewServeMux()
	mux.Handle(compassv1internalconnect.NewAgentGatewayHandler(h))

	l, err := listenAgentSocket(context.Background(), path, mux, func() {})
	if err != nil {
		t.Fatalf("listenAgentSocket: %v", err)
	}

	callErr := make(chan error, 1)
	go func() {
		callErr <- callComms(context.Background(), agentClient(t, path))
	}()

	// Event-gate: proceed only once the handler is genuinely in-flight.
	select {
	case <-h.started:
	case <-time.After(testTimeout):
		t.Fatal("handler never entered in-flight; cannot exercise the force-close path")
	}

	// The handler is wedged; a normal drain would block until the grace, so a
	// short-deadline context forces the drain to overrun and Close to force-close.
	closeCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	closeErr := l.Close(closeCtx)
	if closeErr == nil {
		t.Fatal("Close over a wedged handler must report the overran drain, got nil")
	}

	select {
	case err := <-callErr:
		if err == nil {
			t.Fatal("force-close must terminate the in-flight call; client saw a success")
		}
	case <-time.After(testTimeout):
		t.Fatal("in-flight call was not terminated by the force-close; it hung")
	}

	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Close must remove the socket even on the force-close path: Lstat = %v", err)
	}
}

// TestMountTargetsLiveSocketReadWrite pins the bind-mount the runtime is handed:
// the HOST path is the actual live socket (Path()), the container path is the
// caller's, and it is read-write — a read-only mount would both block the
// agent's connect() and flip the runtime relabel suffix to :ro,Z. (The :Z
// relabel string itself is proven in internal/runtime/podman_test.go:
// TestMountArgRelabel, whose mountArg is unexported and unreachable here; the
// in-container connect()-after-relabel invariant is socket_podman_test.go.)
func TestMountTargetsLiveSocketReadWrite(t *testing.T) {
	path := socketPath(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	l, err := listenAgentSocket(ctx, path, stubHandler(t), func() {})
	if err != nil {
		t.Fatalf("listenAgentSocket: %v", err)
	}
	t.Cleanup(func() { _ = l.Close(context.Background()) })

	const containerPath = "/run/compass/agent.sock"
	got := l.Mount(containerPath)
	want := runtime.Mount{HostPath: l.Path(), ContainerPath: containerPath, ReadOnly: false}
	if got != want {
		t.Fatalf("Mount(%q) = %+v, want %+v", containerPath, got, want)
	}
}

// Case 10. The operator-fault sentinel: every path whose remedy
// is an operator knob wraps ErrOperatorConfig so errorResult maps it to
// FAILED_PRECONDITION, while every genuine-internal path does NOT — the negative
// assertion is the over-classification guard the record requires (a future
// blanket-wrap refactor that mislabels an OS anomaly as operator-fault reddens
// here). Both directions matter; see
// docs/designs/infra/runtime/compass-runner-gateway-error-sentinels/design.md.
func TestOperatorFaultSentinelClassification(t *testing.T) {
	t.Run("path over the AF_UNIX limit is operator-fault", func(t *testing.T) {
		// Build a path one byte over the cap under a short root, mirroring
		// TestRejectsPathOverSunPathLimit's construction.
		root, err := os.MkdirTemp("", "g") //nolint:usetesting // a short root is required so a path one byte over sunPathMax fits the AF_UNIX budget; t.TempDir embeds the test name and overflows it — same construction as TestRejectsPathOverSunPathLimit
		if err != nil {
			t.Fatalf("MkdirTemp: %v", err)
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(root); err != nil {
				t.Errorf("removing %q: %v", root, err)
			}
		})
		dir := filepath.Join(root, "run")
		base := len(dir) + 1
		if base > sunPathMax {
			t.Fatalf("temp root %q leaves no room for an over-cap path; set TMPDIR shorter", dir)
		}
		path := filepath.Join(dir, strings.Repeat("s", sunPathMax+1-base))
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		_, err = listenAgentSocket(ctx, path, stubHandler(t), func() {})
		if err == nil {
			t.Fatal("over-cap path listen must fail")
		}
		if !errors.Is(err, ErrOperatorConfig) {
			t.Errorf("path-too-long error %v is not tagged ErrOperatorConfig", err)
		}
	})

	t.Run("socket-dir MkdirAll failure is operator-fault", func(t *testing.T) {
		// A regular file where the socket dir should be makes MkdirAll fail
		// ENOTDIR — an EACCES/EROFS-class deployment fault.
		root := t.TempDir()
		occupied := filepath.Join(root, "run")
		if err := os.WriteFile(occupied, []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		path := filepath.Join(occupied, "agent.sock")
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		_, err := listenAgentSocket(ctx, path, stubHandler(t), func() {})
		if err == nil {
			t.Fatal("MkdirAll into a regular file must fail")
		}
		if !errors.Is(err, ErrOperatorConfig) {
			t.Errorf("socket-dir MkdirAll error %v is not tagged ErrOperatorConfig", err)
		}
		// The multi-%w chain keeps the underlying OS error inspectable too.
		if !errors.Is(err, syscall.ENOTDIR) {
			t.Errorf("socket-dir MkdirAll error %v dropped the wrapped OS error", err)
		}
	})

	t.Run("reclaim non-socket occupant is operator-fault", func(t *testing.T) {
		path := socketPath(t)
		if err := os.MkdirAll(filepath.Dir(path), socketDirMode); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		err := reclaimStaleSocket(path)
		if err == nil {
			t.Fatal("reclaim over a non-socket must fail closed")
		}
		if !errors.Is(err, ErrOperatorConfig) {
			t.Errorf("non-socket refusal %v is not tagged ErrOperatorConfig", err)
		}
	})

	t.Run("reclaim wrong-owner socket is operator-fault", func(t *testing.T) {
		path := socketPath(t)
		leaveOwnedSocket(t, path)
		orig := runnerUID
		runnerUID = func() int { return os.Getuid() + 1 }
		defer func() { runnerUID = orig }()
		err := reclaimStaleSocket(path)
		if err == nil {
			t.Fatal("reclaim over a wrong-owner socket must fail closed")
		}
		if !errors.Is(err, ErrOperatorConfig) {
			t.Errorf("wrong-owner refusal %v is not tagged ErrOperatorConfig", err)
		}
	})

	// Negative guards: genuine-internal paths must NOT carry the sentinel.
	// These assert on a value built the same way the code raises it, not on an
	// error returned by the live path (each needs an OS anomaly — a chmod-race,
	// a bind collision, an Lstat-EACCES — that is not portably forceable in a
	// unit test). So they pin the MESSAGE SHAPE: the unwrapped fmt.Errorf the
	// code uses at these sites does not accidentally satisfy errors.Is for the
	// sentinel. They do NOT catch a future refactor that blanket-wraps the live
	// path — that changes the code, not this literal. The real regression
	// protection for the wrap/unwrap boundary is the live positive guards above
	// plus the live multi-%w assertion; these keep the intended unwrapped shapes
	// documented and executable.
	t.Run("socket-file Chmod failure stays INTERNAL", func(t *testing.T) {
		// Built the same way the code raises it at the socket-FILE Chmod arm
		// (securing agent socket, socket.go), which is deliberately unwrapped.
		err := fmt.Errorf("securing agent socket %q: %w", "/run/x/agent.sock", syscall.EPERM)
		if errors.Is(err, ErrOperatorConfig) {
			t.Error("socket-FILE Chmod error must stay INTERNAL, not operator-fault")
		}
	})

	t.Run("Listen failure stays INTERNAL", func(t *testing.T) {
		err := fmt.Errorf("listening on agent socket %q: %w", "/run/x/agent.sock", syscall.EADDRINUSE)
		if errors.Is(err, ErrOperatorConfig) {
			t.Error("Listen error must stay INTERNAL, not operator-fault")
		}
	})

	t.Run("reclaim Lstat inspection failure stays INTERNAL", func(t *testing.T) {
		err := fmt.Errorf("inspecting agent socket path %q: %w", "/run/x/agent.sock", syscall.EACCES)
		if errors.Is(err, ErrOperatorConfig) {
			t.Error("reclaim Lstat error must stay INTERNAL, not operator-fault")
		}
	})

	t.Run("reclaim ownership-read failure stays INTERNAL", func(t *testing.T) {
		err := fmt.Errorf("agent socket path %q: cannot read socket ownership", "/run/x/agent.sock")
		if errors.Is(err, ErrOperatorConfig) {
			t.Error("reclaim ownership-read error must stay INTERNAL, not operator-fault")
		}
	})
}
