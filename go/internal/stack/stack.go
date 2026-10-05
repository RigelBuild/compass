//go:build unix

package stack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"time"
)

// ErrVersionMismatch is returned by Up when it attaches to a live server whose
// version differs from Deps.ExpectedVersion: an upgraded app must not silently
// drive a previous version's lingering stack. The caller (the CLI) surfaces it
// as a restart-stack prompt.
var ErrVersionMismatch = errors.New("live stack version does not match this build; restart the stack")

// readyPollInterval and readyPollBudget bound the GetServerInfo readiness poll
// after compass-server is spawned: the socket binds before migrations complete,
// so readiness is the first answering probe, not socket existence
// (devenv.nix:229-235). The budget is the failure surface — a server that never
// answers within it yields a Failed status.
const (
	readyPollInterval = 100 * time.Millisecond
	readyPollBudget   = 30 * time.Second
	// dbReadyPollInterval/dbReadyPollBudget bound the postgres-reachability poll
	// between starting postgres and compass-server. The budget is larger than
	// readyPollBudget because this waits on cold cluster init (initdb + start +
	// createdb), ~5.5s idle but heavier on a loaded box, so 60s leaves headroom.
	dbReadyPollInterval = 100 * time.Millisecond
	dbReadyPollBudget   = 60 * time.Second
	// collectorReadyPollInterval/collectorReadyPollBudget bound the collector-health
	// poll between launching the bundled collector and its emitters. The collector
	// holds no on-disk state and does no cold init, binding in under a second, so
	// the budget is the smaller readyPollBudget tier.
	collectorReadyPollInterval = 100 * time.Millisecond
	collectorReadyPollBudget   = 30 * time.Second
	// natsReadyPollInterval/natsReadyPollBudget bound the nats-readiness poll
	// between launching NATS and its consumers. NATS boots fast — no cold init —
	// and a single-node JetStream store recovers in under a second, so the budget
	// is the same readyPollBudget tier as the collector.
	natsReadyPollInterval    = 100 * time.Millisecond
	natsReadyPollBudget      = 30 * time.Second
	gatewayReadyPollInterval = 100 * time.Millisecond
	gatewayReadyPollBudget   = 30 * time.Second
	// Enrollment is a single local probe after Runner preflight; this short budget
	// fails a dead or wedged Runner without holding up a healthy cold start.
	runnerEnrollPollInterval = 100 * time.Millisecond
	runnerEnrollPollBudget   = 15 * time.Second
)

// Stack is a supervised embedded stack: the resolved config plus the child
// handles this process owns. An attached stack (a live server was already
// answering) owns no children — Down on it only releases the lock.
type Stack struct {
	cfg            Config
	deps           Deps
	lock           *stackLock
	server         Process
	runner         Process
	runnerWaitDone chan struct{}
	runnerWaitErr  error
	// listenAddr is the network door spawnChain bound (port resolved from :0).
	listenAddr string
	// cert is the TLS anchor spawnChain issued, reused when the runner restarts.
	cert      CertResult
	pg        Process
	collector Process
	nats      Process
	// collectorContainerName is the stable name of the bundled collector
	// container when it ran (T4); empty on the --otel-external opt-out path. It
	// is the in-process Down's teardown identity for the collector (the same
	// name persisted in the v2 pgid record for a cross-process down).
	collectorContainerName string
	// collectorHealthEndpoint is the host loopback health_check endpoint the
	// bundled collector published when it ran (T4); empty on the opt-out path.
	// startCollector captures it off the spec it already builds so waitCollector
	// reads the readiness target without rebuilding the spec.
	collectorHealthEndpoint string
	// natsContainerName is the stable name of the bundled nats container when it
	// ran; empty on the --nats-external opt-out path. Down does not read this
	// write-only field: durable teardown identity is the persisted v2 pgid entry.
	// Retained for sibling parity and debuggability.
	natsContainerName string
	// natsMonitorEndpoint is the host loopback HTTP monitoring endpoint the
	// bundled nats published when it ran; empty on the opt-out path. startNats
	// captures it off the spec it already builds so waitNats reads the readiness
	// target without rebuilding the spec.
	natsMonitorEndpoint   string
	gateway               Process
	gatewayHealthEndpoint string
	gatewayContainerName  string
	// pgContainerName is the stable name of the container-backed postgres child
	// when the container path ran (S4); empty on the process and external paths.
	// It is the in-process Down's teardown identity for the container (the same
	// name persisted in the v2 pgid record for a cross-process down).
	pgContainerName string
	// guest is the resolved guest image location for the microVM backend:
	// materialised from GuestArtifact, validated from GuestDir, or zero (the
	// Runner image's baked assets stay live). Resolved before the runner spawn,
	// so a fetch failure prevents the launch instead of a failing preflight.
	guest GuestPaths
	// pgids accumulates each spawned child's teardown identity (pgid +
	// start-time token) in start order, so spawnChain can rewrite the state-dir
	// pgid record after each spawn and a fully successful Down knows the file it
	// wrote is complete. Empty on an attached stack (it spawned nothing).
	pgids []pgidEntry
	// attached is true when Up short-circuited to an already-live server; such a
	// stack spawned nothing and Down must not signal children it does not own.
	attached bool
}

