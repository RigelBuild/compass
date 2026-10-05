//go:build unix

package stack

import (
	"context"
	"errors"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// coldStartSequence is the ordered cold-start event log with poll probes filtered out.
var coldStartSequence = []string{
	"start postgres",
	"start otel-collector",
	"start nats",
	"start llm-gateway",
	"ensure-cert",
	"start compass-server",
	"ensure-token",
	"ensure-image",
	"start compass-runner",
}

func TestUpColdSequencing(t *testing.T) {
	cfg, h := newHarness(t)

	s, err := Up(context.Background(), cfg, h.deps)
	if err != nil {
		t.Fatalf("Up() = %v, want nil", err)
	}
	if s.attached {
		t.Fatal("cold Up should not be attached")
	}
	got := filterEvents(h.rec.snapshot())
	if !reflect.DeepEqual(got, coldStartSequence) {
		t.Fatalf("cold start sequence:\n got  %v\n want %v", got, coldStartSequence)
	}

	// Cold Up leaves it Ready.
	st, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health() = %v", err)
	}
	if st.State != StatusReady {
		t.Fatalf("Health state = %v, want Ready", st.State)
	}
}

// The runner spec must carry --runner-id, and the SAME id must reach the token
// ensurer — the runner cross-checks its --runner-id against the token subject at
// enroll, so a drift between the two silently breaks boot. This test fails if
// either side stops using embeddedRunnerID.
func TestRunnerIDCouplesSpawnAndMint(t *testing.T) {
	// (a) runnerSpec carries --runner-id with the constant's value.
	spec := runnerSpec(Config{ListenAddr: "127.0.0.1:50052"}, CertResult{CertPath: "/c"}, "tok", GuestPaths{}, "", "127.0.0.1:50052")
	specID, ok := flagValue(spec.Args, "--runner-id")
	if !ok {
		t.Fatalf("runner spec args %v carry no --runner-id", spec.Args)
	}
	if specID != embeddedRunnerID {
		t.Fatalf("runner spec --runner-id = %q, want %q", specID, embeddedRunnerID)
	}

	// (b) the same id reaches the token ensurer during a cold Up.
	cfg, h := newHarness(t)
	if _, err := Up(context.Background(), cfg, h.deps); err != nil {
		t.Fatalf("Up() = %v", err)
	}
	if h.token.mintedID != specID {
		t.Fatalf("token minted for id %q but runner spawned with %q; the two must agree", h.token.mintedID, specID)
	}
}

// flagValue returns the argument following the named flag in a --flag value arg
// vector.
func flagValue(args []string, flag string) (string, bool) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1], true
		}
	}
	return "", false
}

func TestUpAttachIfLive(t *testing.T) {
	cfg, h := newHarness(t)
	h.prober.forceLive = true // the server answers before any spawn

	s, err := Up(context.Background(), cfg, h.deps)
	if err != nil {
		t.Fatalf("Up() = %v, want nil", err)
	}
	if !s.attached {
		t.Fatal("Up should have attached to the live server")
	}
	// Nothing was spawned or ensured.
	for _, e := range filterEvents(h.rec.snapshot()) {
		t.Fatalf("attach spawned/ensured %q; want no spawn events", e)
	}

	st, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health() = %v", err)
	}
	if st.State != StatusAttached {
		t.Fatalf("Health state = %v, want Attached", st.State)
	}
}

func TestUpAttachWithoutGatewayImage(t *testing.T) {
	cfg, h := newHarness(t)
	cfg.GatewayImage = ""
	h.prober.forceLive = true
	s, err := Up(context.Background(), cfg, h.deps)
	if err != nil {
		t.Fatalf("Up() = %v, want attach without a gateway image", err)
	}
	if !s.attached {
		t.Fatal("Up should have attached to the live server")
	}
}

func TestUpAttachVersionMismatch(t *testing.T) {
	cfg, h := newHarness(t)
	h.prober.forceLive = true
	h.prober.version = "9.9.9" // != ExpectedVersion (testVersion)

	_, err := Up(context.Background(), cfg, h.deps)
	if !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("Up() = %v, want ErrVersionMismatch", err)
	}
	// No spawn on a mismatched attach.
	for _, e := range filterEvents(h.rec.snapshot()) {
		t.Fatalf("mismatch attach spawned %q; want none", e)
	}
	// The lock must be released so a corrected Up can proceed.
	assertLockFree(t, cfg.StateDir)
}

