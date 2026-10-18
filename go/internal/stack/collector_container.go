//go:build unix

package stack

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"time"
)

// collectorStopTimeout gives the stateless collector a short graceful-stop budget; postgres needs longer for cluster shutdown.
const collectorStopTimeout = 10 * time.Second

// These fixed container ports are shared by the generated config and adapter publish flags.
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

// CollectorContainerSpec resolves the config, endpoints, and mount paths for the bundled collector.
type CollectorContainerSpec struct {
	// Name is stable per state dir and persisted for detached teardown.
	Name string
	// Image is the collector image ref, normally the pinned DefaultCollectorImage.
	Image string
	// ConfigDir holds generated config mounted read-only; the adapter creates and writes it before launch.
	ConfigDir string
	// ConfigYAML renders the receive-and-drop posture; the adapter writes this pure, testable value to disk.
	ConfigYAML string
	// GRPCEndpoint is the host loopback receiver published for grpc emitters.
	GRPCEndpoint string
	// HTTPEndpoint is the host loopback receiver published for HTTP emitters.
	HTTPEndpoint string
	// HealthEndpoint is where readiness probes the host-published health_check extension.
	HealthEndpoint string
	// StopTimeout is the `--stop-timeout` pinned into the run (collectorStopTimeout).
	StopTimeout time.Duration
}

// collectorContainerSpec resolves the name, state-dir config, generated posture, and loopback endpoints without I/O.
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

// collectorContainerName is stable per state dir for detached teardown; its hash avoids podman name collisions.
func collectorContainerName(stateDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(stateDir)))
	return "compass-otel-collector-" + hex.EncodeToString(sum[:6])
}

// collectorConfigYAML exposes OTLP receivers and health checks while sending every signal to the nop exporter.
// It intentionally has no queue, file storage, or data mount: received telemetry is dropped, not persisted or exported.
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
