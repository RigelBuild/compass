package envelope

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
)

func mustKey(t *testing.T, raw []byte) Key {
	t.Helper()
	k, err := NewKey(raw)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	return k
}

func repeat(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestNewKeyRejectsWrongLength(t *testing.T) {
	for _, n := range []int{0, 1, 16, 31, 33, 64} {
		if _, err := NewKey(make([]byte, n)); err == nil {
			t.Fatalf("NewKey(%d bytes): want error, got nil", n)
		} else if !strings.Contains(err.Error(), "32") {
			t.Fatalf("NewKey(%d bytes): error should name the 32-byte requirement, got %q", n, err.Error())
		}
	}
	if _, err := NewKey(repeat(0x01)); err != nil {
		t.Fatalf("NewKey(32 bytes): unexpected error %v", err)
	}
}

func TestNewKeyCopiesInput(t *testing.T) {
	raw := repeat(0x07)
	k := mustKey(t, raw)
	aad := []byte("aad")
	nonce, ct, err := k.Encrypt([]byte("hello"), aad)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// Caller zeroes its slice after NewKey; the Key must be unaffected.
	for i := range raw {
		raw[i] = 0
	}
	pt, err := k.Decrypt(nonce, ct, aad)
	if err != nil {
		t.Fatalf("Decrypt after caller zeroed slice: %v", err)
	}
	if string(pt) != "hello" {
		t.Fatalf("round-trip mismatch: %q", pt)
	}
}

func TestRoundTrip(t *testing.T) {
	k := mustKey(t, repeat(0x02))
	aad := []byte("row-identity")
	msg := []byte("super secret value")
	nonce, ct, err := k.Encrypt(msg, aad)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	pt, err := k.Decrypt(nonce, ct, aad)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(pt, msg) {
		t.Fatalf("round-trip mismatch: %q", pt)
	}
}

func TestEncryptFreshNonce(t *testing.T) {
	k := mustKey(t, repeat(0x03))
	aad := []byte("aad")
	msg := []byte("identical")
	n1, c1, err := k.Encrypt(msg, aad)
	if err != nil {
		t.Fatalf("Encrypt 1: %v", err)
	}
	n2, c2, err := k.Encrypt(msg, aad)
	if err != nil {
		t.Fatalf("Encrypt 2: %v", err)
	}
	if len(n1) != 12 {
		t.Fatalf("nonce length: want 12, got %d", len(n1))
	}
	if bytes.Equal(n1, n2) {
		t.Fatal("two encrypts produced the same nonce")
	}
	if bytes.Equal(c1, c2) {
		t.Fatal("two encrypts produced the same ciphertext")
	}
}

func TestDecryptTamperCiphertext(t *testing.T) {
	k := mustKey(t, repeat(0x04))
	aad := []byte("aad")
	nonce, ct, _ := k.Encrypt([]byte("value"), aad)
	ct[0] ^= 0xff
	if _, err := k.Decrypt(nonce, ct, aad); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("tampered ciphertext: want ErrDecrypt, got %v", err)
	}
}

func TestDecryptTamperNonce(t *testing.T) {
	k := mustKey(t, repeat(0x05))
	aad := []byte("aad")
	nonce, ct, _ := k.Encrypt([]byte("value"), aad)
	nonce[0] ^= 0xff
	if _, err := k.Decrypt(nonce, ct, aad); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("tampered nonce: want ErrDecrypt, got %v", err)
	}
}

func TestDecryptTamperAAD(t *testing.T) {
	k := mustKey(t, repeat(0x06))
	nonce, ct, _ := k.Encrypt([]byte("value"), []byte("aad-A"))
	if _, err := k.Decrypt(nonce, ct, []byte("aad-B")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong aad: want ErrDecrypt, got %v", err)
	}
}

func TestDecryptWrongKey(t *testing.T) {
	k1 := mustKey(t, repeat(0x08))
	k2 := mustKey(t, repeat(0x09))
	aad := []byte("aad")
	nonce, ct, _ := k1.Encrypt([]byte("value"), aad)
	if _, err := k2.Decrypt(nonce, ct, aad); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong key: want ErrDecrypt, got %v", err)
	}
}

func TestDecryptWrongLengthNonce(t *testing.T) {
	k := mustKey(t, repeat(0x0a))
	aad := []byte("aad")
	_, ct, _ := k.Encrypt([]byte("value"), aad)
	if _, err := k.Decrypt(make([]byte, 8), ct, aad); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong-length nonce: want ErrDecrypt, got %v", err)
	}
}

