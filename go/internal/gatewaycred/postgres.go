package gatewaycred

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RigelBuild/compass/go/internal/envelope"
	"github.com/RigelBuild/compass/go/internal/store"
	"github.com/RigelBuild/compass/go/internal/store/db"
)

// Postgres is the encrypted, tenant-scoped gateway credential store.
type Postgres struct {
	st         *store.Store
	key        envelope.Key
	keyVersion int16
}

var (
	_ CredentialStore = (*Postgres)(nil)
	_ PoolResolver    = (*Postgres)(nil)
)

// NewPostgres returns a credential store backed by st and sealed with key.
func NewPostgres(st *store.Store, key envelope.Key, keyVersion int16) *Postgres {
	return &Postgres{st: st, key: key, keyVersion: keyVersion}
}

// Create seals and stores a new credential at version 1.
func (p *Postgres) Create(ctx context.Context, c Credential) (Credential, error) {
	if err := validateCredential(c); err != nil {
		return Credential{}, err
	}
	id, err := credentialID()
	if err != nil {
		return Credential{}, fmt.Errorf("gatewaycred: create id: %w", err)
	}
	c.ID = id
	c.Version = 1
	nonce, ciphertext, err := p.seal(ctx, c)
	if err != nil {
		return Credential{}, fmt.Errorf("gatewaycred: seal credential: %w", err)
	}
	params := db.InsertGatewayCredentialParams{
		ID: id, Provider: c.Provider, Scope: int16(c.Scope),
		OwnerUserID: pgtype.Text{String: string(c.OwnerUserID), Valid: c.OwnerUserID != ""},
		Kind:        credentialKind(c), Version: 1, ExpiresAtUnixMs: credentialExpires(c),
		ValueCiphertext: ciphertext, ValueNonce: nonce, KeyVersion: p.keyVersion,
	}
	if err := p.st.WithTx(ctx, func(tx pgx.Tx) error {
		q := db.New(tx)
		if c.Scope == ScopeOwn {
			exists, err := q.GatewayCredentialOwnerExists(ctx, string(c.OwnerUserID))
			if err != nil {
				return fmt.Errorf("gatewaycred: check owner: %w", err)
			}
			if !exists {
				return fmt.Errorf("%w: owner is not in the current tenant", ErrInvalidArgument)
			}
		}
		return q.InsertGatewayCredential(ctx, params)
	}); err != nil {
		return Credential{}, fmt.Errorf("gatewaycred: create credential: %w", err)
	}
	return cloneCredential(c), nil
}

