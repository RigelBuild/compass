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

// stackLock is a held state-dir lock: it owns the lockfile at path until
// release. The lockfile's existence IS the lock, so no open handle is retained.
type stackLock struct {
	path string
}

// acquireLock takes the state-dir lock via O_CREATE|O_EXCL. On contention it
// distinguishes a live holder (another Up in flight — return errLockHeld) from a
// stale lockfile whose writer is gone: a stale file is removed and the acquire
// retried once, so a crashed Up does not wedge the state dir forever. The
// lockfile records the holder's pid so staleness is decidable.
func acquireLock(stateDir string) (*stackLock, error) {
	path := filepath.Join(stateDir, lockFileName)
	l, err := tryCreateLock(path)
	if err == nil {
		return l, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("acquire stack lock: %w", err)
	}
	// The file exists. If its recorded holder is gone, the file is stale — remove
	// it and retry exactly once. If the holder is live, the lock is genuinely
	// held.
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
		if errors.Is(err, os.ErrExist) {
			// Another Up won the race to recreate it after we cleared the stale
			// file — that holder is now the live one.
			return nil, errLockHeld
		}
		return nil, fmt.Errorf("acquire stack lock after clearing stale file: %w", err)
	}
	return l, nil
}

// errLockHeld reports that a live holder owns the state-dir lock — another Up is
// in flight.
var errLockHeld = errors.New("stack lock is held by another live up")

// tryCreateLock publishes the lockfile atomically with the current pid already
// in it, so a concurrent acquirer never observes a created-but-empty file (which
// it would misjudge as stale and clobber, defeating the lock). A fully-written
// temp file in the same directory is hard-linked to path: link(2) fails with
// EEXIST if path already exists, giving the same exclusive-create guarantee as
// O_EXCL while guaranteeing the visible file's content is complete. On EEXIST the
// caller decides live-vs-stale.
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
