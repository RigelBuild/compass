//go:build pgtest && unix

package server

// Store-gated RevokeToken round-trip over the production network-door chain via a real
// connect client. Admin-gated; the handler hashes the presented plaintext and marks the
// stored hash revoked. Load-bearing: after a revoke, auth.ResolveToken fails as
// ErrTokenRevoked. Edge codes: unknown → NotFound; already-revoked → idempotent success.

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/store"
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

	t.Run("revoking an empty token is InvalidArgument", func(t *testing.T) {
		err := revoke(adminTok, "")
		if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
			t.Fatalf("revoking an empty token = %v, want CodeInvalidArgument", code)
		}
	})
}
