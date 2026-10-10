//go:build pgtest && unix

package gatewaycred_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RigelBuild/compass/go/internal/envelope"
	"github.com/RigelBuild/compass/go/internal/gatewaycred"
	"github.com/RigelBuild/compass/go/internal/gatewaycred/gatewaycredtest"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
	"github.com/RigelBuild/compass/go/internal/store/db"
)

const testKeyVersion int16 = 1

func TestPostgresGatewayCredentialContract(t *testing.T) {
	gatewaycredtest.Run(t, postgresHarness())
}

func TestGatewayCredentialsCiphertextAtRest(t *testing.T) {
	st, pool, creds, _, owner, ctx := newCredentialFixture(t)
	apiSecret := "api-key-secret-value"
	accessSecret := "oauth-access-secret-value"
	refreshSecret := "oauth-refresh-secret-value"
	api := mustCreate(t, ctx, creds, gatewaycred.NewAPIKeyCredential(gatewaycred.Credential{
		Provider: "anthropic", Scope: gatewaycred.ScopeOwn, OwnerUserID: owner,
	}, apiSecret))
	oauth := mustCreate(t, ctx, creds, gatewaycred.NewOAuthCredential(gatewaycred.Credential{
		Provider: "openai", Scope: gatewaycred.ScopeOwn, OwnerUserID: owner,
	}, gatewaycred.OAuthToken{Access: accessSecret, Refresh: refreshSecret}))

	secrets := []string{apiSecret, accessSecret, refreshSecret}
	for _, row := range []struct {
		id      string
		secrets []string
	}{
		{id: api.ID, secrets: []string{apiSecret}},
		{id: oauth.ID, secrets: []string{accessSecret, refreshSecret}},
	} {
		assertRawCredentialRowHasNoSecrets(t, ctx, pool, row.id, secrets...)
		assertCredentialDecrypts(t, ctx, pool, row.id, st.EffectiveTenant(ctx), row.secrets...)
	}
}

func assertRawCredentialRowHasNoSecrets(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string, secrets ...string) {
	t.Helper()
	var idColumn, tenantColumn, providerColumn, scopeColumn, ownerColumn []byte
	var kindColumn, versionColumn, expiresColumn, ciphertextColumn, nonceColumn []byte
	var keyVersionColumn, disabledAtColumn, disabledCauseColumn, createdColumn, updatedColumn []byte
	err := pool.QueryRow(ctx, `SELECT id::text, tenant_id::text, provider::text,
		scope::text, COALESCE(owner_user_id, '')::text, kind::text, version::text,
		expires_at_unix_ms::text, encode(value_ciphertext, 'escape'),
		encode(value_nonce, 'escape'), key_version::text,
		COALESCE(disabled_at::text, ''), disabled_cause::text,
		created_at::text, updated_at::text
		FROM gateway_credentials WHERE id = $1`, id).Scan(
		&idColumn, &tenantColumn, &providerColumn, &scopeColumn, &ownerColumn,
		&kindColumn, &versionColumn, &expiresColumn, &ciphertextColumn, &nonceColumn,
		&keyVersionColumn, &disabledAtColumn, &disabledCauseColumn, &createdColumn, &updatedColumn,
	)
	if err != nil {
		t.Fatalf("read raw credential row: %v", err)
	}
	for i, column := range [][]byte{
		idColumn, tenantColumn, providerColumn, scopeColumn, ownerColumn, kindColumn,
		versionColumn, expiresColumn, ciphertextColumn, nonceColumn, keyVersionColumn,
		disabledAtColumn, disabledCauseColumn, createdColumn, updatedColumn,
	} {
		for _, secret := range secrets {
			if bytes.Contains(column, []byte(secret)) {
				t.Fatalf("secret appeared in raw credential column %d", i)
			}
		}
	}
}

func assertCredentialDecrypts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string, tenant store.TenantID, secrets ...string) {
	t.Helper()
	var nonce, ciphertext []byte
	if err := pool.QueryRow(ctx, "SELECT value_nonce, value_ciphertext FROM gateway_credentials WHERE id = $1", id).Scan(&nonce, &ciphertext); err != nil {
		t.Fatalf("read encrypted credential: %v", err)
	}
	aad, err := envelope.GatewayCredentialAAD(string(tenant), id, testKeyVersion)
	if err != nil {
		t.Fatalf("credential AAD: %v", err)
	}
	plaintext, err := mustKey(t).Decrypt(nonce, ciphertext, aad)
	if err != nil {
		t.Fatalf("decrypt credential: %v", err)
	}
	for _, secret := range secrets {
		if !bytes.Contains(plaintext, []byte(secret)) {
			t.Fatalf("decrypted credential did not contain expected secret")
		}
	}
}

