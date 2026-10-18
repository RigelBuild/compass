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

// Harness pgids and tokens are deterministic; tests seed matching group identities from them.
const (
	pgPgid     = 1001
	serverPgid = 1002
	runnerPgid = 1003
)

func pgToken(pgid int) uint64 { return uint64(pgid) * 10 }

// downTestDeps shrinks drain budgets while preserving group-backed probes unless a test replaces them.
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

// shrinkBudgets makes deadline behavior real without waiting on wall-clock durations.
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

// stepClock advances on each read so budgets elapse by poll count, not wall time.
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

// assertKillAfterBudget requires SIGKILL no earlier than the full SIGTERM drain budget.
func assertKillAfterBudget(t *testing.T, termAt, killAt time.Time, budget time.Duration) {
	t.Helper()
	if termAt.IsZero() || killAt.IsZero() {
		t.Fatalf("SIGTERM at %v, SIGKILL at %v; want both delivered", termAt, killAt)
	}
	if gap := killAt.Sub(termAt); gap < budget {
		t.Fatalf("SIGKILL %v after SIGTERM, want at least the %v drain budget", gap, budget)
	}
}

// seedFullRecord writes all three process identities and marks their groups live.
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
	if err := writePgidFile(t.Context(), cfg.StateDir, rec); err != nil {
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

// TestDownDetachedReverseOrderSIGTERM pins reverse-order graceful teardown with no SIGKILL.
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

// TestDownDetachedEscalatesToSIGKILL checks one kill after a stubborn group exceeds its drain budget.
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

// TestDownDetachedServerSocketDarkWaitsForGroupExit ensures a dark socket does not confirm a live group.
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

// TestDownDetachedPostgresSocketDarkWaitsForGroupExit applies the same check to postgres.
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

// TestDownDetachedOrphanedGroupEscalates keeps a group with surviving members signalable after its leader exits.
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

// TestDownDetachedRecycledBeforeSIGKILLIsNotSignalled rechecks identity before escalating a recycled pgid.
func TestDownDetachedRecycledBeforeSIGKILLIsNotSignalled(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := darkSocketDownDeps(t, h)
	clock := newStepClock(time.Millisecond)
	var termAt time.Time
	// Recycle the runner on the clock read that expires its budget.
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

// TestDownDetachedRecycledBeforeSIGTERMIsNotSignalled rechecks identity immediately before signalling.
func TestDownDetachedRecycledBeforeSIGTERMIsNotSignalled(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := darkSocketDownDeps(t, h)
	h.groupSig.onTerm[pgPgid] = func() { h.groupSig.set(pgPgid, pgToken(pgPgid), false) }
	// The runner SIGTERM coincides with server pgid reuse.
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

// TestDownDetachedFailedSIGKILLDoesNotShortCircuit reports a live group when SIGKILL was not delivered.
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

// TestDownDetachedCanceledSIGKILLDoesNotShortCircuit rechecks liveness after cancellation during kill.
func TestDownDetachedCanceledSIGKILLDoesNotShortCircuit(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := darkSocketDownDeps(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.groupSig.onTerm[pgPgid] = func() { h.groupSig.set(pgPgid, pgToken(pgPgid), false) }
	h.groupSig.onTerm[serverPgid] = func() { h.groupSig.set(serverPgid, pgToken(serverPgid), false) }
	// Kill is delivered but cancellation leaves the group present.
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

// TestDownDetachedPartialFailureRewritesSurvivors retains and reports a socket-confirmed survivor for retry.
func TestDownDetachedPartialFailureRewritesSurvivors(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := downTestDeps(t, h)

	// Runner and postgres drain; the server remains alive and answers after SIGKILL.
	h.groupSig.onTerm[pgPgid] = func() { h.groupSig.set(pgPgid, pgToken(pgPgid), false) }
	h.groupSig.onTerm[runnerPgid] = func() { h.groupSig.set(runnerPgid, pgToken(runnerPgid), false) }
	// The server stays alive and its socket continues answering.

	err := DownDetached(context.Background(), cfg, deps)
	if err == nil {
		t.Fatal("DownDetached = nil, want partial-failure error naming the survivor")
	}

	// The record retains only the surviving server entry.
	rec, rerr := readPgidFile(cfg.StateDir)
	if rerr != nil {
		t.Fatalf("survivor record read = %v, want the rewritten survivor set", rerr)
	}
	if len(rec.Entries) != 1 || rec.Entries[0].Component != ComponentServer {
		t.Fatalf("survivor record = %+v, want exactly the compass-server entry", rec.Entries)
	}
}

// TestDownDetachedRefusesLiveUp rejects teardown while a live Up holds the lock.
func TestDownDetachedRefusesLiveUp(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := downTestDeps(t, h)

	// The current process is the live lockfile holder.
	writeLockHeldBy(t, cfg.StateDir, os.Getpid())

	err := DownDetached(context.Background(), cfg, deps)
	if !errors.Is(err, ErrStackStarting) {
		t.Fatalf("DownDetached = %v, want ErrStackStarting", err)
	}
	if got := signalEvents(h.rec.snapshot()); len(got) != 0 {
		t.Fatalf("refusal must signal nothing, got %v", got)
	}
	// Refusal leaves the teardown record available for a later down.
	if _, rerr := readPgidFile(cfg.StateDir); rerr != nil {
		t.Fatalf("pgid record should be untouched on refusal: %v", rerr)
	}
}

// TestDownDetachedSkipsRecycledAndDeadGroups signals only live identities that still match.
func TestDownDetachedSkipsRecycledAndDeadGroups(t *testing.T) {
	cfg, h := newHarness(t)
	seedFullRecord(t, cfg, h)
	deps := downTestDeps(t, h)

	// Postgres is gone; the server pgid now belongs to an unrelated process.
	h.groupSig.set(pgPgid, pgToken(pgPgid), false)
	h.groupSig.set(serverPgid, 999999, true)
	// Runner remains live and drains on SIGTERM.
	h.groupSig.onTerm[runnerPgid] = func() { h.groupSig.set(runnerPgid, pgToken(runnerPgid), false) }

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached = %v, want nil (dead+recycled skipped, live torn down)", err)
	}
	events := signalEvents(h.rec.snapshot())
	// Neither the dead postgres group nor recycled server is signalled.
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

// TestDownDetachedHalfSpawnedPrefix drains only the postgres entry in a partial record.
func TestDownDetachedHalfSpawnedPrefix(t *testing.T) {
	cfg, h := newHarness(t)
	rec := pgidRecord{
		WriterPid: 1, Version: pgidFileVersion,
		Entries: []pgidEntry{{Component: ComponentPostgres, Pgid: pgPgid, StartTime: pgToken(pgPgid)}},
	}
	if err := writePgidFile(t.Context(), cfg.StateDir, rec); err != nil {
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

// TestDownDetachedNilContainersAllProcessRecord verifies process-only records do not need a container controller.
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
	if err := writePgidFile(t.Context(), cfg.StateDir, rec); err != nil {
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

// TestDownDetachedAbsentFileAnsweringSocketErrors refuses to guess teardown identities for a live stack.
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

// TestDownDetachedConcurrentSerializedByGuard ensures one of two downs consumes and signals the record.
// The loser may see a no-record error while probing the winner's in-flight drain; that race is safe.
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

	// One down succeeds; any loser error may only be ErrNoTeardownRecord.
	if e1 != nil && e2 != nil {
		t.Fatalf("both concurrent downs failed: %v / %v", e1, e2)
	}
	for _, e := range []error{e1, e2} {
		if e != nil && !errors.Is(e, ErrNoTeardownRecord) {
			t.Fatalf("unexpected concurrent-down error %v; only a benign ErrNoTeardownRecord race is allowed", e)
		}
	}

	// The guard guarantees each group is signalled once across both downs.
	events := signalEvents(h.rec.snapshot())
	for _, pgid := range []int{runnerPgid, serverPgid, pgPgid} {
		if n := countEvent(events, "group-term "+strconv.Itoa(pgid)); n != 1 {
			t.Fatalf("group %d SIGTERMed %d times across concurrent downs; want exactly 1 (no double-signal): %v", pgid, n, events)
		}
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// The stable name persisted for a v2 postgres container entry.
const pgContainerName = "compass-postgres-test01"

// seedContainerRecord writes a live record with a postgres container and server/runner process entries.
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
	if err := writePgidFile(t.Context(), cfg.StateDir, rec); err != nil {
		t.Fatalf("seed container record = %v", err)
	}
	h.containers.setExists(true)
	h.groupSig.set(serverPgid, pgToken(serverPgid), true)
	h.groupSig.set(runnerPgid, pgToken(runnerPgid), true)
}

// containerBackedDBProber reports postgres reachable while its container exists.
type containerBackedDBProber struct {
	c    *fakeContainerController
	name string
}

func (p *containerBackedDBProber) ProbeDB(ctx context.Context, dsn string) error {
	if p.c.Exists(ctx, p.name) {
		return nil // container up → socket answers → reachable
	}
	return errPostgresNotReady // container gone → socket dark → confirmed dead
}

// containerDownDeps confirms process groups by liveness and postgres by container presence.
func containerDownDeps(t *testing.T, h *harness) Deps {
	t.Helper()
	shrinkBudgets(t)
	deps := h.deps
	deps.Prober = &groupBackedProber{gs: h.groupSig, pgid: serverPgid, token: pgToken(serverPgid)}
	deps.DBProber = &containerBackedDBProber{c: h.containers, name: pgContainerName}
	return deps
}

// ctrEvents keeps container stop and remove events in order.
func ctrEvents(events []string) []string {
	var out []string
	for _, e := range events {
		if len(e) >= 4 && e[:4] == "ctr-" {
			out = append(out, e)
		}
	}
	return out
}

// TestDownDetachedContainerGracefulStop confirms by-name stop with no force-remove escalation.
func TestDownDetachedContainerGracefulStop(t *testing.T) {
	cfg, h := newHarness(t)
	seedContainerRecord(t, cfg, h)
	deps := containerDownDeps(t, h)

	// SIGTERM tears the two groups down; `podman stop` removes the container.
	h.groupSig.onTerm[serverPgid] = func() { h.groupSig.set(serverPgid, pgToken(serverPgid), false) }
	h.groupSig.onTerm[runnerPgid] = func() { h.groupSig.set(runnerPgid, pgToken(runnerPgid), false) }
	h.containers.onStop[pgContainerName] = func(context.Context) { h.containers.setExists(false) }

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

// TestDownDetachedContainerEscalatesToRemove force-removes postgres that survives graceful stop.
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

// TestDownDetachedContainerSurvivorRewritesRecord keeps the present container as the sole retry entry.
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

// TestDownDetachedContainerGoneIsSkipped skips an absent container while still draining live processes.
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

// TestDownDetachedContainerDarkDBStillExistsIsSurvivor confirms container postgres only by its absence.
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

// TestDownDetachedCancelledDuringSlowContainerStopRewritesSurvivors preserves entries when stop is canceled.
func TestDownDetachedCancelledDuringSlowContainerStopRewritesSurvivors(t *testing.T) {
	cfg, h := newHarness(t)
	seedContainerRecord(t, cfg, h)
	deps := containerDownDeps(t, h)
	h.groupSig.onTerm[serverPgid] = func() { h.groupSig.set(serverPgid, pgToken(serverPgid), false) }
	h.groupSig.onTerm[runnerPgid] = func() { h.groupSig.set(runnerPgid, pgToken(runnerPgid), false) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.containers.onStop[pgContainerName] = func(stopCtx context.Context) {
		cancel() // the down process is signalled mid-stop
		if stopCtx.Err() == nil {
			t.Error("container stop ran on a ctx the caller's cancellation cannot reach")
		}
	}

	err := DownDetached(ctx, cfg, deps)
	if err == nil || !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("DownDetached = %v, want a survivor error naming postgres", err)
	}
	rec, rerr := readPgidFile(cfg.StateDir)
	if rerr != nil {
		t.Fatalf("survivor record read = %v, want the rewritten survivor set", rerr)
	}
	if len(rec.Entries) != 1 || rec.Entries[0].ContainerName != pgContainerName {
		t.Fatalf("survivor record = %+v, want exactly the postgres container entry", rec.Entries)
	}
}

// TestDownDetachedTierSignalDoesNotWaitOnContainerDrain: signalling a tier never
// blocks on a container's drain. The gateway's stop is sent first in its tier,
// and the runner and server get their SIGTERM before any container is removed.
func TestDownDetachedTierSignalDoesNotWaitOnContainerDrain(t *testing.T) {
	cfg, h := newHarness(t)
	seedGatewayRecord(t, cfg, h)
	deps := sidecarContainerDownDeps(t, h)
	for _, p := range []int{pgPgid, serverPgid, runnerPgid} {
		pgid := p
		h.groupSig.onTerm[pgid] = func() { h.groupSig.set(pgid, pgToken(pgid), false) }
	}
	// Move gateway stop first to prove it does not delay the consumer signals.
	rec, err := readPgidFile(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	h.containers.onStop[gatewayContainerNameTest] = func(context.Context) { h.containers.setExited(gatewayContainerNameTest) }
	targets := liveTargets(context.Background(), cfg, deps, rec)
	for i, tg := range targets {
		if tg.entry.Component == ComponentGateway {
			targets[0], targets[i] = targets[i], targets[0]
		}
	}
	if survivors := drainTier(context.Background(), deps, targets[:3]); len(survivors) != 0 {
		t.Fatalf("survivors = %v, want none", survivors)
	}
	events := h.rec.snapshot()
	runnerTerm := indexOf(events, "group-term "+strconv.Itoa(runnerPgid))
	serverTerm := indexOf(events, "group-term "+strconv.Itoa(serverPgid))
	if indexOf(events, "ctr-stop "+gatewayContainerNameTest) != 0 || runnerTerm < 0 || serverTerm < 0 {
		t.Fatalf("want the gateway stop first and both consumers signalled: %v", events)
	}
	lastTerm := max(runnerTerm, serverTerm)
	for i, e := range events[:lastTerm] {
		if strings.HasPrefix(e, "ctr-rm") {
			t.Fatalf("event %d %q removed a container before its tier was signalled: %v", i, e, events)
		}
	}
}

// TestDownDetachedInfraTierWaitsForConsumerTier holds NATS and postgres until the server drains or is killed.
func TestDownDetachedInfraTierWaitsForConsumerTier(t *testing.T) {
	cfg, h := newHarness(t)
	seedNatsRecord(t, cfg, h)
	deps := sidecarContainerDownDeps(t, h)
	h.groupSig.onTerm[runnerPgid] = func() { h.groupSig.set(runnerPgid, pgToken(runnerPgid), false) }
	h.groupSig.onKill[serverPgid] = func() { h.groupSig.set(serverPgid, pgToken(serverPgid), false) }
	h.groupSig.onTerm[pgPgid] = func() { h.groupSig.set(pgPgid, pgToken(pgPgid), false) }
	h.containers.onStop[natsContainerNameTest] = func(context.Context) { h.containers.setExistsName(natsContainerNameTest, false) }

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	events := h.rec.snapshot()
	serverKill := indexOf(events, "group-kill "+strconv.Itoa(serverPgid))
	if serverKill < 0 {
		t.Fatalf("server never escalated: %v", events)
	}
	for _, infra := range []string{"ctr-stop " + natsContainerNameTest, "group-term " + strconv.Itoa(pgPgid)} {
		if i := indexOf(events, infra); i < serverKill {
			t.Fatalf("%q at %d, before the server's budget expired at %d: %v", infra, i, serverKill, events)
		}
	}
}

// TestDownDetachedCancelInConsumerTierStillStopsInfra sends infra stops on a live context after cancellation.
func TestDownDetachedCancelInConsumerTierStillStopsInfra(t *testing.T) {
	cfg, h := newHarness(t)
	seedNatsRecord(t, cfg, h)
	deps := sidecarContainerDownDeps(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.groupSig.onTerm[runnerPgid] = func() {
		cancel() // the down process is signalled during the consumer tier
		h.groupSig.set(runnerPgid, pgToken(runnerPgid), false)
	}
	h.groupSig.onTerm[serverPgid] = func() { h.groupSig.set(serverPgid, pgToken(serverPgid), false) }
	var stopCtxErr error
	h.containers.onStop[natsContainerNameTest] = func(stopCtx context.Context) { stopCtxErr = stopCtx.Err() }

	err := DownDetached(ctx, cfg, deps)
	if err == nil {
		t.Fatal("DownDetached = nil, want a survivor error for the unconfirmed infra tier")
	}
	if indexOf(h.rec.snapshot(), "ctr-stop "+natsContainerNameTest) < 0 {
		t.Fatalf("nats never got its stop after cancel: %v", h.rec.snapshot())
	}
	if stopCtxErr != nil {
		t.Fatalf("nats stop ran on a done ctx (%v); exec would refuse it", stopCtxErr)
	}
	rec, rerr := readPgidFile(cfg.StateDir)
	if rerr != nil {
		t.Fatalf("survivor record read = %v", rerr)
	}
	got := map[Component]bool{}
	for _, e := range rec.Entries {
		got[e.Component] = true
	}
	if !got[ComponentNats] || !got[ComponentPostgres] {
		t.Fatalf("survivor record = %+v, want nats and postgres", rec.Entries)
	}
}

// The stable name a v2 collector container entry carries in these tests.
const collectorContainerNameTest = "compass-otel-collector-test01"

// seedCollectorRecord writes a live v2 record with a collector container between the server and postgres.
// This hermetic case ensures detached teardown includes ComponentCollector without requiring podman.
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
	if err := writePgidFile(t.Context(), cfg.StateDir, rec); err != nil {
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

// TestDownDetachedCollectorContainerTornDownByName verifies graceful by-name teardown between server and postgres.
func TestDownDetachedCollectorContainerTornDownByName(t *testing.T) {
	cfg, h := newHarness(t)
	seedCollectorRecord(t, cfg, h)
	deps := sidecarContainerDownDeps(t, h)

	for _, pgid := range []int{pgPgid, serverPgid, runnerPgid} {
		h.groupSig.onTerm[pgid] = func() { h.groupSig.set(pgid, pgToken(pgid), false) }
	}
	h.containers.onStop[collectorContainerNameTest] = func(context.Context) {
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

// TestDownDetachedCollectorEscalatesToRemove force-removes a collector that ignores graceful stop.
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

// seedNatsRecord writes a live v2 record with a NATS container between the server and postgres.
// This hermetic case ensures detached teardown includes ComponentNats without requiring podman.
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
	if err := writePgidFile(t.Context(), cfg.StateDir, rec); err != nil {
		t.Fatalf("seed nats record = %v", err)
	}
	h.containers.setExistsName(natsContainerNameTest, true)
	h.groupSig.set(pgPgid, pgToken(pgPgid), true)
	h.groupSig.set(serverPgid, pgToken(serverPgid), true)
	h.groupSig.set(runnerPgid, pgToken(runnerPgid), true)
}

// TestDownDetachedNatsContainerTornDownByName verifies graceful reverse-order teardown so JetStream can flush.
func TestDownDetachedNatsContainerTornDownByName(t *testing.T) {
	cfg, h := newHarness(t)
	seedNatsRecord(t, cfg, h)
	deps := sidecarContainerDownDeps(t, h)

	for _, pgid := range []int{pgPgid, serverPgid, runnerPgid} {
		h.groupSig.onTerm[pgid] = func() { h.groupSig.set(pgid, pgToken(pgid), false) }
	}
	h.containers.onStop[natsContainerNameTest] = func(context.Context) {
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
	if err := writePgidFile(t.Context(), cfg.StateDir, rec); err != nil {
		t.Fatal(err)
	}
	h.containers.setExistsName(gatewayContainerNameTest, true)
	h.groupSig.set(pgPgid, pgToken(pgPgid), true)
	h.groupSig.set(serverPgid, pgToken(serverPgid), true)
	h.groupSig.set(runnerPgid, pgToken(runnerPgid), true)
}

// TestDownDetachedGatewayExitedIsRemovedAndConfirmed: the gateway runs without
// --rm, so after the stop signal it exits but lingers; the confirm poll removes
// it with a non-forced rm and confirms by absence, with no rm -f escalation.
func TestDownDetachedGatewayExitedIsRemovedAndConfirmed(t *testing.T) {
	cfg, h := newHarness(t)
	seedGatewayRecord(t, cfg, h)
	deps := sidecarContainerDownDeps(t, h)
	for _, p := range []int{pgPgid, serverPgid, runnerPgid} {
		pgid := p
		h.groupSig.onTerm[pgid] = func() { h.groupSig.set(pgid, pgToken(pgid), false) }
	}
	h.containers.onStop[gatewayContainerNameTest] = func(context.Context) { h.containers.setExited(gatewayContainerNameTest) }
	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	got := ctrEvents(h.rec.snapshot())
	want := []string{"ctr-stop " + gatewayContainerNameTest, "ctr-rm-exited " + gatewayContainerNameTest}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gateway teardown = %v, want stop then non-forced rm", got)
	}
	assertPgidFileGone(t, cfg.StateDir)
}

// TestDownDetachedGatewayStillRunningEscalatesToRemove: while the gateway runs,
// the non-forced rm is a no-op, so it stays present until the budget's rm -f.
func TestDownDetachedGatewayStillRunningEscalatesToRemove(t *testing.T) {
	cfg, h := newHarness(t)
	seedGatewayRecord(t, cfg, h)
	deps := sidecarContainerDownDeps(t, h)
	for _, p := range []int{pgPgid, serverPgid, runnerPgid} {
		pgid := p
		h.groupSig.onTerm[pgid] = func() { h.groupSig.set(pgid, pgToken(pgid), false) }
	}
	h.containers.onRemove[gatewayContainerNameTest] = func() { h.containers.setExistsName(gatewayContainerNameTest, false) }
	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	got := ctrEvents(h.rec.snapshot())
	want := []string{"ctr-stop " + gatewayContainerNameTest, "ctr-rm " + gatewayContainerNameTest}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gateway teardown = %v, want stop then rm -f", got)
	}
	assertPgidFileGone(t, cfg.StateDir)
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
	if err := writePgidFile(t.Context(), dir, survivors); err != nil {
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
	h.containers.onStop[gatewayContainerNameTest] = func(context.Context) { h.containers.setExited(gatewayContainerNameTest) }

	if err := DownDetached(context.Background(), cfg, deps); err != nil {
		t.Fatalf("DownDetached on a prior-boot record = %v, want nil", err)
	}
	events := h.rec.snapshot()
	if got := signalEvents(events); len(got) != 0 {
		t.Fatalf("prior-boot record signalled groups: %v", got)
	}
	want := []string{"ctr-stop " + gatewayContainerNameTest, "ctr-rm-exited " + gatewayContainerNameTest}
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
