//go:build unix

package stack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The recorded pgids/tokens the harness supervisor persists (fakePid(c) and
// pid*10 from newHarness's readStartTime stub). A DownDetached test seeds the
// fake group signaller from these so identity matches the on-disk record.
const (
	pgPgid     = 1001
	serverPgid = 1002
	runnerPgid = 1003
)

func pgToken(pgid int) uint64 { return uint64(pgid) * 10 }

// downTestDeps returns deps with shrunk drain budgets. Existing tests retain
// group-backed probes; regression tests install independent socket fakes.
func downTestDeps(t *testing.T, h *harness) Deps {
	t.Helper()
	shrinkBudgets(t)
	deps := h.deps
	deps.Prober = &groupBackedProber{gs: h.groupSig, pgid: serverPgid, token: pgToken(serverPgid)}
	deps.DBProber = &groupBackedDBProber{gs: h.groupSig, pgid: pgPgid, token: pgToken(pgPgid)}
	return deps
}

// groupBackedProber answers GetServerInfo iff the server group is alive.
type groupBackedProber struct {
	gs    *fakeGroupSignaller
	pgid  int
	token uint64
}

func (p *groupBackedProber) Probe(context.Context, string) (ServerInfo, error) {
	if p.gs.ours(p.pgid, p.token) {
		return ServerInfo{Version: testVersion}, nil
	}
	return ServerInfo{}, errNotAnswering
}

type groupBackedDBProber struct {
	gs    *fakeGroupSignaller
	pgid  int
	token uint64
}

func (p *groupBackedDBProber) ProbeDB(context.Context, string) error {
	if p.gs.ours(p.pgid, p.token) {
		return nil
	}
	return errPostgresNotReady
}

// shrinkBudgets shrinks the package drain budgets/poll for the duration of a
// test, restoring them after. Small but nonzero so the deadline math is real.
func shrinkBudgets(t *testing.T) {
	t.Helper()
	pr, ps, pp, pc, pn, pg, pk, pi := runnerDrainBudget, serverDrainBudget, postgresDrainBudget, collectorDrainBudget, natsDrainBudget, gatewayDrainBudget, postKillGrace, downPollInterval
	runnerDrainBudget = 20 * time.Millisecond
	serverDrainBudget = 20 * time.Millisecond
	postgresDrainBudget = 20 * time.Millisecond
	collectorDrainBudget = 20 * time.Millisecond
	natsDrainBudget = 20 * time.Millisecond
	gatewayDrainBudget = 20 * time.Millisecond
	postKillGrace = 20 * time.Millisecond
	downPollInterval = time.Millisecond
	t.Cleanup(func() {
		runnerDrainBudget, serverDrainBudget, postgresDrainBudget, collectorDrainBudget, natsDrainBudget, gatewayDrainBudget, postKillGrace, downPollInterval = pr, ps, pp, pc, pn, pg, pk, pi
	})
}

type fixedServerProber bool

func (p fixedServerProber) Probe(context.Context, string) (ServerInfo, error) {
	if p {
		return ServerInfo{Version: testVersion}, nil
	}
	return ServerInfo{}, errNotAnswering
}

type fixedDBProber bool

func (p fixedDBProber) ProbeDB(context.Context, string) error {
	if p {
		return nil
	}
	return errPostgresNotReady
}

// darkSocketDownDeps decouples socket state from process-group liveness.
func darkSocketDownDeps(t *testing.T, h *harness) Deps {
	t.Helper()
	deps := downTestDeps(t, h)
	deps.Prober = fixedServerProber(false)
	deps.DBProber = fixedDBProber(false)
	return deps
}

// stepClock advances a fixed step on every read, so drain budgets elapse on
// poll count rather than wall time.
type stepClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

func newStepClock(step time.Duration) *stepClock {
	return &stepClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), step: step}
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.t
	c.t = c.t.Add(c.step)
	return t
}

func (c *stepClock) peek() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// assertKillAfterBudget fails unless pgid's SIGKILL came a full budget after its SIGTERM.
func assertKillAfterBudget(t *testing.T, termAt, killAt time.Time, budget time.Duration) {
	t.Helper()
	if termAt.IsZero() || killAt.IsZero() {
		t.Fatalf("SIGTERM at %v, SIGKILL at %v; want both delivered", termAt, killAt)
	}
	if gap := killAt.Sub(termAt); gap < budget {
		t.Fatalf("SIGKILL %v after SIGTERM, want at least the %v drain budget", gap, budget)
	}
}

// seedFullRecord writes a complete three-child pgid record and marks all three
// groups live in the fake signaller, matching the identity tokens.
func seedFullRecord(t *testing.T, cfg Config, h *harness) {
	t.Helper()
	rec := pgidRecord{
		WriterPid: 4242,
		Version:   pgidFileVersion,
		Entries: []pgidEntry{
			{Component: ComponentPostgres, Pgid: pgPgid, StartTime: pgToken(pgPgid)},
			{Component: ComponentServer, Pgid: serverPgid, StartTime: pgToken(serverPgid)},
			{Component: ComponentRunner, Pgid: runnerPgid, StartTime: pgToken(runnerPgid)},
		},
	}
	if err := writePgidFile(cfg.StateDir, rec); err != nil {
		t.Fatalf("seed pgid file = %v", err)
	}
	h.groupSig.set(pgPgid, pgToken(pgPgid), true)
	h.groupSig.set(serverPgid, pgToken(serverPgid), true)
	h.groupSig.set(runnerPgid, pgToken(runnerPgid), true)
}

// signalEvents keeps only group-term/group-kill events in order.
func signalEvents(events []string) []string {
	var out []string
	for _, e := range events {
		if len(e) >= 6 && e[:6] == "group-" {
			out = append(out, e)
		}
	}
	return out
}

// TestDownDetachedReverseOrderSIGTERM proves the happy path: all three groups
// live, SIGTERM makes each go dead (their group-ESRCH / socket-dark confirm),
// signaled in reverse start order (runner → server → postgres), no SIGKILL, the
// pgid file removed and the lockfile clear.
func TestDownDetachedReverseOrderSIGTERM(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := downTestDeps(t, h)

	// SIGTERM tears each group down immediately (models a graceful exit).
	for _, pgid := range []int{pgPgid, serverPgid, runnerPgid} {
		h.groupSig.onTerm[pgid] = func() { h.groupSig.set(pgid, pgToken(pgid), false) }
	}

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached = %v, want nil", err)
	}

	got := signalEvents(h.rec.snapshot())
	want := []string{
		"group-term " + strconv.Itoa(runnerPgid),
		"group-term " + strconv.Itoa(serverPgid),
		"group-term " + strconv.Itoa(pgPgid),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SIGTERM order:\n got  %v\n want %v", got, want)
	}
	assertPgidFileGone(t, cfg.StateDir)
	assertLockFree(t, cfg.StateDir)
}

