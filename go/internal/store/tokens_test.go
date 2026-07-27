//go:build pgtest

package store

// Token contracts: a hash resolves to its subject WITH kind; an Account-kind and
// a Runner-kind token that carry the same id string do not collide; an unknown
// hash is ErrNotFound while a revoked one is the distinct ErrTokenRevoked; revoke
// is idempotent; revoking an unknown hash is ErrNotFound; and re-putting a stored
// hash is ErrConflict.

import (
	"context"
	"crypto/sha256"
	"testing"
)

// tokenHash derives a deterministic [32]byte hash from a label, so each test
// addresses a distinct token without holding any plaintext credential.
func tokenHash(label string) [32]byte {
	return sha256.Sum256([]byte(label))
}

func TestPutResolveRoundTripCarriesKind(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	hash := tokenHash("acct-token")
	want := Subject{Kind: SubjectAccount, ID: "account-123"}
	if err := s.PutTokenHash(ctx, hash, want); err != nil {
		t.Fatalf("PutTokenHash: %v", err)
	}
	got, err := s.ResolveTokenHash(ctx, hash)
	if err != nil {
		t.Fatalf("ResolveTokenHash: %v", err)
	}
	if got != want {
		t.Fatalf("resolved = %+v, want %+v (kind must round-trip)", got, want)
	}
}

func TestTokenKindsDoNotCollideOnSameID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Two distinct tokens whose subjects share an id STRING but differ in kind.
	// The id space is per-kind, so they must resolve to their own kind, never
	// cross over.
	const sharedID = "id-shared"
	accountHash := tokenHash("account-side")
	runnerHash := tokenHash("runner-side")
	if err := s.PutTokenHash(ctx, accountHash, Subject{Kind: SubjectAccount, ID: sharedID}); err != nil {
		t.Fatalf("PutTokenHash(account): %v", err)
	}
	if err := s.PutTokenHash(ctx, runnerHash, Subject{Kind: SubjectRunner, ID: sharedID}); err != nil {
		t.Fatalf("PutTokenHash(runner): %v", err)
	}

	gotAccount, err := s.ResolveTokenHash(ctx, accountHash)
	if err != nil {
		t.Fatalf("ResolveTokenHash(account): %v", err)
	}
	if gotAccount.Kind != SubjectAccount {
		t.Fatalf("account token resolved to kind %d, want SubjectAccount", gotAccount.Kind)
	}
	gotRunner, err := s.ResolveTokenHash(ctx, runnerHash)
	if err != nil {
		t.Fatalf("ResolveTokenHash(runner): %v", err)
	}
	if gotRunner.Kind != SubjectRunner {
		t.Fatalf("runner token resolved to kind %d, want SubjectRunner", gotRunner.Kind)
	}
}

func TestResolveUnknownNotFound(t *testing.T) {
	_, err := newTestStore(t).ResolveTokenHash(context.Background(), tokenHash("never-issued"))
	sentinelIs(t, err, ErrNotFound, "unknown token hash")
}

func TestResolveRevokedIsDistinctFromNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	hash := tokenHash("to-revoke")
	if err := s.PutTokenHash(ctx, hash, Subject{Kind: SubjectAccount, ID: "acct"}); err != nil {
		t.Fatalf("PutTokenHash: %v", err)
	}
	if err := s.RevokeToken(ctx, hash); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	_, err := s.ResolveTokenHash(ctx, hash)
	// A revoked token is ErrTokenRevoked — and specifically NOT ErrNotFound, so
	// the door can tell a withdrawn credential from an unknown one.
	sentinelIs(t, err, ErrTokenRevoked, "revoked token")
	if isSentinel(err, ErrNotFound) {
		t.Fatal("revoked token also matched ErrNotFound; the two must be distinct")
	}
}

func TestRevokeIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	hash := tokenHash("revoke-twice")
	if err := s.PutTokenHash(ctx, hash, Subject{Kind: SubjectRunner, ID: "runner"}); err != nil {
		t.Fatalf("PutTokenHash: %v", err)
	}
	if err := s.RevokeToken(ctx, hash); err != nil {
		t.Fatalf("RevokeToken(first): %v", err)
	}
	// A second revoke of an already-revoked token is a no-op success, not an error.
	if err := s.RevokeToken(ctx, hash); err != nil {
		t.Fatalf("RevokeToken(second) = %v, want nil (idempotent)", err)
	}
}

func TestRevokeUnknownNotFound(t *testing.T) {
	err := newTestStore(t).RevokeToken(context.Background(), tokenHash("never-issued"))
	sentinelIs(t, err, ErrNotFound, "revoke unknown token")
}

func TestPutDuplicateHashConflicts(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	hash := tokenHash("dup-hash")
	if err := s.PutTokenHash(ctx, hash, Subject{Kind: SubjectAccount, ID: "a"}); err != nil {
		t.Fatalf("PutTokenHash(first): %v", err)
	}
	// Re-putting the same hash (even with a different subject) is a conflict.
	err := s.PutTokenHash(ctx, hash, Subject{Kind: SubjectAccount, ID: "b"})
	sentinelIs(t, err, ErrConflict, "duplicate token hash")
}

func TestPutEmptySubjectIDInvalid(t *testing.T) {
	err := newTestStore(t).PutTokenHash(context.Background(), tokenHash("empty-subj"), Subject{Kind: SubjectAccount})
	sentinelIs(t, err, ErrInvalidArgument, "empty subject id")
}
