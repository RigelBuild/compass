//go:build unix

package server

// DB-free unit tests for resolveMasterKey's decode + fail-closed arms — the
// parts that return BEFORE the server_key_state tripwire (which needs a store).
// The tripwire reconcile (first-boot insert, matching-key boot, mismatched-key
// boot fails) is proven in the pgtest lane. The load-bearing security property
// tested here: no error message ever contains any substring of the secret value.

import (
	"context"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/secrets"
	"github.com/RigelBuild/compass/go/internal/store"
)

// resolverWith returns a fakeResolver scripted to hold value under MasterKeyName.
func resolverWith(value string) *fakeResolver {
	return &fakeResolver{resolved: []secrets.ResolvedSecret{
		{Name: store.MasterKeyName, Value: value},
	}}
}

func TestResolveMasterKeyAbsentFailsClosed(t *testing.T) {
	// An empty resolved set: the key is not provisioned. The error must name the
	// key AND the provisioning runbook, and must never generate a key. A nil
	// store is safe: the absent arm returns before any tripwire read.
	_, _, err := resolveMasterKey(context.Background(), nil, &fakeResolver{})
	if err == nil {
		t.Fatal("resolveMasterKey with no provisioned key = nil error, want fail-closed")
	}
	if !strings.Contains(err.Error(), store.MasterKeyName) {
		t.Errorf("error %q does not name %s", err, store.MasterKeyName)
	}
	if !strings.Contains(err.Error(), "openssl rand -hex 32") {
		t.Errorf("error %q does not name the provisioning command", err)
	}
}

func TestResolveMasterKeyEmptyValueFailsClosed(t *testing.T) {
	// A declared-but-empty value is treated as unprovisioned, not as a zero key.
	_, _, err := resolveMasterKey(context.Background(), nil, resolverWith(""))
	if err == nil {
		t.Fatal("resolveMasterKey with an empty value = nil error, want fail-closed")
	}
	if !strings.Contains(err.Error(), "not provisioned") {
		t.Errorf("error %q does not report the key as unprovisioned", err)
	}
}

func TestResolveMasterKeyHexDecodeFailures(t *testing.T) {
	// A 63-char (odd) and a 66-char value are wrong LENGTH; a 64-char non-hex
	// value is wrong ALPHABET. Each must be a startup failure. The secret value
	// is 64 valid hex chars in the wrong-alphabet case: the message must still
	// leak none of it.
	cases := []struct {
		name  string
		value string
	}{
		{"too short", strings.Repeat("a", 63)},
		{"too long", strings.Repeat("a", 66)},
		{"non-hex", strings.Repeat("g", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := resolveMasterKey(context.Background(), nil, resolverWith(tc.value))
			if err == nil {
				t.Fatalf("resolveMasterKey(%q) = nil error, want a decode failure", tc.name)
			}
			if !strings.Contains(err.Error(), store.MasterKeyName) {
				t.Errorf("error %q does not name %s", err, store.MasterKeyName)
			}
		})
	}
}

// TestResolveMasterKeyErrorNeverLeaksValue is the load-bearing security test: no
// error resolveMasterKey returns may contain any substring of the secret value.
// It sweeps every DB-free failure arm with a distinctive, non-degenerate value
// and asserts no window of that value survives into the message.
func TestResolveMasterKeyErrorNeverLeaksValue(t *testing.T) {
	// A value carrying a recognisable marker in each failure class. The wrong-hex
	// value is valid hex chars only (so the marker is hex too) — a naive
	// "echo what we got" would surface it.
	values := []string{
		"deadbeefcafef00dfeedface00c0ffee",                                     // short (32 chars)
		"deadbeefcafef00dfeedface00c0ffeedeadbeefcafef00dfeedface00c0ffee1234", // long (66 chars)
		"deadbeefcafef00dfeedface00c0ffeedeadbeefcafef00dfeedface00c0ffgg",     // 64 chars, non-hex tail
	}
	for _, v := range values {
		_, _, err := resolveMasterKey(context.Background(), nil, resolverWith(v))
		if err == nil {
			t.Fatalf("value %q did not fail", v)
		}
		msg := err.Error()
		// No substring of length >= 8 from the value may appear in the message.
		const window = 8
		for i := 0; i+window <= len(v); i++ {
			if sub := v[i : i+window]; strings.Contains(msg, sub) {
				t.Fatalf("error %q leaks value substring %q", msg, sub)
			}
		}
	}
}
