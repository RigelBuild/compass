//go:build pgtest

package store

// Real-Postgres contracts for the agent-driven state-transition memo (design
// docs/designs/server/compass-forge-state-transition/design.md §Actor
// attribution): the coordinate-keyed upsert (latest transition wins), the
// single-statement clear-and-return consume (a memo attributes at most one
// event, so a second consume finds nothing), the freshness bound, and the miss
// cases a human/external transition produces. context.Background is the test
// root (the pgtest-suite convention, sibling forge_authored_pgtest_test.go).

import (
	"context"
	"testing"
	"time"
)

// txnNow reads Postgres's own statement timestamp, so the tests' freshness
// arithmetic is anchored to the SAME clock the consume's now() stamps rather
// than to the Go process's — a skewed test box would otherwise make the
// freshness bound flaky.
func txnNow(t *testing.T, ctx context.Context, s *Store) time.Time {
	t.Helper()
	var now time.Time
	if err := s.pool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		t.Fatalf("read database now(): %v", err)
	}
	return now
}

// recordAt is the common write: an issue memo at the fixture coordinate.
func recordAt(t *testing.T, ctx context.Context, s *Store, agent AccountID, state string, at time.Time) {
	t.Helper()
	if err := s.RecordStateTransition(ctx, ForgeProviderGitHub, "github.com", "a/b", ForgeArtifactKindIssue, 42, state, agent, at); err != nil {
		t.Fatalf("record state transition: %v", err)
	}
}

// consumeAt is the common read: consume the fixture coordinate's memo for state,
// bounded by fresh.
func consumeAt(t *testing.T, ctx context.Context, s *Store, state string, fresh time.Time) (AccountID, bool) {
	t.Helper()
	agent, ok, err := s.ConsumeStateTransition(ctx, ForgeProviderGitHub, "github.com", "a/b", ForgeArtifactKindIssue, 42, state, fresh)
	if err != nil {
		t.Fatalf("consume state transition: %v", err)
	}
	return agent, ok
}

// ── Upsert: latest transition wins, and re-arms an already-consumed memo ──────

