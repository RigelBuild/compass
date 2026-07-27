//go:build pgtest

package store

// Read-path authorization: RequireAgentSessionSubscriber resolves
// the ownership chain (session_id -> container_name -> agent_account_id ->
// home_channel_id) and authorizes a caller iff it is a member of that home
// channel. The load-bearing contract is the not-found/forbidden MERGE: an
// unknown session and a known-but-foreign session are refused with the SAME
// ErrNotFound, so a caller holding a foreign session_id cannot probe whether it
// exists. Pgtest-backed — the authz lives in the JOIN, provable only against a
// real database.

import (
	"context"
	"testing"
)

// recordChain seeds a full ownership chain for agent and returns the session_id
// a subscriber would present. container_name and session_id are opaque handles
// here (the server mints them in production).
func recordChain(t *testing.T, s *Store, agent Account, container, sessionID string) {
	t.Helper()
	ctx := context.Background()
	if err := s.RecordAgentContainer(ctx, container, agent.ID); err != nil {
		t.Fatalf("RecordAgentContainer(%q): %v", container, err)
	}
	if err := s.RecordAgentSession(ctx, sessionID, container); err != nil {
		t.Fatalf("RecordAgentSession(%q): %v", sessionID, err)
	}
}

// TestRequireAgentSessionSubscriberAuthorizesHomeChannelMember pins the happy
// path AND, by construction, that the resolve walks all three hops: the caller
// is authorized only because the full session->container->agent->home_channel
// chain resolves to a channel it belongs to. The owner is seeded into the
// agent's home channel at CreateAgent. A bug in ANY of the three joins would
// drop the row and turn this legitimate subscribe into an ErrNotFound, reddening
// this test.
func TestRequireAgentSessionSubscriberAuthorizesHomeChannelMember(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	recordChain(t, s, agent, "cont-1", "sess-1")

	// owner is a member of the agent's home channel (seeded at CreateAgent).
	if err := s.RequireAgentSessionSubscriber(ctx, owner.ID, "sess-1"); err != nil {
		t.Fatalf("home-channel member authorize = %v, want nil (full chain resolves + member)", err)
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
	recordChain(t, s, agent, "cont-1", "real-session")

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