func TestKeyDoesNotLeakBytes(t *testing.T) {
	const b = 0xAB
	secret := repeat(b)
	k := mustKey(t, secret)

	// Needles built from the ACTUAL renderings, not a guessed hex string:
	// fmt emits a byte as decimal for %v/%+v and as 0xNN for %#v. A run of two
	// catches the [32]byte array without matching incidental single occurrences.
	dec := strconv.Itoa(b)    // "171"
	decRun := dec + " " + dec // "171 171"
	hexRun := "0xab, 0xab"    // %#v array element form
	rawNeedles := [][]byte{[]byte(decRun), []byte(hexRun), secret}

	assertClean := func(label, out string) {
		low := bytes.ToLower([]byte(out))
		for _, n := range rawNeedles {
			if bytes.Contains(low, bytes.ToLower(n)) {
				t.Fatalf("%s leaks key bytes (needle %q): %q", label, n, out)
			}
		}
	}

	assertClean("%v", fmt.Sprintf("%v", k))
	assertClean("%+v", fmt.Sprintf("%+v", k))
	assertClean("%#v", fmt.Sprintf("%#v", k))
	assertClean("%s", k.String())

	j, err := json.Marshal(k) //nolint:staticcheck // marshaling a no-exported-field Key to prove it yields no key bytes IS the test
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	assertClean("json.Marshal", string(j))

	// slog is the vector the type must close: both handlers, key as an attr value.
	var textBuf, jsonBuf bytes.Buffer
	slog.New(slog.NewTextHandler(&textBuf, nil)).Info("m", "master_key", k)
	slog.New(slog.NewJSONHandler(&jsonBuf, nil)).Info("m", "master_key", k)
	assertClean("slog TextHandler", textBuf.String())
	assertClean("slog JSONHandler", jsonBuf.String())
}

func TestFingerprintStableAndDistinct(t *testing.T) {
	k1 := mustKey(t, repeat(0x11))
	k2 := mustKey(t, repeat(0x22))
	saltA := []byte("salt-A")
	saltB := []byte("salt-B")

	// Stable for one (key, salt).
	if !bytes.Equal(k1.Fingerprint(saltA), k1.Fingerprint(saltA)) {
		t.Fatal("Fingerprint not stable for one (key, salt)")
	}
	// Differs across salts for one key.
	if bytes.Equal(k1.Fingerprint(saltA), k1.Fingerprint(saltB)) {
		t.Fatal("Fingerprint identical across different salts")
	}
	// Differs across keys for one salt.
	if bytes.Equal(k1.Fingerprint(saltA), k2.Fingerprint(saltA)) {
		t.Fatal("Fingerprint identical across different keys")
	}
	// SHA-256 width.
	if got := len(k1.Fingerprint(saltA)); got != 32 {
		t.Fatalf("Fingerprint length: want 32, got %d", got)
	}
}

// mustAAD builds an AAD the encoding must accept; a NUL-free tuple never errors.
func mustAAD(t *testing.T, tenantID string, scopeKind int16, scopeID, name string, keyVersion int16) []byte {
	t.Helper()
	aad, err := UserSecretAAD(tenantID, scopeKind, scopeID, name, keyVersion)
	if err != nil {
		t.Fatalf("UserSecretAAD(%q,%d,%q,%q,%d): %v", tenantID, scopeKind, scopeID, name, keyVersion, err)
	}
	return aad
}

func TestUserSecretAADInjective(t *testing.T) {
	// Adjacent-field ambiguity: without the \x00 separators, moving the "B"
	// from the name into the scopeID boundary would collide.
	a := mustAAD(t, "tenant", 1, "", "AB", 1)
	b := mustAAD(t, "tenant", 1, "B", "A", 1)
	if bytes.Equal(a, b) {
		t.Fatal("UserSecretAAD not injective across name/scopeID field boundary")
	}

	// Numeric run-together: a trailing-digit name plus keyVersion must not
	// concatenate into the same bytes as a shorter name and a longer version.
	c := mustAAD(t, "t", 0, "", "KEY1", 2)
	d := mustAAD(t, "t", 0, "", "KEY", 12)
	if bytes.Equal(c, d) {
		t.Fatal("UserSecretAAD not injective across name/keyVersion digit boundary")
	}

	// scopeKind is bound: same everything else, different scope kind differs.
	e := mustAAD(t, "t", 1, "acct", "N", 1)
	f := mustAAD(t, "t", 2, "acct", "N", 1)
	if bytes.Equal(e, f) {
		t.Fatal("UserSecretAAD does not bind scopeKind")
	}

	// Exact canonical byte string.
	want := []byte("compass/user-secret/v1\x00t\x001\x00acct\x00N\x001")
	if !bytes.Equal(e, want) {
		t.Fatalf("canonical AAD mismatch:\n got %q\nwant %q", e, want)
	}
}