// Up brings the embedded stack to Ready (or attaches to a live one). The
// attach-probe and the spawn decision are serialized under the state-dir O_EXCL
// lockfile, so two concurrent Ups yield exactly one spawning stack and never a
// double spawn.
//
// Cold sequence (devenv.nix:122-143): private postgres up+reachable → TLS anchor
// (expiry-aware) → compass-server → poll GetServerInfo readiness → runner token
// (idempotent 0600) → agent image present → compass-runner (token via env) → poll
// GetServerInfo until this Runner id is enrolled. Attach to a live server skips
// enrollment gating because this stack owns no Runner. On any failure children
// started so far are drained and the lock released.
func Up(ctx context.Context, cfg Config, deps Deps) (*Stack, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// Attach-if-live BEFORE taking the lock is a fast path, but the authoritative
	// probe→spawn decision must be under the lock to close the TOCTOU. We take
	// the lock first; if it is held, another Up is spawning — probe the live
	// server and attach to it (or report contention if it is not yet answering).
	lock, err := acquireLock(cfg.StateDir)
	if err != nil {
		if errors.Is(err, errLockHeld) {
			return attachContended(ctx, cfg, deps)
		}
		return nil, err
	}

	// From here we hold the lock. On any error upLocked leaves the lock held
	// (it only hands lock ownership to a returned Stack, or releases it itself on
	// the attach path), so every error path here releases it exactly once.
	s, err := upLocked(ctx, cfg, deps, lock)
	if err != nil {
		_ = lock.release()
		if errors.Is(err, errVersionMismatchAttached) {
			return nil, ErrVersionMismatch
		}
		return nil, err
	}
	return s, nil
}

// errVersionMismatchAttached is the internal signal that the lock-holding probe
// found a live server of the wrong version. It is mapped to ErrVersionMismatch
// at the Up boundary; kept distinct so the lock-release bookkeeping above can
// tell it apart.
var errVersionMismatchAttached = errors.New("attached to live server with mismatched version")

// upLocked runs the attach-or-spawn decision while holding the lock.
func upLocked(ctx context.Context, cfg Config, deps Deps, lock *stackLock) (*Stack, error) {
	// Under the lock, probe the socket: a live server short-circuits to attach,
	// closing the probe→spawn TOCTOU (no second Up can be spawning, since it
	// would hold this lock).
	if info, err := deps.Prober.Probe(ctx, cfg.SocketPath); err == nil {
		if info.Version != deps.ExpectedVersion {
			return nil, errVersionMismatchAttached
		}
		// Attached: we own no children. Release the lock — an attached stack does
		// not hold the spawn lock (it did not spawn), and a later Down has nothing
		// to serialize.
		if err := lock.release(); err != nil {
			return nil, fmt.Errorf("release lock after attach: %w", err)
		}
		return &Stack{cfg: cfg, deps: deps, attached: true}, nil
	}

	// Checked after the attach probe, not in Validate: status, attach, and down need no image.
	if cfg.GatewayImage == "" && cfg.ExternalGatewayURL == "" {
		return nil, errors.New("stack config: gateway image is required; pass --gateway-image or --gateway-external")
	}

	// Not live — spawn the chain. Accumulate started children so a mid-sequence
	// failure can drain them in reverse.
	s := &Stack{cfg: cfg, deps: deps, lock: lock}
	if err := s.spawnChain(ctx); err != nil {
		return nil, errors.Join(err, s.drainChildren(context.WithoutCancel(ctx)))
	}
	return s, nil
}

