//go:build unix

package stack

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
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
// drain order is assertable. pid is a fake, unique-per-child process id the
// pgid-capture path persists; a test stubs readStartTime so no real /proc lookup
// happens.
type stubProcess struct {
	name     string
	pid      int
	rec      *recorder
	waitDone chan struct{}
	waitMu   sync.Mutex
	waitErr  error
	exited   bool
	waitOnce sync.Once
}

func (p *stubProcess) Signal(_ context.Context, sig ProcessSignal) error {
	p.rec.add("signal " + p.name)
	p.waitMu.Lock()
	if p.exited {
		p.waitMu.Unlock()
		return os.ErrProcessDone
	}
	p.exited = true
	p.waitMu.Unlock()
	p.waitOnce.Do(func() {
		if p.waitDone != nil {
			close(p.waitDone)
		}
	})
	return nil
}

// Wait records on return, not entry: the enrollment watcher calls it early, and drain order is what tests pin.
func (p *stubProcess) Wait(ctx context.Context) error {
	if p.waitDone == nil {
		p.rec.add("wait " + p.name)
		return nil
	}
	select {
	case <-p.waitDone:
		p.rec.add("wait " + p.name)
		p.waitMu.Lock()
		defer p.waitMu.Unlock()
		return p.waitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *stubProcess) Pid() int { return p.pid }

func (p *stubProcess) exit(err error) {
	p.waitMu.Lock()
	p.waitErr = err
	p.exited = true
	p.waitMu.Unlock()
	p.waitOnce.Do(func() { close(p.waitDone) })
}

// stubSupervisor records component starts and exposes their process handles.
type stubSupervisor struct {
	rec                  *recorder
	startErr             map[Component]error
	gate                 map[Component]chan struct{}
	entered              map[Component]chan struct{}
	serverStarted        *atomic.Bool
	runnerProcessStarted *atomic.Bool
	runnerStarted        *atomic.Bool
	runnerEnrollments    *atomic.Uint64
	mu                   sync.Mutex
	specs                map[Component]ProcessSpec
	processes            map[Component]*stubProcess
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
	s.mu.Lock()
	s.specs[spec.Component] = spec
	s.mu.Unlock()
	s.rec.add("start " + spec.Component.String())
	if spec.Component == ComponentServer && s.serverStarted != nil {
		s.serverStarted.Store(true)
	}
	proc := &stubProcess{name: spec.Component.String(), pid: fakePid(spec.Component), rec: s.rec, waitDone: make(chan struct{})}
	s.mu.Lock()
	s.processes[spec.Component] = proc
	s.mu.Unlock()
	if spec.Component == ComponentRunner {
		if s.runnerStarted != nil {
			s.runnerStarted.Store(true)
		}
		if s.runnerEnrollments != nil {
			s.runnerEnrollments.Add(1)
		}
		if s.runnerProcessStarted != nil {
			s.runnerProcessStarted.Store(true)
		}
	}
	return proc, nil
}

func (s *stubSupervisor) process(component Component) *stubProcess {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.processes[component]
}
func (s *stubSupervisor) spec(t *testing.T, c Component) ProcessSpec {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	spec, ok := s.specs[c]
	if !ok {
		t.Fatalf("%s never started", c)
	}
	return spec
}

// lastArgs returns the argument vector the named component was last started
// with, failing the test if it never started.
func (s *stubSupervisor) lastArgs(t *testing.T, c Component) []string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	spec, ok := s.specs[c]
	if !ok {
		t.Fatalf("%s never started", c)
	}
	return spec.Args
}

// fakePid maps a component to a stable, distinct fake pid the pgid-capture path
// persists. The values are arbitrary but unique so a test can assert reverse
// drain order and identity by pgid.
func fakePid(c Component) int {
	switch c {
	case ComponentPostgres:
		return 1001
	case ComponentServer:
		return 1002
	case ComponentRunner:
		return 1003
	default:
		return 1000
	}
}

// stubProber answers GetServerInfo. forceLive makes it answer unconditionally
// (attach-if-live); otherwise it answers only once compass-server has started.
type stubProber struct {
	rec                  *recorder
	forceLive            bool
	version              string
	serverStarted        *atomic.Bool
	runnerProcessStarted *atomic.Bool
	runnerEnrollments    *atomic.Uint64
	runnerProbes         chan struct{}
	manualRunnerEnroll   *atomic.Bool
	mu                   sync.Mutex
	enrolled             []EnrolledRunner
}

