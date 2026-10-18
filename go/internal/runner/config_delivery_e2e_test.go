//go:build podman

package runner

// Real-agent E2E: a config version bump reloads the container and the agent logs
// the new bundle hash. It skips without rootless podman or a bootable image.

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// agentImage is the real agent image the E2E boots. Present in containers-storage
// on a real-podman host; the test skips when it is absent rather than building
// it (the image is the heavy dogfood:agent-image nix build, not an in-test one).
const agentImage = "compass-agent:latest"

// agentImageExists reports whether the real agent image is present in local
// storage. A missing image means skip, not fail — the E2E is meaningless without
// the real reader.
func agentImageExists(ctx context.Context) bool {
	return exec.CommandContext(ctx, "podman", "image", "exists", agentImage).Run() == nil
}

// agentBootResult distinguishes a loadable native addon from an image that
// cannot reach the config read under the Runner's default-deny egress.
type agentBootResult int

const (
	// agentBootable: the addon loaded (loadNative's success marker is present).
	agentBootable agentBootResult = iota
	// agentUnbootable: the known image-recipe defect fired (the addon-load
	// failure prefix is present) — a present-but-unbootable image, skip not fail.
	agentUnbootable
	// No marker means the startup-probe contract may have drifted; skip loudly.
	agentBootInconclusive
)

// probeAgentBoot runs offline to avoid hanging on addon recovery, then
// distinguishes a known load failure from an inconclusive marker probe.
func probeAgentBoot(ctx context.Context) agentBootResult {
	out, _ := exec.CommandContext(ctx,
		"podman", "run", "--rm", "--network=none",
		"--env", "PI_DEBUG_STARTUP=1",
		agentImage, "compass-agent",
	).CombinedOutput()
	s := string(out)
	// The addon loader throws this exact prefix when no pi_natives*.node is
	// resolvable; its presence is the known-defect signal.
	if strings.Contains(s, "Failed to load pi_natives native addon") {
		return agentUnbootable
	}
	// loadNative's success marker confirms the addon actually loaded.
	if strings.Contains(s, "native:loadNative:done") {
		return agentBootable
	}
	return agentBootInconclusive
}

// podmanUsable reports whether rootless podman can run a container here. A
// missing binary or broken rootless setup means skip, not fail — mirrors the
// sibling guard in runtime/lifecycle_test.go and gateway/socket_podman_test.go.
func podmanUsable(ctx context.Context) bool {
	return exec.CommandContext(ctx, "podman", "run", "--rm", "docker.io/library/alpine:latest", "true").Run() == nil
}

// collectingLog retains every record and wakes waiters on each log event, so the
// real agent's stderr cannot overflow a bounded test channel.
type collectingLog struct {
	mu      sync.Mutex
	cond    *sync.Cond
	records []logLine
}

func newCollectingLog() *collectingLog {
	c := &collectingLog{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *collectingLog) Enabled(context.Context, slog.Level) bool { return true }

func (c *collectingLog) Handle(_ context.Context, r slog.Record) error {
	line := logLine{msg: r.Message, attrs: make(map[string]string, r.NumAttrs())}
	r.Attrs(func(a slog.Attr) bool {
		line.attrs[a.Key] = a.Value.String()
		return true
	})
	c.mu.Lock()
	c.records = append(c.records, line)
	c.mu.Unlock()
	c.cond.Broadcast()
	return nil
}

// WithAttrs / WithGroup return the same handler: the drain stamps every attribute
// inline on its Debug call (as captureLog documents), so nothing is lost.
func (c *collectingLog) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *collectingLog) WithGroup(string) slog.Handler      { return c }

func (c *collectingLog) logger() *slog.Logger { return slog.New(c) }

// waitForConfigVersionLine waits for the agent's captured version line; a
// watchdog wakes it to fail when no matching record arrives.
func waitForConfigVersionLine(t *testing.T, log *collectingLog, hash string, timeout time.Duration) {
	t.Helper()
	want := "config version: " + hash
	deadline := time.Now().Add(timeout)
	watchdog := time.AfterFunc(timeout, func() {
		log.mu.Lock()
		log.cond.Broadcast()
		log.mu.Unlock()
	})
	defer watchdog.Stop()

	log.mu.Lock()
	defer log.mu.Unlock()
	scanned := 0
	for {
		for ; scanned < len(log.records); scanned++ {
			rec := log.records[scanned]
			if rec.msg == "agent stderr" && strings.Contains(rec.attrs["line"], want) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for agent stderr line containing %q (real agent never emitted the %s bundle version — no Reload, or the reader did not pick up the flipped config)", timeout, want, hash)
		}
		log.cond.Wait()
	}
}