// attachContended handles the case where the lock is held by another live Up: we
// did not win the spawn, so the only valid outcome is to attach to the stack
// that Up is bringing (or is) up. If the server already answers, attach; if not
// yet, report contention rather than spawning a second stack.
func attachContended(ctx context.Context, cfg Config, deps Deps) (*Stack, error) {
	info, err := deps.Prober.Probe(ctx, cfg.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("stack lock held by another up and the server is not yet answering: %w", err)
	}
	if info.Version != deps.ExpectedVersion {
		return nil, ErrVersionMismatch
	}
	return &Stack{cfg: cfg, deps: deps, attached: true}, nil
}

// Down stops the stack's children in reverse start order and releases the lock.
// An attached stack owns no children, so Down only releases (a no-op lock, since
// attach released it). Draining is best-effort across children: a stop error on
// one does not skip the rest, and all errors are joined.
//
// On a fully successful drain (no child stop errored) the state-dir pgid record
// is removed too: this process just tore down every child it recorded, so the
// cross-process teardown record has nothing left to describe. A partial or
// failed drain leaves the file in place so a later fresh down can still finish
// the job. An attached stack recorded no children, so removal is a no-op.
// An expired ctx fails each stop and wait fast, so children may survive; the
// record stays in place and a later DownDetached finishes them.
func (s *Stack) Down(ctx context.Context) error {
	err := s.drainChildren(ctx)
	if err == nil {
		// Fully successful drain: this process tore down every child it
		// recorded, so the cross-process teardown record has nothing left to
		// describe. A partial/failed drain leaves the file for a fresh down to
		// finish, and errors.Join with a nil err would just be rerr anyway.
		err = removePgidFile(s.cfg.StateDir)
	}
	if rerr := s.lock.release(); rerr != nil {
		err = errors.Join(err, fmt.Errorf("release lock: %w", rerr))
	}
	return err
}

// ListenAddr reports the network-door address an owned stack bound in Up (the
// resolved port when configured with :0). An attached stack binds nothing, so it
// reports the configured address, which is dialable only when it is a fixed port.
func (s *Stack) ListenAddr() string {
	if s.listenAddr != "" {
		return s.listenAddr
	}
	return s.cfg.ListenAddr
}

// RestartRunner replaces the owned runner process without stopping the server.
func (s *Stack) RestartRunner(ctx context.Context) error {
	if s.attached || s.runner == nil {
		return errors.New("stack: runner is not owned")
	}
	// Check the record first so a bad record fails before anything is torn down.
	if len(s.pgids) == 0 || s.pgids[len(s.pgids)-1].Component != ComponentRunner {
		return errors.New("stack: runner teardown record is missing")
	}
	if err := s.runner.Signal(ctx, SignalTerm); err != nil {
		return fmt.Errorf("stop runner: %w", err)
	}
	if err := s.waitRunnerProcess(ctx); err != nil {
		return fmt.Errorf("wait for runner: %w", err)
	}
	s.runner = nil
	s.runnerWaitDone = nil
	s.runnerWaitErr = nil
	s.pgids = s.pgids[:len(s.pgids)-1]
	if err := writePgidFile(s.cfg.StateDir, pgidRecord{WriterPid: os.Getpid(), Version: pgidFileVersion, Entries: s.pgids}); err != nil {
		return fmt.Errorf("persist runner stop: %w", err)
	}
	if err := s.startRunner(ctx); err != nil {
		return err
	}
	return s.waitRunnerEnrolled(ctx)
}

// Health probes current readiness by asking the server over the socket. An
// answering probe is Ready (or Attached, for a stack that never spawned); a
// failing probe is Failed with the probe error as the detail.
func (s *Stack) Health(ctx context.Context) (Status, error) {
	info, err := s.deps.Prober.Probe(ctx, s.cfg.SocketPath)
	if err != nil {
		return Status{State: StatusFailed, Detail: err.Error()}, nil //nolint:nilerr // a failing probe is reported as Status{Failed} with the error as Detail — Health itself succeeded, so it returns no call error
	}
	state := StatusReady
	if s.attached {
		state = StatusAttached
	}
	return Status{State: state, Detail: "server version " + info.Version}, nil
}

