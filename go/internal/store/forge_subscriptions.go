package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The DL-053 agent-notification subscription writer (RIG-2732 Piece 1, design
// docs/designs/product/compass-notification-delivery/design.md): the Server-side
// Postgres row that records an agent's standing interest in one forge artifact.
// agent_forge_subscriptions is the per-subscriber DELIVERY-cursor table
// (delivered_revision/delivered_at); forge_artifact_cursors is the shared
// per-artifact FETCH cursor the poll driver writes. The GC invariant here is the
// only place this slice touches forge_artifact_cursors: when the LAST
// subscription for a coordinate is deleted, its cursor row is collected in the
// same transaction (DL-053). The poll driver owns the cursor WRITER (Piece 2);
// this file never inserts a cursor row.

// AgentForgeSubscription is one row of agent_forge_subscriptions: an agent's
// standing interest in one forge artifact coordinate, plus that subscriber's
// per-artifact DELIVERY cursor (DeliveredRevision/DeliveredAt — the last
// revision this agent was notified of, distinct from the shared FETCH cursor on
// forge_artifact_cursors). Mirrors ForgeRepoSubscription's field style
// (forge_cursors.go).
type AgentForgeSubscription struct {
	ID             string
	AgentAccountID AccountID
	Provider       ForgeProvider // GITHUB(1)/GITLAB(2)/FORGEJO(3)/LINEAR(4); never 0
	Host           string
	Repo           string
	Kind           ForgeArtifactKind // issue(1)/pull_request(2); never 0
	Number         uint64

	DeliveredRevision string
	DeliveredAt       *time.Time
	CreatedAt         time.Time
}

// validSubscriptionCoordinate rejects the zero/empty coordinate fields
// EnsureAgentForgeSubscription guards on before any DB round trip: the
// provider/host/repo triple (via validCoordinate), a zero kind (never
// UNSPECIFIED(0), the CHECK's job in Go space), and a zero artifact number. A
// caller bug is ErrInvalidArgument.
func validSubscriptionCoordinate(provider ForgeProvider, host, repo string, kind ForgeArtifactKind, number uint64) error {
	if err := validCoordinate(provider, host, repo); err != nil {
		return err
	}
	if kind != ForgeArtifactKindIssue && kind != ForgeArtifactKindPullRequest {
		return fmt.Errorf("%w: artifact kind must be issue or pull_request", ErrInvalidArgument)
	}
	if number == 0 {
		return fmt.Errorf("%w: artifact number is required", ErrInvalidArgument)
	}
	return nil
}

// EnsureAgentForgeSubscription idempotently inserts the agent's subscription to
// one artifact coordinate, keyed by the UNIQUE (agent_account_id, provider,
// host, repo, kind, number). A repeat subscribe by the same agent to the same
// artifact returns the EXISTING subscription id and creates no duplicate row —
// the DO UPDATE is a no-op touch (re-setting agent_account_id to itself) that
// makes RETURNING fire on the conflict path so a repeat returns the stored id,
// not a fresh one. A new coordinate mints a fresh id via newID(). Zero/empty
// coordinate fields / a zero kind / a zero number -> ErrInvalidArgument; an
// unknown agent (the FK RESTRICT) -> ErrInvalidArgument.
func (s *Store) EnsureAgentForgeSubscription(ctx context.Context, sub AgentForgeSubscription) (string, error) {
	if err := validSubscriptionCoordinate(sub.Provider, sub.Host, sub.Repo, sub.Kind, sub.Number); err != nil {
		return "", err
	}
	if sub.AgentAccountID == "" {
		return "", fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	var id string
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO agent_forge_subscriptions
		     (id, agent_account_id, forge_provider, forge_host, repo, kind, number)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (agent_account_id, forge_provider, forge_host, repo, kind, number) DO UPDATE
		    SET agent_account_id = EXCLUDED.agent_account_id
		 RETURNING id`,
		newID(), string(sub.AgentAccountID), int32(sub.Provider), sub.Host, sub.Repo,
		int32(sub.Kind), int64(sub.Number), //nolint:gosec // G115: number is a canonical forge artifact number (a positive issue/PR number) written to a BIGINT, always well within the int64 domain.
	).Scan(&id); err != nil {
		if pgErrIs(err, pgForeignKeyViolation) {
			return "", fmt.Errorf("%w: unknown agent %q", ErrInvalidArgument, sub.AgentAccountID)
		}
		return "", fmt.Errorf("store: ensure agent forge subscription: %w", err)
	}
	return id, nil
}

// DeleteAgentForgeSubscription removes the agent's subscription by id, scoped to
// the calling agent (an agent cannot delete another agent's subscription — the
// WHERE clause matches on both id AND agent). Zero rows deleted (an unknown id,
// or an id owned by a different agent, indistinguishable by design) -> ErrNotFound.
//
// The delete runs the DL-053 garbage-collection invariant in ONE transaction:
// after removing the subscription row, its coordinate's forge_artifact_cursors
// row is collected IFF that was the last subscription for the coordinate — the
// NOT EXISTS guard leaves the cursor in place if any other agent still
// subscribes to the same artifact. The coordinate is taken from the deleted
// row's RETURNING, so the GC targets exactly the artifact whose last subscriber
// just left.
func (s *Store) DeleteAgentForgeSubscription(ctx context.Context, agent AccountID, subscriptionID string) error {
	if agent == "" {
		return fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	if subscriptionID == "" {
		return fmt.Errorf("%w: subscription id is required", ErrInvalidArgument)
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var (
			provider int32
			host     string
			repo     string
			kind     int32
			number   int64
		)
		if err := tx.QueryRow(ctx,
			`DELETE FROM agent_forge_subscriptions
			  WHERE id = $1 AND agent_account_id = $2
			 RETURNING forge_provider, forge_host, repo, kind, number`,
			subscriptionID, string(agent),
		).Scan(&provider, &host, &repo, &kind, &number); err != nil {
			if noRows(err) {
				return fmt.Errorf("%w: subscription %q", ErrNotFound, subscriptionID)
			}
			return fmt.Errorf("store: delete agent forge subscription: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM forge_artifact_cursors
			  WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3 AND kind = $4 AND number = $5
			    AND NOT EXISTS (
			        SELECT 1 FROM agent_forge_subscriptions
			         WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3 AND kind = $4 AND number = $5
			    )`,
			provider, host, repo, kind, number,
		); err != nil {
			return fmt.Errorf("store: garbage-collect forge artifact cursor: %w", err)
		}
		return nil
	})
}

// AgentForgeSubscriptionsForArtifact counts the subscriptions currently held on
// one artifact coordinate — the minimal reader the GC test needs to assert the
// last-subscription-deletes-cursor invariant. It is NOT the poll driver's
// subscriber-enumeration reader (Piece 2); it returns only the row count.
// Zero/empty coordinate fields / a zero kind / a zero number -> ErrInvalidArgument.
func (s *Store) AgentForgeSubscriptionsForArtifact(ctx context.Context, provider ForgeProvider, host, repo string, kind ForgeArtifactKind, number uint64) (int, error) {
	if err := validSubscriptionCoordinate(provider, host, repo, kind, number); err != nil {
		return 0, err
	}
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_forge_subscriptions
		  WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3 AND kind = $4 AND number = $5`,
		int32(provider), host, repo, int32(kind), int64(number), //nolint:gosec // G115: number is a canonical forge artifact number written to a BIGINT, always within the int64 domain.
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count agent forge subscriptions for artifact: %w", err)
	}
	return n, nil
}
