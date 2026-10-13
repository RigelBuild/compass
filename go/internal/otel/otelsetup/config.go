// Package otelsetup is the only place a TracerProvider or MeterProvider is
// constructed for the Compass backend (compass-server and compass-runner), so
// the repo keeps exactly one tracer/exporter posture. It is split from
// internal/otel because it links the OTel SDK and OTLP exporters; binaries
// that install no provider (the compass CLI) import only internal/otel.
//
// Off by default, endpoint-gated: when cfg.Endpoint (the
// OTEL_EXPORTER_OTLP_ENDPOINT value) is empty, Setup* installs NO global
// provider and returns a non-nil no-op shutdown — zero overhead, no network
// egress — mirroring the agent's src/transport/otel-layer.ts. When the
// endpoint is set, exporters read OTEL_EXPORTER_OTLP_ENDPOINT themselves
// (standard OTLP base-endpoint per-signal path suffixing), so cfg.Endpoint
// serves purely as the on/off switch and is never passed as an explicit URL.
package otelsetup

// Config carries the per-binary identity and the endpoint gate.
type Config struct {
	ServiceName    string // "compass-server" | "compass-runner"
	ServiceVersion string // each main's ldflags var version
	Endpoint       string // OTEL_EXPORTER_OTLP_ENDPOINT; empty = disabled
}

// enabled reports whether an export pipeline should be installed.
func (c Config) enabled() bool {
	return c.Endpoint != ""
}
