//go:build pgtest

package store

// DL-055 forge ownership-index store contracts (design
// docs/designs/product/compass-forge-write-path/design.md §T7 test cycle, the
// DL-174 pair: this pgtest suite plus the in-memory reference in
// forge_authored_test.go): the migration 0002 table shape, the idempotent
// coordinate upsert, the FK RESTRICT on agent/owner, the by-agent scan order,
// the F3 memo lookup (hit/miss), and the UNIQUE violation on a duplicate
// (agent, client_request_id) non-null key with null-key rows never colliding.
// context.Background is the test root (the pgtest-suite convention, sibling
// forge_cursors_pgtest_test.go).

import (
	"context"
	"testing"
)

// seedAgent creates an owner user + owned agent and returns their ids, so the
// ownership rows have real FK referents.
func seedAgent(t *testing.T, s *Store, handle string) (agent, owner AccountID) {
	t.Helper()
	u := mustUser(t, s, handle+"-owner")
	a := mustAgent(t, s, u.ID, handle+"-agent")
	return a.ID, u.ID
}

// ── Test 1: insert + read-back through the by-agent scan ──────────────────────

func TestRecordAuthoredArtifactInsertReadBack(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "t1")

	want := AuthoredArtifact{
		Provider:        ForgeProviderGitHub,
		Host:            "github.com",
		Repo:            "a/b",
		Kind:            ForgeArtifactKindIssue,
		Number:          42,
		AgentAccountID:  agent,
		OwnerUserID:     owner,
		SessionID:       "sess-1",
		ClientRequestID: "req-1",
		CreatedAtUnixMS: 1000,
	}
	if err := s.RecordAuthoredArtifact(ctx, want); err != nil {
		t.Fatalf("record: %v", err)
	}

	got, err := s.ListAuthoredArtifactsByAgent(ctx, agent)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("row count = %d, want 1", len(got))
	}
	if got[0] != want {
		t.Fatalf("read-back = %+v, want %+v", got[0], want)
	}
}

// ── Test 2: idempotent re-insert on the coordinate PK — one row, updated ──────

func TestRecordAuthoredArtifactIdempotentUpsert(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "t2")

	base := AuthoredArtifact{
		Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b",
		Kind: ForgeArtifactKindPullRequest, Number: 7,
		AgentAccountID: agent, OwnerUserID: owner,
		SessionID: "sess-1", ClientRequestID: "req-1", CreatedAtUnixMS: 1000,
	}
	if err := s.RecordAuthoredArtifact(ctx, base); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Re-record the SAME coordinate with mutated non-key fields: an upsert, not
	// a duplicate. session_id/created_at update in place.
	base.SessionID = "sess-2"
	base.CreatedAtUnixMS = 2000
	if err := s.RecordAuthoredArtifact(ctx, base); err != nil {
		t.Fatalf("re-record: %v", err)
	}

	got, err := s.ListAuthoredArtifactsByAgent(ctx, agent)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("row count = %d, want 1 (upsert, not insert)", len(got))
	}
	if got[0].SessionID != "sess-2" || got[0].CreatedAtUnixMS != 2000 {
		t.Fatalf("row not updated in place: %+v", got[0])
	}

	// Authorship is WRITE-ONCE: re-landing the SAME coordinate under a DIFFERENT
	// agent must NOT rewrite who authored it (the DO UPDATE SET omits
	// agent_account_id/owner_user_id). This can't happen on the real path (a
	// forge never reuses a coordinate), but the store must not silently transfer
	// ownership if it ever did.
	other, otherOwner := seedAgent(t, s, "t2-other")
	steal := base
	steal.AgentAccountID = other
	steal.OwnerUserID = otherOwner
	steal.SessionID = "sess-3"
	if err := s.RecordAuthoredArtifact(ctx, steal); err != nil {
		t.Fatalf("re-record under different agent: %v", err)
	}
	// The original author still owns the row; the thief's by-agent scan is empty.
	orig, err := s.ListAuthoredArtifactsByAgent(ctx, agent)
	if err != nil {
		t.Fatalf("list original: %v", err)
	}
	if len(orig) != 1 || orig[0].AgentAccountID != agent || orig[0].OwnerUserID != owner {
		t.Fatalf("authorship not write-once: original agent %q lost the row: %+v", agent, orig)
	}
	if orig[0].SessionID != "sess-3" || orig[0].CreatedAtUnixMS != 2000 {
		t.Fatalf("non-authorship fields should still update in place: %+v", orig[0])
	}
	stolen, err := s.ListAuthoredArtifactsByAgent(ctx, other)
	if err != nil {
		t.Fatalf("list thief: %v", err)
	}
	if len(stolen) != 0 {
		t.Fatalf("different agent must not own the row: %+v", stolen)
	}
}