func (p *stubProber) Probe(ctx context.Context, socketPath string) (ServerInfo, error) {
	p.rec.add("probe")
	p.mu.Lock()
	enrolled := append([]EnrolledRunner(nil), p.enrolled...)
	p.mu.Unlock()
	if p.runnerProcessStarted != nil && p.runnerProcessStarted.Load() {
		select {
		case p.runnerProbes <- struct{}{}:
		default:
		}
		if p.manualRunnerEnroll != nil && p.manualRunnerEnroll.Load() {
			return ServerInfo{Version: p.version, EnrolledRunners: enrolled}, nil
		}
		return ServerInfo{Version: p.version, EnrolledRunners: []EnrolledRunner{{ID: embeddedRunnerID, Enrollment: p.runnerEnrollments.Load()}}}, nil
	}
	if p.forceLive || (p.serverStarted != nil && p.serverStarted.Load()) {
		return ServerInfo{Version: p.version, EnrolledRunners: enrolled}, nil
	}
	return ServerInfo{}, errNotAnswering
}

func (p *stubProber) setEnrolledRunners(runners ...EnrolledRunner) {
	p.mu.Lock()
	p.enrolled = append([]EnrolledRunner(nil), runners...)
	p.mu.Unlock()
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
	mu         sync.Mutex
	lastDSN    string
}

func (p *stubDBProber) ProbeDB(ctx context.Context, dsn string) error {
	p.rec.add("probe-db")
	p.mu.Lock()
	p.lastDSN = dsn
	p.mu.Unlock()
	n := p.calls.Add(1)
	if p.never {
		return errPostgresNotReady
	}
	if n <= int64(p.readyAfter) {
		return errPostgresNotReady
	}
	return nil
}

func (p *stubDBProber) lastProbedDSN() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastDSN
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

// fakeGroupSignaller records signals and models the process-group identity states
// used by detached teardown. alive represents group existence; leaderReadable
// distinguishes a missing/unreadable leader from a recycled leader.
type fakeGroupSignaller struct {
	rec            *recorder
	mu             sync.Mutex
	alive          map[int]bool
	identity       map[int]uint64
	leaderReadable map[int]bool
	signalErr      map[int]map[ProcessSignal]error
	// onKill / onTerm run after a delivered signal, outside the lock, so a test
	// can flip group state at the right escalation step.
	onKill map[int]func()
	onTerm map[int]func()
}

func newFakeGroupSignaller(rec *recorder) *fakeGroupSignaller {
	return &fakeGroupSignaller{
		rec:            rec,
		alive:          map[int]bool{},
		identity:       map[int]uint64{},
		leaderReadable: map[int]bool{},
		signalErr:      map[int]map[ProcessSignal]error{},
		onKill:         map[int]func(){},
		onTerm:         map[int]func(){},
	}
}

func (f *fakeGroupSignaller) Signal(pgid int, sig ProcessSignal) error {
	f.mu.Lock()
	name := "term"
	if sig == SignalKill {
		name = "kill"
	}
	f.rec.add("group-" + name + " " + strconv.Itoa(pgid))
	var cb func()
	switch sig {
	case SignalKill:
		cb = f.onKill[pgid]
	case SignalTerm:
		cb = f.onTerm[pgid]
	}
	err := f.signalErr[pgid][sig]
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if cb != nil {
		cb()
	}
	return nil
}

func (f *fakeGroupSignaller) Liveness(pgid int, startTime uint64) GroupLiveness {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case !f.alive[pgid]:
		return GroupGone
	case !f.leaderReadable[pgid]:
		return GroupOrphaned
	case f.identity[pgid] != startTime:
		return GroupRecycled
	default:
		return GroupOwned
	}
}

// ours reports whether the recorded group is still present and ours to signal.
func (f *fakeGroupSignaller) ours(pgid int, startTime uint64) bool {
	l := f.Liveness(pgid, startTime)
	return l == GroupOwned || l == GroupOrphaned
}

func (f *fakeGroupSignaller) set(pgid int, startTime uint64, alive bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive[pgid] = alive
	f.identity[pgid] = startTime
	f.leaderReadable[pgid] = alive
}

