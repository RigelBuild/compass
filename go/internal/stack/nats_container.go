//go:build unix

package stack

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"time"
)

// natsStopTimeout is the graceful-stop budget pinned into the NATS container's
// run spec as `--stop-timeout` (the safe default for any `podman stop` that
// passes no explicit `-t`). It is LONGER than the collector's 10s because the
// two have opposite shutdown costs: the collector holds no on-disk state (D3
// drops rather than buffering), while NATS with JetStream file storage owns the
// fabric's durable stream state and flushes its store on SIGTERM. It is still
// shorter than postgres's 30s cluster shutdown — a single-node R1 JetStream
// store has no cluster consensus to unwind, only a bounded fsync of at most one
// sync_interval of unflushed writes. 20s leaves generous headroom for that
// flush on a loaded box while still failing a genuinely wedged server before the
// caller's patience runs out.
const natsStopTimeout = 20 * time.Second

// The NATS server's fixed listen ports: the client port surfaces reach over
// nats://, and the HTTP monitoring port is the readiness probe target (/healthz).
// These are container-internal ports the generated config binds on 0.0.0.0; the
// run spec publishes them on the host loopback. They are the upstream nats-server
// defaults, kept literal so the generated config and the published ports never
// drift.
const (
	natsClientPort  = "4222"
	natsMonitorPort = "8222"
	// natsListenHost is the host interface the ports are published on: the
	// loopback only, never 0.0.0.0. NATS sits on the trusted control-plane tier
	// and carries no credentials in this shape, so it must never be
	// network-exposed; an operator wanting a shared NATS runs --nats-external and
	// points at their own.
	natsListenHost = "127.0.0.1"
)

// NatsStoreDir is the in-container directory JetStream's file storage lives in:
// the `store_dir` the core renders into the config AND the mount target the
// adapter bind-mounts the host DataDir onto (read-WRITE — this is durable
// state, unlike the collector's config-only read-only mount). It is exported
// precisely because those two sides live in different packages: a duplicated
// literal that drifted would leave nats-server writing JetStream state into the
// container's ephemeral layer, silently losing every stream on restart with no
// error anywhere. nats-server creates a `jetstream` subdirectory beneath it.
const NatsStoreDir = "/var/lib/nats"

// NatsContainerSpec is the fully-resolved description of the bundled NATS
// container child. The core builds it from Config; the adapter translates it
// into the `podman run` argv and writes the generated config file — the same
// core-builds-spec / adapter-runs-it split CollectorContainerSpec uses, so the
// config and port set is a pure, unit-testable value rather than argv assembled
// behind the seam. NATS is a hybrid of the two existing container components: a
// generated config dir like the collector AND a persistent data dir like
// postgres, because JetStream file storage is durable state.
type NatsContainerSpec struct {
	// Name is the stable per-state-dir container name (derived from StateDir),
	// the teardown identity a fresh `down` reconstructs and the v2 container
	// entry persists. Unique per state dir so concurrent stacks never collide in
	// podman's flat container namespace. Like the collector this scopes only the
	// name: the loopback ports below are fixed, so two stacks on one host still
	// contend for those binds — the embedded stack is one-per-host by design.
	Name string
	// Image is the NATS image ref to run (Config.NatsImage; the pinned
	// DefaultNatsImage on the installed path).
	Image string
	// ConfigDir is the host directory the generated nats-server config is
	// written into and bind-mounted (read-only) from, fixed under the state dir
	// (<StateDir>/nats-config). Kept DISTINCT from DataDir so the read-only
	// config mount never overlaps the read-write JetStream store.
	ConfigDir string
	// ConfigYAML is the fully-rendered nats-server config (JetStream on with the
	// record's bounded-fsync sync_interval, the monitoring endpoint, the client
	// port). The core renders it so the posture is a pure, unit-tested value;
	// the adapter only writes it to disk. Named ConfigYAML for parity with
	// CollectorContainerSpec — the nats-server config grammar is its own
	// JSON-superset dialect, not literally YAML.
	ConfigYAML string
	// DataDir is the host directory JetStream's file store is bind-mounted from,
	// fixed under the state dir (<StateDir>/nats), mirroring how
	// PostgresContainerSpec.DataDir fixes PGDATA. It is mounted read-WRITE at
	// NatsStoreDir: this is the fabric's durable state and must survive a
	// container replace.
	DataDir string
	// ClientEndpoint is the host loopback endpoint the client port is published
	// on (host:port). It is what a future nats:// URL is formed from; nothing
	// in-tree connects to it yet.
	ClientEndpoint string
	// MonitorEndpoint is the host loopback endpoint the HTTP monitoring port is
	// published on (host:port); the readiness probe issues an HTTP GET against
	// its /healthz.
	MonitorEndpoint string
	// StopTimeout is the `--stop-timeout` pinned into the run (natsStopTimeout).
	StopTimeout time.Duration
}

