//go:build unix

package main

// Unit tests for the two injectable seams of the runner-token provisioning CLI:
// resolveDSN (flag/env precedence) and mintAndPrint (mint + print-once
// invariant). Both are exercised without a real Postgres: resolveDSN is pure
// over its flag arg and $COMPASS_DATABASE_DSN, and mintAndPrint takes a
// runnerhub.TokenPutter, so a fake captures the (hash, subject) it stores.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// TestResolveDSN pins compass-server's precedence: an explicit --database flag
// wins over $COMPASS_DATABASE_DSN, the env is the fallback, and with neither the
// caller gets an actionable error. t.Setenv scopes the env per subtest and
// restores it, so the cases are hermetic and order-independent.
func TestResolveDSN(t *testing.T) {
	tests := []struct {
		name    string
		flag    string
		env     string // value set via t.Setenv on COMPASS_DATABASE_DSN
		want    string
		wantErr bool
	}{
		{name: "flag set, env empty", flag: "postgres://flag", env: "", want: "postgres://flag"},
		{name: "flag wins over env", flag: "postgres://flag", env: "postgres://env", want: "postgres://flag"},
		{name: "env fallback when flag empty", flag: "", env: "postgres://env", want: "postgres://env"},
		{name: "both empty errors", flag: "", env: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Set the env unconditionally so a leaked value from the ambient
			// shell can never bleed into a case that expects it empty.
			t.Setenv("COMPASS_DATABASE_DSN", tc.env)

			got, err := resolveDSN(tc.flag)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveDSN(%q) with env %q: want error, got nil (dsn=%q)", tc.flag, tc.env, got)
				}
				if !strings.Contains(err.Error(), "--database") {
					t.Fatalf("error %q does not mention --database", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveDSN(%q) with env %q: unexpected error: %v", tc.flag, tc.env, err)
			}
			if got != tc.want {
				t.Fatalf("resolveDSN(%q) with env %q = %q, want %q", tc.flag, tc.env, got, tc.want)
			}
		})
	}
}

// putCall is one captured PutTokenHash invocation.
type putCall struct {
	hash [32]byte
	subj store.Subject
}

// fakeTokenPutter records every PutTokenHash so mintAndPrint's storage contract
// is asserted directly, and returns a configurable error to drive the failure
// path. It also resolves hashes it has stored, so it doubles as the tokenStore
// mintToFile needs: a hash that was put resolves to its subject; any other hash
// is store.ErrNotFound (a token the store has never seen). It implements both
// runnerhub.TokenPutter and runnerhub.TokenHashResolver.
type fakeTokenPutter struct {
	calls   []putCall
	err     error                      // returned by PutTokenHash when set
	stored  map[[32]byte]store.Subject // hashes this store knows (seed for the "already registered" case)
	resolve error                      // returned by ResolveTokenHash when set (non-sentinel lookup failure)
}

func (f *fakeTokenPutter) PutTokenHash(_ context.Context, hash [32]byte, subj store.Subject) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, putCall{hash: hash, subj: subj})
	if f.stored == nil {
		f.stored = map[[32]byte]store.Subject{}
	}
	f.stored[hash] = subj
	return nil
}

func (f *fakeTokenPutter) ResolveTokenHash(_ context.Context, hash [32]byte) (store.Subject, error) {
	if f.resolve != nil {
		return store.Subject{}, f.resolve
	}
	if subj, ok := f.stored[hash]; ok {
		return subj, nil
	}
	return store.Subject{}, fmt.Errorf("%w: token hash", store.ErrNotFound)
}

// fakeTokenPutter satisfies both store surfaces the mint injects.
var (
	_ runnerhub.TokenPutter       = (*fakeTokenPutter)(nil)
	_ runnerhub.TokenHashResolver = (*fakeTokenPutter)(nil)
)

