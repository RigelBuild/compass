package store

import (
	"context"
	"fmt"
)

// The durable session-ownership chain: the persistent
// session_id -> container_name -> agent_account_id -> home_channel_id mapping
// that SubscribeAgentSession resolves to authorize a subscriber. It is rooted
// non-spoofably — agent_account_id is a request field, but container_name and
// session_id are server-minted response values recorded only after the Runner
// call succeeds (agent_sessions.sql / 0003_agent_ownership.sql). The rows
// survive a Server restart, so the authz boundary does not depend on the
// in-memory RunnerHub enrollment.

// RecordAgentContainer persists the container_name -> agent_account_id mapping
// at ProvisionAgentWorkspace, where the agent identity is known (the request
// field) and container_name is the server-minted response. It is idempotent:
// one container belongs to exactly one agent and never changes owner, so a
// client_request_id retry that returns the same container_name re-records the
// same row as a no-op (ON CONFLICT DO NOTHING) rather than an ErrConflict. An
// unknown agent_account_id is ErrInvalidArgument (the FK).
func (s *Store) RecordAgentContainer(ctx context.Context, containerName string, agentAccountID AccountID) error {
	if containerName == "" {
		return fmt.Errorf("%w: container name is required", ErrInvalidArgument)
	}
	if agentAccountID == "" {
		return fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO agent_containers (container_name, agent_account_id)
		 VALUES ($1, $2)
		 ON CONFLICT (container_name) DO NOTHING`,
		containerName, string(agentAccountID),
	); err != nil {
		if pgErrIs(err, pgForeignKeyViolation) {
			return fmt.Errorf("%w: agent account %q does not exist", ErrInvalidArgument, agentAccountID)
		}
		return fmt.Errorf("store: record agent container: %w", err)
	}
	return nil
}

// RecordAgentSession persists the session_id -> container_name mapping at
// StartAgentSession, where container_name is the request handle and session_id
// is the server-minted response. The FK to agent_containers means a session
// cannot bind to a container the Server never provisioned — the chain is
// complete or it does not exist. An unknown container_name is
// ErrInvalidArgument (the FK); a re-used session_id is ErrConflict.
func (s *Store) RecordAgentSession(ctx context.Context, sessionID, containerName string) error {
	if sessionID == "" {
		return fmt.Errorf("%w: session id is required", ErrInvalidArgument)
	}
	if containerName == "" {
		return fmt.Errorf("%w: container name is required", ErrInvalidArgument)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO agent_sessions (session_id, container_name) VALUES ($1, $2)`,
		sessionID, containerName,
	); err != nil {
		if pgErrIs(err, pgUniqueViolation) {
			return fmt.Errorf("%w: session %q already recorded", ErrConflict, sessionID)
		}
		if pgErrIs(err, pgForeignKeyViolation) {
			return fmt.Errorf("%w: container %q was not provisioned", ErrInvalidArgument, containerName)
		}
		return fmt.Errorf("store: record agent session: %w", err)
	}
	return nil
}

// RequireAgentSessionSubscriber is the read-path authorization primitive for
// SubscribeAgentSession — the streaming sibling of requireChannelMember. In ONE
// query it resolves the ownership chain (session_id -> container_name ->
// agent_account_id -> home_channel_id) and checks the caller's membership on
// that home channel, returning ErrNotFound for BOTH an unknown session_id AND a
// non-member caller. Merging the two into one indistinguishable error is
// load-bearing: it must not leak session existence to a caller who holds a
// foreign session_id — neither via the error class NOR via timing skew. A
// two-step shape (resolve, then separately check membership) would take one
// round-trip for an unknown session but two for a known-but-foreign one, so the
// latency itself would distinguish "does not exist" from "exists but
// forbidden". The single EXISTS query below is constant-shape: it returns true
// only when the session exists AND the caller is a member, and every other
// outcome is the same false -> the same ErrNotFound in the same one round-trip.
// The caller (SubscribeAgentSession) maps ErrNotFound to its stream rejection,
// so a foreign or unknown session is refused identically and enumerates nothing
// (the not-found/forbidden merge, D9).
func (s *Store) RequireAgentSessionSubscriber(ctx context.Context, caller AccountID, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("%w: session id is required", ErrInvalidArgument)
	}
	var authorized bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
		          SELECT 1
		            FROM agent_sessions se
		            JOIN agent_containers c ON c.container_name = se.container_name
		            JOIN agent_accounts ag ON ag.account_id = c.agent_account_id
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
