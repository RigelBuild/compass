package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The board arm's durable state (RIG-2883): the per-REPO poll targets and their
// swept-updated-at watermark (forge_repo_subscriptions), plus the poll driver's
// per-page FETCH cursor (forge_list_cursors) — the latter retires atomically
// with its serve.go consumer in T5, so it survives this additive slice. The two
// DL-053 anticipatory tables (agent_forge_subscriptions, forge_artifact_cursors)
// are writer-less this slice and get their store surface with their writers.

// ForgeListPageCursor is one durable page row of a repo's issue-LIST fetch
// cursor (the DL-053 FETCH-cursor model at repo-LIST granularity). ETag ""
// means never fetched (an unconditional GET). Retires with the poll driver (T5).
type ForgeListPageCursor struct {
	Provider ForgeProvider // GITHUB(1)/GITLAB(2)/FORGEJO(3)/LINEAR(4); never 0
	Host     string
	Repo     string
	Page     int32 // 1-based
	ETag     string
	HasNext  bool
}

// ForgeRepoSubscription is one board poll target: a repo the board arm walks
// (OQ-C's table model). Enabled=false soft-disables the target without deleting
// its watermark history.
type ForgeRepoSubscription struct {
	Provider ForgeProvider
	Host     string
	Repo     string
	Enabled  bool
}

// validCoordinate rejects the zero/empty coordinate fields every forge method
// guards on: a zero provider (never UNSPECIFIED(0), the CHECK's job in Go
// space) or an empty host/repo is a caller bug, ErrInvalidArgument, before any
// DB round trip.
func validCoordinate(provider ForgeProvider, host, repo string) error {
	if provider == ForgeProviderUnspecified {
		return fmt.Errorf("%w: forge provider is required", ErrInvalidArgument)
	}
	if host == "" {
		return fmt.Errorf("%w: forge host is required", ErrInvalidArgument)
	}
	if repo == "" {
		return fmt.Errorf("%w: repo is required", ErrInvalidArgument)
	}
	return nil
}

// ForgeListCursor reads every stored page row for the repo, ascending page. No
// rows is a nil slice, not an error (a never-polled repo). Zero/empty
// coordinate fields -> ErrInvalidArgument.
func (s *Store) ForgeListCursor(ctx context.Context, provider ForgeProvider, host, repo string) ([]ForgeListPageCursor, error) {
	if err := validCoordinate(provider, host, repo); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT forge_provider, forge_host, repo, page, etag, has_next
		   FROM forge_list_cursors
		  WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3
		  ORDER BY page ASC`,
		int32(provider), host, repo,
	)
	if err != nil {
		return nil, fmt.Errorf("store: read forge list cursor: %w", err)
	}
	defer rows.Close()

	var out []ForgeListPageCursor
	for rows.Next() {
		var c ForgeListPageCursor
		var p int32
		if err := rows.Scan(&p, &c.Host, &c.Repo, &c.Page, &c.ETag, &c.HasNext); err != nil {
			return nil, fmt.Errorf("store: scan forge list cursor: %w", err)
		}
		c.Provider = ForgeProvider(p)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate forge list cursor: %w", err)
	}
	return out, nil
}

// UpsertForgeListCursorPage inserts-or-updates one page row (touching
// advanced_at — the last CONTENT advance, since this is called only after a
// 200+sink; an all-304 tick rewrites no row). Called by the driver ONLY after
// the page's content durably sank — the advance-attests-sink invariant lives in
// the caller; the store method is a plain upsert. Zero/empty coordinate fields
// or page < 1 -> ErrInvalidArgument.
func (s *Store) UpsertForgeListCursorPage(ctx context.Context, cur ForgeListPageCursor) error {
	if err := validCoordinate(cur.Provider, cur.Host, cur.Repo); err != nil {
		return err
	}
	if cur.Page < 1 {
		return fmt.Errorf("%w: page must be >= 1", ErrInvalidArgument)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO forge_list_cursors
		     (forge_provider, forge_host, repo, page, etag, has_next, advanced_at)
		 VALUES ($1, $2, $3, $4, $5, $6, now())
		 ON CONFLICT (forge_provider, forge_host, repo, page) DO UPDATE
		    SET etag = EXCLUDED.etag, has_next = EXCLUDED.has_next, advanced_at = now()`,
		int32(cur.Provider), cur.Host, cur.Repo, cur.Page, cur.ETag, cur.HasNext,
	); err != nil {
		return fmt.Errorf("store: upsert forge list cursor page: %w", err)
	}
	return nil
}