// setLeaderUnknown models a group whose members survive an exited, reaped leader.
func (f *fakeGroupSignaller) setLeaderUnknown(pgid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive[pgid] = true
	f.leaderReadable[pgid] = false
}

func (f *fakeGroupSignaller) failSignal(pgid int, sig ProcessSignal, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.signalErr[pgid] == nil {
		f.signalErr[pgid] = map[ProcessSignal]error{}
	}
	f.signalErr[pgid][sig] = err
}

// fakeContainerController is the container-teardown seam under test: it records
// each stop/remove in order and models per-container existence as a controllable
// state machine, the container analogue of fakeGroupSignaller. exists maps
// name→presence; a name absent from exists is treated as gone. exited marks a
// present container that has stopped, which RemoveExited may remove. onStop /
// onRemove hooks flip a container's state at the right escalation step so a test
// can model a graceful stop, a stop-ignored→rm-f escalation, or a genuine
// survivor; an onStop hook receives the caller's ctx so it can model a slow stop.
type fakeContainerController struct {
	rec      *recorder
	mu       sync.Mutex
	exists   map[string]bool
	exited   map[string]bool
	onStop   map[string]func(ctx context.Context)
	onRemove map[string]func()
}

func newFakeContainerController(rec *recorder) *fakeContainerController {
	return &fakeContainerController{
		rec:      rec,
		exists:   map[string]bool{},
		exited:   map[string]bool{},
		onStop:   map[string]func(context.Context){},
		onRemove: map[string]func(){},
	}
}

func (c *fakeContainerController) Exists(_ context.Context, name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exists[name]
}

func (c *fakeContainerController) Stop(ctx context.Context, name string) error {
	c.mu.Lock()
	c.rec.add("ctr-stop " + name)
	cb := c.onStop[name]
	c.mu.Unlock()
	if cb != nil {
		cb(ctx) // outside the lock: a hook calls setExists, which locks c.mu.
	}
	return nil
}

// RemoveExited removes only an exited container, as a non-forced `podman rm`
// does; a running one is left alone without error.
func (c *fakeContainerController) RemoveExited(_ context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.exists[name] && c.exited[name] {
		c.rec.add("ctr-rm-exited " + name)
		c.exists[name] = false
	}
	return nil
}

func (c *fakeContainerController) Remove(_ context.Context, name string) error {
	c.mu.Lock()
	c.rec.add("ctr-rm " + name)
	cb := c.onRemove[name]
	c.mu.Unlock()
	if cb != nil {
		cb()
	}
	return nil
}

func (c *fakeContainerController) setExists(exists bool) {
	c.setExistsName(pgContainerName, exists)
}

// setExistsName models a specific container's presence, so a record with more
// than one container entry (postgres + collector) can drive each independently.
func (c *fakeContainerController) setExistsName(name string, exists bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.exists[name] = exists
}

// setExited marks a present container as stopped but not yet removed.
func (c *fakeContainerController) setExited(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.exited[name] = true
}

// fakePostgresContainer is the container-START seam under test (the analogue of
// stubSupervisor for the container path): Start records the run and the spec it
// was handed, so a test can assert the container path was taken and inspect the
// resolved run spec. It returns a stub Process whose Signal/Wait record into the
// shared recorder as "signal postgres"/"wait postgres" — the same event labels
// the process path uses — so the reverse-drain assertion is path-agnostic.
// startErr injects a launch failure.
type fakePostgresContainer struct {
	rec      *recorder
	mu       sync.Mutex
	startErr error
	started  int
	lastSpec PostgresContainerSpec
}

func newFakePostgresContainer(rec *recorder) *fakePostgresContainer {
	return &fakePostgresContainer{rec: rec}
}

func (c *fakePostgresContainer) Start(_ context.Context, spec PostgresContainerSpec) (Process, error) {
	c.mu.Lock()
	c.started++
	c.lastSpec = spec
	c.rec.add("start postgres-container")
	err := c.startErr
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &stubContainerProcess{rec: c.rec}, nil
}

func (c *fakePostgresContainer) spec() PostgresContainerSpec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSpec
}

