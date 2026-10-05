//go:build unix

package stack

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"time"
)

// collectorStopTimeout is the graceful-stop budget pinned into the collector
// container's run spec as `--stop-timeout` (the safe default for any `podman
// stop` that passes no explicit `-t`). The collector holds no on-disk state to
// drain (D3: it drops rather than buffering to disk), so it needs no long grace
// like postgres's cluster shutdown — a short budget is ample for the receivers
// to close and the process to exit. Distinct from postgres's 30s
// containerStopTimeout because the two components have different shutdown costs.
const collectorStopTimeout = 10 * time.Second

// The collector's fixed container-internal ports (D3): OTLP grpc and http, and
// the health_check probe target. The generated config binds them inside the
// container and the adapter publishes them, so both share these upstream defaults.
const (
	CollectorContainerGRPCPort   = "4317"
	CollectorContainerHTTPPort   = "4318"
	CollectorContainerHealthPort = "13133"
)

// DefaultCollector*Port are the host loopback ports the CLI publishes on unless
// told otherwise: today's fixed values. The collector publishes on loopback only
// (loopbackEndpoint); remote ingestion is --otel-external.
const (
	DefaultCollectorGRPCPort   = 4317
	DefaultCollectorHTTPPort   = 4318
	DefaultCollectorHealthPort = 13133
)

// CollectorContainerSpec is the fully-resolved description of the Plane-B fan-in
// OTel Collector container child (T4, D3). The core builds it from Config; the
// adapter translates it into the `podman run` argv and writes the generated
// config file — the same core-builds-spec / adapter-runs-it split
// PostgresContainerSpec uses, so the flag/env/config set is a pure,
// unit-testable value rather than argv assembled behind the seam.
type CollectorContainerSpec struct {
	// Name is the stable per-state-dir container name (derived from StateDir),
	// the teardown identity a fresh `down` reconstructs and the v2 pgid record
	// persists. Unique per state dir so concurrent stacks never collide in
	// podman's flat container namespace; the host ports below come from Config.
	Name string
	// Image is the collector image ref to run (Config.CollectorImage; the pinned
	// DefaultCollectorImage on the installed path).
	Image string
	// ConfigDir is the host directory the generated collector config is written
	// into and bind-mounted (read-only) from, fixed under the state dir
	// (<StateDir>/collector). The adapter creates it and writes ConfigYAML into
	// <ConfigDir>/config.yaml before the run.
	ConfigDir string
	// ConfigYAML is the fully-rendered collector config realizing the D3 default
	// posture (otlp receiver, drop sink, health_check extension, no disk
	// buffering). The core renders it so the posture is a pure, unit-tested
	// value; the adapter only writes it to disk.
	ConfigYAML string
	// GRPCEndpoint is the host loopback endpoint the OTLP/grpc receiver is
	// published on (host:port). It is what OTEL_EXPORTER_OTLP_ENDPOINT points at
	// for grpc emitters.
	GRPCEndpoint string
	// HTTPEndpoint is the host loopback endpoint the OTLP/http receiver is
	// published on (host:port).
	HTTPEndpoint string
	// HealthEndpoint is the host loopback endpoint the health_check extension is
	// published on (host:port); the readiness probe issues an HTTP GET against
	// it.
	HealthEndpoint string
	// StopTimeout is the `--stop-timeout` pinned into the run (collectorStopTimeout).
	StopTimeout time.Duration
}

// collectorContainerSpec builds the collector run spec from the resolved config:
// name and config dir from the state dir, the D3 config, and loopback endpoints
// from Config's host ports. It does no I/O and refuses an incomplete config.
func collectorContainerSpec(cfg Config) (CollectorContainerSpec, error) {
	if cfg.StateDir == "" {
		return CollectorContainerSpec{}, errors.New("stack config: StateDir is required for the collector container (config bind-mount + name derivation)")
	}
	if cfg.CollectorImage == "" {
		return CollectorContainerSpec{}, errors.New("stack config: CollectorImage is required to bundle the collector (set --collector-image or use --otel-external to opt out)")
	}
	var eps [3]string
	for i, p := range []struct {
		field string
		port  int
	}{
		{"CollectorGRPCPort", cfg.CollectorGRPCPort},
		{"CollectorHTTPPort", cfg.CollectorHTTPPort},
		{"CollectorHealthPort", cfg.CollectorHealthPort},
	} {
		ep, err := loopbackEndpoint(p.field, p.port)
		if err != nil {
			return CollectorContainerSpec{}, err
		}
		eps[i] = ep
	}
	return CollectorContainerSpec{
		Name:           collectorContainerName(cfg.StateDir),
		Image:          cfg.CollectorImage,
		ConfigDir:      filepath.Join(cfg.StateDir, "collector"),
		ConfigYAML:     collectorConfigYAML(),
		GRPCEndpoint:   eps[0],
		HTTPEndpoint:   eps[1],
		HealthEndpoint: eps[2],
		StopTimeout:    collectorStopTimeout,
	}, nil
}

// collectorContainerName derives the stable per-state-dir collector container
// name. Like containerName (the postgres derivation) it is a deterministic
// function of the state dir alone so a fresh `down` with no in-memory handle
// reconstructs the same name, and the hash keeps concurrent stacks on different
// state dirs from colliding in podman's flat container namespace. A
// distinct prefix from the postgres name keeps the two components' containers
// legible apart in `podman ps`.
func collectorContainerName(stateDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(stateDir)))
	return "compass-otel-collector-" + hex.EncodeToString(sum[:6])
}

// collectorConfigYAML renders the collector config realizing the D3 default
// posture. It is a pure function (the endpoints are container-internal fixed
// ports) so the posture is unit-tested directly:
//
//   - receivers.otlp: grpc + http, the fan-in endpoint, bound 0.0.0.0 inside the
//     container (the run spec publishes the ports on the host loopback).
//   - exporters.nop: the drop sink. D3's "exports NOWHERE until an export
//     endpoint is configured" — there is no live exporter in the default config,
//     so received telemetry is accepted and dropped, never egressed and never
//     buffered. Configuring an export backend is a future config addition (out of
//     scope for the D3 default; --otel-external skips the bundled collector
//     entirely).
//   - extensions.health_check: :13133, the readiness probe target.
//   - service.pipelines: traces + metrics + logs, each otlp -> nop. No
//     sending_queue and no file_storage anywhere — D3's "drops rather than
//     buffering to disk". A live self-hoster gets a receiving endpoint with zero
//     sink-fill risk.
func collectorConfigYAML() string {
	return `receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:` + CollectorContainerGRPCPort + `
      http:
        endpoint: 0.0.0.0:` + CollectorContainerHTTPPort + `

exporters:
  nop: {}

extensions:
  health_check:
    endpoint: 0.0.0.0:` + CollectorContainerHealthPort + `

service:
  extensions: [health_check]
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [nop]
    metrics:
      receivers: [otlp]
      exporters: [nop]
    logs:
      receivers: [otlp]
      exporters: [nop]
`
}
