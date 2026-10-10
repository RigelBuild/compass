//go:build pgtest

package store

import (
	"sync"
	"testing"
	"time"
)

func TestTourStateClaimIsSingleWinner(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	account := mustUser(t, s, "tour-claim")

	state, err := s.GetTourState(ctx, account.ID)
	if err != nil {
		t.Fatalf("GetTourState before claim: %v", err)
	}
	if state.Outcome != TourOutcomeUnspecified || state.StepID != "" {
		t.Fatalf("unset state = %+v, want unspecified with empty step", state)
	}

	claimed, err := s.ClaimTourStart(ctx, account.ID, "welcome")
	if err != nil {
		t.Fatalf("first ClaimTourStart: %v", err)
	}
	if !claimed {
		t.Fatal("first ClaimTourStart returned false, want true")
	}
	state, err = s.GetTourState(ctx, account.ID)
	if err != nil {
		t.Fatalf("GetTourState after claim: %v", err)
	}
	if state.Outcome != TourOutcomeStarted || state.StepID != "welcome" {
		t.Fatalf("state after claim = %+v, want started at welcome", state)
	}
	claimed, err = s.ClaimTourStart(ctx, account.ID, "welcome")
	if err != nil {
		t.Fatalf("second ClaimTourStart: %v", err)
	}
	if claimed {
		t.Fatal("second ClaimTourStart returned true, want false")
	}
}

func TestTourStateConcurrentClaimsHaveOneWinner(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	account := mustUser(t, s, "tour-concurrent")

	const claimants = 8
	results := make(chan bool, claimants)
	errs := make(chan error, claimants)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range claimants {
		wg.Go(func() {
			<-start
			claimed, err := s.ClaimTourStart(ctx, account.ID, "welcome")
			if err != nil {
				errs <- err
				return
			}
			results <- claimed
		})
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Errorf("ClaimTourStart: %v", err)
	}
	winners := 0
	for claimed := range results {
		if claimed {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent claim winners = %d, want exactly one", winners)
	}
}

func TestTourStateSetRoundTripsAndUpdatesTimestamp(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	account := mustUser(t, s, "tour-set")

	var beforeWrite time.Time
	if err := s.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&beforeWrite); err != nil {
		t.Fatalf("read pre-update time: %v", err)
	}
	if err := s.SetTourState(ctx, account.ID, TourOutcomeDismissed, "step-two"); err != nil {
		t.Fatalf("SetTourState: %v", err)
	}
	before, err := s.GetTourState(ctx, account.ID)
	if err != nil {
		t.Fatalf("GetTourState after first set: %v", err)
	}
	if before.Outcome != TourOutcomeDismissed || before.StepID != "step-two" {
		t.Fatalf("state after first set = %+v, want dismissed at step-two", before)
	}

	var updateTime time.Time
	if err := s.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&updateTime); err != nil {
		t.Fatalf("read pre-update timestamp: %v", err)
	}
	if err := s.SetTourState(ctx, account.ID, TourOutcomeCompleted, ""); err != nil {
		t.Fatalf("SetTourState update: %v", err)
	}
	after, err := s.GetTourState(ctx, account.ID)
	if err != nil {
		t.Fatalf("GetTourState after update: %v", err)
	}
	if after.Outcome != TourOutcomeCompleted || after.StepID != "" {
		t.Fatalf("state after update = %+v, want completed with empty step", after)
	}
	var stepIsNull bool
	if err := s.pool.QueryRow(ctx,
		"SELECT step_id IS NULL FROM account_tour_state WHERE tenant_id = $1 AND account_id = $2",
		string(s.EffectiveTenant(ctx)), string(account.ID),
	).Scan(&stepIsNull); err != nil {
		t.Fatalf("read stored step: %v", err)
	}
	if !stepIsNull {
		t.Fatal("empty step_id was stored as non-NULL, want SQL NULL")
	}
	if !before.UpdatedAt.After(beforeWrite) {
		t.Fatalf("initial updated_at %s did not follow write time %s", before.UpdatedAt, beforeWrite)
	}
	if !after.UpdatedAt.After(updateTime) {
		t.Fatalf("updated_at %s did not advance past update time %s", after.UpdatedAt, updateTime)
	}
}

func TestTourStateIsTenantIsolated(t *testing.T) {
	ctxA := t.Context()
	s := newTestStore(t)
	tenantB := seedTenant(t, s, "tour-tenant-b")
	account := mustUser(t, s, "tour-tenant-a")
	if err := s.SetTourState(ctxA, account.ID, TourOutcomeCompleted, "private-step"); err != nil {
		t.Fatalf("SetTourState(tenant A): %v", err)
	}

	state, err := s.GetTourState(WithTenant(t.Context(), tenantB), account.ID)
	if err != nil {
		t.Fatalf("GetTourState(tenant B): %v", err)
	}
	if state.Outcome != TourOutcomeUnspecified || state.StepID != "" {
		t.Fatalf("tenant B read tenant A state: %+v", state)
	}
}