func TestRestartRunnerReusesBoundNetworkAddress(t *testing.T) {
	cfg, h := newHarness(t)
	s, err := Up(context.Background(), cfg, h.deps)
	if err != nil {
		t.Fatalf("Up() = %v", err)
	}
	addr := s.ListenAddr()
	t.Cleanup(func() {
		if err := s.Down(context.Background()); err != nil {
			t.Errorf("Down() = %v", err)
		}
	})
	if err := s.RestartRunner(context.Background()); err != nil {
		t.Fatalf("RestartRunner() = %v", err)
	}
	if got := s.ListenAddr(); got != addr {
		t.Fatalf("ListenAddr after runner restart = %q, want %q", got, addr)
	}
	runnerSpec := h.sup.spec(t, ComponentRunner)
	if got, ok := flagValue(runnerSpec.Args, "--server"); !ok || got != "https://"+addr {
		t.Fatalf("restarted runner --server = %q, %v; want https://%s", got, ok, addr)
	}
}

func TestUpFailureClosesNetworkListener(t *testing.T) {
	cfg, h := newHarness(t)
	h.cert.err = errors.New("certificate failure")
	addrCh := make(chan string, 1)
	listenTCP := h.deps.ListenTCP
	h.deps.ListenTCP = func(network, address string) (*net.TCPListener, error) {
		listener, err := listenTCP(network, address)
		if err == nil {
			addrCh <- listener.Addr().String()
			t.Cleanup(func() { _ = listener.Close() })
		}
		return listener, err
	}
	if _, err := Up(context.Background(), cfg, h.deps); err == nil {
		t.Fatal("Up() = nil, want certificate failure")
	}
	addr := <-addrCh
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("rebind %s after failed Up: %v", addr, err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close re-bound listener: %v", err)
	}
}
func TestUpCertRotation(t *testing.T) {
	tests := []struct {
		name        string
		now         time.Time
		notAfter    time.Time
		wantRotated bool
	}{
		{
			name:        "near expiry rotates",
			now:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			notAfter:    time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), // 9 days out, inside the 30-day window
			wantRotated: true,
		},
		{
			name:        "fresh anchor does not rotate",
			now:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			notAfter:    time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), // a year out
			wantRotated: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, h := newHarness(t)
			h.cert.notAfter = tc.notAfter
			h.cert.rotateWindow = 30 * 24 * time.Hour
			h.deps.Now = func() time.Time { return tc.now }

			if _, err := Up(context.Background(), cfg, h.deps); err != nil {
				t.Fatalf("Up() = %v", err)
			}
			called, rotated := h.cert.didRotate()
			if !called {
				t.Fatal("EnsureCert was never called")
			}
			if rotated != tc.wantRotated {
				t.Fatalf("rotated = %v, want %v", rotated, tc.wantRotated)
			}
		})
	}
}

func TestUpFailureSurface(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(*harness)
	}{
		{
			name:    "postgres unreachable",
			arrange: func(h *harness) { h.sup.startErr[ComponentPostgres] = errors.New("pg boom") },
		},
		{
			name:    "token mint fails",
			arrange: func(h *harness) { h.token.err = errors.New("token boom") },
		},
		{
			name:    "image ensure fails",
			arrange: func(h *harness) { h.image.err = errors.New("image boom") },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, h := newHarness(t)
			tc.arrange(h)

			s, err := Up(context.Background(), cfg, h.deps)
			if err == nil {
				t.Fatal("Up() = nil, want failure")
			}
			if s != nil {
				t.Fatal("Up() returned a Stack on failure; want nil (no half-started leak)")
			}
			// Any children started before the failure were drained (their
			// signal/wait events appear in the log).
			assertDrainedCleanly(t, h)
			// The lock is released, so a retry can acquire.
			assertLockFree(t, cfg.StateDir)
		})
	}
}

