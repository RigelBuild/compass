//go:build unix

// Package gateway is the Runner side of the agent->Runner call transport: a
// per-container Unix-socket Connect server the in-container first-party agent
// dials to reach its Runner (design
// docs/designs/product/compass-agent-runner-transport/design.md, SEA-1351 T2).
//
// One socket per container, 1:1 with the session the container hosts, so the
// socket IS that session's identity: no credential travels the local hop, and
// the Runner maps a connection to its container structurally (Decision #4). The
// listener is created at Provision (before `podman run`, so the bind-mount
// source exists) and torn down at container teardown; socket, session, and
// container share one lifecycle.
//
// The egress seal is untouched: a Unix socket is a local hop, not a network
// address — no new port, no outbound route (design "Why egress stays sealed").
package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sealedsecurity/compass/go/internal/runtime"
)

// socketFileMode is the mode of the agent socket file: owner read/write only.
// Meaningful only because the parent dir is not traversable by other host users
// (socketDirMode), so no other uid can reach the socket to connect().
const socketFileMode os.FileMode = 0o600

// socketDirMode is the mode of the Runner-owned directory the socket lives
// under: owner-only, so a 0600 socket inside it is genuinely unreachable by
// other host users (design "Host socket-directory perms").
const socketDirMode os.FileMode = 0o700

// shutdownGrace bounds the graceful drain in Close before in-flight handlers are
// force-closed. A broker blocked in a RelayCommsCall forward past this deadline
// is force-closed so it receives the promised Connect error rather than hanging
// teardown (design SocketListener.Close).
const shutdownGrace = 5 * time.Second

// runnerUID reports the uid a reclaimable stale socket must be owned by. It is a
// package var over os.Getuid so a hermetic test can drive the wrong-owner
// fail-closed branch (which cannot be forged on disk without root).
var runnerUID = os.Getuid

// SocketListener is a per-container Unix-socket Connect server for agent->Runner
// calls. It owns the socket file's whole lifecycle: listenAgentSocket creates it,
// Close drains the server and removes it.
type SocketListener struct {
	path string
	srv  *http.Server
	done chan struct{}
}

// listenAgentSocket opens the per-container agent socket at path and serves h
// over cleartext HTTP/2, returning once the listener is bound (so a caller that
// bind-mounts path next sees a live socket). It is called at Provision, before
// `podman run`.
//
// Stale-socket recovery: a Runner crash/restart can leave the socket file on
// disk, so a fresh net.Listen would fail EADDRINUSE. The path is reclaimed ONLY
// when an Lstat confirms a socket owned by this Runner's uid; any other object
// (regular file, dir, symlink, or a socket owned by another uid — a path
// collision or partial op, never an abandoned Runner socket) is rejected fail-
// closed and never deleted.
func listenAgentSocket(ctx context.Context, path string, h http.Handler) (*SocketListener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, socketDirMode); err != nil {
		return nil, fmt.Errorf("creating agent socket dir %q: %w", dir, err)
	}
	// A pre-existing dir may carry a looser mode (umask, or a prior op); force
	// it owner-only so the 0600 socket inside is genuinely unreachable.
	if err := os.Chmod(dir, socketDirMode); err != nil {
		return nil, fmt.Errorf("securing agent socket dir %q: %w", dir, err)
	}

	if err := reclaimStaleSocket(path); err != nil {
		return nil, err
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on agent socket %q: %w", path, err)
	}
	// net.Listen creates the socket with the process umask applied, which can
	// clear group/other bits inconsistently; pin the mode explicitly.
	if err := os.Chmod(path, socketFileMode); err != nil {
		ln.Close() //nolint:errcheck,gosec // teardown on an already-failing create path — nothing actionable remains
		return nil, fmt.Errorf("securing agent socket %q: %w", path, err)
	}

	srv := &http.Server{Handler: h, Protocols: cleartextHTTP2()} //nolint:gosec // G112: socket-only door (never internet-facing), so the Slowloris ReadHeaderTimeout does not apply
	l := &SocketListener{path: path, srv: srv, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		// A clean Shutdown/Close returns ErrServerClosed; anything else is a
		// serve fault with no caller to return it to, so it is dropped here (the
		// next agent dial fails visibly).
		_ = srv.Serve(ln)
	}()
	return l, nil
}

// reclaimStaleSocket removes a leftover socket at path IFF it is a socket owned
// by this process's uid; every other case is left in place. A missing path is
// the normal first-Provision case (nil, nothing to reclaim). A non-socket or
// wrong-owner object is a fail-closed error: it is never an abandoned Runner
// socket, so deleting it would be destroying an unrelated inode.
func reclaimStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspecting agent socket path %q: %w", path, err)
	}
	// Lstat, not Stat: a symlink must be rejected as itself, never followed —
	// following it could target reclaim at an unrelated inode.
	if info.Mode().Type() != os.ModeSocket {
		return fmt.Errorf("agent socket path %q is occupied by a non-socket (%s); refusing to remove", path, info.Mode().Type())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("agent socket path %q: cannot read socket ownership", path)
	}
	if int(st.Uid) != runnerUID() {
		return fmt.Errorf("agent socket path %q is a socket owned by uid %d, not the Runner uid %d; refusing to remove", path, st.Uid, runnerUID())
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("removing stale agent socket %q: %w", path, err)
	}
	return nil
}

// Close drains the server under a bounded deadline (so in-flight calls finish),
// then force-closes any handler still blocked past it — the forced close is what
// delivers the promised Connect error to a broker blocked in a RelayCommsCall
// forward — and finally removes the socket file. Called at container teardown.
func (l *SocketListener) Close(ctx context.Context) error {
	shutdownCtx, cancel := context.WithTimeout(ctx, shutdownGrace)
	defer cancel()

	drainErr := l.srv.Shutdown(shutdownCtx)
	if drainErr != nil {
		// The drain overran (a handler wedged past the deadline); force every
		// remaining connection closed so teardown completes and blocked brokers
		// get their error.
		l.srv.Close() //nolint:errcheck,gosec // force-close after a failed drain — the drain error is what we report
	}
	<-l.done // serve goroutine has returned; the listener is fully closed

	var rmErr error
	if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		rmErr = fmt.Errorf("removing agent socket %q: %w", l.path, err)
	}

	if drainErr != nil {
		return fmt.Errorf("draining agent socket server: %w", drainErr)
	}
	return rmErr
}

// Path returns the host path of the agent socket.
func (l *SocketListener) Path() string { return l.path }

// Mount describes the socket bind-mount handed to the runtime: the host socket
// at containerPath inside the container, read-write (the agent must connect()).
func (l *SocketListener) Mount(containerPath string) runtime.Mount {
	return runtime.Mount{HostPath: l.path, ContainerPath: containerPath, ReadOnly: false}
}

// cleartextHTTP2 enables HTTP/1.1 and prior-knowledge cleartext HTTP/2 (h2c) on
// the socket door, matching the Server's socket door (server/serve.go) so native
// gRPC, gRPC-Web, and Connect clients all reach the handler over the local hop.
func cleartextHTTP2() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}
