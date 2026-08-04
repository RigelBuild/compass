//go:build unix

package stack

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recorder is a concurrency-safe ordered event log the stub seams append to, so
// tests can assert the spawn/ensure sequence.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.events = append(r.events, s)
	r.mu.Unlock()
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

// stubProcess is a started child that records its stop signals in start-order so
// drain order is assertable.
type stubProcess struct {
	name string
	rec  *recorder
}

func (p *stubProcess) Signal(sig ProcessSignal) error {
	p.rec.add("signal " + p.name)
	return nil
}

func (p *stubProcess) Wait(ctx context.Context) error {
	p.rec.add("wait " + p.name)
	return nil
}

// stubSupervisor records each Start, can inject a per-component start error, can
// gate a component's Start on a channel (for the concurrency race), and flips
// serverStarted when compass-server launches (which the prober reads as "now
// answering").
type stubSupervisor struct {
	rec           *recorder
	startErr      map[Component]error
	gate          map[Component]chan struct{}
	entered       map[Component]chan struct{}
	serverStarted *atomic.Bool
}

func (s *stubSupervisor) Start(ctx context.Context, spec ProcessSpec) (Process, error) {
	if ch, ok := s.entered[spec.Component]; ok {
		close(ch)
	}
	if g, ok := s.gate[spec.Component]; ok {
		<-g
	}
	if err := s.startErr[spec.Component]; err != nil {
		s.rec.add("start-failed " + spec.Component.String())
		return nil, err
	}
	s.rec.add("start " + spec.Component.String())
	if spec.Component == ComponentServer && s.serverStarted != nil {
		s.serverStarted.Store(true)
	}
	return &stubProcess{name: spec.Component.String(), rec: s.rec}, nil
}

// stubProber answers GetServerInfo. forceLive makes it answer unconditionally
// (attach-if-live); otherwise it answers only once compass-server has started,
// which models the socket-binds-before-migrations reality (readiness is the
// first answering probe, not socket existence).
type stubProber struct {
	rec           *recorder
	forceLive     bool
	version       string
	serverStarted *atomic.Bool
}

func (p *stubProber) Probe(ctx context.Context, socketPath string) (ServerInfo, error) {
	p.rec.add("probe")
	if p.forceLive || (p.serverStarted != nil && p.serverStarted.Load()) {
		return ServerInfo{Version: p.version}, nil
	}
	return ServerInfo{}, errNotAnswering
}

var errNotAnswering = &probeError{}

type probeError struct{}

func (*probeError) Error() string { return "server not answering" }

// stubDBProber answers the postgres-reachability probe. By default it is ready
// immediately (readyAfter == 0). readyAfter > 0 models a cold cluster: the first
// readyAfter probes report not-ready, then it flips ready — so a test can prove
// waitPostgres polls until postgres accepts. never makes it report not-ready
// forever, so a test can drive the budget-timeout failure via a controlled
// clock.
type stubDBProber struct {
	rec        *recorder
	readyAfter int
	never      bool
	calls      atomic.Int64
}

func (p *stubDBProber) ProbeDB(ctx context.Context, dsn string) error {
	p.rec.add("probe-db")
	n := p.calls.Add(1)
	if p.never {
		return errPostgresNotReady
	}
	if n <= int64(p.readyAfter) {
		return errPostgresNotReady
	}
	return nil
}

var errPostgresNotReady = &dbProbeError{}

type dbProbeError struct{}

func (*dbProbeError) Error() string { return "postgres not accepting connections" }

// stubCert models the expiry-aware anchor: it rotates iff now falls within
// rotateWindow of notAfter, and records the outcome so the rotation branch is
// assertable. err injects an ensure failure.
type stubCert struct {
	rec          *recorder
	notAfter     time.Time
	rotateWindow time.Duration
	err          error

	mu      sync.Mutex
	rotated bool
	called  bool
}

func (c *stubCert) EnsureCert(ctx context.Context, stateDir string, now time.Time) (CertResult, error) {
	c.rec.add("ensure-cert")
	if c.err != nil {
		return CertResult{}, c.err
	}
	rotate := now.After(c.notAfter.Add(-c.rotateWindow))
	c.mu.Lock()
	c.called = true
	c.rotated = rotate
	c.mu.Unlock()
	return CertResult{
		CertPath: filepath.Join(stateDir, "tls.crt"),
		KeyPath:  filepath.Join(stateDir, "tls.key"),
		Rotated:  rotate,
	}, nil
}

func (c *stubCert) didRotate() (called, rotated bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.called, c.rotated
}

// stubToken / stubImage record their calls and can inject an error. stubToken
// also records the runner id it was minted for, so a test can assert mint and
// spawn agree on one id.
type stubToken struct {
	rec      *recorder
	err      error
	mintedID string
}

func (t *stubToken) EnsureToken(ctx context.Context, stateDir, runnerID string) (string, error) {
	t.rec.add("ensure-token")
	t.mintedID = runnerID
	if t.err != nil {
		return "", t.err
	}
	return "runner-token-value", nil
}

type stubImage struct {
	rec *recorder
	err error
}

func (i *stubImage) EnsureImage(ctx context.Context, image string) error {
	i.rec.add("ensure-image")
	return i.err
}

// harness bundles the recorder, the shared serverStarted flag, and the stub
// seams so a test can tweak individual fields before calling Up.
type harness struct {
	rec           *recorder
	serverStarted *atomic.Bool
	sup           *stubSupervisor
	cert          *stubCert
	token         *stubToken
	image         *stubImage
	prober        *stubProber
	dbProber      *stubDBProber
	deps          Deps
}

const testVersion = "1.0.0"

func newHarness(t *testing.T) (Config, *harness) {
	t.Helper()
	rec := &recorder{}
	started := &atomic.Bool{}
	sup := &stubSupervisor{
		rec:           rec,
		startErr:      map[Component]error{},
		gate:          map[Component]chan struct{}{},
		entered:       map[Component]chan struct{}{},
		serverStarted: started,
	}
	cert := &stubCert{rec: rec, notAfter: time.Now().Add(365 * 24 * time.Hour), rotateWindow: 30 * 24 * time.Hour}
	token := &stubToken{rec: rec}
	image := &stubImage{rec: rec}
	prober := &stubProber{rec: rec, version: testVersion, serverStarted: started}
	dbProber := &stubDBProber{rec: rec}
	h := &harness{
		rec: rec, serverStarted: started,
		sup: sup, cert: cert, token: token, image: image, prober: prober, dbProber: dbProber,
	}
	h.deps = Deps{
		Supervisor:      sup,
		Certs:           cert,
		Tokens:          token,
		Images:          image,
		Prober:          prober,
		DBProber:        dbProber,
		ExpectedVersion: testVersion,
	}
	cfg := Config{
		StateDir:    t.TempDir(),
		SocketPath:  filepath.Join(t.TempDir(), "server.sock"),
		ListenAddr:  "127.0.0.1:50052",
		DatabaseDSN: "postgres:///compass",
		AgentImage:  "ghcr.io/example/compass-agent:latest",
		RuntimeDir:  "/run/user/1000/compass",
	}
	return cfg, h
}

// filterEvents keeps only the sequencing-relevant events (start/ensure), dropping
// the interleaved probes so the ordered assertion reads the chain, not the poll.
func filterEvents(events []string) []string {
	out := events[:0:0]
	for _, e := range events {
		if e == "probe" || e == "probe-db" {
			continue
		}
		out = append(out, e)
	}
	return out
}
