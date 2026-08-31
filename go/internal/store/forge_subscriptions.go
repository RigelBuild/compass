package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RigelBuild/compass/go/internal/store/db"
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
	id, err := s.q.EnsureAgentForgeSubscription(ctx, db.EnsureAgentForgeSubscriptionParams{
		ID:             newID(),
		AgentAccountID: string(sub.AgentAccountID),
		ForgeProvider:  int16(sub.Provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum (agent_forge_subscriptions.forge_provider), always within int16
		ForgeHost:      sub.Host,
		Repo:           sub.Repo,
		Kind:           int16(sub.Kind),                  //nolint:gosec // G115: ForgeArtifactKind is a CHECK-constrained 1/2 enum, always within int16
		Number:         int64(sub.Number),                //nolint:gosec // G115: number is a canonical forge artifact number (a positive issue/PR number, or 0 for a container) written to a BIGINT, always well within the int64 domain.
		Scope:          int16(normalizeScope(sub.Scope)), //nolint:gosec // G115: ForgeSubscriptionScope is a CHECK-constrained 1/2 enum (normalized), always within int16
		Project:        sub.Project,
	})
	if err != nil {
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
		qtx := db.New(tx)
		coord, err := qtx.DeleteAgentForgeSubscription(ctx, db.DeleteAgentForgeSubscriptionParams{
			ID:             subscriptionID,
			AgentAccountID: string(agent),
		})
		if err != nil {
			if noRows(err) {
				return fmt.Errorf("%w: subscription %q", ErrNotFound, subscriptionID)
			}
			return fmt.Errorf("store: delete agent forge subscription: %w", err)
		}
		if err := qtx.GCForgeArtifactCursorIfUnsubscribed(ctx, db.GCForgeArtifactCursorIfUnsubscribedParams(coord)); err != nil {
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
	n, err := s.q.CountAgentForgeSubscriptionsForArtifact(ctx, db.CountAgentForgeSubscriptionsForArtifactParams{
		ForgeProvider: int16(provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum, always within int16
		ForgeHost:     host,
		Repo:          repo,
		Kind:          int16(kind),   //nolint:gosec // G115: ForgeArtifactKind is a CHECK-constrained 1/2 enum, always within int16
		Number:        int64(number), //nolint:gosec // G115: canonical artifact number written to a BIGINT, always within the int64 domain.
	})
	if err != nil {
		return 0, fmt.Errorf("store: count agent forge subscriptions for artifact: %w", err)
	}
	return int(n), nil
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
	rows, err := s.q.SubscribersForArtifact(ctx, db.SubscribersForArtifactParams{
		ForgeProvider: int16(provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum, always within int16
		ForgeHost:     host,
		Repo:          repo,
		Kind:          int16(kind),   //nolint:gosec // G115: ForgeArtifactKind is a CHECK-constrained 1/2 enum, always within int16
		Number:        int64(number), //nolint:gosec // G115: canonical artifact number in a BIGINT domain.
		Column6:       openedEvent,
		Project:       project,
	})
	if err != nil {
		return nil, fmt.Errorf("store: subscribers for artifact: %w", err)
	}
	var out []ForgeNotifySubscriber
	for _, r := range rows {
		out = append(out, ForgeNotifySubscriber{
			SubscriptionID:    r.ID,
			AgentAccountID:    AccountID(r.AgentAccountID),
			DeliveredRevision: r.DeliveredRevision,
			Project:           r.Project,
		})
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
	rows, err := s.q.ListForgeNotifyTargets(ctx, db.ListForgeNotifyTargetsParams{
		ForgeProvider: int16(provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum, always within int16
		ForgeHost:     host,
	})
	if err != nil {
		return nil, fmt.Errorf("store: list forge notify targets: %w", err)
	}
	var (
		out []ForgeNotifyTarget
		cur *ForgeNotifyTarget // the target the current run of rows belongs to
	)
	for _, r := range rows {
		kind := r.Kind
		// coord_number is a canonical artifact number (or 0) from a BIGINT,
		// always within the uint64 domain — cast once, reuse for the coordinate
		// compare and both target/cursor constructs.
		coord := uint64(r.CoordNumber) //nolint:gosec // G115: see above.
		if cur == nil || cur.Repo != r.Repo || int16(cur.Kind) != kind || cur.Number != coord {
			out = append(out, ForgeNotifyTarget{
				Provider: provider,
				Host:     host,
				Repo:     r.Repo,
				Kind:     ForgeArtifactKind(kind),
				Number:   coord,
			})
			cur = &out[len(out)-1]
			if r.HasCursor {
				cur.Cursor = &ForgeArtifactCursor{
					Provider:     provider,
					Host:         host,
					Repo:         r.Repo,
					Kind:         ForgeArtifactKind(kind),
					Number:       coord,
					ETag:         r.Etag.String,
					CommentsETag: r.CommentsEtag.String,
					ChecksETag:   r.ChecksEtag.String,
					Revision:     r.Revision.String,
					Snapshot:     r.Snapshot,
				}
				if r.PolledAt.Valid {
					cur.Cursor.PolledAt = r.PolledAt.Time
				}
			}
		}
		cur.Subscribers = append(cur.Subscribers, ForgeNotifySubscriber{
			SubscriptionID:    r.ID,
			AgentAccountID:    AccountID(r.AgentAccountID),
			DeliveredRevision: r.DeliveredRevision,
			Project:           r.Project,
		})
	}
	return out, nil
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
	if err := s.q.UpsertForgeArtifactCursor(ctx, db.UpsertForgeArtifactCursorParams{
		ForgeProvider: int16(cur.Provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum, always within int16
		ForgeHost:     cur.Host,
		Repo:          cur.Repo,
		Kind:          int16(cur.Kind),   //nolint:gosec // G115: ForgeArtifactKind is a CHECK-constrained 1/2 enum, always within int16
		Number:        int64(cur.Number), //nolint:gosec // G115: canonical artifact number (or 0 container) in a BIGINT domain.
		Etag:          cur.ETag,
		CommentsEtag:  cur.CommentsETag,
		ChecksEtag:    cur.ChecksETag,
		Revision:      cur.Revision,
		Snapshot:      cur.Snapshot,
		PolledAt:      pgtype.Timestamptz{Time: polledAt, Valid: true},
	}); err != nil {
		return fmt.Errorf("store: upsert forge artifact cursor: %w", err)
	}
	return nil
}

// LoadForgeArtifactCursor point-reads the shared per-artifact FETCH cursor at
// the coordinate (provider, host, repo, kind, number), or (nil, nil) when the
// coordinate has never been observed (no row). It is the single-coordinate
// reader the notify router needs (LoadArtifactCursor on the ingest.NotifyStore
// seam); ListForgeNotifyTargets is the bulk reconcile-sweep enumerate, not a
// per-event point read. number == 0 is the legal container-scope reconcile
// cursor row (the PK admits it). Zero provider / empty host/repo / a zero kind
// -> ErrInvalidArgument.
func (s *Store) LoadForgeArtifactCursor(ctx context.Context, provider ForgeProvider, host, repo string, kind ForgeArtifactKind, number uint64) (*ForgeArtifactCursor, error) {
	if err := validCoordinate(provider, host, repo); err != nil {
		return nil, err
	}
	if kind != ForgeArtifactKindIssue && kind != ForgeArtifactKindPullRequest {
		return nil, fmt.Errorf("%w: artifact kind must be issue or pull_request", ErrInvalidArgument)
	}
	row, err := s.q.LoadForgeArtifactCursor(ctx, db.LoadForgeArtifactCursorParams{
		ForgeProvider: int16(provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum, always within int16
		ForgeHost:     host,
		Repo:          repo,
		Kind:          int16(kind),
		Number:        int64(number), //nolint:gosec // G115: canonical artifact number (or 0 container) in a BIGINT domain.
	})
	if noRows(err) {
		return nil, nil //nolint:nilnil // a never-observed cursor is (nil, nil) by the load contract: the caller (notify router via forgeNotifyStore, serve.go:1067) guards nil as "unobserved". A sentinel would force every reader to special-case it.
	}
	if err != nil {
		return nil, fmt.Errorf("store: load forge artifact cursor: %w", err)
	}
	cur := ForgeArtifactCursor{
		Provider:     provider,
		Host:         host,
		Repo:         repo,
		Kind:         kind,
		Number:       number,
		ETag:         row.Etag,
		CommentsETag: row.CommentsEtag,
		ChecksETag:   row.ChecksEtag,
		Revision:     row.Revision,
		Snapshot:     row.Snapshot,
	}
	if row.PolledAt.Valid {
		cur.PolledAt = row.PolledAt.Time
	}
	return &cur, nil
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
	affected, err := s.q.AdvanceForgeDeliveredRevision(ctx, db.AdvanceForgeDeliveredRevisionParams{
		AgentAccountID:    string(agent),
		ID:                subscriptionID,
		DeliveredRevision: revision,
	})
	if err != nil {
		return fmt.Errorf("store: advance forge delivered revision: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: subscription %q", ErrNotFound, subscriptionID)
	}
	return nil
}