func TestRecordStateTransitionUpsertLatestWins(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	first, _ := seedAgent(t, s, "fst1")
	second, _ := seedAgent(t, s, "fst1-second")
	now := txnNow(t, ctx, s)

	// Two transitions at the SAME coordinate: the second must REPLACE the first,
	// not accrete a row — the coordinate PK is the memo's identity.
	recordAt(t, ctx, s, first, TransitionStateClosed, now.Add(-2*time.Minute))
	recordAt(t, ctx, s, second, TransitionStateOpen, now.Add(-1*time.Minute))

	var rows int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM forge_state_transitions`).Scan(&rows); err != nil {
		t.Fatalf("count memos: %v", err)
	}
	if rows != 1 {
		t.Fatalf("memo rows = %d, want 1 (upsert on the coordinate PK, not a second row)", rows)
	}

	// The superseded state no longer matches; the latest one resolves its agent.
	if agent, ok := consumeAt(t, ctx, s, TransitionStateClosed, now.Add(-time.Hour)); ok {
		t.Fatalf("superseded state resolved agent %q, want no match", agent)
	}
	agent, ok := consumeAt(t, ctx, s, TransitionStateOpen, now.Add(-time.Hour))
	if !ok || agent != second {
		t.Fatalf("latest transition resolved (%q, %v), want (%q, true)", agent, ok, second)
	}
}

// A re-transition must RE-ARM the memo: the upsert clears consumed_at, so the
// newest transition is attributable even though the previous one at the same
// coordinate was already consumed. Without the reset, an agent could close an
// issue, have that event attributed, then reopen it and go unattributed forever.
func TestRecordStateTransitionUpsertReArmsConsumedMemo(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, _ := seedAgent(t, s, "fst2")
	now := txnNow(t, ctx, s)

	recordAt(t, ctx, s, agent, TransitionStateClosed, now.Add(-2*time.Minute))
	if _, ok := consumeAt(t, ctx, s, TransitionStateClosed, now.Add(-time.Hour)); !ok {
		t.Fatalf("first consume found no memo")
	}

	recordAt(t, ctx, s, agent, TransitionStateOpen, now.Add(-time.Minute))
	got, ok := consumeAt(t, ctx, s, TransitionStateOpen, now.Add(-time.Hour))
	if !ok || got != agent {
		t.Fatalf("re-armed memo resolved (%q, %v), want (%q, true)", got, ok, agent)
	}
}

// ── Consume-once: one memo attributes at most one event ──────────────────────

func TestConsumeStateTransitionConsumesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, _ := seedAgent(t, s, "fst3")
	now := txnNow(t, ctx, s)
	fresh := now.Add(-time.Hour)

	recordAt(t, ctx, s, agent, TransitionStateClosed, now.Add(-time.Minute))

	got, ok := consumeAt(t, ctx, s, TransitionStateClosed, fresh)
	if !ok || got != agent {
		t.Fatalf("first consume = (%q, %v), want (%q, true)", got, ok, agent)
	}
	// The clear and the return are ONE statement, so the memo is already claimed:
	// a second reader (the reconcile sweep racing the webhook arm) resolves no
	// actor and its event is delivered unattributed — the documented fail-open.
	if got, ok := consumeAt(t, ctx, s, TransitionStateClosed, fresh); ok {
		t.Fatalf("second consume = (%q, true), want no actor (the memo was already claimed)", got)
	}

	// The row survives its consume (stamped, not deleted), so the claim is
	// durable rather than depending on a delete the next upsert would race.
	var consumed bool
	if err := s.pool.QueryRow(ctx, `SELECT consumed_at IS NOT NULL FROM forge_state_transitions`).Scan(&consumed); err != nil {
		t.Fatalf("read consumed_at: %v", err)
	}
	if !consumed {
		t.Fatalf("consumed_at is still NULL after a successful consume")
	}
}

// ── The freshness bound ───────────────────────────────────────────────────────

func TestConsumeStateTransitionFreshnessBound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, _ := seedAgent(t, s, "fst4")
	now := txnNow(t, ctx, s)

	// A memo written BEFORE the bound is stale: a lingering memo (the Route
	// cursor-advance race) must not attribute an event minutes later.
	recordAt(t, ctx, s, agent, TransitionStateClosed, now.Add(-10*time.Minute))
	if got, ok := consumeAt(t, ctx, s, TransitionStateClosed, now.Add(-5*time.Minute)); ok {
		t.Fatalf("stale memo resolved agent %q, want no match", got)
	}
	// Rejecting a stale memo must NOT consume it — the bound is a filter, not a
	// sweep. Relaxing the bound still finds it.
	got, ok := consumeAt(t, ctx, s, TransitionStateClosed, now.Add(-time.Hour))
	if !ok || got != agent {
		t.Fatalf("relaxed bound = (%q, %v), want (%q, true) — the stale read must not have consumed the memo", got, ok, agent)
	}
}

// The bound is inclusive at its edge: a memo written exactly AT fresh resolves.
// An exclusive comparison would drop an on-the-boundary memo, so the edge is
// pinned rather than left to the operator's reading of ">=".
func TestConsumeStateTransitionFreshnessBoundIsInclusive(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, _ := seedAgent(t, s, "fst5")
	at := txnNow(t, ctx, s).Add(-time.Minute)

	recordAt(t, ctx, s, agent, TransitionStateClosed, at)
	got, ok := consumeAt(t, ctx, s, TransitionStateClosed, at)
	if !ok || got != agent {
		t.Fatalf("memo written exactly at the bound = (%q, %v), want (%q, true)", got, ok, agent)
	}
}

// ── Miss cases: every human/external transition lands here ───────────────────

func TestConsumeStateTransitionMisses(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, _ := seedAgent(t, s, "fst6")
	now := txnNow(t, ctx, s)
	fresh := now.Add(-time.Hour)

	// No memo at all — a human closing an issue Compass never transitioned.
	if got, ok := consumeAt(t, ctx, s, TransitionStateClosed, fresh); ok {
		t.Fatalf("no-memo consume = (%q, true), want no actor", got)
	}

	recordAt(t, ctx, s, agent, TransitionStateClosed, now.Add(-time.Minute))

	// State mismatch: the echoed event's state is not the one the agent applied,
	// so the memo does not attribute it (and is NOT consumed by the near-miss).
	if got, ok := consumeAt(t, ctx, s, TransitionStateOpen, fresh); ok {
		t.Fatalf("state-mismatch consume = (%q, true), want no actor", got)
	}

	// Coordinate mismatch on each key component: a memo attributes ONLY its own
	// artifact. Sharing a repo, a number, or a kind must not be enough.
	for _, m := range []struct {
		name     string
		provider ForgeProvider
		host     string
		repo     string
		kind     ForgeArtifactKind
		number   uint64
	}{
		{"other provider", ForgeProviderLinear, "github.com", "a/b", ForgeArtifactKindIssue, 42},
		{"other host", ForgeProviderGitHub, "ghe.example", "a/b", ForgeArtifactKindIssue, 42},
		{"other repo", ForgeProviderGitHub, "github.com", "a/c", ForgeArtifactKindIssue, 42},
		{"other kind", ForgeProviderGitHub, "github.com", "a/b", ForgeArtifactKindPullRequest, 42},
		{"other number", ForgeProviderGitHub, "github.com", "a/b", ForgeArtifactKindIssue, 43},
	} {
		t.Run(m.name, func(t *testing.T) {
			got, ok, err := s.ConsumeStateTransition(ctx, m.provider, m.host, m.repo, m.kind, m.number, TransitionStateClosed, fresh)
			if err != nil {
				t.Fatalf("consume: %v", err)
			}
			if ok {
				t.Fatalf("%s resolved agent %q, want no actor", m.name, got)
			}
		})
	}

	// None of the misses consumed the real memo: its own coordinate still
	// resolves. A miss that silently claimed the row would lose the attribution
	// the memo exists for.
	got, ok := consumeAt(t, ctx, s, TransitionStateClosed, fresh)
	if !ok || got != agent {
		t.Fatalf("exact match after the misses = (%q, %v), want (%q, true)", got, ok, agent)
	}
}

// ── Door-side validation: a caller bug never reaches Postgres ────────────────

func TestStateTransitionValidation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	agent, _ := seedAgent(t, s, "fst7")
	at := txnNow(t, ctx, s)

	sentinelIs(t, s.RecordStateTransition(ctx, ForgeProviderUnspecified, "github.com", "a/b", ForgeArtifactKindIssue, 42, TransitionStateClosed, agent, at),
		ErrInvalidArgument, "record with an unspecified provider")
	sentinelIs(t, s.RecordStateTransition(ctx, ForgeProviderGitHub, "", "a/b", ForgeArtifactKindIssue, 42, TransitionStateClosed, agent, at),
		ErrInvalidArgument, "record with an empty host")
	sentinelIs(t, s.RecordStateTransition(ctx, ForgeProviderGitHub, "github.com", "", ForgeArtifactKindIssue, 42, TransitionStateClosed, agent, at),
		ErrInvalidArgument, "record with an empty repo")
	sentinelIs(t, s.RecordStateTransition(ctx, ForgeProviderGitHub, "github.com", "a/b", ForgeArtifactKindUnspecified, 42, TransitionStateClosed, agent, at),
		ErrInvalidArgument, "record with an unspecified kind")
	// The state domain is guarded in Go space, so an out-of-domain value is a
	// caller bug rather than a raw CHECK violation surfacing from the driver.
	sentinelIs(t, s.RecordStateTransition(ctx, ForgeProviderGitHub, "github.com", "a/b", ForgeArtifactKindIssue, 42, "merged", agent, at),
		ErrInvalidArgument, "record with an out-of-domain state")
	sentinelIs(t, s.RecordStateTransition(ctx, ForgeProviderGitHub, "github.com", "a/b", ForgeArtifactKindIssue, 42, TransitionStateClosed, "", at),
		ErrInvalidArgument, "record with an empty agent")
	sentinelIs(t, s.RecordStateTransition(ctx, ForgeProviderGitHub, "github.com", "a/b", ForgeArtifactKindIssue, 42, TransitionStateClosed, agent, time.Time{}),
		ErrInvalidArgument, "record with a zero timestamp")
	// The FK RESTRICT: a memo can only name a real agent, so a deleted or
	// fabricated account cannot be recorded as an actor.
	sentinelIs(t, s.RecordStateTransition(ctx, ForgeProviderGitHub, "github.com", "a/b", ForgeArtifactKindIssue, 42, TransitionStateClosed, "no-such-agent", at),
		ErrInvalidArgument, "record with an unknown agent")

	_, _, err := s.ConsumeStateTransition(ctx, ForgeProviderUnspecified, "github.com", "a/b", ForgeArtifactKindIssue, 42, TransitionStateClosed, at)
	sentinelIs(t, err, ErrInvalidArgument, "consume with an unspecified provider")
	_, _, err = s.ConsumeStateTransition(ctx, ForgeProviderGitHub, "github.com", "a/b", ForgeArtifactKindUnspecified, 42, TransitionStateClosed, at)
	sentinelIs(t, err, ErrInvalidArgument, "consume with an unspecified kind")
	_, _, err = s.ConsumeStateTransition(ctx, ForgeProviderGitHub, "github.com", "a/b", ForgeArtifactKindIssue, 42, "merged", at)
	sentinelIs(t, err, ErrInvalidArgument, "consume with an out-of-domain state")
}
