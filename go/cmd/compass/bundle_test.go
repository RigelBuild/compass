//go:build unix

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sealedsecurity/compass/go/internal/store"
)

// writeBundleDir materializes a map of relative-path -> content under a fresh
// temp dir and returns its root — the operator-pointed bundle root push walks.
func writeBundleDir(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", full, err)
		}
	}
	return root
}

// TestBuildBundleValidDir asserts a well-formed dir produces a tarball whose
// members carry exactly the expected clean relative paths, and that the tar
// obeys the grammar invariants. Note: assertBundleGrammar here asserts a
// PARALLEL copy of the door's invariants (regular-file typeflag, clean relative
// path, whitelisted top dir), not the door itself — it can drift from the real
// door. TestBuildBundleDoorParity is the real door-parity check: it feeds the
// built bytes through store.ValidateConfigBundle.
func TestBuildBundleValidDir(t *testing.T) {
	root := writeBundleDir(t, map[string]string{
		"skills/alpha/SKILL.md":     "# alpha",
		"skills/alpha/ref/notes.md": "notes",
		"extensions/beta/main.go":   "package beta",
		"mcp/gamma.json":            `{"ok":true}`,
	})

	bundle, err := buildBundle(root)
	if err != nil {
		t.Fatalf("buildBundle(valid) = %v, want nil", err)
	}

	names, err := bundleMemberNames(bundle)
	if err != nil {
		t.Fatalf("bundleMemberNames: %v", err)
	}
	want := []string{
		"extensions/beta/main.go",
		"mcp/gamma.json",
		"skills/alpha/SKILL.md",
		"skills/alpha/ref/notes.md",
	}
	if !slices.Equal(names, want) {
		t.Errorf("bundle members = %v, want %v", names, want)
	}

	// Structurally assert every written member is a regular file with a clean,
	// relative, whitelisted path. This is a PARALLEL hand-rolled copy of the
	// door's structural invariants, not the door validator itself; see
	// TestBuildBundleDoorParity for the real door check.
	assertBundleGrammar(t, bundle)
}

// TestBuildBundleRejects covers the client-side grammar rejections: a bad top
// dir, a bad <name>, a non-JSON mcp member, and a symlink. Each must fail at
// build with a message naming the offending member — before any RPC.
func TestBuildBundleRejects(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"bad top dir", map[string]string{"secrets/x/f": "y"}, "not under skills/"},
		{"bad skill name", map[string]string{"skills/bad name/f": "y"}, "must match"},
		{"non-json mcp", map[string]string{"mcp/svc.json": "not json"}, "not valid JSON"},
		{"mcp not json ext", map[string]string{"mcp/svc.txt": "hi"}, "must be a .json file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeBundleDir(t, tc.files)
			_, err := buildBundle(root)
			if err == nil {
				t.Fatalf("buildBundle(%s) = nil error, want rejection", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("buildBundle(%s) error %q does not contain %q", tc.name, err, tc.want)
			}
		})
	}
}

// TestBuildBundleRejectsSymlink asserts a symlink member is a hard error (the
// store door rejects symlink members outright, so they never enter the tar).
func TestBuildBundleRejectsSymlink(t *testing.T) {
	root := writeBundleDir(t, map[string]string{"skills/alpha/SKILL.md": "# alpha"})
	link := filepath.Join(root, "skills", "alpha", "link")
	if err := os.Symlink("SKILL.md", link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	_, err := buildBundle(root)
	if err == nil {
		t.Fatal("buildBundle(dir with symlink) = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("buildBundle(symlink) error %q does not name a symlink", err)
	}
}

// assertBundleGrammar re-reads a built bundle and asserts every member obeys the
// store-door grammar: regular-file typeflag, a relative (non-absolute) clean
// path with no ".." components, and a whitelisted top dir.
func assertBundleGrammar(t *testing.T, bundle []byte) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer func() { _ = gz.Close() }() // read-only decompress; close error not actionable
	tr := tar.NewReader(gz)
	tops := map[string]bool{"skills": true, "extensions": true, "mcp": true}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			t.Errorf("member %q typeflag = %d, want regular file %d", hdr.Name, hdr.Typeflag, tar.TypeReg)
		}
		if strings.HasPrefix(hdr.Name, "/") {
			t.Errorf("member %q is an absolute path", hdr.Name)
		}
		if hdr.Name != path.Clean(hdr.Name) {
			t.Errorf("member %q is not a clean path", hdr.Name)
		}
		parts := strings.Split(hdr.Name, "/")
		for _, p := range parts {
			if p == "." || p == ".." || p == "" {
				t.Errorf("member %q has an illegal path component %q", hdr.Name, p)
			}
		}
		if !tops[parts[0]] {
			t.Errorf("member %q top dir %q is not whitelisted", hdr.Name, parts[0])
		}
	}
}