func TestGatewayCredentialsTenantBindingFailsAuthentication(t *testing.T) {
	_, pool, creds, _, owner, ctxA := newCredentialFixture(t)
	credential := mustCreate(t, ctxA, creds, gatewaycred.NewOAuthCredential(gatewaycred.Credential{
		Provider: "anthropic", Scope: gatewaycred.ScopeShared,
	}, gatewaycred.OAuthToken{Access: "tenant-bound-access"}))
	ctxB := store.WithTenant(t.Context(), store.TenantID("gateway-tenant-b"))
	if _, err := pool.Exec(ctxA, "INSERT INTO tenants (id, slug, display_name, created_at_unix_ms) VALUES ($1, $1, $1, 0)", "gateway-tenant-b"); err != nil {
		t.Fatalf("insert second tenant: %v", err)
	}
	tenantB, ok := store.TenantFromContext(ctxB)
	if !ok {
		t.Fatal("second tenant context has no tenant")
	}
	if _, err := pool.Exec(ctxA, "UPDATE gateway_credentials SET tenant_id = $1 WHERE id = $2", tenantB, credential.ID); err != nil {
		t.Fatalf("move credential tenant: %v", err)
	}
	if _, err := creds.List(ctxB, owner, "anthropic"); !errors.Is(err, envelope.ErrDecrypt) {
		t.Fatalf("List moved credential = %v, want ErrDecrypt", err)
	}
}

func TestGatewayCredentialsRejectWrongKeyAndVersion(t *testing.T) {
	st, _, creds, _, owner, ctx := newCredentialFixture(t)
	mustCreate(t, ctx, creds, gatewaycred.NewOAuthCredential(gatewaycred.Credential{
		Provider: "anthropic", Scope: gatewaycred.ScopeOwn, OwnerUserID: owner,
	}, gatewaycred.OAuthToken{Access: "protected-access"}))

	otherKey, err := envelope.NewKey([]byte("abcdef0123456789abcdef0123456789"))
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	wrongKeyStore := gatewaycred.NewPostgres(st, otherKey, testKeyVersion)
	if _, err := wrongKeyStore.List(ctx, owner, "anthropic"); !errors.Is(err, envelope.ErrDecrypt) {
		t.Fatalf("List with wrong key = %v, want ErrDecrypt", err)
	}

	wrongVersionStore := gatewaycred.NewPostgres(st, mustKey(t), testKeyVersion+1)
	_, err = wrongVersionStore.List(ctx, owner, "anthropic")
	if err == nil || !strings.Contains(err.Error(), "row 1") || !strings.Contains(err.Error(), "configured 2") {
		t.Fatalf("List with wrong key version = %v, want mismatch naming row 1 and configured 2", err)
	}
	if errors.Is(err, envelope.ErrDecrypt) {
		t.Fatalf("key version mismatch must not be ErrDecrypt: %v", err)
	}
}

func TestGatewayCredentialsUpdateVersionPredicate(t *testing.T) {
	st, _, creds, agent, owner, ctx := newCredentialFixture(t)
	credential := mustCreate(t, ctx, creds, gatewaycred.NewOAuthCredential(gatewaycred.Credential{
		Provider: "anthropic", Scope: gatewaycred.ScopeOwn, OwnerUserID: owner,
	}, gatewaycred.OAuthToken{Access: "initial-access"}))
	if _, err := creds.UpdateOAuth(ctx, agent, credential.ID, gatewaycred.OAuthToken{Access: "current-access"}, credential.Version); err != nil {
		t.Fatalf("UpdateOAuth: %v", err)
	}
	if err := st.WithTx(ctx, func(tx pgx.Tx) error {
		affected, err := db.New(tx).UpdateGatewayOAuthCredential(ctx, db.UpdateGatewayOAuthCredentialParams{
			ValueCiphertext: []byte("stale"), ValueNonce: []byte("nonce"), KeyVersion: testKeyVersion,
			ExpiresAtUnixMs: 0, ID: credential.ID, Version: credential.Version,
		})
		if err != nil {
			return err
		}
		if affected != 0 {
			return fmt.Errorf("stale update affected %d rows, want 0", affected)
		}
		return nil
	}); err != nil {
		t.Fatalf("stale SQL update: %v", err)
	}
}

