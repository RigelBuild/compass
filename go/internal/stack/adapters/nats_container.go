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

// natsConfigDir is the in-container directory the generated nats-server config
// is bind-mounted into, and natsConfigPath the file within it the server is
// pointed at via its `-c` argument. A dedicated compass-owned path (rather than
// the image's own /etc/nats) keeps our generated config from colliding with the
// image's default nats-server.conf, which the entrypoint's default CMD reads —
// the explicit `-c` overrides that CMD entirely.
const (
	natsConfigDir  = "/etc/compass-nats"
	natsConfigFile = "nats-server.conf"
	natsConfigPath = natsConfigDir + "/" + natsConfigFile
)

// natsHealthTimeout bounds a single readiness-probe HTTP GET against the NATS
// monitoring endpoint. The probe is a fast liveness check the core polls; a
// per-request timeout keeps one wedged request from stalling a poll tick.
const natsHealthTimeout = 5 * time.Second

// NatsContainer is the real stack.NatsContainer AND stack.ContainerController
// AND stack.NatsProber: it starts the bundled NATS server via `podman run`,
// tears it down by name, and probes its HTTP monitoring endpoint. The three
// seams share one podman surface and one container-naming contract, so they live
// in one adapter — the start side writes the config and records the name the
// teardown side later signals, the probe side reads the monitoring endpoint the
// same spec fixes. This is the collector adapter's shape verbatim.
type NatsContainer struct {
	cli    containerCLI
	health healthGetter
}

// Compile-time proof the adapter satisfies all three seams it fills.
var (
	_ stack.NatsContainer       = (*NatsContainer)(nil)
	_ stack.ContainerController = (*NatsContainer)(nil)
	_ stack.NatsProber          = (*NatsContainer)(nil)
)

// NewNatsContainer builds a NatsContainer over the real podman CLI and a real
// HTTP client. Like NewCollectorContainer it resolves no OS user — nats-server
// runs as the image's own user against a host-owned bind-mount — but it returns
// an error for signature parity with the sibling container constructors and so a
// future construction-time dependency has a place to surface.
func NewNatsContainer() (*NatsContainer, error) {
	return &NatsContainer{
		cli:    newPodmanExec(),
		health: &httpHealthGetter{client: &http.Client{Timeout: natsHealthTimeout}},
	}, nil
}

// Start writes the generated nats-server config to disk, creates the JetStream
// data dir, and runs the NATS container detached, returning a Process handle for
// the in-process lifecycle. Both bind-mount sources are created before the run
// so podman does not fail on a missing source. Start returns at launch, not at
// readiness — the core's waitNats poll is the health gate.
func (c *NatsContainer) Start(ctx context.Context, spec stack.NatsContainerSpec) (stack.Process, error) {
	// Both bind-mount SOURCES must pre-exist or `podman run` fails with a statfs
	// error. Both dirs are 0700 (app-private): the config dir holds only the
	// generated server config, and the data dir holds the fabric's JetStream
	// store, which no other user has any business reading.
	if err := os.MkdirAll(spec.ConfigDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating nats config dir %q: %w", spec.ConfigDir, err)
	}
	if err := os.MkdirAll(spec.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating nats jetstream data dir %q: %w", spec.DataDir, err)
	}
	configFile := filepath.Join(spec.ConfigDir, natsConfigFile)
	// The pinned image runs as root and 0600 is verified to work; retain 0644
	// defensively for a future image that runs non-root, as the collector does.
	// The config is non-secret and only contains ports, store dir, and interval.
	if err := os.WriteFile(configFile, []byte(spec.ConfigYAML), 0o644); err != nil { //nolint:gosec // G306: the nats server config is non-secret and must be readable by the container's user over the read-only bind-mount
		return nil, fmt.Errorf("writing nats config %q: %w", configFile, err)
	}
	if err := c.cli.run(ctx, natsRunArgs(spec, configFile)); err != nil {
		return nil, fmt.Errorf("podman run nats %q: %w", spec.Name, err)
	}
	return &containerProcess{cli: c.cli, name: spec.Name, stopTimeout: spec.StopTimeout}, nil
}

