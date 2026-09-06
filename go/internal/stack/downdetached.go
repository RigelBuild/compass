//go:build unix

package stack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Per-component drain budgets: after SIGTERM, how long DownDetached waits for a
// component's confirmation channel to go quiet before escalating to a group
// SIGKILL. Reverse start order (runner → server → nats → collector → postgres); the
// server's graceful drain is the long pole. On the SIGTERM-succeeds path they
// sum to 85s; on the escalation path the four non-runner components each add a
// postKillGrace, so the true worst case is 15 + (30+5) + (20+5) + (10+5) + (10+5) = 105s.
// That can exceed the app's 60s stackDownTimeout (lifecycle.go:35) — and is
// bounded by its ctx cancellation, not by this arithmetic: on ctx.Done waitDead
// returns the current dead() verdict, so an overrun becomes a partial-failure
// survivor rewrite + loud report (Open Question 2), never an unbounded wait.
// They are package vars, not consts, so tests can shrink them; the budget is
// enforced via deps.now(), so a test drives expiry with a controlled clock
// rather than real waiting.
var (
	runnerDrainBudget   = 15 * time.Second
	serverDrainBudget   = 30 * time.Second
	postgresDrainBudget = 10 * time.Second
	// collectorDrainBudget bounds the collector container's graceful `podman
	// stop` before the `podman rm -f` escalation. The collector holds no on-disk
	// state to drain (D3 drops rather than buffering), so it stops fast; the
	// budget matches postgres's container-drain tier for parity.
	collectorDrainBudget = 10 * time.Second
	// natsDrainBudget bounds the nats container's graceful `podman stop` before
	// the `podman rm -f` escalation. Unlike the collector, NATS flushes its
	// JetStream file store on SIGTERM, so it gets the wider budget its in-process
	// natsStopTimeout also reserves — a `rm -f` mid-flush is exactly the
	// unclean-shutdown case the store has to recover from on next boot.
	natsDrainBudget = 20 * time.Second
	// postKillGrace bounds the confirm after the hard kill for the non-runner
	// components (server, collector, nats, postgres): the kill is unblockable
	// (group SIGKILL for the socket children, `podman rm -f` for the container),
	// so a component still confirming alive past this grace — a socket still
	// answering, or the container still existing — is a genuine survivor, not a
	// zombie. The runner (group-ESRCH confirmed) needs no grace — see drainTarget.
	postKillGrace = 5 * time.Second
	// downPollInterval paces the confirmation polls. Real wall-time; a test
	// shrinks it so the suite does not pay a full interval per poll.
	downPollInterval = 100 * time.Millisecond
)

// ErrStackStarting is returned by DownDetached when a live up holds the state-dir
// lock — a stack is mid-bring-up and must not be torn down half-spawned. The
// caller retries once the stack is up.
var ErrStackStarting = errors.New("a stack is starting; retry once it is up")

// ErrNoTeardownRecord is returned when a stack is live (its server answers) but
// there is no pgid file to tear it down by — spawned by an older build, or the
// file was removed. DownDetached never guesses at pids, so it refuses legibly
// rather than signal blindly (Open Question 1).
var ErrNoTeardownRecord = errors.New("a stack is live but this build holds no teardown record; stop it with the build that started it")

