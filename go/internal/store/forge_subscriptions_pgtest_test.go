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
	"encoding/json"
	"errors"
	"reflect"
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

// jsonSemanticEqual reports whether two JSON byte slices are semantically equal
// (same parsed value), ignoring whitespace and key order. The forge_artifact_cursors
// snapshot is a JSONB column, so a written value reads back Postgres-normalized —
// a byte compare would be Postgres-version-fragile, a value compare is not.
func jsonSemanticEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshal snapshot a %q: %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshal snapshot b %q: %v", b, err)
	}
	return reflect.DeepEqual(av, bv)
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

// ── T3: container-scope subscriptions ─────────────────────────────────────────

// containerSubCount counts subscription rows at a container coordinate
// (scope=2, number=0) for a given project — the T3 assertion surface.
func containerSubCount(t *testing.T, s *Store, provider ForgeProvider, host, repo string, kind ForgeArtifactKind, project string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_forge_subscriptions
		  WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3 AND kind = $4
		    AND scope = 2 AND number = 0 AND project = $5`,
		int32(provider), host, repo, int32(kind), project,
	).Scan(&n); err != nil {
		t.Fatalf("count container subs: %v", err)
	}
	return n
}

// TestAgentForgeContainerSubscriptionIdempotent: a GitHub CONTAINER subscribe is
// idempotent on (agent, repo, kind, 0, project=”) — a repeat returns the same
// id and adds no row.
func TestAgentForgeContainerSubscriptionIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, _ := seedAgent(t, s, "t3-ctr-idem")

	base := AgentForgeSubscription{
		AgentAccountID: agent,
		Provider:       ForgeProviderGitHub,
		Host:           "github.com",
		Repo:           "a/b",
		Kind:           ForgeArtifactKindIssue,
		Scope:          ForgeSubscriptionScopeContainer,
	}
	first, err := s.EnsureAgentForgeSubscription(ctx, base)
	if err != nil {
		t.Fatalf("first container ensure: %v", err)
	}
	second, err := s.EnsureAgentForgeSubscription(ctx, base)
	if err != nil {
		t.Fatalf("second container ensure: %v", err)
	}
	if second != first {
		t.Fatalf("repeat container subscribe id = %q, want %q (idempotent)", second, first)
	}
	if n := containerSubCount(t, s, base.Provider, base.Host, base.Repo, base.Kind, ""); n != 1 {
		t.Fatalf("container row count = %d, want 1 (no duplicate)", n)
	}
}

// TestAgentForgeSubscriptionScopeValidation: the scope-shape guard rejects each
// mismatched (scope, number, project) combination with ErrInvalidArgument.
func TestAgentForgeSubscriptionScopeValidation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, _ := seedAgent(t, s, "t3-scope-valid")

	cases := []struct {
		name string
		sub  AgentForgeSubscription
	}{
		{"artifact with number=0", AgentForgeSubscription{
			AgentAccountID: agent, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b",
			Kind: ForgeArtifactKindIssue, Scope: ForgeSubscriptionScopeArtifact, Number: 0,
		}},
		{"artifact with project set", AgentForgeSubscription{
			AgentAccountID: agent, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b",
			Kind: ForgeArtifactKindIssue, Scope: ForgeSubscriptionScopeArtifact, Number: 5, Project: "P1",
		}},
		{"container with number set", AgentForgeSubscription{
			AgentAccountID: agent, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b",
			Kind: ForgeArtifactKindIssue, Scope: ForgeSubscriptionScopeContainer, Number: 3,
		}},
		{"linear container without project", AgentForgeSubscription{
			AgentAccountID: agent, Provider: ForgeProviderLinear, Host: "linear.app", Repo: "TEAM",
			Kind: ForgeArtifactKindIssue, Scope: ForgeSubscriptionScopeContainer,
		}},
		{"github container with project", AgentForgeSubscription{
			AgentAccountID: agent, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b",
			Kind: ForgeArtifactKindIssue, Scope: ForgeSubscriptionScopeContainer, Project: "P1",
		}},
		{"unknown scope", AgentForgeSubscription{
			AgentAccountID: agent, Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b",
			Kind: ForgeArtifactKindIssue, Scope: ForgeSubscriptionScope(99), Number: 5,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.EnsureAgentForgeSubscription(ctx, tc.sub)
			sentinelIs(t, err, ErrInvalidArgument, tc.name)
		})
	}
}

// TestAgentForgeLinearProjectContainersDistinct: two Linear project containers
// on one team (same agent, repo, kind; different project) coexist as two rows —
// the project column in the widened UNIQUE keeps them from colliding.
func TestAgentForgeLinearProjectContainersDistinct(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, _ := seedAgent(t, s, "t3-linear-proj")

	base := AgentForgeSubscription{
		AgentAccountID: agent, Provider: ForgeProviderLinear, Host: "linear.app", Repo: "TEAM",
		Kind: ForgeArtifactKindIssue, Scope: ForgeSubscriptionScopeContainer,
	}
	p1 := base
	p1.Project = "proj-1"
	p2 := base
	p2.Project = "proj-2"

	id1, err := s.EnsureAgentForgeSubscription(ctx, p1)
	if err != nil {
		t.Fatalf("ensure proj-1: %v", err)
	}
	id2, err := s.EnsureAgentForgeSubscription(ctx, p2)
	if err != nil {
		t.Fatalf("ensure proj-2: %v", err)
	}
	if id1 == id2 {
		t.Fatalf("two project containers share id %q, want distinct", id1)
	}
	if n := containerSubCount(t, s, base.Provider, base.Host, base.Repo, base.Kind, "proj-1"); n != 1 {
		t.Fatalf("proj-1 container count = %d, want 1", n)
	}
	if n := containerSubCount(t, s, base.Provider, base.Host, base.Repo, base.Kind, "proj-2"); n != 1 {
		t.Fatalf("proj-2 container count = %d, want 1", n)
	}
}

// ── T3: SubscribersForArtifact — exact + container fan-out ────────────────────

// TestSubscribersForArtifactGitHub: an artifact event returns the exact-artifact
// subscriber always, and the GitHub container subscriber only when openedEvent.
func TestSubscribersForArtifactGitHub(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	exact, _ := seedAgent(t, s, "t3-sfa-exact")
	ctr, _ := seedAgent(t, s, "t3-sfa-ctr")

	const (
		host   = "github.com"
		repo   = "a/b"
		number = uint64(42)
	)
	provider := ForgeProviderGitHub
	kind := ForgeArtifactKindIssue

	if _, err := s.EnsureAgentForgeSubscription(ctx, AgentForgeSubscription{
		AgentAccountID: exact, Provider: provider, Host: host, Repo: repo, Kind: kind,
		Number: number, Scope: ForgeSubscriptionScopeArtifact,
	}); err != nil {
		t.Fatalf("ensure exact: %v", err)
	}
	if _, err := s.EnsureAgentForgeSubscription(ctx, AgentForgeSubscription{
		AgentAccountID: ctr, Provider: provider, Host: host, Repo: repo, Kind: kind,
		Scope: ForgeSubscriptionScopeContainer,
	}); err != nil {
		t.Fatalf("ensure container: %v", err)
	}

	// Non-opened event: only the exact-artifact subscriber.
	subs, err := s.SubscribersForArtifact(ctx, provider, host, repo, kind, number, "", false)
	if err != nil {
		t.Fatalf("SubscribersForArtifact (not opened): %v", err)
	}
	if len(subs) != 1 || subs[0].AgentAccountID != exact {
		t.Fatalf("not-opened subs = %+v, want just exact agent %q", subs, exact)
	}

	// Opened event: exact + container.
	subs, err = s.SubscribersForArtifact(ctx, provider, host, repo, kind, number, "", true)
	if err != nil {
		t.Fatalf("SubscribersForArtifact (opened): %v", err)
	}
	got := map[AccountID]bool{}
	for _, sub := range subs {
		got[sub.AgentAccountID] = true
	}
	if len(subs) != 2 || !got[exact] || !got[ctr] {
		t.Fatalf("opened subs = %+v, want exact %q + container %q", subs, exact, ctr)
	}
}

// TestSubscribersForArtifactLinearProjectMatch: a Linear opened event fans out
// to the container subscriber whose project matches the artifact's project, and
// NOT to a container subscriber on a different project.
func TestSubscribersForArtifactLinearProjectMatch(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	match, _ := seedAgent(t, s, "t3-sfa-match")
	miss, _ := seedAgent(t, s, "t3-sfa-miss")

	const (
		host   = "linear.app"
		repo   = "TEAM"
		number = uint64(7)
	)
	provider := ForgeProviderLinear
	kind := ForgeArtifactKindIssue

	if _, err := s.EnsureAgentForgeSubscription(ctx, AgentForgeSubscription{
		AgentAccountID: match, Provider: provider, Host: host, Repo: repo, Kind: kind,
		Scope: ForgeSubscriptionScopeContainer, Project: "proj-A",
	}); err != nil {
		t.Fatalf("ensure match container: %v", err)
	}
	if _, err := s.EnsureAgentForgeSubscription(ctx, AgentForgeSubscription{
		AgentAccountID: miss, Provider: provider, Host: host, Repo: repo, Kind: kind,
		Scope: ForgeSubscriptionScopeContainer, Project: "proj-B",
	}); err != nil {
		t.Fatalf("ensure miss container: %v", err)
	}

	// Opened event on an artifact in proj-A: only the proj-A container subscriber.
	subs, err := s.SubscribersForArtifact(ctx, provider, host, repo, kind, number, "proj-A", true)
	if err != nil {
		t.Fatalf("SubscribersForArtifact (linear opened): %v", err)
	}
	if len(subs) != 1 || subs[0].AgentAccountID != match {
		t.Fatalf("linear opened subs = %+v, want just proj-A agent %q", subs, match)
	}
}

// TestSubscribersForArtifactRejectsZeroNumber: an artifact event MUST name an
// artifact — number=0 is a caller bug.
func TestSubscribersForArtifactRejectsZeroNumber(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_, err := s.SubscribersForArtifact(ctx, ForgeProviderGitHub, "github.com", "a/b", ForgeArtifactKindIssue, 0, "", false)
	sentinelIs(t, err, ErrInvalidArgument, "artifact event with number=0")
}

// ── T3: ListForgeNotifyTargets — enumeration + grouping ───────────────────────

// TestListForgeNotifyTargetsArtifactGrouping: two agents on one artifact collapse
// to ONE target carrying two subscribers, with a nil cursor before any upsert.
func TestListForgeNotifyTargetsArtifactGrouping(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agentA, _ := seedAgent(t, s, "t3-lnt-a")
	agentB, _ := seedAgent(t, s, "t3-lnt-b")

	const (
		host   = "github.com"
		repo   = "a/b"
		number = uint64(11)
	)
	provider := ForgeProviderGitHub
	kind := ForgeArtifactKindIssue

	for _, ag := range []AccountID{agentA, agentB} {
		if _, err := s.EnsureAgentForgeSubscription(ctx, AgentForgeSubscription{
			AgentAccountID: ag, Provider: provider, Host: host, Repo: repo, Kind: kind,
			Number: number, Scope: ForgeSubscriptionScopeArtifact,
		}); err != nil {
			t.Fatalf("ensure %s: %v", ag, err)
		}
	}

	targets, err := s.ListForgeNotifyTargets(ctx, provider, host)
	if err != nil {
		t.Fatalf("ListForgeNotifyTargets: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(targets))
	}
	tg := targets[0]
	if tg.Number != number || tg.Repo != repo || tg.Kind != kind {
		t.Fatalf("target coord = %+v, want repo=%q kind=%d number=%d", tg, repo, kind, number)
	}
	if tg.Cursor != nil {
		t.Fatalf("cursor = %+v, want nil before first upsert", tg.Cursor)
	}
	if len(tg.Subscribers) != 2 {
		t.Fatalf("subscribers = %d, want 2", len(tg.Subscribers))
	}

	// After an upsert, the cursor is observed.
	if err := s.UpsertForgeArtifactCursor(ctx, ForgeArtifactCursor{
		Provider: provider, Host: host, Repo: repo, Kind: kind, Number: number,
		ETag: `"e1"`, Revision: "rev-1",
	}); err != nil {
		t.Fatalf("UpsertForgeArtifactCursor: %v", err)
	}
	targets, err = s.ListForgeNotifyTargets(ctx, provider, host)
	if err != nil {
		t.Fatalf("ListForgeNotifyTargets (post-upsert): %v", err)
	}
	if len(targets) != 1 || targets[0].Cursor == nil {
		t.Fatalf("post-upsert target cursor = %+v, want non-nil", targets)
	}
	if targets[0].Cursor.Revision != "rev-1" || targets[0].Cursor.ETag != `"e1"` {
		t.Fatalf("cursor = %+v, want rev-1 / \"e1\"", targets[0].Cursor)
	}
}

// TestListForgeNotifyTargetsContainerCollapse: N Linear project container subs on
// one team collapse to ONE (repo, kind, number=0) container target carrying all
// N subscribers.
func TestListForgeNotifyTargetsContainerCollapse(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agentA, _ := seedAgent(t, s, "t3-cc-a")
	agentB, _ := seedAgent(t, s, "t3-cc-b")
	agentC, _ := seedAgent(t, s, "t3-cc-c")

	const (
		host = "linear.app"
		repo = "TEAM"
	)
	provider := ForgeProviderLinear
	kind := ForgeArtifactKindIssue

	projects := map[AccountID]string{agentA: "p1", agentB: "p2", agentC: "p3"}
	for ag, proj := range projects {
		if _, err := s.EnsureAgentForgeSubscription(ctx, AgentForgeSubscription{
			AgentAccountID: ag, Provider: provider, Host: host, Repo: repo, Kind: kind,
			Scope: ForgeSubscriptionScopeContainer, Project: proj,
		}); err != nil {
			t.Fatalf("ensure %s: %v", ag, err)
		}
	}

	targets, err := s.ListForgeNotifyTargets(ctx, provider, host)
	if err != nil {
		t.Fatalf("ListForgeNotifyTargets: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want 1 collapsed container target", len(targets))
	}
	tg := targets[0]
	if tg.Number != 0 || tg.Repo != repo || tg.Kind != kind {
		t.Fatalf("container target coord = %+v, want repo=%q kind=%d number=0", tg, repo, kind)
	}
	if len(tg.Subscribers) != 3 {
		t.Fatalf("container subscribers = %d, want 3", len(tg.Subscribers))
	}
	// Each collapsed subscriber must carry back its own project — the router
	// fans a project-P change out to only its project-P subscribers, so the
	// per-subscriber project must survive the (repo, kind) collapse.
	gotProjects := make(map[AccountID]string, len(tg.Subscribers))
	for _, sub := range tg.Subscribers {
		gotProjects[sub.AgentAccountID] = sub.Project
	}
	for ag, want := range projects {
		if got := gotProjects[ag]; got != want {
			t.Fatalf("subscriber %s project = %q, want %q", ag, got, want)
		}
	}

	// The container arm of the cursor LEFT JOIN (c.number = 0 for scope=2) is
	// exercised by upserting a cursor at the CONTAINER coordinate (Number:0) and
	// asserting the collapsed target picks it up — mirrors the number=11
	// post-upsert assertion in TestListForgeNotifyTargetsArtifactGrouping.
	if tg.Cursor != nil {
		t.Fatalf("container cursor = %+v, want nil before first upsert", tg.Cursor)
	}
	if err := s.UpsertForgeArtifactCursor(ctx, ForgeArtifactCursor{
		Provider: provider, Host: host, Repo: repo, Kind: kind, Number: 0,
		ETag: `"c1"`, Revision: "rev-c1",
	}); err != nil {
		t.Fatalf("UpsertForgeArtifactCursor (container): %v", err)
	}
	targets, err = s.ListForgeNotifyTargets(ctx, provider, host)
	if err != nil {
		t.Fatalf("ListForgeNotifyTargets (post-upsert): %v", err)
	}
	if len(targets) != 1 || targets[0].Cursor == nil {
		t.Fatalf("post-upsert container target cursor = %+v, want non-nil", targets)
	}
	if targets[0].Cursor.Revision != "rev-c1" || targets[0].Cursor.ETag != `"c1"` {
		t.Fatalf("container cursor = %+v, want rev-c1 / \"c1\"", targets[0].Cursor)
	}
}

// TestListForgeNotifyTargetsMixedArtifactAndContainer: one artifact sub
// (number>0) and two container subs (number=0, distinct projects) on the same
// (repo, kind) yield exactly TWO targets — the artifact target stands alone
// (number>0) while the two container subs collapse to ONE (number=0) target.
// This pins the collapse-vs-distinct boundary of the
// CASE WHEN s.scope=2 THEN 0 ELSE s.number END grouping.
func TestListForgeNotifyTargetsMixedArtifactAndContainer(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	artAgent, _ := seedAgent(t, s, "t3-mix-art")
	c1Agent, _ := seedAgent(t, s, "t3-mix-c1")
	c2Agent, _ := seedAgent(t, s, "t3-mix-c2")

	const (
		host   = "linear.app"
		repo   = "TEAM"
		number = uint64(7)
	)
	provider := ForgeProviderLinear
	kind := ForgeArtifactKindIssue

	if _, err := s.EnsureAgentForgeSubscription(ctx, AgentForgeSubscription{
		AgentAccountID: artAgent, Provider: provider, Host: host, Repo: repo, Kind: kind,
		Number: number, Scope: ForgeSubscriptionScopeArtifact,
	}); err != nil {
		t.Fatalf("ensure artifact: %v", err)
	}
	for ag, proj := range map[AccountID]string{c1Agent: "p1", c2Agent: "p2"} {
		if _, err := s.EnsureAgentForgeSubscription(ctx, AgentForgeSubscription{
			AgentAccountID: ag, Provider: provider, Host: host, Repo: repo, Kind: kind,
			Scope: ForgeSubscriptionScopeContainer, Project: proj,
		}); err != nil {
			t.Fatalf("ensure container %s: %v", ag, err)
		}
	}

	targets, err := s.ListForgeNotifyTargets(ctx, provider, host)
	if err != nil {
		t.Fatalf("ListForgeNotifyTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2 (one artifact + one collapsed container)", len(targets))
	}
	var artTarget, containerTarget *ForgeNotifyTarget
	for i := range targets {
		switch targets[i].Number {
		case number:
			artTarget = &targets[i]
		case 0:
			containerTarget = &targets[i]
		default:
			t.Fatalf("unexpected target number %d", targets[i].Number)
		}
	}
	if artTarget == nil {
		t.Fatalf("no artifact target (number=%d) in %+v", number, targets)
	}
	if len(artTarget.Subscribers) != 1 {
		t.Fatalf("artifact subscribers = %d, want 1", len(artTarget.Subscribers))
	}
	if containerTarget == nil {
		t.Fatalf("no collapsed container target (number=0) in %+v", targets)
	}
	if len(containerTarget.Subscribers) != 2 {
		t.Fatalf("container subscribers = %d, want 2", len(containerTarget.Subscribers))
	}
}

// ── T3: AdvanceForgeDeliveredRevision ─────────────────────────────────────────

// TestAdvanceForgeDeliveredRevision: happy path advances the cursor; an unknown
// id and a foreign-agent id both return ErrNotFound.
func TestAdvanceForgeDeliveredRevision(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner, _ := seedAgent(t, s, "t3-adv-owner")
	foreign, _ := seedAgent(t, s, "t3-adv-foreign")

	id, err := s.EnsureAgentForgeSubscription(ctx, AgentForgeSubscription{
		AgentAccountID: owner, Provider: ForgeProviderGitHub, Host: "github.com",
		Repo: "a/b", Kind: ForgeArtifactKindIssue, Number: 1, Scope: ForgeSubscriptionScopeArtifact,
	})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}

	// Happy path.
	if err := s.AdvanceForgeDeliveredRevision(ctx, owner, id, "rev-9"); err != nil {
		t.Fatalf("advance happy: %v", err)
	}
	var got string
	if err := s.pool.QueryRow(ctx,
		`SELECT delivered_revision FROM agent_forge_subscriptions WHERE id = $1`, id,
	).Scan(&got); err != nil {
		t.Fatalf("read delivered_revision: %v", err)
	}
	if got != "rev-9" {
		t.Fatalf("delivered_revision = %q, want rev-9", got)
	}

	// Unknown id -> ErrNotFound.
	sentinelIs(t, s.AdvanceForgeDeliveredRevision(ctx, owner, "no-such-id", "rev-x"), ErrNotFound, "advance unknown id")
	// Foreign agent on a real id -> ErrNotFound (scoping), row untouched.
	sentinelIs(t, s.AdvanceForgeDeliveredRevision(ctx, foreign, id, "rev-x"), ErrNotFound, "advance foreign agent")
	// Empty agent / empty subscription id -> ErrInvalidArgument (early guards).
	sentinelIs(t, s.AdvanceForgeDeliveredRevision(ctx, "", id, "rev-x"), ErrInvalidArgument, "advance empty agent")
	sentinelIs(t, s.AdvanceForgeDeliveredRevision(ctx, owner, "", "rev-x"), ErrInvalidArgument, "advance empty id")
	if err := s.pool.QueryRow(ctx,
		`SELECT delivered_revision FROM agent_forge_subscriptions WHERE id = $1`, id,
	).Scan(&got); err != nil {
		t.Fatalf("re-read delivered_revision: %v", err)
	}
	if got != "rev-9" {
		t.Fatalf("delivered_revision after foreign advance = %q, want rev-9 (untouched)", got)
	}
}

// ── T7a: LoadForgeArtifactCursor point-read ───────────────────────────────────

// TestLoadForgeArtifactCursor: a written cursor round-trips through the
// single-coordinate reader; a never-observed coordinate returns (nil, nil); an
// invalid coordinate (zero kind) returns ErrInvalidArgument.
func TestLoadForgeArtifactCursor(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	const (
		host   = "github.com"
		repo   = "a/b"
		number = uint64(42)
	)
	provider := ForgeProviderGitHub
	kind := ForgeArtifactKindIssue

	if err := s.UpsertForgeArtifactCursor(ctx, ForgeArtifactCursor{
		Provider: provider, Host: host, Repo: repo, Kind: kind, Number: number,
		ETag: `"e1"`, CommentsETag: `"c1"`, ChecksETag: `"k1"`,
		Revision: "rev-1", Snapshot: []byte(`{"x":1}`),
	}); err != nil {
		t.Fatalf("UpsertForgeArtifactCursor: %v", err)
	}

	// 1. Round-trip: the written cursor reads back with matching fields.
	got, err := s.LoadForgeArtifactCursor(ctx, provider, host, repo, kind, number)
	if err != nil {
		t.Fatalf("LoadForgeArtifactCursor: %v", err)
	}
	if got == nil {
		t.Fatalf("cursor = nil, want non-nil round-trip")
	}
	if got.ETag != `"e1"` || got.CommentsETag != `"c1"` || got.ChecksETag != `"k1"` ||
		got.Revision != "rev-1" {
		t.Fatalf("cursor = %+v, want e1/c1/k1/rev-1", got)
	}
	// Snapshot is a JSONB column: Postgres normalizes the stored JSON (whitespace,
	// key order), so the round-trip is semantically equal but NOT byte-identical
	// to the written `{"x":1}` (it reads back `{"x": 1}`). Compare parsed values,
	// never bytes — a byte compare here is Postgres-version-fragile.
	if !jsonSemanticEqual(t, got.Snapshot, []byte(`{"x":1}`)) {
		t.Fatalf("snapshot = %s, want JSON-equal to {\"x\":1}", got.Snapshot)
	}

	// 2. Never-observed coordinate → (nil, nil).
	got, err = s.LoadForgeArtifactCursor(ctx, provider, host, repo, kind, 999)
	if err != nil {
		t.Fatalf("LoadForgeArtifactCursor (unseeded): %v", err)
	}
	if got != nil {
		t.Fatalf("cursor = %+v, want nil for never-observed coordinate", got)
	}

	// 3. Invalid coordinate (zero kind) → ErrInvalidArgument.
	if _, err := s.LoadForgeArtifactCursor(ctx, provider, host, repo, ForgeArtifactKind(0), number); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero kind err = %v, want ErrInvalidArgument", err)
	}
}
