//go:build unix

package stack

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestCollectorContainerSpecBuildsFromConfig pins the T4 collector spec builder:
// image passthrough, config dir under the state dir, the fixed loopback OTLP +
// health endpoints, the stop timeout, and a stable derived name.
func TestCollectorContainerSpecBuildsFromConfig(t *testing.T) {
	cfg := Config{
		StateDir:       "/state",
		CollectorImage: "docker.io/otel/opentelemetry-collector-contrib@sha256:abc",
	}
	spec, err := collectorContainerSpec(cfg)
	if err != nil {
		t.Fatalf("collectorContainerSpec() = %v, want nil", err)
	}
	if spec.Image != cfg.CollectorImage {
		t.Errorf("spec.Image = %q, want %q", spec.Image, cfg.CollectorImage)
	}
	if spec.ConfigDir != "/state/collector" {
		t.Errorf("spec.ConfigDir = %q, want /state/collector", spec.ConfigDir)
	}
	if spec.GRPCEndpoint != "127.0.0.1:4317" {
		t.Errorf("spec.GRPCEndpoint = %q, want 127.0.0.1:4317", spec.GRPCEndpoint)
	}
	if spec.HTTPEndpoint != "127.0.0.1:4318" {
		t.Errorf("spec.HTTPEndpoint = %q, want 127.0.0.1:4318", spec.HTTPEndpoint)
	}
	if spec.HealthEndpoint != "127.0.0.1:13133" {
		t.Errorf("spec.HealthEndpoint = %q, want 127.0.0.1:13133", spec.HealthEndpoint)
	}
	if spec.StopTimeout != collectorStopTimeout {
		t.Errorf("spec.StopTimeout = %v, want %v", spec.StopTimeout, collectorStopTimeout)
	}
	if spec.Name != collectorContainerName(cfg.StateDir) {
		t.Errorf("spec.Name = %q, want the derived name %q", spec.Name, collectorContainerName(cfg.StateDir))
	}
	if spec.ConfigYAML == "" {
		t.Error("spec.ConfigYAML is empty; want the rendered D3-posture config")
	}
}

// TestCollectorContainerSpecRejectsMissingStateDir pins that a config with no
// state dir is a hard error, not a run against a half-formed spec (the state dir
// is both the config bind-mount root and the name-derivation input).
func TestCollectorContainerSpecRejectsMissingStateDir(t *testing.T) {
	_, err := collectorContainerSpec(Config{CollectorImage: "img:pinned"})
	if err == nil {
		t.Fatal("collectorContainerSpec(no StateDir) = nil error, want a rejection")
	}
	if !strings.Contains(err.Error(), "StateDir") {
		t.Fatalf("error %q does not mention StateDir", err.Error())
	}
}

