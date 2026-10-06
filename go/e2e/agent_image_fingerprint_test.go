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
		"moon.yml":                "ignored\n",
		"node_modules/x/index.js": "ignored\n",
		"src/node_modules/y.ts":   "kept\n",
	})
	got, err := agentSourceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	// (cd dir && sha256sum package.json src/cli.ts src/node_modules/y.ts | sha256sum)
	const want = "6bab76d3452b67556d18aecfdb78afc9a867a45933f09cc9109d6a688f3a7e20"
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
