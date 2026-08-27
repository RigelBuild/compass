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
// per-artifact FETCH cursor. This file owns two writers/readers of that shared
// table: UpsertForgeArtifactCursor (the FETCH-cursor WRITER — INSERT ... ON
// CONFLICT DO UPDATE, keyed by the coordinate PK) and ListForgeNotifyTargets
// (the notify-target READER — LEFT JOINs the cursor onto each subscribed
// coordinate). The GC invariant also lives here: when the LAST subscription for
// a coordinate is deleted, its cursor row is collected in the same transaction
// (DL-053).

// ForgeSubscriptionScope mirrors compass.v1 ForgeSubscriptionScope
// (UNSPECIFIED=0, ARTIFACT=1, CONTAINER=2; RIG-2732 T3, OQ-1 ruled (i)). It
// discriminates a subscription to one issue/PR (ARTIFACT: number > 0) from a
// subscription to a whole container (CONTAINER: number == 0 — the whole repo on
// GitHub, a PROJECT on Linear). The wire UNSPECIFIED zero is treated as ARTIFACT
// for pre-scope callers; the store never persists 0 (the CHECK is scope IN
// (1, 2)), so the writer normalizes UNSPECIFIED -> ARTIFACT before insert.
type ForgeSubscriptionScope int32

const (
	ForgeSubscriptionScopeUnspecified ForgeSubscriptionScope = 0
	ForgeSubscriptionScopeArtifact    ForgeSubscriptionScope = 1
	ForgeSubscriptionScopeContainer   ForgeSubscriptionScope = 2
)

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

	// Scope discriminates ARTIFACT (one issue/PR; Number > 0, Project empty)
	// from CONTAINER (the whole repo on GitHub / a Linear PROJECT; Number == 0).
	// The zero value (UNSPECIFIED) is normalized to ARTIFACT by the writer.
	Scope   ForgeSubscriptionScope
	Project string // Linear CONTAINER rows: the project id; else ""

	DeliveredRevision string
	DeliveredAt       *time.Time
	CreatedAt         time.Time
}

// normalizeScope maps the wire UNSPECIFIED(0) zero to ARTIFACT — pre-scope
// callers subscribe to an artifact — so the store never persists scope 0 (the
// CHECK is scope IN (1, 2)).
func normalizeScope(scope ForgeSubscriptionScope) ForgeSubscriptionScope {
	if scope == ForgeSubscriptionScopeUnspecified {
		return ForgeSubscriptionScopeArtifact
	}
	return scope
}

// validSubscriptionCoordinate rejects the zero/empty coordinate fields
// EnsureAgentForgeSubscription guards on before any DB round trip: the
// provider/host/repo triple (via validCoordinate) and a zero kind (never
// UNSPECIFIED(0), the CHECK's job in Go space) fail on EVERY scope arm. The
// scope discriminator then decides the number/project shape (RIG-2732 T3):
//   - ARTIFACT: number REQUIRED (> 0) AND project MUST be empty (the W2
//     silent-misfire class — a zero number under ARTIFACT stays a caller bug).
//   - CONTAINER: number MUST be 0 (the whole container, no artifact number);
//     project REQUIRED on LINEAR (which container/project) and FORBIDDEN on any
//     non-Linear forge (GitHub's container is the whole repo, no project id).
//
// A caller bug is ErrInvalidArgument. scope is taken pre-normalized so an
// explicit CONTAINER is distinguishable from the ARTIFACT default.
func validSubscriptionCoordinate(provider ForgeProvider, host, repo string, kind ForgeArtifactKind, number uint64, scope ForgeSubscriptionScope, project string) error {
	if err := validCoordinate(provider, host, repo); err != nil {
		return err
	}
	if kind != ForgeArtifactKindIssue && kind != ForgeArtifactKindPullRequest {
		return fmt.Errorf("%w: artifact kind must be issue or pull_request", ErrInvalidArgument)
	}
	switch normalizeScope(scope) {
	case ForgeSubscriptionScopeArtifact:
		if number == 0 {
			return fmt.Errorf("%w: artifact number is required", ErrInvalidArgument)
		}
		if project != "" {
			return fmt.Errorf("%w: project must be empty for an artifact subscription", ErrInvalidArgument)
		}
	case ForgeSubscriptionScopeContainer:
		if number != 0 {
			return fmt.Errorf("%w: number must be 0 for a container subscription", ErrInvalidArgument)
		}
		if provider == ForgeProviderLinear {
			if project == "" {
				return fmt.Errorf("%w: project is required for a Linear container subscription", ErrInvalidArgument)
			}
		} else if project != "" {
			return fmt.Errorf("%w: project is forbidden for a non-Linear container subscription", ErrInvalidArgument)
		}
	default:
		return fmt.Errorf("%w: unknown subscription scope %d", ErrInvalidArgument, scope)
	}
	return nil
}

