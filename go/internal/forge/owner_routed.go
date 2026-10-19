package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrInstallationLookup marks a failure to resolve a repository owner's App installation.
var ErrInstallationLookup = errors.New("forge: installation lookup failed")

// OwnerRoutedGitHub routes each repository operation through its owner's App installation.
type OwnerRoutedGitHub struct {
	primary    *GitHub
	resolver   *InstallationResolver
	cfg        GitHubAppConfig
	httpClient *http.Client

	mu      sync.Mutex
	clients map[int64]*GitHub
}

// NewOwnerRoutedGitHub builds a GitHub provider that shares primary for the default installation.
func NewOwnerRoutedGitHub(primary *GitHub, resolver *InstallationResolver, cfg GitHubAppConfig, httpClient *http.Client) *OwnerRoutedGitHub {
	if httpClient == nil {
		httpClient = cfg.Client
	}
	return &OwnerRoutedGitHub{
		primary:    primary,
		resolver:   resolver,
		cfg:        cfg,
		httpClient: httpClient,
		clients:    make(map[int64]*GitHub),
	}
}

func (r *OwnerRoutedGitHub) Name() string { return r.primary.Name() }

func (r *OwnerRoutedGitHub) BodyLimit() int { return r.primary.BodyLimit() }

// AppLogin returns the bot login from the shared primary App client.
func (r *OwnerRoutedGitHub) AppLogin(ctx context.Context) (string, error) {
	return r.primary.AppLogin(ctx)
}

func (r *OwnerRoutedGitHub) CreateIssue(ctx context.Context, repo string, in CreateIssue) (Issue, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return Issue{}, err
	}
	return client.CreateIssue(ctx, repo, in)
}

func (r *OwnerRoutedGitHub) CommentOnIssue(ctx context.Context, repo string, number uint64, body, clientID string) (Comment, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return Comment{}, err
	}
	return client.CommentOnIssue(ctx, repo, number, body, clientID)
}

func (r *OwnerRoutedGitHub) EditComment(ctx context.Context, repo, commentKey, body string) (Comment, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return Comment{}, err
	}
	return client.EditComment(ctx, repo, commentKey, body)
}

func (r *OwnerRoutedGitHub) DeleteComment(ctx context.Context, repo, commentKey string) error {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return err
	}
	return client.DeleteComment(ctx, repo, commentKey)
}

func (r *OwnerRoutedGitHub) ListCommentsSince(ctx context.Context, repo string, number uint64, since time.Time) ([]Comment, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return nil, err
	}
	return client.ListCommentsSince(ctx, repo, number, since)
}

func (r *OwnerRoutedGitHub) GetIssue(ctx context.Context, repo string, number uint64) (Issue, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return Issue{}, err
	}
	return client.GetIssue(ctx, repo, number)
}

func (r *OwnerRoutedGitHub) ListIssues(ctx context.Context, repo string, filter IssueFilter) ([]Issue, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return nil, err
	}
	return client.ListIssues(ctx, repo, filter)
}

func (r *OwnerRoutedGitHub) CreatePullRequest(ctx context.Context, repo string, in CreatePR) (PullRequest, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return PullRequest{}, err
	}
	return client.CreatePullRequest(ctx, repo, in)
}

func (r *OwnerRoutedGitHub) UpdatePullRequest(ctx context.Context, repo string, number uint64, in UpdatePR) (PullRequest, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return PullRequest{}, err
	}
	return client.UpdatePullRequest(ctx, repo, number, in)
}

func (r *OwnerRoutedGitHub) FindPullRequestByHead(ctx context.Context, repo, headRef string) (PullRequest, bool, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return PullRequest{}, false, err
	}
	return client.FindPullRequestByHead(ctx, repo, headRef)
}

func (r *OwnerRoutedGitHub) FindByWriteMarker(ctx context.Context, repo string, kind WriteKind, parent uint64, intentID string, since time.Time) (WrittenRef, bool, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return WrittenRef{}, false, err
	}
	return client.FindByWriteMarker(ctx, repo, kind, parent, intentID, since)
}

func (r *OwnerRoutedGitHub) CommentOnPullRequest(ctx context.Context, repo string, number uint64, body, clientID string) (Comment, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return Comment{}, err
	}
	return client.CommentOnPullRequest(ctx, repo, number, body, clientID)
}

func (r *OwnerRoutedGitHub) SubmitReview(ctx context.Context, repo string, number uint64, in SubmitReview) (SubmittedReview, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return SubmittedReview{}, err
	}
	return client.SubmitReview(ctx, repo, number, in)
}

func (r *OwnerRoutedGitHub) GetPullRequest(ctx context.Context, repo string, number uint64) (PullRequest, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return PullRequest{}, err
	}
	return client.GetPullRequest(ctx, repo, number)
}

func (r *OwnerRoutedGitHub) Checks(ctx context.Context, repo string, number uint64) (Checks, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return Checks{}, err
	}
	return client.Checks(ctx, repo, number)
}

func (r *OwnerRoutedGitHub) TransitionIssueState(ctx context.Context, repo string, number uint64, in TransitionState) (Issue, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return Issue{}, err
	}
	return client.TransitionIssueState(ctx, repo, number, in)
}

func (r *OwnerRoutedGitHub) TransitionPullRequestState(ctx context.Context, repo string, number uint64, in TransitionState) (PullRequest, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return PullRequest{}, err
	}
	return client.TransitionPullRequestState(ctx, repo, number, in)
}
func (r *OwnerRoutedGitHub) clientFor(ctx context.Context, repo string) (*GitHub, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("forge: malformed github repo %q, want owner/name", repo)
	}
	if r.resolver == nil {
		return nil, fmt.Errorf("%w: %w", ErrInstallationLookup, errors.New("forge: installation resolver is unavailable"))
	}
	installationID, err := r.resolver.InstallationFor(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInstallationLookup, err)
	}
	if installationID == r.resolver.DefaultInstallation() {
		return r.primary, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if client := r.clients[installationID]; client != nil {
		return client, nil
	}
	cfg := r.cfg
	cfg.InstallationID = installationID
	cfg.Client = r.httpClient
	token, err := NewAppTokenSource(cfg)
	if err != nil {
		return nil, err
	}
	client := NewGitHub(GitHubConfig{Host: cfg.Host, Token: token, Client: r.httpClient})
	r.clients[installationID] = client
	return client, nil
}

var _ Provider = (*OwnerRoutedGitHub)(nil)
