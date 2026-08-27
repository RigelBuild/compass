//go:build unix

package adapters

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/RigelBuild/compass/go/internal/stack"
)

// collectorConfigPath is the in-container path the collector image's entrypoint
// reads its config from (`CMD --config /etc/otelcol-contrib/config.yaml`). The
// generated config's host directory is bind-mounted read-only onto the parent
// of this path, so the container reads exactly the config the core rendered.
const collectorConfigPath = "/etc/otelcol-contrib/config.yaml"

// collectorHealthTimeout bounds a single health-probe HTTP GET. The probe is a
// fast liveness check the core polls; a per-request timeout keeps one wedged
// request from stalling a poll tick.
const collectorHealthTimeout = 5 * time.Second

// CollectorContainer is the real stack.CollectorContainer AND
// stack.ContainerController AND stack.CollectorProber: it starts the T4
// container-backed OTel collector via `podman run`, tears it down by name, and
// probes its health_check extension. The three seams share one podman surface
// and one container-naming contract, so they live in one adapter — the start
// side writes the config and records the name the teardown side later signals,
// the probe side reads the health endpoint the same spec fixes.
type CollectorContainer struct {
	cli    containerCLI
	health healthGetter
}

// Compile-time proof the adapter satisfies all three seams it fills.
var (
	_ stack.CollectorContainer  = (*CollectorContainer)(nil)
	_ stack.ContainerController = (*CollectorContainer)(nil)
	_ stack.CollectorProber     = (*CollectorContainer)(nil)
)

// healthGetter issues the collector health-probe HTTP GET. *http.Client
// satisfies the shape via httpGet below; a fake satisfies it in tests so the
// probe's healthy/unhealthy verdict is unit-testable without a live collector.
type healthGetter interface {
	get(ctx context.Context, url string) (int, error)
}

// NewCollectorContainer builds a CollectorContainer over the real podman CLI and
// a real HTTP client. Unlike the postgres adapter it resolves no OS user: the
// collector runs as the image's own non-root user (10001) with no host-user DSN
// identity to match, so there is nothing to fail at construction — but it
// returns an error for signature parity with NewPostgresContainer and so a
// future construction-time dependency has a place to surface.
func NewCollectorContainer() (*CollectorContainer, error) {
	return &CollectorContainer{
		cli:    newPodmanExec(),
		health: &httpHealthGetter{client: &http.Client{Timeout: collectorHealthTimeout}},
	}, nil
}

// Start writes the generated collector config to disk and runs the T4 collector
// container detached, returning a Process handle for the in-process lifecycle.
// The config dir is created and the config written before the run so the podman
// read-only bind-mount source exists. Start returns at launch, not at readiness
// — the core's waitCollector poll is the health gate.
func (c *CollectorContainer) Start(ctx context.Context, spec stack.CollectorContainerSpec) (stack.Process, error) {
	// The bind-mount SOURCE must pre-exist or `podman run` fails with a statfs
	// error. Create the config dir (0700: app-private) and write the config
	// world-readable within it (0644) so the container's non-root user can read
	// the read-only bind-mount.
	if err := os.MkdirAll(spec.ConfigDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating otel-collector config dir %q: %w", spec.ConfigDir, err)
	}
	configFile := filepath.Join(spec.ConfigDir, "config.yaml")
	if err := os.WriteFile(configFile, []byte(spec.ConfigYAML), 0o644); err != nil { //nolint:gosec // G306: the collector config is non-secret and must be readable by the container's non-root user over the read-only bind-mount
		return nil, fmt.Errorf("writing otel-collector config %q: %w", configFile, err)
	}
	if err := c.cli.run(ctx, collectorRunArgs(spec, configFile)); err != nil {
		return nil, fmt.Errorf("podman run otel-collector %q: %w", spec.Name, err)
	}
	return &containerProcess{cli: c.cli, name: spec.Name, stopTimeout: spec.StopTimeout}, nil
}