func TestGatewayCredentialsCreateRejectsForeignTenantOwner(t *testing.T) {
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	ctxA := store.WithTenant(t.Context(), st.EffectiveTenant(t.Context()))
	ctxB := seedSecondTenant(t, dsn, 3)
	ownerB, err := st.CreateUser(ctxB, store.NewUser{Handle: "foreign-gateway-owner", DisplayName: "Foreign Owner"})
	if err != nil {
		t.Fatalf("CreateUser tenant B: %v", err)
	}
	creds := gatewaycred.NewPostgres(st, mustKey(t), testKeyVersion)
	_, err = creds.Create(ctxA, gatewaycred.NewAPIKeyCredential(gatewaycred.Credential{
		Provider: "anthropic", Scope: gatewaycred.ScopeOwn, OwnerUserID: ownerB.ID,
	}, "foreign-owner-key"))
	if !errors.Is(err, gatewaycred.ErrInvalidArgument) {
		t.Fatalf("Create with tenant B owner = %v, want ErrInvalidArgument", err)
	}
}

func TestGatewayCredentialsRowSwapFailsAuthentication(t *testing.T) {
	_, pool, creds, _, owner, ctx := newCredentialFixture(t)
	first := mustCreate(t, ctx, creds, gatewaycred.NewOAuthCredential(gatewaycred.Credential{
		Provider: "anthropic", Scope: gatewaycred.ScopeOwn, OwnerUserID: owner,
	}, gatewaycred.OAuthToken{Access: "first-access"}))
	second := mustCreate(t, ctx, creds, gatewaycred.NewOAuthCredential(gatewaycred.Credential{
		Provider: "openai", Scope: gatewaycred.ScopeOwn, OwnerUserID: owner,
	}, gatewaycred.OAuthToken{Access: "second-access"}))
	if _, err := pool.Exec(ctx, `UPDATE gateway_credentials g
		SET value_ciphertext = CASE g.id WHEN $1 THEN b.value_ciphertext ELSE a.value_ciphertext END,
		    value_nonce = CASE g.id WHEN $1 THEN b.value_nonce ELSE a.value_nonce END
		FROM gateway_credentials a, gateway_credentials b
		WHERE g.id IN ($1, $2) AND a.id = $1 AND b.id = $2`, first.ID, second.ID); err != nil {
		t.Fatalf("swap credential ciphertext: %v", err)
	}
	if _, err := creds.List(ctx, owner, "anthropic"); err == nil {
		t.Fatal("List first swapped credential succeeded")
	}
	if _, err := creds.List(ctx, owner, "openai"); err == nil {
		t.Fatal("List second swapped credential succeeded")
	}
}

func TestGatewayCredentialsRefreshWritesFreshNonce(t *testing.T) {
	_, pool, creds, agent, owner, ctx := newCredentialFixture(t)
	credential := mustCreate(t, ctx, creds, gatewaycred.NewOAuthCredential(gatewaycred.Credential{
		Provider: "anthropic", Scope: gatewaycred.ScopeOwn, OwnerUserID: owner,
	}, gatewaycred.OAuthToken{Access: "old-access", Refresh: "old-refresh"}))
	var before []byte
	if err := pool.QueryRow(ctx, "SELECT value_nonce FROM gateway_credentials WHERE id = $1", credential.ID).Scan(&before); err != nil {
		t.Fatalf("read initial nonce: %v", err)
	}
	if _, err := creds.UpdateOAuth(ctx, agent, credential.ID, gatewaycred.OAuthToken{Access: "new-access"}, credential.Version); err != nil {
		t.Fatalf("UpdateOAuth: %v", err)
	}
	var after []byte
	if err := pool.QueryRow(ctx, "SELECT value_nonce FROM gateway_credentials WHERE id = $1", credential.ID).Scan(&after); err != nil {
		t.Fatalf("read refreshed nonce: %v", err)
	}
	if string(before) == string(after) {
		t.Fatal("OAuth refresh reused the nonce")
	}
}

