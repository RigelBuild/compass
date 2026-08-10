package store

import (
	"context"
	"fmt"
)

// The durable session-ownership chain: the persistent
// session_id -> agent_account_id -> home_channel_id mapping that
// SubscribeAgentSession resolves to authorize a subscriber. It is rooted
// non-spoofably — session_id is a server-minted response value recorded only
// after the Runner call succeeds (0001_init.sql, agent_sessions/agent_placements).
// The rows survive a Server restart, so the authz boundary does not depend on
// the in-memory RunnerHub enrollment.
//
// The chain used to hop through a container_name row (a since-squashed migration).
// That hop was removed on a SCHEMA fact, not a naming one: in the old
// agent_containers, container_name was the PRIMARY KEY and agent_account_id a
// NOT NULL FK to the account, so the hop was a provable 1:1 pass-through — it
// could resolve exactly one account for a name, and never fewer. Removing it
// therefore authorizes the identical set of (session, caller) pairs; only the
// table count differs. That argument holds whatever container names look like,
// which is why it is the one the collapse rests on.
//
// Secondarily, on where a container name comes from at all: it is derived from
// the agent account (internal/runner/spec.go BuildSpec, NamePrefix + accountID),
// so nothing is lost by not storing it — where one is genuinely needed it is
// recomputed. But that is a Runner-side convention the Server never enforces,
// so it is a remark, not the justification.

// RecordAgentSession persists the session_id -> agent_account_id mapping at
// StartAgentSession, where session_id is the server-minted response and the
// agent account is the one the started session belongs to. An unknown
// agent_account_id is ErrInvalidArgument (the FK); a re-used session_id is
// ErrConflict.
func (s *Store) RecordAgentSession(ctx context.Context, sessionID string, agentAccountID AccountID) error {
	if sessionID == "" {
		return fmt.Errorf("%w: session id is required", ErrInvalidArgument)
	}
	if agentAccountID == "" {
		return fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO agent_sessions (session_id, agent_account_id) VALUES ($1, $2)`,
		sessionID, string(agentAccountID),
	); err != nil {
		if pgErrIs(err, pgUniqueViolation) {
			return fmt.Errorf("%w: session %q already recorded", ErrConflict, sessionID)
		}
		if pgErrIs(err, pgForeignKeyViolation) {
			return fmt.Errorf("%w: agent account %q does not exist", ErrInvalidArgument, agentAccountID)
		}
		return fmt.Errorf("store: record agent session: %w", err)
	}
	return nil
}

// RequireAgentSessionSubscriber is the read-path authorization primitive for
// SubscribeAgentSession — the streaming sibling of requireChannelMember. In ONE
// query it resolves the ownership chain (session_id -> agent_account_id ->
// home_channel_id) and checks the caller's membership on that home channel,
// returning ErrNotFound for BOTH an unknown session_id AND a non-member caller.
// Merging the two into one indistinguishable error is load-bearing: it must not
// leak session existence to a caller who holds a foreign session_id — neither
// via the error class NOR via timing skew. A two-step shape (resolve, then
// separately check membership) would take one round-trip for an unknown session
// but two for a known-but-foreign one, so the latency itself would distinguish
// "does not exist" from "exists but forbidden". The single EXISTS query below is
// constant-shape: it returns true only when the session exists AND the caller is
// a member, and every other outcome is the same false -> the same ErrNotFound in
// the same one round-trip. The caller (SubscribeAgentSession) maps ErrNotFound to
// its stream rejection, so a foreign or unknown session is refused identically
// and enumerates nothing (the not-found/forbidden merge, D9).
func (s *Store) RequireAgentSessionSubscriber(ctx context.Context, caller AccountID, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("%w: session id is required", ErrInvalidArgument)
	}
	var authorized bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
		          SELECT 1
		            FROM agent_sessions se
		            JOIN agent_accounts ag ON ag.account_id = se.agent_account_id
		            JOIN channel_members cm ON cm.channel_id = ag.home_channel_id
		                                   AND cm.account_id = $2
		           WHERE se.session_id = $1)`,
		sessionID, string(caller),
	).Scan(&authorized); err != nil {
		return fmt.Errorf("store: authorize agent session subscriber: %w", err)
	}
	if !authorized {
		// Unknown session_id OR non-member caller — one indistinguishable
		// ErrNotFound, so a caller holding a foreign session_id cannot tell
		// "exists but forbidden" from "does not exist".
		return fmt.Errorf("%w: session %q", ErrNotFound, sessionID)
	}
	return nil
}
