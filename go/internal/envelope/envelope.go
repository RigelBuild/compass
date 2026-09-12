// Package envelope is the at-rest crypto seam for user-provided secrets:
// AES-256-GCM under a single master key, with the nonce generated internally
// per encryption so reuse is structurally impossible. The key bytes are held
// unexported so no reflection-based logger or marshaler can reach them.
package envelope

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// keyLen is the AES-256 key size and the only length NewKey accepts.
const keyLen = 32

// nonceLen is the GCM standard 96-bit nonce.
const nonceLen = 12

// ErrDecrypt is the single opaque decrypt failure. It never wraps cipher
// internals and carries no plaintext or key material.
var ErrDecrypt = errors.New("envelope: decrypt failed")

// Key is a 256-bit AES-GCM key. The bytes are unexported so no exported field,
// formatter, or marshaler can render them.
type Key struct {
	k [keyLen]byte
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
	return k, nil
}

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
	block, err := aes.NewCipher(k.k[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// UserSecretAAD builds the canonical user-secret AAD binding a ciphertext to
// its scope tuple, name, tenant, and key generation:
//
//	"compass/user-secret/v1\x00" + tenantID + "\x00" + decimal(scopeKind) +
//	  "\x00" + scopeID + "\x00" + name + "\x00" + decimal(keyVersion)
//
// Every field is bound unconditionally (a tenant-scoped row passes scopeID="")
// so the field count never varies, and the \x00 separators make the encoding
// injective: no two distinct field tuples concatenate to the same bytes.
func UserSecretAAD(tenantID string, scopeKind int16, scopeID, name string, keyVersion int16) []byte {
	const sep = "\x00"
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
	return buf
}