// PruneForgeListCursorPages deletes the repo's page rows with page > maxPage (a
// repo whose walk shrank). maxPage < 1 -> ErrInvalidArgument; zero/empty
// coordinate fields -> ErrInvalidArgument. Pruning a never-polled repo is a
// no-op success.
func (s *Store) PruneForgeListCursorPages(ctx context.Context, provider ForgeProvider, host, repo string, maxPage int32) error {
	if err := validCoordinate(provider, host, repo); err != nil {
		return err
	}
	if maxPage < 1 {
		return fmt.Errorf("%w: max page must be >= 1", ErrInvalidArgument)
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM forge_list_cursors
		  WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3 AND page > $4`,
		int32(provider), host, repo, maxPage,
	); err != nil {
		return fmt.Errorf("store: prune forge list cursor pages: %w", err)
	}
	return nil
}

// LoadForgeRepoWatermark reads the repo's swept_updated_at watermark and its
// conditional-GET list_etag. A never-swept repo (swept_updated_at IS NULL)
// returns the zero time.Time; an unknown coordinate is not an error — it too
// returns the zero watermark and empty etag (the reconciler treats "no row" and
// "never swept" identically, walking from the beginning). Zero/empty coordinate
// fields -> ErrInvalidArgument.
func (s *Store) LoadForgeRepoWatermark(ctx context.Context, provider ForgeProvider, host, repo string) (time.Time, string, error) {
	if err := validCoordinate(provider, host, repo); err != nil {
		return time.Time{}, "", err
	}
	var swept *time.Time
	var etag string
	err := s.pool.QueryRow(ctx,
		`SELECT swept_updated_at, list_etag
		   FROM forge_repo_subscriptions
		  WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3`,
		int32(provider), host, repo,
	).Scan(&swept, &etag)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, "", nil
	}
	if err != nil {
		return time.Time{}, "", fmt.Errorf("store: load forge repo watermark: %w", err)
	}
	if swept == nil {
		return time.Time{}, etag, nil
	}
	return *swept, etag, nil
}

// StoreForgeRepoWatermark writes the repo's swept_updated_at watermark and
// list_etag, touching updated_at. An unknown coordinate -> ErrNotFound (the
// subscription must exist — the seed/upsert path owns row creation). Zero/empty
// coordinate fields -> ErrInvalidArgument.
func (s *Store) StoreForgeRepoWatermark(ctx context.Context, provider ForgeProvider, host, repo string, mark time.Time, etag string) error {
	if err := validCoordinate(provider, host, repo); err != nil {
		return err
	}
	var swept *time.Time
	if !mark.IsZero() {
		swept = &mark
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE forge_repo_subscriptions
		    SET swept_updated_at = $4, list_etag = $5, updated_at = now()
		  WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3`,
		int32(provider), host, repo, swept, etag,
	)
	if err != nil {
		return fmt.Errorf("store: store forge repo watermark: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: forge repo subscription (%d, %q, %q)", ErrNotFound, provider, host, repo)
	}
	return nil
}