// startRunner mints the enrollment token, checks the agent image, and spawns
// and records compass-runner (token via env only).
func (s *Stack) startRunner(ctx context.Context) error {
	token, err := s.deps.Tokens.EnsureToken(ctx, s.cfg.StateDir, embeddedRunnerID)
	if err != nil {
		return fmt.Errorf("ensure runner token: %w", err)
	}
	// The agent ships inside the guest rootfs under microVM; there is no image to pull.
	if !s.cfg.microVM() {
		if err := s.deps.Images.EnsureImage(ctx, s.cfg.AgentImage); err != nil {
			return fmt.Errorf("ensure agent image: %w", err)
		}
	}
	// Resolve and verify guest paths before spawning, so a bad guest fails here.
	if err := s.resolveGuest(ctx); err != nil {
		return err
	}
	runner, err := s.deps.Supervisor.Start(ctx, runnerSpec(s.cfg, s.cert, token, s.guest, os.Getenv(microVMRunRootEnvVar), s.ListenAddr()))
	if err != nil {
		return fmt.Errorf("start compass-runner: %w", err)
	}
	s.runner = runner
	return s.recordChild(ComponentRunner, runner)
}

// spawnChain runs the cold-start sequence in order. Each spawned child is
// recorded on the Stack before the next step, so drainChildren can reverse
// exactly what started.
func (s *Stack) spawnChain(ctx context.Context) error {
	if err := s.cfg.checkBundledPortsDistinct(); err != nil {
		return err
	}
	// 0. Bind the network door before any child starts, so no other process can
	// take the port; the server inherits it as fd 3 and :0 resolves here.
	listen, err := s.listenTCP("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("bind stack network door at %s: %w", s.cfg.ListenAddr, err)
	}
	// Covers every return before the server starts; after it, the explicit
	// Close below has already run and this second Close is a harmless no-op.
	defer func() { _ = listen.Close() }()
	s.listenAddr = listen.Addr().String()

	// 1. Private postgres child. Three paths (S4): external (skip the component,
	// probe the caller's DSN as-is), container-backed (the installed default), or
	// the dev-path wrapper process. Start returns at launch, not readiness — the
	// waitPostgres poll below is the readiness gate for all three.
	if err := s.startPostgres(ctx); err != nil {
		return err
	}

	// 1b. Wait until postgres is accepting — Supervisor.Start returned at
	// process launch, not at readiness; compass-server's store.Open pings once
	// and does not retry, so it must not start before postgres is reachable.
	if err := s.waitPostgres(ctx); err != nil {
		return err
	}

	// 1c. Bundled OTel Collector. Placed early — before server and runner, which
	// emit TO it — so the fan-in endpoint is receiving before an emitter comes up.
	// On --otel-external (ExternalOTLPEndpoint set) startCollector is a no-op and no
	// readiness gate runs. Start returns at launch; waitCollector is the gate.
	if err := s.startCollector(ctx); err != nil {
		return err
	}
	if err := s.waitCollector(ctx); err != nil {
		return err
	}

	// 1d. Bundled NATS. Grouped after postgres and the collector, before the server
	// and runner that will CONNECT to it (cutover PR3/PR4), so the broker is
	// accepting before a consumer comes up. On --nats-external (ExternalNatsURL set)
	// startNats is a no-op. Start returns at launch; waitNats is the gate.
	if err := s.startNats(ctx); err != nil {
		return err
	}
	if err := s.waitNats(ctx); err != nil {
		return err
	}

	// 1e. Bundled LLM gateway, healthy before any agent can call it. On
	// --gateway-external startGateway is a no-op; waitGateway is the gate.
	if err := s.startGateway(ctx); err != nil {
		return err
	}
	if err := s.waitGateway(ctx); err != nil {
		return err
	}

	// 2. TLS anchor, expiry-aware (rotates when NotAfter is within the window).
	cert, err := s.deps.Certs.EnsureCert(ctx, s.cfg.StateDir, s.deps.now())
	if err != nil {
		return fmt.Errorf("ensure tls anchor: %w", err)
	}
	s.cert = cert

	// 3. compass-server (socket / listen-fd / tls / database).
	listenFile, err := listen.File()
	if err != nil {
		return fmt.Errorf("duplicate stack network listener: %w", err)
	}
	server, err := s.deps.Supervisor.Start(ctx, serverSpec(s.cfg, cert, listenFile))
	// The child holds its own copy now. Keeping ours open would let the kernel
	// queue connections for a dead server instead of refusing them.
	closeErr := errors.Join(listenFile.Close(), listen.Close())
	if err != nil {
		return fmt.Errorf("start compass-server: %w", errors.Join(err, closeErr))
	}
	s.server = server
	if err := s.recordChild(ComponentServer, server); err != nil {
		return err
	}
	if closeErr != nil {
		return fmt.Errorf("release stack network listener: %w", closeErr)
	}

	// 4. Poll GetServerInfo readiness — the socket binds before migrations, so
	// only an answering probe means ready.
	if err := s.waitReady(ctx); err != nil {
		return err
	}

	// 5-7. Runner token, agent image, guest paths, then compass-runner.
	if err := s.startRunner(ctx); err != nil {
		return err
	}
	return s.waitRunnerEnrolled(ctx)
}

