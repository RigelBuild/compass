package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/RigelBuild/compass/go/internal/store"
)

// ErrGatewayTokenInvalid is the one error Verify returns for an unknown, revoked
// or malformed token, so no caller can tell which.
var ErrGatewayTokenInvalid = errors.New("auth: invalid gateway token")

// TokenMinter mints, verifies and revokes per-agent LLM gateway bearer tokens.
type TokenMinter interface {
	// Mint returns a fresh token for the agent and revokes its previous one.
	Mint(ctx context.Context, agentAccountID string) (token string, err error)
	// Verify resolves a live token to its agent and that agent's owner.
	Verify(ctx context.Context, token string) (store.GatewayCaller, error)
	// Revoke withdraws the agent's live token; it is a no-op when none is live.
	Revoke(ctx context.Context, agentAccountID string) error
}

// GatewayTokens is the store-backed TokenMinter.
type GatewayTokens struct {
	st *store.Store
}

var _ TokenMinter = (*GatewayTokens)(nil)

// NewGatewayTokens returns a TokenMinter over st.
func NewGatewayTokens(st *store.Store) *GatewayTokens {
	return &GatewayTokens{st: st}
}

// Mint runs under ctx's tenant, so an agent outside it is store.ErrNotFound.
// The plaintext leaves only through the return value; the store keeps its hash.
func (g *GatewayTokens) Mint(ctx context.Context, agentAccountID string) (string, error) {
	for range 2 {
		token := mintToken()
		err := g.st.RotateGatewayToken(ctx, store.AccountID(agentAccountID), hashToken(token))
		if err == nil {
			return token, nil
		}
		if errors.Is(err, store.ErrConflict) {
			continue // astronomically rare hash collision: re-mint once, as IssueAccountToken does
		}
		return "", fmt.Errorf("minting gateway token: %w", err)
	}
	return "", errors.New("minting gateway token: hash collision on two successive mints")
}

// Verify looks the token up by its SHA-256, so no stored value is compared
// byte-by-byte against attacker input. A store fault is returned wrapped, never
// folded into ErrGatewayTokenInvalid, so an outage is not reported as a bad token.
func (g *GatewayTokens) Verify(ctx context.Context, token string) (store.GatewayCaller, error) {
	if !wellFormedToken(token) {
		return store.GatewayCaller{}, ErrGatewayTokenInvalid
	}
	caller, err := g.st.ResolveGatewayToken(ctx, hashToken(token))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.GatewayCaller{}, ErrGatewayTokenInvalid
		}
		return store.GatewayCaller{}, fmt.Errorf("verifying gateway token: %w", err)
	}
	return caller, nil
}

// Revoke runs under ctx's tenant, so an agent outside it is store.ErrNotFound.
func (g *GatewayTokens) Revoke(ctx context.Context, agentAccountID string) error {
	if err := g.st.RevokeGatewayToken(ctx, store.AccountID(agentAccountID)); err != nil {
		return fmt.Errorf("revoking gateway token: %w", err)
	}
	return nil
}

// wellFormedToken reports whether token has mintToken's shape, so garbage never
// reaches the store.
func wellFormedToken(token string) bool {
	if len(token) != base64.RawURLEncoding.EncodedLen(tokenBytes) {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil
}
