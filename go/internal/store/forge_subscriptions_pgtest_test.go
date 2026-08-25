//go:build pgtest

package store

// DL-053 agent-forge-subscription store contracts (RIG-2732 Piece 1): the
// idempotent per-agent subscribe on the UNIQUE (agent, coordinate), the
// id+agent-scoped delete, and the load-bearing GC invariant — the
// forge_artifact_cursors row is collected exactly when the LAST subscription for
// its coordinate is deleted, and survives while any other agent still
// subscribes. context.Background is the test root (the pgtest-suite convention,
// sibling forge_authored_pgtest_test.go).

import (
	"context"
	"testing"
)

// artifactCursorExists reports whether a forge_artifact_cursors row is present
// at the coordinate — the GC assertion surface (no public reader for the
// poll-driver's cursor table in this slice).
func artifactCursorExists(t *testing.T, s *Store, provider ForgeProvider, host, repo string, kind ForgeArtifactKind, number uint64) bool {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM forge_artifact_cursors
		  WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3 AND kind = $4 AND number = $5`,
		int32(provider), host, repo, int32(kind), int64(number),
	).Scan(&n); err != nil {
		t.Fatalf("count artifact cursor rows: %v", err)
	}
	return n > 0
}

// seedArtifactCursor inserts a bare forge_artifact_cursors row at the coordinate
// (the poll driver's WRITER is a later slice, so the GC test seeds directly).
func seedArtifactCursor(t *testing.T, s *Store, provider ForgeProvider, host, repo string, kind ForgeArtifactKind, number uint64) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO forge_artifact_cursors (forge_provider, forge_host, repo, kind, number)
		 VALUES ($1, $2, $3, $4, $5)`,
		int32(provider), host, repo, int32(kind), int64(number),
	); err != nil {
		t.Fatalf("seed artifact cursor: %v", err)
	}
}

// ── Test 1: idempotent subscribe on the UNIQUE (agent, coordinate) ────────────

func TestAgentForgeSubscriptionIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, _ := seedAgent(t, s, "sub1")

	base := AgentForgeSubscription{
		AgentAccountID: agent,
		Provider:       ForgeProviderGitHub,
		Host:           "github.com",
		Repo:           "a/b",
		Kind:           ForgeArtifactKindIssue,
		Number:         42,
	}
	first, err := s.EnsureAgentForgeSubscription(ctx, base)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if first == "" {
		t.Fatalf("first ensure returned empty id")
	}

	second, err := s.EnsureAgentForgeSubscription(ctx, base)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if second != first {
		t.Fatalf("repeat subscribe id = %q, want %q (idempotent)", second, first)
	}

	n, err := s.AgentForgeSubscriptionsForArtifact(ctx, base.Provider, base.Host, base.Repo, base.Kind, base.Number)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("row count = %d, want 1 (no duplicate)", n)
	}
}

// ── Test 2: two agents on the same coordinate → two distinct rows ─────────────

func TestAgentForgeSubscriptionDistinctPerAgent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agentA, _ := seedAgent(t, s, "sub2a")
	agentB, _ := seedAgent(t, s, "sub2b")

	coord := AgentForgeSubscription{
		Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b",
		Kind: ForgeArtifactKindIssue, Number: 7,
	}
	a := coord
	a.AgentAccountID = agentA
	b := coord
	b.AgentAccountID = agentB

	idA, err := s.EnsureAgentForgeSubscription(ctx, a)
	if err != nil {
		t.Fatalf("ensure A: %v", err)
	}
	idB, err := s.EnsureAgentForgeSubscription(ctx, b)
	if err != nil {
		t.Fatalf("ensure B: %v", err)
	}
	if idA == idB {
		t.Fatalf("two agents share id %q, want distinct", idA)
	}
	n, err := s.AgentForgeSubscriptionsForArtifact(ctx, coord.Provider, coord.Host, coord.Repo, coord.Kind, coord.Number)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("row count = %d, want 2", n)
	}
}

// ── Test 3: delete is id+agent-scoped ─────────────────────────────────────────

func TestAgentForgeSubscriptionDeleteScoping(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agentA, _ := seedAgent(t, s, "sub3a")
	agentB, _ := seedAgent(t, s, "sub3b")

	id, err := s.EnsureAgentForgeSubscription(ctx, AgentForgeSubscription{
		AgentAccountID: agentA, Provider: ForgeProviderGitHub, Host: "github.com",
		Repo: "a/b", Kind: ForgeArtifactKindIssue, Number: 1,
	})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// Deleting an unknown id -> ErrNotFound.
	sentinelIs(t, s.DeleteAgentForgeSubscription(ctx, agentA, "no-such-id"), ErrNotFound, "delete unknown id")
	// Deleting another agent's id -> ErrNotFound (scoping), row survives.
	sentinelIs(t, s.DeleteAgentForgeSubscription(ctx, agentB, id), ErrNotFound, "delete foreign id")
	if n, _ := s.AgentForgeSubscriptionsForArtifact(ctx, ForgeProviderGitHub, "github.com", "a/b", ForgeArtifactKindIssue, 1); n != 1 {
		t.Fatalf("row count after foreign delete = %d, want 1 (untouched)", n)
	}
	// Owner deletes -> gone.
	if err := s.DeleteAgentForgeSubscription(ctx, agentA, id); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	if n, _ := s.AgentForgeSubscriptionsForArtifact(ctx, ForgeProviderGitHub, "github.com", "a/b", ForgeArtifactKindIssue, 1); n != 0 {
		t.Fatalf("row count after owner delete = %d, want 0", n)
	}
}

