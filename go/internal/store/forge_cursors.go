package store

import (
	"context"
	"fmt"
)

// The forge poll driver's durable state (SEA-1810 T2, design
// docs/designs/product/compass-forge-poll-driver/design.md §T2): the repo-LIST
// per-page FETCH cursor (forge_list_cursors) and the board's per-REPO poll
// targets (forge_repo_subscriptions). The two DL-053 anticipatory tables
// (agent_forge_subscriptions, forge_artifact_cursors) are writer-less this
// slice and get their store surface with their writers.

// ForgeListPageCursor is one durable page row of a repo's issue-LIST fetch
// cursor (the DL-053 FETCH-cursor model at repo-LIST granularity). ETag ""
// means never fetched (an unconditional GET).
type ForgeListPageCursor struct {
	Provider ForgeProvider // GITHUB(1)/GITLAB(2)/FORGEJO(3)/LINEAR(4); never 0
	Host     string
	Repo     string
	Page     int32 // 1-based
	ETag     string
	HasNext  bool
}

// ForgeRepoSubscription is one board poll target: a repo the poll driver walks
// (OQ-C's table model). Enabled=false soft-disables the target without deleting
// its cursor history.
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

// EnsureForgeRepoSubscription inserts the target if absent; on conflict it DOES
// NOTHING — the T4 seed reconcile is a bootstrap-only insert and the table is
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

// ListEnabledForgeRepoSubscriptions reads the enabled targets for one (provider,
// host), ascending repo — the driver's per-pass target enumeration. No rows is a
// nil slice, not an error. Zero provider / empty host -> ErrInvalidArgument.
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
