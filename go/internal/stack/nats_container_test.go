//go:build unix

package stack

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestNatsContainerSpecBuildsFromConfig pins the nats spec builder: image
// passthrough, the two DISTINCT dirs under the state dir (a read-only config dir
// and a read-write JetStream data dir), the fixed loopback client + monitor
// endpoints, the stop timeout, and a stable derived name.
func TestNatsContainerSpecBuildsFromConfig(t *testing.T) {
	cfg := Config{
		StateDir:  "/state",
		NatsImage: "docker.io/library/nats@sha256:abc",
	}
	spec, err := natsContainerSpec(cfg)
	if err != nil {
		t.Fatalf("natsContainerSpec() = %v, want nil", err)
	}
	if spec.Image != cfg.NatsImage {
		t.Errorf("spec.Image = %q, want %q", spec.Image, cfg.NatsImage)
	}
	if spec.ConfigDir != "/state/nats-config" {
		t.Errorf("spec.ConfigDir = %q, want /state/nats-config", spec.ConfigDir)
	}
	if spec.DataDir != "/state/nats" {
		t.Errorf("spec.DataDir = %q, want /state/nats", spec.DataDir)
	}
	// The read-only config mount and the read-write JetStream store must never
	// be the same host dir, or the config bind would land inside the store (or
	// vice versa) and one of the two mounts would shadow the other.
	if spec.ConfigDir == spec.DataDir {
		t.Errorf("spec.ConfigDir and spec.DataDir are the same path %q; the ro config mount must not overlap the rw JetStream store", spec.ConfigDir)
	}
	if spec.ClientEndpoint != "127.0.0.1:4222" {
		t.Errorf("spec.ClientEndpoint = %q, want 127.0.0.1:4222", spec.ClientEndpoint)
	}
	if spec.MonitorEndpoint != "127.0.0.1:8222" {
		t.Errorf("spec.MonitorEndpoint = %q, want 127.0.0.1:8222", spec.MonitorEndpoint)
	}
	if spec.StopTimeout != natsStopTimeout {
		t.Errorf("spec.StopTimeout = %v, want %v", spec.StopTimeout, natsStopTimeout)
	}
	if spec.Name != natsContainerName(cfg.StateDir) {
		t.Errorf("spec.Name = %q, want the derived name %q", spec.Name, natsContainerName(cfg.StateDir))
	}
	if spec.ConfigYAML == "" {
		t.Error("spec.ConfigYAML is empty; want the rendered nats-server config")
	}
}

// TestNatsContainerSpecRejectsMissingStateDir pins that a config with no state
// dir is a hard error, not a run against a half-formed spec (the state dir is
// the root of both bind-mounts and the name-derivation input).
func TestNatsContainerSpecRejectsMissingStateDir(t *testing.T) {
	_, err := natsContainerSpec(Config{NatsImage: "img:pinned"})
	if err == nil {
		t.Fatal("natsContainerSpec(no StateDir) = nil error, want a rejection")
	}
	if !strings.Contains(err.Error(), "StateDir") {
		t.Fatalf("error %q does not mention StateDir", err.Error())
	}
}

// TestNatsContainerSpecRejectsMissingImage pins that a bundle-path config with
// no nats image is a hard error at spec time, not an opaque `podman run ""` deep
// in the adapter. NATS is container-only (no process fallback like postgres), so
// an empty image is always invalid; a struct-literal Config that leaves NatsImage
// empty without opting out via ExternalNatsURL must be rejected here.
func TestNatsContainerSpecRejectsMissingImage(t *testing.T) {
	_, err := natsContainerSpec(Config{StateDir: "/state"})
	if err == nil {
		t.Fatal("natsContainerSpec(no NatsImage) = nil error, want a rejection")
	}
	if !strings.Contains(err.Error(), "NatsImage") {
		t.Fatalf("error %q does not mention NatsImage", err.Error())
	}
}

