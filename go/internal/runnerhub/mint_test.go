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
