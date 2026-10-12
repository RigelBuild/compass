package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const trunkBatchSize = 50

// TrunkConfig configures read access to the Trunk Merge Queue API.
type TrunkConfig struct {
	Token func(ctx context.Context) ([]byte, error)
	Host  string
	HTTP  *http.Client
}

// Trunk reads submitted pull request state from Trunk Merge Queue.
type Trunk struct {
	cfg TrunkConfig
}

// NewTrunk returns a Trunk merge-queue provider.
func NewTrunk(cfg TrunkConfig) *Trunk {
	if cfg.Host == "" {
		cfg.Host = "github.com"
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second}
	} else {
		copyClient := *cfg.HTTP
		cfg.HTTP = &copyClient
	}
	cfg.HTTP.CheckRedirect = noTrunkRedirects
	return &Trunk{cfg: cfg}
}

func noTrunkRedirects(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// Name returns the provider key used by merge-queue configuration.
func (*Trunk) Name() string { return "trunk" }

// Entries reads queue state for one repository in batches of at most 50 PRs.
func (t *Trunk) Entries(ctx context.Context, repo, targetBranch string, numbers []uint64) (map[uint64]QueueEntry, error) {
	out := make(map[uint64]QueueEntry, len(numbers))
	if len(numbers) == 0 {
		return out, nil
	}
	for start := 0; start < len(numbers); start += trunkBatchSize {
		end := min(start+trunkBatchSize, len(numbers))
		batch, err := t.entriesAt(ctx, trunkAPIBase, repo, targetBranch, numbers[start:end])
		if err != nil {
			return nil, err
		}
		maps.Copy(out, batch)
	}
	return out, nil
}

const trunkAPIBase = "https://api.trunk.io"

func (t *Trunk) entriesAt(ctx context.Context, base, repo, targetBranch string, numbers []uint64) (map[uint64]QueueEntry, error) {
	if len(numbers) > trunkBatchSize {
		return nil, fmt.Errorf("forge: Trunk batch has %d pull requests, maximum is %d", len(numbers), trunkBatchSize)
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("forge: invalid Trunk repo %q: want owner/name", repo)
	}
	if t.cfg.Token == nil {
		return nil, errors.New("forge: Trunk token resolver is required")
	}
	token, err := t.cfg.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("forge: resolve Trunk token: %w", err)
	}
	prs := make([]trunkPRRequest, 0, len(numbers))
	for _, number := range numbers {
		prs = append(prs, trunkPRRequest{Number: number})
	}
	body, err := json.Marshal(trunkRequest{
		Repo:         trunkRepo{Host: t.cfg.Host, Owner: owner, Name: name},
		TargetBranch: targetBranch,
		PRs:          prs,
	})
	if err != nil {
		return nil, fmt.Errorf("forge: marshal Trunk request: %w", err)
	}
	endpoint, err := url.JoinPath(base, "/v1/getSubmittedPullRequests")
	if err != nil {
		return nil, fmt.Errorf("forge: build Trunk endpoint: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("forge: build Trunk request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Token", string(token))
	resp, err := t.cfg.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("forge: request Trunk merge queue: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("forge: Trunk merge queue returned %s", resp.Status)
	}
	var result trunkResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("forge: decode Trunk response: %w", err)
	}
	out := make(map[uint64]QueueEntry, len(result.PullRequests))
	for _, item := range result.PullRequests {
		state, ok := trunkQueueState(item.State)
		if !ok {
			return nil, fmt.Errorf("forge: unknown Trunk queue state %q", item.State)
		}
		if !slices.Contains(numbers, item.PRNumber) {
			return nil, fmt.Errorf("forge: Trunk returned unrequested pull request #%d", item.PRNumber)
		}
		if !item.IsCurrentlySubmittedToQueue && (state == QueueQueued || state == QueueTesting) {
			state = QueueRemoved
		}

		changedAt, err := time.Parse(time.RFC3339Nano, item.StateChangedAt)
		if err != nil {
			return nil, fmt.Errorf("forge: parse Trunk stateChangedAt for pull request #%d: %w", item.PRNumber, err)
		}
		out[item.PRNumber] = QueueEntry{Number: item.PRNumber, State: state, ChangedAt: changedAt}
	}
	return out, nil
}

func trunkQueueState(state string) (QueueState, bool) {
	switch state {
	case "not_ready", "pending":
		return QueueQueued, true
	case "testing", "tests_passed":
		return QueueTesting, true
	case "failed", "pending_failure":
		return QueueFailed, true
	case "cancelled":
		return QueueRemoved, true
	case "merged":
		return QueueMerged, true
	default:
		return 0, false
	}
}

type trunkRequest struct {
	Repo         trunkRepo        `json:"repo"`
	TargetBranch string           `json:"targetBranch"`
	PRs          []trunkPRRequest `json:"prs"`
}

type trunkRepo struct {
	Host  string `json:"host"`
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

type trunkPRRequest struct {
	Number uint64 `json:"number"`
}

type trunkResponse struct {
	PullRequests []trunkPullRequest `json:"pullRequests"`
	NotFound     []trunkPRRequest   `json:"notFound"`
}

type trunkPullRequest struct {
	PRNumber                    uint64 `json:"prNumber"`
	State                       string `json:"state"`
	StateChangedAt              string `json:"stateChangedAt"`
	IsCurrentlySubmittedToQueue bool   `json:"isCurrentlySubmittedToQueue"`
}
