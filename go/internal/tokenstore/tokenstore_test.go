package tokenstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testURL   = "https://remote-a.example:8443"
	otherURL  = "https://remote-b.example:8443"
	testToken = "bearer-secret-do-not-log-abc123"
)

// newTestStore returns the file-fallback store directly, bypassing any keyring
// on the host so the default CI suite always exercises the fallback path.
func newTestStore(t *testing.T) *fileStore {
	t.Helper()
	return &fileStore{dir: t.TempDir()}
}

func TestFileStoreRoundTrip(t *testing.T) {
	s := newTestStore(t)
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
}

func TestFileStoreMode0600(t *testing.T) {
	s := newTestStore(t)
	if err := s.Write(testURL, testToken); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(filepath.Join(s.dir, tokenFileName))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file mode = %o, want 600", perm)
	}
}

func TestFileStoreReadAbsent(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Read(testURL); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read absent err = %v, want ErrNotFound", err)
	}
}

// TestFileStoreReadURLMismatch is the F1 replay guard: a token stored for one
// remote must never be returned for a different requested URL.
func TestFileStoreReadURLMismatch(t *testing.T) {
	s := newTestStore(t)
	if err := s.Write(testURL, testToken); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := s.Read(otherURL); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read(otherURL) err = %v, want ErrNotFound (F1 replay guard)", err)
	}
}

func TestFileStoreAtomicOverwrite(t *testing.T) {
	s := newTestStore(t)
	if err := s.Write(testURL, testToken); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	const second = "second-token-xyz789"
	if err := s.Write(otherURL, second); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	// Old pair is fully replaced: old URL no longer resolves, new one does.
	if _, err := s.Read(testURL); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read(old URL) after overwrite err = %v, want ErrNotFound", err)
	}
	got, err := s.Read(otherURL)
	if err != nil {
		t.Fatalf("Read(new URL): %v", err)
	}
	if got != second {
		t.Fatalf("Read(new URL) = %q, want %q", got, second)
	}
	// Exactly one file exists (no stray temp files left behind).
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("dir has %d entries %v, want 1 (no stray temp files)", len(entries), names)
	}
}

func TestFileStoreDelete(t *testing.T) {
	s := newTestStore(t)
	if err := s.Write(testURL, testToken); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Delete(testURL); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Read(testURL); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read after Delete err = %v, want ErrNotFound", err)
	}
	// Delete on an absent file is a no-op, not an error.
	if err := s.Delete(testURL); err != nil {
		t.Fatalf("Delete on absent: %v", err)
	}
}

// TestTokenNeverInErrorStrings asserts no returned error string leaks the token.
func TestTokenNeverInErrorStrings(t *testing.T) {
	s := newTestStore(t)
	if err := s.Write(testURL, testToken); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Corrupt the file into invalid JSON that STILL CONTAINS the token, so a
	// regression that wrapped the raw file bytes into the error would surface
	// the token and redden this test (defends DL-109, not a tautology).
	corrupt := `{"serverUrl":"` + testURL + `","token":"` + testToken + `" GARBAGE`
	if err := os.WriteFile(filepath.Join(s.dir, tokenFileName), []byte(corrupt), 0o600); err != nil {
		t.Fatalf("corrupt file: %v", err)
	}
	_, err := s.Read(testURL)
	if err == nil {
		t.Fatal("Read of corrupt file: want error, got nil")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error string leaks the token: %v", err)
	}
}

// TestFileStorePersistsPairShape guards the on-disk format is the {serverURL,
// token} JSON pair, not a bare token (the URL binding lives on disk).
func TestFileStorePersistsPairShape(t *testing.T) {
	s := newTestStore(t)
	if err := s.Write(testURL, testToken); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(s.dir, tokenFileName))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var pair struct {
		ServerURL string `json:"serverUrl"`
		Token     string `json:"token"`
	}
	if err := json.Unmarshal(raw, &pair); err != nil {
		t.Fatalf("stored file is not a JSON pair: %v", err)
	}
	if pair.ServerURL != testURL || pair.Token != testToken {
		t.Fatalf("stored pair = %+v, want {%q,%q}", pair, testURL, testToken)
	}
}
