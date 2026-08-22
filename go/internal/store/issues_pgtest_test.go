//go:build pgtest

package store

import (
	"context"
	"errors"
	"testing"
)

// Issues store contracts (SEA-1728 part 3a): the forge coordinate is the
// idempotency key so a re-poll keeps a stable id and never clobbers a human-set
// lifecycle state, a persisted issue always has a real state (DEFAULT BACKLOG),
// unknown ids are ErrNotFound, and the issue survives a store restart (DL-019
// durability). Deterministic, no sleeps.

// forgeFields is a valid IssueForgeFields for a fresh coordinate, with the
// caller overriding whatever the case exercises.
func forgeFields(number uint32) IssueForgeFields {
	return IssueForgeFields{
		ForgeProvider: ForgeProviderGitHub,
		ForgeHost:     "github.com",
		Repo:          "RigelBuild/compass",
		Number:        number,
		Title:         "a bug",
		Body:          "it broke",
		ForgeState:    "open",
		URL:           "https://github.com/RigelBuild/compass/issues/1",
		ForgeAccount:  "octocat",
		Labels:        []string{"bug"},
		AgentHandle:   "",
	}
}

func TestUpsertInsertsAndReturnsStableID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	in := forgeFields(1)
	id, err := s.UpsertIssueForgeFields(ctx, in)
	if err != nil {
		t.Fatalf("UpsertIssueForgeFields: %v", err)
	}
	if id == "" {
		t.Fatal("UpsertIssueForgeFields returned empty id")
	}

	got, err := s.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if got.ID != id {
		t.Fatalf("id = %q, want %q", got.ID, id)
	}
	if got.ForgeProvider != in.ForgeProvider || got.ForgeHost != in.ForgeHost ||
		got.Repo != in.Repo || got.Number != in.Number {
		t.Fatalf("coordinate = %+v, want %+v", got, in)
	}
	if got.Title != in.Title || got.Body != in.Body || got.ForgeState != in.ForgeState ||
		got.URL != in.URL || got.ForgeAccount != in.ForgeAccount || got.AgentHandle != in.AgentHandle {
		t.Fatalf("forge fields = %+v, want %+v", got, in)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "bug" {
		t.Fatalf("labels = %v, want [bug]", got.Labels)
	}
	// The DEFAULT: a freshly ingested issue enters the board in Backlog.
	if got.State != IssueStateBacklog {
		t.Fatalf("state = %d, want Backlog(%d)", got.State, IssueStateBacklog)
	}
	// Machinery is empty until its own producer writes it.
	if got.Priority != "" || got.Assignee != "" || got.Summary != "" || got.Branch != "" {
		t.Fatalf("machinery not empty: %+v", got)
	}
}

func TestUpsertRepollIsIdempotentOnID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	first, err := s.UpsertIssueForgeFields(ctx, forgeFields(1))
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	updated := forgeFields(1)
	updated.Title = "renamed"
	updated.ForgeState = "closed"
	updated.Labels = []string{"bug", "p1"}
	second, err := s.UpsertIssueForgeFields(ctx, updated)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if second != first {
		t.Fatalf("re-poll minted a new id %q, want stable %q", second, first)
	}

	got, err := s.GetIssue(ctx, first)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if got.Title != "renamed" || got.ForgeState != "closed" {
		t.Fatalf("forge fields not updated: %+v", got)
	}
	if len(got.Labels) != 2 || got.Labels[0] != "bug" || got.Labels[1] != "p1" {
		t.Fatalf("labels = %v, want [bug p1]", got.Labels)
	}
}

func TestUpsertRepollDoesNotClobberState(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	id, err := s.UpsertIssueForgeFields(ctx, forgeFields(1))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.SetIssueState(ctx, id, IssueStateInProgress); err != nil {
		t.Fatalf("SetIssueState: %v", err)
	}

	updated := forgeFields(1)
	updated.Title = "renamed"
	if _, err := s.UpsertIssueForgeFields(ctx, updated); err != nil {
		t.Fatalf("re-poll upsert: %v", err)
	}

	got, err := s.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	// The load-bearing property: a re-poll updates the forge fields but leaves
	// the human-set lifecycle state untouched.
	if got.State != IssueStateInProgress {
		t.Fatalf("state = %d, want InProgress(%d) — re-poll clobbered it", got.State, IssueStateInProgress)
	}
	if got.Title != "renamed" {
		t.Fatalf("title = %q, want renamed", got.Title)
	}
}