// DownDetached tears down a stack a prior, now-exited up spawned — the
// cross-process teardown this process holds no in-memory child handles for. It
// reads the persisted pgid record, identity-checks each recorded group, SIGTERMs
// the live ones in reverse start order with bounded SIGKILL escalation, and
// confirms teardown per component by the channel each has (server/postgres by
// socket quiescence, the socketless runner by group-ESRCH). Only pgids read from
// this stack's own state-dir file are ever signaled, and each group's identity
// (pgid + leader start-time token) is re-verified immediately before every
// signal.
func DownDetached(ctx context.Context, cfg Config, deps Deps) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	// 1. Refuse to race a live up. The guard flock does NOT cover this — up
	// releases the guard before spawnChain (lockfile.go:41-48) — so the
	// lockfile-holder check is the real interlock: a live holder means an up is
	// in flight (possibly parked in waitReady, not having written every child
	// line yet), and tearing down a half-spawned set is wrong.
	lockPath := filepath.Join(cfg.StateDir, lockFileName)
	if live, err := lockHolderLive(lockPath); err != nil {
		return fmt.Errorf("inspect stack lock: %w", err)
	} else if live {
		return ErrStackStarting
	}

	// 2. Read the record under the guard flock and CONSUME it (remove it) so two
	// concurrent downs cannot both consume the same record and double-signal —
	// the loser re-reads, finds no file, and no-ops. The guard is held ONLY for
	// this read+consume decision and released before the long signal→wait→SIGKILL
	// sequence, so a down never blocks a concurrent up's acquireGuard for the
	// whole drain budget.
	rec, consumed, err := consumeRecord(ctx, cfg, deps)
	if err != nil {
		return err
	}
	if !consumed {
		// Absent file + no answering socket → nothing to do.
		return nil
	}

	// 3. Build the live teardown targets in reverse start order, identity-checking
	// each recorded group. A gone (ESRCH) or recycled (start-time mismatch) group
	// is skipped — never signaled, never an error.
	targets := liveTargets(ctx, cfg, deps, rec)

	// 4/5/6. SIGTERM every live target up front (reverse order), then per-target
	// bounded wait → SIGKILL escalation → per-component confirmation.
	survivors := drainTargets(ctx, deps, targets)

	// 7. Removal / partial-failure policy.
	if len(survivors) == 0 {
		// Full success: the record was already consumed (removed) in step 2, and
		// the stale lockfile (its up holder is long dead in the linger case) is
		// cleared so the state dir is left clean.
		if err := clearLockFile(cfg.StateDir); err != nil {
			return fmt.Errorf("clear stale lock after teardown: %w", err)
		}
		return nil
	}
	// Partial: re-publish the surviving set so a retried down (or a human) can
	// finish. Removing the record would orphan the survivors with nothing to
	// retry against.
	if werr := writePgidFile(cfg.StateDir, survivorRecord(rec, survivors)); werr != nil {
		return errors.Join(survivorError(survivors), fmt.Errorf("rewrite pgid record to survivors: %w", werr))
	}
	return survivorError(survivors)
}

// consumeRecord takes the guard flock, reads the pgid file, and removes it
// (taking ownership) before releasing the guard. It reports consumed=false only
// for the absent-file case with no answering socket ("no stack"); an absent file
// WITH an answering socket is the no-teardown-record error (Open Question 1).
func consumeRecord(ctx context.Context, cfg Config, deps Deps) (rec pgidRecord, consumed bool, err error) {
	guard, err := acquireGuard(cfg.StateDir)
	if err != nil {
		return pgidRecord{}, false, fmt.Errorf("acquire guard for pgid read: %w", err)
	}
	defer guard.release()

	rec, err = readPgidFile(cfg.StateDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No record. If a server is nonetheless answering, this build cannot
			// tear it down safely (never guess at pids); refuse legibly.
			if _, perr := deps.Prober.Probe(ctx, cfg.SocketPath); perr == nil {
				return pgidRecord{}, false, ErrNoTeardownRecord
			}
			return pgidRecord{}, false, nil
		}
		return pgidRecord{}, false, fmt.Errorf("read pgid record: %w", err)
	}

	// Consume: remove the record under the guard so a concurrent down cannot also
	// act on it. A partial teardown re-publishes the survivor set at the end.
	if rerr := removePgidFile(cfg.StateDir); rerr != nil {
		return pgidRecord{}, false, fmt.Errorf("consume pgid record: %w", rerr)
	}
	return rec, true, nil
}

// target is one live child to tear down: its recorded identity plus the
// component-specific confirmation channel and drain budget.
type target struct {
	entry   pgidEntry
	budget  time.Duration
	confirm func() bool // reports the component confirmed dead by its own channel
}