// ── Test 4: DL-053 GC — cursor collected on LAST unsubscribe only ─────────────

func TestAgentForgeSubscriptionCursorGC(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agentA, _ := seedAgent(t, s, "gc-a")
	agentB, _ := seedAgent(t, s, "gc-b")

	const (
		host   = "github.com"
		repo   = "a/b"
		number = uint64(99)
	)
	provider := ForgeProviderGitHub
	kind := ForgeArtifactKindPullRequest

	seedArtifactCursor(t, s, provider, host, repo, kind, number)

	idA, err := s.EnsureAgentForgeSubscription(ctx, AgentForgeSubscription{
		AgentAccountID: agentA, Provider: provider, Host: host, Repo: repo, Kind: kind, Number: number,
	})
	if err != nil {
		t.Fatalf("ensure A: %v", err)
	}
	idB, err := s.EnsureAgentForgeSubscription(ctx, AgentForgeSubscription{
		AgentAccountID: agentB, Provider: provider, Host: host, Repo: repo, Kind: kind, Number: number,
	})
	if err != nil {
		t.Fatalf("ensure B: %v", err)
	}

	// Delete A: B still subscribes -> cursor STAYS.
	if err := s.DeleteAgentForgeSubscription(ctx, agentA, idA); err != nil {
		t.Fatalf("delete A: %v", err)
	}
	if !artifactCursorExists(t, s, provider, host, repo, kind, number) {
		t.Fatalf("cursor collected after A left, but B still subscribes")
	}

	// Delete B: last subscription gone -> cursor COLLECTED.
	if err := s.DeleteAgentForgeSubscription(ctx, agentB, idB); err != nil {
		t.Fatalf("delete B: %v", err)
	}
	if artifactCursorExists(t, s, provider, host, repo, kind, number) {
		t.Fatalf("cursor survived after last subscription deleted (DL-053 GC failed)")
	}
}

// ── Test 5: coordinate validation ─────────────────────────────────────────────

func TestAgentForgeSubscriptionValidation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, _ := seedAgent(t, s, "sub5")

	good := AgentForgeSubscription{
		AgentAccountID: agent, Provider: ForgeProviderGitHub, Host: "github.com",
		Repo: "a/b", Kind: ForgeArtifactKindIssue, Number: 1,
	}
	cases := []struct {
		name  string
		mutfn func(*AgentForgeSubscription)
	}{
		{"zero provider", func(a *AgentForgeSubscription) { a.Provider = ForgeProviderUnspecified }},
		{"empty host", func(a *AgentForgeSubscription) { a.Host = "" }},
		{"empty repo", func(a *AgentForgeSubscription) { a.Repo = "" }},
		{"zero kind", func(a *AgentForgeSubscription) { a.Kind = ForgeArtifactKindUnspecified }},
		{"zero number", func(a *AgentForgeSubscription) { a.Number = 0 }},
		{"empty agent", func(a *AgentForgeSubscription) { a.AgentAccountID = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := good
			tc.mutfn(&bad)
			_, err := s.EnsureAgentForgeSubscription(ctx, bad)
			sentinelIs(t, err, ErrInvalidArgument, tc.name)
		})
	}
}

// TestAgentForgeSubscriptionUnknownAgentIsInvalidArgument covers the FK-violation
// branch of EnsureAgentForgeSubscription: a well-formed coordinate naming an
// agent that does not exist trips the agent_account_id FK RESTRICT, which the
// writer must classify as ErrInvalidArgument (rendered in-band as invalid_argument
// via storeForgeError) — NOT a raw pg error / CodeInternal teardown. The sibling
// TestAgentForgeSubscriptionFKRestrict asserts the DB constraint fires on a raw
// INSERT; this asserts the METHOD's error-classification branch, which that test
// bypasses.
func TestAgentForgeSubscriptionUnknownAgentIsInvalidArgument(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	_, err := s.EnsureAgentForgeSubscription(ctx, AgentForgeSubscription{
		AgentAccountID: AccountID("no-such-agent"),
		Provider:       ForgeProviderGitHub,
		Host:           "github.com",
		Repo:           "a/b",
		Kind:           ForgeArtifactKindIssue,
		Number:         1,
	})
	sentinelIs(t, err, ErrInvalidArgument, "unknown agent (FK violation)")
}
