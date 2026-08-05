//go:build pgtest

package store

import (
	"context"
	"testing"
)

// Agent-activity store contracts (Record C, T2): the durable activity string is
// last-write-wins, absent agents report empty (absent from the map), an empty
// query is a no-op, and — the load-bearing DL-074 divergence — the activity
// survives a store restart because it is read from Postgres, not process memory.

func TestSetActivityRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "a")

	if err := s.SetActivity(ctx, a.ID, "reviewing PR", 1000); err != nil {
		t.Fatalf("SetActivity: %v", err)
	}

	got, err := s.ActivityFor(ctx, []AccountID{a.ID})
	if err != nil {
		t.Fatalf("ActivityFor: %v", err)
	}
	act, ok := got[a.ID]
	if !ok {
		t.Fatalf("ActivityFor missing agent %q", a.ID)
	}
	if act.Activity != "reviewing PR" || act.ActivityAtUnixMs != 1000 {
		t.Fatalf("activity = %+v, want {reviewing PR 1000}", act)
	}
}

func TestSetActivityUpsertOverwrites(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "a")

	if err := s.SetActivity(ctx, a.ID, "first", 1000); err != nil {
		t.Fatalf("SetActivity first: %v", err)
	}
	if err := s.SetActivity(ctx, a.ID, "second", 2000); err != nil {
		t.Fatalf("SetActivity second: %v", err)
	}

	got, err := s.ActivityFor(ctx, []AccountID{a.ID})
	if err != nil {
		t.Fatalf("ActivityFor: %v", err)
	}
	if act := got[a.ID]; act.Activity != "second" || act.ActivityAtUnixMs != 2000 {
		t.Fatalf("activity = %+v, want {second 2000} (last-write-wins)", act)
	}
}

func TestActivityForAbsentAgentIsAbsentFromMap(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	known := mustAgent(t, s, owner.ID, "known")
	unknown := mustAgent(t, s, owner.ID, "unknown")

	if err := s.SetActivity(ctx, known.ID, "busy", 1000); err != nil {
		t.Fatalf("SetActivity: %v", err)
	}

	got, err := s.ActivityFor(ctx, []AccountID{known.ID, unknown.ID})
	if err != nil {
		t.Fatalf("ActivityFor: %v", err)
	}
	if _, ok := got[unknown.ID]; ok {
		t.Fatalf("unknown agent %q present in map, want absent", unknown.ID)
	}
	if _, ok := got[known.ID]; !ok {
		t.Fatalf("known agent %q missing from map", known.ID)
	}
}

func TestActivityForEmptyInput(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	got, err := s.ActivityFor(ctx, nil)
	if err != nil {
		t.Fatalf("ActivityFor(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ActivityFor(nil) = %v, want empty map", got)
	}
}

// TestActivitySurvivesRestart is the DL-074 load-bearing test: the activity is
// written through one store, a SECOND store is opened against the SAME dsn, and
// the activity is still there — proving it is read from Postgres, not the first
// store's process memory (design.md T2:449-452).
func TestActivitySurvivesRestart(t *testing.T) {
	ctx := context.Background()
	s, dsn := newTestStoreDSN(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "a")

	if err := s.SetActivity(ctx, a.ID, "durable", 4242); err != nil {
		t.Fatalf("SetActivity: %v", err)
	}

	reopened := reopenStore(t, dsn)
	got, err := reopened.ActivityFor(ctx, []AccountID{a.ID})
	if err != nil {
		t.Fatalf("ActivityFor after reopen: %v", err)
	}
	act, ok := got[a.ID]
	if !ok {
		t.Fatalf("activity for %q missing after restart", a.ID)
	}
	if act.Activity != "durable" || act.ActivityAtUnixMs != 4242 {
		t.Fatalf("activity after restart = %+v, want {durable 4242}", act)
	}
}
