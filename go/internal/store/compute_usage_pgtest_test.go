//go:build pgtest

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type computeUsageEvent struct {
	TenantID       string
	ID             string
	IntervalID     string
	Kind           string
	AgentAccountID string
	OwnerUserID    string
	SessionID      string
	RunnerID       string
	OccurredAt     time.Time
	Estimated      bool
}

func computeEvents(t *testing.T, s *Store, tenant TenantID, accountID AccountID) []computeUsageEvent {
	t.Helper()
	rows, err := s.pool.Query(t.Context(), `
		SELECT tenant_id, id, interval_id, kind, agent_account_id, owner_user_id,
		       session_id, runner_id, occurred_at, estimated
		  FROM compute_usage_events
		 WHERE tenant_id = $1 AND agent_account_id = $2
		 ORDER BY session_id, CASE kind WHEN 'start' THEN 0 ELSE 1 END`, string(tenant), string(accountID))
	if err != nil {
		t.Fatalf("read compute usage events: %v", err)
	}
	defer rows.Close()
	events := []computeUsageEvent{}
	for rows.Next() {
		var event computeUsageEvent
		if err := rows.Scan(&event.TenantID, &event.ID, &event.IntervalID, &event.Kind,
			&event.AgentAccountID, &event.OwnerUserID, &event.SessionID, &event.RunnerID,
			&event.OccurredAt, &event.Estimated); err != nil {
			t.Fatalf("scan compute usage event: %v", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate compute usage events: %v", err)
	}
	return events
}

func TestComputeUsageBindingLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "compute-owner")
	agent := mustAgent(t, s, owner.ID, "compute-agent")
	tenant := s.EffectiveTenant(ctx)

	mustBind(t, ctx, s, "compute-session", agent.ID, "compute-runner")
	events := computeEvents(t, s, tenant, agent.ID)
	if len(events) != 1 {
		t.Fatalf("events after first bind = %+v, want one start", events)
	}
	start := events[0]
	if start.Kind != "start" || start.TenantID != string(tenant) || start.OwnerUserID != string(owner.ID) ||
		start.AgentAccountID != string(agent.ID) || start.SessionID != "compute-session" ||
		start.RunnerID != "compute-runner" || start.Estimated {
		t.Fatalf("start event = %+v, want the tenant, owner, agent, session, runner, and non-estimated start", start)
	}

	if err := s.DeleteSessionBinding(ctx, "compute-session"); err != nil {
		t.Fatalf("DeleteSessionBinding: %v", err)
	}
	events = computeEvents(t, s, tenant, agent.ID)
	if len(events) != 2 {
		t.Fatalf("events after delete = %+v, want start and end", events)
	}
	var end computeUsageEvent
	for _, event := range events {
		if event.Kind == "end" {
			end = event
		}
	}
	if end.ID == "" || end.IntervalID != start.IntervalID || end.TenantID != string(tenant) ||
		end.OwnerUserID != string(owner.ID) || end.SessionID != start.SessionID || end.RunnerID != start.RunnerID || end.Estimated {
		t.Fatalf("end event = %+v, want same interval metadata and a non-estimated end", end)
	}
	if err := s.DeleteSessionBinding(ctx, "compute-session"); err != nil {
		t.Fatalf("second DeleteSessionBinding: %v", err)
	}
	if got := computeEvents(t, s, tenant, agent.ID); len(got) != 2 {
		t.Fatalf("events after duplicate delete = %+v, want no additional end", got)
	}
}

func TestComputeUsageRepointClosesPriorInterval(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "compute-repoint-owner")
	agent := mustAgent(t, s, owner.ID, "compute-repoint-agent")
	tenant := s.EffectiveTenant(ctx)
	mustBind(t, ctx, s, "session-before", agent.ID, "runner-before")
	mustBind(t, ctx, s, "session-after", agent.ID, "runner-after")

	events := computeEvents(t, s, tenant, agent.ID)
	if len(events) != 3 {
		t.Fatalf("events after re-point = %+v, want prior start/end and replacement start", events)
	}
	starts := map[string]computeUsageEvent{}
	ends := map[string]computeUsageEvent{}
	for _, event := range events {
		switch event.Kind {
		case "start":
			starts[event.SessionID] = event
		case "end":
			ends[event.SessionID] = event
		}
	}
	before, hasBefore := starts["session-before"]
	after, hasAfter := starts["session-after"]
	closed, hasEnd := ends["session-before"]
	if !hasBefore || !hasAfter || !hasEnd || before.IntervalID == after.IntervalID || closed.IntervalID != before.IntervalID {
		t.Fatalf("re-point events = %+v, want distinct starts and end of displaced interval", events)
	}
	if closed.OccurredAt.After(after.OccurredAt) {
		t.Fatalf("re-point end occurred at %s, after replacement start at %s", closed.OccurredAt, after.OccurredAt)
	}
	if after.RunnerID != "runner-after" || closed.RunnerID != "runner-before" || closed.OwnerUserID != string(owner.ID) {
		t.Fatalf("re-point event metadata = %+v, want runner and owner preserved per interval", events)
	}
}