// TestDownDetachedEscalatesToSIGKILL proves the bounded escalation: a group that
// ignores SIGTERM survives its drain budget, gets a group SIGKILL, and (socket
// then dark) is confirmed. The escalation is bounded — exactly one SIGKILL per
// stubborn group — never a first-resort or repeated kill.
func TestDownDetachedEscalatesToSIGKILL(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := downTestDeps(t, h)

	// runner + postgres die on SIGTERM; the server ignores SIGTERM and only dies
	// on SIGKILL — exercising group-aware escalation.
	h.groupSig.onTerm[pgPgid] = func() { h.groupSig.set(pgPgid, pgToken(pgPgid), false) }
	h.groupSig.onTerm[runnerPgid] = func() { h.groupSig.set(runnerPgid, pgToken(runnerPgid), false) }
	h.groupSig.onKill[serverPgid] = func() { h.groupSig.set(serverPgid, pgToken(serverPgid), false) }

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached = %v, want nil", err)
	}

	events := signalEvents(h.rec.snapshot())
	if n := countEvent(events, "group-kill "+strconv.Itoa(serverPgid)); n != 1 {
		t.Fatalf("server SIGKILL count = %d, want exactly 1 (bounded escalation)", n)
	}
	// No group is ever SIGKILLed that died on SIGTERM.
	if countEvent(events, "group-kill "+strconv.Itoa(pgPgid)) != 0 ||
		countEvent(events, "group-kill "+strconv.Itoa(runnerPgid)) != 0 {
		t.Fatalf("SIGKILL sent to a group that drained on SIGTERM: %v", events)
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// TestDownDetachedServerSocketDarkWaitsForGroupExit proves a dark server socket
// does not confirm a live group: SIGKILL waits out the full drain budget.
func TestDownDetachedServerSocketDarkWaitsForGroupExit(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := darkSocketDownDeps(t, h)
	clock := newStepClock(time.Millisecond)
	deps.Now = clock.Now
	var termAt, killAt time.Time
	h.groupSig.onTerm[pgPgid] = func() { h.groupSig.set(pgPgid, pgToken(pgPgid), false) }
	h.groupSig.onTerm[runnerPgid] = func() { h.groupSig.set(runnerPgid, pgToken(runnerPgid), false) }
	h.groupSig.onTerm[serverPgid] = func() { termAt = clock.peek() }
	h.groupSig.onKill[serverPgid] = func() {
		killAt = clock.peek()
		h.groupSig.set(serverPgid, pgToken(serverPgid), false)
	}

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached = %v, want nil after the server group exits", err)
	}
	if h.groupSig.ours(serverPgid, pgToken(serverPgid)) {
		t.Fatal("server group is still alive after DownDetached returned nil")
	}
	events := signalEvents(h.rec.snapshot())
	if countEvent(events, "group-kill "+strconv.Itoa(serverPgid)) != 1 {
		t.Fatalf("server SIGKILL count = %d, want 1: %v", countEvent(events, "group-kill "+strconv.Itoa(serverPgid)), events)
	}
	assertKillAfterBudget(t, termAt, killAt, serverDrainBudget)
	assertPgidFileGone(t, cfg.StateDir)
}

// TestDownDetachedPostgresSocketDarkWaitsForGroupExit is the postgres analogue:
// a dark DSN does not confirm a live group before the drain budget elapses.
func TestDownDetachedPostgresSocketDarkWaitsForGroupExit(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := darkSocketDownDeps(t, h)
	clock := newStepClock(time.Millisecond)
	deps.Now = clock.Now
	var termAt, killAt time.Time
	h.groupSig.onTerm[serverPgid] = func() { h.groupSig.set(serverPgid, pgToken(serverPgid), false) }
	h.groupSig.onTerm[runnerPgid] = func() { h.groupSig.set(runnerPgid, pgToken(runnerPgid), false) }
	h.groupSig.onTerm[pgPgid] = func() { termAt = clock.peek() }
	h.groupSig.onKill[pgPgid] = func() {
		killAt = clock.peek()
		h.groupSig.set(pgPgid, pgToken(pgPgid), false)
	}

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached = %v, want nil after the postgres group exits", err)
	}
	if h.groupSig.ours(pgPgid, pgToken(pgPgid)) {
		t.Fatal("postgres group is still alive after DownDetached returned nil")
	}
	events := signalEvents(h.rec.snapshot())
	if countEvent(events, "group-kill "+strconv.Itoa(pgPgid)) != 1 {
		t.Fatalf("postgres SIGKILL count = %d, want 1: %v", countEvent(events, "group-kill "+strconv.Itoa(pgPgid)), events)
	}
	assertKillAfterBudget(t, termAt, killAt, postgresDrainBudget)
	assertPgidFileGone(t, cfg.StateDir)
}

// TestDownDetachedOrphanedGroupEscalates proves a group whose leader is gone but
// whose members remain is still ours: it is signalled, killed, and reported.
func TestDownDetachedOrphanedGroupEscalates(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := darkSocketDownDeps(t, h)
	h.groupSig.onTerm[pgPgid] = func() { h.groupSig.set(pgPgid, pgToken(pgPgid), false) }
	h.groupSig.onTerm[serverPgid] = func() { h.groupSig.set(serverPgid, pgToken(serverPgid), false) }
	// The runner leader exited; a forked member keeps the group alive through SIGKILL.
	h.groupSig.setLeaderUnknown(runnerPgid)
	h.groupSig.failSignal(runnerPgid, SignalKill, errors.New("operation not permitted"))

	err := DownDetached(context.Background(), cfg, deps)
	if err == nil || !strings.Contains(err.Error(), "runner") {
		t.Fatalf("DownDetached = %v, want a survivor error naming the orphaned runner group", err)
	}
	events := signalEvents(h.rec.snapshot())
	for _, want := range []string{"group-term " + strconv.Itoa(runnerPgid), "group-kill " + strconv.Itoa(runnerPgid)} {
		if countEvent(events, want) != 1 {
			t.Fatalf("orphaned group should get exactly one %q: %v", want, events)
		}
	}
	rec, readErr := readPgidFile(cfg.StateDir)
	if readErr != nil || len(rec.Entries) != 1 || rec.Entries[0].Component != ComponentRunner {
		t.Fatalf("survivor record = %+v, err = %v; want only the runner", rec, readErr)
	}
}