// liveTargets returns the identity-matched live groups in reverse start order
// (runner → server → nats → collector → postgres). Each recorded group is checked with
// GroupSignaller.Alive (existence AND start-time identity); a gone or recycled
// group is omitted — never signaled.
func liveTargets(ctx context.Context, cfg Config, deps Deps, rec pgidRecord) []target {
	// Index entries by component so we can emit them in reverse start order
	// regardless of the file's line order (which is start order by construction).
	byComp := map[Component]pgidEntry{}
	for _, e := range rec.Entries {
		byComp[e.Component] = e
	}

	order := []struct {
		comp    Component
		budget  time.Duration
		confirm func(pgidEntry) func() bool
	}{
		{ComponentRunner, runnerDrainBudget, func(e pgidEntry) func() bool {
			// Socketless: confirmed only by its group going ESRCH (identity check
			// false = gone or recycled-away).
			return func() bool { return !deps.GroupSignaller.Alive(e.Pgid, e.StartTime) }
		}},
		{ComponentServer, serverDrainBudget, func(pgidEntry) func() bool {
			// Socket quiescence: the UDS stops answering GetServerInfo.
			return func() bool { _, err := deps.Prober.Probe(ctx, cfg.SocketPath); return err != nil }
		}},
		{ComponentNats, natsDrainBudget, func(e pgidEntry) func() bool {
			// Container existence: nats is a container child torn down by name,
			// confirmed gone when `podman container exists` reports absent.
			// Reverse start order places it after the server and runner (its
			// future consumers, PR3/PR4) so no live consumer outlives the broker
			// it publishes to.
			return func() bool { return !deps.Containers.Exists(e.ContainerName) }
		}},
		{ComponentCollector, collectorDrainBudget, func(e pgidEntry) func() bool {
			// Container existence: the collector is a container child (like the
			// container-backed postgres), torn down by name; it is confirmed gone
			// when `podman container exists` reports absent. Reverse start order
			// places it after the server (which emits to it in T4b) and before
			// postgres.
			return func() bool { return !deps.Containers.Exists(e.ContainerName) }
		}},
		{ComponentPostgres, postgresDrainBudget, func(pgidEntry) func() bool {
			// Socket quiescence: postgres stops accepting on the DSN socket.
			return func() bool { return deps.DBProber.ProbeDB(ctx, cfg.DatabaseDSN) != nil }
		}},
	}

	var targets []target
	for _, o := range order {
		e, ok := byComp[o.comp]
		if !ok {
			continue // never recorded (half-spawned prefix) — nothing to tear down
		}
		if !entryAlive(deps, e) {
			continue // gone or recycled — skip, never signal
		}
		targets = append(targets, target{entry: e, budget: o.budget, confirm: o.confirm(e)})
	}
	return targets
}

// drainTargets SIGTERMs every live target (reverse order, already ordered by
// liveTargets), then per target waits the drain budget, escalates to a group
// SIGKILL, and confirms per component. It returns the components that were still
// alive at budget expiry after the SIGKILL — the survivor set for the
// partial-failure rewrite.
func drainTargets(ctx context.Context, deps Deps, targets []target) []Component {
	// Phase A: SIGTERM all live groups up front. Signaling the server also makes
	// a surviving runner exit when its link drops (run.go:115-119), belt-and-
	// suspenders alongside directly signaling the runner group. A delivery error
	// is not the verdict — the per-component confirm below is — so it is
	// intentionally not fatal here (an ESRCH means the group vanished in the
	// irreducible verify→signal gap, which the confirm reads as dead).
	for _, t := range targets {
		signalTerm(deps, t.entry, t.budget)
	}

	// Phase B: per-target confirm with bounded SIGKILL escalation.
	var survivors []Component
	for _, t := range targets {
		if drainOne(ctx, deps, t) {
			continue
		}
		survivors = append(survivors, t.entry.Component)
	}
	return survivors
}

// drainOne waits for one target to confirm dead within its drain budget; on
// timeout it escalates to a group SIGKILL and re-confirms. It returns true when
// the component is torn down.
//
// The runner is group-ESRCH confirmed and has no socket: because SIGKILL is
// unblockable, a runner group still non-ESRCH after the group SIGKILL can only
// be zombies awaiting init's reap (a guaranteed-terminal state), so the runner
// is treated as torn down once SIGKILL is sent — the post-SIGKILL zombie window
// is success, not failure. The socket-confirmed components (server, postgres) do
// get a bounded post-SIGKILL confirm: their socket going dark is the proof, and
// a socket still answering past postKillGrace is a genuine survivor.
func drainOne(ctx context.Context, deps Deps, t target) bool {
	if waitDead(ctx, deps.now, t.budget, t.confirm) {
		return true // SIGTERM sufficed (or the group was already gone)
	}

	// Escalate: hard-kill. For a process group this is a group SIGKILL; for a
	// container it is `podman rm -f`. A delivery error is not the verdict (an
	// ESRCH / already-gone means it died during the drain); the confirm decides.
	signalKill(deps, t.entry)

	if t.entry.Component == ComponentRunner {
		// Socketless + SIGKILL unblockable → any residual non-ESRCH group is a
		// zombie awaiting reap, i.e. terminal. Treat as torn down.
		return true
	}
	return waitDead(ctx, deps.now, postKillGrace, t.confirm)
}

// waitDead polls dead() until it reports true or the budget (measured on the
// now clock) elapses. It checks once before waiting so an already-dead group
// returns immediately, and does a final check on ctx cancellation.
func waitDead(ctx context.Context, now func() time.Time, budget time.Duration, dead func() bool) bool {
	deadline := now().Add(budget)
	ticker := time.NewTicker(downPollInterval)
	defer ticker.Stop()
	for {
		if dead() {
			return true
		}
		if !now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return dead()
		case <-ticker.C:
		}
	}
}

