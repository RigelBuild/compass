package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// The board arm's durable state (RIG-2883): the per-REPO poll targets and their
// swept-updated-at watermark (forge_repo_subscriptions). The two DL-053
// anticipatory tables (agent_forge_subscriptions, forge_artifact_cursors) are
// writer-less this slice and get their store surface with their writers.

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
	row, err := s.q.LoadForgeRepoWatermark(ctx, db.LoadForgeRepoWatermarkParams{
		ForgeProvider: int16(provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum (forge_repo_subscriptions.forge_provider), always within int16
		ForgeHost:     host,
		Repo:          repo,
	})
	if noRows(err) {
		return time.Time{}, "", nil
	}
	if err != nil {
		return time.Time{}, "", fmt.Errorf("store: load forge repo watermark: %w", err)
	}
	if !row.SweptUpdatedAt.Valid {
		return time.Time{}, row.ListEtag, nil
	}
	return row.SweptUpdatedAt.Time, row.ListEtag, nil
}

// StoreForgeRepoWatermark writes the repo's swept_updated_at watermark and
// list_etag, touching updated_at. An unknown coordinate -> ErrNotFound (the
// subscription must exist — the seed/upsert path owns row creation). Zero/empty
// coordinate fields -> ErrInvalidArgument.
func (s *Store) StoreForgeRepoWatermark(ctx context.Context, provider ForgeProvider, host, repo string, mark time.Time, etag string) error {
	if err := validCoordinate(provider, host, repo); err != nil {
		return err
	}
	var swept pgtype.Timestamptz
	if !mark.IsZero() {
		swept = pgtype.Timestamptz{Time: mark, Valid: true}
	}
	affected, err := s.q.StoreForgeRepoWatermark(ctx, db.StoreForgeRepoWatermarkParams{
		ForgeProvider:  int16(provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum, always within int16
		ForgeHost:      host,
		Repo:           repo,
		SweptUpdatedAt: swept,
		ListEtag:       etag,
	})
	if err != nil {
		return fmt.Errorf("store: store forge repo watermark: %w", err)
	}
	if affected == 0 {
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
	if err := s.q.EnsureForgeRepoSubscription(ctx, db.EnsureForgeRepoSubscriptionParams{
		ForgeProvider: int16(sub.Provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum, always within int16
		ForgeHost:     sub.Host,
		Repo:          sub.Repo,
		Enabled:       sub.Enabled,
	}); err != nil {
		return fmt.Errorf("store: ensure forge repo subscription: %w", err)
	}
	return nil
}

// ListEnabledForgeRepos reads every enabled target's repo, ascending — the board
// reconciler's per-pass target enumeration across all coordinates. No rows is a
// nil slice, not an error.
//
// Repo-only keyed (no provider/host), matching the frozen repo-keyed ingest
// seam. In a github.com-only deployment repo is unambiguous; if multi-host is
// ever enabled, two coordinates sharing a repo string (e.g. github.com and a GHE
// host both carrying "a/b") would collapse to one entry here and to an ambiguous
// watermark under the coordinate-keyed Load/Store methods — thread (provider,
// host) through this seam before enabling multi-host.
func (s *Store) ListEnabledForgeRepos(ctx context.Context) ([]string, error) {
	repos, err := s.q.ListEnabledForgeRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list enabled forge repos: %w", err)
	}
	return repos, nil
}

// IsEnabledForgeRepo reports whether an enabled subscription exists for the repo
// (the point membership check the webhook arm gates on). An empty repo ->
// ErrInvalidArgument. Repo-only keyed like ListEnabledForgeRepos — returns true
// if ANY coordinate's subscription for the repo is enabled; unambiguous in a
// github.com-only deployment (see ListEnabledForgeRepos for the multi-host note).
func (s *Store) IsEnabledForgeRepo(ctx context.Context, repo string) (bool, error) {
	if repo == "" {
		return false, fmt.Errorf("%w: repo is required", ErrInvalidArgument)
	}
	exists, err := s.q.IsEnabledForgeRepo(ctx, repo)
	if err != nil {
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
	rows, err := s.q.ListEnabledForgeRepoSubscriptions(ctx, db.ListEnabledForgeRepoSubscriptionsParams{
		ForgeProvider: int16(provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum, always within int16
		ForgeHost:     host,
	})
	if err != nil {
		return nil, fmt.Errorf("store: list enabled forge repo subscriptions: %w", err)
	}
	var out []ForgeRepoSubscription
	for _, r := range rows {
		out = append(out, ForgeRepoSubscription{
			Provider: ForgeProvider(r.ForgeProvider),
			Host:     r.ForgeHost,
			Repo:     r.Repo,
			Enabled:  r.Enabled,
		})
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
	affected, err := s.q.SetForgeRepoSubscriptionEnabled(ctx, db.SetForgeRepoSubscriptionEnabledParams{
		ForgeProvider: int16(provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum, always within int16
		ForgeHost:     host,
		Repo:          repo,
		Enabled:       enabled,
	})
	if err != nil {
		return fmt.Errorf("store: set forge repo subscription enabled: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: forge repo subscription (%d, %q, %q)", ErrNotFound, provider, host, repo)
	}
	return nil
}
