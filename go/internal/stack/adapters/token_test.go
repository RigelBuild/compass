//go:build unix

package adapters

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// fakeTokenStore is an in-memory tokenStore: it records hash→subject with a
// write counter so a test can assert no re-mint/re-register happened. It mirrors
// the real store's contract for the two methods ensure exercises — an unknown
// hash resolves to store.ErrNotFound (so RunnerTokenRegistered reports false).
type fakeTokenStore struct {
	hashes map[[32]byte]store.Subject
	writes int
}

func newFakeTokenStore() *fakeTokenStore {
	return &fakeTokenStore{hashes: make(map[[32]byte]store.Subject)}
}

func (f *fakeTokenStore) PutTokenHash(_ context.Context, hash [32]byte, subj store.Subject) error {
	f.writes++
	f.hashes[hash] = subj
	return nil
}

func (f *fakeTokenStore) ResolveTokenHash(_ context.Context, hash [32]byte) (store.Subject, error) {
	subj, ok := f.hashes[hash]
	if !ok {
		return store.Subject{}, store.ErrNotFound
	}
	return subj, nil
}

// hashOf is the store's token-hash function, so a test can seed the fake with a
// token's hash exactly as ensure will look it up.
func hashOf(token string) [32]byte { return sha256.Sum256([]byte(token)) }

func TestEnsure(t *testing.T) {
	t.Run("absent file mints, writes 0600, registers hash", testEnsureAbsentMints)
	t.Run("present and registered returns same token, no re-mint", testEnsurePresentRegistered)
	t.Run("present but store forgot re-registers same token, file unchanged", testEnsurePresentStoreForgot)
}

func testEnsureAbsentMints(t *testing.T) {
	const runnerID = "runner-alpha"
	dir := t.TempDir()
	st := newFakeTokenStore()

	token, err := ensure(context.Background(), st, dir, runnerID)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if token == "" {
		t.Fatal("minted token is empty")
	}

	path := filepath.Join(dir, tokenFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("token file mode = %o, want 600", perm)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(onDisk) != token {
		t.Fatalf("file token %q != returned token %q", onDisk, token)
	}

	subj, ok := st.hashes[hashOf(token)]
	if !ok {
		t.Fatal("store has no hash registered for the minted token")
	}
	if subj.Kind != store.SubjectRunner || subj.ID != runnerID {
		t.Fatalf("registered subject = %+v, want {SubjectRunner, %q}", subj, runnerID)
	}
	if st.writes != 1 {
		t.Fatalf("store writes = %d, want 1 (one mint)", st.writes)
	}
}

func testEnsurePresentRegistered(t *testing.T) {
	const runnerID = "runner-alpha"
	dir := t.TempDir()
	st := newFakeTokenStore()

	// Seed: a token file whose hash the store already knows.
	const existing = "seeded-registered-token"
	path := filepath.Join(dir, tokenFileName)
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatalf("seed token file: %v", err)
	}
	st.hashes[hashOf(existing)] = store.Subject{Kind: store.SubjectRunner, ID: runnerID}
	before := string(readFile(t, path))

	token, err := ensure(context.Background(), st, dir, runnerID)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if token != existing {
		t.Fatalf("returned token %q, want existing %q", token, existing)
	}
	if after := string(readFile(t, path)); after != before {
		t.Fatalf("file bytes changed: before %q, after %q", before, after)
	}
}

func testEnsurePresentStoreForgot(t *testing.T) {
	const runnerID = "runner-alpha"
	dir := t.TempDir()
	st := newFakeTokenStore() // empty: simulates a replaced database

	const existing = "seeded-orphaned-token"
	path := filepath.Join(dir, tokenFileName)
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatalf("seed token file: %v", err)
	}
	before := string(readFile(t, path))

	token, err := ensure(context.Background(), st, dir, runnerID)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if token != existing {
		t.Fatalf("returned token %q, want existing %q (no rotation)", token, existing)
	}
	if after := string(readFile(t, path)); after != before {
		t.Fatalf("file bytes changed: before %q, after %q", before, after)
	}

	subj, ok := st.hashes[hashOf(existing)]
	if !ok {
		t.Fatal("store did not re-register the existing token")
	}
	if subj.Kind != store.SubjectRunner || subj.ID != runnerID {
		t.Fatalf("re-registered subject = %+v, want {SubjectRunner, %q}", subj, runnerID)
	}
	if st.writes != 1 {
		t.Fatalf("store writes = %d, want 1 (one re-register)", st.writes)
	}
}

// TestEnsurePropagatesReadError proves a token file that exists but cannot be
// read surfaces the error rather than silently minting over it.
func TestEnsurePropagatesReadError(t *testing.T) {
	dir := t.TempDir()
	st := newFakeTokenStore()

	// A directory at the token path: os.ReadFile fails with a non-not-exist
	// error, which ensure must surface.
	path := filepath.Join(dir, tokenFileName)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("seed dir at token path: %v", err)
	}

	_, err := ensure(context.Background(), st, dir, runnerID())
	if err == nil {
		t.Fatal("ensure returned nil error for an unreadable token file")
	}
	if errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ensure mis-mapped read error to not-found: %v", err)
	}
}

func runnerID() string { return "runner-alpha" }
