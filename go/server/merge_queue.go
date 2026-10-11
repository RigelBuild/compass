//go:build unix

package server

import (
	"context"
	"errors"
	"fmt"
	"google.golang.org/protobuf/encoding/protojson"
	"net/http"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/ingest"
	"github.com/RigelBuild/compass/go/internal/store"
)

type mergeQueueConfigStore struct {
	st   *store.Store
	host string
}

func (q *mergeQueueConfigStore) MergeQueueFor(ctx context.Context, repo string) (string, string, error) {
	return q.st.MergeQueueConfigFor(ctx, q.host, repo)
}

func (q *mergeQueueConfigStore) MergeQueueRepos(ctx context.Context) ([]string, error) {
	configs, err := q.st.MergeQueueConfigs(ctx, q.host)
	if err != nil {
		return nil, err
	}
	repos := make([]string, 0, len(configs))
	for _, config := range configs {
		repos = append(repos, config.Repo)
	}
	return repos, nil
}

func (q *mergeQueueConfigStore) MergeQueueHost() string { return q.host }

type mergeQueuePullRequestStore struct {
	st   *store.Store
	host string
}

func (s mergeQueuePullRequestStore) OpenPullRequests(ctx context.Context, repo string) ([]uint64, error) {
	return s.st.OpenPullRequests(ctx, s.host, repo)
}

type mergeQueueProjection struct {
	st  *store.Store
	brd *board.IssueProjection
}

func (p mergeQueueProjection) PublishPullRequestMergeQueue(ctx context.Context, coord store.ForgeCoord, entry *compassv1.MergeQueueEntry) error {
	body := []byte("null")
	if entry != nil {
		var err error
		body, err = protojson.Marshal(entry)
		if err != nil {
			return fmt.Errorf("marshal merge queue entry: %w", err)
		}
	}
	changed, err := p.brd.UpdatePullRequestMergeQueue(ctx, coord, body)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return p.brd.PublishPullRequestMergeQueue(ctx, coord, entry)
}

func mergeQueueProviders(host string, token func(context.Context) ([]byte, error), httpClient *http.Client) map[string]forge.MergeQueue {
	trunk := forge.NewTrunk(forge.TrunkConfig{Token: token, Host: host, HTTP: httpClient})
	return map[string]forge.MergeQueue{trunk.Name(): trunk}
}

func validateMergeQueueConfigs(configs []MergeQueueConfig) error {
	seen := make(map[string]struct{}, len(configs))
	for _, config := range configs {
		if config.Repo == "" || config.Branch == "" {
			return errors.New("merge queue repo and branch are required")
		}
		if config.Provider != "trunk" {
			return fmt.Errorf("unsupported merge queue provider %q for %q", config.Provider, config.Repo)
		}
		if _, exists := seen[config.Repo]; exists {
			return fmt.Errorf("duplicate merge queue repo %q", config.Repo)
		}
		seen[config.Repo] = struct{}{}
	}
	return nil
}

var _ ingest.QueueConfigs = (*mergeQueueConfigStore)(nil)
var _ ingest.MergeQueueStore = mergeQueuePullRequestStore{}
var _ ingest.MergeQueueSink = mergeQueueProjection{}

func reconcileMergeQueueConfigs(ctx context.Context, st *store.Store, host string, configs []MergeQueueConfig) error {
	for i := range configs {
		repo, err := normalizeMergeQueueRepo(configs[i].Repo)
		if err != nil {
			return err
		}
		configs[i].Repo = repo
	}
	if err := validateMergeQueueConfigs(configs); err != nil {
		return err
	}
	if len(configs) == 0 && st == nil {
		return nil
	}
	if st == nil {
		return errors.New("merge queue configuration requires a store")
	}
	if err := st.ClearForgeRepoMergeQueueConfigs(ctx, host); err != nil {
		return fmt.Errorf("clear forge merge queues: %w", err)
	}
	for _, config := range configs {
		if err := st.SetForgeRepoMergeQueueConfig(ctx, host, config.Repo, config.Provider, config.Branch); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("set forge merge queue %q: %w", config.Repo, err)
			}
			if err := st.EnsureForgeRepoSubscription(ctx, store.ForgeRepoSubscription{
				Provider:    store.ForgeProviderGitHub,
				Host:        host,
				Repo:        config.Repo,
				Enabled:     true,
				MergeQueue:  config.Provider,
				MergeBranch: config.Branch,
			}); err != nil {
				return fmt.Errorf("seed merge queue subscription %q: %w", config.Repo, err)
			}
		}
	}
	return nil
}

func normalizeMergeQueueRepo(raw string) (string, error) {
	return normalizeGitHubRepo(raw)
}
