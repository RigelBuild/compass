package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// GatewayCaller is the identity a live gateway token resolves to.
type GatewayCaller struct {
	AgentAccountID AccountID
	OwnerUserID    AccountID
}

// RotateGatewayToken stores hash as agentID's only live gateway token, revoking
// any earlier one in the same transaction. The agent must be visible in ctx's
// tenant; otherwise it is ErrNotFound.
func (s *Store) RotateGatewayToken(ctx context.Context, agentID AccountID, hash [32]byte) (err error) {
	if agentID == "" {
		return fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return fmt.Errorf("store: begin rotate gateway token: %w", err)
	}
	defer func() { err = errors.Join(err, rollbackUnlessDone(ctx, tx)) }()
	qtx := s.q.WithTx(tx)

	agent, err := lockGatewayTokenAgent(ctx, qtx, agentID)
	if err != nil {
		return err
	}
	if _, err := qtx.RevokeLiveGatewayToken(ctx, db.RevokeLiveGatewayTokenParams{
		AgentAccountID: string(agentID), TenantID: agent.TenantID,
	}); err != nil {
		return fmt.Errorf("store: revoke prior gateway token: %w", err)
	}
	if err := qtx.InsertGatewayToken(ctx, db.InsertGatewayTokenParams{
		Hash: hash[:], AgentAccountID: string(agentID), OwnerUserID: agent.OwnerUserID, TenantID: agent.TenantID,
	}); err != nil {
		// Only a hash collision is a conflict the caller can retry with a fresh token.
		if pgErrIs(err, pgUniqueViolation) && pgConstraintName(err) == "gateway_tokens_pkey" {
			return fmt.Errorf("%w: gateway token hash already stored", ErrConflict)
		}
		return fmt.Errorf("store: insert gateway token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit rotate gateway token: %w", err)
	}
	return nil
}

// RevokeGatewayToken revokes agentID's live gateway token. Idempotent: an agent
// with no live token succeeds. An agent not visible in ctx's tenant is ErrNotFound.
func (s *Store) RevokeGatewayToken(ctx context.Context, agentID AccountID) (err error) {
	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return fmt.Errorf("store: begin revoke gateway token: %w", err)
	}
	defer func() { err = errors.Join(err, rollbackUnlessDone(ctx, tx)) }()
	qtx := s.q.WithTx(tx)

	agent, err := lockGatewayTokenAgent(ctx, qtx, agentID)
	if err != nil {
		return err
	}
	if _, err := qtx.RevokeLiveGatewayToken(ctx, db.RevokeLiveGatewayTokenParams{
		AgentAccountID: string(agentID), TenantID: agent.TenantID,
	}); err != nil {
		return fmt.Errorf("store: revoke gateway token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit revoke gateway token: %w", err)
	}
	return nil
}

// ResolveGatewayToken returns the caller a live token hash belongs to. Unknown
// and revoked hashes are both ErrNotFound, so callers cannot tell them apart.
func (s *Store) ResolveGatewayToken(ctx context.Context, hash [32]byte) (GatewayCaller, error) {
	row, err := s.q.ResolveLiveGatewayToken(ctx, hash[:])
	if err != nil {
		if noRows(err) {
			return GatewayCaller{}, fmt.Errorf("%w: gateway token", ErrNotFound)
		}
		return GatewayCaller{}, fmt.Errorf("store: resolve gateway token: %w", err)
	}
	return GatewayCaller{AgentAccountID: AccountID(row.AgentAccountID), OwnerUserID: AccountID(row.OwnerUserID)}, nil
}

// lockGatewayTokenAgent locks the agent row so concurrent rotations serialize.
func lockGatewayTokenAgent(ctx context.Context, q *db.Queries, agentID AccountID) (db.LockGatewayTokenAgentRow, error) {
	agent, err := q.LockGatewayTokenAgent(ctx, string(agentID))
	if err != nil {
		if noRows(err) {
			return db.LockGatewayTokenAgentRow{}, fmt.Errorf("%w: agent %q", ErrNotFound, agentID)
		}
		return db.LockGatewayTokenAgentRow{}, fmt.Errorf("store: lock agent for gateway token: %w", err)
	}
	return agent, nil
}

// rollbackUnlessDone rolls tx back, ignoring the expected error after a commit.
func rollbackUnlessDone(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return fmt.Errorf("store: rollback: %w", err)
	}
	return nil
}
