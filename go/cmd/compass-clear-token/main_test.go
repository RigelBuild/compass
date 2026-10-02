//go:build unix

package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	keyring "github.com/zalando/go-keyring"

	"github.com/RigelBuild/compass/go/internal/tokenstore"
)

func TestRunValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing server", args: []string{"--state-dir", t.TempDir()}, want: "server URL"},
		{name: "missing state", args: []string{"--server-url", "https://example.test"}, want: "state directory"},
		{name: "positional", args: []string{"extra"}, want: "unexpected positional"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("run() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestRunVersion(t *testing.T) {
	if version == "" {
		t.Fatal("version must not be empty, or the assertion below is vacuous")
	}
	output := captureStdout(t, func() error {
		return run([]string{"--version"})
	})
	if strings.TrimSpace(output) != version {
		t.Fatalf("run(--version) output = %q, want %q", output, version)
	}
}

func TestRunNeverLeaksTokenOnReadFailure(t *testing.T) {
	stateDir := t.TempDir()
	keyring.MockInitWithError(errors.New("no secret service bus"))
	store := tokenstore.New(stateDir)
	const token = "known-secret-token"
	if err := store.Write("https://stored.test", token); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateDir, "remote-token")
	if err := os.WriteFile(path, []byte("malformed "+token), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"--server-url", "https://stored.test", "--state-dir", stateDir})
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("run() error = %v, want non-secret read failure", err)
	}
}

func TestRunDeleteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can unlink files from mode-0500 directories")
	}
	stateDir := t.TempDir()
	keyring.MockInitWithError(errors.New("no secret service bus"))
	store := tokenstore.New(stateDir)
	if err := store.Write("https://stored.test", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(stateDir, 0o700); err != nil {
			t.Errorf("restore state directory permissions: %v", err)
		}
	})
	err := run([]string{"--server-url", "https://stored.test", "--state-dir", stateDir})
	if err == nil || !strings.HasPrefix(err.Error(), "delete stored token:") {
		t.Fatalf("run() error = %v, want delete stored token prefix", err)
	}
}

func TestRunDeletesOnlyMatchingURL(t *testing.T) {
	stateDir := t.TempDir()
	keyring.MockInitWithError(errors.New("no secret service bus"))
	store := tokenstore.New(stateDir)
	const wantToken = "secret"
	if err := store.Write("https://stored.test", wantToken); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--server-url", "https://other.test", "--state-dir", stateDir}); err != nil {
		t.Fatalf("mismatched URL: %v", err)
	}
	gotToken, err := store.Read("https://stored.test")
	if err != nil {
		t.Fatalf("read token after mismatch: %v", err)
	}
	if gotToken != wantToken {
		t.Fatalf("token after mismatch = %q, want %q", gotToken, wantToken)
	}
	if err := run([]string{"--server-url", "https://stored.test", "--state-dir", stateDir}); err != nil {
		t.Fatalf("matching URL: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "remote-token")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote-token after match: %v", err)
	}
}

func TestRunAbsentURLIsSuccess(t *testing.T) {
	keyring.MockInitWithError(errors.New("no secret service bus"))
	if err := run([]string{"--server-url", "https://absent.test", "--state-dir", t.TempDir()}); err != nil {
		t.Fatalf("absent URL: %v", err)
	}
}

func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	if err := fn(); err != nil {
		_ = write.Close()
		os.Stdout = original
		t.Fatal(err)
	}
	if err := write.Close(); err != nil {
		os.Stdout = original
		t.Fatal(err)
	}
	os.Stdout = original
	output, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	if err := read.Close(); err != nil {
		t.Fatal(err)
	}
	return string(output)
}