// stubContainerProcess is the in-process handle a fakePostgresContainer.Start
// hands back. Its Signal/Wait record with the "postgres" label so drain-order
// assertions read identically to the process path; Pid is the sentinel 0 the
// real container handle also returns (a container carries no persisted pgid).
type stubContainerProcess struct {
	rec     *recorder
	stopped atomic.Bool
}

func (p *stubContainerProcess) Signal(_ context.Context, sig ProcessSignal) error {
	p.rec.add("signal postgres")
	p.stopped.Store(true)
	return nil
}

func (p *stubContainerProcess) Wait(_ context.Context) error {
	p.rec.add("wait postgres")
	return nil
}

func (p *stubContainerProcess) Pid() int { return 0 }

// fakeCollectorContainer is the collector-START seam under test (the analogue of
// fakePostgresContainer): Start records the run and the spec it was handed, so a
// test can assert the collector path was taken and inspect the resolved spec. It
// returns a stub Process whose Signal/Wait record into the shared recorder as
// "signal otel-collector"/"wait otel-collector" so the reverse-drain assertion
// reads the collector like any other child. startErr injects a launch failure.
type fakeCollectorContainer struct {
	rec      *recorder
	mu       sync.Mutex
	startErr error
	started  int
	lastSpec CollectorContainerSpec
}

func newFakeCollectorContainer(rec *recorder) *fakeCollectorContainer {
	return &fakeCollectorContainer{rec: rec}
}

func (c *fakeCollectorContainer) Start(_ context.Context, spec CollectorContainerSpec) (Process, error) {
	c.mu.Lock()
	c.started++
	c.lastSpec = spec
	c.rec.add("start otel-collector")
	err := c.startErr
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &stubCollectorProcess{rec: c.rec}, nil
}

func (c *fakeCollectorContainer) spec() CollectorContainerSpec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSpec
}

// stubCollectorProcess is the in-process handle a fakeCollectorContainer.Start
// hands back. Its Signal/Wait record with the "otel-collector" label so
// drain-order assertions read it like the process/postgres paths; Pid is the 0
// sentinel a real container handle also returns.
type stubCollectorProcess struct {
	rec     *recorder
	stopped atomic.Bool
}

func (p *stubCollectorProcess) Signal(_ context.Context, sig ProcessSignal) error {
	p.rec.add("signal otel-collector")
	p.stopped.Store(true)
	return nil
}

func (p *stubCollectorProcess) Wait(_ context.Context) error {
	p.rec.add("wait otel-collector")
	return nil
}

func (p *stubCollectorProcess) Pid() int { return 0 }

// stubCollectorProber answers the collector-health probe. By default it is ready
// immediately (readyAfter == 0). readyAfter > 0 models a cold collector: the
// first readyAfter probes report not-ready, then it flips ready. never makes it
// report not-ready forever, so a test can drive the budget-timeout failure via a
// controlled clock. It records the last health endpoint it probed.
type stubCollectorProber struct {
	rec        *recorder
	readyAfter int
	never      bool
	calls      atomic.Int64
	mu         sync.Mutex
	lastEP     string
}

func (p *stubCollectorProber) ProbeCollector(_ context.Context, healthEndpoint string) error {
	p.rec.add("probe-collector")
	p.mu.Lock()
	p.lastEP = healthEndpoint
	p.mu.Unlock()
	n := p.calls.Add(1)
	if p.never {
		return errCollectorNotReady
	}
	if n <= int64(p.readyAfter) {
		return errCollectorNotReady
	}
	return nil
}

func (p *stubCollectorProber) lastEndpoint() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastEP
}

var errCollectorNotReady = &collectorProbeError{}

type collectorProbeError struct{}

func (*collectorProbeError) Error() string { return "otel-collector not answering" }

// fakeNatsContainer is the nats-START seam under test (the analogue of
// fakeCollectorContainer): Start records the run and the spec it was handed, so
// a test can assert the nats path was taken and inspect the resolved spec. It
// returns a stub Process whose Signal/Wait record into the shared recorder as
// "signal nats"/"wait nats" so the reverse-drain assertion reads nats like any
// other child. startErr injects a launch failure.
type fakeNatsContainer struct {
	rec      *recorder
	mu       sync.Mutex
	startErr error
	started  int
	lastSpec NatsContainerSpec
}

func newFakeNatsContainer(rec *recorder) *fakeNatsContainer {
	return &fakeNatsContainer{rec: rec}
}