// TestUpServerNeverReady exercises the readiness-budget failure: the server
// process starts but the prober never answers, so waitReady times out and the
// spawned children are drained.
func TestUpServerNeverReady(t *testing.T) {
	cfg, h := newHarness(t)
	// The prober answers only when serverStarted is set; detach it so it never
	// answers, and shrink the budget via a controlled clock.
	h.prober.serverStarted = nil
	h.prober.forceLive = false
	// Drive the clock so the readiness budget elapses without real sleeping.
	var ticks int
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h.deps.Now = func() time.Time {
		ticks++
		// First reads include postgres, collector, nats, and gateway gate deadlines,
		// cert setup, and server readiness; each container gate adds a clock read.
		// After enough reads jump past the budget so waitReady gives up deterministically.
		if ticks > 6 {
			return start.Add(readyPollBudget + time.Second)
		}
		return start
	}

	s, err := Up(context.Background(), cfg, h.deps)
	if err == nil {
		t.Fatal("Up() = nil, want readiness timeout")
	}
	if s != nil {
		t.Fatal("Up() returned a Stack; want nil")
	}
	// postgres and compass-server started, then were drained in reverse.
	assertDrainedCleanly(t, h)
	assertLockFree(t, cfg.StateDir)
}

func TestUpWaitsForRunnerEnrollment(t *testing.T) {
	cfg, h := newHarness(t)
	h.manualRunnerEnrollFlag.Store(true)
	done := make(chan struct {
		stack *Stack
		err   error
	}, 1)
	go func() {
		s, err := Up(context.Background(), cfg, h.deps)
		done <- struct {
			stack *Stack
			err   error
		}{stack: s, err: err}
	}()

	select {
	case <-h.prober.runnerProbes:
	case <-time.After(5 * time.Second):
		t.Fatal("Up did not probe Runner enrollment")
	}
	h.prober.setEnrolledRunnerIDs(embeddedRunnerID)
	result := <-done
	if result.err != nil {
		t.Fatalf("Up() = %v, want nil after Runner enrollment", result.err)
	}
	if result.stack == nil || result.stack.attached {
		t.Fatalf("Up() stack = %+v, want spawned stack", result.stack)
	}
	if err := result.stack.Down(context.Background()); err != nil {
		t.Fatalf("Down() = %v", err)
	}
}

func TestUpRunnerExitBeforeEnrollmentDrainsChildren(t *testing.T) {
	cfg, h := newHarness(t)
	h.manualRunnerEnrollFlag.Store(true)
	exitErr := errors.New("preflight failed")
	done := make(chan error, 1)

	go func() {
		_, err := Up(context.Background(), cfg, h.deps)
		done <- err
	}()

	select {
	case <-h.prober.runnerProbes:
	case <-time.After(5 * time.Second):
		t.Fatal("Up did not probe Runner enrollment")
	}
	h.sup.process(ComponentRunner).exit(exitErr)
	err := <-done
	if err == nil || !strings.Contains(err.Error(), exitErr.Error()) || !strings.Contains(err.Error(), "compass-runner exited before enrolling") {
		t.Fatalf("Up() error = %v, want Runner exit before enrollment: %v", err, exitErr)
	}
	assertDrainedCleanly(t, h)
}

func TestUpRunnerEnrollmentBudgetExhausted(t *testing.T) {
	cfg, h := newHarness(t)
	h.manualRunnerEnrollFlag.Store(true)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var runnerClockReads atomic.Int32
	h.deps.Now = func() time.Time {
		if h.sup.runnerStarted.Load() && runnerClockReads.Add(1) > 1 {
			return start.Add(runnerEnrollPollBudget + time.Second)
		}
		return start
	}

	done := make(chan error, 1)
	go func() {
		_, err := Up(context.Background(), cfg, h.deps)
		done <- err
	}()
	select {
	case <-h.prober.runnerProbes:
	case <-time.After(5 * time.Second):
		t.Fatal("Up did not probe Runner enrollment")
	}
	err := <-done
	if err == nil || !strings.Contains(err.Error(), embeddedRunnerID) || !strings.Contains(err.Error(), runnerEnrollPollBudget.String()) {
		t.Fatalf("Up() error = %v, want enrollment timeout naming %q and %s", err, embeddedRunnerID, runnerEnrollPollBudget)
	}
	assertDrainedCleanly(t, h)
}

