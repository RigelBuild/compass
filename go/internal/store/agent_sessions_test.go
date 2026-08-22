//go:build pgtest

package store

// Read-path authorization: RequireAgentSessionSubscriber resolves
// the ownership chain (session_id -> agent_account_id -> home_channel_id) and
// authorizes a caller iff it is a member of that home channel. The load-bearing
// contract is the not-found/forbidden MERGE: an unknown session and a
// known-but-foreign session are refused with the SAME ErrNotFound, so a caller
// holding a foreign session_id cannot probe whether it exists. Pgtest-backed —
// the authz lives in the JOIN, provable only against a real database.

import (
	"context"
	"fmt"
	"testing"
)

// recordSession seeds the ownership chain for agent and returns the session_id a
// subscriber would present. session_id is an opaque handle here (the server
// mints it in production). The chain used to hop through a container_name row;
// 0004 collapsed that hop, so seeding is now the single session write.
func recordSession(t *testing.T, s *Store, agent Account, sessionID string) {
	t.Helper()
	if err := s.RecordAgentSession(t.Context(), sessionID, agent.ID); err != nil {
		t.Fatalf("RecordAgentSession(%q): %v", sessionID, err)
	}
}

// TestRequireAgentSessionSubscriberAuthorizesHomeChannelMember pins the happy
// path AND, by construction, that the resolve walks every hop: the caller is
// authorized only because the full session->agent->home_channel chain resolves
// to a channel it belongs to. The owner is seeded into the agent's home channel
// at CreateAgent. A bug in EITHER join would drop the row and turn this
// legitimate subscribe into an ErrNotFound, reddening this test.
func TestRequireAgentSessionSubscriberAuthorizesHomeChannelMember(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	recordSession(t, s, agent, "sess-1")

	// owner is a member of the agent's home channel (seeded at CreateAgent).
	if err := s.RequireAgentSessionSubscriber(ctx, owner.ID, "sess-1"); err != nil {
		t.Fatalf("home-channel member authorize = %v, want nil (chain resolves + member)", err)
	}
}

// TestRequireAgentSessionSubscriberUnknownSessionNotFound pins the unknown-
// session refusal: a session_id that was never recorded resolves no chain and is
// ErrNotFound.
func TestRequireAgentSessionSubscriberUnknownSessionNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	_ = mustAgent(t, s, owner.ID, "agent") // an agent exists, but no session recorded

	err := s.RequireAgentSessionSubscriber(ctx, owner.ID, "never-recorded")
	sentinelIs(t, err, ErrNotFound, "unknown session subscribe")
}

// TestRequireAgentSessionSubscriberNonMemberSameAsUnknown is the headline
// contract: a caller holding a REAL, existing session_id it does not own is
// refused with the EXACT SAME ErrNotFound as a caller presenting a nonexistent
// session. The two are indistinguishable by error class, so a foreign holder
// cannot use the error to confirm the session exists (D9 not-found/forbidden
// merge). A two-step "resolve then check" that returned a distinct forbidden
// error would leak existence and redden this parity assertion.
func TestRequireAgentSessionSubscriberNonMemberSameAsUnknown(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	outsider := mustUser(t, s, "outsider")
	agent := mustAgent(t, s, owner.ID, "agent")
	recordSession(t, s, agent, "real-session")

	// outsider even owns an unrelated channel — so it IS a member of something,
	// proving membership is checked against the RESOLVED home channel, not any
	// channel. It is still refused on the agent's session.
	_ = mustChannel(t, s, outsider.ID)

	knownForeign := s.RequireAgentSessionSubscriber(ctx, outsider.ID, "real-session")
	sentinelIs(t, knownForeign, ErrNotFound, "known-but-foreign session subscribe")

	unknown := s.RequireAgentSessionSubscriber(ctx, outsider.ID, "does-not-exist")
	sentinelIs(t, unknown, ErrNotFound, "unknown session subscribe")

	// Parity: both refusals are the same sentinel — the foreign holder learns
	// nothing about existence from the error it gets back.
	if (knownForeign == nil) != (unknown == nil) {
		t.Fatalf("existence leak: foreign-session err = %v, unknown-session err = %v (must be indistinguishable)", knownForeign, unknown)
	}
}

// TestRequireAgentSessionSubscriberEmptySessionIDIsInvalidArgument pins the
// input guard (agent_sessions.go:85-86): an empty session_id is rejected as
// ErrInvalidArgument before any DB round trip. It is a distinct error class from
// the not-found/forbidden merge — an empty id is a caller bug (a malformed
// request), not a probe to be masked — so it must NOT collapse into ErrNotFound.
// A future edit that dropped the guard would fall through to the JOIN, which
// returns no rows for "" and surfaces ErrNotFound, reddening this parity check.
func TestRequireAgentSessionSubscriberEmptySessionIDIsInvalidArgument(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")

	err := s.RequireAgentSessionSubscriber(ctx, owner.ID, "")
	sentinelIs(t, err, ErrInvalidArgument, "empty session id subscribe")
}