// TestCollectorContainerNameDeterministicPerStateDir pins the stable-name
// contract: the name is a pure function of the state dir (so a fresh down
// reconstructs it), distinct across state dirs (so concurrent stacks never
// collide), clean-normalized, and carries the collector-specific prefix (so it
// is legible apart from the postgres container in `podman ps`).
func TestCollectorContainerNameDeterministicPerStateDir(t *testing.T) {
	a1 := collectorContainerName("/state/a")
	a2 := collectorContainerName("/state/a")
	b := collectorContainerName("/state/b")
	if a1 != a2 {
		t.Fatalf("collectorContainerName not deterministic: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Fatalf("collectorContainerName collides across state dirs: both %q", a1)
	}
	if !strings.HasPrefix(a1, "compass-otel-collector-") {
		t.Fatalf("collectorContainerName %q missing the compass-otel-collector- prefix", a1)
	}
	if collectorContainerName("/state/a/") != a1 {
		t.Fatalf("collectorContainerName not clean-normalized: %q vs %q", collectorContainerName("/state/a/"), a1)
	}
	// Distinct from the postgres container name for the same state dir, so the
	// two components never collide in podman's flat namespace.
	if a1 == containerName("/state/a") {
		t.Fatalf("collector and postgres container names collide for the same state dir: %q", a1)
	}
}

// TestCollectorConfigYAMLRealizesD3Posture pins the D3 default posture in the
// generated config: an otlp receiver (grpc + http), the health_check extension
// on :13133, traces/metrics/logs pipelines, and — critically — NO disk
// buffering (no file_storage, no sending_queue, no persistent queue) and no live
// export exporter (only the nop drop sink). This is the "receives by default,
// exports nowhere, drops rather than buffers" contract.
func TestCollectorConfigYAMLRealizesD3Posture(t *testing.T) {
	yaml := collectorConfigYAML()

	// Receivers: otlp with both protocols.
	for _, want := range []string{"otlp:", "grpc:", "http:", "0.0.0.0:4317", "0.0.0.0:4318"} {
		if !strings.Contains(yaml, want) {
			t.Errorf("config missing otlp receiver marker %q:\n%s", want, yaml)
		}
	}
	// The drop sink and the pipelines that reference it.
	if !strings.Contains(yaml, "nop:") {
		t.Errorf("config missing the nop drop sink:\n%s", yaml)
	}
	for _, pipe := range []string{"traces:", "metrics:", "logs:"} {
		if !strings.Contains(yaml, pipe) {
			t.Errorf("config missing %q pipeline:\n%s", pipe, yaml)
		}
	}
	// The health_check extension on the probe port, referenced by the service.
	if !strings.Contains(yaml, "health_check:") || !strings.Contains(yaml, "0.0.0.0:13133") {
		t.Errorf("config missing the health_check extension on :13133:\n%s", yaml)
	}
	if !strings.Contains(yaml, "extensions: [health_check]") {
		t.Errorf("config does not enable the health_check extension in service.extensions:\n%s", yaml)
	}

	// The load-bearing D3 negatives: NO disk buffering of any kind, NO live
	// exporter. A future "bundle AND export" posture would add these — their
	// absence is the default-posture contract, so assert it directly.
	for _, forbidden := range []string{"file_storage", "sending_queue", "persistent", "otlphttp", "otlp/", "queue"} {
		if strings.Contains(yaml, forbidden) {
			t.Errorf("config contains forbidden %q — D3 default drops rather than buffers/exports:\n%s", forbidden, yaml)
		}
	}
}

// TestExternalOTLPSkipsCollector pins the D3 --otel-external opt-out at the
// spawn-chain gate: with ExternalOTLPEndpoint set, Up starts NO collector
// component (the CollectorContainer seam is never touched) and nothing
// collector-shaped is recorded for teardown. The rest of the cold chain is
// unchanged.
func TestExternalOTLPSkipsCollector(t *testing.T) {
	cfg, h := newHarness(t)
	cfg.ExternalOTLPEndpoint = "otlp.example.com:4317"
	// A collector seam is wired but MUST NOT be touched on the external path.
	col := newFakeCollectorContainer(h.rec)
	h.deps.CollectorContainer = col

	s, err := Up(context.Background(), cfg, h.deps)
	if err != nil {
		t.Fatalf("Up() = %v, want nil", err)
	}
	if s.attached {
		t.Fatal("cold Up should not be attached")
	}
	if col.started != 0 {
		t.Fatalf("CollectorContainer.Start called %d times on the --otel-external path, want 0", col.started)
	}
	// Nothing collector-shaped recorded, so a down tears down only the other
	// children.
	rec, rerr := readPgidFile(cfg.StateDir)
	if rerr != nil {
		t.Fatalf("readPgidFile = %v", rerr)
	}
	for _, e := range rec.Entries {
		if e.Component == ComponentCollector {
			t.Fatalf("collector entry recorded on the --otel-external path: %+v", e)
		}
	}
}

// TestCollectorStartRecordsContainerEntry pins the bundled-collector start path:
// with ExternalOTLPEndpoint empty (the D3 default), Up starts the collector via
// the CollectorContainer seam, probes its health endpoint, and records a v2
// container-kind pgid entry keyed by the stable per-state-dir name — the
// teardown identity a fresh down reconstructs. An in-process Down then drains it
// in reverse order.
func TestCollectorStartRecordsContainerEntry(t *testing.T) {
	cfg, h := newHarness(t)
	cfg.CollectorImage = "docker.io/otel/opentelemetry-collector-contrib@sha256:abc"

	s, err := Up(context.Background(), cfg, h.deps)
	if err != nil {
		t.Fatalf("Up() = %v, want nil", err)
	}
	if h.collector.started != 1 {
		t.Fatalf("CollectorContainer.Start called %d times, want 1", h.collector.started)
	}
	// The health probe ran against the spec's health endpoint.
	spec := h.collector.spec()
	if got := h.collectorProber.lastEndpoint(); got != spec.HealthEndpoint {
		t.Fatalf("CollectorProber probed %q, want the spec health endpoint %q", got, spec.HealthEndpoint)
	}

	// The persisted record carries the collector as a v2 container entry keyed
	// by the derived name — never a pgid.
	rec, rerr := readPgidFile(cfg.StateDir)
	if rerr != nil {
		t.Fatalf("readPgidFile = %v", rerr)
	}
	var col *pgidEntry
	for i := range rec.Entries {
		if rec.Entries[i].Component == ComponentCollector {
			col = &rec.Entries[i]
		}
	}
	if col == nil {
		t.Fatal("no collector entry recorded on the bundled path")
	}
	if col.Kind != entryContainer {
		t.Errorf("collector entry kind = %v, want entryContainer", col.Kind)
	}
	if col.ContainerName != collectorContainerName(cfg.StateDir) {
		t.Errorf("recorded collector name = %q, want %q", col.ContainerName, collectorContainerName(cfg.StateDir))
	}
	if col.Pgid != 0 || col.StartTime != 0 {
		t.Errorf("collector entry carries pgid/starttime %d/%d, want zero (torn down by name)", col.Pgid, col.StartTime)
	}

	if err := s.Down(context.Background()); err != nil {
		t.Fatalf("Down() = %v", err)
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// TestCollectorStartFailureDrains pins the failure surface on the collector
// path: a collector that fails to launch surfaces the error and leaves no
// half-started stack (compass-server never started after it).
func TestCollectorStartFailureDrains(t *testing.T) {
	cfg, h := newHarness(t)
	h.collector.startErr = errNotAnswering

	if _, err := Up(context.Background(), cfg, h.deps); err == nil {
		t.Fatal("Up() = nil, want the collector start error")
	}
	for _, e := range h.rec.snapshot() {
		if e == "start compass-server" {
			t.Fatal("compass-server started after a failed collector start")
		}
	}
}

// TestCollectorNeverReady pins the collector-readiness budget failure: the
// collector launches but its health probe never answers, so waitCollector times
// out with a legible error and the children started so far are drained — and
// crucially compass-server never starts against a not-ready collector.
func TestCollectorNeverReady(t *testing.T) {
	cfg, h := newHarness(t)
	h.collectorProber.never = true
	var ticks int
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h.deps.Now = func() time.Time {
		ticks++
		if ticks > 3 {
			return start.Add(collectorReadyPollBudget + time.Second)
		}
		return start
	}

	s, err := Up(context.Background(), cfg, h.deps)
	if err == nil {
		t.Fatal("Up() = nil, want collector-readiness timeout")
	}
	if s != nil {
		t.Fatal("Up() returned a Stack; want nil")
	}
	if n := countEvent(h.rec.snapshot(), "start compass-server"); n != 0 {
		t.Fatalf("compass-server started %d times before the collector was ready; want 0", n)
	}
	// The launched-but-never-ready collector must be drained on the failure path
	// (drainChildren owns s.collector): a regression dropping it from the drain
	// list would leak the container here.
	if n := countEvent(h.rec.snapshot(), "signal otel-collector"); n != 1 {
		t.Fatalf("collector signalled %d times on the never-ready drain; want 1", n)
	}
	assertLockFree(t, cfg.StateDir)
}
