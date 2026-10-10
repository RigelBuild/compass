// Package gatewaycred is the LLM gateway's credential value store: provider
// credentials sealed at rest, a per-row version for compare-and-set writes, and
// the own-before-shared pool every gateway request draws on.
package gatewaycred

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/RigelBuild/compass/go/internal/store"
)

// The sentinels the RPC edge maps to codes. A miss, a disabled row, and a row
// outside the caller's pool are one ErrNotFound so an id cannot be probed.
var (
	ErrInvalidArgument    = errors.New("gatewaycred: invalid argument")
	ErrNotFound           = errors.New("gatewaycred: not found")
	ErrFailedPrecondition = errors.New("gatewaycred: failed precondition")
	ErrVersionConflict    = errors.New("gatewaycred: version conflict")
)

// Scope says where a credential comes from in a pool.
type Scope int16

const (
	// ScopeOwn is a credential one user brought; only that user's agents see it.
	ScopeOwn Scope = 1
	// ScopeShared is the tenant's key, used for a provider only when the
	// caller's owner has no enabled credential of their own for it.
	ScopeShared Scope = 2
)

// OAuthToken is a stored OAuth grant. Its String, GoString, LogValue and
// MarshalJSON all redact, so no formatter or encoder can print Access or Refresh.
type OAuthToken struct {
	Access               string
	Refresh              string
	ExpiresUnixMs        int64
	EnterpriseURL        string
	ProjectID            string
	Email                string
	AccountID            string
	APIEndpoint          string
	OrgID                string
	OrgName              string
	AuthorizedAtUnixMs   int64
	Region               string
	InferenceRegion      string
	ActiveOrganizationID string
}

const redacted = "REDACTED"

func (t OAuthToken) String() string {
	return fmt.Sprintf("gatewaycred.OAuthToken{access: %s, refresh: %s, expires_unix_ms: %d}",
		redacted, redacted, t.ExpiresUnixMs)
}

func (t OAuthToken) GoString() string { return t.String() }

func (t OAuthToken) LogValue() slog.Value { return slog.StringValue(t.String()) }

func (t OAuthToken) MarshalJSON() ([]byte, error) { return json.Marshal(t.String()) }

// Credential is one decrypted pool entry. Its secret value is unexported and
// read through APIKey or OAuth, so reflection-based encoders see no secret.
type Credential struct {
	ID       string
	Provider string
	Scope    Scope
	// OwnerUserID is the owning user for ScopeOwn and empty for ScopeShared.
	OwnerUserID store.AccountID
	Version     int64

	apiKey string
	oauth  *OAuthToken
}

// NewAPIKeyCredential returns c carrying an API key value.
func NewAPIKeyCredential(c Credential, apiKey string) Credential {
	c.apiKey, c.oauth = apiKey, nil
	return c
}

// NewOAuthCredential returns c carrying an OAuth value.
func NewOAuthCredential(c Credential, tok OAuthToken) Credential {
	c.apiKey, c.oauth = "", &tok
	return c
}

// APIKey returns the key and true when c is an API-key credential.
func (c Credential) APIKey() (string, bool) { return c.apiKey, c.oauth == nil && c.apiKey != "" }

// OAuth returns the grant and true when c is an OAuth credential.
func (c Credential) OAuth() (OAuthToken, bool) {
	if c.oauth == nil {
		return OAuthToken{}, false
	}
	return *c.oauth, true
}

func (c Credential) String() string {
	kind := "api_key"
	if c.oauth != nil {
		kind = "oauth"
	}
	return fmt.Sprintf("gatewaycred.Credential{id: %q, provider: %q, scope: %d, version: %d, kind: %s, value: %s}",
		c.ID, c.Provider, c.Scope, c.Version, kind, redacted)
}

func (c Credential) GoString() string { return c.String() }

func (c Credential) LogValue() slog.Value { return slog.StringValue(c.String()) }

func (c Credential) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }

// CredentialStore persists credentials. Every call is scoped to ctx's tenant.
type CredentialStore interface {
	// Create seals and stores a new credential at version 1 and returns it with
	// its server-assigned ID. Enrollment is the only intended caller.
	Create(ctx context.Context, c Credential) (Credential, error)
	// List returns the enabled own credentials of ownerUserID plus the tenant's
	// enabled shared ones, for one provider or every provider when it is empty.
	List(ctx context.Context, ownerUserID store.AccountID, provider string) ([]Credential, error)
	// UpdateOAuth replaces the grant of an OAuth credential in agentAccountID's
	// pool when its version is expectedVersion; an empty field keeps the stored
	// value. It returns the new version.
	UpdateOAuth(ctx context.Context, agentAccountID store.AccountID, id string, tok OAuthToken, expectedVersion int64) (int64, error)
	// Disable removes a credential in agentAccountID's pool from every pool when
	// its version is expectedVersion, recording cause for its owner.
	Disable(ctx context.Context, agentAccountID store.AccountID, id, cause string, expectedVersion int64) error
}

// PoolResolver resolves the credentials a request on behalf of an agent may use.
type PoolResolver interface {
	// Pool returns, per provider, the agent owner's own enabled credentials, or
	// the shared ones when the owner has none. An unknown agent is ErrNotFound.
	Pool(ctx context.Context, agentAccountID store.AccountID) ([]Credential, error)
}