// ── Test 3: FK RESTRICT — unknown agent / owner is rejected ───────────────────

func TestRecordAuthoredArtifactFKRestrict(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "t3")

	// Unknown agent.
	err := s.RecordAuthoredArtifact(ctx, AuthoredArtifact{
		Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b",
		Kind: ForgeArtifactKindIssue, Number: 1,
		AgentAccountID: "no-such-agent", OwnerUserID: owner, CreatedAtUnixMS: 1,
	})
	sentinelIs(t, err, ErrInvalidArgument, "unknown agent FK")

	// Unknown owner.
	err = s.RecordAuthoredArtifact(ctx, AuthoredArtifact{
		Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b",
		Kind: ForgeArtifactKindIssue, Number: 2,
		AgentAccountID: agent, OwnerUserID: "no-such-owner", CreatedAtUnixMS: 1,
	})
	sentinelIs(t, err, ErrInvalidArgument, "unknown owner FK")
}

// ── Test 4: by-agent scan ordering (created_at then coordinate) ───────────────

func TestListAuthoredArtifactsByAgentOrdering(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "t4")
	other, otherOwner := seedAgent(t, s, "t4-other")

	// Insert out of created-at order; expect ascending created_at back.
	rows := []AuthoredArtifact{
		{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b", Kind: ForgeArtifactKindIssue, Number: 3, AgentAccountID: agent, OwnerUserID: owner, CreatedAtUnixMS: 300},
		{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b", Kind: ForgeArtifactKindIssue, Number: 1, AgentAccountID: agent, OwnerUserID: owner, CreatedAtUnixMS: 100},
		{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b", Kind: ForgeArtifactKindIssue, Number: 2, AgentAccountID: agent, OwnerUserID: owner, CreatedAtUnixMS: 200},
	}
	for _, r := range rows {
		if err := s.RecordAuthoredArtifact(ctx, r); err != nil {
			t.Fatalf("record %d: %v", r.Number, err)
		}
	}
	// A foreign agent's row must not leak into this agent's scan.
	if err := s.RecordAuthoredArtifact(ctx, AuthoredArtifact{
		Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b", Kind: ForgeArtifactKindIssue, Number: 9,
		AgentAccountID: other, OwnerUserID: otherOwner, CreatedAtUnixMS: 50,
	}); err != nil {
		t.Fatalf("record foreign: %v", err)
	}

	got, err := s.ListAuthoredArtifactsByAgent(ctx, agent)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("row count = %d, want 3 (foreign agent excluded)", len(got))
	}
	if got[0].Number != 1 || got[1].Number != 2 || got[2].Number != 3 {
		t.Fatalf("order = [%d %d %d], want ascending created_at [1 2 3]", got[0].Number, got[1].Number, got[2].Number)
	}

	// No rows for an agent that authored nothing is a nil slice, not an error.
	empty, otherErr := s.ListAuthoredArtifactsByAgent(ctx, otherOwner)
	if otherErr != nil {
		t.Fatalf("list empty: %v", otherErr)
	}
	if empty != nil {
		t.Fatalf("no-rows = %v, want nil", empty)
	}
}

// ── Test 4b: ordering tie-break on the coordinate when created_at is equal ─────

func TestListAuthoredArtifactsByAgentOrderingTieBreak(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "t4b")

	// All rows share created_at, so the secondary coordinate keys (provider,
	// host, repo, kind, number) decide the order — a regression that dropped the
	// coordinate tie-break from the ORDER BY would make this non-deterministic.
	// Insert scrambled; expect back sorted by (repo, kind, number) at equal time.
	const t0 = 777
	rows := []AuthoredArtifact{
		{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b", Kind: ForgeArtifactKindPullRequest, Number: 2, AgentAccountID: agent, OwnerUserID: owner, CreatedAtUnixMS: t0},
		{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b", Kind: ForgeArtifactKindIssue, Number: 5, AgentAccountID: agent, OwnerUserID: owner, CreatedAtUnixMS: t0},
		{Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/a", Kind: ForgeArtifactKindIssue, Number: 1, AgentAccountID: agent, OwnerUserID: owner, CreatedAtUnixMS: t0},
	}
	for _, r := range rows {
		if err := s.RecordAuthoredArtifact(ctx, r); err != nil {
			t.Fatalf("record %s#%d: %v", r.Repo, r.Number, err)
		}
	}

	got, err := s.ListAuthoredArtifactsByAgent(ctx, agent)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("row count = %d, want 3", len(got))
	}
	// Expected coordinate order at equal created_at: repo a/a before a/b, then
	// within a/b kind issue(1) before pull_request(2).
	type coord struct {
		repo   string
		kind   ForgeArtifactKind
		number uint64
	}
	want := []coord{
		{"a/a", ForgeArtifactKindIssue, 1},
		{"a/b", ForgeArtifactKindIssue, 5},
		{"a/b", ForgeArtifactKindPullRequest, 2},
	}
	for i, w := range want {
		if got[i].Repo != w.repo || got[i].Kind != w.kind || got[i].Number != w.number {
			t.Fatalf("row %d = {%s %d #%d}, want {%s %d #%d}", i, got[i].Repo, got[i].Kind, got[i].Number, w.repo, w.kind, w.number)
		}
	}
}

// ── Test 5: F3 memo lookup — hit and miss ─────────────────────────────────────

func TestAuthoredArtifactByRequestID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "t5")

	want := AuthoredArtifact{
		Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b",
		Kind: ForgeArtifactKindIssue, Number: 5,
		AgentAccountID: agent, OwnerUserID: owner,
		SessionID: "sess", ClientRequestID: "req-hit", CreatedAtUnixMS: 500,
	}
	if err := s.RecordAuthoredArtifact(ctx, want); err != nil {
		t.Fatalf("record: %v", err)
	}

	// Hit.
	got, ok, err := s.AuthoredArtifactByRequestID(ctx, agent, "req-hit")
	if err != nil {
		t.Fatalf("lookup hit: %v", err)
	}
	if !ok {
		t.Fatal("lookup = miss, want hit")
	}
	if got != want {
		t.Fatalf("lookup = %+v, want %+v", got, want)
	}

	// Miss: unknown key.
	_, ok, err = s.AuthoredArtifactByRequestID(ctx, agent, "req-none")
	if err != nil {
		t.Fatalf("lookup miss: %v", err)
	}
	if ok {
		t.Fatal("unknown key = hit, want miss")
	}

	// Miss: right key, wrong agent (the memo is per-agent scoped).
	otherAgent, _ := seedAgent(t, s, "t5-other")
	_, ok, err = s.AuthoredArtifactByRequestID(ctx, otherAgent, "req-hit")
	if err != nil {
		t.Fatalf("lookup wrong agent: %v", err)
	}
	if ok {
		t.Fatal("key under wrong agent = hit, want miss")
	}

	// Empty clientRequestID is ALWAYS a miss — it never matches a NULL-key row.
	_, ok, err = s.AuthoredArtifactByRequestID(ctx, agent, "")
	if err != nil {
		t.Fatalf("lookup empty key: %v", err)
	}
	if ok {
		t.Fatal("empty clientRequestID = hit, want always-miss")
	}
}

