//go:build unix

package stack

// DefaultCollectorImage is the container image the Plane-B fan-in OTel Collector
// component (T4, D3) runs by default: the upstream
// otel/opentelemetry-collector-contrib distribution, pinned BY DIGEST. The
// contrib build (not the slim core) is chosen for headroom on the receivers and
// processors a fan-in router grows into (e.g. tail-sampling), matching the D3
// prior-art shape without prejudging the RIG-2793/SF1 heavy-backend question.
//
// The pin is by DIGEST, not a mutable tag, for the same reason
// DefaultPostgresImage pins postgres: the collector is a supervised component of
// the installed stack, and its config-schema and OTLP wire behavior are
// version-sensitive, so a mutable tag would ship an unreviewed collector under
// the installed stack. This is a Go const Renovate cannot see, so it moves only
// via a reviewed manual PR.
//
// Bump procedure: advance the digest below when the collector version moves,
// then re-run the T4 collector integration test (up -> health :13133 answers ->
// fresh-process down -> container gone) against the new digest before landing,
// and re-validate the generated config against the new image
// (`otelcol-contrib validate --config <generated>`), since the config schema can
// drift between collector versions.
//
// Resolution provenance: this is the multi-arch image index digest for
// otel/opentelemetry-collector-contrib:0.140.0, resolved from the Docker Hub
// registry (docker-content-digest for the manifest-list media type) and verified
// to pull and boot in-environment on 2026-08-26.
const DefaultCollectorImage = "docker.io/otel/opentelemetry-collector-contrib@sha256:ffc818ee108d0b934fd14207fd87ff247c9b64f344ed349d0b66c166c18a2312"