func (c *fakeNatsContainer) Start(_ context.Context, spec NatsContainerSpec) (Process, error) {
	c.mu.Lock()
	c.started++
	c.lastSpec = spec
	c.rec.add("start nats")
	err := c.startErr
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &stubNatsProcess{rec: c.rec}, nil
}

func (c *fakeNatsContainer) spec() NatsContainerSpec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSpec
}

// stubNatsProcess is the in-process handle a fakeNatsContainer.Start hands back.
// Its Signal/Wait record with the "nats" label so drain-order assertions read it
// like the other container children; Pid is the 0 sentinel a real container
// handle also returns.
type stubNatsProcess struct {
	rec     *recorder
	stopped atomic.Bool
}

func (p *stubNatsProcess) Signal(_ context.Context, sig ProcessSignal) error {
	p.rec.add("signal nats")
	p.stopped.Store(true)
	return nil
}

func (p *stubNatsProcess) Wait(_ context.Context) error {
	p.rec.add("wait nats")
	return nil
}

func (p *stubNatsProcess) Pid() int { return 0 }

// stubNatsProber answers the nats-readiness probe, mirroring
// stubCollectorProber: ready immediately by default, not-ready for the first
// readyAfter probes, or never ready so a test can drive the budget timeout via a
// controlled clock. It records the last monitor endpoint it probed.
type stubNatsProber struct {
	rec        *recorder
	readyAfter int
	never      bool
	calls      atomic.Int64
	mu         sync.Mutex
	lastEP     string
}

func (p *stubNatsProber) ProbeNats(_ context.Context, monitorEndpoint string) error {
	p.rec.add("probe-nats")
	p.mu.Lock()
	p.lastEP = monitorEndpoint
	p.mu.Unlock()
	n := p.calls.Add(1)
	if p.never {
		return errNatsNotReady
	}
	if n <= int64(p.readyAfter) {
		return errNatsNotReady
	}
	return nil
}

func (p *stubNatsProber) lastEndpoint() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastEP
}

var errNatsNotReady = &natsProbeError{}

type natsProbeError struct{}

func (*natsProbeError) Error() string { return "nats not answering" }

// harness bundles the recorder, the shared serverStarted flag, and the stub
// seams so a test can tweak individual fields before calling Up.
type harness struct {
	rec                    *recorder
	manualRunnerEnrollFlag *atomic.Bool
	serverStarted          *atomic.Bool
	sup                    *stubSupervisor
	cert                   *stubCert
	token                  *stubToken
	image                  *stubImage
	prober                 *stubProber
	dbProber               *stubDBProber
	groupSig               *fakeGroupSignaller
	containers             *fakeContainerController
	collector              *fakeCollectorContainer
	collectorProber        *stubCollectorProber
	nats                   *fakeNatsContainer
	natsProber             *stubNatsProber
	gateway                *fakeGatewayContainer
	gatewayProber          *stubGatewayProber
	deps                   Deps
}

const testVersion = "1.0.0"

func newHarness(t *testing.T) (Config, *harness) {
	t.Helper()
	return newHarnessWithRunnerEnrollment(t, false)
}

func newHarnessWithRunnerEnrollment(t *testing.T, manual bool) (Config, *harness) {
	t.Helper()
	manualRunnerEnroll := &atomic.Bool{}
	manualRunnerEnroll.Store(manual)
	return newHarnessWithRunnerEnrollmentFlag(t, manualRunnerEnroll)
}

