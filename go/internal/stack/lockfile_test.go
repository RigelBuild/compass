//go:build unix

package stack

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// assertLockFree fails if the state-dir lockfile still exists (a leaked lock).
func assertLockFree(t *testing.T, stateDir string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(stateDir, lockFileName))
	if err == nil {
		t.Fatalf("lockfile %q still present; want released", lockFileName)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat lockfile: %v", err)
	}
}

func TestLockAcquireExclusive(t *testing.T) {
	dir := t.TempDir()

	l1, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("first acquire = %v", err)
	}

	// A second acquire while held by this (live) process reports contention.
	if _, err := acquireLock(dir); !errors.Is(err, errLockHeld) {
		t.Fatalf("second acquire = %v, want errLockHeld", err)
	}

	if err := l1.release(); err != nil {
		t.Fatalf("release = %v", err)
	}

	// After release the lock is free again.
	l2, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("re-acquire after release = %v", err)
	}
	if err := l2.release(); err != nil {
		t.Fatalf("release l2 = %v", err)
	}
}

// A stale lockfile (holder pid is gone) is reclaimed rather than wedging the
// state dir forever.
func TestLockReclaimsStale(t *testing.T) {
	dir := t.TempDir()
	// Write a lockfile naming a pid that cannot be live. Max pid + 1 is never a
	// running process.
	stalePID := deadPID(t)
	if err := os.WriteFile(filepath.Join(dir, lockFileName), []byte(strconv.Itoa(stalePID)), 0o600); err != nil {
		t.Fatalf("seed stale lock: %v", err)
	}

	l, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("acquire over stale lock = %v, want success", err)
	}
	if err := l.release(); err != nil {
		t.Fatalf("release = %v", err)
	}
}

// A lockfile with unparseable contents is treated as stale (a half-written lock
// from a crashed writer must not wedge the dir).
func TestLockReclaimsGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, lockFileName), []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatalf("seed garbage lock: %v", err)
	}
	l, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("acquire over garbage lock = %v, want success", err)
	}
	if err := l.release(); err != nil {
		t.Fatalf("release = %v", err)
	}
}

// deadPID returns a pid that is not a live process: it scans upward from a high
// number until syscall.Kill(pid, 0) reports the process does not exist.
func deadPID(t *testing.T) int {
	t.Helper()
	for pid := 4_000_000; pid < 4_001_000; pid++ {
		if !pidLive(pid) {
			return pid
		}
	}
	t.Fatal("could not find a dead pid")
	return 0
}
