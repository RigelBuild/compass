//go:build pgtest && unix

package server

// Store-gated boot tests for resolveMasterKey's server_key_state tripwire. Each
// opens its own isolated-schema store (pgtest.RequireDSN + store.Open), SKIPping
// when no runtime/DSN is available. The fail-closed assertions sit AFTER the
// store fixture (A8 ordering): a guard placed before it would fire in a
// container-less sandbox where the suite is defined to skip.
//
// Covered: first-boot writes the tripwire row; a matching-key second boot passes
// and returns the stored version; a mismatched-key boot fails startup before any
// decrypt; and the COMPASS_MASTER_KEY row is constructible and resolves end to
// end (the F1 regression guard that the rename did not leave a never-resolving
// row). The migration CHECK's LIKE-escape (admits COMPASS_MASTER_KEY, rejects a
// COMPASSX_ near-miss) is proven at the store layer in server_secrets_pgtest.

import (
	"bytes"
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/envelope"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/secrets"
	"github.com/RigelBuild/compass/go/internal/store"
)

// keyHexA and keyHexB are two distinct, valid 64-hex-char (32-byte) keys.
const (
	keyHexA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	keyHexB = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

func masterKeyResolver(value string) *fakeResolver {
	return &fakeResolver{resolved: []secrets.ResolvedSecret{{Name: store.MasterKeyName, Value: value}}}
}

func openMasterKeyStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), pgtest.RequireDSN(t))
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func TestResolveMasterKeyFirstBootWritesTripwire(t *testing.T) {
	st := openMasterKeyStore(t)
	ctx := context.Background()

	// No row yet: first boot must write it and return the initial version.
	if _, err := st.ServerKeyState(ctx); err == nil {
		t.Fatal("precondition broken: a tripwire row already exists before first boot")
	}

	_, version, err := resolveMasterKey(ctx, st, masterKeyResolver(keyHexA))
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if version != initialKeyVersion {
		t.Fatalf("first-boot version = %d, want %d", version, initialKeyVersion)
	}

	// The row now exists and its fingerprint verifies against the same key under
	// the stored salt — proving the write persisted the real fingerprint+salt.
	state, err := st.ServerKeyState(ctx)
	if err != nil {
		t.Fatalf("read tripwire after first boot: %v", err)
	}
	raw, _ := hex.DecodeString(keyHexA)
	key, err := envelope.NewKey(raw)
	if err != nil {
		t.Fatalf("build key: %v", err)
	}
	if !bytes.Equal(key.Fingerprint(state.FingerprintSalt), state.KeyFingerprint) {
		t.Fatal("stored fingerprint does not verify against the first-boot key")
	}
}

func TestResolveMasterKeyMatchingBootPasses(t *testing.T) {
	st := openMasterKeyStore(t)
	ctx := context.Background()

	if _, _, err := resolveMasterKey(ctx, st, masterKeyResolver(keyHexA)); err != nil {
		t.Fatalf("first boot: %v", err)
	}
	// A second boot under the SAME key passes and returns the stored version.
	_, version, err := resolveMasterKey(ctx, st, masterKeyResolver(keyHexA))
	if err != nil {
		t.Fatalf("matching second boot: %v", err)
	}
	if version != initialKeyVersion {
		t.Fatalf("matching-boot version = %d, want %d", version, initialKeyVersion)
	}
}

func TestResolveMasterKeyMismatchedBootFails(t *testing.T) {
	st := openMasterKeyStore(t)
	ctx := context.Background()

	if _, _, err := resolveMasterKey(ctx, st, masterKeyResolver(keyHexA)); err != nil {
		t.Fatalf("first boot: %v", err)
	}
	// A second boot under a DIFFERENT key must fail startup before any decrypt.
	_, _, err := resolveMasterKey(ctx, st, masterKeyResolver(keyHexB))
	if err == nil {
		t.Fatal("mismatched-key boot = nil error, want a fail-closed startup error")
	}
	if !strings.Contains(err.Error(), "undecryptable") {
		t.Errorf("mismatch error %q does not explain the undecryptable-data hazard", err)
	}
}

// TestResolveMasterKeyResolvesDeclaredName is the F1 regression guard: the
// COMPASS_ rename does not leave an unconstructible, never-resolving row. A real
// DeclareServerSecret of COMPASS_MASTER_KEY succeeds (the extended CHECK and
// HasServerSecretPrefix admit it), and resolveMasterKey reads it end to end.
func TestResolveMasterKeyResolvesDeclaredName(t *testing.T) {
	st := openMasterKeyStore(t)
	ctx := context.Background()

	// The declare must succeed — this is the row that was unconstructible before
	// the COMPASS_ prefix was added.
	if err := st.DeclareServerSecret(ctx, "", store.MasterKeyName); err != nil {
		t.Fatalf("declare %s: %v", store.MasterKeyName, err)
	}

	if _, _, err := resolveMasterKey(ctx, st, masterKeyResolver(keyHexA)); err != nil {
		t.Fatalf("resolve declared master key: %v", err)
	}
}
