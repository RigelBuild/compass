package e2e

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The expected value is sha256sum-format lines piped through sha256sum, the
// contract agent-image/source-fingerprint.nix implements.
func TestAgentSourceFingerprintMatchesNixContract(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"package.json":            "{}\n",
		"src/cli.ts":              "export {};\n",
		"src/empty.ts":            "",
		"moon.yml":                "ignored\n",
		"node_modules/x/index.js": "ignored\n",
		"src/node_modules/y.ts":   "kept\n",
	})
	got, err := agentSourceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	// (cd dir && sha256sum package.json src/cli.ts src/empty.ts src/node_modules/y.ts | sha256sum)
	const want = "fb720ef4d881b8725f4194510d6274dfd3279047ca921cd0bdc9574c1df40751"
	if got != want {
		t.Fatalf("fingerprint = %s, want %s", got, want)
	}
}

func TestAgentSourceFingerprintTracksContent(t *testing.T) {
	a := writeTree(t, map[string]string{"src/cli.ts": "one\n"})
	b := writeTree(t, map[string]string{"src/cli.ts": "two\n"})
	fa, errA := agentSourceFingerprint(a)
	fb, errB := agentSourceFingerprint(b)
	if errA != nil || errB != nil || fa == fb {
		t.Fatalf("fingerprints = %s, %s (%v, %v); want two distinct values", fa, fb, errA, errB)
	}
}

func TestCheckAgentImageFresh(t *testing.T) {
	if err := checkAgentImageFresh("abc\n", "abc"); err != nil {
		t.Fatalf("matching stamp: %v", err)
	}
	for _, stamped := range []string{"def", ""} {
		err := checkAgentImageFresh(stamped, "abc")
		if !errors.Is(err, errStaleAgentImage) || !strings.Contains(err.Error(), "podman load") {
			t.Fatalf("stamp %q: err = %v, want errStaleAgentImage naming the rebuild", stamped, err)
		}
	}
}

// lib.fileset hashes a symlink by its target's content, so the Go walk must too.
func TestAgentSourceFingerprintFollowsSymlinks(t *testing.T) {
	plain := writeTree(t, map[string]string{"src/a.ts": "body\n", "src/b.ts": "body\n"})
	linked := writeTree(t, map[string]string{"src/a.ts": "body\n"})
	if err := os.Symlink("a.ts", filepath.Join(linked, "src", "b.ts")); err != nil {
		t.Fatal(err)
	}
	fp, errP := agentSourceFingerprint(plain)
	fl, errL := agentSourceFingerprint(linked)
	if errP != nil || errL != nil || fp != fl {
		t.Fatalf("plain %s (%v) vs symlinked %s (%v); want equal", fp, errP, fl, errL)
	}

	dangling := writeTree(t, map[string]string{"src/a.ts": "body\n"})
	if err := os.Symlink("missing.ts", filepath.Join(dangling, "src", "b.ts")); err != nil {
		t.Fatal(err)
	}
	if _, err := agentSourceFingerprint(dangling); err == nil {
		t.Fatal("dangling symlink: err = nil, want an error")
	}
}

func TestAgentSourceFingerprintExcludesTopLevelNodeModulesSymlink(t *testing.T) {
	base := writeTree(t, map[string]string{"src/a.ts": "body\n"})
	withLink := writeTree(t, map[string]string{"src/a.ts": "body\n"})
	if err := os.Symlink("src", filepath.Join(withLink, "node_modules")); err != nil {
		t.Fatal(err)
	}
	fb, errB := agentSourceFingerprint(base)
	fw, errW := agentSourceFingerprint(withLink)
	if errB != nil || errW != nil || fb != fw {
		t.Fatalf("base %s (%v) vs node_modules symlink %s (%v); want equal", fb, errB, fw, errW)
	}
}

func TestAgentImageGateHonorsOnlyThePublishedOptOut(t *testing.T) {
	dir := writeTree(t, map[string]string{"src/cli.ts": "body\n"})
	tree, err := agentSourceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	env := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}
	stale := func() string { return "0000" }

	// A CI run on a tree-built seed sets GITHUB_ACTIONS but not the opt-out.
	err = agentImageGate(env(map[string]string{"GITHUB_ACTIONS": "true"}), dir, stale)
	if !errors.Is(err, errStaleAgentImage) {
		t.Fatalf("tree-built seed with a stale stamp: err = %v, want errStaleAgentImage", err)
	}
	if err := agentImageGate(env(nil), dir, func() string { return tree + "\n" }); err != nil {
		t.Fatalf("matching stamp: %v", err)
	}
	read := false
	optOut := env(map[string]string{"COMPASS_E2E_ALLOW_PUBLISHED_AGENT_IMAGE": "1"})
	if err := agentImageGate(optOut, dir, func() string { read = true; return "0000" }); err != nil || read {
		t.Fatalf("published opt-out: err = %v, stamp read = %v; want nil and no read", err, read)
	}
}
