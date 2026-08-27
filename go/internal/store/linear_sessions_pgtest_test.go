//go:build pgtest

package store

// Linear Agent Session association + by-coordinate ownership read store
// contracts (compass-linear-agent-responder §T3): the linear_agent_sessions
// table shape, the idempotent upsert (created=true first, false on replay), the
// PK lookup (hit + ErrNotFound miss), and AuthoredArtifactByCoordinate
// (seeded-row hit + ErrNotFound miss). context.Background is the test root
// (the pgtest-suite convention, sibling forge_authored_pgtest_test.go).

import (
	"context"
	"testing"
)

// ── Upsert + lookup round-trip; replay returns created=false ──────────────────

func TestUpsertLinearAgentSession(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	row := LinearAgentSessionRow{
		LinearSessionID:  "sess-abc",
		ManagerAccountID: "mgr-1",
		ChannelID:        "chan-1",
		TopicID:          "topic-1",
		LinearIssueID:    "issue-1",
	}
	created, err := s.UpsertLinearAgentSession(ctx, row)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if !created {
		t.Fatalf("first upsert created = false, want true")
	}

	// Replay the SAME session id (ON CONFLICT DO NOTHING): no error, created=false.
	replay := row
	replay.ManagerAccountID = "mgr-CHANGED" // DO NOTHING must not rewrite it
	created, err = s.UpsertLinearAgentSession(ctx, replay)
	if err != nil {
		t.Fatalf("replay upsert: %v", err)
	}
	if created {
		t.Fatalf("replay upsert created = true, want false")
	}

	// The original row survives the replay unchanged.
	got, err := s.LinearAgentSession(ctx, "sess-abc")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.LinearSessionID != row.LinearSessionID ||
		got.ManagerAccountID != row.ManagerAccountID ||
		got.ChannelID != row.ChannelID ||
		got.TopicID != row.TopicID ||
		got.LinearIssueID != row.LinearIssueID {
		t.Fatalf("read-back = %+v, want %+v (replay must not clobber)", got, row)
	}
	if got.CreatedAt.IsZero() {
		t.Fatalf("created_at is zero, want the DEFAULT now() birth time")
	}
}

// ── Lookup miss → ErrNotFound; empty id → ErrInvalidArgument ───────────────────

func TestLinearAgentSession(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	_, err := s.LinearAgentSession(ctx, "no-such-session")
	sentinelIs(t, err, ErrNotFound, "lookup unknown session")

	_, err = s.LinearAgentSession(ctx, "")
	sentinelIs(t, err, ErrInvalidArgument, "lookup empty session id")

	// Empty LinearIssueID stores as SQL NULL and reads back as "".
	if _, err := s.UpsertLinearAgentSession(ctx, LinearAgentSessionRow{
		LinearSessionID:  "sess-noissue",
		ManagerAccountID: "mgr-1",
		ChannelID:        "chan-1",
		TopicID:          "topic-1",
	}); err != nil {
		t.Fatalf("upsert no-issue: %v", err)
	}
	got, err := s.LinearAgentSession(ctx, "sess-noissue")
	if err != nil {
		t.Fatalf("lookup no-issue: %v", err)
	}
	if got.LinearIssueID != "" {
		t.Fatalf("linear_issue_id = %q, want \"\" (NULL → empty)", got.LinearIssueID)
	}

	// Empty session id on the write path is a caller bug too.
	if _, err := s.UpsertLinearAgentSession(ctx, LinearAgentSessionRow{}); err == nil {
		t.Fatalf("upsert empty session id: want ErrInvalidArgument, got nil")
	} else {
		sentinelIs(t, err, ErrInvalidArgument, "upsert empty session id")
	}
}

// ── AuthoredArtifactByCoordinate: seeded-row hit + ErrNotFound miss ────────────

func TestAuthoredArtifactByCoordinate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "coord")

	want := AuthoredArtifact{
		Provider:        ForgeProviderGitHub,
		Host:            "github.com",
		Repo:            "a/b",
		Kind:            ForgeArtifactKindIssue,
		Number:          7,
		AgentAccountID:  agent,
		OwnerUserID:     owner,
		SessionID:       "sess-1",
		ClientRequestID: "req-1",
		CreatedAtUnixMS: 1000,
	}
	if err := s.RecordAuthoredArtifact(ctx, want); err != nil {
		t.Fatalf("record: %v", err)
	}

	got, err := s.AuthoredArtifactByCoordinate(ctx, want.Provider, want.Host, want.Repo, want.Kind, want.Number)
	if err != nil {
		t.Fatalf("by-coordinate hit: %v", err)
	}
	if got != want {
		t.Fatalf("by-coordinate read = %+v, want %+v", got, want)
	}

	// Unknown coordinate (same repo, different number) → ErrNotFound.
	_, err = s.AuthoredArtifactByCoordinate(ctx, want.Provider, want.Host, want.Repo, want.Kind, 999)
	sentinelIs(t, err, ErrNotFound, "by-coordinate unknown number")

	// A wrong kind at the same number is a distinct coordinate → miss.
	_, err = s.AuthoredArtifactByCoordinate(ctx, want.Provider, want.Host, want.Repo, ForgeArtifactKindPullRequest, want.Number)
	sentinelIs(t, err, ErrNotFound, "by-coordinate wrong kind")

	// Invalid coordinate fields → ErrInvalidArgument.
	_, err = s.AuthoredArtifactByCoordinate(ctx, ForgeProviderUnspecified, want.Host, want.Repo, want.Kind, want.Number)
	sentinelIs(t, err, ErrInvalidArgument, "by-coordinate zero provider")
	_, err = s.AuthoredArtifactByCoordinate(ctx, want.Provider, want.Host, want.Repo, ForgeArtifactKindUnspecified, want.Number)
	sentinelIs(t, err, ErrInvalidArgument, "by-coordinate zero kind")
}
