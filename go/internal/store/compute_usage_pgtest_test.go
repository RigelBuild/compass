//go:build pgtest

package store

import (
	"context"
	"testing"

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
	Estimated      bool
}

func computeEvents(t *testing.T, s *Store, tenant TenantID, accountID AccountID) []computeUsageEvent {
	t.Helper()
	rows, err := s.pool.Query(t.Context(), `
		SELECT tenant_id, id, interval_id, kind, agent_account_id, owner_user_id,
		       session_id, runner_id, estimated
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
			&event.Estimated); err != nil {
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
	if after.RunnerID != "runner-after" || closed.RunnerID != "runner-before" || closed.OwnerUserID != string(owner.ID) {
		t.Fatalf("re-point event metadata = %+v, want runner and owner preserved per interval", events)
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