// resolveGuest resolves the microVM guest image to concrete paths. Validate
// already rejected the incoherent combinations, so three arms are exhaustive:
// fetch a GuestArtifact into the content-addressed dir, validate a GuestDir
// as-is, or leave the paths zero and keep the Runner image's baked assets live.
func (s *Stack) resolveGuest(ctx context.Context) error {
	switch {
	case s.cfg.GuestArtifact != "":
		paths, err := materializeGuest(ctx, s.cfg.GuestArtifact, s.cfg.StateDir)
		if err != nil {
			return fmt.Errorf("materialise guest image %q: %w", s.cfg.GuestArtifact, err)
		}
		s.guest = paths
	case s.cfg.GuestDir != "":
		paths, err := resolveGuestDir(s.cfg.GuestDir)
		if err != nil {
			return fmt.Errorf("resolve guest dir: %w", err)
		}
		s.guest = paths
	}
	return nil
}

// startPostgres brings up the private store-of-record via the path Config
// selects (S4), or skips it entirely for an external database. All three paths
// leave readiness to the waitPostgres poll spawnChain runs next — Start returns
// at launch, not at accept.
//
//   - ExternalDatabase: no postgres component starts. The caller points the
//     stack at their own postgres; spawnChain probes DatabaseDSN as-is. Nothing
//     is recorded, so a down tears down only server+runner.
//   - PostgresImage set: the container-backed path (the installed default). The
//     container is a supervised child (its Process handle drives the in-process
//     Down), and its durable teardown identity — the stable name — is persisted
//     as a v2 container entry so a fresh cross-process down tears it down by name.
//   - PostgresImage empty: the dev/devenv path, today's ProcessSupervisor
//     LookPath spawn of the compass-postgres wrapper, unchanged.
func (s *Stack) startPostgres(ctx context.Context) error {
	if s.cfg.ExternalDatabase {
		return nil
	}
	if s.cfg.PostgresImage != "" {
		return s.startPostgresContainer(ctx)
	}
	pg, err := s.deps.Supervisor.Start(ctx, ProcessSpec{
		Component: ComponentPostgres,
		Args:      []string{"--state-dir", s.cfg.StateDir, "--database", s.cfg.DatabaseDSN},
	})
	if err != nil {
		return fmt.Errorf("start postgres: %w", err)
	}
	s.pg = pg
	return s.recordChild(ComponentPostgres, pg)
}

// startPostgresContainer runs the S4 container-backed postgres and records it as
// a v2 container entry (torn down by name, never by pgid — a rootless container
// runs beneath conmon, outside the client's process group). The Process handle
// is held on the Stack so the in-process Down drains it (Signal → podman stop);
// the persisted name is what a fresh cross-process down reconstructs and signals.
func (s *Stack) startPostgresContainer(ctx context.Context) error {
	spec, err := postgresContainerSpec(s.cfg)
	if err != nil {
		return err
	}
	pg, err := s.deps.PostgresContainer.Start(ctx, spec)
	if err != nil {
		return fmt.Errorf("start postgres container: %w", err)
	}
	s.pg = pg
	s.pgContainerName = spec.Name
	return s.appendEntry(ComponentPostgres, pgidEntry{Kind: entryContainer, Component: ComponentPostgres, ContainerName: spec.Name})
}

