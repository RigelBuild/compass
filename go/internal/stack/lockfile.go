//go:build unix

package stack

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// lockFileName is the O_EXCL lockfile under the state dir that serializes the
// probe→spawn decision, so two concurrent Ups cannot both spawn.
const lockFileName = "stack.lock"

// lockGuardName is a persistent per-state-dir file whose advisory flock
// serializes the acquire decision (probe→reclaim→create) across processes. It is
// never removed, so it cannot go stale; the flock releases on process exit.
const lockGuardName = "stack.lock.guard"

// stackLock is a held state-dir lock: it owns the lockfile at path until
// release. The lockfile's existence IS the lock, so no open handle is retained.
type stackLock struct {
	path string
}

// acquireLock serializes acquisition, reclaims a stale lock once, and rejects a live holder.
func acquireLock(stateDir string) (*stackLock, error) {
	// The guard flock makes stale-file reclaim atomic; the O_EXCL pidfile alone cannot prevent two concurrent spawns.
	guard, err := acquireGuard(stateDir)
	if err != nil {
		return nil, err
	}
	defer guard.release()

	path := filepath.Join(stateDir, lockFileName)
	l, err := tryCreateLock(path)
	if err == nil {
		return l, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("acquire stack lock: %w", err)
	}
	// The guard excludes other reclaimers while we inspect, remove, and recreate the stale file.
	if live, herr := lockHolderLive(path); herr != nil {
		return nil, fmt.Errorf("inspect stack lock %q: %w", path, herr)
	} else if live {
		return nil, errLockHeld
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale stack lock %q: %w", path, err)
	}
	l, err = tryCreateLock(path)
	if err != nil {
		return nil, fmt.Errorf("acquire stack lock after clearing stale file: %w", err)
	}
	return l, nil
}

// guardHandle holds the advisory guard lock for the duration of an acquire
// decision. Closing the file releases the flock, so release cannot leak it.
type guardHandle struct {
	f *os.File
}

// acquireGuard opens (creating if absent) the guard file and takes an exclusive
// advisory lock on it, blocking until any concurrent acquirer's decision
// completes. The guard file is never removed — it is a stable coordination
// anchor, not the lock itself — so unlike the pidfile it can never go stale.
func acquireGuard(stateDir string) (*guardHandle, error) {
	path := filepath.Join(stateDir, lockGuardName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: the guard path is the stack-owned coordination file in the state dir, not user input
	if err != nil {
		return nil, fmt.Errorf("open stack lock guard %q: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock stack lock guard %q: %w", path, err)
	}
	return &guardHandle{f: f}, nil
}

// release drops the advisory lock and closes the guard file. Closing the fd
// releases the flock even if the explicit unlock errors, so no lock is leaked.
func (g *guardHandle) release() {
	_ = syscall.Flock(int(g.f.Fd()), syscall.LOCK_UN)
	_ = g.f.Close()
}

// errLockHeld reports that a live holder owns the state-dir lock — another Up is
// in flight.
var errLockHeld = errors.New("stack lock is held by another live up")

// tryCreateLock links a fully-written pid temp into place so no acquirer sees an empty file.
// EEXIST preserves exclusive creation; the caller distinguishes live and stale holders.
func tryCreateLock(path string) (*stackLock, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), lockFileName+".*.tmp")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(strconv.Itoa(os.Getpid())); err != nil {
		// The write error is the actionable failure; closing and unlinking the
		// temp is best-effort cleanup whose errors add nothing over it.
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return nil, err
	}
	// Link the complete temp into place; EEXIST surfaces as os.ErrExist. Remove
	// the temp regardless — on success the link is the durable name, on EEXIST
	// the temp is unneeded.
	linkErr := os.Link(tmpName, path)
	if rmErr := os.Remove(tmpName); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) && linkErr == nil {
		// The link succeeded but the temp could not be cleaned up: surface it
		// rather than leak a temp file silently, and drop the lock we just took.
		_ = os.Remove(path)
		return nil, fmt.Errorf("remove lock temp %q: %w", tmpName, rmErr)
	}
	if linkErr != nil {
		return nil, linkErr
	}
	return &stackLock{path: path}, nil
}

// lockHolderLive reports whether the pid recorded in the lockfile is a live
// process. An unparseable or empty file is treated as stale (not live): a
// half-written lock from a crashed writer must not wedge the state dir.
func lockHolderLive(path string) (bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the stack-owned lockfile in the state dir, not user input
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// It vanished between the failed create and this read — not held.
			return false, nil
		}
		return false, err
	}
	pid, err := strconv.Atoi(string(trimSpace(data)))
	if err != nil || pid <= 0 {
		return false, nil //nolint:nilerr // an unparseable or empty pid means a stale lock (crashed writer), not a live holder — treat as not-live per the doc contract, not an error
	}
	return pidLive(pid), nil
}

// pidLive reports whether pid names a live process. signal 0 probes existence
// without delivering a signal: nil (permitted) or EPERM (exists, not ours) mean
// live; ESRCH means gone.
func pidLive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// release removes the lockfile. It is idempotent — a double release (Down after a
// failed Up already released) is a no-op, since path is cleared after the first.
func (l *stackLock) release() error {
	if l == nil || l.path == "" {
		return nil
	}
	path := l.path
	l.path = ""
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// trimSpace strips leading/trailing ASCII whitespace from a byte slice without
// pulling in strings for a single call.
func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && asciiSpace(b[start]) {
		start++
	}
	end := len(b)
	for end > start && asciiSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func asciiSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