func TestMintAndPrint(t *testing.T) {
	// success: the plaintext is written exactly once as a single trailing-newline
	// line, and its sha256 equals the hash the store was handed under the Runner
	// keyspace with the given id. This binds the printed credential to the stored
	// hash (the "printed once, hash-only in store" invariant) and to the
	// SubjectRunner keyspace (the OQ7 cross-door isolation).
	t.Run("success prints token whose hash was stored under runner subject", func(t *testing.T) {
		fake := &fakeTokenPutter{}
		var buf bytes.Buffer

		if err := mintAndPrint(context.Background(), fake, "runner-1", &buf); err != nil {
			t.Fatalf("mintAndPrint: unexpected error: %v", err)
		}
		if len(fake.calls) != 1 {
			t.Fatalf("PutTokenHash called %d times, want 1", len(fake.calls))
		}

		out := buf.String()
		// Exactly one trailing newline and nothing after it: a clean single-line
		// credential a redirect can capture. A double-print or missing newline
		// reddens here.
		if !strings.HasSuffix(out, "\n") {
			t.Fatalf("output %q does not end in a newline", out)
		}
		if strings.Count(out, "\n") != 1 {
			t.Fatalf("output has %d newlines, want exactly 1: %q", strings.Count(out, "\n"), out)
		}
		token := strings.TrimRight(out, "\n")
		if token == "" {
			t.Fatal("printed token is empty")
		}
		if strings.TrimSpace(token) != token {
			t.Fatalf("printed token line has surrounding whitespace: %q", out)
		}

		// The printed plaintext is exactly the token whose hash was stored.
		wantHash := sha256.Sum256([]byte(token))
		if fake.calls[0].hash != wantHash {
			t.Fatalf("stored hash != sha256(printed token): stored %x, want %x", fake.calls[0].hash, wantHash)
		}

		if got := fake.calls[0].subj.Kind; got != store.SubjectRunner {
			t.Fatalf("stored subject kind = %d, want SubjectRunner (%d)", got, store.SubjectRunner)
		}
		if got := fake.calls[0].subj.ID; got != "runner-1" {
			t.Fatalf("stored subject id = %q, want %q", got, "runner-1")
		}
	})

	// empty runner id is rejected before any store write and before anything is
	// printed: a token with no subject id could never be resolved, so nothing may
	// leak to stdout and the store must be untouched.
	t.Run("empty runner id errors without printing or storing", func(t *testing.T) {
		fake := &fakeTokenPutter{}
		var buf bytes.Buffer

		if err := mintAndPrint(context.Background(), fake, "", &buf); err == nil {
			t.Fatal("mintAndPrint(\"\"): want error, got nil")
		}
		if buf.Len() != 0 {
			t.Fatalf("nothing must be printed on error, got %q", buf.String())
		}
		if len(fake.calls) != 0 {
			t.Fatalf("PutTokenHash called %d times on empty id, want 0", len(fake.calls))
		}
	})

	// store failure propagates and prints nothing: if the hash could not be
	// persisted, the plaintext must not be emitted (an unstored token is
	// unusable and would leak a dead credential).
	t.Run("store error propagates without printing", func(t *testing.T) {
		fake := &fakeTokenPutter{err: errors.New("boom")}
		var buf bytes.Buffer

		if err := mintAndPrint(context.Background(), fake, "runner-1", &buf); err == nil {
			t.Fatal("mintAndPrint with failing store: want error, got nil")
		}
		if buf.Len() != 0 {
			t.Fatalf("nothing must be printed when the mint fails, got %q", buf.String())
		}
	})
}

// TestWriteTokenFile pins the --token-out sink's file contract: the token lands
// on disk raw (no trailing newline — a bearer credential with a stray \n is
// corrupted), owner-only at 0600 (it's a live secret), and a second write
// atomically replaces the first (the temp+rename overwrite path). Each subtest
// uses its own t.TempDir(), so the cases are hermetic and order-independent.
func TestWriteTokenFile(t *testing.T) {
	// The load-bearing case: the bytes on disk equal the token exactly, with no
	// trailing newline and no extra bytes. If the impl used Fprintln instead of
	// WriteString, the exact-bytes compare below reddens.
	t.Run("writes raw token with no trailing newline", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "runner.token")
		const token = "tok-abc123"

		if err := writeTokenFile(path, token); err != nil {
			t.Fatalf("writeTokenFile: unexpected error: %v", err)
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading token file: %v", err)
		}
		if !bytes.Equal(got, []byte(token)) {
			t.Fatalf("token file bytes = %q, want exactly %q (no trailing newline, no extra bytes)", got, token)
		}
	})

	// The file holds a live credential, so it must be owner-only. If the temp
	// file were created without the chmod 0600, a permissive umask would leave
	// group/other read bits and this reddens.
	t.Run("file is mode 0600", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "runner.token")

		if err := writeTokenFile(path, "tok-perm"); err != nil {
			t.Fatalf("writeTokenFile: unexpected error: %v", err)
		}

		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat token file: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("token file mode = %o, want 0600", got)
		}
	})

	// A second write atomically renames a fresh temp file over the existing
	// path: the call succeeds and the file holds the second token exactly,
	// proving the overwrite (os.CreateTemp + Rename over an existing file) path.
	t.Run("second write overwrites with the new token", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "runner.token")

		if err := writeTokenFile(path, "tok-first"); err != nil {
			t.Fatalf("writeTokenFile (first): unexpected error: %v", err)
		}
		if err := writeTokenFile(path, "tok-second"); err != nil {
			t.Fatalf("writeTokenFile (second): unexpected error: %v", err)
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading token file: %v", err)
		}
		if !bytes.Equal(got, []byte("tok-second")) {
			t.Fatalf("token file bytes = %q, want %q after overwrite", got, "tok-second")
		}
	})
}

