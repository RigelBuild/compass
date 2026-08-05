//go:build podman

package runner

// Full-stack config-delivery proof against a REAL rootless-podman container
// running the REAL compass-agent:latest image. It closes the deferred sub-slice
// of the config-in-place-update work: the substrate half (bind-mount RO + live
// current flip, alpine + shell reader) is already proven by
// runtime/config_mount_test.go, and the signal->worker->RefreshConfig hop is
// already proven hermetically by dispatch_test.go. What no other test exercises
// is the REAL agent reader picking up a NEW bundle across a REAL Reload:
//
//	ConfigVersion (fleet bump) -> host.RefreshConfig -> re-materialize v-2 + flip
//	current -> in-place Reload (Stop old agent + StartAgent re-exec) -> the REAL
//	compass-agent re-boots, reads current/, emits the NEW bundle's version hash.
//
// The observable is the agent's own boot line. On each boot the real agent
// connects its gateway socket, loads the mounted config, and emits to STDERR
// `[compass-agent] config version: <hash>` (cli.ts) where <hash> is the content
// of current/version — a runner-written file sourced from bundle.Version. The
// runner drains the agent's stderr into its slog (agent_exec.go drainToLog with
// msg "agent stderr" and a "line" attr), so the version line lands as a captured
// log record: msg == "agent stderr", attrs["line"] contains the boot line.
//
// A bare `podman run compass-agent:latest` emits nothing — main() returns early
// at the gateway-socket connect when no socket is mounted, BEFORE the config
// load. The line only emits when the agent is exec'd into a container whose
// gateway socket is live, i.e. through the real runner host. So the E2E MUST go
// through NewSessionHost, never a bare run.
//
// Skipped (not failed) when podman is unusable or compass-agent:latest is absent
// from local storage, matching lifecycle_test / config_mount_test. Build-tagged
// (podman) so it is not part of the hermetic gate; it runs on real-podman hosts
// only.

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

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/runtime"
)

// agentImage is the real agent image the E2E boots. Present in containers-storage
// on a real-podman host; the test skips when it is absent rather than building
// it (the image is the heavy dogfood:agent-image nix build, not an in-test one).
const agentImage = "compass-agent:latest"

// agentImageExists reports whether the real agent image is present in local
// storage. A missing image means skip, not fail — the E2E is meaningless without
// the real reader.
func agentImageExists() bool {
	return exec.Command("podman", "image", "exists", agentImage).Run() == nil
}

// agentBootResult is the outcome of the agent-image boot probe. Present is not
// the same as bootable: the image bundles the compass-agent JS entrypoint, which
// loads a native addon (pi_natives) at module init, before main(). An image
// whose recipe never ships that addon beside the bundle throws at load and,
// under the Runner's default-deny egress, hangs on the addon's npmjs-fetch
// recovery — so the agent never reaches the config read this test observes.
type agentBootResult int

const (
	// agentBootable: the addon loaded (loadNative's success marker is present).
	agentBootable agentBootResult = iota
	// agentUnbootable: the known image-recipe defect fired (the addon-load
	// failure prefix is present) — a present-but-unbootable image, skip not fail.
	agentUnbootable
	// agentBootInconclusive: the probe saw NEITHER startup marker. The success
	// marker is an oh-my-pi debug string emitted under PI_DEBUG_STARTUP, a
	// contract that lives in a sibling fork and has been volatile; its absence on
	// an otherwise-runnable image means that contract may have DRIFTED, so the
	// caller must skip loudly and distinctly (silently reusing the known-defect
	// narrative would let this test stop auto-activating with no alarm).
	agentBootInconclusive
)

