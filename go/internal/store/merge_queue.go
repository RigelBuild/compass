package store

import (
	"context"
	"fmt"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// MergeQueueConfig is the configured queue provider and target branch for a repo.
type MergeQueueConfig struct {
	Repo     string
	Provider string
	Branch   string
}

// MergeQueueConfigs returns enabled GitHub repositories configured for queue polling.
func (s *Store) MergeQueueConfigs(ctx context.Context, host string) ([]MergeQueueConfig, error) {
	rows, err := s.q.ListForgeRepoMergeQueueConfigs(ctx, db.ListForgeRepoMergeQueueConfigsParams{
		ForgeProvider: int16(ForgeProviderGitHub),
		ForgeHost:     host,
	})
	if err != nil {
		return nil, fmt.Errorf("store: list forge merge queue configs: %w", err)
	}
	out := make([]MergeQueueConfig, 0, len(rows))
	for _, row := range rows {
		out = append(out, MergeQueueConfig{Repo: row.Repo, Provider: row.MergeQueue, Branch: row.MergeQueueBranch})
	}
	return out, nil
}

// MergeQueueConfigFor returns the queue configured for a repository.
func (s *Store) MergeQueueConfigFor(ctx context.Context, host, repo string) (string, string, error) {
	row, err := s.q.GetForgeRepoMergeQueueConfig(ctx, db.GetForgeRepoMergeQueueConfigParams{
		ForgeProvider: int16(ForgeProviderGitHub),
		ForgeHost:     host,
		Repo:          repo,
	})
	if noRows(err) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("store: get forge repo merge queue config: %w", err)
	}
	return row.MergeQueue, row.MergeQueueBranch, nil
}

// SetForgeRepoMergeQueueConfig updates a GitHub repo's provider and queue branch.
func (s *Store) SetForgeRepoMergeQueueConfig(ctx context.Context, host, repo, provider, branch string) error {
	if err := validCoordinate(ForgeProviderGitHub, host, repo); err != nil {
		return err
	}
	affected, err := s.q.SetForgeRepoMergeQueue(ctx, db.SetForgeRepoMergeQueueParams{
		ForgeProvider:    int16(ForgeProviderGitHub),
		ForgeHost:        host,
		Repo:             repo,
		MergeQueue:       provider,
		MergeQueueBranch: branch,
	})
	if err != nil {
		return fmt.Errorf("store: set forge merge queue config: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: forge repo subscription (%q, %q)", ErrNotFound, host, repo)
	}
	return nil
}

// ClearForgeRepoMergeQueueConfigs disables merge-queue polling for every GitHub repo at host.
func (s *Store) ClearForgeRepoMergeQueueConfigs(ctx context.Context, host string) error {
	if host == "" {
		return fmt.Errorf("%w: forge host is required", ErrInvalidArgument)
	}
	if err := s.q.ClearForgeRepoMergeQueueConfigs(ctx, db.ClearForgeRepoMergeQueueConfigsParams{
		ForgeProvider: int16(ForgeProviderGitHub),
		ForgeHost:     host,
	}); err != nil {
		return fmt.Errorf("store: clear forge merge queue configs: %w", err)
	}
	return nil
}

// OpenPullRequests returns the open PR numbers for one GitHub repository.
func (s *Store) OpenPullRequests(ctx context.Context, host, repo string) ([]uint64, error) {
	rows, err := s.q.ListOpenPullRequests(ctx, db.ListOpenPullRequestsParams{
		ForgeProvider: int16(ForgeProviderGitHub),
		ForgeHost:     host,
		Repo:          repo,
	})
	if err != nil {
		return nil, fmt.Errorf("store: list open pull requests: %w", err)
	}
	out := make([]uint64, 0, len(rows))
	for _, number := range rows {
		out = append(out, uint64(number)) //nolint:gosec // G115: a stored PR number is a positive BIGINT written from a canonical uint64 forge number.
	}
	return out, nil
}

// SetPullRequestMergeQueue replaces only pr.mergeQueue, returning true when the value changed.
func (s *Store) SetPullRequestMergeQueue(ctx context.Context, coord ForgeCoord, entry []byte) (bool, error) {
	coord = coord.Normalized()
	if err := coord.valid(); err != nil {
		return false, err
	}
	if len(entry) == 0 {
		return false, fmt.Errorf("%w: merge queue entry is required", ErrInvalidArgument)
	}
	p := coord.db()
	affected, err := s.q.SetPullRequestMergeQueue(ctx, db.SetPullRequestMergeQueueParams{
		ForgeProvider: p.provider,
		ForgeHost:     p.host,
		Repo:          p.repo,
		Number:        p.number,
		Column5:       entry,
	})
	if err != nil {
		return false, fmt.Errorf("store: set pull request merge queue: %w", err)
	}
	return affected > 0, nil
}
