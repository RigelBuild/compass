package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
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

// GetIssueConditional reads issue details through the repository owner's installation.
func (r *OwnerRoutedGitHub) GetIssueConditional(ctx context.Context, repo string, number uint64, etag string) (ConditionalResult[Issue], error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return ConditionalResult[Issue]{}, err
	}
	return client.GetIssueConditional(ctx, repo, number, etag)
}

// GetPullRequestConditional reads pull request details through the owner's installation.
func (r *OwnerRoutedGitHub) GetPullRequestConditional(ctx context.Context, repo string, number uint64, etag string) (ConditionalResult[PullRequest], error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return ConditionalResult[PullRequest]{}, err
	}
	return client.GetPullRequestConditional(ctx, repo, number, etag)
}

// ListComments reads artifact comments through the repository owner's installation.
func (r *OwnerRoutedGitHub) ListComments(ctx context.Context, repo string, kind compassv1internal.ForgeArtifactKind, number uint64, etag string) (ConditionalResult[[]Comment], error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return ConditionalResult[[]Comment]{}, err
	}
	return client.ListComments(ctx, repo, kind, number, etag)
}

// ChecksConditional reads the pull request check roll-up through the owner's installation.
func (r *OwnerRoutedGitHub) ChecksConditional(ctx context.Context, repo string, number uint64, headSHA, etag string) (ConditionalResult[Checks], error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return ConditionalResult[Checks]{}, err
	}
	return client.ChecksConditional(ctx, repo, number, headSHA, etag)
}

// ListNewArtifacts reads container additions through the repository owner's installation.
func (r *OwnerRoutedGitHub) ListNewArtifacts(ctx context.Context, repo string, kind compassv1internal.ForgeArtifactKind, sinceNumber uint64, etag string) (ConditionalResult[[]Issue], error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return ConditionalResult[[]Issue]{}, err
	}
	return client.ListNewArtifacts(ctx, repo, kind, sinceNumber, etag)
}

// ListUpdatedIssues reads changed issues through the repository owner's installation.
func (r *OwnerRoutedGitHub) ListUpdatedIssues(ctx context.Context, repo string, since time.Time, etag string) (ConditionalResult[UpdatedRows], error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return ConditionalResult[UpdatedRows]{}, err
	}
	return client.ListUpdatedIssues(ctx, repo, since, etag)
}

// PullRequestForSHA resolves a commit through the repository owner's installation.
func (r *OwnerRoutedGitHub) PullRequestForSHA(ctx context.Context, repo, sha string) (uint64, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return 0, err
	}
	return client.PullRequestForSHA(ctx, repo, sha)
}

// GetPullRequest reads pull request details through the repository owner's installation.
func (r *OwnerRoutedGitHub) GetPullRequest(ctx context.Context, repo string, number uint64) (PullRequest, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return PullRequest{}, err
	}
	return client.GetPullRequest(ctx, repo, number)
}

// ListOpenPullRequests reads open pull requests through the owner's installation.
func (r *OwnerRoutedGitHub) ListOpenPullRequests(ctx context.Context, repo string) ([]UpdatedPull, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return nil, err
	}
	return client.ListOpenPullRequests(ctx, repo)
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

func (r *OwnerRoutedGitHub) EditIssue(ctx context.Context, repo string, number uint64, in EditIssue) (Issue, error) {
	client, err := r.clientFor(ctx, repo)
	if err != nil {
		return Issue{}, err
	}
	return client.EditIssue(ctx, repo, number, in)
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
		return nil, fmt.Errorf("%w: %q", ErrMalformedRepo, repo)
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

var (
	_ Provider            = (*OwnerRoutedGitHub)(nil)
	_ NotifyReader        = (*OwnerRoutedGitHub)(nil)
	_ PullRequestResolver = (*OwnerRoutedGitHub)(nil)
)