// TestNatsContainerNameDeterministicPerStateDir pins the stable-name contract:
// the name is a pure function of the state dir (so a fresh down reconstructs
// it), distinct across state dirs (so concurrent stacks never collide),
// clean-normalized, and carries the nats-specific prefix (so it is legible apart
// from the postgres and collector containers in `podman ps`).
func TestNatsContainerNameDeterministicPerStateDir(t *testing.T) {
	a1 := natsContainerName("/state/a")
	a2 := natsContainerName("/state/a")
	b := natsContainerName("/state/b")
	if a1 != a2 {
		t.Fatalf("natsContainerName not deterministic: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Fatalf("natsContainerName collides across state dirs: both %q", a1)
	}
	if !strings.HasPrefix(a1, "compass-nats-") {
		t.Fatalf("natsContainerName %q missing the compass-nats- prefix", a1)
	}
	if natsContainerName("/state/a/") != a1 {
		t.Fatalf("natsContainerName not clean-normalized: %q vs %q", natsContainerName("/state/a/"), a1)
	}
	// Distinct from the other two container components' names for the same state
	// dir, so the three never collide in podman's flat namespace.
	if a1 == containerName("/state/a") || a1 == collectorContainerName("/state/a") {
		t.Fatalf("nats container name collides with postgres/collector for the same state dir: %q", a1)
	}
}

// TestNatsConfigYAMLRealizesJetStreamPosture is the record-compliance gate: the
// generated nats-server config MUST enable JetStream with a store_dir on the
// read-write bind-mount and the record's bounded-fsync sync_interval of 100ms
// (design.md:363). sync_interval is a SERVER setting, not a per-stream
// jetstream.StreamConfig field, so this config is the only place the value can
// live — a regression dropping it here silently reverts every stream to the
// server default fsync cadence, with no error anywhere. The config must also
// carry the client port and the HTTP monitoring endpoint the readiness probe
// hits.
func TestNatsConfigYAMLRealizesJetStreamPosture(t *testing.T) {
	conf := natsConfigYAML()

	// JetStream on, storing into the read-write bind-mount target.
	if !strings.Contains(conf, "jetstream {") {
		t.Errorf("config missing the jetstream block:\n%s", conf)
	}
	if !strings.Contains(conf, `store_dir: "`+NatsStoreDir+`"`) {
		t.Errorf("config missing the JetStream store_dir at the mount target %q:\n%s", NatsStoreDir, conf)
	}
	// The Jepsen-driven bounded-fsync value. Asserted as the literal setting, not
	// just the substring "100ms", so a stray 100ms elsewhere could not satisfy it.
	if !strings.Contains(conf, `sync_interval: "100ms"`) {
		t.Errorf("config missing sync_interval: \"100ms\" (design.md:363, the bounded-fsync value):\n%s", conf)
	}

	// The client listener and the monitoring endpoint the probe GETs /healthz on.
	if !strings.Contains(conf, "port: "+natsClientPort) {
		t.Errorf("config missing the client port %s:\n%s", natsClientPort, conf)
	}
	if !strings.Contains(conf, `http: "0.0.0.0:`+natsMonitorPort+`"`) {
		t.Errorf("config missing the http monitoring endpoint on %s:\n%s", natsMonitorPort, conf)
	}

	// Out of scope for this shape, and each would be a silent posture change:
	// clustering (T5) and any authorization/credentials block (the RIG-2861 auth
	// seam). Their absence is the contract, so assert it directly.
	for _, forbidden := range []string{"cluster", "authorization", "accounts", "operator"} {
		if strings.Contains(conf, forbidden) {
			t.Errorf("config contains forbidden %q — single-node, no-auth is this shape's posture:\n%s", forbidden, conf)
		}
	}
}

// TestExternalNatsSkipsNats pins the --nats-external opt-out at the spawn-chain
// gate: with ExternalNatsURL set, Up starts NO nats component (the NatsContainer
// seam is never touched) and nothing nats-shaped is recorded for teardown. The
// rest of the cold chain is unchanged.
func TestExternalNatsSkipsNats(t *testing.T) {
	cfg, h := newHarness(t)
	cfg.ExternalNatsURL = "nats://nats.example.com:4222"

	s, err := Up(context.Background(), cfg, h.deps)
	if err != nil {
		t.Fatalf("Up() = %v, want nil", err)
	}
	if s.attached {
		t.Fatal("cold Up should not be attached")
	}
	if h.nats.started != 0 {
		t.Fatalf("NatsContainer.Start called %d times on the --nats-external path, want 0", h.nats.started)
	}
	rec, rerr := readPgidFile(cfg.StateDir)
	if rerr != nil {
		t.Fatalf("readPgidFile = %v", rerr)
	}
	for _, e := range rec.Entries {
		if e.Component == ComponentNats {
			t.Fatalf("nats entry recorded on the --nats-external path: %+v", e)
		}
	}
}

// TestNatsStartRecordsContainerEntry pins the bundled-nats start path: with
// ExternalNatsURL empty (the default), Up starts nats via the NatsContainer
// seam, probes its monitoring endpoint, and records a v2 container-kind pgid
// entry keyed by the stable per-state-dir name — the teardown identity a fresh
// down reconstructs. An in-process Down then drains it.
func TestNatsStartRecordsContainerEntry(t *testing.T) {
	cfg, h := newHarness(t)
	cfg.NatsImage = "docker.io/library/nats@sha256:abc"

	s, err := Up(context.Background(), cfg, h.deps)
	if err != nil {
		t.Fatalf("Up() = %v, want nil", err)
	}
	if h.nats.started != 1 {
		t.Fatalf("NatsContainer.Start called %d times, want 1", h.nats.started)
	}
	// The readiness probe ran against the spec's monitor endpoint — not the
	// client endpoint, which speaks no HTTP.
	spec := h.nats.spec()
	if got := h.natsProber.lastEndpoint(); got != spec.MonitorEndpoint {
		t.Fatalf("NatsProber probed %q, want the spec monitor endpoint %q", got, spec.MonitorEndpoint)
	}

	rec, rerr := readPgidFile(cfg.StateDir)
	if rerr != nil {
		t.Fatalf("readPgidFile = %v", rerr)
	}
	var entry *pgidEntry
	for i := range rec.Entries {
		if rec.Entries[i].Component == ComponentNats {
			entry = &rec.Entries[i]
		}
	}
	if entry == nil {
		t.Fatal("no nats entry recorded on the bundled path")
	}
	if entry.Kind != entryContainer {
		t.Errorf("nats entry kind = %v, want entryContainer", entry.Kind)
	}
	if entry.ContainerName != natsContainerName(cfg.StateDir) {
		t.Errorf("recorded nats name = %q, want %q", entry.ContainerName, natsContainerName(cfg.StateDir))
	}
	if entry.Pgid != 0 || entry.StartTime != 0 {
		t.Errorf("nats entry carries pgid/starttime %d/%d, want zero (torn down by name)", entry.Pgid, entry.StartTime)
	}

	if err := s.Down(context.Background()); err != nil {
		t.Fatalf("Down() = %v", err)
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// TestNatsStartFailureDrains pins the failure surface on the nats path: a nats
// container that fails to launch surfaces the error and leaves no half-started
// stack (compass-server never started after it).
func TestNatsStartFailureDrains(t *testing.T) {
	cfg, h := newHarness(t)
	h.nats.startErr = errNotAnswering

	if _, err := Up(context.Background(), cfg, h.deps); err == nil {
		t.Fatal("Up() = nil, want the nats start error")
	}
	for _, e := range h.rec.snapshot() {
		if e == "start compass-server" {
			t.Fatal("compass-server started after a failed nats start")
		}
	}
}

// TestNatsNeverReady pins the nats-readiness budget failure: nats launches but
// its health probe never answers, so waitNats times out with a legible error and
// the children started so far are drained — and crucially compass-server never
// starts against a not-ready broker.
func TestNatsNeverReady(t *testing.T) {
	cfg, h := newHarness(t)
	h.natsProber.never = true
	var ticks int
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h.deps.Now = func() time.Time {
		ticks++
		if ticks > 4 {
			return start.Add(natsReadyPollBudget + time.Second)
		}
		return start
	}

	s, err := Up(context.Background(), cfg, h.deps)
	if err == nil {
		t.Fatal("Up() = nil, want nats-readiness timeout")
	}
	if s != nil {
		t.Fatal("Up() returned a Stack; want nil")
	}
	if n := countEvent(h.rec.snapshot(), "start compass-server"); n != 0 {
		t.Fatalf("compass-server started %d times before nats was ready; want 0", n)
	}
	// The launched-but-never-ready nats must be drained on the failure path
	// (drainChildren owns s.nats): a regression dropping it from the drain list
	// would leak the container here.
	if n := countEvent(h.rec.snapshot(), "signal nats"); n != 1 {
		t.Fatalf("nats signalled %d times on the never-ready drain; want 1", n)
	}
	assertLockFree(t, cfg.StateDir)
}
