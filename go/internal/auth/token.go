package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/sealedsecurity/compass/go/internal/store"
)

// tokenBytes is the number of random bytes in a freshly minted token, before
// base64url encoding. 32 bytes = 256 bits of entropy, so a token is infeasible
// to guess.
const tokenBytes = 32

// mintToken returns a fresh bearer token: 32 random bytes, base64url-encoded (no
// padding). The plaintext is the credential shown to its holder exactly once; the
// store of record retains only its SHA-256 hash (IssueAccountToken), so the
// plaintext is unrecoverable after issuance.
func mintToken() string {
	var raw [tokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand.Read never returns an error on the supported platforms
		// (it reads from the OS RNG); a failure here means the OS entropy source
		// is unavailable, which is unrecoverable for a security token.
		panic("auth: OS RNG for a fresh auth token: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

// hashToken is the SHA-256 digest of a token's base64url string — the token
// store's row key. The one place issuance and resolution agree on how a token
// becomes a key, so they can never drift apart.
func hashToken(token string) [32]byte {
	return sha256.Sum256([]byte(token))
}

// IssueAccountToken mints a bearer token for account and persists its hash in the
// store of record under an account subject (store.SubjectAccount), returning the
// plaintext once. The store keeps only the SHA-256 hash, so the credential is
// unrecoverable after this call; present it later as "authorization: Bearer
// <token>" and ResolveToken maps it back.
//
// A hash collision (PutTokenHash returns store.ErrConflict) is astronomically
// improbable for a 256-bit CSPRNG token, and it is not a client error, so on the
// off chance it happens the token is re-minted once rather than surfaced. A
// second collision is treated as a real error (an entropy or store fault).
func IssueAccountToken(ctx context.Context, st *store.Store, account store.AccountID) (string, error) {
	subj := store.Subject{Kind: store.SubjectAccount, ID: string(account)}
	for range 2 {
		token := mintToken()
		err := st.PutTokenHash(ctx, hashToken(token), subj)
		if err == nil {
			return token, nil
		}
		if errors.Is(err, store.ErrConflict) {
			continue // astronomically rare hash collision: re-mint and retry once
		}
		return "", fmt.Errorf("persisting issued token: %w", err)
	}
	return "", errors.New("persisting issued token: hash collision on two successive mints")
}

// Sentinel resolution failures returned by ResolveToken. They exist so the
// server can LOG which case fired (audit), NOT so a door can tell them apart to
// the client: a door MUST map all three to the same bare CodeUnauthenticated, or
// the response becomes an oracle for whether a token is unknown, revoked, or
// issued for the other door.
var (
	// ErrTokenNotFound: the presented token was never issued (or the store has
	// no live record of it). Any unexpected store error folds here too, so
	// resolution fails closed.
	ErrTokenNotFound = errors.New("auth: token not found")
	// ErrTokenRevoked: the token was issued but has since been withdrawn.
	ErrTokenRevoked = errors.New("auth: token revoked")
	// ErrWrongKind: the token resolves, but to the other subject kind — a Runner
	// token presented to the account door, or an account token to the Runner
	// door. The OQ7 cross-door rejection (design.md:1308-1314).
	ErrWrongKind = errors.New("auth: token subject kind mismatch")
)

// ResolveToken authenticates a presented bearer to a subject of the required
// kind, enforcing the cross-door subject rule (design.md:1308-1314): a token
// minted for one kind is unauthenticated on a door serving the other. It hashes
// the presentation and resolves the digest against the store of record (the
// comparison never touches a stored plaintext — there is none), then verifies
// the resolved subject's kind against want.
//
// It returns a distinct sentinel per failure — ErrTokenNotFound (never issued or
// an unexpected store error, folded here to fail closed), ErrTokenRevoked
// (withdrawn), ErrWrongKind (issued for the other door) — so the server can log
// which fired. Every caller MUST map all three to the same bare
// CodeUnauthenticated: the distinction is a server-side audit signal, never a
// client-visible one (a distinguishable response is a token-existence oracle).
// Both the account door (want=SubjectAccount) and the Runner door
// (want=SubjectRunner) share this one resolver, so the security-critical
// resolve+kind-gate lives and is tested in exactly one place; each door adds only
// its own trivial typed wrap on the returned Subject.
func ResolveToken(ctx context.Context, st *store.Store, presented string, want store.SubjectKind) (store.Subject, error) {
	subj, err := st.ResolveTokenHash(ctx, hashToken(presented))
	if err != nil {
		if errors.Is(err, store.ErrTokenRevoked) {
			return store.Subject{}, ErrTokenRevoked
		}
		// store.ErrNotFound — and any other store error — is not a live
		// credential; fail closed as not-found.
		return store.Subject{}, ErrTokenNotFound
	}
	if subj.Kind != want {
		return store.Subject{}, ErrWrongKind
	}
	return subj, nil
}