// Exists reports whether the named nats container is present
// (stack.ContainerController). Like the postgres and collector adapters, a
// genuine podman engine error (neither exit-0 present nor exit-1 absent — a
// wedged daemon) is treated as PRESENT, not absent: a false "absent" would drop
// the teardown target after the pgid record is consumed, stranding a live
// container holding the JetStream store's file locks. Stop/Remove are
// idempotent, so assuming-present is safe.
//
// The ContainerController seam takes no ctx (stack/deps.go), so there is no
// caller context to thread here — the podman call is bounded by the CLI's own
// per-command timeout.
func (c *NatsContainer) Exists(name string) bool {
	present, err := c.cli.exists(context.Background(), name)
	if err != nil {
		return true // cannot confirm absence → assume present and drive teardown
	}
	return present
}

// Stop requests a graceful `podman stop -t <timeout>` (stack.ContainerController),
// which SIGTERMs nats-server and lets it flush the JetStream store.
func (c *NatsContainer) Stop(name string, timeout time.Duration) error {
	return c.cli.stop(context.Background(), name, timeout)
}

// Remove force-removes the container, the SIGKILL-tier escalation
// (stack.ContainerController): `podman rm -f`. This kills nats-server mid-flush,
// so the store recovers on next boot — the escalation is for a server that
// ignored the graceful stop, never the first resort.
func (c *NatsContainer) Remove(name string) error {
	return c.cli.remove(context.Background(), name)
}

// ProbeNats issues an HTTP GET against the NATS server's monitoring /healthz
// endpoint (stack.NatsProber). A 200 means the server is up and JetStream has
// finished enabling; any non-200 status or a dial/request error means
// not-yet-ready, which the core's readiness poll retries. The endpoint is
// spec.MonitorEndpoint (host:port).
//
// It is deliberately an HTTP probe, not a nats:// client connect: the readiness
// gate must not pull a NATS client library into the supervisor, and /healthz is
// the server's own readiness verdict, strictly better than "a TCP dial
// succeeded".
func (c *NatsContainer) ProbeNats(ctx context.Context, monitorEndpoint string) error {
	url := "http://" + monitorEndpoint + "/healthz"
	code, err := c.health.get(ctx, url)
	if err != nil {
		return fmt.Errorf("nats health GET %q: %w", url, err)
	}
	if code != http.StatusOK {
		return fmt.Errorf("nats health %q returned status %d, want 200", url, code)
	}
	return nil
}

// natsRunArgs assembles the NATS `podman run` argv (detached). Split out as a
// pure function so the full flag/publish/mount set is unit-tested without
// spawning podman, mirroring collectorRunArgs. The contract (verified against
// docker.io/library/nats:2.14.6-alpine):
//
//   - --rm: the container auto-removes when it exits, so a graceful `podman stop`
//     (the SIGTERM teardown tier) both stops AND removes it. The
//     ContainerController.Remove (`podman rm -f`) escalation tier stays for the
//     stop-ignored case and is idempotent on an already-removed container. The
//     JetStream store survives regardless: it lives on the host bind-mount, not
//     the container layer.
//   - --replace: idempotent name reuse — a survivor of this name (a crash before
//     --rm fired, an escalation race) is cleared so a fresh up never collides on
//     the stable per-state-dir name.
//   - --stop-timeout: the safe default for any `podman stop` that names no -t,
//     sized to let nats-server flush its JetStream store (natsStopTimeout).
//   - -p host:port:container-port for the client and monitoring ports, published
//     on the loopback only (the spec endpoints carry the 127.0.0.1 host) — a
//     trusted-tier local broker, never network-exposed.
//   - -v config file -> the in-container config path, read-only: the server reads
//     exactly the core-rendered config.
//   - -v data dir -> the in-container JetStream store dir, read-WRITE: unlike the
//     collector, NATS holds durable state that must survive a container replace.
//   - server args: an explicit `-c <path>`, which overrides the image's default
//     CMD (`nats-server --config /etc/nats/nats-server.conf`) so the server reads
//     our generated config rather than the image's stock one.
func natsRunArgs(spec stack.NatsContainerSpec, configFile string) []string {
	return []string{
		cmdPodmanRun, flagPodmanDetach,
		flagPodmanRM,
		flagPodmanReplace,
		flagPodmanName, spec.Name,
		flagPodmanStopTimeout, strconv.FormatInt(stopSeconds(spec.StopTimeout), 10),
		"-p", spec.ClientEndpoint + ":4222",
		"-p", spec.MonitorEndpoint + ":8222",
		"-v", configFile + ":" + natsConfigPath + ":ro,Z",
		"-v", spec.DataDir + ":" + stack.NatsStoreDir + ":Z",
		spec.Image,
		"-c", natsConfigPath,
	}
}
