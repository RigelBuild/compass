// Package envelope is the at-rest crypto seam for user-provided secrets:
// AES-256-GCM under a single master key, with the nonce generated internally
// per encryption so reuse is structurally impossible. The key bytes are held
// unexported so encoding/json and external packages cannot reach them; fmt and
// slog read unexported fields reflectively, so Key.String/GoString/LogValue are
// what stop those two vectors.
package envelope

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
)

// keyLen is the AES-256 key size and the only length NewKey accepts.
const keyLen = 32

// nonceLen is the GCM standard 96-bit nonce.
const nonceLen = 12

// ErrDecrypt is the single opaque decrypt failure. It never wraps cipher
// internals and carries no plaintext or key material.
var ErrDecrypt = errors.New("envelope: decrypt failed")

// ErrUnsetKey is returned when a zero-value Key is used to encrypt or decrypt.
// It is deliberately NOT ErrDecrypt: a zero Key is a wiring bug (a Key that
// skipped NewKey), not a tamper or wrong-key failure, and conflating it with
// ErrDecrypt would hide the misuse behind the opaque decrypt path.
var ErrUnsetKey = errors.New("envelope: key is unset (must be built with NewKey)")

// Key is a 256-bit AES-GCM key. The bytes are unexported so encoding/json and
// external packages cannot reach them; String/GoString/LogValue close the fmt
// and slog reflection vectors. set is true only for a NewKey-built Key, so the
// zero value fails closed instead of acting as an all-zero (publicly known) key.
type Key struct {
	k   [keyLen]byte
	set bool
}

// NewKey copies raw (which must be exactly 32 bytes) into a Key. The copy lets
// the caller zero its own slice afterward. A wrong length is an operator-config
// error, so the message names the requirement rather than staying opaque.
func NewKey(raw []byte) (Key, error) {
	if len(raw) != keyLen {
		return Key{}, fmt.Errorf("envelope: key must be exactly %d bytes, got %d", keyLen, len(raw))
	}
	var k Key
	copy(k.k[:], raw)
	k.set = true
	return k, nil
}

// String redacts the key for fmt %v/%s and any Stringer consumer.
func (k Key) String() string { return "envelope.Key(REDACTED)" }

// GoString redacts the key for fmt %#v.
func (k Key) GoString() string { return "envelope.Key(REDACTED)" }

// LogValue redacts the key for slog. This is the vector slog actually honors:
// without a LogValuer, a JSONHandler renders the struct reflectively.
func (k Key) LogValue() slog.Value { return slog.StringValue("REDACTED") }

// Fingerprint returns the salted SHA-256 digest of the key under salt — the
// non-secret server_key_state tripwire value.
func (k Key) Fingerprint(salt []byte) []byte {
	h := sha256.New()
	h.Write(salt)
	h.Write(k.k[:])
	return h.Sum(nil)
}

// Encrypt seals plaintext under aad with a fresh random 96-bit nonce and
// returns (nonce, ciphertext). There is no nonce parameter, so reuse cannot
// happen by construction.
func (k Key) Encrypt(plaintext, aad []byte) (nonce, ciphertext []byte, err error) {
	gcm, err := k.gcm()
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, nonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("envelope: nonce generation: %w", err)
	}
	ciphertext = gcm.Seal(nil, nonce, plaintext, aad)
	return nonce, ciphertext, nil
}

// Decrypt opens ciphertext under nonce and aad. Any tamper — ciphertext,
// nonce, aad, wrong key, or a malformed nonce — returns ErrDecrypt with no
// cipher internals attached.
func (k Key) Decrypt(nonce, ciphertext, aad []byte) ([]byte, error) {
	gcm, err := k.gcm()
	if err != nil {
		// An unset key is a wiring bug, not a decrypt failure: surface it plainly
		// rather than folding it into the opaque ErrDecrypt path.
		if errors.Is(err, ErrUnsetKey) {
			return nil, err
		}
		return nil, ErrDecrypt
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, ErrDecrypt
	}
	pt, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

func (k Key) gcm() (cipher.AEAD, error) {
	if !k.set {
		return nil, ErrUnsetKey
	}
	block, err := aes.NewCipher(k.k[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// ErrAADField is returned by UserSecretAAD when a bound field contains a \x00.
// A \x00 is the field separator, so an in-field one shifts a boundary and makes
// the encoding non-injective. The error names the field only — a rejected value
// may carry attacker-controlled bytes and must never reach a log or error string.
var ErrAADField = errors.New("envelope: AAD field contains NUL")

// UserSecretAAD builds the canonical user-secret AAD binding a ciphertext to
// its scope tuple, name, tenant, and key generation:
//
//	"compass/user-secret/v1\x00" + tenantID + "\x00" + decimal(scopeKind) +
//	  "\x00" + scopeID + "\x00" + name + "\x00" + decimal(keyVersion)
//
// Every field is bound unconditionally (a tenant-scoped row passes scopeID="")
// so the field count never varies. The \x00 separators make the encoding
// injective over its accepted domain; that domain — no \x00 in tenantID,
// scopeID, or name — is enforced here rather than assumed from caller grammar.
// The name grammar itself lives in secrets.ValidateName; this boundary only
// rejects the separator byte that would break injectivity.
func UserSecretAAD(tenantID string, scopeKind int16, scopeID, name string, keyVersion int16) ([]byte, error) {
	const sep = "\x00"
	for _, f := range []struct {
		name, value string
	}{{"tenantID", tenantID}, {"scopeID", scopeID}, {"name", name}} {
		if strings.IndexByte(f.value, 0) >= 0 {
			return nil, fmt.Errorf("%w: field %s", ErrAADField, f.name)
		}
	}
	buf := make([]byte, 0, len("compass/user-secret/v1")+len(tenantID)+len(scopeID)+len(name)+16)
	buf = append(buf, "compass/user-secret/v1"...)
	buf = append(buf, sep...)
	buf = append(buf, tenantID...)
	buf = append(buf, sep...)
	buf = append(buf, strconv.FormatInt(int64(scopeKind), 10)...)
	buf = append(buf, sep...)
	buf = append(buf, scopeID...)
	buf = append(buf, sep...)
	buf = append(buf, name...)
	buf = append(buf, sep...)
	buf = append(buf, strconv.FormatInt(int64(keyVersion), 10)...)
	return buf, nil
}
