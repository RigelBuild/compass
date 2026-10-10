//go:build unix

// The per-Runner token mint: a dedicated Runner-subject path (NOT the Client-door
// IssueToken) issuing against SubjectRunner under a distinct keyspace so Runner
// and account subjects never collide. An operator step: the plaintext is returned
// once, delivered out of band, stored 0600; the store keeps only the SHA-256 hash.
package runnerhub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/RigelBuild/compass/go/internal/store"
)

// tokenBytes is the minted-token entropy: 32 random bytes, matching the account
// door's IssueToken (compass.proto:245).
const tokenBytes = 32

// TokenPutter is the store write surface the mint needs — just PutTokenHash — so
// the mint is unit-testable against a fake and depends on nothing else in the
// store.
type TokenPutter interface {
	PutTokenHash(ctx context.Context, hash [32]byte, subj store.Subject) error
}

// TokenHashResolver is the store read surface a provisioning path needs to tell
// a token the store already knows from one it has never seen — the read half
// that makes file-based idempotence store-aware rather than file-presence-only.
type TokenHashResolver interface {
	ResolveTokenHash(ctx context.Context, hash [32]byte) (store.Subject, error)
}

// GenerateRunnerToken returns a fresh bearer token — 32 random bytes as base64url
// (no padding), presented as `authorization: Bearer <token>` — without touching
// any store. Pair it with StoreRunnerTokenHash to register the hash;
// MintRunnerToken does both in one call for the common path. Separating the two
// lets a file sink write the plaintext to disk BEFORE committing the hash, so a
// failed file write never orphans a hash the caller can no longer produce.
func GenerateRunnerToken() (string, error) {
	var raw [tokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generating runner token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// StoreRunnerTokenHash registers token's SHA-256 hash under the SubjectRunner
// keyspace for runnerID. Only the hash is stored; the plaintext is the caller's
// to deliver and is never recoverable from the store. A re-used hash is the
// store's ErrConflict.
func StoreRunnerTokenHash(ctx context.Context, st TokenPutter, token, runnerID string) error {
	if runnerID == "" {
		return errors.New("runner id is required to store a token")
	}
	if err := checkMintedRunnerID(runnerID); err != nil {
		return err
	}
	// A token with "." is routed to the projected-token verifier and would never resolve.
	if strings.Contains(token, ".") {
		return fmt.Errorf("%w: runner token must not contain %q", store.ErrInvalidArgument, ".")
	}
	hash := sha256.Sum256([]byte(token))
	if err := st.PutTokenHash(ctx, hash, store.Subject{Kind: store.SubjectRunner, ID: runnerID}); err != nil {
		return fmt.Errorf("storing runner token hash: %w", err)
	}
	return nil
}

// TokenState is what the store knows about a runner token file's token, relative
// to the runner id the caller provisions for.
type TokenState int

const (
	// TokenUnknown: the store never saw the hash (e.g. the database was
	// replaced), so the caller heals by re-registering that exact token.
	TokenUnknown TokenState = iota
	// TokenRegistered: the hash is live under this runner's subject.
	TokenRegistered
	// TokenRevoked: the operator revoked it deliberately; leave it (--force rotates).
	TokenRevoked
	// TokenOtherSubject: the hash is live under a different subject, so it can
	// never enroll as this runner and the hash cannot be re-registered — rotate.
	TokenOtherSubject
)

// RunnerTokenStatus classifies token against the store for runnerID and returns
// the resolved subject (zero unless live). Any other lookup error surfaces.
func RunnerTokenStatus(ctx context.Context, r TokenHashResolver, token, runnerID string) (TokenState, store.Subject, error) {
	hash := sha256.Sum256([]byte(token))
	subj, err := r.ResolveTokenHash(ctx, hash)
	switch {
	case err == nil:
		if subj.Kind == store.SubjectRunner && subj.ID == runnerID {
			return TokenRegistered, subj, nil
		}
		return TokenOtherSubject, subj, nil
	case errors.Is(err, store.ErrNotFound):
		return TokenUnknown, store.Subject{}, nil
	case errors.Is(err, store.ErrTokenRevoked):
		return TokenRevoked, store.Subject{}, nil
	default:
		return TokenUnknown, store.Subject{}, fmt.Errorf("resolving runner token hash: %w", err)
	}
}

// MintRunnerToken mints a bearer token for runnerID under the SubjectRunner
// keyspace, stores only its SHA-256 hash, and returns the plaintext exactly once
// (base64url, no padding — present as `authorization: Bearer <token>`). The
// caller delivers it to the Runner host out of band and stores it 0600; it is
// never recoverable from the store. A re-used hash (astronomically unlikely) is
// the store's ErrConflict. It is the generate-then-store composition; a file
// sink that must order the file write before the hash commit uses the two halves
// directly.
func MintRunnerToken(ctx context.Context, st TokenPutter, runnerID string) (string, error) {
	if runnerID == "" {
		return "", errors.New("runner id is required to mint a token")
	}
	if err := checkMintedRunnerID(runnerID); err != nil {
		return "", err
	}
	token, err := GenerateRunnerToken()
	if err != nil {
		return "", err
	}
	if err := StoreRunnerTokenHash(ctx, st, token, runnerID); err != nil {
		return "", err
	}
	return token, nil
}

// checkMintedRunnerID rejects "/", which is reserved for projected-token Runner
// IDs ("<cluster>/<node>") so a minted ID can never shadow one.
func checkMintedRunnerID(runnerID string) error {
	if strings.Contains(runnerID, "/") {
		return fmt.Errorf("%w: runner id %q must not contain %q", store.ErrInvalidArgument, runnerID, "/")
	}
	return nil
}