func newHarnessWithRunnerEnrollmentFlag(t *testing.T, manualRunnerEnroll *atomic.Bool) (Config, *harness) {
	t.Helper()

	rec := &recorder{}
	started := &atomic.Bool{}
	sup := &stubSupervisor{
		rec: rec, startErr: map[Component]error{}, gate: map[Component]chan struct{}{}, entered: map[Component]chan struct{}{},
		serverStarted: started, runnerStarted: &atomic.Bool{}, runnerEnrollments: &atomic.Uint64{}, runnerProcessStarted: &atomic.Bool{},
		specs: map[Component]ProcessSpec{}, processes: map[Component]*stubProcess{},
	}
	cert := &stubCert{rec: rec, notAfter: time.Now().Add(365 * 24 * time.Hour), rotateWindow: 30 * 24 * time.Hour}
	token := &stubToken{rec: rec}
	image := &stubImage{rec: rec}
	prober := &stubProber{
		rec: rec, version: testVersion, serverStarted: started, runnerProcessStarted: sup.runnerProcessStarted,
		runnerEnrollments: sup.runnerEnrollments,
		runnerProbes:      make(chan struct{}, 1), manualRunnerEnroll: manualRunnerEnroll,
	}
	dbProber := &stubDBProber{rec: rec}
	groupSig := newFakeGroupSignaller(rec)
	containers := newFakeContainerController(rec)
	collector := newFakeCollectorContainer(rec)
	collectorProber := &stubCollectorProber{rec: rec}
	nats := newFakeNatsContainer(rec)
	gateway := newFakeGatewayContainer(rec)
	gatewayProber := &stubGatewayProber{rec: rec}
	natsProber := &stubNatsProber{rec: rec}

	// Stub the start-time reader so the pgid-capture path never touches /proc:
	// map each fake pid to a deterministic token (pid*10) and restore the real
	// reader when the test ends.
	prev := readStartTime
	readStartTime = func(pid int) (uint64, error) { return uint64(pid) * 10, nil }
	t.Cleanup(func() { readStartTime = prev })
	stubBootID(t, testBootID)
	h := &harness{
		rec: rec, serverStarted: started, manualRunnerEnrollFlag: manualRunnerEnroll,
		sup: sup, cert: cert, token: token, image: image, prober: prober, dbProber: dbProber, groupSig: groupSig, containers: containers, collector: collector, collectorProber: collectorProber, nats: nats, natsProber: natsProber,
		gateway: gateway, gatewayProber: gatewayProber,
	}
	h.deps = Deps{
		ListenTCP: func(network, address string) (*net.TCPListener, error) {
			addr, err := net.ResolveTCPAddr(network, address)
			if err != nil {
				return nil, err
			}
			return net.ListenTCP(network, addr)
		},
		Supervisor:         sup,
		Certs:              cert,
		Tokens:             token,
		Images:             image,
		Prober:             prober,
		DBProber:           dbProber,
		GroupSignaller:     groupSig,
		Containers:         containers,
		CollectorContainer: collector,
		CollectorProber:    collectorProber,
		NatsContainer:      nats,
		NatsProber:         natsProber,
		GatewayContainer:   gateway,
		GatewayProber:      gatewayProber,
		ExpectedVersion:    testVersion,
	}
	cfg := Config{
		StateDir:    t.TempDir(),
		SocketPath:  filepath.Join(t.TempDir(), "server.sock"),
		ListenAddr:  "127.0.0.1:0",
		DatabaseDSN: "postgres:///compass",
		AgentImage:  "ghcr.io/example/compass-agent:latest",
		RuntimeDir:  "/run/user/1000/compass",
		// Bundle path (ExternalOTLPEndpoint unset): the fake CollectorContainer
		// runs, but collectorContainerSpec now rejects an empty image, so the
		// harness pins a dummy pinned ref. Tests exercising the opt-out set
		// ExternalOTLPEndpoint explicitly.
		CollectorImage: "otel/opentelemetry-collector-contrib:test",
		// Same for the bundle-path nats image: natsContainerSpec rejects an empty
		// NatsImage, so the harness pins a dummy ref. Tests exercising the
		// --nats-external opt-out set ExternalNatsURL explicitly.
		NatsImage:    "nats:test",
		GatewayImage: "gateway:test",
		// Bundle-path host ports: the spec builders reject 0.
		NatsClientPort: DefaultNatsClientPort, NatsMonitorPort: DefaultNatsMonitorPort,
		CollectorGRPCPort: DefaultCollectorGRPCPort, CollectorHTTPPort: DefaultCollectorHTTPPort,
		CollectorHealthPort: DefaultCollectorHealthPort,
	}
	return cfg, h
}

// filterEvents keeps only the sequencing-relevant events (start/ensure), dropping
// the interleaved probes so the ordered assertion reads the chain, not the poll.
func filterEvents(events []string) []string {
	out := events[:0:0]
	for _, e := range events {
		if e == "probe" || e == "probe-db" || e == "probe-collector" || e == "probe-nats" || e == "probe-gateway" {
			continue
		}
		out = append(out, e)
	}
	return out
}