func TestRestartRunnerWaitsForEnrollment(t *testing.T) {
	cfg, h := newHarness(t)
	h.manualRunnerEnrollFlag.Store(true)
	h.prober.setEnrolledRunnerIDs(embeddedRunnerID)
	s, err := Up(context.Background(), cfg, h.deps)
	if err != nil {
		t.Fatalf("initial Up() = %v", err)
	}
	select {
	case <-h.prober.runnerProbes:
	case <-time.After(5 * time.Second):
		t.Fatal("initial Up did not probe Runner enrollment")
	}
	h.prober.setEnrolledRunnerIDs()

	done := make(chan error, 1)
	go func() { done <- s.RestartRunner(context.Background()) }()
	select {
	case <-h.prober.runnerProbes:
	case <-time.After(5 * time.Second):
		t.Fatal("RestartRunner did not probe Runner enrollment")
	}
	h.prober.setEnrolledRunnerIDs(embeddedRunnerID)
	if err := <-done; err != nil {
		t.Fatalf("RestartRunner() = %v, want nil after Runner enrollment", err)
	}
	if err := s.Down(context.Background()); err != nil {
		t.Fatalf("Down() = %v", err)
	}
}

// TestUpWaitsForPostgresBeforeServer pins the cold-start gate: when postgres is
// not immediately reachable, spawnChain must poll DBProber until it accepts
// BEFORE starting compass-server — the bug the integration test surfaced, where
// the server started against a not-yet-accepting postgres and its single-ping
// store.Open died. readyAfter=3 models a cold cluster (initdb+start+createdb):
// the first three probes report not-ready, then it flips ready. The proof is
// twofold — the probe was polled more than once, and the ordered event log still
// shows compass-server starting only after the postgres gate cleared.
func TestUpWaitsForPostgresBeforeServer(t *testing.T) {
	cfg, h := newHarness(t)
	h.dbProber.readyAfter = 3

	s, err := Up(context.Background(), cfg, h.deps)
	if err != nil {
		t.Fatalf("Up() = %v, want nil once postgres becomes reachable", err)
	}
	if s.attached {
		t.Fatal("cold Up should not be attached")
	}
	// The gate polled until ready: >1 probe means it did not proceed on the
	// first not-ready answer.
	if n := h.dbProber.calls.Load(); n <= 1 {
		t.Fatalf("DBProber called %d times; want it polled until ready (>1)", n)
	}
	// The full chain still ran in order — the gate sits between postgres and the
	// server, so compass-server appears only after the postgres start.
	got := filterEvents(h.rec.snapshot())
	if !reflect.DeepEqual(got, coldStartSequence) {
		t.Fatalf("cold start sequence with slow postgres:\n got  %v\n want %v", got, coldStartSequence)
	}
	if err := s.Down(context.Background()); err != nil {
		t.Fatalf("Down() = %v", err)
	}
}

// TestUpPostgresNeverReady exercises the postgres-readiness budget failure: the
// postgres process starts but never accepts, so waitPostgres times out with a
// legible error and the one child started so far (postgres) is drained — and
// crucially compass-server is NEVER started against a dead database. A
// controlled clock elapses the budget without real sleeping.
func TestUpPostgresNeverReady(t *testing.T) {
	cfg, h := newHarness(t)
	h.dbProber.never = true
	var ticks int
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h.deps.Now = func() time.Time {
		ticks++
		if ticks > 3 {
			return start.Add(dbReadyPollBudget + time.Second)
		}
		return start
	}

	s, err := Up(context.Background(), cfg, h.deps)
	if err == nil {
		t.Fatal("Up() = nil, want postgres-readiness timeout")
	}
	if s != nil {
		t.Fatal("Up() returned a Stack; want nil")
	}
	// compass-server must never have started against an unreachable postgres.
	if n := countEvent(h.rec.snapshot(), "start compass-server"); n != 0 {
		t.Fatalf("compass-server started %d times before postgres was ready; want 0", n)
	}
	// The one started child (postgres) was drained; the lock is released.
	assertDrainedCleanly(t, h)
	assertLockFree(t, cfg.StateDir)
}