// List returns enabled own and shared credentials, optionally filtered by provider.
func (p *Postgres) List(ctx context.Context, owner store.AccountID, provider string) ([]Credential, error) {
	var credentials []Credential
	err := p.st.WithTx(ctx, func(tx pgx.Tx) error {
		dbq := db.New(tx)
		rows, err := dbq.ListGatewayCredentials(ctx, db.ListGatewayCredentialsParams{
			OwnerUserID: pgtype.Text{String: string(owner), Valid: true},
			Provider:    provider,
		})
		if err != nil {
			return fmt.Errorf("gatewaycred: list credentials: %w", err)
		}
		credentials, err = openCredentialRows(p.keyVersion, p.st.EffectiveTenant(ctx), p.key, rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return credentials, nil
}

// UpdateOAuth replaces an OAuth grant in the agent's active pool.
func (p *Postgres) UpdateOAuth(ctx context.Context, agent store.AccountID, id string, tok OAuthToken, expected int64) (int64, error) {
	if id == "" || agent == "" || expected == 0 || tok.Access == "" {
		return 0, fmt.Errorf("%w: id, agent, expected version, and access token are required", ErrInvalidArgument)
	}
	var updated int64
	err := p.st.WithTx(ctx, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.GetGatewayCredentialForUpdate(ctx, id)
		if err != nil {
			if storeNoRows(err) {
				return ErrNotFound
			}
			return fmt.Errorf("gatewaycred: lock credential: %w", err)
		}
		owner, err := credentialAgentOwner(ctx, q, agent)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return ErrNotFound
			}
			return err
		}
		if row.DisabledAt.Valid {
			return ErrNotFound
		}
		inPool, err := credentialInPool(ctx, q, row.ID, row.Provider, owner)
		if err != nil {
			return err
		}
		if !inPool {
			return ErrNotFound
		}
		if row.Kind != credentialKindOAuth {
			return ErrFailedPrecondition
		}
		if row.Version != expected {
			return ErrVersionConflict
		}
		old, err := openCredentialRow(p.keyVersion, p.st.EffectiveTenant(ctx), p.key, db.GatewayCredential{
			ID: row.ID, Provider: row.Provider, Scope: row.Scope, OwnerUserID: row.OwnerUserID,
			Kind: row.Kind, Version: row.Version,
			ValueCiphertext: row.ValueCiphertext, ValueNonce: row.ValueNonce, KeyVersion: row.KeyVersion,
		})
		if err != nil {
			return err
		}

		oldToken, ok := old.OAuth()
		if !ok {
			return ErrFailedPrecondition
		}
		updatedCredential := NewOAuthCredential(old, mergeOAuth(oldToken, tok))
		nonce, ciphertext, err := p.seal(ctx, updatedCredential)
		if err != nil {
			return fmt.Errorf("gatewaycred: seal updated credential: %w", err)
		}
		affected, err := q.UpdateGatewayOAuthCredential(ctx, db.UpdateGatewayOAuthCredentialParams{
			ValueCiphertext: ciphertext,
			ValueNonce:      nonce,
			KeyVersion:      p.keyVersion,
			ExpiresAtUnixMs: updatedCredential.oauth.ExpiresUnixMs,
			ID:              id,
			Version:         expected,
		})
		if err != nil {
			return fmt.Errorf("gatewaycred: update credential: %w", err)
		}
		if affected == 0 {
			return ErrVersionConflict
		}
		updated = expected + 1
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("gatewaycred: update OAuth credential: %w", err)
	}
	return updated, nil
}

// Disable removes a credential from every pool and records its cause.
func (p *Postgres) Disable(ctx context.Context, agent store.AccountID, id, cause string, expected int64) error {
	if id == "" || agent == "" || expected == 0 {
		return fmt.Errorf("%w: id, agent, and expected version are required", ErrInvalidArgument)
	}
	err := p.st.WithTx(ctx, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.GetGatewayCredentialForUpdate(ctx, id)
		if err != nil {
			if storeNoRows(err) {
				return ErrNotFound
			}
			return fmt.Errorf("gatewaycred: lock credential: %w", err)
		}
		owner, err := credentialAgentOwner(ctx, q, agent)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return ErrNotFound
			}
			return err
		}
		if row.DisabledAt.Valid {
			return ErrNotFound
		}
		inPool, err := credentialInPool(ctx, q, row.ID, row.Provider, owner)
		if err != nil {
			return err
		}
		if !inPool {
			return ErrNotFound
		}
		if row.Version != expected {
			return ErrVersionConflict
		}
		affected, err := q.DisableGatewayCredential(ctx, db.DisableGatewayCredentialParams{
			DisabledAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
			DisabledCause: cause,
			ID:            id,
			Version:       expected,
		})
		if err != nil {
			return fmt.Errorf("gatewaycred: disable credential: %w", err)
		}
		if affected == 0 {
			return ErrVersionConflict
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("gatewaycred: disable credential: %w", err)
	}
	return nil
}

// Pool resolves own-before-shared credentials for each provider.
func (p *Postgres) Pool(ctx context.Context, agent store.AccountID) ([]Credential, error) {
	if agent == "" {
		return nil, ErrNotFound
	}
	owner, err := p.st.AgentOwner(ctx, agent)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("gatewaycred: resolve agent owner: %w", err)
	}
	var credentials []Credential
	err = p.st.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := db.New(tx).ListGatewayCredentials(ctx, db.ListGatewayCredentialsParams{
			OwnerUserID: pgtype.Text{String: string(owner), Valid: true},
			PoolOnly:    true,
		})
		if err != nil {
			return fmt.Errorf("gatewaycred: list credential pool: %w", err)
		}
		credentials, err = openCredentialRows(p.keyVersion, p.st.EffectiveTenant(ctx), p.key, rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return credentials, nil
}

func credentialInPool(ctx context.Context, q *db.Queries, id, provider string, owner store.AccountID) (bool, error) {
	rows, err := q.ListGatewayCredentials(ctx, db.ListGatewayCredentialsParams{
		OwnerUserID: pgtype.Text{String: string(owner), Valid: true},
		Provider:    provider,
		PoolOnly:    true,
	})
	if err != nil {
		return false, fmt.Errorf("gatewaycred: resolve credential pool: %w", err)
	}
	for _, row := range rows {
		if row.GatewayCredential.ID == id {
			return true, nil
		}
	}
	return false, nil
}

func credentialAgentOwner(ctx context.Context, q *db.Queries, agent store.AccountID) (store.AccountID, error) {
	owner, err := q.GetAgentOwner(ctx, string(agent))
	if err != nil {
		if storeNoRows(err) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("gatewaycred: resolve agent owner: %w", err)
	}
	return store.AccountID(owner), nil
}

func openCredentialRows(keyVersion int16, tenant store.TenantID, key envelope.Key, rows []db.ListGatewayCredentialsRow) ([]Credential, error) {
	out := make([]Credential, 0, len(rows))
	for _, row := range rows {
		credential, err := openCredentialRow(keyVersion, tenant, key, row.GatewayCredential)
		if err != nil {
			return nil, err
		}
		out = append(out, credential)
	}
	return out, nil
}

