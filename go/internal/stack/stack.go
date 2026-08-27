//go:build unix

package stack

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	// between starting the postgres child and compass-server. The budget is
	// larger than readyPollBudget because this waits on cold cluster init
	// (initdb + postgres start + createdb), not a server bind: initdb+createdb
	// measured ~5.5s on an idle box, but runs far heavier on a loaded shared
	// box, so 60s leaves generous headroom while still failing a genuinely
	// wedged cluster legibly rather than hanging.
	dbReadyPollInterval = 100 * time.Millisecond
	dbReadyPollBudget   = 60 * time.Second
	// collectorReadyPollInterval/collectorReadyPollBudget bound the
	// collector-health poll between launching the bundled collector and the
	// components that emit to it. The collector holds no on-disk state and does
	// no cold init like postgres's initdb — it binds its OTLP receivers and
	// health_check extension in well under a second once the image is present —
	// so the budget is the smaller readyPollBudget-tier value, ample for a cold
	// `podman run` of a present image while still failing a genuinely wedged
	// collector legibly rather than hanging.
	collectorReadyPollInterval = 100 * time.Millisecond
	collectorReadyPollBudget   = 30 * time.Second
)

// Stack is a supervised embedded stack: the resolved config plus the child
// handles this process owns. An attached stack (a live server was already
// answering) owns no children — Down on it only releases the lock.
type Stack struct {
	cfg       Config
	deps      Deps
	lock      *stackLock
	server    Process
	runner    Process
	pg        Process
	collector Process
	// collectorContainerName is the stable name of the bundled collector
	// container when it ran (T4); empty on the --otel-external opt-out path. It
	// is the in-process Down's teardown identity for the collector (the same
	// name persisted in the v2 pgid record for a cross-process down).
	collectorContainerName string
	// pgContainerName is the stable name of the container-backed postgres child
	// when the container path ran (S4); empty on the process and external paths.
	// It is the in-process Down's teardown identity for the container (the same
	// name persisted in the v2 pgid record for a cross-process down).
	pgContainerName string
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
// (idempotent 0600) → agent image present → compass-runner (token via env). On
// any step failure the children started so far are drained and the lock
// released, so no half-started stack leaks.
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

// spawnChain runs the cold-start sequence in order. Each spawned child is
// recorded on the Stack before the next step, so drainChildren can reverse
// exactly what started.
func (s *Stack) spawnChain(ctx context.Context) error {
	// 1. Private postgres child. Three paths (S4): external (skip the component
	// entirely, probe the caller's DSN as-is), container-backed (the installed
	// default), or the dev-path wrapper process. Start returns at launch, not at
	// readiness — the waitPostgres poll below is the readiness gate for all
	// three.
	if err := s.startPostgres(ctx); err != nil {
		return err
	}

	// 1b. Wait until postgres is accepting — Supervisor.Start returned at
	// process launch, not at readiness; compass-server's store.Open pings once
	// and does not retry, so it must not start before postgres is reachable.
	if err := s.waitPostgres(ctx); err != nil {
		return err
	}

	// 1c. Bundled Plane-B fan-in OTel Collector (T4 / D3). Placed early — before
	// compass-server and compass-runner — because those are the surfaces that
	// emit TO it (server/runner emission lands in T4b; the agent already emits),
	// so the fan-in endpoint must be receiving before an emitter comes up. On
	// the --otel-external opt-out (ExternalOTLPEndpoint set) startCollector is a
	// no-op and no readiness gate runs: surfaces point straight at the external
	// endpoint, so nothing bundled starts (mirrors startPostgres's early return
	// on ExternalDatabase). Start returns at launch; waitCollector is the
	// readiness gate.
	if err := s.startCollector(ctx); err != nil {
		return err
	}
	if err := s.waitCollector(ctx); err != nil {
		return err
	}

	// 2. TLS anchor, expiry-aware (rotates when NotAfter is within the window).
	cert, err := s.deps.Certs.EnsureCert(ctx, s.cfg.StateDir, s.deps.now())
	if err != nil {
		return fmt.Errorf("ensure tls anchor: %w", err)
	}

	// 3. compass-server (socket / listen / tls / database).
	server, err := s.deps.Supervisor.Start(ctx, serverSpec(s.cfg, cert))
	if err != nil {
		return fmt.Errorf("start compass-server: %w", err)
	}
	s.server = server
	if err := s.recordChild(ComponentServer, server); err != nil {
		return err
	}

	// 4. Poll GetServerInfo readiness — the socket binds before migrations, so
	// only an answering probe means ready.
	if err := s.waitReady(ctx); err != nil {
		return err
	}

	// 5. Runner enrollment token (idempotent, 0600), minted for the embedded
	// runner id the runner spawn cross-checks against.
	token, err := s.deps.Tokens.EnsureToken(ctx, s.cfg.StateDir, embeddedRunnerID)
	if err != nil {
		return fmt.Errorf("ensure runner token: %w", err)
	}

	// 6. Agent image present in the local store.
	if err := s.deps.Images.EnsureImage(ctx, s.cfg.AgentImage); err != nil {
		return fmt.Errorf("ensure agent image: %w", err)
	}

	// 7. compass-runner (token via env only).
	runner, err := s.deps.Supervisor.Start(ctx, runnerSpec(s.cfg, cert, token))
	if err != nil {
		return fmt.Errorf("start compass-runner: %w", err)
	}
	s.runner = runner
	if err := s.recordChild(ComponentRunner, runner); err != nil {
		return err
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
	col, err := s.deps.CollectorContainer.Start(ctx, spec)
	if err != nil {
		return fmt.Errorf("start otel-collector container: %w", err)
	}
	s.collector = col
	s.collectorContainerName = spec.Name
	return s.appendEntry(ComponentCollector, pgidEntry{Kind: entryContainer, Component: ComponentCollector, ContainerName: spec.Name})
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
	spec, err := collectorContainerSpec(s.cfg)
	if err != nil {
		return err
	}
	deadline := s.deps.now().Add(collectorReadyPollBudget)
	ticker := time.NewTicker(collectorReadyPollInterval)
	defer ticker.Stop()
	for {
		if err := s.deps.CollectorProber.ProbeCollector(ctx, spec.HealthEndpoint); err == nil {
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

// drainChildren signals and waits each owned child in reverse start order
// (runner → server → collector → postgres). It is safe to call with nil handles
// (a mid-sequence failure) and on an attached stack (all nil).
func (s *Stack) drainChildren(ctx context.Context) error {
	var errs error
	for _, c := range []struct {
		name string
		p    Process
	}{
		{"compass-runner", s.runner},
		{"compass-server", s.server},
		{"otel-collector", s.collector},
		{"postgres", s.pg},
	} {
		if c.p == nil {
			continue
		}
		if err := c.p.Signal(SignalTerm); err != nil {
			errs = errors.Join(errs, fmt.Errorf("signal %s: %w", c.name, err))
			continue
		}
		if err := c.p.Wait(ctx); err != nil {
			errs = errors.Join(errs, fmt.Errorf("wait %s: %w", c.name, err))
		}
	}
	s.runner, s.server, s.collector, s.pg = nil, nil, nil, nil
	return errs
}
