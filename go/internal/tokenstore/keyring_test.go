package tokenstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	keyring "github.com/zalando/go-keyring"
)

// The go-keyring mock (MockInit / MockInitWithError) swaps a process-global
// provider, so these tests must not run in parallel with each other or with any
// test that touches the keyring. Each test reinitialises the provider up front.

// fallbackFileExists reports whether the fileStore fallback wrote its file under
// dir — the discriminator for "keyring path vs file fallback was taken".
func fallbackFileExists(t *testing.T, dir string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, tokenFileName))
	switch {
	case err == nil:
		return true
	case errors.Is(err, os.ErrNotExist):
		return false
	default:
		t.Fatalf("stat fallback file: %v", err)
		return false
	}
}

// When the keyring backend is available, Write/Read go through it and the file
// fallback is never written — proving the keyring path, not the fallback.
func TestKeyringStoreUsesKeyringWhenAvailable(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	s := New(dir)

	if err := s.Write(testURL, testToken); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := s.Read(testURL)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != testToken {
		t.Fatalf("Read = %q, want %q", got, testToken)
	}
	if fallbackFileExists(t, dir) {
		t.Error("fallback file written while keyring available; want keyring path only")
	}
}

// A missing keyring entry (keyring.ErrNotFound) is a real "no token" answer,
// mapped to ErrNotFound — NOT a trigger to consult the file fallback.
func TestKeyringStoreAbsentIsNotFoundNotFallback(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	// Plant a fallback file so a wrongful fallback consult would return it and
	// fail the ErrNotFound assertion.
	if err := (&fileStore{dir: dir}).Write(testURL, "stale-fallback-token"); err != nil {
		t.Fatalf("seed fallback: %v", err)
	}
	s := New(dir)

	if _, err := s.Read(testURL); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read absent-from-keyring err = %v, want ErrNotFound (must not fall back to file)", err)
	}
}

// When the keyring backend is unavailable (any non-ErrNotFound error), Write
// lands in the file fallback and Read returns it — proving unavailable→fallback.
func TestKeyringStoreUnavailableFallsBackToFile(t *testing.T) {
	keyring.MockInitWithError(errors.New("no secret service bus"))
	dir := t.TempDir()
	s := New(dir)

	if err := s.Write(testURL, testToken); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !fallbackFileExists(t, dir) {
		t.Fatal("keyring unavailable but no fallback file written")
	}
	got, err := s.Read(testURL)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != testToken {
		t.Fatalf("Read = %q, want %q", got, testToken)
	}
}

// keyring.ErrNotFound must be classified as a real not-found even when the mock
// is configured to return it for every op: Read returns ErrNotFound and does NOT
// silently serve a planted fallback file (the classification is the guard).
func TestKeyringStoreErrNotFoundNeverFallsBack(t *testing.T) {
	keyring.MockInitWithError(keyring.ErrNotFound)
	dir := t.TempDir()
	if err := (&fileStore{dir: dir}).Write(testURL, "stale-fallback-token"); err != nil {
		t.Fatalf("seed fallback: %v", err)
	}
	s := New(dir)

	if _, err := s.Read(testURL); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read err = %v, want ErrNotFound (ErrNotFound must not trigger fallback)", err)
	}
}