func TestScopeBinding(t *testing.T) {
	k := mustKey(t, repeat(0x33))
	// User scope shadows tenant scope for the same name/tenant. An AAD that
	// differs in ONLY the scope field must fail to decrypt.
	aadUser := mustAAD(t, "tenant-x", 1, "acct-1", "OPENAI_API_KEY", 1)
	aadAgent := mustAAD(t, "tenant-x", 2, "acct-1", "OPENAI_API_KEY", 1)

	nonce, ct, err := k.Encrypt([]byte("sk-live"), aadUser)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := k.Decrypt(nonce, ct, aadAgent); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("decrypt across scope field: want ErrDecrypt, got %v", err)
	}
	// Same AAD still round-trips.
	if pt, err := k.Decrypt(nonce, ct, aadUser); err != nil || string(pt) != "sk-live" {
		t.Fatalf("same-AAD round-trip failed: pt=%q err=%v", pt, err)
	}
}

func TestZeroValueKeyFailsClosed(t *testing.T) {
	var zero Key // never through NewKey: 32 zero bytes would be a publicly-known key
	aad := []byte("aad")

	if _, _, err := zero.Encrypt([]byte("secret"), aad); !errors.Is(err, ErrUnsetKey) {
		t.Fatalf("zero-value Encrypt: want ErrUnsetKey, got %v", err)
	}
	// An unset key is a wiring bug, not a tamper: it must NOT masquerade as ErrDecrypt.
	if _, err := zero.Decrypt(make([]byte, nonceLen), []byte("ct"), aad); !errors.Is(err, ErrUnsetKey) {
		t.Fatalf("zero-value Decrypt: want ErrUnsetKey, got %v", err)
	}
	if errors.Is(ErrUnsetKey, ErrDecrypt) {
		t.Fatal("ErrUnsetKey must be distinct from ErrDecrypt")
	}

	// A NewKey-built key still works end to end.
	k := mustKey(t, repeat(0x5A))
	nonce, ct, err := k.Encrypt([]byte("secret"), aad)
	if err != nil {
		t.Fatalf("NewKey Encrypt: %v", err)
	}
	if pt, err := k.Decrypt(nonce, ct, aad); err != nil || string(pt) != "secret" {
		t.Fatalf("NewKey round-trip: pt=%q err=%v", pt, err)
	}
}

func TestUserSecretAADRejectsNUL(t *testing.T) {
	// The \x00 separator makes an in-field \x00 a boundary shifter, so the
	// boundary must refuse it rather than emit a colliding AAD.
	for _, tc := range []struct {
		field                   string
		tenantID, scopeID, name string
	}{
		{"tenantID", "t\x00x", "s", "N"},
		{"scopeID", "t", "s\x00y", "N"},
		{"name", "t", "s", "N\x00M"},
	} {
		aad, err := UserSecretAAD(tc.tenantID, 1, tc.scopeID, tc.name, 1)
		if aad != nil {
			t.Errorf("%s: want nil AAD on NUL, got %q", tc.field, aad)
		}
		if !errors.Is(err, ErrAADField) {
			t.Fatalf("%s: want ErrAADField, got %v", tc.field, err)
		}
		if !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: error must name the field, got %q", tc.field, err)
		}
		// The rejected value carries attacker-controlled bytes: it must never
		// appear in the error string.
		for _, v := range []string{tc.tenantID, tc.scopeID, tc.name} {
			if strings.Contains(v, "\x00") && strings.Contains(err.Error(), v) {
				t.Errorf("%s: error leaked the offending value %q", tc.field, err)
			}
		}
	}

	// The exact reproduced collision pair is now refused rather than equal:
	// a NUL in name vs. a NUL in scopeID both error instead of colliding.
	if _, err := UserSecretAAD("t", 1, "a", "b\x00c", 1); !errors.Is(err, ErrAADField) {
		t.Fatalf("collision pair (name NUL): want ErrAADField, got %v", err)
	}
	if _, err := UserSecretAAD("t", 1, "a\x00b", "c", 1); !errors.Is(err, ErrAADField) {
		t.Fatalf("collision pair (scopeID NUL): want ErrAADField, got %v", err)
	}

	// The accepted (NUL-free) domain stays injective: distinct tuples differ.
	c := mustAAD(t, "tenant", 1, "a1b2c3", "OPENAI_API_KEY", 1)
	d := mustAAD(t, "tenant", 1, "a1b2", "c3OPENAI_API_KEY", 1)
	if bytes.Equal(c, d) {
		t.Fatal("distinct NUL-free tuples must not collide")
	}
}
