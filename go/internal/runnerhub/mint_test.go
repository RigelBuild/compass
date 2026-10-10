//go:build unix

package runnerhub

// MintRunnerToken round-trips: the hash stored is exactly sha256(token), the
// subject is stored under Kind=SubjectRunner with the runner id, and each mint
// yields a fresh, valid base64url token. A fake TokenPutter captures the (hash,
// subject) so the contract is asserted without a store.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// putCall is one captured PutTokenHash.
type putCall struct {
	hash [32]byte
	subj store.Subject
}

// fakeTokenPutter captures every PutTokenHash so the mint's storage contract is
// asserted directly.
type fakeTokenPutter struct {
	calls []putCall
	err   error // returned by PutTokenHash when set
}

func (f *fakeTokenPutter) PutTokenHash(_ context.Context, hash [32]byte, subj store.Subject) error {
	f.calls = append(f.calls, putCall{hash: hash, subj: subj})
	return f.err
}

// The stored subject is Kind=SubjectRunner with the runner id, and the stored
// hash is exactly sha256 of the returned plaintext token. A bug that stored the
// account keyspace, the wrong id, or a hash of something other than the token
// would redden one of these.
func TestMintRunnerTokenStoresRunnerSubjectAndTokenHash(t *testing.T) {
	putter := &fakeTokenPutter{}
	token, err := MintRunnerToken(context.Background(), putter, "runner-42")
	if err != nil {
		t.Fatalf("MintRunnerToken = %v, want success", err)
	}
	if len(putter.calls) != 1 {
		t.Fatalf("PutTokenHash called %d times, want 1", len(putter.calls))
	}
	call := putter.calls[0]

	if call.subj.Kind != store.SubjectRunner {
		t.Fatalf("stored subject kind = %v, want SubjectRunner (never the account keyspace)", call.subj.Kind)
	}
	if call.subj.ID != "runner-42" {
		t.Fatalf("stored subject id = %q, want runner-42", call.subj.ID)
	}

	// The stored hash is exactly sha256(token) — the door resolves a presented
	// token by hashing it, so a mismatch means the minted token can never
	// authenticate.
	want := sha256.Sum256([]byte(token))
	if call.hash != want {
		t.Fatalf("stored hash != sha256(returned token); the minted token could never resolve")
	}

	// The plaintext is valid base64url (no padding) of 32 bytes — the wire
	// format the door parses.
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("returned token is not valid base64url: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("decoded token = %d bytes, want 32 (the entropy contract)", len(raw))
	}
}

// Each mint yields a distinct token (fresh entropy), so two Runners never share
// a credential. A bug that reused a buffer or a constant would collide.
func TestMintRunnerTokenDistinctPerCall(t *testing.T) {
	putter := &fakeTokenPutter{}
	seen := map[string]bool{}
	seenHash := map[[32]byte]bool{}
	for i := range 8 {
		tok, err := MintRunnerToken(context.Background(), putter, "runner")
		if err != nil {
			t.Fatalf("mint %d = %v, want success", i, err)
		}
		if seen[tok] {
			t.Fatalf("mint %d returned a duplicate token; each mint must be fresh entropy", i)
		}
		seen[tok] = true
	}
	// The stored hashes are likewise all distinct.
	for _, c := range putter.calls {
		if seenHash[c.hash] {
			t.Fatal("two mints stored the same hash; entropy collision")
		}
		seenHash[c.hash] = true
	}
}

// An empty runner id is rejected before any store write — a token with no
// subject id can never be resolved, so it is a caller error, not a stored no-op.
func TestMintRunnerTokenRequiresRunnerId(t *testing.T) {
	putter := &fakeTokenPutter{}
	_, err := MintRunnerToken(context.Background(), putter, "")
	if err == nil {
		t.Fatal("MintRunnerToken with empty runner id = nil error, want a required-id error")
	}
	if len(putter.calls) != 0 {
		t.Fatalf("PutTokenHash called %d times for an empty id, want 0 (reject before storing)", len(putter.calls))
	}
}