// survivorRecord builds the partial-failure record: the recorded entries whose
// components are still alive, preserving the header for provenance/version.
func survivorRecord(rec pgidRecord, survivors []Component) pgidRecord {
	live := map[Component]bool{}
	for _, c := range survivors {
		live[c] = true
	}
	out := pgidRecord{WriterPid: rec.WriterPid, Version: rec.Version}
	for _, e := range rec.Entries {
		if live[e.Component] {
			out.Entries = append(out.Entries, e)
		}
	}
	return out
}

// survivorError names the components that survived the bounded teardown, so a
// caller (and its logs) knows exactly which groups a retry must finish.
func survivorError(survivors []Component) error {
	names := make([]string, len(survivors))
	for i, c := range survivors {
		names[i] = c.String()
	}
	return fmt.Errorf("teardown incomplete: %v still live after the drain budget; retry down to finish", names)
}

// clearLockFile removes the state-dir lockfile idempotently, reusing stackLock's
// idempotent release. In the cross-process path the up that wrote it is long
// dead (linger), so the file is stale; leaving it is harmless but a clean state
// dir after a full teardown is tidier.
func clearLockFile(stateDir string) error {
	l := &stackLock{path: filepath.Join(stateDir, lockFileName)}
	return l.release()
}

// logSignalMiss records a non-fatal signal-delivery error at debug level.
// Delivery is never the teardown verdict — the per-component confirm channel is
// — so a miss (most often ESRCH: the group vanished in the irreducible
// verify→signal gap) is an expected, benign event, logged at debug for a
// deep-dive trace rather than surfaced to the operator or returned. Kept as one
// helper so both the SIGTERM and SIGKILL sites read identically.
func logSignalMiss(sig string, e pgidEntry, err error) {
	slog.Debug("group signal not delivered (group likely already gone)",
		"signal", sig, "component", e.Component.String(), "pgid", e.Pgid, "error", err)
}

// entryAlive reports whether a recorded entry's target is still live, dispatched
// on kind: a process group by identity-checked pgid (existence AND leader
// start-time), a container by name via `podman container exists`. A gone target
// reports not-alive so it is skipped, never signaled.
func entryAlive(deps Deps, e pgidEntry) bool {
	switch e.Kind {
	case entryContainer:
		return deps.Containers.Exists(e.ContainerName)
	default:
		return deps.GroupSignaller.Alive(e.Pgid, e.StartTime)
	}
}

// signalTerm delivers the graceful-stop tier, dispatched on kind: a group
// SIGTERM for a process, `podman stop -t <budget>` for a container (the budget
// is the container's own drain budget, deliberate parity with the process
// model's capped drain). A delivery error is not the teardown verdict — the
// per-component confirm channel is — so it is logged, never fatal.
func signalTerm(deps Deps, e pgidEntry, budget time.Duration) {
	switch e.Kind {
	case entryContainer:
		if err := deps.Containers.Stop(e.ContainerName, budget); err != nil {
			logContainerSignalMiss("stop", e, err)
		}
	default:
		if err := deps.GroupSignaller.Signal(e.Pgid, SignalTerm); err != nil {
			logSignalMiss("SIGTERM", e, err)
		}
	}
}

// signalKill delivers the hard-kill tier, dispatched on kind: a group SIGKILL
// for a process, `podman rm -f` for a container. A delivery error is not the
// teardown verdict — the per-component confirm decides — so it is logged.
func signalKill(deps Deps, e pgidEntry) {
	switch e.Kind {
	case entryContainer:
		if err := deps.Containers.Remove(e.ContainerName); err != nil {
			logContainerSignalMiss("rm -f", e, err)
		}
	default:
		if err := deps.GroupSignaller.Signal(e.Pgid, SignalKill); err != nil {
			logSignalMiss("SIGKILL", e, err)
		}
	}
}

// logContainerSignalMiss is the container analogue of logSignalMiss: a podman
// teardown-delivery error (most often "no such container": it went away in the
// verify→signal gap) is an expected, benign event, logged at debug rather than
// surfaced, since the socket-quiescence confirm is the verdict.
func logContainerSignalMiss(op string, e pgidEntry, err error) {
	slog.Debug("container teardown not delivered (container likely already gone)",
		"op", op, "component", e.Component.String(), "container", e.ContainerName, "error", err)
}