// e2eSpecBuilder creates the real image's agent workspace. Provision adds the
// config and socket mounts; the checkout is under the only agent-writable path,
// HOME, because the image has no world-writable /work or /workspace.
type e2eSpecBuilder struct {
	name string
}

func (b e2eSpecBuilder) BuildSpec(*compassv1.ProvisionAgentWorkspaceRequest, string) (runtime.AgentSpec, error) {
	return runtime.AgentSpec{
		Name:  b.name,
		Image: agentImage,
		Workspace: runtime.Workspace{
			CheckoutDir: "/home/agent/repo",
			HomeDir:     "/home/agent",
			UID:         1000,
		},
		Egress: runtime.MustAllowEgress("github.com"),
	}, nil
}

// TestConfigDeliveryReloadPicksUpNewBundle proves RefreshConfig flips the bundle
// and reloads the real agent, whose second boot line must contain the v2 hash.
func TestConfigDeliveryReloadPicksUpNewBundle(t *testing.T) {
	if !podmanUsable(t.Context()) {
		t.Skip("rootless podman not usable in this environment")
	}
	if !agentImageExists(t.Context()) {
		t.Skip(agentImage + " not present in local storage")
	}
	switch probeAgentBoot(t.Context()) {
	case agentUnbootable:
		// Skip loudly when the present image cannot boot, since this E2E cannot
		// reach the config path under test.
		t.Skip(agentImage + " is present but its agent cannot boot (no loadable pi_natives addon); config-delivery loop unreachable until the agent image ships a working addon")
	case agentBootInconclusive:
		// Missing startup markers may indicate probe drift; report a distinct skip.
		t.Skip(agentImage + ": startup probe inconclusive — saw neither the pi_natives load-failure nor the native:loadNative:done marker; the PI_DEBUG_STARTUP marker contract may have drifted, so this test is no longer auto-activating — investigate the probe rather than assuming the known image defect")
	case agentBootable:
		// Bootable — run the config-delivery proof below.
	}

	// Distinct versions ensure the v2 wait cannot match the initial boot line.
	const v1hash = "deliveryalpha"
	const v2hash = "deliverybravo"

	// Use the production runtime so the agent runs inside a real container.
	engine := runtime.NewPodmanCLI()
	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(engine, registry)

	// setConfigBundle drives the version fetched by the materializer.
	pub := newCapturePublish()
	link := newLink(newRunnerServiceServer(t, pub))

	// Capture all records so the drained stderr version line cannot be dropped.
	log := newCollectingLog()

	name := "compass-cfgdelivery-" + strconv.Itoa(os.Getpid())

	// Remove stale containers and register teardown so failures cannot leak one.
	// Stop reaps the stream; force-remove tears down the container.
	cleanupCtx := context.WithoutCancel(t.Context())
	_ = exec.CommandContext(cleanupCtx, "podman", "rm", "--force", name).Run()
	t.Cleanup(func() { _ = exec.CommandContext(cleanupCtx, "podman", "rm", "--force", name).Run() })

	cfg := AgentHostConfig{RuntimeDir: shortRuntimeDir(t)}
	host := NewSessionHost(link, rt, registry, engine, e2eSpecBuilder{name: name}, cfg, log.logger()).(*agentHost)
	// Close the host last: only it closes the per-container socket listener.
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		host.Close(closeCtx)
	})

	ctx := t.Context()

	// 1. Seed the fleet at v1 and bring the container + agent online.
	pub.setConfigBundle(AgentConfigBundle{Version: v1hash, Tarball: buildConfigTarball(t, validBundle())})

	container, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{}, "e2e")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	sessionID, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: container}, "", "test-session")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = host.Stop(context.WithoutCancel(t.Context()), sessionID) })

	// Verify the first boot read the initially materialized v1 bundle.
	waitForConfigVersionLine(t, log, v1hash, 90*time.Second)

	// Bump to v2 and call RefreshConfig directly; the signal-to-worker hop is
	// covered separately, while this E2E proves the real-container reader pickup.
	pub.setConfigBundle(AgentConfigBundle{Version: v2hash, Tarball: buildConfigTarball(t, validBundle())})
	if err := host.RefreshConfig(ctx); err != nil {
		t.Fatalf("RefreshConfig: %v", err)
	}

	// The second boot must report v2 after Reload reads the flipped current link.
	waitForConfigVersionLine(t, log, v2hash, 90*time.Second)
}