// "/" is reserved for projected-token IDs; a minted "a/b" could otherwise
// impersonate the Runner on node b of cluster a.
func TestMintRunnerTokenRejectsSlashInRunnerID(t *testing.T) {
	putter := &fakeTokenPutter{}
	_, err := MintRunnerToken(t.Context(), putter, "a/b")
	if !errors.Is(err, store.ErrInvalidArgument) {
		t.Fatalf("MintRunnerToken(a/b) = %v, want ErrInvalidArgument", err)
	}
	if len(putter.calls) != 0 {
		t.Fatalf("PutTokenHash called %d times, want 0", len(putter.calls))
	}
	if err := StoreRunnerTokenHash(t.Context(), putter, "tok", "a/b"); !errors.Is(err, store.ErrInvalidArgument) {
		t.Fatalf("StoreRunnerTokenHash(id a/b) = %v, want ErrInvalidArgument", err)
	}
	if len(putter.calls) != 0 {
		t.Fatalf("PutTokenHash called %d times, want 0", len(putter.calls))
	}
}

// A token with "." goes to the projected-token branch at the door and could
// never resolve by hash, so storing one is refused.
func TestStoreRunnerTokenHashRejectsDotInToken(t *testing.T) {
	putter := &fakeTokenPutter{}
	err := StoreRunnerTokenHash(t.Context(), putter, "a.b.c", "runner-1")
	if !errors.Is(err, store.ErrInvalidArgument) {
		t.Fatalf("StoreRunnerTokenHash(token with dot) = %v, want ErrInvalidArgument", err)
	}
	if len(putter.calls) != 0 {
		t.Fatalf("PutTokenHash called %d times, want 0", len(putter.calls))
	}
}

// fakeHashResolver answers ResolveTokenHash from a fixed map or a fixed error.
type fakeHashResolver struct {
	known map[[32]byte]store.Subject
	err   error
}

func (f fakeHashResolver) ResolveTokenHash(_ context.Context, hash [32]byte) (store.Subject, error) {
	if f.err != nil {
		return store.Subject{}, f.err
	}
	if s, ok := f.known[hash]; ok {
		return s, nil
	}
	return store.Subject{}, store.ErrNotFound
}

// A hash live under another subject must not read as registered for this
// runner: that skip stranded enrollment after a dev-DB wipe.
func TestRunnerTokenStatus(t *testing.T) {
	const tok = "tok"
	h := sha256.Sum256([]byte(tok))
	dogfood := store.Subject{Kind: store.SubjectRunner, ID: "dogfood"}
	tests := []struct {
		name      string
		r         fakeHashResolver
		want      TokenState
		wantPrior store.Subject
	}{
		{"same runner", fakeHashResolver{known: map[[32]byte]store.Subject{h: {Kind: store.SubjectRunner, ID: "r1"}}}, TokenRegistered, store.Subject{Kind: store.SubjectRunner, ID: "r1"}},
		{"other runner id", fakeHashResolver{known: map[[32]byte]store.Subject{h: dogfood}}, TokenOtherSubject, dogfood},
		{"same id other kind", fakeHashResolver{known: map[[32]byte]store.Subject{h: {Kind: store.SubjectAccount, ID: "r1"}}}, TokenOtherSubject, store.Subject{Kind: store.SubjectAccount, ID: "r1"}},
		{"unknown", fakeHashResolver{}, TokenUnknown, store.Subject{}},
		{"revoked", fakeHashResolver{err: store.ErrTokenRevoked}, TokenRevoked, store.Subject{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, prior, err := RunnerTokenStatus(context.Background(), tc.r, tok, "r1")
			if err != nil {
				t.Fatalf("RunnerTokenStatus: %v", err)
			}
			if got != tc.want {
				t.Fatalf("RunnerTokenStatus = %d, want %d", got, tc.want)
			}
			if prior != tc.wantPrior {
				t.Fatalf("prior subject = %+v, want %+v", prior, tc.wantPrior)
			}
		})
	}
	t.Run("lookup failure surfaces", func(t *testing.T) {
		if _, _, err := RunnerTokenStatus(context.Background(), fakeHashResolver{err: errors.New("db down")}, tok, "r1"); err == nil {
			t.Fatal("RunnerTokenStatus with a failing store = nil error, want failure")
		}
	})
}
