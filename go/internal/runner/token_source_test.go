//go:build unix

package runner

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileTokenReReadsFileEveryCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	writeToken := func(token string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("  "+token+"\n"), 0o600); err != nil {
			t.Fatalf("write token file: %v", err)
		}
	}
	writeToken("first")
	source := NewFileToken(path, discardLoggerRunner(), time.Now)

	first, err := source.Token()
	if err != nil {
		t.Fatalf("first Token = %v, want nil", err)
	}
	if first != "first" {
		t.Fatalf("first Token = %q, want first", first)
	}

	writeToken("second")
	second, err := source.Token()
	if err != nil {
		t.Fatalf("second Token = %v, want nil", err)
	}
	if second != "second" {
		t.Fatalf("second Token = %q, want the rewritten token", second)
	}
}

func TestFileTokenReadErrorUsesCachedTokenUntilExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	now := time.Unix(1_800_000_000, 0)
	cached := tokenWithExpiry(t, now.Add(time.Minute))
	if err := os.WriteFile(path, []byte(cached), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	source := NewFileToken(path, discardLoggerRunner(), func() time.Time { return now })
	if got, err := source.Token(); err != nil || got != cached {
		t.Fatalf("initial Token = (%q, %v), want cached token and nil", got, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove token file: %v", err)
	}

	got, err := source.Token()
	if err != nil || got != cached {
		t.Fatalf("Token after read error = (%q, %v), want cached token and nil", got, err)
	}

	now = now.Add(time.Minute)
	got, err = source.Token()
	if err == nil || got != "" {
		t.Fatalf("Token after cached expiry = (%q, %v), want an error", got, err)
	}
}

func TestFileTokenEmptyFileUsesCachedToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	now := time.Unix(1_800_000_000, 0)
	cached := tokenWithExpiry(t, now.Add(time.Minute))
	if err := os.WriteFile(path, []byte(cached), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	source := NewFileToken(path, discardLoggerRunner(), func() time.Time { return now })
	if _, err := source.Token(); err != nil {
		t.Fatalf("initial Token = %v, want nil", err)
	}
	if err := os.WriteFile(path, []byte(" \n\t"), 0o600); err != nil {
		t.Fatalf("empty token file: %v", err)
	}

	got, err := source.Token()
	if err != nil || got != cached {
		t.Fatalf("Token after empty file = (%q, %v), want cached token and nil", got, err)
	}
}

func TestFileTokenRejectsExpiredTokenInFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	now := time.Unix(1_800_000_000, 0)
	if err := os.WriteFile(path, []byte(tokenWithExpiry(t, now)), 0o600); err != nil {
		t.Fatalf("write expired token: %v", err)
	}
	source := NewFileToken(path, discardLoggerRunner(), func() time.Time { return now })
	got, err := source.Token()
	if err == nil || got != "" {
		t.Fatalf("Token with expired file value = (%q, %v), want an error", got, err)
	}
}

func tokenWithExpiry(t *testing.T, expiry time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"exp": expiry.Unix()})
	if err != nil {
		t.Fatalf("marshal token claims: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}