// EnsureAgentForgeSubscription idempotently inserts the agent's subscription to
// one coordinate, keyed by the UNIQUE (agent_account_id, provider, host, repo,
// kind, number, project). A repeat subscribe by the same agent to the same
// artifact/container returns the EXISTING subscription id and creates no
// duplicate row — the DO UPDATE is a no-op touch (re-setting agent_account_id to
// itself) that makes RETURNING fire on the conflict path so a repeat returns the
// stored id, not a fresh one. A new coordinate mints a fresh id via newID().
// scope is normalized (UNSPECIFIED -> ARTIFACT) before insert so the CHECK
// (scope IN (1,2)) always holds; the guard enforces the number/project shape per
// scope. Zero/empty coordinate fields / a zero kind / a scope-shape violation ->
// ErrInvalidArgument; an unknown agent (the FK RESTRICT) -> ErrInvalidArgument.
func (s *Store) EnsureAgentForgeSubscription(ctx context.Context, sub AgentForgeSubscription) (string, error) {
	if err := validSubscriptionCoordinate(sub.Provider, sub.Host, sub.Repo, sub.Kind, sub.Number, sub.Scope, sub.Project); err != nil {
		return "", err
	}
	if sub.AgentAccountID == "" {
		return "", fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	var id string
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO agent_forge_subscriptions
		     (id, agent_account_id, forge_provider, forge_host, repo, kind, number, scope, project)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 ON CONFLICT (agent_account_id, forge_provider, forge_host, repo, kind, number, project) DO UPDATE
		    SET agent_account_id = EXCLUDED.agent_account_id
		 RETURNING id`,
		newID(), string(sub.AgentAccountID), int32(sub.Provider), sub.Host, sub.Repo,
		int32(sub.Kind), int64(sub.Number), //nolint:gosec // G115: number is a canonical forge artifact number (a positive issue/PR number, or 0 for a container) written to a BIGINT, always well within the int64 domain.
		int32(normalizeScope(sub.Scope)), sub.Project,
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
	if err := validSubscriptionCoordinate(provider, host, repo, kind, number, ForgeSubscriptionScopeArtifact, ""); err != nil {
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

// ForgeNotifySubscriber is one subscriber the notify path fans a change out to:
// the subscription id (the ack correlation key), the owning agent, that
// subscriber's last-notified DeliveredRevision (the router suppresses a
// re-notify when the change's revision equals it), and — for a collapsed
// container target whose subscribers span multiple Linear projects — the
// subscriber's own Project, so the router matches a project-P change to only
// its project-P subscribers ("" for artifact/GitHub subs). Struct shape frozen
// by the design record (RIG-2732 T3, §ListForgeNotifyTargets).
type ForgeNotifySubscriber struct {
	SubscriptionID    string
	AgentAccountID    AccountID
	DeliveredRevision string
	Project           string
}

// ForgeArtifactCursor is one row of forge_artifact_cursors: the shared
// per-artifact FETCH cursor (conditional-GET ETags + the last observed snapshot
// + its revision digest). Number == 0 is the container-scope reconcile cursor
// row (one per (repo, kind); the table admits number=0, project-less). Snapshot
// is the raw JSONB (nil when never stored). UpsertForgeArtifactCursor (this
// file) is the writer of these rows.
type ForgeArtifactCursor struct {
	Provider                 ForgeProvider
	Host, Repo               string
	Kind                     ForgeArtifactKind
	Number                   uint64 // 0 = the container-scope reconcile cursor row
	ETag                     string
	CommentsETag, ChecksETag string
	Revision                 string
	Snapshot                 []byte // raw JSONB
	PolledAt                 time.Time
}

// ForgeNotifyTarget is one enumerated poll/reconcile coordinate: the artifact
// (or collapsed container) coordinate, its shared FETCH cursor (nil when never
// observed), and the subscribers riding it. Container targets collapse per
// (repo, kind) to Number == 0.
type ForgeNotifyTarget struct {
	Provider    ForgeProvider
	Host, Repo  string
	Kind        ForgeArtifactKind
	Number      uint64
	Cursor      *ForgeArtifactCursor // nil: never observed
	Subscribers []ForgeNotifySubscriber
}

// SubscribersForArtifact returns the subscribers a change on one artifact must
// fan out to: the exact-artifact subscribers (scope ARTIFACT, matching number)
// and — when openedEvent (a newly OPENED artifact) — the container-scope
// subscribers for the same container. A container matches by (repo, kind) plus
// project: GitHub containers are project-less (project ”), so a GitHub artifact
// event passes project "" and matches the ” container rows; a Linear artifact
// event passes the artifact's project and matches only that project's container
// rows. One indexed query over agent_forge_subscriptions_artifact_idx (its
// leading (forge_provider, forge_host, repo, kind) columns). number MUST be > 0
// (an artifact event always names an artifact); zero provider/kind/empty repo ->
// ErrInvalidArgument.
func (s *Store) SubscribersForArtifact(ctx context.Context, provider ForgeProvider, host, repo string, kind ForgeArtifactKind, number uint64, project string, openedEvent bool) ([]ForgeNotifySubscriber, error) {
	if err := validCoordinate(provider, host, repo); err != nil {
		return nil, err
	}
	if kind != ForgeArtifactKindIssue && kind != ForgeArtifactKindPullRequest {
		return nil, fmt.Errorf("%w: artifact kind must be issue or pull_request", ErrInvalidArgument)
	}
	if number == 0 {
		return nil, fmt.Errorf("%w: artifact number is required", ErrInvalidArgument)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, agent_account_id, delivered_revision, project
		   FROM agent_forge_subscriptions
		  WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3 AND kind = $4
		    AND (
		          (scope = 1 AND number = $5)
		       OR ($6 AND scope = 2 AND number = 0 AND project = $7)
		    )`,
		int32(provider), host, repo, int32(kind),
		int64(number), //nolint:gosec // G115: canonical artifact number in a BIGINT domain.
		openedEvent, project,
	)
	if err != nil {
		return nil, fmt.Errorf("store: subscribers for artifact: %w", err)
	}
	defer rows.Close()
	var out []ForgeNotifySubscriber
	for rows.Next() {
		var sub ForgeNotifySubscriber
		var agent string
		if err := rows.Scan(&sub.SubscriptionID, &agent, &sub.DeliveredRevision, &sub.Project); err != nil {
			return nil, fmt.Errorf("store: scan artifact subscriber: %w", err)
		}
		sub.AgentAccountID = AccountID(agent)
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate artifact subscribers: %w", err)
	}
	return out, nil
}

