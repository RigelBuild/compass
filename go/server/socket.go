//go:build unix

// Socket-door semantics for the server's Unix listener: private parent dirs,
// single-instance stale-socket handling, and inode-checked cleanup.
package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// DefaultSocketPath resolves the socket the server serves on when no --socket is
// given: $XDG_RUNTIME_DIR/compass/server.sock when XDG_RUNTIME_DIR holds an
// absolute path (the Linux norm), else $HOME/.compass/server.sock. A
// non-absolute (empty or relative) XDG_RUNTIME_DIR counts as unset, so it falls
// back to HOME rather than resolving a cwd-dependent path. A co-located client
// (the desktop shell) resolves the same path from this one source of truth.
//
// A non-absolute HOME is likewise treated as unset and errors here rather than
// resolving a relative ".compass/server.sock": a socket bound at a
// process-cwd-relative path is a footgun, so the absolute-path filter applied to
// XDG_RUNTIME_DIR is applied to HOME too.
func DefaultSocketPath() (string, error) {
	if runtime := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(runtime) {
		return filepath.Join(runtime, "compass", "server.sock"), nil
	}
	home := os.Getenv("HOME")
	if !filepath.IsAbs(home) {
		return "", errors.New("HOME is unset or not an absolute path")
	}
	return filepath.Join(home, ".compass", "server.sock"), nil
}

// parentDir returns the directory that will contain the socket, or "" when the
// path is a bare filename (bound in the current directory, nothing to create).
func parentDir(socketPath string) string {
	dir := filepath.Dir(socketPath)
	if dir == "." {
		return ""
	}
	return dir
}

// listenUnixPrivate binds a Unix domain socket at path with the process umask
// temporarily tightened to 0177, so the socket is created owner-only (0600)
// with no window in which a local peer could connect before the mode is
// tightened. syscall.Umask is process-global and not goroutine-safe; the server
// calls this once during single-goroutine startup (before any server goroutine
// spawns), so the set/restore pair cannot race a concurrent file creation. The
// prior umask is always restored, including on bind failure.
//
// Unlink-on-close is disabled on the returned listener. Go's net.UnixListener
// defaults to unlinking the socket path when Close runs; that is a second,
// unconditional remover that would defeat the inode-guarded cleanupSocket and
// delete a successor server's rebound socket when this server drains, so removal
// is left solely to cleanupSocket here.
func listenUnixPrivate(path string) (net.Listener, error) {
	prev := syscall.Umask(0o177)
	defer syscall.Umask(prev)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// net.Listen("unix", …) always returns a *net.UnixListener; the assertion
	// guards against a future stdlib change rather than a real alternative type.
	ul, ok := l.(*net.UnixListener)
	if !ok {
		l.Close() //nolint:errcheck,gosec // teardown on a type-assertion failure path — nothing actionable remains (errcheck + its gosec G104 twin)
		return nil, fmt.Errorf("unix listener has unexpected type %T", l)
	}
	ul.SetUnlinkOnClose(false)
	return ul, nil
}

// ensurePrivateDir creates dir and any missing ancestors, tightening every
// directory it actually creates to 0700 so a socket placed inside is never
// briefly reachable through a world-traversable path. A directory that already
// exists is left untouched — the server never rewrites the mode of a parent the
// operator set up (a custom --socket under a shared dir must keep that dir's
// mode).
func ensurePrivateDir(dir string) error {
	// Walk from the topmost missing ancestor downward, creating each with 0700.
	// Collect the chain of not-yet-existing dirs first, then create in order.
	var missing []string
	for d := dir; ; {
		if _, err := os.Stat(d); err == nil {
			break // exists (and every ancestor above it does too)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat %s: %w", d, err)
		}
		missing = append(missing, d)
		parent := filepath.Dir(d)
		if parent == d {
			break // reached the root
		}
		d = parent
	}
	// missing is deepest-first; create shallowest-first. Chmod each dir we
	// create to 0700 after the mkdir — os.Mkdir's mode is masked by the process
	// umask, so under a restrictive umask the bits would land tighter or looser
	// than intended; an explicit chmod pins the documented 0700 regardless.
	for _, dir := range slices.Backward(missing) {
		switch err := os.Mkdir(dir, 0o700); {
		case err == nil:
			if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: 0o700 is correct for a PRIVATE DIRECTORY (owner needs the traverse bit); G302's ≤0600 bar is a file-mode rule misapplied to a dir
				return fmt.Errorf("chmod 0700 %s: %w", dir, err)
			}
		case errors.Is(err, os.ErrExist):
			// A concurrent creator won the race; it owns the mode, don't chmod.
		default:
			return fmt.Errorf("creating private dir %s: %w", dir, err)
		}
	}
	return nil
}

// clearStaleSocket refuses to start on top of a live server and clears only a
// genuinely stale socket.
//
// Nothing at the path → nothing to do. A non-socket file there is an operator
// error the server refuses rather than delete. A successful connect means
// another compass-server owns the path, so it bails; a refused connect means the
// socket is stale (its server is gone) and it removes it so the caller can
// rebind.
//
// This serializes correctly against a sequential restart but not against two
// servers racing to start on the same path at once.
func clearStaleSocket(socketPath string) error {
	info, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil // nothing to do
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", socketPath, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to start: %s exists and is not a socket", socketPath)
	}

	// A live peer answers a connect; a stale socket refuses it. Only a refused
	// or already-gone connect proves the socket is stale — any other probe error
	// (timeout, permission, descriptor exhaustion) is propagated rather than
	// treated as stale, so a transient failure never unlinks a live server's
	// socket.
	conn, err := net.DialTimeout("unix", socketPath, 250*time.Millisecond)
	if err == nil {
		conn.Close() //nolint:errcheck,gosec // the probe dial succeeded; closing the probe conn, its result is irrelevant to the liveness check (errcheck + its gosec G104 twin)
		return fmt.Errorf("refusing to start: another compass-server is already serving at %s", socketPath)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("probing socket %s: %w", socketPath, err)
	}

	// Refused or gone → stale. Remove so the caller can rebind.
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing stale socket %s: %w", socketPath, err)
	}
	return nil
}

// socketInode returns the inode backing path, or (0, false) if it is missing or
// unstat-able. Used to detect a successor server rebinding the socket path.
func socketInode(path string) (uint64, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Ino, true
}

// cleanupSocket removes the socket file on shutdown, but only if it can prove
// the on-disk socket is still the one the server bound. If the inode bound was
// never pinned (boundInode ok=false — socketInode failed right after bind), or a
// successor server has already rebound the path to a different inode, it leaves
// the file alone rather than risk deleting another server's live socket.
func cleanupSocket(socketPath string, boundInode uint64, boundOK bool) {
	if !boundOK {
		return
	}
	current, ok := socketInode(socketPath)
	if !ok || current != boundInode {
		return // gone already, or a successor rebound it
	}
	_ = os.Remove(socketPath)
}