// EnsureForgeRepoSubscription inserts the target if absent; on conflict it DOES
// NOTHING — the seed reconcile is a bootstrap-only insert and the table is
// authoritative after the first insert (the seed never deletes, disables, or
// re-enables an existing row). Zero/empty coordinate fields -> ErrInvalidArgument.
func (s *Store) EnsureForgeRepoSubscription(ctx context.Context, sub ForgeRepoSubscription) error {
	if err := validCoordinate(sub.Provider, sub.Host, sub.Repo); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO forge_repo_subscriptions (forge_provider, forge_host, repo, enabled)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (forge_provider, forge_host, repo) DO NOTHING`,
		int32(sub.Provider), sub.Host, sub.Repo, sub.Enabled,
	); err != nil {
		return fmt.Errorf("store: ensure forge repo subscription: %w", err)
	}
	return nil
}

// ListEnabledForgeRepos reads every enabled target's repo, ascending — the board
// reconciler's per-pass target enumeration across all coordinates. No rows is a
// nil slice, not an error.
func (s *Store) ListEnabledForgeRepos(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT repo
		   FROM forge_repo_subscriptions
		  WHERE enabled = TRUE
		  ORDER BY repo ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list enabled forge repos: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			return nil, fmt.Errorf("store: scan forge repo: %w", err)
		}
		out = append(out, repo)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate forge repos: %w", err)
	}
	return out, nil
}

// IsEnabledForgeRepo reports whether an enabled subscription exists for the repo
// (the point membership check the webhook arm gates on). An empty repo ->
// ErrInvalidArgument.
func (s *Store) IsEnabledForgeRepo(ctx context.Context, repo string) (bool, error) {
	if repo == "" {
		return false, fmt.Errorf("%w: repo is required", ErrInvalidArgument)
	}
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM forge_repo_subscriptions
		    WHERE repo = $1 AND enabled = TRUE)`,
		repo,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: is enabled forge repo: %w", err)
	}
	return exists, nil
}

// ListEnabledForgeRepoSubscriptions reads the enabled targets for one (provider,
// host), ascending repo. No rows is a nil slice, not an error. Zero provider /
// empty host -> ErrInvalidArgument.
func (s *Store) ListEnabledForgeRepoSubscriptions(ctx context.Context, provider ForgeProvider, host string) ([]ForgeRepoSubscription, error) {
	if provider == ForgeProviderUnspecified {
		return nil, fmt.Errorf("%w: forge provider is required", ErrInvalidArgument)
	}
	if host == "" {
		return nil, fmt.Errorf("%w: forge host is required", ErrInvalidArgument)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT forge_provider, forge_host, repo, enabled
		   FROM forge_repo_subscriptions
		  WHERE forge_provider = $1 AND forge_host = $2 AND enabled = TRUE
		  ORDER BY repo ASC`,
		int32(provider), host,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list enabled forge repo subscriptions: %w", err)
	}
	defer rows.Close()

	var out []ForgeRepoSubscription
	for rows.Next() {
		var sub ForgeRepoSubscription
		var p int32
		if err := rows.Scan(&p, &sub.Host, &sub.Repo, &sub.Enabled); err != nil {
			return nil, fmt.Errorf("store: scan forge repo subscription: %w", err)
		}
		sub.Provider = ForgeProvider(p)
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate forge repo subscriptions: %w", err)
	}
	return out, nil
}

// SetForgeRepoSubscriptionEnabled flips one target's enabled bit, touching
// updated_at (the soft-disable path; the admin mutation surface is a later
// slice's — this method exists for it, for operators via SQL parity, and for
// tests). Unknown coordinate -> ErrNotFound; zero/empty fields ->
// ErrInvalidArgument.
func (s *Store) SetForgeRepoSubscriptionEnabled(ctx context.Context, provider ForgeProvider, host, repo string, enabled bool) error {
	if err := validCoordinate(provider, host, repo); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE forge_repo_subscriptions
		    SET enabled = $4, updated_at = now()
		  WHERE forge_provider = $1 AND forge_host = $2 AND repo = $3`,
		int32(provider), host, repo, enabled,
	)
	if err != nil {
		return fmt.Errorf("store: set forge repo subscription enabled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: forge repo subscription (%d, %q, %q)", ErrNotFound, provider, host, repo)
	}
	return nil
}