// ListForgeNotifyTargets enumerates the distinct subscribed coordinates for one
// (provider, host) — the reconcile sweep's work list — each with its shared
// FETCH cursor (LEFT JOIN forge_artifact_cursors; nil when never observed) and
// the subscribers riding it. Container-scope rows collapse per (repo, kind) to a
// single Number == 0 target: every Linear project sub on one team shares one
// team-keyed LIST walk, so they fold into one target whose Subscribers hold all
// the container subs for that (repo, kind). Zero provider / empty host ->
// ErrInvalidArgument.
func (s *Store) ListForgeNotifyTargets(ctx context.Context, provider ForgeProvider, host string) ([]ForgeNotifyTarget, error) {
	if provider == ForgeProviderUnspecified {
		return nil, fmt.Errorf("%w: forge provider is required", ErrInvalidArgument)
	}
	if host == "" {
		return nil, fmt.Errorf("%w: forge host is required", ErrInvalidArgument)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT s.repo, s.kind,
		        CASE WHEN s.scope = 2 THEN 0 ELSE s.number END AS coord_number,
		        s.id, s.agent_account_id, s.delivered_revision, s.project,
		        c.forge_provider IS NOT NULL AS has_cursor,
		        c.etag, c.comments_etag, c.checks_etag, c.revision, c.snapshot, c.polled_at
		   FROM agent_forge_subscriptions s
		   LEFT JOIN forge_artifact_cursors c
		     ON c.forge_provider = s.forge_provider
		    AND c.forge_host = s.forge_host
		    AND c.repo = s.repo
		    AND c.kind = s.kind
		    AND c.number = CASE WHEN s.scope = 2 THEN 0 ELSE s.number END
		  WHERE s.forge_provider = $1 AND s.forge_host = $2
		  ORDER BY s.repo, s.kind, coord_number`,
		int32(provider), host,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list forge notify targets: %w", err)
	}
	defer rows.Close()
	var (
		out []ForgeNotifyTarget
		cur *ForgeNotifyTarget // the target the current run of rows belongs to
	)
	for rows.Next() {
		var (
			repo         string
			kind         int32
			coordNumber  int64
			subID        string
			agent        string
			delivered    string
			project      string
			hasCursor    bool
			etag         *string
			commentsETag *string
			checksETag   *string
			revision     *string
			snapshot     []byte
			polledAt     *time.Time
		)
		if err := rows.Scan(&repo, &kind, &coordNumber, &subID, &agent, &delivered, &project,
			&hasCursor, &etag, &commentsETag, &checksETag, &revision, &snapshot, &polledAt); err != nil {
			return nil, fmt.Errorf("store: scan forge notify target: %w", err)
		}
		// coord_number is a canonical artifact number (or 0) from a BIGINT,
		// always within the uint64 domain — cast once, reuse for the coordinate
		// compare and both target/cursor constructs.
		coord := uint64(coordNumber) //nolint:gosec // G115: see above.
		if cur == nil || cur.Repo != repo || int32(cur.Kind) != kind || cur.Number != coord {
			out = append(out, ForgeNotifyTarget{
				Provider: provider,
				Host:     host,
				Repo:     repo,
				Kind:     ForgeArtifactKind(kind),
				Number:   coord,
			})
			cur = &out[len(out)-1]
			if hasCursor {
				cur.Cursor = &ForgeArtifactCursor{
					Provider:     provider,
					Host:         host,
					Repo:         repo,
					Kind:         ForgeArtifactKind(kind),
					Number:       coord,
					ETag:         derefString(etag),
					CommentsETag: derefString(commentsETag),
					ChecksETag:   derefString(checksETag),
					Revision:     derefString(revision),
					Snapshot:     snapshot,
				}
				if polledAt != nil {
					cur.Cursor.PolledAt = *polledAt
				}
			}
		}
		cur.Subscribers = append(cur.Subscribers, ForgeNotifySubscriber{
			SubscriptionID:    subID,
			AgentAccountID:    AccountID(agent),
			DeliveredRevision: delivered,
			Project:           project,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate forge notify targets: %w", err)
	}
	return out, nil
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// UpsertForgeArtifactCursor writes (inserts or replaces) the shared per-artifact
// FETCH cursor at cur's coordinate, keyed by the PK (provider, host, repo, kind,
// number). number == 0 is the legal container-scope reconcile cursor row (the PK
// admits it). polled_at records this write; a zero cur.PolledAt defaults to now.
// Zero provider / empty host/repo / a zero kind -> ErrInvalidArgument.
func (s *Store) UpsertForgeArtifactCursor(ctx context.Context, cur ForgeArtifactCursor) error {
	if err := validCoordinate(cur.Provider, cur.Host, cur.Repo); err != nil {
		return err
	}
	if cur.Kind != ForgeArtifactKindIssue && cur.Kind != ForgeArtifactKindPullRequest {
		return fmt.Errorf("%w: artifact kind must be issue or pull_request", ErrInvalidArgument)
	}
	polledAt := cur.PolledAt
	if polledAt.IsZero() {
		polledAt = time.Now().UTC()
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO forge_artifact_cursors
		     (forge_provider, forge_host, repo, kind, number, etag, comments_etag, checks_etag, revision, snapshot, polled_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 ON CONFLICT (forge_provider, forge_host, repo, kind, number) DO UPDATE
		    SET etag = EXCLUDED.etag,
		        comments_etag = EXCLUDED.comments_etag,
		        checks_etag = EXCLUDED.checks_etag,
		        revision = EXCLUDED.revision,
		        snapshot = EXCLUDED.snapshot,
		        polled_at = EXCLUDED.polled_at`,
		int32(cur.Provider), cur.Host, cur.Repo, int32(cur.Kind),
		int64(cur.Number), //nolint:gosec // G115: canonical artifact number (or 0 container) in a BIGINT domain.
		cur.ETag, cur.CommentsETag, cur.ChecksETag, cur.Revision, cur.Snapshot, polledAt,
	); err != nil {
		return fmt.Errorf("store: upsert forge artifact cursor: %w", err)
	}
	return nil
}

// AdvanceForgeDeliveredRevision advances one subscription's per-subscriber
// DELIVERY cursor to revision, scoped to the owning agent (id AND
// agent_account_id must both match — an agent cannot advance another's cursor).
// Called from the hub's ForgeNotificationAck arm (W3; T7), never from the
// router's dispatch path. Zero rows (unknown id, or an id owned by a different
// agent, or unsubscribed mid-flight) -> ErrNotFound (log and move on).
func (s *Store) AdvanceForgeDeliveredRevision(ctx context.Context, agent AccountID, subscriptionID, revision string) error {
	if agent == "" {
		return fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	if subscriptionID == "" {
		return fmt.Errorf("%w: subscription id is required", ErrInvalidArgument)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE agent_forge_subscriptions
		    SET delivered_revision = $3, delivered_at = now()
		  WHERE id = $2 AND agent_account_id = $1`,
		string(agent), subscriptionID, revision,
	)
	if err != nil {
		return fmt.Errorf("store: advance forge delivered revision: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: subscription %q", ErrNotFound, subscriptionID)
	}
	return nil
}
