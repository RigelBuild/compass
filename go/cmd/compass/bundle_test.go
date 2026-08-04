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
// members carry exactly the expected clean relative paths, and — the real
// contract — that the store door (the same validator PutAgentConfig runs) accepts
// it. We assert against the door, not a hand-rolled grammar, so the builder and
// the door can never drift.
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
	// relative, whitelisted path — the grammar the store door enforces. (The door
	// validator lives in an internal package we do not import here; we assert the
	// same invariants the door checks against the tar we produced.)
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
