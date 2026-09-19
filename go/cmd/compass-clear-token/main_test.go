//go:build unix

package main

import (
	"errors"
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
			if err == nil || !contains(err.Error(), tt.want) {
				t.Fatalf("run() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestRunVersion(t *testing.T) {
	if err := run([]string{"--version"}); err != nil {
		t.Fatalf("run(--version): %v", err)
	}
}

func TestRunDeletesOnlyMatchingURL(t *testing.T) {
	stateDir := t.TempDir()
	keyring.MockInitWithError(errors.New("no secret service bus"))
	store := tokenstore.New(stateDir)
	if err := store.Write("https://stored.test", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--server-url", "https://other.test", "--state-dir", stateDir}); err != nil {
		t.Fatalf("mismatched URL: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "remote-token")); err != nil {
		t.Fatalf("remote-token after mismatch: %v", err)
	}
	if err := run([]string{"--server-url", "https://stored.test", "--state-dir", stateDir}); err != nil {
		t.Fatalf("matching URL: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "remote-token")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote-token after match: %v", err)
	}
}

func TestRunAbsentURLIsSuccess(t *testing.T) {
	if err := run([]string{"--server-url", "https://absent.test", "--state-dir", t.TempDir()}); err != nil {
		t.Fatalf("absent URL: %v", err)
	}
}

func contains(value, substring string) bool {
	return strings.Contains(value, substring)
}
