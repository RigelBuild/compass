package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/forgecred"
)

func TestGitCredential(t *testing.T) {
	home := t.TempDir()
	writeToken(t, home, "owner-a", "token-a")
	writeToken(t, home, "owner-b", "token-b")
	if err := os.WriteFile(forgecred.DefaultOwnerPath(home, "github.com"), []byte("owner-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		input   string
		wantOut string
		wantErr string
	}{
		{name: "owner token", input: "protocol=https\nhost=github.com\npath=owner-b/repo.git\n\n", wantOut: "username=x-access-token\npassword=token-b\n\n"},
		{name: "default owner falls through", input: "protocol=https\nhost=github.com\npath=owner-a/repo\n\n"},
		{name: "no path falls through", input: "protocol=https\nhost=github.com\n\n"},
		{name: "refuses other owner", input: "protocol=https\nhost=github.com\npath=owner-c/repo\n\n", wantOut: "quit=1\n\n", wantErr: "no credential for owner owner-c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			err := gitCredential(home, []string{"get"}, strings.NewReader(tt.input), &out, &stderr)
			if err != nil {
				t.Fatalf("gitCredential(): %v", err)
			}
			if out.String() != tt.wantOut {
				t.Fatalf("stdout = %q, want %q", out.String(), tt.wantOut)
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want to contain %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

func TestGitCredentialWithoutOwnerMarkerFallsThrough(t *testing.T) {
	var out, stderr bytes.Buffer
	err := gitCredential(t.TempDir(), []string{"get"}, strings.NewReader("protocol=https\nhost=github.com\npath=owner-z/repo\n\n"), &out, &stderr)
	if err != nil || out.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("marker-free Git lookup = (%q, %q, %v), want empty fallthrough", out.String(), stderr.String(), err)
	}
}

func TestGitCredentialStoreAndEraseAreNoOps(t *testing.T) {
	for _, action := range []string{"store", "erase"} {
		t.Run(action, func(t *testing.T) {
			var out, stderr bytes.Buffer
			if err := gitCredential(t.TempDir(), []string{action}, strings.NewReader("password=secret\n\n"), &out, &stderr); err != nil {
				t.Fatalf("gitCredential(%s): %v", action, err)
			}
			if out.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("store/erase output = (%q, %q), want empty", out.String(), stderr.String())
			}
		})
	}
}

func TestToken(t *testing.T) {
	home := t.TempDir()
	writeToken(t, home, "owner-a", "token-a")
	writeToken(t, home, "owner-b", "token-b")
	if err := os.WriteFile(forgecred.DefaultOwnerPath(home, "github.com"), []byte("owner-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		args      []string
		wantOut   string
		wantErr   string
		wantError bool
	}{
		{name: "owner token", args: []string{"--host", "github.com", "--repo", "owner-b/repo"}, wantOut: "token-b"},
		{name: "default owner token", args: []string{"--host", "github.com", "--repo", "owner-a/repo"}, wantOut: "token-a"},
		{name: "no repo returns default owner token", args: []string{"--host", "github.com"}, wantOut: "token-a"},
		{name: "refuses other owner", args: []string{"--host", "github.com", "--repo", "owner-c/repo"}, wantErr: "no credential for owner owner-c", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			err := token(home, tt.args, &out, &stderr)
			if (err != nil) != tt.wantError {
				t.Fatalf("token() error = %v, wantError %t", err, tt.wantError)
			}
			gotErr := stderr.String()
			if err != nil {
				gotErr += err.Error()
			}
			if out.String() != tt.wantOut || !strings.Contains(gotErr, tt.wantErr) {
				t.Fatalf("token() output = (%q, %q), want (%q, containing %q)", out.String(), gotErr, tt.wantOut, tt.wantErr)
			}
		})
	}
}

func TestTokenWithRepoWithoutOwnerMarkerFallsThrough(t *testing.T) {
	installFakeGH(t)
	var out, stderr bytes.Buffer
	err := token(t.TempDir(), []string{"--host", "github.com", "--repo", "owner-z/repo"}, &out, &stderr)
	if err != nil || out.String() != "gh-fallback\n" || stderr.Len() != 0 {
		t.Fatalf("marker-free token lookup = (%q, %q, %v), want gh fallback", out.String(), stderr.String(), err)
	}
}

func TestTokenDefaultWithoutOwnerFileExecsGH(t *testing.T) {
	home := t.TempDir()
	installFakeGH(t)
	var out, stderr bytes.Buffer
	if err := token(home, []string{"--host", "github.com"}, &out, &stderr); err != nil {
		t.Fatalf("token() fallback: %v", err)
	}
	if out.String() != "gh-fallback\n" || stderr.Len() != 0 {
		t.Fatalf("fallback output = (%q, %q), want gh output and empty stderr", out.String(), stderr.String())
	}
}

func installFakeGH(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	gh := filepath.Join(binDir, "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\nprintf 'gh-fallback\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
}

func writeToken(t *testing.T, home, owner, value string) {
	t.Helper()
	path := forgecred.TokenPath(home, "github.com", owner)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}
