//go:build podman

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/RigelBuild/compass/go/internal/pgshare"
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
	ctx := context.Background() // run-root: TestMain has no test context
	registerAskFixtureOptions()
	registerDuoFixtureOptions()
	registerFanoutFixtureOptions()
	registerListMessagesFixtureOptions()
	registerPeeringFixtureOptions()
	registerRedeliverFixtureOptions()
	registerCommsToolFixtureOptions()
	registerToolsFixtureOptions()
	registerLegFiveFixtureOptions()
	registerForgeFixtureOptions()
	registerLegTwoFixtureOptions()
	registerLegThreeFourFixtureOptions()
	registerSeamProbeFixtureOptions()
	if !podmanUsable(ctx) {
		os.Exit(m.Run())
	}
	if err := requireFreshAgentImage(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "e2e TestMain: %v\n", err)
		os.Exit(1)
	}

	binDir, err := buildStackBinaries(ctx)
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

	stopPG, err := startSuitePostgres(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e TestMain: %v\n", err)
		_ = os.RemoveAll(binDir)
		os.Exit(1)
	}

	code := m.Run()
	// Down the shared fixture before exiting: its children hold this binary's
	// stdout, so leaving them up strands the run in an I/O-incomplete wait.
	// The suite postgres stops after it, since the shared server holds its store.
	shutdownShared(ctx)
	stopPG()
	_ = os.RemoveAll(binDir)
	os.Exit(code)
}

// startSuitePostgres starts the one postgres the shared stack attaches to and
// records its DSN in shared.pgDSN. Own-stack legs keep their own postgres. The
// returned stop SIGTERMs it and removes its dirs.
func startSuitePostgres(ctx context.Context) (func(), error) {
	stateDir, err := os.MkdirTemp("", "compass-e2e-suite-pg-")
	if err != nil {
		return nil, fmt.Errorf("make suite postgres state dir: %w", err)
	}
	// The socket path is sun_path-budgeted, so it lives under a short /tmp root.
	sockDir := filepath.Join("/tmp", "ce"+strconv.Itoa(os.Getpid())+"pg")
	cleanup := func() {
		_ = os.RemoveAll(stateDir)
		_ = os.RemoveAll(sockDir)
	}
	ports, err := freeSharedPorts(ctx, 1)
	if err != nil {
		cleanup()
		return nil, err
	}
	dsn, stop, err := pgshare.StartSuitePostgresMain(ctx, stateDir, sockDir, ports[0])
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("start suite postgres: %w", err)
	}
	shared.pgDSN = dsn
	return func() { stop(); cleanup() }, nil
}

// buildStackBinaries compiles the three stack child binaries from the module
// root into a fresh temp dir and returns it. This package lives at go/e2e, so
// the module root (the dir holding go.mod) is ONE `..` up — verified by the
// go.mod check below, which fails legibly if the layout ever moves. The t-free
// signature lets TestMain call it once per run (there is no *testing.T there);
// the caller owns the returned dir's removal.
func buildStackBinaries(ctx context.Context) (string, error) {
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
		cmd := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(binDir, name), "./cmd/"+name)
		cmd.Dir = moduleRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			_ = os.RemoveAll(binDir)
			return "", fmt.Errorf("build %s: %w\n%s", name, err, out)
		}
	}
	return binDir, nil
}

// requireFreshAgentImage gates the run on agentImageGate with the real tree,
// environment and image stamp.
func requireFreshAgentImage(ctx context.Context) error {
	return agentImageGate(os.Getenv, filepath.Join("..", "..", "packages", "compass-agent"), func() string {
		// A missing stamp file exits non-zero; that image predates stamping, so stale.
		out, _ := exec.CommandContext(ctx, "podman", "run", "--rm", "--entrypoint", "cat", agentImage,
			"/etc/compass-agent/source-fingerprint").Output()
		return string(out)
	})
}
