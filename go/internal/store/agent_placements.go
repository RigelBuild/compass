package store

import (
	"context"
	"fmt"
)

// Agent placement: the durable record of WHERE each agent runs — which Runner,
// under which container name — written at ProvisionAgentWorkspace, the one hop
// where every fact is in hand (agent_account_id is the Server's own request
// field, container_name the Runner's response, runner_id the Runner it relayed
// to). Before 0004 the Server learned that triple and kept it only in RAM (the
// RunnerHub's in-memory container->account binding), so a Server restart or a
// Runner re-enroll lost it.
//
// Placement is NOT authorization. SubscribeAgentSession authorizes through
// agent_sessions -> agent_accounts -> channel_members and never reads this
// table; keeping the two apart is what stops the container hop 0003 introduced
// from growing back into the security boundary. What placement is for is the
// two reads below: StartAgentSession resolving the account that owns an incoming
// container_name, and reattach recovery (SEA-1516) naming every agent stranded
// by a Runner restart.

// AgentPlacement is one agent's placement: the Runner it runs on and the
// container name it runs under. Returned by ListAgentPlacementsForRunner, which
// is the reattach read — re-driving Provision needs both fields.
type AgentPlacement struct {
	AgentAccountID AccountID
	RunnerID       string
	ContainerName  string
}

// RecordAgentPlacement persists where an agent is placed, at
// ProvisionAgentWorkspace. It is an UPSERT keyed on the agent, because an agent
// is on at most one Runner under one container name: a re-provision REPLACES the
// placement, updating runner_id and container_name TOGETHER so a row can never
// pair a fresh Runner with the name from a previous one. That also makes the
// call idempotent under the client_request_id provision-retry contract — a retry
// rewrites the same values rather than conflicting.
//
// An unknown agent_account_id is ErrInvalidArgument (the FK).
//
// A container_name already placed for a DIFFERENT agent is ErrConflict (the
// unique index) — but that is a GUARD, not a live safety property today, and
// the distinction matters for anyone changing either side. Placements are
// PERMANENT for now: nothing anywhere deletes a row, so a name is never
// released. And container names are DERIVED, not allocated: BuildSpec computes
// NamePrefix + accountID (internal/runner/spec.go:73), so two distinct agents
// cannot produce the same name and the conflict is unreachable in production.
// The unique index is therefore leaning on that derivation. If name derivation
// ever changes — a Runner-scoped prefix, a random suffix, an operator-supplied
// name — the conflict becomes reachable, and a re-provision that yields a new
// name for the same agent would leave the OLD name owned forever with no
// release path: that change needs a delete path added alongside it.
func (s *Store) RecordAgentPlacement(ctx context.Context, agentAccountID AccountID, runnerID, containerName string) error {
	if agentAccountID == "" {
		return fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	if runnerID == "" {
		return fmt.Errorf("%w: runner id is required", ErrInvalidArgument)
	}
	if containerName == "" {
		return fmt.Errorf("%w: container name is required", ErrInvalidArgument)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO agent_placements (agent_account_id, runner_id, container_name)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (agent_account_id) DO UPDATE
		    SET runner_id      = EXCLUDED.runner_id,
		        container_name = EXCLUDED.container_name,
		        updated_at     = now()`,
		string(agentAccountID), runnerID, containerName,
	); err != nil {
		if pgErrIs(err, pgForeignKeyViolation) {
			return fmt.Errorf("%w: agent account %q does not exist", ErrInvalidArgument, agentAccountID)
		}
		if pgErrIs(err, pgUniqueViolation) {
			return fmt.Errorf("%w: container %q is already placed for another agent", ErrConflict, containerName)
		}
		return fmt.Errorf("store: record agent placement: %w", err)
	}
	return nil
}

// AgentForContainer resolves the agent account a placed container belongs to.
// StartAgentSession is the caller: its request carries only container_name (the
// frozen StartAgentSessionRequest field), so this is how it learns whose session
// it is about to record. Reading it from the durable placement rather than an
// in-memory binding is what makes the ownership record survive a Server restart
// or a Runner re-enroll between Provision and Start.
//
// An unplaced container is ErrNotFound — the Server never provisioned it (or
// the placement was superseded), so no session may bind to it.
func (s *Store) AgentForContainer(ctx context.Context, containerName string) (AccountID, error) {
	if containerName == "" {
		return "", fmt.Errorf("%w: container name is required", ErrInvalidArgument)
	}
	var accountID string
	if err := s.pool.QueryRow(ctx,
		`SELECT agent_account_id FROM agent_placements WHERE container_name = $1`,
		containerName,
	).Scan(&accountID); err != nil {
		if noRows(err) {
			return "", fmt.Errorf("%w: container %q is not placed", ErrNotFound, containerName)
		}
		return "", fmt.Errorf("store: resolve agent for container: %w", err)
	}
	return AccountID(accountID), nil
}

// ListAgentPlacementsForRunner returns every agent placed on runnerID. This is
// the reattach read (SEA-1516): after a Runner restart its surviving containers
// are orphaned, and the Server re-drives Provision for exactly this set — which
// is why each row carries the container name and not just the account. Ordered
// by agent account id so a recovery pass is deterministic and its logs diffable.
// A Runner with no placements yields an empty slice, not an error: nothing to
// reattach is a normal outcome, not a failure.
func (s *Store) ListAgentPlacementsForRunner(ctx context.Context, runnerID string) ([]AgentPlacement, error) {
	if runnerID == "" {
		return nil, fmt.Errorf("%w: runner id is required", ErrInvalidArgument)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT agent_account_id, runner_id, container_name
		   FROM agent_placements
		  WHERE runner_id = $1
		  ORDER BY agent_account_id`,
		runnerID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list agent placements: %w", err)
	}
	defer rows.Close()

	placements := []AgentPlacement{}
	for rows.Next() {
		var p AgentPlacement
		var accountID string
		if err := rows.Scan(&accountID, &p.RunnerID, &p.ContainerName); err != nil {
			return nil, fmt.Errorf("store: scan agent placement: %w", err)
		}
		p.AgentAccountID = AccountID(accountID)
		placements = append(placements, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate agent placements: %w", err)
	}
	return placements, nil
}