// TestRequireAgentSessionSubscriberCollapseIsUniformAcrossRefusalCauses is the
// 0004 regression guard for the visibility collapse. Collapsing the chain from
// session -> container -> agent -> home_channel down to
// session -> agent -> home_channel removed a JOIN from a SECURITY query, and
// the risk of that edit is not that a refusal becomes an authorization — it is
// that the refusals stop being IDENTICAL, so the error a caller gets back
// starts discriminating between causes it must not distinguish.
//
// The three tests above each pin one refusal in isolation. This one pins them
// against EACH OTHER: every way of failing to be authorized — the session was
// never recorded, the session exists but belongs to another owner's agent, the
// session exists and the caller is a total stranger to the system — must produce
// one indistinguishable ErrNotFound. If a future rewrite of the JOIN surfaced,
// say, a distinct error once the agent row resolves but membership does not, a
// caller could subtract the two answers and enumerate live session ids. Every
// refusal below is checked against the same sentinel AND against the same
// message, so a divergence in either reddens this test.
func TestRequireAgentSessionSubscriberCollapseIsUniformAcrossRefusalCauses(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	recordSession(t, s, agent, "live-session")

	// Three unauthorized callers, each failing for a structurally DIFFERENT
	// reason, all presenting the same real session id.
	rival := mustUser(t, s, "rival")
	rivalAgent := mustAgent(t, s, rival.ID, "rival-agent") // owns a DIFFERENT agent + home channel
	recordSession(t, s, rivalAgent, "rival-session")
	stranger := mustUser(t, s, "stranger") // member of nothing at all

	refusals := []struct {
		name      string
		sessionID string
		err       error
	}{
		// The session does not exist: no chain resolves.
		{"unknown session", "no-such-session", s.RequireAgentSessionSubscriber(ctx, owner.ID, "no-such-session")},
		// The session exists; the caller is a member of a home channel, just not
		// THIS one. The chain resolves fully and membership is what fails — the
		// case a two-step resolve-then-check would answer differently.
		{"foreign session, caller owns another agent", "live-session", s.RequireAgentSessionSubscriber(ctx, rival.ID, "live-session")},
		// The session exists; the caller belongs to nothing.
		{"foreign session, caller belongs to nothing", "live-session", s.RequireAgentSessionSubscriber(ctx, stranger.ID, "live-session")},
		// Symmetric: the owner probing the RIVAL's session. Proves the refusal is
		// not a property of one privileged caller.
		{"owner probing a rival's session", "rival-session", s.RequireAgentSessionSubscriber(ctx, owner.ID, "rival-session")},
	}

	for _, r := range refusals {
		sentinelIs(t, r.err, ErrNotFound, r.name)
	}

	// Uniformity, the actual contract: every refusal reads identically apart from
	// the session id the caller itself supplied. A message that varied by cause
	// would leak the cause even while the sentinel matched.
	for _, r := range refusals {
		want := fmt.Sprintf("%v: session %q", ErrNotFound, r.sessionID)
		if got := r.err.Error(); got != want {
			t.Fatalf("%s refusal = %q, want %q (every refusal must read identically, echoing only the presented session id)", r.name, got, want)
		}
	}

	// And the collapse must not have cost the happy path: the legitimate owner
	// still resolves through the shortened chain. Without this, a query that
	// refused EVERYTHING would satisfy the uniformity check above.
	if err := s.RequireAgentSessionSubscriber(ctx, owner.ID, "live-session"); err != nil {
		t.Fatalf("owner on its own agent's session = %v, want nil (uniform refusal must not mean universal refusal)", err)
	}
}

// setRecordedAt overwrites a session row's recorded_at_unix_ms directly, so a
// test controls recency ordering deterministically rather than relying on
// wall-clock skew between two RecordAgentSession calls.
func setRecordedAt(t *testing.T, s *Store, sessionID string, ms int64) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE agent_sessions SET recorded_at_unix_ms = $2 WHERE session_id = $1`,
		sessionID, ms,
	); err != nil {
		t.Fatalf("setRecordedAt(%q, %d): %v", sessionID, ms, err)
	}
}

// TestLatestSessionForAccountReturnsMostRecent pins the wake path's recency read:
// with two sessions recorded for one agent at increasing recorded_at, the read
// returns the LATER session_id. The recency column is what disambiguates them —
// without ORDER BY recorded_at_unix_ms DESC the read could return either row, so
// setting the later timestamp on the second and asserting it comes back is the
// teeth.
//
// Mutation: dropping the ORDER BY (or ordering ASC) reddens this — the earlier
// session would come back.
func TestLatestSessionForAccountReturnsMostRecent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	recordSession(t, s, agent, "sess-old")
	setRecordedAt(t, s, "sess-old", 1000)
	recordSession(t, s, agent, "sess-new")
	setRecordedAt(t, s, "sess-new", 2000)

	sessionID, ok, err := s.LatestSessionForAccount(ctx, agent.ID)
	if err != nil {
		t.Fatalf("LatestSessionForAccount = %v, want nil", err)
	}
	if !ok {
		t.Fatal("LatestSessionForAccount ok = false, want true (the agent has recorded sessions)")
	}
	if sessionID != "sess-new" {
		t.Fatalf("LatestSessionForAccount = %q, want %q (the most-recently-recorded session)", sessionID, "sess-new")
	}
}

// TestLatestSessionForAccountNeverRecordedIsNotOk pins the no-prior-session
// signal the wake path branches on: an agent that has never had a session
// recorded returns ok=false and no error (not ErrNotFound), so the caller falls
// to the fresh-start path rather than treating it as a fault.
//
// Mutation: surfacing noRows as an error instead of ("", false, nil) reddens the
// "no error" assertion and would make the wake log outcome=failed for a
// never-started agent.
func TestLatestSessionForAccountNeverRecordedIsNotOk(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	sessionID, ok, err := s.LatestSessionForAccount(ctx, agent.ID)
	if err != nil {
		t.Fatalf("LatestSessionForAccount(never-recorded) = %v, want nil error", err)
	}
	if ok {
		t.Fatalf("LatestSessionForAccount(never-recorded) ok = true (session %q), want false", sessionID)
	}
}
