package envelope

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	secret := repeat(0xAB)
	k := mustKey(t, secret)
	needle := fmt.Sprintf("%02x", secret[0]) // "ab"

	// %v / %+v / %#v must not render the key bytes.
	for _, s := range []string{
		fmt.Sprintf("%v", k),
		fmt.Sprintf("%+v", k),
		fmt.Sprintf("%#v", k),
		fmt.Sprintf("%s", k),
	} {
		if bytes.Contains(bytes.ToLower([]byte(s)), []byte(needle+needle)) {
			t.Fatalf("formatted Key leaks key bytes: %q", s)
		}
	}

	j, err := json.Marshal(k) //nolint:staticcheck // marshaling a no-exported-field Key to prove it yields no key bytes IS the test
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if bytes.Contains(bytes.ToLower(j), []byte(needle)) {
		t.Fatalf("json.Marshal(Key) leaks key bytes: %s", j)
	}
	// The bytes must not appear as a base64/array either: marshaling an all-0xAB
	// key should not embed a run of the raw value in any form.
	if bytes.Contains(j, secret) {
		t.Fatalf("json.Marshal(Key) embeds raw key: %s", j)
	}
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

func TestUserSecretAADInjective(t *testing.T) {
	// Adjacent-field ambiguity: without the \x00 separators, moving the "\x00B"
	// from the name into the scopeID boundary would collide.
	a := UserSecretAAD("tenant", 1, "", "A\x00B", 1)
	b := UserSecretAAD("tenant", 1, "B", "A", 1)
	if bytes.Equal(a, b) {
		t.Fatal("UserSecretAAD not injective across name/scopeID field boundary")
	}

	// Numeric run-together: a trailing-digit name plus keyVersion must not
	// concatenate into the same bytes as a shorter name and a longer version.
	c := UserSecretAAD("t", 0, "", "KEY1", 2)
	d := UserSecretAAD("t", 0, "", "KEY", 12)
	if bytes.Equal(c, d) {
		t.Fatal("UserSecretAAD not injective across name/keyVersion digit boundary")
	}

	// scopeKind is bound: same everything else, different scope kind differs.
	e := UserSecretAAD("t", 1, "acct", "N", 1)
	f := UserSecretAAD("t", 2, "acct", "N", 1)
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
	aadUser := UserSecretAAD("tenant-x", 1, "acct-1", "OPENAI_API_KEY", 1)
	aadAgent := UserSecretAAD("tenant-x", 2, "acct-1", "OPENAI_API_KEY", 1)

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
