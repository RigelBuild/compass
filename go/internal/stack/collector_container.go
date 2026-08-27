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

// The collector's fixed listen ports (D3): OTLP grpc + http are the fan-in
// endpoint compass surfaces emit to, and the health_check extension is the
// readiness probe target. These are container-internal ports the generated
// config binds on 0.0.0.0; the run spec publishes them on the host loopback so
// on-host surfaces (and, in T4b, server/runner) can reach the OTLP endpoint and
// the readiness poll can reach health. They are the upstream collector defaults,
// kept literal so the generated config and the published ports never drift.
const (
	collectorGRPCPort   = "4317"
	collectorHTTPPort   = "4318"
	collectorHealthPort = "13133"
	// collectorListenHost is the host interface the ports are published on: the
	// loopback only, never 0.0.0.0 — the bundled collector is a local fan-in
	// endpoint for on-host surfaces, not a network-exposed service. An operator
	// wanting remote ingestion runs --otel-external and points at their own.
	collectorListenHost = "127.0.0.1"
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
	// podman's flat container namespace. Note this scopes only the container
	// name: the loopback ports below publish on fixed host ports (like postgres's
	// default 5432), so two stacks on one host still contend for those binds —
	// the embedded stack is one-per-host by design.
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

// collectorContainerSpec builds the T4 collector run spec from the resolved
// config: it derives the stable container name and config dir from the state
// dir, renders the D3-posture config, and fixes the published loopback
// endpoints. It is pure (no I/O) so the config and endpoint set it encodes is
// unit-tested directly, and it errors on a config missing the state dir the
// container's config bind-mount and name derivation need rather than running
// podman against a half-formed spec.
func collectorContainerSpec(cfg Config) (CollectorContainerSpec, error) {
	if cfg.StateDir == "" {
		return CollectorContainerSpec{}, errors.New("stack config: StateDir is required for the collector container (config bind-mount + name derivation)")
	}
	return CollectorContainerSpec{
		Name:           collectorContainerName(cfg.StateDir),
		Image:          cfg.CollectorImage,
		ConfigDir:      filepath.Join(cfg.StateDir, "collector"),
		ConfigYAML:     collectorConfigYAML(),
		GRPCEndpoint:   collectorListenHost + ":" + collectorGRPCPort,
		HTTPEndpoint:   collectorListenHost + ":" + collectorHTTPPort,
		HealthEndpoint: collectorListenHost + ":" + collectorHealthPort,
		StopTimeout:    collectorStopTimeout,
	}, nil
}

// collectorContainerName derives the stable per-state-dir collector container
// name. Like containerName (the postgres derivation) it is a deterministic
// function of the state dir alone so a fresh `down` with no in-memory handle
// reconstructs the same name, and the hash keeps concurrent stacks on different
// state dirs from colliding in podman's flat container namespace (the host-port
// binds are fixed, so multi-stack-per-host is out of scope either way). A
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
        endpoint: 0.0.0.0:` + collectorGRPCPort + `
      http:
        endpoint: 0.0.0.0:` + collectorHTTPPort + `

exporters:
  nop: {}

extensions:
  health_check:
    endpoint: 0.0.0.0:` + collectorHealthPort + `

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
