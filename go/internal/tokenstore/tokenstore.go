// Package tokenstore persists the native-client remote bearer token, keyring
// first with an atomic 0600-file fallback (SEA-1686 T5.2, DL-109). It is keyed
// by the remote server URL so two remotes never collide, and — critically — the
// file fallback stores the {serverURL, token} pair so a re-pointed server_url
// can never replay one remote's bearer to another (the F1 replay guard, OQ-4).
//
// The token is a live credential: it is NEVER logged, mirroring the server's
// admin-token discipline ("Log the path, never the token",
// go/server/network_door.go:258-259).
package tokenstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	keyring "github.com/zalando/go-keyring"
)

// ErrNotFound is returned by Read when no token is stored for the requested URL
// — either nothing is stored at all, or the stored URL does not match.
var ErrNotFound = errors.New("tokenstore: no token for server url")

// keyringService is the OS-keychain service name; the server URL is the key
// (user) within it, so distinct remotes occupy distinct entries.
const keyringService = "compass-app"

// tokenFileName is the fallback file under the caller-supplied state dir.
const tokenFileName = "remote-token"

// Store reads, writes, and deletes the remote bearer token by server URL.
type Store interface {
	// Read returns the stored token for serverURL, or ErrNotFound when none is
	// stored OR the stored URL does not match serverURL.
	Read(serverURL string) (token string, err error)
	// Write persists token for serverURL, replacing any prior entry.
	Write(serverURL, token string) error
	// Delete removes any stored token for serverURL. Absent is not an error.
	Delete(serverURL string) error
}

// New returns a keyring-first store that transparently falls back to an atomic
// 0600 file under stateDir when the OS keyring is unavailable. stateDir is the
// already-resolved state directory (XDG resolution lives in main.go, T5.6); it
// is used only when the keyring backend errors out.
func New(stateDir string) Store {
	return &keyringStore{fallback: &fileStore{dir: stateDir}}
}

// keyringStore prefers the OS keyring and falls back to fallback when the
// keyring backend is unavailable (e.g. no D-Bus Secret Service on Linux).
type keyringStore struct {
	fallback *fileStore
}

// unavailable reports whether err indicates the keyring backend is absent, as
// opposed to a genuine operational failure. go-keyring surfaces a missing
// backend as ErrUnsupportedPlatform or a D-Bus/Secret-Service dial error; only
// keyring.ErrNotFound is a real "no such entry" answer, which is NOT a fallback
// trigger.
func unavailable(err error) bool {
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return false
	}
	// Any non-ErrNotFound error from the keyring means the backend could not
	// service the request (unsupported platform, no Secret Service bus, etc.);
	// fall back to the file so a keyring-less host still works (DL-109).
	return true
}

func (s *keyringStore) Read(serverURL string) (string, error) {
	token, err := keyring.Get(keyringService, serverURL)
	switch {
	case err == nil:
		return token, nil
	case errors.Is(err, keyring.ErrNotFound):
		return "", ErrNotFound
	case unavailable(err):
		return s.fallback.Read(serverURL)
	default:
		return "", fmt.Errorf("tokenstore: reading keyring: %w", err)
	}
}

func (s *keyringStore) Write(serverURL, token string) error {
	err := keyring.Set(keyringService, serverURL, token)
	switch {
	case err == nil:
		return nil
	case unavailable(err):
		return s.fallback.Write(serverURL, token)
	default:
		return fmt.Errorf("tokenstore: writing keyring: %w", err)
	}
}

func (s *keyringStore) Delete(serverURL string) error {
	err := keyring.Delete(keyringService, serverURL)
	switch {
	case err == nil, errors.Is(err, keyring.ErrNotFound):
		return nil
	case unavailable(err):
		return s.fallback.Delete(serverURL)
	default:
		return fmt.Errorf("tokenstore: deleting keyring: %w", err)
	}
}

// tokenPair is the on-disk fallback record. The URL is stored alongside the
// token so Read can reject a mismatch (F1 replay guard, OQ-4).
type tokenPair struct {
	ServerURL string `json:"serverUrl"`
	Token     string `json:"token"`
}

// fileStore is the URL-bound atomic 0600-file fallback.
type fileStore struct {
	dir string
}

func (s *fileStore) Read(serverURL string) (string, error) {
	raw, err := os.ReadFile(s.path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("tokenstore: reading token file: %w", err)
	}
	var pair tokenPair
	if err := json.Unmarshal(raw, &pair); err != nil {
		// Never wrap raw file bytes into the error — they hold the token.
		return "", fmt.Errorf("tokenstore: parsing token file: %w", err)
	}
	// F1 replay guard: a token stored for remote A must never be returned for a
	// request against remote B.
	if pair.ServerURL != serverURL {
		return "", ErrNotFound
	}
	return pair.Token, nil
}

func (s *fileStore) Write(serverURL, token string) error {
	pair, err := json.Marshal(tokenPair{ServerURL: serverURL, Token: token})
	if err != nil {
		// Marshaling two strings cannot fail; surface it without the token.
		return errors.New("tokenstore: marshaling token pair")
	}
	return s.writeAtomic(pair)
}

func (s *fileStore) Delete(serverURL string) error {
	if err := os.Remove(s.path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("tokenstore: deleting token file: %w", err)
	}
	return nil
}

func (s *fileStore) path() string { return filepath.Join(s.dir, tokenFileName) }

// writeAtomic writes data to the token file 0600, atomically: a temp file in the
// same directory (born 0600) is written, synced, and renamed over the final
// path, so a reader never observes a partial credential. Mirrors the server's
// writeTokenFile (go/server/network_door.go:341-383). Error strings carry only
// paths and syscall causes, never the token bytes.
func (s *fileStore) writeAtomic(data []byte) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("tokenstore: ensuring state dir %q: %w", s.dir, err)
	}
	tmp, err := os.CreateTemp(s.dir, tokenFileName+".*")
	if err != nil {
		return fmt.Errorf("tokenstore: creating temp token file in %q: %w", s.dir, err)
	}
	tmpName := tmp.Name()
	// os.CreateTemp already creates the file 0600; the explicit chmod pins it
	// regardless of umask, belt-and-suspenders for a live credential.
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close() // cleanup close; the chmod error is the real failure.
		_ = os.Remove(tmpName)
		return fmt.Errorf("tokenstore: chmod 0600 temp token file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("tokenstore: writing token file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("tokenstore: syncing token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("tokenstore: closing temp token file: %w", err)
	}
	if err := os.Rename(tmpName, s.path()); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("tokenstore: renaming token file into place: %w", err)
	}
	return nil
}