// probeAgentBoot boots the agent offline (--network=none, the Runner's egress
// posture) with the addon-loader's startup markers on, and classifies the
// outcome. Booting offline forces a broken image to fail fast rather than hang
// on npmjs recovery. Classifying (rather than a bare bool) keeps this test
// HONEST: the caller emits a DIFFERENT loud skip for a known-unbootable image
// than for a drifted-marker probe, so neither a present-but-unbootable image nor
// a stale probe ever reads as a config-delivery failure — but neither is a
// silent pass either.
func probeAgentBoot() agentBootResult {
	out, _ := exec.Command(
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
func podmanUsable() bool {
	return exec.Command("podman", "run", "--rm", "docker.io/library/alpine:latest", "true").Run() == nil
}

// collectingLog is a mutex-guarded slice-collecting slog.Handler: it records
// EVERY log record's message and string attributes, so a real agent boot (which
// emits many stderr lines) never overflows and drops the one line under test —
// the 64-cap channel handler (captureLog.recvLine) would. A sync.Cond wakes any
// waiter the instant a new record lands, so waitForConfigVersionLine gates on the
// record event, never a clock.
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

// waitForConfigVersionLine blocks until a captured record proves the real agent
// booted and read the bundle whose version is hash — msg == "agent stderr" and
// its "line" attr contains `config version: <hash>` — or fails the test at the
// deadline. It gates on the record event (cond.Broadcast on each Handle) rather
// than polling a clock; a watchdog timer broadcasts once at the deadline so a
// wedged boot breaks out of the wait and fails loudly instead of hanging.
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

// e2eSpecBuilder produces a real AgentSpec for the E2E container: the real agent
// image, a unique name, the unprivileged agent workspace, and an egress
// allowlist. The config mount + socket mount are added by Provision itself
// (host.go), so the builder does NOT add them.
//
// CheckoutDir sits UNDER the agent-writable $HOME (/home/agent/repo), not the
// production /workspace default: Launch's ensureCheckoutDir does `mkdir -p` the
// checkout dir as the unprivileged agent uid, and its precondition is a
// parent writable by that uid. The real compass-agent:latest image ships no
// world-writable /work or /workspace (only $HOME is agent-owned), so a checkout
// under $HOME is what satisfies that precondition here. The checkout dir is not
// what this test proves — the config-delivery loop is — so anchoring it on the
// image's one agent-writable tree keeps the fixture launchable without a
// production or image change.
type e2eSpecBuilder struct {
	name string
}

func (b e2eSpecBuilder) BuildSpec(_ *compassv1.ProvisionAgentWorkspaceRequest) (runtime.AgentSpec, error) {
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

// TestConfigDeliveryReloadPicksUpNewBundle is the full-stack proof: a
// ConfigVersion bump drives the real runner's RefreshConfig, which re-materializes
// the new bundle, flips current, and Reloads the real agent in place — and the
// re-booted agent reads the flipped config and emits the NEW version hash. The
// load-bearing assertion is the SECOND agent-stderr line carrying the v2 hash.
func TestConfigDeliveryReloadPicksUpNewBundle(t *testing.T) {
	if !podmanUsable() {
		t.Skip("rootless podman not usable in this environment")
	}
	if !agentImageExists() {
		t.Skip(agentImage + " not present in local storage")
	}
	switch probeAgentBoot() {
	case agentUnbootable:
		// LOUD skip, never a silent pass: the image is present but its agent
		// process cannot start (the recipe ships no loadable pi_natives addon),
		// so the config-delivery loop this test proves is unreachable through no
		// fault of the runner. A known image-recipe defect, tracked separately;
		// the test auto-activates the instant a bootable image is loaded. A skip,
		// not a fail: a fail here would misattribute an image defect to the
		// runner's config path.
		t.Skip(agentImage + " is present but its agent cannot boot (no loadable pi_natives addon); config-delivery loop unreachable until the agent image ships a working addon")
	case agentBootInconclusive:
		// The probe saw neither the known load-failure nor the success marker.
		// The image may be otherwise bootable, but the PI_DEBUG_STARTUP marker
		// contract this probe keys on has likely drifted — so this test is no
		// longer reliably auto-activating. Skip LOUDLY and distinctly: do NOT
		// reuse the known-defect narrative, which would hide a real coverage loss.
		t.Skip(agentImage + ": startup probe inconclusive — saw neither the pi_natives load-failure nor the native:loadNative:done marker; the PI_DEBUG_STARTUP marker contract may have drifted, so this test is no longer auto-activating — investigate the probe rather than assuming the known image defect")
	case agentBootable:
		// Bootable — run the config-delivery proof below.
	}

	// Two distinct bundle versions, neither a substring of the other, so the v2
	// wait cannot match on the v1 boot line. The version file the runner writes
	// from bundle.Version is what the agent emits, so setting Version controls
	// the observable hash directly.
	const v1hash = "deliveryalpha"
	const v2hash = "deliverybravo"

	// Real engine + runtime: the production PodmanCLI and AgentRuntime, so the
	// agent is really exec'd into a real container.
	engine := runtime.NewPodmanCLI()
	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(engine, registry)

	// Live config wire: setConfigBundle drives the fleet version the materializer
	// fetches, exactly as newConfigRefreshFixture does.
	pub := newCapturePublish()
	link := newLink(newRunnerServiceServer(t, pub))

	// Capturing logger that keeps EVERY record (see collectingLog): the agent's
	// stderr drains here, so the version line lands as a captured record.
	log := newCollectingLog()

	name := "compass-cfgdelivery-" + strconv.Itoa(os.Getpid())

	// Force-remove any leftover from a crashed run so the name is free, and guard
	// teardown (Go has no Drop): a failure before Stop must never leak the real
	// container. The force-remove is the belt to Stop's suspenders — Stop reaps
	// the agent stream; the container itself is torn down here.
	// t.Cleanup discards are deliberate: best-effort teardown of a real container
	// has no actionable error path (the run is already ending).
	_ = exec.Command("podman", "rm", "--force", name).Run()
	t.Cleanup(func() { _ = exec.Command("podman", "rm", "--force", name).Run() })

	cfg := AgentHostConfig{RuntimeDir: shortRuntimeDir(t)}
	host := NewSessionHost(link, rt, registry, engine, e2eSpecBuilder{name: name}, cfg, log.logger(), monotonicIDs()).(*agentHost)
	// Close the host last: it Closes the per-container socket listener that
	// Provision opens, which neither Stop (agent stream only) nor the container
	// force-remove reaches. Mirrors e2e_transport_test.go.
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		host.Close(closeCtx)
	})

	// context.Background() as the test root — the rule's explicit test exemption.
	ctx := context.Background()

	// 1. Seed the fleet at v1 and bring the container + agent online.
	pub.setConfigBundle(AgentConfigBundle{Version: v1hash, Tarball: buildConfigTarball(t, validBundle())})

	container, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "e2e"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	sessionID, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: container}, "")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = host.Stop(context.Background(), sessionID) })

	// 2. The first boot must emit the v1 hash — proves the real reader ran on
	// construction and read the initially materialized bundle.
	waitForConfigVersionLine(t, log, v1hash, 90*time.Second)

	// 3. Bump the fleet to v2 (distinct version => distinct version file) and
	// drive the real ConfigVersion-driven pass: re-materialize + flip current +
	// Reload. Driving RefreshConfig directly is correct — the signal->worker hop
	// is already proven hermetically in dispatch_test.go; the E2E's unique value
	// is the real-container reader pickup.
	pub.setConfigBundle(AgentConfigBundle{Version: v2hash, Tarball: buildConfigTarball(t, validBundle())})
	if err := host.RefreshConfig(ctx); err != nil {
		t.Fatalf("RefreshConfig: %v", err)
	}

	// 4. The load-bearing assertion: a SECOND agent-stderr line carries the NEW
	// hash. The real agent re-booted on Reload and read the flipped current/ ->
	// the new bundle.
	waitForConfigVersionLine(t, log, v2hash, 90*time.Second)
}
