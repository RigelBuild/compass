//go:build pgtest

package store

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

// computeRollupTotals is one agent's rollup sums at one granularity.
type computeRollupTotals struct {
	ActiveMs  int64
	Intervals int64
}

// readComputeRollups sums one agent's hourly and daily rollup rows as owner.
func readComputeRollups(t *testing.T, s *Store, tenant TenantID, accountID AccountID) (hourly, daily computeRollupTotals) {
	t.Helper()
	for _, row := range []struct {
		table string
		dst   *computeRollupTotals
	}{
		{"compute_usage_rollups_hourly", &hourly},
		{"compute_usage_rollups_daily", &daily},
	} {
		if err := s.pool.QueryRow(t.Context(),
			"SELECT coalesce(sum(active_ms), 0)::bigint, coalesce(sum(intervals), 0)::bigint FROM "+row.table+
				" WHERE tenant_id = $1 AND agent_account_id = $2",
			string(tenant), string(accountID)).Scan(&row.dst.ActiveMs, &row.dst.Intervals); err != nil {
			t.Fatalf("read %s: %v", row.table, err)
		}
	}
	return hourly, daily
}

// wantRollupsMatchEvents checks the rollups hold every closed interval's full
// duration exactly once, at both granularities.
func wantRollupsMatchEvents(t *testing.T, s *Store, tenant TenantID, accountID AccountID, wantClosed int64) {
	t.Helper()
	starts := map[string]computeUsageEvent{}
	var want computeRollupTotals
	events := computeEvents(t, s, tenant, accountID)
	for _, event := range events {
		if event.Kind == "start" {
			starts[event.IntervalID] = event
		}
	}
	for _, event := range events {
		start, ok := starts[event.IntervalID]
		if event.Kind != "end" || !ok {
			continue
		}
		want.ActiveMs += event.OccurredAt.UnixMilli() - start.OccurredAt.UnixMilli()
		want.Intervals++
	}
	if want.Intervals != wantClosed {
		t.Fatalf("closed intervals = %d in %+v, want %d", want.Intervals, events, wantClosed)
	}
	hourly, daily := readComputeRollups(t, s, tenant, accountID)
	if hourly != want || daily != want {
		t.Fatalf("rollups hourly %+v daily %+v, want both %+v from events %+v", hourly, daily, want, events)
	}
}

// advanceComputeHorizonPastOpenIntervals moves the prune horizon past every
// open start, as a prune that ran while they were open would.
func advanceComputeHorizonPastOpenIntervals(t *testing.T, s *Store) {
	t.Helper()
	execAsSystem(t, s, "UPDATE compute_usage_prune_horizon SET horizon = date_trunc('day', now(), 'UTC') + interval '1 day'")
}

func TestComputeUsageRollupsFromRepoint(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner := mustUser(t, s, "rollup-repoint-owner")
	agent := mustAgent(t, s, owner.ID, "rollup-repoint-agent")
	tenant := s.EffectiveTenant(ctx)
	mustBind(t, ctx, s, "rollup-before", agent.ID, "runner-before")
	advanceComputeHorizonPastOpenIntervals(t, s)

	mustBind(t, ctx, s, "rollup-after", agent.ID, "runner-after")
	wantRollupsMatchEvents(t, s, tenant, agent.ID, 1)
}

func TestComputeUsageRollupsFromDeleteSessionBinding(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner := mustUser(t, s, "rollup-delete-owner")
	agent := mustAgent(t, s, owner.ID, "rollup-delete-agent")
	tenant := s.EffectiveTenant(ctx)
	mustBind(t, ctx, s, "rollup-delete", agent.ID, "rollup-runner")
	advanceComputeHorizonPastOpenIntervals(t, s)

	if err := s.DeleteSessionBinding(ctx, "rollup-delete"); err != nil {
		t.Fatalf("DeleteSessionBinding: %v", err)
	}
	wantRollupsMatchEvents(t, s, tenant, agent.ID, 1)
	if err := s.DeleteSessionBinding(ctx, "rollup-delete"); err != nil {
		t.Fatalf("second DeleteSessionBinding: %v", err)
	}
	wantRollupsMatchEvents(t, s, tenant, agent.ID, 1)
}

func TestComputeUsageRollupsFromRunnerSweep(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner := mustUser(t, s, "rollup-sweep-owner")
	agentA := mustAgent(t, s, owner.ID, "rollup-sweep-a")
	agentB := mustAgent(t, s, owner.ID, "rollup-sweep-b")
	tenant := s.EffectiveTenant(ctx)
	mustBind(t, ctx, s, "rollup-sweep-a", agentA.ID, "rollup-sweep-runner")
	mustBind(t, ctx, s, "rollup-sweep-b", agentB.ID, "rollup-sweep-runner")
	advanceComputeHorizonPastOpenIntervals(t, s)

	if _, err := s.DeleteSessionBindingsForRunner(ctx, "rollup-sweep-runner"); err != nil {
		t.Fatalf("DeleteSessionBindingsForRunner: %v", err)
	}
	wantRollupsMatchEvents(t, s, tenant, agentA.ID, 1)
	wantRollupsMatchEvents(t, s, tenant, agentB.ID, 1)
}

func TestComputeUsageRollupsFromOrphanClose(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner := mustUser(t, s, "rollup-orphan-owner")
	orphan := mustAgent(t, s, owner.ID, "rollup-orphan-agent")
	live := mustAgent(t, s, owner.ID, "rollup-live-agent")
	tenant := s.EffectiveTenant(ctx)
	mustBind(t, ctx, s, "rollup-orphan", orphan.ID, "rollup-orphan-runner")
	mustBind(t, ctx, s, "rollup-live", live.ID, "rollup-live-runner")
	execAsSystem(t, s, "DELETE FROM session_bindings WHERE tenant_id = $1 AND agent_account_id = $2",
		string(tenant), string(orphan.ID))
	advanceComputeHorizonPastOpenIntervals(t, s)

	if _, err := s.CloseOrphanedComputeIntervals(WithSystemRole(ctx)); err != nil {
		t.Fatalf("CloseOrphanedComputeIntervals: %v", err)
	}
	wantRollupsMatchEvents(t, s, tenant, orphan.ID, 1)
	wantRollupsMatchEvents(t, s, tenant, live.ID, 0)
}

// The request role appends to the raw log but must never delete from it; only
// the system-role prune may.
func TestComputeUsageAppRoleCannotDeleteEvents(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	owner := mustUser(t, s, "rollup-grant-owner")
	agent := mustAgent(t, s, owner.ID, "rollup-grant-agent")
	tenant := s.EffectiveTenant(ctx)
	mustBind(t, ctx, s, "rollup-grant", agent.ID, "rollup-grant-runner")

	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "DELETE FROM compute_usage_events")
		return err
	})
	if !pgErrIs(err, "42501") {
		t.Fatalf("DELETE as %s error = %v, want insufficient_privilege", appRole, err)
	}
	if got := computeEvents(t, s, tenant, agent.ID); len(got) != 1 {
		t.Fatalf("events after refused DELETE = %+v, want the start kept", got)
	}
}