// ── Test 6: UNIQUE violation on a duplicate (agent, client_request_id) key ────

func TestRecordAuthoredArtifactDuplicateRequestIDConflict(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "t6")

	first := AuthoredArtifact{
		Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b",
		Kind: ForgeArtifactKindIssue, Number: 1,
		AgentAccountID: agent, OwnerUserID: owner, ClientRequestID: "dup", CreatedAtUnixMS: 1,
	}
	if err := s.RecordAuthoredArtifact(ctx, first); err != nil {
		t.Fatalf("record first: %v", err)
	}
	// A DIFFERENT coordinate reusing the same (agent, client_request_id) key:
	// not a PK conflict (distinct coordinate) but a memo UNIQUE violation.
	second := first
	second.Number = 2
	err := s.RecordAuthoredArtifact(ctx, second)
	sentinelIs(t, err, ErrConflict, "duplicate (agent, client_request_id) memo key")

	// The SAME key under a DIFFERENT agent is fine (memo is per-agent).
	otherAgent, otherOwner := seedAgent(t, s, "t6-other")
	third := first
	third.AgentAccountID = otherAgent
	third.OwnerUserID = otherOwner
	third.Number = 3
	if err := s.RecordAuthoredArtifact(ctx, third); err != nil {
		t.Fatalf("record same key different agent: %v", err)
	}
}

// ── Test 7: null-key rows never collide and are never returned by lookup ──────