// TestMintToFile pins the --token-out sink's store-aware idempotence and its
// write-before-commit ordering. mintToFile takes the tokenStore fake, so the
// (hash, subject) it commits and the resolves it performs are asserted directly,
// and each subtest uses its own t.TempDir() so the cases are hermetic.
func TestMintToFile(t *testing.T) {
	// fresh mint: no file yet, so a token is minted, the file lands raw at 0600,
	// and exactly that token's hash is committed under the Runner subject.
	t.Run("no existing file mints, writes 0600, and stores the hash", func(t *testing.T) {
		assertFreshMintWritesAndStores(t)
	})

	// existing file whose token the store already knows: the common restart. No
	// new mint, no new store write, the file is untouched.
	t.Run("existing registered token is a no-op (no rotation, no new hash)", func(t *testing.T) {
		fake := &fakeTokenPutter{}
		path := filepath.Join(t.TempDir(), "runner.token")
		if err := mintToFile(context.Background(), fake, "runner-1", path, false); err != nil {
			t.Fatalf("seed mint: %v", err)
		}
		before, _ := os.ReadFile(path)
		calls := len(fake.calls)

		if err := mintToFile(context.Background(), fake, "runner-1", path, false); err != nil {
			t.Fatalf("second mintToFile: %v", err)
		}
		if len(fake.calls) != calls {
			t.Errorf("PutTokenHash called again on a registered token (%d -> %d); want no new write", calls, len(fake.calls))
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Errorf("token file changed on a no-op run: %q -> %q", before, after)
		}
	})

	// existing file, but the store has no record of its token (the database was
	// replaced): re-register THAT token — the file is unchanged (no rotation) and
	// the stored hash is exactly sha256(existing file), so the Runner's on-disk
	// credential keeps working against the new store.
	t.Run("existing token unknown to store is re-registered without rotating", func(t *testing.T) {
		assertUnknownTokenReregisteredWithoutRotating(t)
	})

	// T2 ordering: the hash commit must not happen before the file lands, and a
	// failed hash commit must not leave a file holding a token the store rejected.
	t.Run("hash-commit failure removes the just-written file", func(t *testing.T) {
		fake := &fakeTokenPutter{err: errors.New("store down")}
		path := filepath.Join(t.TempDir(), "runner.token")
		if err := mintToFile(context.Background(), fake, "runner-1", path, false); err == nil {
			t.Fatal("mintToFile with a failing store = nil error, want failure")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("token file exists after a failed hash commit (stat err = %v); want no dead credential on disk", err)
		}
	})

	// --force always mints fresh and overwrites, even when the file exists and its
	// token is registered: an explicit rotation.
	t.Run("force rotates a registered token", func(t *testing.T) {
		fake := &fakeTokenPutter{}
		path := filepath.Join(t.TempDir(), "runner.token")
		if err := mintToFile(context.Background(), fake, "runner-1", path, false); err != nil {
			t.Fatalf("seed mint: %v", err)
		}
		first, _ := os.ReadFile(path)

		if err := mintToFile(context.Background(), fake, "runner-1", path, true); err != nil {
			t.Fatalf("force mintToFile: %v", err)
		}
		second, _ := os.ReadFile(path)
		if bytes.Equal(first, second) {
			t.Errorf("force did not rotate the token: still %q", second)
		}
		if len(fake.calls) != 2 {
			t.Errorf("PutTokenHash called %d times across seed+force, want 2", len(fake.calls))
		}
	})
}

// assertFreshMintWritesAndStores verifies the no-existing-file path: a token is
// minted, the file lands raw (no trailing newline) at 0600, and exactly that
// token's hash is committed once under the Runner subject.
func assertFreshMintWritesAndStores(t *testing.T) {
	t.Helper()
	fake := &fakeTokenPutter{}
	path := filepath.Join(t.TempDir(), "runner.token")
	if err := mintToFile(context.Background(), fake, "runner-1", path, false); err != nil {
		t.Fatalf("mintToFile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if len(raw) == 0 || strings.HasSuffix(string(raw), "\n") {
		t.Fatalf("token file = %q, want a non-empty raw token with no trailing newline", raw)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode() & 0o777; mode != 0o600 {
		t.Errorf("token file mode = %o, want 600", mode)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("PutTokenHash called %d times, want 1", len(fake.calls))
	}
	if want := sha256.Sum256(raw); fake.calls[0].hash != want {
		t.Errorf("stored hash != sha256(file token)")
	}
	if fake.calls[0].subj.Kind != store.SubjectRunner || fake.calls[0].subj.ID != "runner-1" {
		t.Errorf("stored subject = %+v, want {SubjectRunner runner-1}", fake.calls[0].subj)
	}
}

// assertUnknownTokenReregisteredWithoutRotating verifies the heal path: an
// existing token file whose hash the store does not know is re-registered as-is
// — the file is not rotated and the stored hash is exactly sha256(existing file),
// so the Runner's on-disk credential keeps working against the replaced store.
func assertUnknownTokenReregisteredWithoutRotating(t *testing.T) {
	t.Helper()
	fake := &fakeTokenPutter{}
	path := filepath.Join(t.TempDir(), "runner.token")
	if err := writeTokenFile(path, "pre-existing-token"); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := mintToFile(context.Background(), fake, "runner-1", path, false); err != nil {
		t.Fatalf("mintToFile heal: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "pre-existing-token" {
		t.Errorf("token file rotated during heal: %q, want the pre-existing token", got)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("PutTokenHash called %d times, want 1 (re-register)", len(fake.calls))
	}
	if want := sha256.Sum256([]byte("pre-existing-token")); fake.calls[0].hash != want {
		t.Errorf("re-registered hash != sha256(existing file token)")
	}
}
