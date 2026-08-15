//go:build pgtest && unix

package server

// Store-gated RevokeToken round-trip over the production network-door chain
// (bearer + admin gate) via a real connect client: the same integration seam as
// the IssueToken door tests. RevokeToken is admin-gated (classifyProcedure), so
// the admin bearer is what clears the gate; the handler hashes the presented
// plaintext server-side and marks the stored hash revoked. Behind `pgtest &&
// unix` via the shared pgtest harness (SKIP when no runtime).
//
// The load-bearing assertion: after a revoke, auth.ResolveToken fails the token
// as ErrTokenRevoked — the credential is genuinely withdrawn, not merely
// reported revoked. Plus the two edge codes: an unknown token → CodeNotFound; an
// already-revoked token → success (idempotent no-op).

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/auth"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// TestRevokeTokenRoundTrip drives RevokeToken through the production network-door
// chain: an account's issued token is revoked over the RPC, then resolution fails
// it as revoked. Also pins the two edge codes (unknown → NotFound, already-revoked
// → success).
func TestRevokeTokenRoundTrip(t *testing.T) {
	ctx := t.Context()
	st, admin, _ := newNetworkStore(t)
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("test", bus, st, nil, nil, nil, nil)
	client := networkDoorHandler(t, svc, st, admin)

	adminTok, err := auth.IssueAccountToken(ctx, st, admin)
	if err != nil {
		t.Fatalf("IssueAccountToken(admin): %v", err)
	}

	// An account whose token we revoke.
	acct, err := st.CreateUser(ctx, store.NewUser{Handle: "revokee", DisplayName: "revokee"})
	if err != nil {
		t.Fatalf("CreateUser(revokee): %v", err)
	}

	revoke := func(bearer, token string) error {
		req := connect.NewRequest(&compassv1.RevokeTokenRequest{Token: token})
		req.Header().Set("Authorization", "Bearer "+bearer)
		rpcCtx, cancel := context.WithTimeout(ctx, testTimeout)
		defer cancel()
		_, err := client.RevokeToken(rpcCtx, req)
		return err
	}

	t.Run("revoking an issued token withdraws it", func(t *testing.T) {
		token, err := auth.IssueAccountToken(ctx, st, acct.ID)
		if err != nil {
			t.Fatalf("IssueAccountToken(revokee): %v", err)
		}
		// Sanity: the token resolves before the revoke.
		if _, err := auth.ResolveToken(ctx, st, token, store.SubjectAccount); err != nil {
			t.Fatalf("token must resolve before revoke, got %v", err)
		}
		if err := revoke(adminTok, token); err != nil {
			t.Fatalf("RevokeToken over the door: %v", err)
		}
		if _, err := auth.ResolveToken(ctx, st, token, store.SubjectAccount); !errors.Is(err, auth.ErrTokenRevoked) {
			t.Fatalf("after revoke, ResolveToken = %v, want auth.ErrTokenRevoked", err)
		}

		t.Run("revoking it again is an idempotent success", func(t *testing.T) {
			if err := revoke(adminTok, token); err != nil {
				t.Fatalf("re-revoking an already-revoked token = %v, want success (no-op)", err)
			}
		})
	})

	t.Run("revoking an unknown token is NotFound", func(t *testing.T) {
		err := revoke(adminTok, "never-issued-token")
		if code := connect.CodeOf(err); code != connect.CodeNotFound {
			t.Fatalf("revoking an unknown token = %v, want CodeNotFound", code)
		}
	})
}