// startCollector brings up the bundled Plane-B fan-in OTel Collector (T4 / D3)
// and records it as a v2 container entry (torn down by name, never by pgid — a
// rootless container runs beneath conmon, outside the client's process group),
// exactly like startPostgresContainer. The Process handle is held on the Stack
// so the in-process Down drains it (Signal → podman stop); the persisted name is
// what a fresh cross-process down reconstructs and signals. On the
// --otel-external opt-out (ExternalOTLPEndpoint set) it is a no-op: no bundled
// collector starts, nothing is recorded, and a down tears down only the other
// children — the collector analogue of startPostgres's ExternalDatabase early
// return.
func (s *Stack) startCollector(ctx context.Context) error {
	if s.cfg.ExternalOTLPEndpoint != "" {
		return nil
	}
	spec, err := collectorContainerSpec(s.cfg)
	if err != nil {
		return err
	}
	if s.deps.CollectorContainer == nil {
		return errors.New("start otel-collector: CollectorContainer dep is nil on the bundle path (ExternalOTLPEndpoint unset but no collector adapter wired) — a legible failure, not a nil-deref panic")
	}
	col, err := s.deps.CollectorContainer.Start(ctx, spec)
	if err != nil {
		return fmt.Errorf("start otel-collector container: %w", err)
	}
	s.collector = col
	s.collectorContainerName = spec.Name
	s.collectorHealthEndpoint = spec.HealthEndpoint
	return s.appendEntry(ComponentCollector, pgidEntry{Kind: entryContainer, Component: ComponentCollector, ContainerName: spec.Name})
}

// startNats brings up the bundled NATS container and records it as a v2
// container entry (torn down by name, never by pgid — a rootless container runs
// beneath conmon, outside the client's process group), exactly like
// startCollector. The Process handle is held on the Stack so the in-process Down
// drains it (Signal → podman stop); the persisted name is what a fresh
// cross-process down reconstructs and signals. On the --nats-external opt-out
// (ExternalNatsURL set) it is a no-op: no bundled nats starts, nothing is
// recorded, and a down tears down only the other children — the nats analogue of
// startCollector's ExternalOTLPEndpoint early return.
func (s *Stack) startNats(ctx context.Context) error {
	if s.cfg.ExternalNatsURL != "" {
		return nil
	}
	spec, err := natsContainerSpec(s.cfg)
	if err != nil {
		return err
	}
	if s.deps.NatsContainer == nil {
		return errors.New("start nats: NatsContainer dep is nil on the bundle path (ExternalNatsURL unset but no nats adapter wired) — a legible failure, not a nil-deref panic")
	}
	n, err := s.deps.NatsContainer.Start(ctx, spec)
	if err != nil {
		return fmt.Errorf("start nats container: %w", err)
	}
	s.nats = n
	s.natsContainerName = spec.Name
	s.natsMonitorEndpoint = spec.MonitorEndpoint
	return s.appendEntry(ComponentNats, pgidEntry{Kind: entryContainer, Component: ComponentNats, ContainerName: spec.Name})
}

// startGateway runs the gateway container and records its v2 container entry.
// On --gateway-external it is a no-op.
func (s *Stack) startGateway(ctx context.Context) error {
	if s.cfg.ExternalGatewayURL != "" {
		return nil
	}
	spec, err := gatewayContainerSpec(s.cfg)
	if err != nil {
		return err
	}
	if s.deps.GatewayContainer == nil {
		return errors.New("start gateway: GatewayContainer dep is nil on the bundle path (ExternalGatewayURL unset but no gateway adapter wired)")
	}
	p, err := s.deps.GatewayContainer.Start(ctx, spec)
	if err != nil {
		return fmt.Errorf("start gateway container: %w", err)
	}
	s.gateway, s.gatewayContainerName, s.gatewayHealthEndpoint = p, spec.Name, spec.HealthEndpoint
	return s.appendEntry(ComponentGateway, pgidEntry{Kind: entryContainer, Component: ComponentGateway, ContainerName: spec.Name})
}