func openCredentialRow(keyVersion int16, tenant store.TenantID, key envelope.Key, row db.GatewayCredential) (Credential, error) {
	if row.KeyVersion != keyVersion {
		return Credential{}, fmt.Errorf("gatewaycred: key version mismatch: row %d, configured %d", row.KeyVersion, keyVersion)
	}
	aad, err := envelope.GatewayCredentialAAD(string(tenant), row.ID, row.KeyVersion)
	if err != nil {
		return Credential{}, fmt.Errorf("gatewaycred: credential AAD: %w", err)
	}
	plaintext, err := key.Decrypt(row.ValueNonce, row.ValueCiphertext, aad)
	if err != nil {
		return Credential{}, fmt.Errorf("gatewaycred: decrypt credential: %w", err)
	}
	var value sealedCredentialValue
	if err := json.Unmarshal(plaintext, &value); err != nil {
		return Credential{}, fmt.Errorf("gatewaycred: decode credential: %w", err)
	}
	credential := Credential{ID: row.ID, Provider: row.Provider, Scope: Scope(row.Scope), Version: row.Version}
	if row.OwnerUserID.Valid {
		credential.OwnerUserID = store.AccountID(row.OwnerUserID.String)
	}
	switch row.Kind {
	case credentialKindAPIKey:
		if value.APIKey == "" || value.OAuth != nil {
			return Credential{}, errors.New("gatewaycred: invalid sealed credential value")
		}
		credential = NewAPIKeyCredential(credential, value.APIKey)
	case credentialKindOAuth:
		if value.OAuth == nil || value.APIKey != "" {
			return Credential{}, errors.New("gatewaycred: invalid sealed credential value")
		}
		credential = NewOAuthCredential(credential, value.OAuth.token())
	default:
		return Credential{}, errors.New("gatewaycred: invalid credential kind")
	}
	return credential, nil
}

func (p *Postgres) seal(ctx context.Context, credential Credential) ([]byte, []byte, error) {
	var value sealedCredentialValue
	if apiKey, ok := credential.APIKey(); ok {
		value.APIKey = apiKey
	} else if token, ok := credential.OAuth(); ok {
		value.OAuth = sealedOAuthTokenFrom(token)
	} else {
		return nil, nil, ErrInvalidArgument
	}
	//nolint:gosec // G117: the marshaled value is sealed by envelope before it is stored
	plaintext, err := json.Marshal(value)
	if err != nil {
		return nil, nil, fmt.Errorf("gatewaycred: encode credential: %w", err)
	}
	aad, err := envelope.GatewayCredentialAAD(string(p.st.EffectiveTenant(ctx)), credential.ID, p.keyVersion)
	if err != nil {
		return nil, nil, fmt.Errorf("gatewaycred: credential AAD: %w", err)
	}
	nonce, ciphertext, err := p.key.Encrypt(plaintext, aad)
	if err != nil {
		return nil, nil, fmt.Errorf("gatewaycred: encrypt credential: %w", err)
	}
	return nonce, ciphertext, nil
}

func credentialExpires(c Credential) int64 {
	if c.oauth == nil {
		return 0
	}
	return c.oauth.ExpiresUnixMs
}

func credentialKind(c Credential) int16 {
	if c.oauth != nil {
		return credentialKindOAuth
	}
	return credentialKindAPIKey
}

func credentialID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("gatewaycred: random id generation failed")
	}
	return hex.EncodeToString(b[:]), nil
}

func storeNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

const (
	credentialKindAPIKey int16 = 1
	credentialKindOAuth  int16 = 2
)

type sealedCredentialValue struct {
	APIKey string            `json:"api_key,omitempty"`
	OAuth  *sealedOAuthToken `json:"oauth,omitempty"`
}

type sealedOAuthToken struct {
	Access               string `json:"access"`
	Refresh              string `json:"refresh"`
	ExpiresUnixMs        int64  `json:"expires_unix_ms"`
	EnterpriseURL        string `json:"enterprise_url"`
	ProjectID            string `json:"project_id"`
	Email                string `json:"email"`
	AccountID            string `json:"account_id"`
	APIEndpoint          string `json:"api_endpoint"`
	OrgID                string `json:"org_id"`
	OrgName              string `json:"org_name"`
	AuthorizedAtUnixMs   int64  `json:"authorized_at_unix_ms"`
	Region               string `json:"region"`
	InferenceRegion      string `json:"inference_region"`
	ActiveOrganizationID string `json:"active_organization_id"`
}

func sealedOAuthTokenFrom(t OAuthToken) *sealedOAuthToken {
	return (*sealedOAuthToken)(&t)
}

func (t sealedOAuthToken) token() OAuthToken {
	return OAuthToken(t)
}
