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
// SIGKILL. Reverse start order (runner → server → gateway → nats → collector →
// postgres); the server's graceful drain is the long pole. On the SIGTERM-succeeds
// path they sum to 110s; on the escalation path the five non-runner components each
// add a postKillGrace, so the true worst case is 15 + (30+5) + (25+5) + (20+5) + (10+5) + (10+5) = 135s.
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
	// natsDrainBudget bounds the nats container's graceful `podman stop` before the
	// `podman rm -f` escalation. NATS flushes its JetStream store on SIGTERM, so it
	// gets the wider budget its natsStopTimeout also reserves — a `rm -f` mid-flush
	// is the unclean-shutdown case the store recovers from on next boot.
	natsDrainBudget = 20 * time.Second
	// gatewayDrainBudget matches gatewayStopTimeout: in-flight model calls drain on SIGTERM.
	gatewayDrainBudget = 25 * time.Second
	// postKillGrace bounds the confirm after the hard kill for the non-runner
	// components: the kill is unblockable (group SIGKILL, or `podman rm -f`), so a
	// component still confirming alive past this grace is a genuine survivor, not a
	// zombie. The runner (group-ESRCH confirmed) needs no grace.
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
// confirms process-backed server/postgres entries by socket quiescence AND group
// exit, containers by their own liveness probe, and the runner by group-ESRCH.
// Only recorded pgids are signaled; each identity is re-verified before signal.
func DownDetached(ctx context.Context, cfg Config, deps Deps) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	// 1. Refuse to race a live up. The guard flock does NOT cover this (up releases
	// it before spawnChain), so the lockfile-holder check is the real interlock: a
	// live holder means an up is in flight (possibly parked in waitReady), and
	// tearing down a half-spawned set is wrong.
	lockPath := filepath.Join(cfg.StateDir, lockFileName)
	if live, err := lockHolderLive(lockPath); err != nil {
		return fmt.Errorf("inspect stack lock: %w", err)
	} else if live {
		return ErrStackStarting
	}

	// 2. Read the record under the guard flock and CONSUME it so two concurrent
	// downs cannot both consume it and double-signal — the loser re-reads, finds no
	// file, no-ops. The guard is held ONLY for this read+consume and released before
	// the long signal→wait→SIGKILL sequence, so a down never blocks a concurrent up.
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
	// is skipped — never signaled, never an error. A prior-boot record had its
	// process entries dropped in consumeRecord.
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
	rec, err = dropPriorBootGroups(ctx, cfg, deps, rec)
	if err != nil {
		return pgidRecord{}, false, err
	}

	// Consume: remove the record under the guard so a concurrent down cannot also
	// act on it. A partial teardown re-publishes the survivor set at the end.
	if rerr := removePgidFile(cfg.StateDir); rerr != nil {
		return pgidRecord{}, false, fmt.Errorf("consume pgid record: %w", rerr)
	}
	return rec, true, nil
}

// dropPriorBootGroups removes every process entry when rec was written in an
// earlier boot: a reboot frees every pgid, so a match now would be a stranger.
// An unknown boot on either side (older record, unreadable id) keeps rec as is.
// Container entries stay, since a name is not recycled by a reboot.
// A live server socket contradicts the mismatch, so down refuses and keeps rec.
func dropPriorBootGroups(ctx context.Context, cfg Config, deps Deps, rec pgidRecord) (pgidRecord, error) {
	if rec.BootID == "" {
		return rec, nil
	}
	current, err := readBootID()
	if err != nil {
		// An unreadable current boot is unknown; per-entry identity checks still guard.
		slog.Warn("cannot read the current boot id; keeping per-entry identity checks", "err", err)
		return rec, nil
	}
	if current == "" || current == rec.BootID {
		return rec, nil
	}
	if _, perr := deps.Prober.Probe(ctx, cfg.SocketPath); perr == nil {
		return pgidRecord{}, fmt.Errorf("pgid record claims boot %s but this is boot %s and the stack socket still answers; refusing to tear down", rec.BootID, current)
	}
	slog.Info("pgid record is from an earlier boot; its process groups are gone", "record_boot", rec.BootID, "current_boot", current)
	out := pgidRecord{WriterPid: rec.WriterPid, Version: rec.Version, BootID: rec.BootID}
	for _, e := range rec.Entries {
		if e.Kind == entryContainer {
			out.Entries = append(out.Entries, e)
		}
	}
	return out, nil
}