// Exists reports whether the named collector container is present
// (stack.ContainerController). Like the postgres adapter, a genuine podman
// engine error (neither exit-0 present nor exit-1 absent — a wedged daemon) is
// treated as PRESENT, not absent: a false "absent" would drop the teardown
// target after the pgid record is consumed, stranding a live container. Stop/
// Remove are idempotent, so assuming-present is safe.
func (c *CollectorContainer) Exists(name string) bool {
	present, err := c.cli.exists(context.Background(), name)
	if err != nil {
		return true // cannot confirm absence → assume present and drive teardown
	}
	return present
}

// Stop requests a graceful `podman stop -t <timeout>` (stack.ContainerController).
func (c *CollectorContainer) Stop(name string, timeout time.Duration) error {
	return c.cli.stop(context.Background(), name, timeout)
}

// Remove force-removes the container, the SIGKILL-tier escalation
// (stack.ContainerController): `podman rm -f`.
func (c *CollectorContainer) Remove(name string) error {
	return c.cli.remove(context.Background(), name)
}

// ProbeCollector issues an HTTP GET against the collector's health_check
// endpoint (stack.CollectorProber). A 200 means the collector's receivers and
// pipelines are up and receiving; any non-200 status or a dial/request error
// means not-yet-ready, which the core's readiness poll retries. The endpoint is
// spec.HealthEndpoint (host:port); the health_check extension answers on "/".
func (c *CollectorContainer) ProbeCollector(ctx context.Context, healthEndpoint string) error {
	url := "http://" + healthEndpoint + "/"
	code, err := c.health.get(ctx, url)
	if err != nil {
		return fmt.Errorf("otel-collector health GET %q: %w", url, err)
	}
	if code != http.StatusOK {
		return fmt.Errorf("otel-collector health %q returned status %d, want 200", url, code)
	}
	return nil
}

// collectorRunArgs assembles the T4 `podman run` argv (detached). Split out as a
// pure function so the full flag/publish/mount set is unit-tested without
// spawning podman, mirroring the postgres adapter's runArgs. The contract
// (verified against otel/opentelemetry-collector-contrib):
//
//   - --rm: the container auto-removes when it exits, so a graceful `podman stop`
//     (the SIGTERM teardown tier) both stops AND removes it. The
//     ContainerController.Remove (`podman rm -f`) escalation tier stays for the
//     stop-ignored case and is idempotent on an already-removed container.
//   - --replace: idempotent name reuse — a survivor of this name (a crash before
//     --rm fired, an escalation race) is cleared so a fresh up never collides on
//     the stable per-state-dir name.
//   - --stop-timeout: the safe default for any `podman stop` that names no -t.
//   - -p host:port:container-port for each of grpc/http/health, published on the
//     loopback only (spec endpoints carry the 127.0.0.1 host) — a local fan-in
//     endpoint, never network-exposed.
//   - -v config file -> the in-container config path, read-only: the container
//     reads exactly the core-rendered D3-posture config. No data volume: D3 drops
//     rather than buffering to disk, so the collector holds no on-disk state.
//   - no server args: the image entrypoint's `--config <path>` CMD is inherited,
//     pointing at the bind-mounted config.
func collectorRunArgs(spec stack.CollectorContainerSpec, configFile string) []string {
	return []string{
		"run", "--detach",
		"--rm",
		"--replace",
		"--name", spec.Name,
		"--stop-timeout", strconv.FormatInt(stopSeconds(spec.StopTimeout), 10),
		"-p", spec.GRPCEndpoint + ":4317",
		"-p", spec.HTTPEndpoint + ":4318",
		"-p", spec.HealthEndpoint + ":13133",
		"-v", configFile + ":" + collectorConfigPath + ":ro,Z",
		spec.Image,
	}
}

// httpHealthGetter is the real healthGetter: a thin GET over an *http.Client.
type httpHealthGetter struct {
	client *http.Client
}

// get issues a GET at url under the caller's ctx, returns the status code, and
// closes the response body. A transport error is returned; the status code is
// the caller's healthy/unhealthy verdict.
func (h *httpHealthGetter) get(ctx context.Context, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("build health request: %w", err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	// The status code is the whole verdict; the body is unused. Close it so the
	// probe leaks no connection — a close error on a health GET is not
	// actionable (the code is already read), so it is explicitly discarded.
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
