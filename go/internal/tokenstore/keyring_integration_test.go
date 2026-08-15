//go:build keyring_integration

// This suite exercises the real OS keyring backend (Linux Secret Service via
// D-Bus, macOS Keychain) and is build-tagged so the default CI suite runs only
// the file fallback. Run it with a live backend:
//
//	go test -tags keyring_integration ./internal/tokenstore/...
package tokenstore

import (
	"errors"
	"testing"

	keyring "github.com/zalando/go-keyring"
)

func TestKeyringRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	const url = "https://keyring-remote.example:8443"
	const token = "keyring-bearer-token"
	t.Cleanup(func() {
		if err := s.Delete(url); err != nil {
			t.Errorf("cleanup Delete: %v", err)
		}
	})

	if err := s.Write(url, token); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := s.Read(url)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != token {
		t.Fatalf("Read = %q, want %q", got, token)
	}
	// The value must actually land in the keyring, not the file fallback.
	direct, err := keyring.Get(keyringService, url)
	if err != nil {
		t.Fatalf("keyring.Get: %v", err)
	}
	if direct != token {
		t.Fatalf("keyring.Get = %q, want %q", direct, token)
	}

	if err := s.Delete(url); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Read(url); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read after Delete err = %v, want ErrNotFound", err)
	}
}