// target is one live child to tear down: its recorded identity, confirmation
// channels, and drain budget. socketDark separates socket state from group liveness.
type target struct {
	entry      pgidEntry
	budget     time.Duration
	confirm    func() bool // reports the component confirmed dead by its own channel
	socketDark func() bool
}

// liveTargets returns the recorded children still ours to tear down, in reverse
// start order (runner → server → gateway → nats → collector → postgres). A gone
// or recycled group, or an absent container, is omitted — never signaled.
func liveTargets(ctx context.Context, cfg Config, deps Deps, rec pgidRecord) []target {
	// Index entries by component so we can emit them in reverse start order
	// regardless of the file's line order (which is start order by construction).
	byComp := map[Component]pgidEntry{}
	for _, e := range rec.Entries {
		byComp[e.Component] = e
	}

	order := []struct {
		comp       Component
		budget     time.Duration
		confirm    func(pgidEntry) func() bool
		socketDark func(pgidEntry) func() bool
	}{
		{comp: ComponentRunner, budget: runnerDrainBudget, confirm: func(e pgidEntry) func() bool {
			if e.Kind == entryContainer {
				return func() bool { return !deps.Containers.Exists(e.ContainerName) }
			}
			// Socketless process groups are confirmed only by the group leaving.
			return func() bool { return groupReleased(deps, e) }
		}},
		{comp: ComponentServer, budget: serverDrainBudget, confirm: func(e pgidEntry) func() bool {
			if e.Kind == entryContainer {
				return func() bool { return !deps.Containers.Exists(e.ContainerName) }
			}
			// A dark UDS can precede process exit during graceful shutdown.
			return func() bool {
				if _, err := deps.Prober.Probe(ctx, cfg.SocketPath); err == nil {
					return false
				}
				return groupReleased(deps, e)
			}
		}, socketDark: func(e pgidEntry) func() bool {
			return func() bool {
				_, err := deps.Prober.Probe(ctx, cfg.SocketPath)
				return err != nil
			}
		}},
		{comp: ComponentGateway, budget: gatewayDrainBudget, confirm: func(e pgidEntry) func() bool {
			// Container existence; signalTerm also removes it, since it runs without --rm.
			return func() bool { return !deps.Containers.Exists(e.ContainerName) }
		}},
		{comp: ComponentNats, budget: natsDrainBudget, confirm: func(e pgidEntry) func() bool {
			// Container existence: nats is a container child torn down by name,
			// confirmed gone when `podman container exists` reports absent. Reverse
			// start order places it after the server and runner (its consumers) so no
			// live consumer outlives the broker it publishes to.
			return func() bool { return !deps.Containers.Exists(e.ContainerName) }
		}},
		{comp: ComponentCollector, budget: collectorDrainBudget, confirm: func(e pgidEntry) func() bool {
			// Container existence: the collector is a container child torn down by
			// name, confirmed gone when `podman container exists` reports absent.
			// Reverse start order places it after the server (which emits to it) and
			// before postgres.
			return func() bool { return !deps.Containers.Exists(e.ContainerName) }
		}},
		{comp: ComponentPostgres, budget: postgresDrainBudget, confirm: func(e pgidEntry) func() bool {
			if e.Kind == entryContainer {
				// The bind-mounted socket can go dark while the container lingers.
				return func() bool { return !deps.Containers.Exists(e.ContainerName) }
			}
			// A dark DSN can precede process exit during postgres shutdown.
			return func() bool {
				if deps.DBProber.ProbeDB(ctx, cfg.DatabaseDSN) == nil {
					return false
				}
				return groupReleased(deps, e)
			}
		}, socketDark: func(e pgidEntry) func() bool {
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
		target := target{entry: e, budget: o.budget, confirm: o.confirm(e)}
		if o.socketDark != nil {
			target.socketDark = o.socketDark(e)
		}
		targets = append(targets, target)
	}
	return targets
}

// drainTargets SIGTERMs every live target (reverse order, already ordered by
// liveTargets), then per target waits the drain budget, escalates to a group
// SIGKILL, and confirms per component. It returns the components that were still
// alive at budget expiry after the SIGKILL — the survivor set for the
// partial-failure rewrite.
func drainTargets(ctx context.Context, deps Deps, targets []target) []Component {
	// Phase A: SIGTERM all live groups up front. Signaling the server also makes a
	// surviving runner exit when its link drops, belt-and-suspenders alongside
	// signaling the runner group. A delivery error is not the verdict — the confirm
	// below is — so it is not fatal here (an ESRCH means the group already vanished).
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
// timeout it escalates to SIGKILL and re-confirms. It returns true when the
// component is torn down.
//
// Only a delivered group SIGKILL lets residual members count as zombies awaiting
// reap; otherwise the post-kill confirm decides.
func drainOne(ctx context.Context, deps Deps, t target) bool {
	if waitDead(ctx, deps.now, t.budget, t.confirm) {
		return true // SIGTERM sufficed (or the group was already gone)
	}

	killed := signalKill(deps, t.entry)
	if killed && ctx.Err() == nil {
		if t.entry.Component == ComponentRunner {
			return true
		}
		if t.socketDark != nil && t.socketDark() {
			return true
		}
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

// entryAlive reports whether a recorded entry is still ours to signal: a
// container that exists, or a process group that is owned or orphaned.
func entryAlive(deps Deps, e pgidEntry) bool {
	switch e.Kind {
	case entryContainer:
		return deps.Containers.Exists(e.ContainerName)
	default:
		return groupOurs(deps, e)
	}
}

// groupOurs reports whether a recorded group is still present and not a recycled
// pid. An orphaned group keeps our pgid, since Linux never reuses a live pgid.
func groupOurs(deps Deps, e pgidEntry) bool {
	if e.Pgid <= 0 {
		return false
	}
	l := deps.GroupSignaller.Liveness(e.Pgid, e.StartTime)
	return l == GroupOwned || l == GroupOrphaned
}

// groupReleased reports whether a recorded group no longer needs teardown: it
// is gone, or its pgid now names someone else's process.
func groupReleased(deps Deps, e pgidEntry) bool {
	return !groupOurs(deps, e)
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
		if e.Component == ComponentGateway {
			if err := deps.Containers.Remove(e.ContainerName); err != nil {
				logContainerSignalMiss("remove", e, err)
			}
		}
	default:
		// Re-check identity: the pgid may have been recycled since selection.
		if !groupOurs(deps, e) {
			return
		}
		if err := deps.GroupSignaller.Signal(e.Pgid, SignalTerm); err != nil {
			logSignalMiss("SIGTERM", e, err)
		}
	}
}

// signalKill delivers the hard-kill tier, dispatched on kind: a group SIGKILL
// for a process, `podman rm -f` for a container. It reports true only when a
// process-group SIGKILL was delivered, the precondition for the zombie shortcut.
func signalKill(deps Deps, e pgidEntry) bool {
	switch e.Kind {
	case entryContainer:
		if err := deps.Containers.Remove(e.ContainerName); err != nil {
			logContainerSignalMiss("rm -f", e, err)
		}
		return false
	default:
		// Re-check identity: the drain budget is a long window for pid reuse.
		if !groupOurs(deps, e) {
			return false
		}
		if err := deps.GroupSignaller.Signal(e.Pgid, SignalKill); err != nil {
			logSignalMiss("SIGKILL", e, err)
			return false
		}
		return true
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