// natsContainerSpec builds the NATS run spec from the resolved config: it
// derives the stable container name and the config + data dirs from the state
// dir, renders the JetStream-on config, and fixes the published loopback
// endpoints. It is pure (no I/O) so the config and endpoint set it encodes is
// unit-tested directly, and it errors on a config missing the state dir the
// bind-mounts and name derivation need rather than running podman against a
// half-formed spec.
func natsContainerSpec(cfg Config) (NatsContainerSpec, error) {
	if cfg.StateDir == "" {
		return NatsContainerSpec{}, errors.New("stack config: StateDir is required for the nats container (config + JetStream data bind-mounts + name derivation)")
	}
	if cfg.NatsImage == "" {
		return NatsContainerSpec{}, errors.New("stack config: NatsImage is required to bundle nats (set --nats-image or use --nats-external to opt out)")
	}
	spec := NatsContainerSpec{
		Name:            natsContainerName(cfg.StateDir),
		Image:           cfg.NatsImage,
		ConfigDir:       filepath.Join(cfg.StateDir, "nats-config"),
		DataDir:         filepath.Join(cfg.StateDir, "nats"),
		ClientEndpoint:  natsListenHost + ":" + natsClientPort,
		MonitorEndpoint: natsListenHost + ":" + natsMonitorPort,
		StopTimeout:     natsStopTimeout,
	}
	spec.ConfigYAML = natsConfigYAML()
	return spec, nil
}

// natsContainerName derives the stable per-state-dir NATS container name. Like
// containerName (postgres) and collectorContainerName it is a deterministic
// function of the state dir alone so a fresh `down` with no in-memory handle
// reconstructs the same name, and the hash keeps concurrent stacks on different
// state dirs from colliding in podman's flat container namespace. A distinct
// prefix from the postgres and collector names keeps the three components'
// containers legible apart in `podman ps`.
func natsContainerName(stateDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(stateDir)))
	return "compass-nats-" + hex.EncodeToString(sum[:6])
}

// natsConfigYAML renders the nats-server config realizing the record's fabric
// posture. It is a pure function (every value is a fixed container-internal
// port or path) so the posture is unit-tested directly:
//
//   - host/port: the client listener, bound 0.0.0.0 INSIDE the container (the
//     run spec publishes it on the host loopback only, so the 0.0.0.0 bind is
//     scoped to the container's own netns, not the host's).
//   - http: the HTTP monitoring endpoint. It is the readiness probe target
//     (/healthz) — the reason the monitoring subsystem is enabled at all, since
//     the probe is plain net/http and pulls no NATS client library into the
//     stack package.
//   - jetstream.store_dir: NatsStoreDir, the read-write bind-mount of the host
//     DataDir, so streams survive a container replace. Single-node R1 file
//     storage; clustering is out of scope here.
//   - jetstream.sync_interval: 100ms, the record's Jepsen-driven bounded-fsync
//     value (design.md:363). It is a SERVER setting, not a per-stream
//     jetstream.StreamConfig field, which is exactly why it lives in this
//     container's config and not in any producer's stream declaration: it caps
//     how much acknowledged-but-unflushed data a hard power loss can lose,
//     uniformly for every stream the server holds.
func natsConfigYAML() string {
	return `host: "0.0.0.0"
port: ` + natsClientPort + `

http: "0.0.0.0:` + natsMonitorPort + `"

jetstream {
  store_dir: "` + NatsStoreDir + `"
  sync_interval: "100ms"
}
`
}
