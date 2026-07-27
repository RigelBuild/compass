//go:build pgtest && unix

package auth

// Token issue/resolve contract tests (SEA-1195 T3, the S3 gate), transcribed
// from the authoritative Rust suite in crates/compass-daemon/src/auth.rs
// (#[cfg(test)] mod tests). Intent is carried onto Go idioms; the Rust is the
// spec, not a template.
//
// The seam is now the Postgres store of record (T1): IssueAccountToken persists
// a token's hash under a store.Subject and returns the plaintext once; the
// EXPORTED ResolveToken hashes a presentation, resolves it against the store,
// and enforces the cross-door subject-kind gate — returning a distinct sentinel
// per failure (ErrTokenNotFound / ErrTokenRevoked / ErrWrongKind) so the server
// can audit-log which fired. These tests require a live database, so they are in
// the `pgtest` lane (openTestStore SKIPs when no runtime is available).
//
// White-box (package auth) to reuse the unexported hashToken helper — the one
// place issuance and resolution agree on how a token becomes a store key, so a
// test that revokes by hash addresses the exact row IssueAccountToken wrote.

import (
	"context"
	"errors"
	"testing"

	"github.com/sealedsecurity/compass/go/internal/store"
)

// issue_then_resolve_round_trips_to_the_issued_account: a freshly minted account
// token resolves to a SubjectAccount carrying the account it was issued for.
func TestIssueThenResolveRoundTripsToTheIssuedAccount(t *testing.T) {
	ctx := context.Background()
	st, admin, _ := openTestStore(t)

	token, err := IssueAccountToken(ctx, st, admin)
	if err != nil {
		t.Fatalf("IssueAccountToken: %v", err)
	}

	subj, err := ResolveToken(ctx, st, token, store.SubjectAccount)
	if err != nil {
		t.Fatalf("a freshly issued token must resolve: %v", err)
	}
	if subj.Kind != store.SubjectAccount {
		t.Fatalf("resolved kind = %d, want SubjectAccount", subj.Kind)
	}
	if subj.ID != string(admin) {
		t.Fatalf("resolve must return the account the token was minted for: got %q, want %q", subj.ID, admin)
	}
}

// unknown_token_resolves_to_none: a token the store never issued must not
// resolve — even with an unrelated live token present — and the failure is the
// distinct ErrTokenNotFound sentinel.
func TestUnknownTokenResolvesToNotFound(t *testing.T) {
	ctx := context.Background()
	st, admin, _ := openTestStore(t)

	// A live token exists, so the miss is the lookup failing, not an empty store.
	if _, err := IssueAccountToken(ctx, st, admin); err != nil {
		t.Fatalf("seeding a live token: %v", err)
	}

	_, err := ResolveToken(ctx, st, "aW52YWxpZC10b2tlbg", store.SubjectAccount)
	if !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("a never-issued token must be ErrTokenNotFound, got %v", err)
	}
}

// a revoked token stops resolving and surfaces the distinct ErrTokenRevoked
// sentinel — separate from ErrTokenNotFound so the server can tell a withdrawn
// credential from an unknown one (the distinction is audit-only; the door still
// maps both to one CodeUnauthenticated).
func TestRevokedTokenResolvesToRevoked(t *testing.T) {
	ctx := context.Background()
	st, admin, _ := openTestStore(t)

	token, err := IssueAccountToken(ctx, st, admin)
	if err != nil {
		t.Fatalf("IssueAccountToken: %v", err)
	}
	hash := hashToken(token)
	if err := st.RevokeToken(ctx, hash); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	_, err = ResolveToken(ctx, st, token, store.SubjectAccount)
	if !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("a revoked token must be ErrTokenRevoked, got %v", err)
	}
}

// distinct_issues_produce_distinct_tokens_and_identities: two accounts, two
// issues → distinct random token strings, each resolving to its own account.
func TestDistinctIssuesProduceDistinctTokensAndIdentities(t *testing.T) {
	ctx := context.Background()
	st, a, b := openTestStore(t)

	ta, err := IssueAccountToken(ctx, st, a)
	if err != nil {
		t.Fatalf("IssueAccountToken(a): %v", err)
	}
	tb, err := IssueAccountToken(ctx, st, b)
	if err != nil {
		t.Fatalf("IssueAccountToken(b): %v", err)
	}

	if ta == tb {
		t.Fatal("each issue must mint a fresh random token, got identical strings")
	}
	if subj, err := ResolveToken(ctx, st, ta, store.SubjectAccount); err != nil || subj.ID != string(a) {
		t.Fatalf("token A must resolve to account A: got %q err=%v, want %q", subj.ID, err, a)
	}
	if subj, err := ResolveToken(ctx, st, tb, store.SubjectAccount); err != nil || subj.ID != string(b) {
		t.Fatalf("token B must resolve to account B: got %q err=%v, want %q", subj.ID, err, b)
	}
}

// TestCrossDoorTokenIsWrongKind is the OQ7 cross-door rejection
// (design.md:1308-1314), the security seam both doors share: a token minted for
// one subject kind is NOT authenticated on a door serving the other. A Runner
// token presented where an account is wanted, and an account token presented
// where a Runner is wanted, each resolve the hash but fail the kind gate with the
// distinct ErrWrongKind — never a success, never ErrTokenNotFound. The symmetric
// pair proves the gate compares against `want`, not a hard-coded kind.
func TestCrossDoorTokenIsWrongKind(t *testing.T) {
	ctx := context.Background()
	st, admin, _ := openTestStore(t)

	// A Runner-kind token put directly into the store (the Runner door's issuer
	// is a separate crate; here we only need a live Runner-subject row to present
	// at the account door). hashToken agrees with IssueAccountToken on the key.
	runnerToken := "cnVubmVyLXRva2Vu" // base64url-shaped, never account-issued
	if err := st.PutTokenHash(ctx, hashToken(runnerToken), store.Subject{Kind: store.SubjectRunner, ID: "some-runner"}); err != nil {
		t.Fatalf("PutTokenHash(runner): %v", err)
	}

	t.Run("runner token wanted as account is WrongKind", func(t *testing.T) {
		_, err := ResolveToken(ctx, st, runnerToken, store.SubjectAccount)
		if !errors.Is(err, ErrWrongKind) {
			t.Fatalf("a Runner token at the account door must be ErrWrongKind, got %v", err)
		}
	})

	t.Run("account token wanted as runner is WrongKind", func(t *testing.T) {
		accountToken, err := IssueAccountToken(ctx, st, admin)
		if err != nil {
			t.Fatalf("IssueAccountToken: %v", err)
		}
		_, err = ResolveToken(ctx, st, accountToken, store.SubjectRunner)
		if !errors.Is(err, ErrWrongKind) {
			t.Fatalf("an account token at the Runner door must be ErrWrongKind, got %v", err)
		}
	})
}