func TestUpsertDistinctCoordinatesAreDistinctRows(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Differs only in number.
	id1, err := s.UpsertIssueForgeFields(ctx, forgeFields(1))
	if err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	id2, err := s.UpsertIssueForgeFields(ctx, forgeFields(2))
	if err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	// Differs only in forge_host.
	otherHost := forgeFields(1)
	otherHost.ForgeHost = "gitlab.example.com"
	id3, err := s.UpsertIssueForgeFields(ctx, otherHost)
	if err != nil {
		t.Fatalf("upsert 3: %v", err)
	}

	if id1 == id2 || id1 == id3 || id2 == id3 {
		t.Fatalf("distinct coordinates collided: %q %q %q", id1, id2, id3)
	}

	list, err := s.ListIssues(ctx)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("ListIssues len = %d, want 3", len(list))
	}
}

func TestSetIssueStateRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	id, err := s.UpsertIssueForgeFields(ctx, forgeFields(1))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.SetIssueState(ctx, id, IssueStateDone); err != nil {
		t.Fatalf("SetIssueState: %v", err)
	}
	got, err := s.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if got.State != IssueStateDone {
		t.Fatalf("state = %d, want Done(%d)", got.State, IssueStateDone)
	}
}

func TestSetIssueStateUnknownID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	err := s.SetIssueState(ctx, "nope", IssueStateTodo)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetIssueState unknown id err = %v, want ErrNotFound", err)
	}
}

func TestGetIssueUnknownID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	_, err := s.GetIssue(ctx, "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetIssue unknown id err = %v, want ErrNotFound", err)
	}
}

func TestUpsertEmptyRepoInvalid(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	_, err := s.UpsertIssueForgeFields(ctx, IssueForgeFields{})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty repo err = %v, want ErrInvalidArgument", err)
	}
}

func TestUpsertEmptyForgeHostInvalid(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// A non-empty Repo clears the first guard, so this reaches and trips the
	// ForgeHost guard specifically (the empty-Repo case above short-circuits
	// before ForgeHost is ever evaluated).
	in := forgeFields(1)
	in.ForgeHost = ""
	_, err := s.UpsertIssueForgeFields(ctx, in)
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty forge host err = %v, want ErrInvalidArgument", err)
	}
}

func TestListIssuesEmptyAndOrdered(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	empty, err := s.ListIssues(ctx)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if empty == nil {
		t.Fatal("ListIssues returned nil, want non-nil empty slice")
	}
	if len(empty) != 0 {
		t.Fatalf("ListIssues len = %d, want 0", len(empty))
	}

	for n := uint32(1); n <= 3; n++ {
		if _, err := s.UpsertIssueForgeFields(ctx, forgeFields(n)); err != nil {
			t.Fatalf("upsert %d: %v", n, err)
		}
	}

	list, err := s.ListIssues(ctx)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("ListIssues len = %d, want 3", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].ID >= list[i].ID {
			t.Fatalf("ListIssues not ordered by id: %q >= %q", list[i-1].ID, list[i].ID)
		}
	}
}

// TestIssueSurvivesRestart is the DL-019 load-bearing test: an issue and its
// state are written through one store, a SECOND store is opened against the
// SAME dsn, and both are still there — proving they are read from Postgres, not
// the first store's process memory (mirror TestActivitySurvivesRestart).
func TestIssueSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	s, dsn := newTestStoreDSN(t)

	id, err := s.UpsertIssueForgeFields(ctx, forgeFields(1))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.SetIssueState(ctx, id, IssueStateInReview); err != nil {
		t.Fatalf("SetIssueState: %v", err)
	}

	reopened := reopenStore(t, dsn)
	got, err := reopened.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("GetIssue after reopen: %v", err)
	}
	if got.ID != id {
		t.Fatalf("id after restart = %q, want %q", got.ID, id)
	}
	if got.State != IssueStateInReview {
		t.Fatalf("state after restart = %d, want InReview(%d)", got.State, IssueStateInReview)
	}
}

func TestLabelsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	withLabels := forgeFields(1)
	withLabels.Labels = []string{"bug", "p1"}
	id, err := s.UpsertIssueForgeFields(ctx, withLabels)
	if err != nil {
		t.Fatalf("upsert with labels: %v", err)
	}
	got, err := s.GetIssue(ctx, id)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if len(got.Labels) != 2 || got.Labels[0] != "bug" || got.Labels[1] != "p1" {
		t.Fatalf("labels = %v, want [bug p1]", got.Labels)
	}

	// Empty labels round-trip as nil, not []string{}.
	noLabels := forgeFields(2)
	noLabels.Labels = nil
	id2, err := s.UpsertIssueForgeFields(ctx, noLabels)
	if err != nil {
		t.Fatalf("upsert empty labels: %v", err)
	}
	got2, err := s.GetIssue(ctx, id2)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if got2.Labels != nil {
		t.Fatalf("empty labels = %v, want nil", got2.Labels)
	}
}