// waitGateway polls /healthz until the gateway answers or the budget elapses.
func (s *Stack) waitGateway(ctx context.Context) error {
	if s.cfg.ExternalGatewayURL != "" {
		return nil
	}
	if s.deps.GatewayProber == nil {
		return errors.New("wait gateway: GatewayProber dep is nil on the bundle path (ExternalGatewayURL unset but no gateway adapter wired)")
	}
	deadline := s.deps.now().Add(gatewayReadyPollBudget)
	ticker := time.NewTicker(gatewayReadyPollInterval)
	defer ticker.Stop()
	for {
		if err := s.deps.GatewayProber.ProbeGateway(ctx, s.gatewayHealthEndpoint); err == nil {
			return nil
		}
		if !s.deps.now().Before(deadline) {
			return fmt.Errorf("gateway did not answer healthy within %s", gatewayReadyPollBudget)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// recordChild appends a spawned process child's teardown identity (pgid == pid,
// plus the leader start-time token read at spawn) and rewrites the state-dir
// pgid record so it reflects every child started so far. Rewriting after each
// spawn keeps the crash window one child wide: the atomically-renamed file on
// disk is always a complete earlier prefix of the start sequence, so a fresh
// down never reads a torn record and drains exactly the prefix that was started.
func (s *Stack) recordChild(c Component, p Process) error {
	startTime, err := readStartTime(p.Pid())
	if err != nil {
		return fmt.Errorf("read start time for %s (pid %d): %w", c, p.Pid(), err)
	}
	return s.appendEntry(c, pgidEntry{Kind: entryProc, Component: c, Pgid: p.Pid(), StartTime: startTime})
}

// appendEntry appends one teardown entry and republishes the record, preserving
// the rewrite-after-each-spawn / one-child-wide-crash-window discipline both
// record paths share.
func (s *Stack) appendEntry(c Component, e pgidEntry) error {
	s.pgids = append(s.pgids, e)
	rec := pgidRecord{WriterPid: os.Getpid(), Version: pgidFileVersion, Entries: s.pgids}
	if err := writePgidFile(s.cfg.StateDir, rec); err != nil {
		return fmt.Errorf("persist pgid record after starting %s: %w", c, err)
	}
	return nil
}

// waitReady polls GetServerInfo until the server answers or the budget elapses.
// A budget timeout is the failure surface — a legible error the caller renders as
// Failed. The poll respects ctx cancellation.
func (s *Stack) waitReady(ctx context.Context) error {
	deadline := s.deps.now().Add(readyPollBudget)
	ticker := time.NewTicker(readyPollInterval)
	defer ticker.Stop()
	for {
		if _, err := s.deps.Prober.Probe(ctx, s.cfg.SocketPath); err == nil {
			return nil
		}
		if !s.deps.now().Before(deadline) {
			return fmt.Errorf("compass-server did not answer GetServerInfo within %s", readyPollBudget)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// waitRunnerEnrolled polls GetServerInfo until embeddedRunnerID is enrolled.
// It fails if the Runner exits first or the enrollment budget expires.
func (s *Stack) waitRunnerEnrolled(ctx context.Context) error {
	if s.runner == nil {
		return errors.New("wait for Runner enrollment: runner process is not owned")
	}
	if s.runnerWaitDone == nil {
		s.runnerWaitDone = make(chan struct{})
		go func(runner Process, done chan struct{}) {
			s.runnerWaitErr = runner.Wait(context.WithoutCancel(ctx))
			close(done)
		}(s.runner, s.runnerWaitDone)
	}

	deadline := s.deps.now().Add(runnerEnrollPollBudget)
	ticker := time.NewTicker(runnerEnrollPollInterval)
	defer ticker.Stop()
	for {
		info, err := s.deps.Prober.Probe(ctx, s.cfg.SocketPath)
		if err == nil && slices.Contains(info.EnrolledRunnerIDs, embeddedRunnerID) {
			return nil
		}
		if !s.deps.now().Before(deadline) {
			return fmt.Errorf("compass-runner %q was not enrolled within %s", embeddedRunnerID, runnerEnrollPollBudget)
		}
		select {
		case <-s.runnerWaitDone:
			if s.runnerWaitErr == nil {
				return errors.New("compass-runner exited before enrolling")
			}
			return fmt.Errorf("compass-runner exited before enrolling: %w", s.runnerWaitErr)
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Stack) waitRunnerProcess(ctx context.Context) error {
	if s.runnerWaitDone == nil {
		return s.runner.Wait(ctx)
	}
	select {
	case <-s.runnerWaitDone:
		return s.runnerWaitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitPostgres polls DBProber.ProbeDB until postgres accepts connections on the
// full DSN (dbname=compass) or the budget elapses — a direct mirror of waitReady
// for the postgres precondition. Probing the full DSN (not just the socket)
// validates the exact state store.Open needs: postgres accepting AND the compass
// database created by the postgres wrapper's ensureDatabase. A budget timeout is
// a legible error the caller renders as Failed. The poll respects ctx
// cancellation.
func (s *Stack) waitPostgres(ctx context.Context) error {
	deadline := s.deps.now().Add(dbReadyPollBudget)
	ticker := time.NewTicker(dbReadyPollInterval)
	defer ticker.Stop()
	for {
		if err := s.deps.DBProber.ProbeDB(ctx, s.cfg.DatabaseDSN); err == nil {
			return nil
		}
		if !s.deps.now().Before(deadline) {
			return fmt.Errorf("postgres did not accept connections within %s", dbReadyPollBudget)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// waitCollector polls CollectorProber.ProbeCollector until the bundled
// collector answers healthy on its health_check endpoint or the budget elapses
// — a direct mirror of waitPostgres for the collector precondition, since
// CollectorContainer.Start returns at launch. On the --otel-external opt-out no
// collector was started (deps.CollectorProber is nil), so the gate is skipped
// entirely — mirroring how startCollector no-ops on that path. A budget timeout
// is a legible error the caller renders as Failed. The poll respects ctx
// cancellation.
func (s *Stack) waitCollector(ctx context.Context) error {
	if s.cfg.ExternalOTLPEndpoint != "" {
		return nil
	}
	if s.deps.CollectorProber == nil {
		return errors.New("wait otel-collector: CollectorProber dep is nil on the bundle path (ExternalOTLPEndpoint unset but no collector adapter wired) — a legible failure, not a nil-deref panic")
	}
	deadline := s.deps.now().Add(collectorReadyPollBudget)
	ticker := time.NewTicker(collectorReadyPollInterval)
	defer ticker.Stop()
	for {
		if err := s.deps.CollectorProber.ProbeCollector(ctx, s.collectorHealthEndpoint); err == nil {
			return nil
		}
		if !s.deps.now().Before(deadline) {
			return fmt.Errorf("otel-collector did not answer healthy within %s", collectorReadyPollBudget)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// waitNats polls NatsProber.ProbeNats until the bundled NATS answers healthy on
// its HTTP monitoring endpoint or the budget elapses — a direct mirror of
// waitCollector for the nats precondition, since NatsContainer.Start returns at
// launch. On the --nats-external opt-out no nats was started (deps.NatsProber is
// nil), so the gate is skipped entirely — mirroring how startNats no-ops on that
// path. A budget timeout is a legible error the caller renders as Failed. The
// poll respects ctx cancellation.
func (s *Stack) waitNats(ctx context.Context) error {
	if s.cfg.ExternalNatsURL != "" {
		return nil
	}
	if s.deps.NatsProber == nil {
		return errors.New("wait nats: NatsProber dep is nil on the bundle path (ExternalNatsURL unset but no nats adapter wired) — a legible failure, not a nil-deref panic")
	}
	deadline := s.deps.now().Add(natsReadyPollBudget)
	ticker := time.NewTicker(natsReadyPollInterval)
	defer ticker.Stop()
	for {
		if err := s.deps.NatsProber.ProbeNats(ctx, s.natsMonitorEndpoint); err == nil {
			return nil
		}
		if !s.deps.now().Before(deadline) {
			return fmt.Errorf("nats did not answer healthy within %s", natsReadyPollBudget)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// drainChildren signals and waits each owned child in reverse start order.
func (s *Stack) drainChildren(ctx context.Context) error {
	var errs error
	for _, c := range []struct {
		name string
		p    Process
	}{
		{ComponentRunner.String(), s.runner},
		{ComponentServer.String(), s.server},
		{"llm-gateway", s.gateway},
		{"nats", s.nats},
		{"otel-collector", s.collector},
		{"postgres", s.pg},
	} {
		if c.p == nil {
			continue
		}
		if err := c.p.Signal(ctx, SignalTerm); err != nil {
			errs = errors.Join(errs, fmt.Errorf("signal %s: %w", c.name, err))
			continue
		}
		if c.name == ComponentRunner.String() && s.runnerWaitDone != nil {
			if err := s.waitRunnerProcess(ctx); err != nil {
				errs = errors.Join(errs, fmt.Errorf("wait %s: %w", c.name, err))
			}
			continue
		}
		if err := c.p.Wait(ctx); err != nil {
			errs = errors.Join(errs, fmt.Errorf("wait %s: %w", c.name, err))
		}
	}
	s.runnerWaitDone = nil
	s.runnerWaitErr = nil
	s.runner, s.server, s.gateway, s.nats, s.collector, s.pg = nil, nil, nil, nil, nil, nil
	return errs
}

// listenTCP binds the network door through the Deps seam when one is set.
func (s *Stack) listenTCP(network, address string) (*net.TCPListener, error) {
	if s.deps.ListenTCP != nil {
		return s.deps.ListenTCP(network, address)
	}
	addr, err := net.ResolveTCPAddr(network, address)
	if err != nil {
		return nil, err
	}
	return net.ListenTCP(network, addr)
}