func TestRecordAuthoredArtifactNullKeyRowsDoNotCollide(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "t7")

	// Two DISTINCT coordinates, both with an empty clientRequestID (NULL key).
	// The partial unique index must NOT treat two NULLs as a collision.
	for _, n := range []uint64{1, 2} {
		if err := s.RecordAuthoredArtifact(ctx, AuthoredArtifact{
			Provider: ForgeProviderGitHub, Host: "github.com", Repo: "a/b",
			Kind: ForgeArtifactKindIssue, Number: n,
			AgentAccountID: agent, OwnerUserID: owner, ClientRequestID: "", CreatedAtUnixMS: int64(n),
		}); err != nil {
			t.Fatalf("record null-key %d: %v", n, err)
		}
	}
	got, err := s.ListAuthoredArtifactsByAgent(ctx, agent)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("row count = %d, want 2 (null-key rows do not collide)", len(got))
	}
	for _, a := range got {
		if a.ClientRequestID != "" {
			t.Fatalf("null-key row read back with key %q, want empty", a.ClientRequestID)
		}
	}
	// A null-key row is never returned by the memo lookup, even by empty key.
	if _, ok, _ := s.AuthoredArtifactByRequestID(ctx, agent, ""); ok {
		t.Fatal("empty-key lookup returned a null-key row, want always-miss")
	}
}

// ── Test 8: migration 0002 table shape — provider/kind CHECK domains ──────────

func TestMigration0002AuthoredArtifactChecks(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, owner := seedAgent(t, s, "t8")

	insert := func(provider, kind int) error {
		_, err := s.pool.Exec(ctx,
			`INSERT INTO forge_authored_artifacts
			   (forge_provider, forge_host, repo, kind, number, agent_account_id, owner_user_id, created_at_unix_ms)
			 VALUES ($1, 'github.com', 'a/b', $2, 1, $3, $4, 1)`,
			provider, kind, string(agent), string(owner))
		return err
	}
	// provider domain: 0 and 5 rejected.
	for _, p := range []int{0, 5} {
		if err := insert(p, 1); err == nil {
			t.Fatalf("provider %d accepted, want CHECK rejection", p)
		}
	}
	// kind domain: 0 and 3 rejected.
	for _, k := range []int{0, 3} {
		if err := insert(1, k); err == nil {
			t.Fatalf("kind %d accepted, want CHECK rejection", k)
		}
	}
	// A legal row inserts.
	if err := insert(1, 1); err != nil {
		t.Fatalf("legal row rejected: %v", err)
	}
}

// ── Test 9: invalid input → ErrInvalidArgument on every method ────────────────

func TestForgeAuthoredInvalidArgument(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	good := AuthoredArtifact{
		Provider: ForgeProviderGitHub, Host: "h", Repo: "r",
		Kind: ForgeArtifactKindIssue, Number: 1, AgentAccountID: "a", OwnerUserID: "o",
	}
	zeroProvider := good
	zeroProvider.Provider = ForgeProviderUnspecified
	sentinelIs(t, s.RecordAuthoredArtifact(ctx, zeroProvider), ErrInvalidArgument, "record zero provider")

	emptyHost := good
	emptyHost.Host = ""
	sentinelIs(t, s.RecordAuthoredArtifact(ctx, emptyHost), ErrInvalidArgument, "record empty host")

	emptyRepo := good
	emptyRepo.Repo = ""
	sentinelIs(t, s.RecordAuthoredArtifact(ctx, emptyRepo), ErrInvalidArgument, "record empty repo")

	zeroKind := good
	zeroKind.Kind = ForgeArtifactKindUnspecified
	sentinelIs(t, s.RecordAuthoredArtifact(ctx, zeroKind), ErrInvalidArgument, "record zero kind")

	noAgent := good
	noAgent.AgentAccountID = ""
	sentinelIs(t, s.RecordAuthoredArtifact(ctx, noAgent), ErrInvalidArgument, "record empty agent")

	noOwner := good
	noOwner.OwnerUserID = ""
	sentinelIs(t, s.RecordAuthoredArtifact(ctx, noOwner), ErrInvalidArgument, "record empty owner")

	sentinelIs(t, mustErr(func() error { _, _, e := s.AuthoredArtifactByRequestID(ctx, "", "req"); return e }), ErrInvalidArgument, "lookup empty agent")
	sentinelIs(t, mustErr(func() error { _, e := s.ListAuthoredArtifactsByAgent(ctx, ""); return e }), ErrInvalidArgument, "list empty agent")
}
