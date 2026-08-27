//go:build podman

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestMain compiles the three stack child binaries ONCE for the whole
// podman-tagged e2e run and exports them on PATH, rather than rebuilding them on
// every NewFixture. The suite stands up ~11 fixtures per run, each of which
// needs the identical compass-postgres / compass-server / compass-runner
// binaries; building them per fixture meant 33 go-build invocations into 11
// throwaway dirs. One build per run serves every fixture.
//
// It is //go:build podman on purpose: TestMain governs the whole package's test
// lifecycle, and go/e2e also carries a DELIBERATELY UNTAGGED hermetic lane
// (cannedmodel_test.go) that runs under a plain `go test` with no container and
// no stack. Tagging this file podman keeps it out of that build, so the hermetic
// lane keeps Go's default TestMain and never triggers a stack-binary build.
//
// When podman cannot run the real agent image every leg skips via
// podmanUsable(), so there is no stack to build for — run (everything skips) and
// exit without building.
func TestMain(m *testing.M) {
	if !podmanUsable() {
		os.Exit(m.Run())
	}

	binDir, err := buildStackBinaries()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e TestMain: build stack binaries: %v\n", err)
		os.Exit(1)
	}

	// Prepend the shared bin dir so every fixture's ProcessSupervisor resolves
	// each Component by bare name via exec.LookPath against this one build.
	if err := os.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		fmt.Fprintf(os.Stderr, "e2e TestMain: set PATH: %v\n", err)
		_ = os.RemoveAll(binDir)
		os.Exit(1)
	}

	code := m.Run()
	_ = os.RemoveAll(binDir)
	os.Exit(code)
}

// buildStackBinaries compiles the three stack child binaries from the module
// root into a fresh temp dir and returns it. This package lives at go/e2e, so
// the module root (the dir holding go.mod) is ONE `..` up — verified by the
// go.mod check below, which fails legibly if the layout ever moves. The t-free
// signature lets TestMain call it once per run (there is no *testing.T there);
// the caller owns the returned dir's removal.
func buildStackBinaries() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	moduleRoot := filepath.Join(wd, "..") // go/e2e -> go
	if _, err := os.Stat(filepath.Join(moduleRoot, "go.mod")); err != nil {
		return "", fmt.Errorf("module root %q has no go.mod (layout changed?): %w", moduleRoot, err)
	}
	binDir, err := os.MkdirTemp("", "compass-e2e-bin-")
	if err != nil {
		return "", fmt.Errorf("make bin dir: %w", err)
	}
	for _, name := range []string{"compass-postgres", "compass-server", "compass-runner"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, name), "./cmd/"+name)
		cmd.Dir = moduleRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			_ = os.RemoveAll(binDir)
			return "", fmt.Errorf("build %s: %w\n%s", name, err, out)
		}
	}
	return binDir, nil
}