// The stack binds the door, hands the server fd 3, points the runner at the
// resolved address, and drops its own copy as soon as the server starts: with
// the stub supervisor no child holds the port, so a rebind must succeed while
// Up is still blocked starting the runner.
func TestStackHandsNetworkListenerToServer(t *testing.T) {
	cfg, h := newHarness(t)
	var addr string
	listenTCP := h.deps.ListenTCP
	h.deps.ListenTCP = func(network, address string) (*net.TCPListener, error) {
		l, err := listenTCP(network, address)
		if err == nil {
			addr = l.Addr().String()
		}
		return l, err
	}
	entered, gate := make(chan struct{}), make(chan struct{})
	h.sup.entered[ComponentRunner], h.sup.gate[ComponentRunner] = entered, gate
	type upResult struct {
		s   *Stack
		err error
	}
	done := make(chan upResult, 1)
	go func() {
		s, err := Up(context.Background(), cfg, h.deps)
		done <- upResult{s, err}
	}()
	<-entered // server started, runner Start blocked: Up has not returned
	listener, rebindErr := net.Listen("tcp", addr)
	close(gate)
	res := <-done
	if res.err != nil {
		t.Fatalf("Up() = %v", res.err)
	}
	if rebindErr != nil {
		t.Fatalf("rebind %s after the server started: %v (the stack kept its listener)", addr, rebindErr)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close re-bound listener: %v", err)
	}
	if got := res.s.ListenAddr(); got != addr || got == cfg.ListenAddr {
		t.Fatalf("ListenAddr() = %q, want the resolved %q", got, addr)
	}
	serverSpec := h.sup.spec(t, ComponentServer)
	if got, ok := flagValue(serverSpec.Args, "--listen-fd"); !ok || got != "3" {
		t.Fatalf("server --listen-fd = %q, %v; want 3", got, ok)
	}
	if slices.Contains(serverSpec.Args, "--listen") || len(serverSpec.ExtraFiles) != 1 {
		t.Fatalf("server spec = %+v; want inherited fd 3 and one ExtraFile, no --listen", serverSpec)
	}
	runnerSpec := h.sup.spec(t, ComponentRunner)
	if got, ok := flagValue(runnerSpec.Args, "--server"); !ok || got != "https://"+addr {
		t.Fatalf("runner --server = %q, %v; want https://%s", got, ok, addr)
	}
	if err := res.s.Down(context.Background()); err != nil {
		t.Fatalf("Down() = %v", err)
	}
}

func TestDownDrainsReverseAndReleasesLock(t *testing.T) {
	cfg, h := newHarness(t)

	s, err := Up(context.Background(), cfg, h.deps)
	if err != nil {
		t.Fatalf("Up() = %v", err)
	}

	if err := s.Down(context.Background()); err != nil {
		t.Fatalf("Down() = %v", err)
	}

	// A fully successful drain removes the teardown record, so no stale
	// stack.pgids is left for a later cross-process down to act on.
	assertPgidFileGone(t, cfg.StateDir)

	// Children stop in reverse order: runner → server → gateway → nats → collector → postgres.
	wantStops := []string{
		"signal compass-runner", "wait compass-runner",
		"signal compass-server", "wait compass-server",
		"signal llm-gateway", "wait llm-gateway",
		"signal nats", "wait nats",
		"signal otel-collector", "wait otel-collector",
		"signal postgres", "wait postgres",
	}
	got := stopEvents(h.rec.snapshot())
	if !reflect.DeepEqual(got, wantStops) {
		t.Fatalf("down stop order:\n got  %v\n want %v", got, wantStops)
	}

	// The lock is free, so a subsequent Up can acquire it (attach here, since the
	// prober still reports the server live from the first Up).
	s2, err := Up(context.Background(), cfg, h.deps)
	if err != nil {
		t.Fatalf("second Up() = %v", err)
	}
	if !s2.attached {
		t.Fatal("second Up should attach (server still answering)")
	}
	assertLockFree(t, cfg.StateDir)
}

// stopEvents keeps only signal/wait events, in order.
func stopEvents(events []string) []string {
	var out []string
	for _, e := range events {
		if len(e) >= 6 && (e[:6] == "signal" || e[:4] == "wait") {
			out = append(out, e)
		}
	}
	return out
}

// assertDrainedCleanly asserts every child the stub supervisor started was
// subsequently signalled (no orphaned child on a failed Up).
func assertDrainedCleanly(t *testing.T, h *harness) {
	t.Helper()
	started := map[string]bool{}
	signalled := map[string]bool{}
	for _, e := range h.rec.snapshot() {
		switch {
		case len(e) > 6 && e[:6] == "start " && e != "start-failed":
			started[e[6:]] = true
		case len(e) > 7 && e[:7] == "signal ":
			signalled[e[7:]] = true
		}
	}
	for name := range started {
		if !signalled[name] {
			t.Errorf("child %q started but never drained on failure", name)
		}
	}
}
