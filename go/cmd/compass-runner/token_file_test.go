//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/runner"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

func TestRunRejectsBothTokenSources(t *testing.T) {
	_, err := resolveTokenSource("static-token", "/var/run/token", nil)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("resolveTokenSource with both sources = %v, want mutual-exclusion error", err)
	}
}

func TestCheckMountsAgainstTokenFile(t *testing.T) {
	root := t.TempDir()
	tokenDir := filepath.Join(root, "projected")
	if err := os.Mkdir(tokenDir, 0o700); err != nil {
		t.Fatalf("create token directory: %v", err)
	}
	nestedDir := filepath.Join(tokenDir, "nested")
	if err := os.Mkdir(nestedDir, 0o700); err != nil {
		t.Fatalf("create nested token directory: %v", err)
	}
	unrelated := t.TempDir()
	tokenFile := filepath.Join(tokenDir, "token")

	tests := []struct {
		name    string
		host    string
		wantErr bool
	}{
		{name: "over token directory", host: tokenDir, wantErr: true},
		{name: "inside token directory", host: nestedDir, wantErr: true},
		{name: "ancestor of token directory", host: root, wantErr: true},
		{name: "unrelated mount", host: unrelated},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkMountsAgainstTokenFile([]runtime.Mount{{HostPath: tc.host}}, tokenFile)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkMountsAgainstTokenFile(%q) error = %v, wantErr %t", tc.host, err, tc.wantErr)
			}
		})
	}
}

func TestCheckMountsAgainstTokenFileResolvesSymlinks(t *testing.T) {
	root := t.TempDir()
	tokenDir := filepath.Join(root, "projected")
	if err := os.Mkdir(tokenDir, 0o700); err != nil {
		t.Fatalf("create token directory: %v", err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(tokenDir, alias); err != nil {
		t.Fatalf("symlink token directory: %v", err)
	}

	err := checkMountsAgainstTokenFile(
		[]runtime.Mount{{HostPath: filepath.Join(alias, "nested")}},
		filepath.Join(tokenDir, "token"),
	)
	if err == nil {
		t.Fatal("mount through token-directory symlink was accepted")
	}
}

func TestCheckMountsAgainstTokenFileResolvesSymlinkBeforeParentTraversal(t *testing.T) {
	root := t.TempDir()
	tokenDir := filepath.Join(root, "projected")
	nested := filepath.Join(tokenDir, "nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatalf("create nested token directory: %v", err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(nested, alias); err != nil {
		t.Fatalf("symlink nested token directory: %v", err)
	}
	mountPath := alias + string(filepath.Separator) + ".." + string(filepath.Separator) + "token"
	err := checkMountsAgainstTokenFile(
		[]runtime.Mount{{HostPath: mountPath}},
		filepath.Join(tokenDir, "token"),
	)
	if err == nil {
		t.Fatal("mount through symlink and parent traversal into token directory was accepted")
	}
}

func TestResolveTokenFileFlagWinsOverEnvironment(t *testing.T) {
	t.Setenv("COMPASS_RUNNER_TOKEN", "")
	source, err := resolveTokenSource("", "/flag/token", nil)
	if err != nil {
		t.Fatalf("resolveTokenSource with token-file flag = %v, want nil", err)
	}
	if _, ok := source.(*runner.FileToken); !ok {
		t.Fatalf("resolveTokenSource returned %T, want *runner.FileToken", source)
	}
}