func TestGatewayCredentialsRedactSecrets(t *testing.T) {
	_, pool, creds, _, owner, ctx := newCredentialFixture(t)
	credential := mustCreate(t, ctx, creds, gatewaycred.NewOAuthCredential(gatewaycred.Credential{
		Provider: "anthropic", Scope: gatewaycred.ScopeOwn, OwnerUserID: owner,
	}, gatewaycred.OAuthToken{Access: "redact-access-secret", Refresh: "redact-refresh-secret"}))
	got, err := creds.List(ctx, owner, "anthropic")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	token, ok := got[0].OAuth()
	if !ok {
		t.Fatal("List returned non-OAuth credential")
	}
	for name, value := range map[string]any{
		"credential": got[0],
		"token":      token,
	} {
		for _, format := range []string{"%s", "%v", "%+v", "%#v"} {
			assertRedacted(t, fmt.Sprintf(format, value))
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("MarshalJSON %s: %v", name, err)
		}
		assertRedacted(t, string(encoded))
		var buf strings.Builder
		record := slog.NewRecord(time.Now(), slog.LevelInfo, "credential", 0)
		record.AddAttrs(slog.Any("value", value))
		if err := slog.NewJSONHandler(&buf, nil).Handle(ctx, record); err != nil {
			t.Fatalf("log %s: %v", name, err)
		}
		assertRedacted(t, buf.String())
	}
	if _, err := creds.UpdateOAuth(ctx, store.AccountID("unknown"), credential.ID, gatewaycred.OAuthToken{Access: "next"}, credential.Version); !errors.Is(err, gatewaycred.ErrNotFound) {
		t.Fatalf("not-found guard: %v", err)
	}
	assertRawCredentialRowHasNoSecrets(t, ctx, pool, credential.ID, "redact-access-secret", "redact-refresh-secret")
}

func postgresHarness() gatewaycredtest.Harness {
	var memory atomic.Int64
	var current *store.Store
	var currentDSN string
	var key envelope.Key
	return gatewaycredtest.Harness{
		New: func(t *testing.T) (gatewaycred.CredentialStore, gatewaycred.PoolResolver) {
			t.Helper()
			dsn := pgtest.RequireDSN(t)
			st, err := store.Open(t.Context(), dsn)
			if err != nil {
				t.Fatalf("store Open: %v", err)
			}
			t.Cleanup(st.Close)
			current, currentDSN = st, dsn
			key = mustKey(t)
			creds := gatewaycred.NewPostgres(st, key, testKeyVersion)
			return creds, creds
		},
		Ctx: func(t *testing.T, tenant int) context.Context {
			t.Helper()
			if tenant == 0 {
				return store.WithTenant(t.Context(), current.EffectiveTenant(t.Context()))
			}
			return seedSecondTenant(t, currentDSN, tenant)
		},
		Agent: func(t *testing.T, ctx context.Context, owner string) (store.AccountID, store.AccountID) {
			t.Helper()
			id := memory.Add(1)
			user, err := current.CreateUser(ctx, store.NewUser{Handle: fmt.Sprintf("gc-user-%d", id), DisplayName: owner})
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			agent, err := current.CreateAgent(ctx, user.ID, store.NewAgent{Handle: fmt.Sprintf("gc-agent-%d", id), DisplayName: owner})
			if err != nil {
				t.Fatalf("CreateAgent: %v", err)
			}
			return agent.ID, user.ID
		},
	}
}

func newCredentialFixture(t *testing.T) (*store.Store, *pgxpool.Pool, gatewaycred.CredentialStore, store.AccountID, store.AccountID, context.Context) {
	t.Helper()
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(t.Context(), dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pgxpool New: %v", err)
	}
	t.Cleanup(pool.Close)
	ctx := store.WithTenant(t.Context(), st.EffectiveTenant(t.Context()))
	user, err := st.CreateUser(ctx, store.NewUser{Handle: "credential-owner", DisplayName: "owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, user.ID, store.NewAgent{Handle: "credential-agent", DisplayName: "agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	key := mustKey(t)
	creds := gatewaycred.NewPostgres(st, key, testKeyVersion)
	return st, pool, creds, agent.ID, user.ID, ctx
}

func seedSecondTenant(t *testing.T, dsn string, tenant int) context.Context {
	t.Helper()
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool New: %v", err)
	}
	t.Cleanup(pool.Close)
	tenantID := store.TenantID(fmt.Sprintf("gateway-test-tenant-%d", tenant))
	if _, err := pool.Exec(ctx, "INSERT INTO tenants (id, slug, display_name, created_at_unix_ms) VALUES ($1, $1, $1, 0)", tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	return store.WithTenant(ctx, tenantID)
}

func mustCreate(t *testing.T, ctx context.Context, s gatewaycred.CredentialStore, credential gatewaycred.Credential) gatewaycred.Credential {
	t.Helper()
	created, err := s.Create(ctx, credential)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

func mustKey(t *testing.T) envelope.Key {
	t.Helper()
	key, err := envelope.NewKey([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	return key
}

func assertRedacted(t *testing.T, value string) {
	t.Helper()
	for _, secret := range []string{"redact-access-secret", "redact-refresh-secret"} {
		if strings.Contains(value, secret) {
			t.Fatalf("output leaked OAuth value")
		}
	}
}