// TestBuildBundleEmpty asserts a dir with no regular-file members (empty, or
// only empty subdirs under a valid top dir) is a hard error, not a silently
// valid empty bundle that would replace the fleet config — `agent-config
// delete` is the sanctioned clear path.
func TestBuildBundleEmpty(t *testing.T) {
	t.Run("empty dir", func(t *testing.T) {
		root := t.TempDir()
		_, err := buildBundle(root)
		if err == nil {
			t.Fatal("buildBundle(empty dir) = nil error, want rejection")
		}
		if !strings.Contains(err.Error(), "contains no skills/, extensions/, or mcp/ members") {
			t.Errorf("buildBundle(empty) error %q does not name the no-members condition", err)
		}
	})

	t.Run("only empty subdir", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "skills", "alpha"), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		_, err := buildBundle(root)
		if err == nil {
			t.Fatal("buildBundle(only empty subdir) = nil error, want rejection")
		}
		if !strings.Contains(err.Error(), "contains no skills/, extensions/, or mcp/ members") {
			t.Errorf("buildBundle(empty subdir) error %q does not name the no-members condition", err)
		}
	})
}

// TestBundleCaps asserts the client-side caps fire when the file-count or
// content-byte limit is crossed, each naming its cap. The caps mirror the store
// door and hitting 4096 files or 64 MiB on disk in a test is wasteful, so the
// pure cap check is driven directly at its boundary.
func TestBundleCaps(t *testing.T) {
	if err := checkBundleCaps(maxBundleFileCount, 0); err != nil {
		t.Errorf("checkBundleCaps at file limit = %v, want nil", err)
	}
	if err := checkBundleCaps(maxBundleFileCount+1, 0); err == nil {
		t.Error("checkBundleCaps over file limit = nil error, want rejection")
	} else if !strings.Contains(err.Error(), "file limit") {
		t.Errorf("file-cap error %q does not name the file limit", err)
	}
	if err := checkBundleCaps(1, maxBundleContentBytes+1); err == nil {
		t.Error("checkBundleCaps over byte limit = nil error, want rejection")
	} else if !strings.Contains(err.Error(), "content limit") {
		t.Errorf("byte-cap error %q does not name the content limit", err)
	}
}

// TestBuildBundleDoorParity is the REAL door-parity check: it builds a valid
// bundle and feeds the bytes through the exported store door validator
// (store.ValidateConfigBundle, the pure check PutAgentConfig runs), so any drift
// between the builder's output and the door's grammar is caught here.
func TestBuildBundleDoorParity(t *testing.T) {
	root := writeBundleDir(t, map[string]string{
		"skills/alpha/SKILL.md":     "# alpha",
		"skills/alpha/ref/notes.md": "notes",
		"extensions/beta/main.go":   "package beta",
		"mcp/gamma.json":            `{"ok":true}`,
	})
	bundle, err := buildBundle(root)
	if err != nil {
		t.Fatalf("buildBundle(valid) = %v, want nil", err)
	}
	version, err := store.ValidateConfigBundle(bundle)
	if err != nil {
		t.Fatalf("store.ValidateConfigBundle(built bundle) = %v, want nil (builder/door drift)", err)
	}
	if version == "" {
		t.Error("store.ValidateConfigBundle returned an empty version, want a content hash")
	}
}