// A bind that waits on the account lock must stamp its events after the wait,
// so the interval it opens never starts before the gate released.
func TestComputeUsageEventsStampedAfterLockWait(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "compute-wait-owner")
	agent := mustAgent(t, s, owner.ID, "compute-wait-agent")
	tenant := s.EffectiveTenant(ctx)
	mustBind(t, ctx, s, "wait-before", agent.ID, "runner-1")

	gate, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin gate tx: %v", err)
	}
	defer func() {
		if err := gate.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("rollback gate: %v", err)
		}
	}()
	// Character-identical to LockSessionBindingAccount.
	if _, err := gate.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('binding:' || $1 || ':' || $2))`,
		string(tenant), string(agent.ID)); err != nil {
		t.Fatalf("gate advisory lock: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := s.RecordSessionBinding(ctx, "wait-after", agent.ID, "runner-1")
		done <- err
	}()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiters int
		if err := gate.QueryRow(ctx,
			`WITH k AS (SELECT hashtext('binding:' || $1 || ':' || $2)::bigint AS key)
			 SELECT count(*) FROM pg_locks, k
			 WHERE locktype = 'advisory' AND NOT granted
			   AND classid = ((k.key >> 32) & 4294967295)::oid
			   AND objid = (k.key & 4294967295)::oid`,
			string(tenant), string(agent.ID)).Scan(&waiters); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if waiters >= 1 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("bind finished (err=%v) while the gate held the account lock", err)
		case <-deadline:
			t.Fatal("bind never waited on the account lock")
		case <-tick.C:
		}
	}
	var released time.Time
	if err := gate.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&released); err != nil {
		t.Fatalf("read release time: %v", err)
	}
	if err := gate.Rollback(ctx); err != nil {
		t.Fatalf("release gate: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("RecordSessionBinding after release: %v", err)
	}

	checked := 0
	for _, event := range computeEvents(t, s, tenant, agent.ID) {
		if event.SessionID == "wait-after" || event.Kind == "end" {
			checked++
			if event.OccurredAt.Before(released) {
				t.Fatalf("%s event for %s stamped %s, before the lock release at %s", event.Kind, event.SessionID, event.OccurredAt, released)
			}
		}
	}
	if checked != 2 {
		t.Fatalf("checked %d events, want the displaced end and the replacement start", checked)
	}
}

func TestComputeUsageConflictRollsBackIntervalEvents(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "compute-conflict-owner")
	accountX := mustAgent(t, s, owner.ID, "compute-conflict-x")
	accountY := mustAgent(t, s, owner.ID, "compute-conflict-y")
	tenant := s.EffectiveTenant(ctx)
	mustBind(t, ctx, s, "sess-1", accountX.ID, "runner-1")
	mustBind(t, ctx, s, "sess-2", accountY.ID, "runner-1")

	before := computeEvents(t, s, tenant, accountX.ID)
	if _, _, err := s.RecordSessionBinding(ctx, "sess-2", accountX.ID, "runner-1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("RecordSessionBinding(X, sess-2) error = %v, want ErrConflict", err)
	}
	after := computeEvents(t, s, tenant, accountX.ID)
	if len(before) != 1 || len(after) != 1 || after[0].Kind != "start" || after[0].SessionID != "sess-1" ||
		after[0].IntervalID != before[0].IntervalID || !after[0].OccurredAt.Equal(before[0].OccurredAt) {
		t.Fatalf("account X events before=%+v after=%+v, want its unchanged open start only", before, after)
	}
	if sessionID, runnerID, err := s.SessionForAccount(ctx, accountX.ID); err != nil || sessionID != "sess-1" || runnerID != "runner-1" {
		t.Fatalf("account X resolves to (%q, %q, %v), want (sess-1, runner-1)", sessionID, runnerID, err)
	}
}

func TestComputeUsageSameSessionRunnerRebindKeepsInterval(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "compute-runner-owner")
	agent := mustAgent(t, s, owner.ID, "compute-runner-agent")
	tenant := s.EffectiveTenant(ctx)
	mustBind(t, ctx, s, "stable-session", agent.ID, "runner-old")
	before := computeEvents(t, s, tenant, agent.ID)
	mustBind(t, ctx, s, "stable-session", agent.ID, "runner-new")
	after := computeEvents(t, s, tenant, agent.ID)
	if len(before) != 1 || len(after) != 1 || before[0].Kind != "start" || after[0].IntervalID != before[0].IntervalID {
		t.Fatalf("events before=%+v after=%+v, want one unchanged interval start", before, after)
	}
	if after[0].RunnerID != "runner-old" {
		t.Fatalf("interval start runner = %q, want original runner runner-old", after[0].RunnerID)
	}
}

// A binding an older server wrote mid-deploy has no start event. A later rebind
// or release must still leave one complete interval, with its start estimated.
func TestComputeUsageLegacyBindingGetsEstimatedStart(t *testing.T) {
	for name, release := range map[string]func(*testing.T, context.Context, *Store){
		"single release": func(t *testing.T, ctx context.Context, s *Store) {
			if err := s.DeleteSessionBinding(ctx, "legacy-session"); err != nil {
				t.Fatalf("DeleteSessionBinding: %v", err)
			}
		},
		"runner sweep": func(t *testing.T, ctx context.Context, s *Store) {
			if _, err := s.DeleteSessionBindingsForRunner(ctx, "runner-new"); err != nil {
				t.Fatalf("DeleteSessionBindingsForRunner: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			owner := mustUser(t, s, "compute-legacy-owner")
			agent := mustAgent(t, s, owner.ID, "compute-legacy-agent")
			tenant := s.EffectiveTenant(ctx)
			// The old INSERT omits usage_interval_id, so the column default fills it.
			execAsSystem(t, s, "INSERT INTO session_bindings (tenant_id, agent_account_id, session_id, runner_id) VALUES ($1, $2, 'legacy-session', 'runner-old')",
				string(tenant), string(agent.ID))

			mustBind(t, ctx, s, "legacy-session", agent.ID, "runner-new")
			release(t, ctx, s)

			events := computeEvents(t, s, tenant, agent.ID)
			if len(events) != 2 || events[0].Kind != "start" || events[1].Kind != "end" ||
				events[0].IntervalID != events[1].IntervalID || !events[0].Estimated || events[1].Estimated {
				t.Fatalf("legacy binding events = %+v, want one estimated start and one exact end", events)
			}
		})
	}
}

// A legacy binding released before any new-server rebind still logs its interval.
func TestComputeUsageLegacyBindingReleaseLogsInterval(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "compute-legacy-release-owner")
	agent := mustAgent(t, s, owner.ID, "compute-legacy-release-agent")
	tenant := s.EffectiveTenant(ctx)
	execAsSystem(t, s, "INSERT INTO session_bindings (tenant_id, agent_account_id, session_id, runner_id) VALUES ($1, $2, 'legacy-only', 'runner-old')",
		string(tenant), string(agent.ID))
	if err := s.DeleteSessionBinding(ctx, "legacy-only"); err != nil {
		t.Fatalf("DeleteSessionBinding: %v", err)
	}
	events := computeEvents(t, s, tenant, agent.ID)
	if len(events) != 2 || events[0].Kind != "start" || events[1].Kind != "end" ||
		events[0].IntervalID != events[1].IntervalID || !events[0].Estimated {
		t.Fatalf("legacy release events = %+v, want an estimated start and its end", events)
	}
}

// Re-pointing a legacy binding to a new session ends the legacy interval with an
// estimated start and opens an exact replacement.
func TestComputeUsageLegacyBindingRepointKeepsBothIntervals(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "compute-legacy-repoint-owner")
	agent := mustAgent(t, s, owner.ID, "compute-legacy-repoint-agent")
	tenant := s.EffectiveTenant(ctx)
	execAsSystem(t, s, "INSERT INTO session_bindings (tenant_id, agent_account_id, session_id, runner_id) VALUES ($1, $2, 'legacy-old', 'runner-old')",
		string(tenant), string(agent.ID))

	mustBind(t, ctx, s, "legacy-new", agent.ID, "runner-new")

	byKey := map[string]computeUsageEvent{}
	for _, event := range computeEvents(t, s, tenant, agent.ID) {
		byKey[event.SessionID+"/"+event.Kind] = event
	}
	oldStart, okOldStart := byKey["legacy-old/start"]
	oldEnd, okOldEnd := byKey["legacy-old/end"]
	newStart, okNewStart := byKey["legacy-new/start"]
	if len(byKey) != 3 || !okOldStart || !okOldEnd || !okNewStart ||
		oldStart.IntervalID != oldEnd.IntervalID || !oldStart.Estimated || oldEnd.Estimated || newStart.Estimated {
		t.Fatalf("legacy re-point events = %+v, want estimated legacy start, exact end, exact replacement start", byKey)
	}
}

func TestComputeUsageRunnerSweepClosesIntervalsAndReturnsBindings(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "compute-sweep-owner")
	agentA := mustAgent(t, s, owner.ID, "compute-sweep-a")
	agentB := mustAgent(t, s, owner.ID, "compute-sweep-b")
	tenant := s.EffectiveTenant(ctx)
	mustBind(t, ctx, s, "sweep-session-a", agentA.ID, "sweep-runner")
	mustBind(t, ctx, s, "sweep-session-b", agentB.ID, "sweep-runner")

	swept, err := s.DeleteSessionBindingsForRunner(ctx, "sweep-runner")
	if err != nil {
		t.Fatalf("DeleteSessionBindingsForRunner: %v", err)
	}
	if len(swept) != 2 || swept[0].SessionID != "sweep-session-a" || swept[0].AccountID != agentA.ID ||
		swept[1].SessionID != "sweep-session-b" || swept[1].AccountID != agentB.ID {
		t.Fatalf("swept = %+v, want both runner bindings in session order", swept)
	}
	for _, agent := range []Account{agentA, agentB} {
		events := computeEvents(t, s, tenant, agent.ID)
		if len(events) != 2 {
			t.Fatalf("events for %s after sweep = %+v, want start and end", agent.ID, events)
		}
		if events[0].IntervalID != events[1].IntervalID || events[0].Kind == events[1].Kind || events[0].Estimated || events[1].Estimated {
			t.Fatalf("events for %s = %+v, want matching explicit start/end", agent.ID, events)
		}
	}
}

func TestCloseOrphanedComputeIntervals(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "compute-orphan-owner")
	orphan := mustAgent(t, s, owner.ID, "compute-orphan-agent")
	live := mustAgent(t, s, owner.ID, "compute-live-agent")
	tenant := s.EffectiveTenant(ctx)
	mustBind(t, ctx, s, "orphan-session", orphan.ID, "orphan-runner")
	mustBind(t, ctx, s, "live-session", live.ID, "live-runner")
	if _, err := s.pool.Exec(ctx,
		"DELETE FROM session_bindings WHERE tenant_id = $1 AND agent_account_id = $2",
		string(tenant), string(orphan.ID)); err != nil {
		t.Fatalf("delete orphan binding out of band: %v", err)
	}

	closed, err := s.CloseOrphanedComputeIntervals(WithSystemRole(ctx))
	if err != nil {
		t.Fatalf("CloseOrphanedComputeIntervals: %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want one orphan interval", closed)
	}
	orphanEvents := computeEvents(t, s, tenant, orphan.ID)
	if len(orphanEvents) != 2 || orphanEvents[1].Kind != "end" ||
		orphanEvents[1].IntervalID != orphanEvents[0].IntervalID || !orphanEvents[1].Estimated {
		t.Fatalf("orphan events = %+v, want an estimated end for its start", orphanEvents)
	}
	liveEvents := computeEvents(t, s, tenant, live.ID)
	if len(liveEvents) != 1 || liveEvents[0].Kind != "start" {
		t.Fatalf("live events = %+v, want open start only", liveEvents)
	}
	closed, err = s.CloseOrphanedComputeIntervals(WithSystemRole(ctx))
	if err != nil {
		t.Fatalf("second CloseOrphanedComputeIntervals: %v", err)
	}
	if closed != 0 {
		t.Fatalf("second close count = %d, want 0", closed)
	}
}

func TestComputeUsageEventsAreTenantIsolated(t *testing.T) {
	s := newTestStore(t)
	tenantB := seedTenant(t, s, "compute-tenant-b")
	ctxA := context.Background()
	ctxB := WithTenant(context.Background(), tenantB)
	owner := mustUser(t, s, "compute-isolation-owner")
	agent := mustAgent(t, s, owner.ID, "compute-isolation-agent")
	mustBind(t, ctxA, s, "isolated-session", agent.ID, "isolated-runner")

	var count int64
	err := s.WithTx(ctxB, func(tx pgx.Tx) error {
		return tx.QueryRow(ctxB, "SELECT count(*) FROM compute_usage_events").Scan(&count)
	})
	if err != nil {
		t.Fatalf("read events under tenant B: %v", err)
	}
	if count != 0 {
		t.Fatalf("tenant B sees %d events, want none from tenant A", count)
	}
}

func execAsSystem(t *testing.T, s *Store, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+systemRole); err != nil {
		t.Fatalf("set role: %v", err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
