package vfs

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func TestWriteStampReturnsSyncDirError(t *testing.T) {
	originalSyncDirFn := syncDirFn
	t.Cleanup(func() { syncDirFn = originalSyncDirFn })

	sentinel := errors.New("sync directory failed")
	syncDirFn = func(string) error { return sentinel }

	err := writeStamp(t.TempDir(), closeStamp{Intent: IntentClosed, StampedAt: time.Now()})
	if !errors.Is(err, sentinel) {
		t.Fatalf("writeStamp error = %v, want it to match %v", err, sentinel)
	}
}

func TestClearStampReturnsSyncDirErrorAndSkipsAbsentStamp(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(metaDir(root), volumeDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stampPath(root), []byte("stamp"), stampFileMode); err != nil {
		t.Fatal(err)
	}

	originalSyncDirFn := syncDirFn
	t.Cleanup(func() { syncDirFn = originalSyncDirFn })

	sentinel := errors.New("sync directory failed")
	calls := 0
	syncDirFn = func(string) error {
		calls++
		return sentinel
	}

	if err := clearStamp(root); !errors.Is(err, sentinel) {
		t.Fatalf("clearStamp on present stamp = %v, want it to match %v", err, sentinel)
	}
	if calls != 1 {
		t.Fatalf("syncDirFn called %d times after removing a stamp, want 1", calls)
	}
	if err := clearStamp(root); err != nil {
		t.Fatalf("clearStamp with no stamp = %v, want nil", err)
	}
	if calls != 1 {
		t.Fatalf("syncDirFn called %d times after clearing an absent stamp, want still 1", calls)
	}
}

func TestWriteAndCloseClosedFileReportsWriteAndCloseErrors(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "close-stamp-")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	err = writeAndClose(f, []byte("stamp"))
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("writeAndClose error = %v, want it to match %v", err, os.ErrClosed)
	}
	for _, want := range []string{"writing close stamp", "closing close stamp"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("writeAndClose error %q does not contain %q", err, want)
		}
	}
}

func TestAttachJoinsClearAndReleaseErrors(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-attach-clear-release-errors")
	stampAged(t, v, IntentClosed, 30*24*time.Hour)

	originalSyncDirFn := syncDirFn
	originalUnlockFn := unlockFn
	t.Cleanup(func() {
		syncDirFn = originalSyncDirFn
		unlockFn = originalUnlockFn
	})

	clearErr := errors.New("clear sync failed")
	releaseErr := errors.New("unlock failed")
	syncDirFn = func(string) error { return clearErr }
	releasedFD := -1
	unlockFn = func(fd int, how int) error {
		if how == syscall.LOCK_UN {
			releasedFD = fd
			return releaseErr
		}
		return syscall.Flock(fd, how)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := m.Attach(ctx, v)
	if !errors.Is(err, clearErr) || !errors.Is(err, releaseErr) {
		t.Fatalf("Attach error = %v, want both %v and %v", err, clearErr, releaseErr)
	}
	assertFileDescriptorClosed(t, releasedFD)

	unlockFn = originalUnlockFn
	assertLockCanBeReacquired(t, v.HostRoot)
}

func TestAttachJoinsNotFoundAndReleaseErrors(t *testing.T) {
	m := newManager(t)
	v := mustCreate(t, m, "sess-attach-not-found-release-errors")
	stampAged(t, v, IntentClosed, 30*24*time.Hour)

	held, err := tryLockVolume(t.Context(), v.HostRoot)
	if err != nil || held == nil {
		t.Fatalf("holding volume lock: lock=%v err=%v", held, err)
	}
	t.Cleanup(func() {
		if held.f != nil {
			if err := held.release(); err != nil {
				t.Errorf("releasing fixture lock: %v", err)
			}
		}
	})

	originalSyncDirFn := syncDirFn
	originalUnlockFn := unlockFn
	t.Cleanup(func() {
		syncDirFn = originalSyncDirFn
		unlockFn = originalUnlockFn
	})

	clearErr := errors.New("clear sync failed")
	releaseErr := errors.New("unlock failed")
	syncCalls := 0
	syncDirFn = func(string) error {
		syncCalls++
		return clearErr
	}
	heldFD := int(held.f.Fd())
	releasedFD := -1
	unlockFn = func(fd int, how int) error {
		if fd == heldFD {
			return syscall.Flock(fd, how)
		}
		if how == syscall.LOCK_UN {
			releasedFD = fd
			return releaseErr
		}
		return syscall.Flock(fd, how)
	}

	synctest.Test(t, func(t *testing.T) {
		type attachResult struct {
			err error
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		done := make(chan attachResult, 1)
		go func() {
			_, err := m.Attach(ctx, v)
			done <- attachResult{err: err}
		}()
		synctest.Wait()

		if err := os.RemoveAll(v.HostRoot); err != nil {
			t.Fatal(err)
		}
		if err := held.release(); err != nil {
			t.Fatalf("releasing fixture lock: %v", err)
		}

		got := <-done
		if !errors.Is(got.err, ErrVolumeNotFound) || !errors.Is(got.err, releaseErr) {
			t.Fatalf("Attach error = %v, want both %v and %v", got.err, ErrVolumeNotFound, releaseErr)
		}
	})

	if syncCalls != 0 {
		t.Fatalf("syncDirFn called %d times after root disappeared, want 0", syncCalls)
	}
	assertFileDescriptorClosed(t, releasedFD)

	unlockFn = originalUnlockFn
	assertLockCanBeReacquired(t, v.HostRoot)
}

func assertFileDescriptorClosed(t *testing.T, fd int) {
	t.Helper()
	if fd < 0 {
		t.Fatal("unlock hook did not observe a release file descriptor")
	}
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFD), 0)
	if errno != syscall.EBADF {
		t.Fatalf("fcntl(F_GETFD) for released fd %d returned %v, want %v", fd, errno, syscall.EBADF)
	}
}

func assertLockCanBeReacquired(t *testing.T, root string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	lock, err := lockVolume(ctx, root)
	if err != nil {
		t.Fatalf("re-acquiring lock after release failure: %v", err)
	}
	if err := lock.release(); err != nil {
		t.Fatalf("releasing re-acquired lock: %v", err)
	}
}
