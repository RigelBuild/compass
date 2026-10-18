//go:build unix

package stack

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strconv"
	"time"
)

// natsStopTimeout gives JetStream time to fsync its store on graceful stop; it exceeds the stateless collector's budget.
const natsStopTimeout = 20 * time.Second

// These fixed container ports are shared by the generated config and adapter publish flags.
const (
	NatsContainerClientPort  = "4222"
	NatsContainerMonitorPort = "8222"
)

// DefaultNatsClientPort and DefaultNatsMonitorPort are loopback publish defaults; external NATS is required to share access.
const (
	DefaultNatsClientPort  = 4222
	DefaultNatsMonitorPort = 8222
)

// NatsStoreDir is the in-container JetStream store mount and config path; a mismatch would lose state on replacement.
const NatsStoreDir = "/var/lib/nats"

// NatsContainerSpec combines generated read-only config with persistent JetStream data.
type NatsContainerSpec struct {
	// Name is stable per state dir and persisted for detached teardown.
	Name string
	// Image is the NATS image ref, normally the pinned DefaultNatsImage.
	Image string
	// ConfigDir is mounted read-only; JetStream data uses a separate read-write mount.
	ConfigDir string
	// ConfigYAML is the rendered nats-server config; its grammar is JSON-superset, not YAML.
	ConfigYAML string
	// DataDir stores JetStream state and survives container replacement.
	DataDir string
	// ClientEndpoint is the host loopback endpoint for the published client port.
	ClientEndpoint string
	// MonitorEndpoint is the host loopback port checked by the /healthz probe.
	MonitorEndpoint string
	// StopTimeout is the graceful stop budget pinned into podman's run spec.
	StopTimeout time.Duration
}

// natsContainerSpec resolves the state-dir mounts, JetStream config, and loopback endpoints without I/O.
func natsContainerSpec(cfg Config) (NatsContainerSpec, error) {
	if cfg.StateDir == "" {
		return NatsContainerSpec{}, errors.New("stack config: StateDir is required for the nats container (config + JetStream data bind-mounts + name derivation)")
	}
	if cfg.NatsImage == "" {
		return NatsContainerSpec{}, errors.New("stack config: NatsImage is required to bundle nats (set --nats-image or use --nats-external to opt out)")
	}
	client, err := loopbackEndpoint("NatsClientPort", cfg.NatsClientPort)
	if err != nil {
		return NatsContainerSpec{}, err
	}
	monitor, err := loopbackEndpoint("NatsMonitorPort", cfg.NatsMonitorPort)
	if err != nil {
		return NatsContainerSpec{}, err
	}
	return NatsContainerSpec{
		Name:            natsContainerName(cfg.StateDir),
		Image:           cfg.NatsImage,
		ConfigDir:       filepath.Join(cfg.StateDir, "nats-config"),
		ConfigYAML:      natsConfigYAML(),
		DataDir:         filepath.Join(cfg.StateDir, "nats"),
		ClientEndpoint:  client,
		MonitorEndpoint: monitor,
		StopTimeout:     natsStopTimeout,
	}, nil
}

// natsURL selects the supplied external URL or bundled loopback endpoint.
func natsURL(cfg Config) string {
	if cfg.ExternalNatsURL != "" {
		return cfg.ExternalNatsURL
	}
	return "nats://127.0.0.1:" + strconv.Itoa(cfg.NatsClientPort)
}

// natsContainerName is deterministic per state dir for detached teardown; its prefix distinguishes it in podman.
func natsContainerName(stateDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(stateDir)))
	return "compass-nats-" + hex.EncodeToString(sum[:6])
}

// natsConfigYAML enables the container-local client and monitoring listeners plus persistent JetStream storage.
func natsConfigYAML() string {
	return `host: "0.0.0.0"
port: ` + NatsContainerClientPort + `

http: "0.0.0.0:` + NatsContainerMonitorPort + `"

jetstream {
  store_dir: "` + NatsStoreDir + `"
  sync_interval: "100ms"
}
`
}
