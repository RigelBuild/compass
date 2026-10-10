//go:build podman

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/stack"
)

// TestBundledPortsFollowConfig brings up two stacks side by side, each bundling
// NATS and the collector on its own freePorts host ports. Two stacks on the
// fixed defaults would collide on the second publish, so both answering at once
// proves the host side follows Config.
func TestBundledPortsFollowConfig(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman not usable in this environment")
	}
	ctx := context.Background() // test root context (rule://go-thread-context exemption)

	binDir := buildBinariesFromModuleRoot(t)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	a := upBundledStack(ctx, t, shortRoot(t, "-bpa"))
	b := upBundledStack(ctx, t, shortRoot(t, "-bpb"))

	for _, s := range []bundledStack{a, b} {
		monitor := "127.0.0.1:" + strconv.Itoa(s.cfg.NatsMonitorPort)
		waitNatsHealthy(t, "http://"+monitor+"/healthz", natsHealthyBudget)
		health := "127.0.0.1:" + strconv.Itoa(s.cfg.CollectorHealthPort)
		waitCollectorHealthy(t, "http://"+health+"/", collectorHealthyBudget)
	}
	// Each stack's server dials its own NATS: the published client ports differ.
	if a.cfg.NatsClientPort == b.cfg.NatsClientPort {
		t.Fatalf("both stacks got NATS client port %d; freePorts must hand out distinct ports", a.cfg.NatsClientPort)
	}

	for _, s := range []bundledStack{a, b} {
		if err := s.st.Down(ctx); err != nil {
			t.Fatalf("Down: %v", err)
		}
		waitContainerGone(t, derivedNatsName(s.cfg.StateDir), containerGoneBudget)
		waitContainerGone(t, derivedCollectorName(s.cfg.StateDir), containerGoneBudget)
	}
}

type bundledStack struct {
	cfg stack.Config
	st  *stack.Stack
}

// upBundledStack starts one stack with container postgres, bundled NATS, and the
// bundled collector, every host port drawn from freePorts.
func upBundledStack(ctx context.Context, t *testing.T, root string) bundledStack {
	t.Helper()
	fx := newContainerFixture(t, root)
	ports := freePorts(t, 5)
	cfg := stack.Config{
		StateDir:            fx.cfg.StateDir,
		SocketPath:          fx.cfg.SocketPath,
		ListenAddr:          fx.cfg.ListenAddr,
		DatabaseDSN:         fx.cfg.DatabaseDSN,
		AgentImage:          fx.cfg.AgentImage,
		RuntimeDir:          fx.cfg.RuntimeDir,
		SecretProvider:      seedPortsMasterKey(t),
		PostgresImage:       pgImagePinned,
		CollectorImage:      collectorImagePinned,
		NatsImage:           stack.DefaultNatsImage,
		ExternalGatewayURL:  "http://127.0.0.1:4100",
		NatsClientPort:      ports[0],
		NatsMonitorPort:     ports[1],
		CollectorGRPCPort:   ports[2],
		CollectorHTTPPort:   ports[3],
		CollectorHealthPort: ports[4],
	}
	deps, err := buildDeps(cfg)
	if err != nil {
		t.Fatalf("buildDeps: %v", err)
	}
	t.Cleanup(func() {
		for _, n := range []string{derivedNatsName(cfg.StateDir), derivedCollectorName(cfg.StateDir), derivedContainerName(cfg.StateDir)} {
			if out, err := exec.Command("podman", "rm", "--force", "--volumes", n).CombinedOutput(); err != nil {
				t.Logf("cleanup guard: podman rm %s (ignored): %v\n%s", n, err, out)
			}
		}
	})
	upCtx, cancel := context.WithTimeout(ctx, upBudget)
	defer cancel()
	st, err := stack.Up(upCtx, cfg, deps)
	if err != nil {
		t.Fatalf("Up (%s): %v", filepath.Base(root), err)
	}
	downGuard(t, ctx, st)
	return bundledStack{cfg: cfg, st: st}
}

// seedPortsMasterKey writes a throwaway at-rest key: compass-server fails closed
// without one, and boot decoding wants exactly 64 hex characters.
func seedPortsMasterKey(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(path, []byte("COMPASS_MASTER_KEY="+strings.Repeat("ab", 32)+"\n"), 0o600); err != nil {
		t.Fatalf("write secrets file: %v", err)
	}
	return "dotenv://" + path
}