// TestDownDetachedOrphanedServerGroupIsAwaited proves an orphaned server group is
// awaited until it is gone, never confirmed while its members remain.
func TestDownDetachedOrphanedServerGroupIsAwaited(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := darkSocketDownDeps(t, h)
	h.groupSig.onTerm[pgPgid] = func() { h.groupSig.set(pgPgid, pgToken(pgPgid), false) }
	h.groupSig.onTerm[runnerPgid] = func() { h.groupSig.set(runnerPgid, pgToken(runnerPgid), false) }
	h.groupSig.setLeaderUnknown(serverPgid)
	h.groupSig.onKill[serverPgid] = func() { h.groupSig.set(serverPgid, pgToken(serverPgid), false) }

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached = %v, want nil once the orphaned group exits", err)
	}
	events := signalEvents(h.rec.snapshot())
	for _, want := range []string{"group-term " + strconv.Itoa(serverPgid), "group-kill " + strconv.Itoa(serverPgid)} {
		if countEvent(events, want) != 1 {
			t.Fatalf("orphaned server group should get exactly one %q: %v", want, events)
		}
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// TestDownDetachedRecycledBeforeSIGKILLIsNotSignalled proves identity is re-read
// right before SIGKILL: a pid recycled as the drain budget expires is never killed.
func TestDownDetachedRecycledBeforeSIGKILLIsNotSignalled(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := darkSocketDownDeps(t, h)
	clock := newStepClock(time.Millisecond)
	var termAt time.Time
	// The recycle lands on the clock read that expires the runner budget, after
	// its last confirm poll saw the group still owned.
	deps.Now = func() time.Time {
		now := clock.Now()
		if !termAt.IsZero() && !now.Before(termAt.Add(runnerDrainBudget)) {
			h.groupSig.set(runnerPgid, 777, true)
		}
		return now
	}
	h.groupSig.onTerm[pgPgid] = func() { h.groupSig.set(pgPgid, pgToken(pgPgid), false) }
	h.groupSig.onTerm[serverPgid] = func() { h.groupSig.set(serverPgid, pgToken(serverPgid), false) }
	h.groupSig.onTerm[runnerPgid] = func() { termAt = clock.peek() }

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached = %v, want nil for a recycled group", err)
	}
	if n := countEvent(signalEvents(h.rec.snapshot()), "group-kill "+strconv.Itoa(runnerPgid)); n != 0 {
		t.Fatalf("recycled runner pgid SIGKILLed %d times, want 0", n)
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// TestDownDetachedRecycledBeforeSIGTERMIsNotSignalled proves identity is re-read
// right before SIGTERM, after target selection.
func TestDownDetachedRecycledBeforeSIGTERMIsNotSignalled(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := darkSocketDownDeps(t, h)
	h.groupSig.onTerm[pgPgid] = func() { h.groupSig.set(pgPgid, pgToken(pgPgid), false) }
	// The runner SIGTERM lands just as the server pgid is recycled.
	h.groupSig.onTerm[runnerPgid] = func() {
		h.groupSig.set(runnerPgid, pgToken(runnerPgid), false)
		h.groupSig.set(serverPgid, 888, true)
	}

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached = %v, want nil for a recycled group", err)
	}
	events := signalEvents(h.rec.snapshot())
	for _, forbidden := range []string{"group-term " + strconv.Itoa(serverPgid), "group-kill " + strconv.Itoa(serverPgid)} {
		if countEvent(events, forbidden) != 0 {
			t.Fatalf("recycled server pgid was signalled (%q): %v", forbidden, events)
		}
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// TestDownDetachedFailedSIGKILLDoesNotShortCircuit proves an undelivered group
// SIGKILL never takes the zombie shortcut: the live group is a survivor.
func TestDownDetachedFailedSIGKILLDoesNotShortCircuit(t *testing.T) {
	for _, tc := range []struct {
		name string
		comp Component
		pgid int
	}{
		{"runner", ComponentRunner, runnerPgid},
		{"server", ComponentServer, serverPgid},
		{"postgres", ComponentPostgres, pgPgid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, h := newHarness(t)
			seedFullRecord(t, cfg, h)
			deps := darkSocketDownDeps(t, h)
			for _, pgid := range []int{pgPgid, serverPgid, runnerPgid} {
				if pgid != tc.pgid {
					h.groupSig.onTerm[pgid] = func() { h.groupSig.set(pgid, pgToken(pgid), false) }
				}
			}
			h.groupSig.failSignal(tc.pgid, SignalKill, errors.New("operation not permitted"))

			err := DownDetached(context.Background(), cfg, deps)
			if err == nil || !strings.Contains(err.Error(), tc.comp.String()) {
				t.Fatalf("DownDetached = %v, want a survivor error naming %s", err, tc.comp)
			}
			rec, readErr := readPgidFile(cfg.StateDir)
			if readErr != nil || len(rec.Entries) != 1 || rec.Entries[0].Component != tc.comp {
				t.Fatalf("survivor record = %+v, err = %v; want only %s", rec, readErr, tc.comp)
			}
		})
	}
}

// TestDownDetachedCanceledSIGKILLDoesNotShortCircuit proves a canceled teardown
// re-checks the group instead of assuming post-kill zombies.
func TestDownDetachedCanceledSIGKILLDoesNotShortCircuit(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := darkSocketDownDeps(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.groupSig.onTerm[pgPgid] = func() { h.groupSig.set(pgPgid, pgToken(pgPgid), false) }
	h.groupSig.onTerm[serverPgid] = func() { h.groupSig.set(serverPgid, pgToken(serverPgid), false) }
	// The kill is delivered but the caller gave up; the group is still present.
	h.groupSig.onKill[runnerPgid] = cancel

	err := DownDetached(ctx, cfg, deps)
	if err == nil || !strings.Contains(err.Error(), "runner") {
		t.Fatalf("DownDetached = %v, want a survivor error naming the runner", err)
	}
}

func TestDownDetachedGroupSurvivingSIGKILLIsReported(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := darkSocketDownDeps(t, h)
	deps.DBProber = fixedDBProber(true)
	h.groupSig.onTerm[serverPgid] = func() { h.groupSig.set(serverPgid, pgToken(serverPgid), false) }
	h.groupSig.onTerm[runnerPgid] = func() { h.groupSig.set(runnerPgid, pgToken(runnerPgid), false) }

	err := DownDetached(context.Background(), cfg, deps)
	if err == nil || !strings.Contains(err.Error(), "postgres") || !strings.Contains(err.Error(), "teardown incomplete") {
		t.Fatalf("DownDetached = %v, want survivor error naming postgres", err)
	}
	if !h.groupSig.ours(pgPgid, pgToken(pgPgid)) {
		t.Fatal("fake postgres group did not survive SIGKILL")
	}
	if got := signalEvents(h.rec.snapshot()); countEvent(got, "group-kill "+strconv.Itoa(pgPgid)) != 1 {
		t.Fatalf("postgres SIGKILL count = %d, want 1: %v", countEvent(got, "group-kill "+strconv.Itoa(pgPgid)), got)
	}
	rec, readErr := readPgidFile(cfg.StateDir)
	if readErr != nil || len(rec.Entries) != 1 || rec.Entries[0].Component != ComponentPostgres {
		t.Fatalf("survivor record = %+v, err = %v; want only postgres", rec, readErr)
	}
}

func TestDownDetachedSocketDarkZombieGroupIsSuccess(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := darkSocketDownDeps(t, h)
	// Leave the group visible after SIGKILL, modeling a zombie awaiting reap.
	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached = %v, want nil for a socket-dark zombie group", err)
	}
	events := signalEvents(h.rec.snapshot())
	if countEvent(events, "group-kill "+strconv.Itoa(serverPgid)) != 1 {
		t.Fatalf("server SIGKILL count = %d, want 1: %v", countEvent(events, "group-kill "+strconv.Itoa(serverPgid)), events)
	}
	if !h.groupSig.ours(serverPgid, pgToken(serverPgid)) {
		t.Fatal("fake server zombie group should remain visible after SIGKILL")
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// TestDownDetachedRunnerZombieWindowIsSuccess proves the socketless runner is
// confirmed torn down once SIGKILL is sent, even if its group is still non-ESRCH
// (a zombie awaiting reap). Because SIGKILL is unblockable, a still-present group
// after the group SIGKILL can only be zombies → success, not failure.
func TestDownDetachedRunnerZombieWindowIsSuccess(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := downTestDeps(t, h)

	// server and postgres drain on SIGTERM.
	h.groupSig.onTerm[pgPgid] = func() { h.groupSig.set(pgPgid, pgToken(pgPgid), false) }
	h.groupSig.onTerm[serverPgid] = func() { h.groupSig.set(serverPgid, pgToken(serverPgid), false) }

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached = %v, want nil (zombie window is success)", err)
	}
	events := signalEvents(h.rec.snapshot())
	if countEvent(events, "group-kill "+strconv.Itoa(runnerPgid)) != 1 {
		t.Fatalf("runner should be SIGKILLed exactly once before the zombie-window success: %v", events)
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// TestDownDetachedPartialFailureRewritesSurvivors proves the partial-failure
// policy: a socket-confirmed component still answering after SIGKILL (a genuine
// survivor, not a zombie) is reported, the pgid file is NOT removed, and it is
// rewritten to exactly the surviving set so a retry can finish.
func TestDownDetachedPartialFailureRewritesSurvivors(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := downTestDeps(t, h)

	// runner + postgres drain; the server survives even SIGKILL (its socket keeps
	// answering) — the real-survivor case.
	h.groupSig.onTerm[pgPgid] = func() { h.groupSig.set(pgPgid, pgToken(pgPgid), false) }
	h.groupSig.onTerm[runnerPgid] = func() { h.groupSig.set(runnerPgid, pgToken(runnerPgid), false) }
	// server: no flip → stays alive → socket keeps answering past postKillGrace.

	err := DownDetached(context.Background(), cfg, deps)
	if err == nil {
		t.Fatal("DownDetached = nil, want partial-failure error naming the survivor")
	}

	// The record survives, rewritten to exactly the server entry.
	rec, rerr := readPgidFile(cfg.StateDir)
	if rerr != nil {
		t.Fatalf("survivor record read = %v, want the rewritten survivor set", rerr)
	}
	if len(rec.Entries) != 1 || rec.Entries[0].Component != ComponentServer {
		t.Fatalf("survivor record = %+v, want exactly the compass-server entry", rec.Entries)
	}
}

// TestDownDetachedRefusesLiveUp proves step 1: a live lockfile holder (an up in
// flight) makes DownDetached refuse with ErrStackStarting rather than tear down
// a half-spawned set — and it signals nothing.
func TestDownDetachedRefusesLiveUp(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := downTestDeps(t, h)

	// A lockfile whose holder is THIS (live) process → lockHolderLive true.
	writeLockHeldBy(t, cfg.StateDir, os.Getpid())

	err := DownDetached(context.Background(), cfg, deps)
	if !errors.Is(err, ErrStackStarting) {
		t.Fatalf("DownDetached = %v, want ErrStackStarting", err)
	}
	if got := signalEvents(h.rec.snapshot()); len(got) != 0 {
		t.Fatalf("refusal must signal nothing, got %v", got)
	}
	// The record is untouched (not consumed) so the real down can run later.
	if _, rerr := readPgidFile(cfg.StateDir); rerr != nil {
		t.Fatalf("pgid record should be untouched on refusal: %v", rerr)
	}
}

// TestDownDetachedSkipsRecycledAndDeadGroups proves the identity gate: a recorded
// group that is ESRCH, and one whose leader start-time no longer matches (a
// recycled pid), are BOTH skipped — never signaled, never an error — while the
// one genuinely-live identity-matched group is torn down.
func TestDownDetachedSkipsRecycledAndDeadGroups(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := downTestDeps(t, h)

	// postgres group is gone (ESRCH: absent from alive).
	h.groupSig.set(pgPgid, pgToken(pgPgid), false)
	// server pgid is reused by an unrelated process: alive, but WRONG start-time
	// token → identity mismatch → recycled → must be skipped.
	h.groupSig.set(serverPgid, 999999, true)
	// runner is genuinely live and drains on SIGTERM.
	h.groupSig.onTerm[runnerPgid] = func() { h.groupSig.set(runnerPgid, pgToken(runnerPgid), false) }

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached = %v, want nil (dead+recycled skipped, live torn down)", err)
	}
	events := signalEvents(h.rec.snapshot())
	// Only the runner was signaled; neither the dead pg nor the recycled server.
	for _, forbidden := range []string{
		"group-term " + strconv.Itoa(pgPgid), "group-kill " + strconv.Itoa(pgPgid),
		"group-term " + strconv.Itoa(serverPgid), "group-kill " + strconv.Itoa(serverPgid),
	} {
		if countEvent(events, forbidden) != 0 {
			t.Fatalf("signaled a dead/recycled group (%q): %v", forbidden, events)
		}
	}
	if countEvent(events, "group-term "+strconv.Itoa(runnerPgid)) != 1 {
		t.Fatalf("live runner should be SIGTERMed once: %v", events)
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// TestDownDetachedHalfSpawnedPrefix proves a down against a half-spawned stack
// (the record holds a prefix — postgres only) drains exactly that prefix and no
// more, then removes the record.
func TestDownDetachedHalfSpawnedPrefix(t *testing.T) {
	cfg, h := newHarness(t)
	rec := pgidRecord{
		WriterPid: 1, Version: pgidFileVersion,
		Entries: []pgidEntry{{Component: ComponentPostgres, Pgid: pgPgid, StartTime: pgToken(pgPgid)}},
	}
	if err := writePgidFile(cfg.StateDir, rec); err != nil {
		t.Fatalf("seed prefix record = %v", err)
	}
	h.groupSig.set(pgPgid, pgToken(pgPgid), true)
	h.groupSig.onTerm[pgPgid] = func() { h.groupSig.set(pgPgid, pgToken(pgPgid), false) }
	deps := downTestDeps(t, h)

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached = %v, want nil", err)
	}
	events := signalEvents(h.rec.snapshot())
	want := []string{"group-term " + strconv.Itoa(pgPgid)}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("half-spawned drain:\n got  %v\n want %v (exactly the prefix)", events, want)
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// TestDownDetachedNilContainersAllProcessRecord pins the nil-safety contract
// documented in deps.go: a record with no container entries never dereferences
// deps.Containers. Production wiring leaves Deps.Containers nil, so an all-process
// record must tear down cleanly without a nil-pointer panic — the container path
// is gated on `case entryContainer` and never fires here.
func TestDownDetachedNilContainersAllProcessRecord(t *testing.T) {
	cfg, h := newHarness(t)
	deps := downTestDeps(t, h)
	// Mirror production wiring: no container controller is provided.
	deps.Containers = nil

	rec := pgidRecord{
		WriterPid: 1, Version: pgidFileVersion,
		Entries: []pgidEntry{
			{Component: ComponentPostgres, Pgid: pgPgid, StartTime: pgToken(pgPgid)},
			{Component: ComponentServer, Pgid: serverPgid, StartTime: pgToken(serverPgid)},
		},
	}
	if err := writePgidFile(cfg.StateDir, rec); err != nil {
		t.Fatalf("seed all-process record = %v", err)
	}
	h.groupSig.set(pgPgid, pgToken(pgPgid), true)
	h.groupSig.onTerm[pgPgid] = func() { h.groupSig.set(pgPgid, pgToken(pgPgid), false) }
	h.groupSig.set(serverPgid, pgToken(serverPgid), true)
	h.groupSig.onTerm[serverPgid] = func() { h.groupSig.set(serverPgid, pgToken(serverPgid), false) }

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached with nil Containers on an all-process record = %v, want nil (no deref, clean teardown)", err)
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// TestDownDetachedAbsentFileNoSocketIsNoStack proves the "no stack" branch:
// absent pgid file and no answering socket → nil, no error, no signals.
func TestDownDetachedAbsentFileNoSocketIsNoStack(t *testing.T) {
	cfg, h := newHarness(t)
	deps := downTestDeps(t, h) // no group alive → prober reports dark
	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached with no record and no live server = %v, want nil", err)
	}
	if got := signalEvents(h.rec.snapshot()); len(got) != 0 {
		t.Fatalf("no-stack path must signal nothing, got %v", got)
	}
}

// TestDownDetachedAbsentFileAnsweringSocketErrors proves Open Question 1: a live
// stack (server answering) with no teardown record fails legibly rather than
// guessing at pids.
func TestDownDetachedAbsentFileAnsweringSocketErrors(t *testing.T) {
	cfg, h := newHarness(t)
	deps := h.deps
	shrinkBudgets(t)
	// A server that answers unconditionally, but no pgid file on disk.
	deps.Prober = &stubProber{rec: h.rec, forceLive: true, version: testVersion}
	deps.DBProber = &stubDBProber{rec: h.rec}

	err := DownDetached(context.Background(), cfg, deps)
	if !errors.Is(err, ErrNoTeardownRecord) {
		t.Fatalf("DownDetached = %v, want ErrNoTeardownRecord", err)
	}
}

// TestDownDetachedConcurrentSerializedByGuard proves the load-bearing
// concurrency invariant: the guard serializes the read+consume decision, so of
// two concurrent downs over one full record EXACTLY ONE consumes the record and
// signals — each group is signaled once, never twice (no double-signal). The
// winner returns nil and the record ends gone.
//
// The loser's return value is deliberately NOT asserted: it acquires the guard
// after the winner has consumed the file but released the guard before
// signaling, so its absent-file socket probe races the winner's in-flight drain
// (a still-answering server → ErrNoTeardownRecord; an already-dark one → nil).
// Both outcomes are safe — the loser never double-signals — and which one occurs
// is a timing detail, so pinning it would be a flaky assertion. (Flagged to the
// driver: the loser can surface a misleading no-record error during a genuine
// concurrent down; the no-double-signal guarantee is what actually matters.)
func TestDownDetachedConcurrentSerializedByGuard(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := downTestDeps(t, h)
	for _, pgid := range []int{pgPgid, serverPgid, runnerPgid} {
		h.groupSig.onTerm[pgid] = func() { h.groupSig.set(pgid, pgToken(pgid), false) }
	}

	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- DownDetached(context.Background(), cfg, deps) }()
	}
	e1, e2 := <-errs, <-errs

	// At least one down succeeds (the winner). Any non-nil error may only be the
	// benign concurrent-loser no-record race — never a double-signal or a
	// teardown failure.
	if e1 != nil && e2 != nil {
		t.Fatalf("both concurrent downs failed: %v / %v", e1, e2)
	}
	for _, e := range []error{e1, e2} {
		if e != nil && !errors.Is(e, ErrNoTeardownRecord) {
			t.Fatalf("unexpected concurrent-down error %v; only a benign ErrNoTeardownRecord race is allowed", e)
		}
	}

	// The guard guarantee: exactly one down consumed the record and signaled each
	// group once — never twice.
	events := signalEvents(h.rec.snapshot())
	for _, pgid := range []int{runnerPgid, serverPgid, pgPgid} {
		if n := countEvent(events, "group-term "+strconv.Itoa(pgid)); n != 1 {
			t.Fatalf("group %d SIGTERMed %d times across concurrent downs; want exactly 1 (no double-signal): %v", pgid, n, events)
		}
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// The stable container name a v2 postgres entry carries in these tests.
const pgContainerName = "compass-postgres-test01"

// seedContainerRecord writes a v2 record whose postgres entry is a container
// (ctr) and whose server/runner entries are processes, and marks all three live
// (the container present, the two groups alive + identity-matched).
func seedContainerRecord(t *testing.T, cfg Config, h *harness) {
	t.Helper()
	rec := pgidRecord{
		WriterPid: 4242,
		Version:   pgidFileVersion,
		Entries: []pgidEntry{
			{Kind: entryContainer, Component: ComponentPostgres, ContainerName: pgContainerName},
			{Kind: entryProc, Component: ComponentServer, Pgid: serverPgid, StartTime: pgToken(serverPgid)},
			{Kind: entryProc, Component: ComponentRunner, Pgid: runnerPgid, StartTime: pgToken(runnerPgid)},
		},
	}
	if err := writePgidFile(cfg.StateDir, rec); err != nil {
		t.Fatalf("seed container record = %v", err)
	}
	h.containers.setExists(true)
	h.groupSig.set(serverPgid, pgToken(serverPgid), true)
	h.groupSig.set(runnerPgid, pgToken(runnerPgid), true)
}

// containerBackedDBProber answers the postgres-reachability probe iff the
// container is still present — the socket goes dark when the container stops
// (the socket dir is bind-mounted, so the confirm channel is unchanged; only the
// signal-delivery side differs from the process path).
type containerBackedDBProber struct {
	c    *fakeContainerController
	name string
}

func (p *containerBackedDBProber) ProbeDB(ctx context.Context, dsn string) error {
	if p.c.Exists(p.name) {
		return nil // container up → socket answers → reachable
	}
	return errPostgresNotReady // container gone → socket dark → confirmed dead
}

// containerDownDeps points the server confirm at the group signaller (as the
// process path) and the postgres confirm at the container's existence.
func containerDownDeps(t *testing.T, h *harness) Deps {
	t.Helper()
	shrinkBudgets(t)
	deps := h.deps
	deps.Prober = &groupBackedProber{gs: h.groupSig, pgid: serverPgid, token: pgToken(serverPgid)}
	deps.DBProber = &containerBackedDBProber{c: h.containers, name: pgContainerName}
	return deps
}

// ctrEvents keeps only container stop/rm events in order.
func ctrEvents(events []string) []string {
	var out []string
	for _, e := range events {
		if len(e) >= 4 && e[:4] == "ctr-" {
			out = append(out, e)
		}
	}
	return out
}

// TestDownDetachedContainerGracefulStop proves the container teardown path: a
// container postgres entry is torn down by `podman stop` (graceful), confirmed
// by socket quiescence, with no `rm -f` escalation — the container analogue of
// the reverse-order SIGTERM happy path.
func TestDownDetachedContainerGracefulStop(t *testing.T) {
	cfg, h := newHarness(t)
	seedContainerRecord(t, cfg, h)
	deps := containerDownDeps(t, h)

	// SIGTERM tears the two groups down; `podman stop` removes the container.
	h.groupSig.onTerm[serverPgid] = func() { h.groupSig.set(serverPgid, pgToken(serverPgid), false) }
	h.groupSig.onTerm[runnerPgid] = func() { h.groupSig.set(runnerPgid, pgToken(runnerPgid), false) }
	h.containers.onStop[pgContainerName] = func() { h.containers.setExists(false) }

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached = %v, want nil", err)
	}

	got := ctrEvents(h.rec.snapshot())
	want := []string{"ctr-stop " + pgContainerName}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("container teardown:\n got  %v\n want %v (graceful stop, no rm -f)", got, want)
	}
	// The two process groups were still signaled by group SIGTERM.
	sig := signalEvents(h.rec.snapshot())
	for _, pgid := range []int{serverPgid, runnerPgid} {
		if countEvent(sig, "group-term "+strconv.Itoa(pgid)) != 1 {
			t.Fatalf("process group %d should be SIGTERMed once: %v", pgid, sig)
		}
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// TestDownDetachedContainerEscalatesToRemove proves the container SIGKILL tier: a
// container that survives `podman stop` is force-removed by `podman rm -f`, then
// (socket dark) confirmed — the container analogue of the group-SIGKILL
// escalation.
func TestDownDetachedContainerEscalatesToRemove(t *testing.T) {
	cfg, h := newHarness(t)
	seedContainerRecord(t, cfg, h)
	deps := containerDownDeps(t, h)

	h.groupSig.onTerm[serverPgid] = func() { h.groupSig.set(serverPgid, pgToken(serverPgid), false) }
	h.groupSig.onTerm[runnerPgid] = func() { h.groupSig.set(runnerPgid, pgToken(runnerPgid), false) }
	// The container ignores stop, only dying on rm -f.
	h.containers.onRemove[pgContainerName] = func() { h.containers.setExists(false) }

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached = %v, want nil", err)
	}
	events := ctrEvents(h.rec.snapshot())
	want := []string{"ctr-stop " + pgContainerName, "ctr-rm " + pgContainerName}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("container escalation:\n got  %v\n want %v (stop then rm -f)", events, want)
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// TestDownDetachedContainerSurvivorRewritesRecord proves the partial-failure
// policy holds for a container: one still present after `podman rm -f` (its
// socket keeps answering) is a genuine survivor — reported, the record NOT
// removed, rewritten to exactly the surviving container entry so a retry can
// finish by name.
func TestDownDetachedContainerSurvivorRewritesRecord(t *testing.T) {
	cfg, h := newHarness(t)
	seedContainerRecord(t, cfg, h)
	deps := containerDownDeps(t, h)

	h.groupSig.onTerm[serverPgid] = func() { h.groupSig.set(serverPgid, pgToken(serverPgid), false) }
	h.groupSig.onTerm[runnerPgid] = func() { h.groupSig.set(runnerPgid, pgToken(runnerPgid), false) }
	// The container survives even rm -f (existence never flips) → real survivor.

	err := DownDetached(context.Background(), cfg, deps)
	if err == nil {
		t.Fatal("DownDetached = nil, want a partial-failure error naming the surviving container")
	}
	rec, rerr := readPgidFile(cfg.StateDir)
	if rerr != nil {
		t.Fatalf("survivor record read = %v, want the rewritten survivor set", rerr)
	}
	if len(rec.Entries) != 1 || rec.Entries[0].Kind != entryContainer || rec.Entries[0].ContainerName != pgContainerName {
		t.Fatalf("survivor record = %+v, want exactly the postgres container entry", rec.Entries)
	}
}

// TestDownDetachedContainerGoneIsSkipped proves the identity gate for containers:
// a recorded container that no longer exists is skipped — never stopped, never
// removed, never an error — while the live process entries are still torn down.
func TestDownDetachedContainerGoneIsSkipped(t *testing.T) {
	cfg, h := newHarness(t)
	seedContainerRecord(t, cfg, h)
	deps := containerDownDeps(t, h)

	// The container is already gone.
	h.containers.setExists(false)
	h.groupSig.onTerm[serverPgid] = func() { h.groupSig.set(serverPgid, pgToken(serverPgid), false) }
	h.groupSig.onTerm[runnerPgid] = func() { h.groupSig.set(runnerPgid, pgToken(runnerPgid), false) }

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached = %v, want nil (gone container skipped)", err)
	}
	if events := ctrEvents(h.rec.snapshot()); len(events) != 0 {
		t.Fatalf("a gone container must not be signaled: %v", events)
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// TestDownDetachedContainerDarkDBStillExistsIsSurvivor proves a container
// postgres is confirmed only by its absence: a dark DB never confirms it.
func TestDownDetachedContainerDarkDBStillExistsIsSurvivor(t *testing.T) {
	cfg, h := newHarness(t)
	seedContainerRecord(t, cfg, h)
	deps := containerDownDeps(t, h)
	deps.DBProber = fixedDBProber(false)
	h.groupSig.onTerm[serverPgid] = func() { h.groupSig.set(serverPgid, pgToken(serverPgid), false) }
	h.groupSig.onTerm[runnerPgid] = func() { h.groupSig.set(runnerPgid, pgToken(runnerPgid), false) }

	err := DownDetached(context.Background(), cfg, deps)
	if err == nil || !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("DownDetached = %v, want a survivor error naming the present postgres container", err)
	}
	want := []string{"ctr-stop " + pgContainerName, "ctr-rm " + pgContainerName}
	if got := ctrEvents(h.rec.snapshot()); !reflect.DeepEqual(got, want) {
		t.Fatalf("container escalation:\n got  %v\n want %v", got, want)
	}
	rec, readErr := readPgidFile(cfg.StateDir)
	if readErr != nil || len(rec.Entries) != 1 || rec.Entries[0].ContainerName != pgContainerName {
		t.Fatalf("survivor record = %+v, err = %v; want only the postgres container", rec, readErr)
	}
}

// The stable name a v2 collector container entry carries in these tests.
const collectorContainerNameTest = "compass-otel-collector-test01"

// seedCollectorRecord writes a v2 record whose collector entry is a container
// (ctr) sitting between the server and postgres process entries — the exact
// start-order shape a real up records — and marks all four children live. This
// is the hermetic guard for the cross-process collector teardown: the invariant
// that broke once (liveTargets not knowing ComponentCollector, so a detached
// down silently skipped the container and leaked it) is invisible on the
// podman-gated integration test but caught here on every default-CI run.
func seedCollectorRecord(t *testing.T, cfg Config, h *harness) {
	t.Helper()
	rec := pgidRecord{
		WriterPid: 4242,
		Version:   pgidFileVersion,
		Entries: []pgidEntry{
			{Kind: entryProc, Component: ComponentPostgres, Pgid: pgPgid, StartTime: pgToken(pgPgid)},
			{Kind: entryProc, Component: ComponentServer, Pgid: serverPgid, StartTime: pgToken(serverPgid)},
			{Kind: entryContainer, Component: ComponentCollector, ContainerName: collectorContainerNameTest},
			{Kind: entryProc, Component: ComponentRunner, Pgid: runnerPgid, StartTime: pgToken(runnerPgid)},
		},
	}
	if err := writePgidFile(cfg.StateDir, rec); err != nil {
		t.Fatalf("seed collector record = %v", err)
	}
	h.containers.setExistsName(collectorContainerNameTest, true)
	h.groupSig.set(pgPgid, pgToken(pgPgid), true)
	h.groupSig.set(serverPgid, pgToken(serverPgid), true)
	h.groupSig.set(runnerPgid, pgToken(runnerPgid), true)
}

// sidecarContainerDownDeps points a container component's confirm at container
// existence while the server/postgres confirms track their group liveness (they
// are processes in these records). Shared by the collector and nats teardown
// tests — the Containers seam is name-agnostic, so one wiring serves both.
func sidecarContainerDownDeps(t *testing.T, h *harness) Deps {
	t.Helper()
	deps := downTestDeps(t, h)
	deps.Containers = h.containers
	return deps
}

// indexOf returns the position of want in events, or -1.
func indexOf(events []string, want string) int {
	for i, e := range events {
		if e == want {
			return i
		}
	}
	return -1
}

// TestDownDetachedCollectorContainerTornDownByName proves the cross-process
// teardown of the bundled collector: a detached down reads the v2 record, stops
// the collector container BY NAME (graceful, no rm -f) and in reverse start
// order — after the server is signaled, before postgres. A regression dropping
// ComponentCollector from liveTargets' order slice fails here (the container is
// never stopped), on every default-CI run.
func TestDownDetachedCollectorContainerTornDownByName(t *testing.T) {
	cfg, h := newHarness(t)
	seedCollectorRecord(t, cfg, h)
	deps := sidecarContainerDownDeps(t, h)

	for _, pgid := range []int{pgPgid, serverPgid, runnerPgid} {
		h.groupSig.onTerm[pgid] = func() { h.groupSig.set(pgid, pgToken(pgid), false) }
	}
	h.containers.onStop[collectorContainerNameTest] = func() {
		h.containers.setExistsName(collectorContainerNameTest, false)
	}

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached = %v, want nil", err)
	}

	// The collector was stopped by name, gracefully (no rm -f escalation).
	if got := ctrEvents(h.rec.snapshot()); !reflect.DeepEqual(got, []string{"ctr-stop " + collectorContainerNameTest}) {
		t.Fatalf("collector teardown:\n got  %v\n want [ctr-stop %s] (graceful, by name)", got, collectorContainerNameTest)
	}
	// Reverse start order: the collector stop lands after the server SIGTERM and
	// before the postgres SIGTERM (runner → server → collector → postgres).
	ev := h.rec.snapshot()
	iServer := indexOf(ev, "group-term "+strconv.Itoa(serverPgid))
	iColl := indexOf(ev, "ctr-stop "+collectorContainerNameTest)
	iPg := indexOf(ev, "group-term "+strconv.Itoa(pgPgid))
	if !(iServer >= 0 && iColl >= 0 && iPg >= 0 && iServer < iColl && iColl < iPg) {
		t.Fatalf("teardown order wrong: server@%d collector@%d postgres@%d; want server<collector<postgres in %v", iServer, iColl, iPg, ev)
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// TestDownDetachedCollectorEscalatesToRemove proves the collector SIGKILL tier:
// a collector container that ignores `podman stop` is force-removed by
// `podman rm -f`, then (existence gone) confirmed — the container escalation for
// the collector entry specifically.
func TestDownDetachedCollectorEscalatesToRemove(t *testing.T) {
	cfg, h := newHarness(t)
	seedCollectorRecord(t, cfg, h)
	deps := sidecarContainerDownDeps(t, h)

	for _, pgid := range []int{pgPgid, serverPgid, runnerPgid} {
		h.groupSig.onTerm[pgid] = func() { h.groupSig.set(pgid, pgToken(pgid), false) }
	}
	// The collector ignores stop, only dying on rm -f.
	h.containers.onRemove[collectorContainerNameTest] = func() {
		h.containers.setExistsName(collectorContainerNameTest, false)
	}

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached = %v, want nil", err)
	}
	got := ctrEvents(h.rec.snapshot())
	want := []string{"ctr-stop " + collectorContainerNameTest, "ctr-rm " + collectorContainerNameTest}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("collector escalation:\n got  %v\n want %v (stop then rm -f)", got, want)
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// The stable name a v2 nats container entry carries in these tests.
const natsContainerNameTest = "compass-nats-test01"

// seedNatsRecord writes a v2 record whose nats entry is a container (ctr)
// sitting between the server and postgres process entries — the start-order
// shape a real up records — and marks all four children live. It is the
// hermetic guard for the cross-process nats teardown: a liveTargets that does
// not know ComponentNats silently skips the container and leaks it, which no
// podman-gated integration test would catch on a default-CI run.
func seedNatsRecord(t *testing.T, cfg Config, h *harness) {
	t.Helper()
	rec := pgidRecord{
		WriterPid: 4242,
		Version:   pgidFileVersion,
		Entries: []pgidEntry{
			{Kind: entryProc, Component: ComponentPostgres, Pgid: pgPgid, StartTime: pgToken(pgPgid)},
			{Kind: entryContainer, Component: ComponentNats, ContainerName: natsContainerNameTest},
			{Kind: entryProc, Component: ComponentServer, Pgid: serverPgid, StartTime: pgToken(serverPgid)},
			{Kind: entryProc, Component: ComponentRunner, Pgid: runnerPgid, StartTime: pgToken(runnerPgid)},
		},
	}
	if err := writePgidFile(cfg.StateDir, rec); err != nil {
		t.Fatalf("seed nats record = %v", err)
	}
	h.containers.setExistsName(natsContainerNameTest, true)
	h.groupSig.set(pgPgid, pgToken(pgPgid), true)
	h.groupSig.set(serverPgid, pgToken(serverPgid), true)
	h.groupSig.set(runnerPgid, pgToken(runnerPgid), true)
}

// TestDownDetachedNatsContainerTornDownByName proves the cross-process teardown
// of the bundled nats: a detached down reads the v2 record, stops the nats
// container BY NAME (graceful, no rm -f) and in reverse start order — after the
// server is signaled (its future consumer, PR3/PR4), before postgres. Graceful
// matters more here than for the collector: `podman stop` SIGTERMs nats-server
// so it flushes the JetStream store, while an rm -f would leave the store to
// recover on next boot.
func TestDownDetachedNatsContainerTornDownByName(t *testing.T) {
	cfg, h := newHarness(t)
	seedNatsRecord(t, cfg, h)
	deps := sidecarContainerDownDeps(t, h)

	for _, pgid := range []int{pgPgid, serverPgid, runnerPgid} {
		h.groupSig.onTerm[pgid] = func() { h.groupSig.set(pgid, pgToken(pgid), false) }
	}
	h.containers.onStop[natsContainerNameTest] = func() {
		h.containers.setExistsName(natsContainerNameTest, false)
	}

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached = %v, want nil", err)
	}

	if got := ctrEvents(h.rec.snapshot()); !reflect.DeepEqual(got, []string{"ctr-stop " + natsContainerNameTest}) {
		t.Fatalf("nats teardown:\n got  %v\n want [ctr-stop %s] (graceful, by name)", got, natsContainerNameTest)
	}
	ev := h.rec.snapshot()
	iServer := indexOf(ev, "group-term "+strconv.Itoa(serverPgid))
	iNats := indexOf(ev, "ctr-stop "+natsContainerNameTest)
	iPg := indexOf(ev, "group-term "+strconv.Itoa(pgPgid))
	if !(iServer >= 0 && iNats >= 0 && iPg >= 0 && iServer < iNats && iNats < iPg) {
		t.Fatalf("teardown order wrong: server@%d nats@%d postgres@%d; want server<nats<postgres in %v", iServer, iNats, iPg, ev)
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// TestDownDetachedNatsEscalatesToRemove proves the nats SIGKILL tier: a nats
// container that ignores `podman stop` is force-removed by `podman rm -f`, then
// (existence gone) confirmed.
func TestDownDetachedNatsEscalatesToRemove(t *testing.T) {
	cfg, h := newHarness(t)
	seedNatsRecord(t, cfg, h)
	deps := sidecarContainerDownDeps(t, h)

	for _, pgid := range []int{pgPgid, serverPgid, runnerPgid} {
		h.groupSig.onTerm[pgid] = func() { h.groupSig.set(pgid, pgToken(pgid), false) }
	}
	h.containers.onRemove[natsContainerNameTest] = func() {
		h.containers.setExistsName(natsContainerNameTest, false)
	}

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached = %v, want nil", err)
	}
	got := ctrEvents(h.rec.snapshot())
	want := []string{"ctr-stop " + natsContainerNameTest, "ctr-rm " + natsContainerNameTest}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nats escalation:\n got  %v\n want %v (stop then rm -f)", got, want)
	}
	assertPgidFileGone(t, cfg.StateDir)
}

const gatewayContainerNameTest = "compass-gateway-test01"

func seedGatewayRecord(t *testing.T, cfg Config, h *harness) {
	t.Helper()
	rec := pgidRecord{WriterPid: 4242, Version: pgidFileVersion, Entries: []pgidEntry{{Kind: entryProc, Component: ComponentPostgres, Pgid: pgPgid, StartTime: pgToken(pgPgid)}, {Kind: entryContainer, Component: ComponentGateway, ContainerName: gatewayContainerNameTest}, {Kind: entryProc, Component: ComponentServer, Pgid: serverPgid, StartTime: pgToken(serverPgid)}, {Kind: entryProc, Component: ComponentRunner, Pgid: runnerPgid, StartTime: pgToken(runnerPgid)}}}
	if err := writePgidFile(cfg.StateDir, rec); err != nil {
		t.Fatal(err)
	}
	h.containers.setExistsName(gatewayContainerNameTest, true)
	h.groupSig.set(pgPgid, pgToken(pgPgid), true)
	h.groupSig.set(serverPgid, pgToken(serverPgid), true)
	h.groupSig.set(runnerPgid, pgToken(runnerPgid), true)
}

func TestDownDetachedGatewayStopsThenRemoves(t *testing.T) {
	cfg, h := newHarness(t)
	seedGatewayRecord(t, cfg, h)
	deps := sidecarContainerDownDeps(t, h)
	for _, p := range []int{pgPgid, serverPgid, runnerPgid} {
		pgid := p
		h.groupSig.onTerm[pgid] = func() { h.groupSig.set(pgid, pgToken(pgid), false) }
	}
	h.containers.onStop[gatewayContainerNameTest] = func() { h.containers.setExistsName(gatewayContainerNameTest, false) }
	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	got := ctrEvents(h.rec.snapshot())
	want := []string{"ctr-stop " + gatewayContainerNameTest, "ctr-rm " + gatewayContainerNameTest}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gateway teardown = %v, want stop then remove", got)
	}
}

// assertPgidFileGone fails if the pgid record still exists.
func assertPgidFileGone(t *testing.T, stateDir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(stateDir, pgidFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pgid file still present; want removed (err=%v)", err)
	}
}

// writeLockHeldBy writes a lockfile recording pid as the holder, so
// lockHolderLive(path) reports the holder's liveness.
func writeLockHeldBy(t *testing.T, stateDir string, pid int) {
	t.Helper()
	path := filepath.Join(stateDir, lockFileName)
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatalf("write lockfile = %v", err)
	}
}

// TestSurvivorRecordV1RoundTrip is the FIX-1 regression: a v1 record (header
// version "1", untagged proc entries) read from a shipped build, narrowed to
// survivors on the partial-teardown path, then rewritten and reread must survive
// the round-trip. Before the fix writePgidFile stamped the carried "1" header
// over v2 `proc` grammar, so the reread dispatched to the v1 parser and
// hard-errored `malformed proc entry line`, turning a recoverable partial
// teardown into a permanently unreadable record.
func TestSurvivorRecordV1RoundTrip(t *testing.T) {
	dir := t.TempDir()
	rec := pgidRecord{
		WriterPid: 7,
		Version:   pgidFileVersionV1,
		Entries: []pgidEntry{
			{Kind: entryProc, Component: ComponentPostgres, Pgid: 200, StartTime: 999},
			{Kind: entryProc, Component: ComponentServer, Pgid: 201, StartTime: 1000},
			{Kind: entryProc, Component: ComponentRunner, Pgid: 202, StartTime: 1001},
		},
	}

	stubBootID(t, testBootID)
	survivors := survivorRecord(rec, []Component{ComponentServer, ComponentRunner})
	if err := writePgidFile(dir, survivors); err != nil {
		t.Fatalf("writePgidFile survivor = %v", err)
	}

	got, err := readPgidFile(dir)
	if err != nil {
		t.Fatalf("readPgidFile after survivor round-trip = %v; want success", err)
	}
	want := pgidRecord{
		WriterPid: 7,
		Version:   pgidFileVersion,
		BootID:    testBootID,
		Entries: []pgidEntry{
			{Kind: entryProc, Component: ComponentServer, Pgid: 201, StartTime: 1000},
			{Kind: entryProc, Component: ComponentRunner, Pgid: 202, StartTime: 1001},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reread record = %+v; want %+v", got, want)
	}
}

// TestDownDetachedRebootedRecordSignalsNothing proves a record from an earlier
// boot, with no answering socket, signals no group and is removed.
func TestDownDetachedRebootedRecordSignalsNothing(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := darkSocketDownDeps(t, h)
	// The live groups now belong to whoever holds those pids this boot.
	stubBootID(t, "99999999-8888-7777-6666-555555555555")

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached on a prior-boot record = %v, want nil", err)
	}
	if got := signalEvents(h.rec.snapshot()); len(got) != 0 {
		t.Fatalf("prior-boot record signalled groups: %v", got)
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// TestDownDetachedRebootedRecordLiveSocketRefuses proves an answering socket
// contradicts a boot mismatch: down refuses, keeps the record, and signals nothing.
func TestDownDetachedRebootedRecordLiveSocketRefuses(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := downTestDeps(t, h)
	deps.Prober = fixedServerProber(true)
	stubBootID(t, "99999999-8888-7777-6666-555555555555")

	if err := DownDetached(context.Background(), cfg, deps); err == nil {
		t.Fatal("DownDetached with a live socket under a boot mismatch = nil, want a refusal")
	}
	if got := signalEvents(h.rec.snapshot()); len(got) != 0 {
		t.Fatalf("refusal signalled groups: %v", got)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, pgidFileName)); err != nil {
		t.Fatalf("pgid record after refusal: %v; want it kept", err)
	}
}

// TestDownDetachedRebootedRecordStillChecksContainers proves a reboot drops only
// process entries: a container keeps its name identity and is confirmed by absence.
func TestDownDetachedRebootedRecordStillChecksContainers(t *testing.T) {
	cfg, h := newHarness(t)
	seedGatewayRecord(t, cfg, h)
	deps := sidecarContainerDownDeps(t, h)
	deps.Prober = fixedServerProber(false)
	stubBootID(t, "99999999-8888-7777-6666-555555555555")
	h.containers.onStop[gatewayContainerNameTest] = func() { h.containers.setExistsName(gatewayContainerNameTest, false) }

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached on a prior-boot record = %v, want nil", err)
	}
	events := h.rec.snapshot()
	if got := signalEvents(events); len(got) != 0 {
		t.Fatalf("prior-boot record signalled groups: %v", got)
	}
	want := []string{"ctr-stop " + gatewayContainerNameTest, "ctr-rm " + gatewayContainerNameTest}
	if got := ctrEvents(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("prior-boot container teardown = %v, want %v", got, want)
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// TestDownDetachedLegacyHeaderKeepsIdentityTeardown proves a record with no boot
// id (an older build) is an unknown boot and tears down exactly as before.
func TestDownDetachedLegacyHeaderKeepsIdentityTeardown(t *testing.T) {
	cfg, h := newHarness(t)
	legacy := "2 4242\nproc postgres " + strconv.Itoa(pgPgid) + " " + strconv.FormatUint(pgToken(pgPgid), 10) + "\n"
	if err := os.WriteFile(filepath.Join(cfg.StateDir, pgidFileName), []byte(legacy), 0o600); err != nil {
		t.Fatalf("seed legacy record = %v", err)
	}
	h.groupSig.set(pgPgid, pgToken(pgPgid), true)
	h.groupSig.onTerm[pgPgid] = func() { h.groupSig.set(pgPgid, pgToken(pgPgid), false) }
	deps := downTestDeps(t, h)

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached(legacy) = %v, want nil", err)
	}
	if got, want := signalEvents(h.rec.snapshot()), []string{"group-term " + strconv.Itoa(pgPgid)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy teardown:\n got  %v\n want %v", got, want)
	}
	assertPgidFileGone(t, cfg.StateDir)
}
