//go:build pgtest && unix

package auth

import (
	"errors"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// newGatewayAgents returns a minter and two agents with distinct owners.
func newGatewayAgents(t *testing.T) (*GatewayTokens, store.Account, store.Account) {
	t.Helper()
	ctx := t.Context()
	st, _, member := openTestStore(t)
	other, err := st.CreateUser(ctx, store.NewUser{Handle: "other", DisplayName: "other"})
	if err != nil {
		t.Fatalf("CreateUser(other): %v", err)
	}
	a, err := st.CreateAgent(ctx, member, store.NewAgent{Handle: "gw-a", DisplayName: "A"})
	if err != nil {
		t.Fatalf("CreateAgent(a): %v", err)
	}
	b, err := st.CreateAgent(ctx, other.ID, store.NewAgent{Handle: "gw-b", DisplayName: "B"})
	if err != nil {
		t.Fatalf("CreateAgent(b): %v", err)
	}
	return NewGatewayTokens(st), a, b
}

func TestGatewayTokenMintVerifyRevoke(t *testing.T) {
	ctx := t.Context()
	g, a, _ := newGatewayAgents(t)

	token, err := g.Mint(ctx, string(a.ID))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	got, err := g.Verify(ctx, token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.AgentAccountID != a.ID || got.OwnerUserID != a.Agent.OwnerUserID {
		t.Fatalf("Verify = %+v, want agent %q owner %q", got, a.ID, a.Agent.OwnerUserID)
	}
	if err := g.Revoke(ctx, string(a.ID)); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := g.Verify(ctx, token); !errors.Is(err, ErrGatewayTokenInvalid) {
		t.Fatalf("Verify after revoke err = %v, want ErrGatewayTokenInvalid", err)
	}
}

func TestGatewayTokenRotationInvalidatesOld(t *testing.T) {
	ctx := t.Context()
	g, a, _ := newGatewayAgents(t)

	old, err := g.Mint(ctx, string(a.ID))
	if err != nil {
		t.Fatalf("Mint(old): %v", err)
	}
	fresh, err := g.Mint(ctx, string(a.ID))
	if err != nil {
		t.Fatalf("Mint(fresh): %v", err)
	}
	if _, err := g.Verify(ctx, old); !errors.Is(err, ErrGatewayTokenInvalid) {
		t.Fatalf("Verify(old) err = %v, want ErrGatewayTokenInvalid", err)
	}
	if got, err := g.Verify(ctx, fresh); err != nil || got.AgentAccountID != a.ID {
		t.Fatalf("Verify(fresh) = %+v, %v; want agent %q", got, err, a.ID)
	}
}

// Every failure is the same sentinel with the same text, so nothing tells a
// revoked token from an unknown or malformed one.
func TestGatewayTokenFailuresAreIndistinguishable(t *testing.T) {
	ctx := t.Context()
	g, a, _ := newGatewayAgents(t)

	revoked, err := g.Mint(ctx, string(a.ID))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if err := g.Revoke(ctx, string(a.ID)); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	for name, token := range map[string]string{
		"revoked":   revoked,
		"unknown":   mintToken(),
		"malformed": "not a token",
		"empty":     "",
	} {
		_, err := g.Verify(ctx, token)
		if !errors.Is(err, ErrGatewayTokenInvalid) || err.Error() != ErrGatewayTokenInvalid.Error() {
			t.Errorf("%s: Verify err = %v, want bare ErrGatewayTokenInvalid", name, err)
		}
	}
}

// Agent A's token resolves only to A and A's owner, never to B, so the gateway
// can never select B's pool from it.
func TestGatewayTokenIdentityIsolation(t *testing.T) {
	ctx := t.Context()
	g, a, b := newGatewayAgents(t)

	ta, err := g.Mint(ctx, string(a.ID))
	if err != nil {
		t.Fatalf("Mint(a): %v", err)
	}
	tb, err := g.Mint(ctx, string(b.ID))
	if err != nil {
		t.Fatalf("Mint(b): %v", err)
	}
	gotA, err := g.Verify(ctx, ta)
	if err != nil {
		t.Fatalf("Verify(a): %v", err)
	}
	gotB, err := g.Verify(ctx, tb)
	if err != nil {
		t.Fatalf("Verify(b): %v", err)
	}
	if gotA.AgentAccountID != a.ID || gotA.OwnerUserID != a.Agent.OwnerUserID {
		t.Fatalf("token A resolved to %+v", gotA)
	}
	if gotB.AgentAccountID != b.ID || gotB.OwnerUserID != b.Agent.OwnerUserID {
		t.Fatalf("token B resolved to %+v", gotB)
	}
	if gotA.OwnerUserID == gotB.OwnerUserID {
		t.Fatal("fixture: A and B must have distinct owners")
	}
	// Revoking B leaves A live: revocation is per agent.
	if err := g.Revoke(ctx, string(b.ID)); err != nil {
		t.Fatalf("Revoke(b): %v", err)
	}
	if _, err := g.Verify(ctx, ta); err != nil {
		t.Fatalf("Verify(a) after revoking b: %v", err)
	}
}

func TestGatewayTokenMintUnknownAgent(t *testing.T) {
	g, _, _ := newGatewayAgents(t)
	if _, err := g.Mint(t.Context(), "no-such-agent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Mint(unknown) err = %v, want store.ErrNotFound", err)
	}
}
