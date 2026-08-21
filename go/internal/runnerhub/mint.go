//go:build unix

// The per-Runner token mint (OQ7, go-toolchain-default.md:1410-1423). A
// dedicated Runner-subject mint path — NOT the Client-door IssueToken — issues a
// token against store.Subject{Kind: SubjectRunner}, reusing T3's hash-only token
// store under the distinct SubjectRunner keyspace so a Runner subject and an
// account subject share one store but can never collide.
//
// Minting is an operator provisioning step, not an automated RPC: the plaintext
// token is returned once, delivered to the Runner host out of band, and stored
// there 0600 (the same class as the bootstrap-admin token). The store keeps only
// the SHA-256 hash. This function is the seam a provisioning CLI/admin path
// calls; there is deliberately no RunnerService RPC that mints.
package runnerhub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

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
	hash := sha256.Sum256([]byte(token))
	if err := st.PutTokenHash(ctx, hash, store.Subject{Kind: store.SubjectRunner, ID: runnerID}); err != nil {
		return fmt.Errorf("storing runner token hash: %w", err)
	}
	return nil
}

// RunnerTokenRegistered reports whether token's hash is already known to the
// store. A never-registered token is (false, nil) so a provisioning path can
// heal a token file whose hash the store lost (e.g. the database was replaced)
// by re-registering that exact token instead of rotating it. A revoked token is
// (true, nil): the store knows it and the operator revoked it deliberately, so
// it is left alone (pass --force to rotate). Any other lookup error surfaces.
func RunnerTokenRegistered(ctx context.Context, r TokenHashResolver, token string) (bool, error) {
	hash := sha256.Sum256([]byte(token))
	_, err := r.ResolveTokenHash(ctx, hash)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	case errors.Is(err, store.ErrTokenRevoked):
		return true, nil
	default:
		return false, fmt.Errorf("resolving runner token hash: %w", err)
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
	token, err := GenerateRunnerToken()
	if err != nil {
		return "", err
	}
	if err := StoreRunnerTokenHash(ctx, st, token, runnerID); err != nil {
		return "", err
	}
	return token, nil
}
