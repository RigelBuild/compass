package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// ForgeCoord names one forge issue or PR. It is comparable, so it keys maps.
type ForgeCoord struct {
	Provider ForgeProvider
	Host     string
	Repo     string
	Number   uint64
}

// PullRequestRow is one stored PR: the scalars the store reads plus the whole
// PR as protojson bytes.
type PullRequestRow struct {
	Coord     ForgeCoord
	State     string
	CreatedAt time.Time
	UpdatedAt time.Time
	PR        []byte
}

const (
	linkSourceExplicit   int16 = 1
	linkSourceClosingRef int16 = 2
)

// normalized trims and, on GitHub, lowercases the repo so board joins never miss on case.
func (c ForgeCoord) normalized() ForgeCoord {
	c.Repo = strings.TrimSpace(c.Repo)
	if c.Provider == ForgeProviderGitHub {
		c.Repo = strings.ToLower(c.Repo)
	}
	return c
}

func (c ForgeCoord) valid() error {
	if err := validCoordinate(c.Provider, c.Host, c.Repo); err != nil {
		return err
	}
	if c.Number == 0 {
		return fmt.Errorf("%w: number is required", ErrInvalidArgument)
	}
	return nil
}

// forgeCoordDB is a coordinate in its column types.
type forgeCoordDB struct {
	provider int16
	host     string
	repo     string
	number   int64
}

func (c ForgeCoord) db() forgeCoordDB {
	return forgeCoordDB{
		provider: int16(c.Provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum, always within int16
		host:     c.Host,
		repo:     c.Repo,
		number:   int64(c.Number), //nolint:gosec // G115: a canonical forge issue/PR number, well within int64
	}
}

func coordFromDB(provider int16, host, repo string, number int64) ForgeCoord {
	return ForgeCoord{
		Provider: ForgeProvider(provider),
		Host:     host,
		Repo:     repo,
		Number:   uint64(number), //nolint:gosec // G115: written only from a uint64 forge number
	}
}

func (pr PullRequestRow) normalized() (PullRequestRow, error) {
	pr.Coord = pr.Coord.normalized()
	if err := pr.Coord.valid(); err != nil {
		return PullRequestRow{}, err
	}
	if len(pr.PR) == 0 {
		return PullRequestRow{}, fmt.Errorf("%w: pull request body is required", ErrInvalidArgument)
	}
	if pr.CreatedAt.IsZero() || pr.UpdatedAt.IsZero() {
		return PullRequestRow{}, fmt.Errorf("%w: forge created and updated times are required", ErrInvalidArgument)
	}
	return pr, nil
}

// normalizeCoords validates and normalizes coordinates, dropping duplicates.
func normalizeCoords(in []ForgeCoord) ([]ForgeCoord, error) {
	out := make([]ForgeCoord, 0, len(in))
	seen := make(map[ForgeCoord]bool, len(in))
	for _, c := range in {
		c = c.normalized()
		if err := c.valid(); err != nil {
			return nil, err
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out, nil
}

func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// CreatePullRequestWithLink records an agent-created PR in one transaction: the
// ownership row, the PR row (an existing hydrated row wins) and, when issue is
// set, the explicit link. The ownership row keeps the caller's repo casing.
func (s *Store) CreatePullRequestWithLink(ctx context.Context, a AuthoredArtifact, pr PullRequestRow, issue *ForgeCoord) error {
	pr, err := pr.normalized()
	if err != nil {
		return err
	}
	if err := a.valid(); err != nil {
		return err
	}
	authored := ForgeCoord{Provider: a.Provider, Host: a.Host, Repo: a.Repo, Number: a.Number}.normalized()
	if a.Kind != ForgeArtifactKindPullRequest || authored != pr.Coord {
		return fmt.Errorf("%w: authored artifact does not name the pull request", ErrInvalidArgument)
	}
	var target ForgeCoord
	if issue != nil {
		target = issue.normalized()
		if err := target.valid(); err != nil {
			return err
		}
	}

	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return fmt.Errorf("store: begin create pull request: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	if err := recordAuthoredArtifact(ctx, qtx, a); err != nil {
		return err
	}
	p := pr.Coord.db()
	if err := qtx.InsertPullRequestIfAbsent(ctx, db.InsertPullRequestIfAbsentParams{
		ForgeProvider: p.provider, ForgeHost: p.host, Repo: p.repo, Number: p.number,
		ForgeState: pr.State, ForgeCreatedAt: timestamptz(pr.CreatedAt),
		ForgeUpdatedAt: timestamptz(pr.UpdatedAt), Pr: pr.PR,
	}); err != nil {
		return fmt.Errorf("store: insert pull request: %w", err)
	}
	if issue != nil {
		i := target.db()
		if err := qtx.UpsertExplicitPullRequestLink(ctx, db.UpsertExplicitPullRequestLinkParams{
			PrForgeProvider: p.provider, PrForgeHost: p.host, PrRepo: p.repo, PrNumber: p.number,
			IssueForgeProvider: i.provider, IssueForgeHost: i.host, IssueRepo: i.repo, IssueNumber: i.number,
		}); err != nil {
			return fmt.Errorf("store: link pull request: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit create pull request: %w", err)
	}
	return nil
}

// UpsertPullRequest writes a hydrated PR and replaces its closing-reference
// links; explicit links are never removed. It returns every issue linked before
// or after the write. An update older than the stored row is skipped and
// affects nothing.
func (s *Store) UpsertPullRequest(ctx context.Context, pr PullRequestRow, closingRefs []ForgeCoord) ([]ForgeCoord, error) {
	pr, err := pr.normalized()
	if err != nil {
		return nil, err
	}
	refs, err := normalizeCoords(closingRefs)
	if err != nil {
		return nil, err
	}

	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin upsert pull request: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	p := pr.Coord.db()
	written, err := qtx.UpsertPullRequestGuarded(ctx, db.UpsertPullRequestGuardedParams{
		ForgeProvider: p.provider, ForgeHost: p.host, Repo: p.repo, Number: p.number,
		ForgeState: pr.State, ForgeCreatedAt: timestamptz(pr.CreatedAt),
		ForgeUpdatedAt: timestamptz(pr.UpdatedAt), Pr: pr.PR,
	})
	if err != nil {
		return nil, fmt.Errorf("store: upsert pull request: %w", err)
	}
	if written == 0 {
		return nil, nil
	}
	affected, err := replaceClosingRefs(ctx, qtx, p, refs)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit upsert pull request: %w", err)
	}
	return affected, nil
}

// replaceClosingRefs makes the PR's closing-reference set equal refs and returns
// the union of the issues linked before and after.
func replaceClosingRefs(ctx context.Context, qtx *db.Queries, p forgeCoordDB, refs []ForgeCoord) ([]ForgeCoord, error) {
	old, err := qtx.ListPullRequestLinks(ctx, db.ListPullRequestLinksParams{
		PrForgeProvider: p.provider, PrForgeHost: p.host, PrRepo: p.repo, PrNumber: p.number,
	})
	if err != nil {
		return nil, fmt.Errorf("store: list pull request links: %w", err)
	}
	keep := make(map[ForgeCoord]bool, len(refs))
	for _, r := range refs {
		keep[r] = true
	}
	seen := make(map[ForgeCoord]bool, len(old)+len(refs))
	affected := make([]ForgeCoord, 0, len(old)+len(refs))
	for _, l := range old {
		c := coordFromDB(l.IssueForgeProvider, l.IssueForgeHost, l.IssueRepo, l.IssueNumber)
		seen[c] = true
		affected = append(affected, c)
		if l.Source != linkSourceClosingRef || keep[c] {
			continue
		}
		if err := qtx.DeleteClosingRefLink(ctx, db.DeleteClosingRefLinkParams{
			PrForgeProvider: p.provider, PrForgeHost: p.host, PrRepo: p.repo, PrNumber: p.number,
			IssueForgeProvider: l.IssueForgeProvider, IssueForgeHost: l.IssueForgeHost,
			IssueRepo: l.IssueRepo, IssueNumber: l.IssueNumber,
		}); err != nil {
			return nil, fmt.Errorf("store: unlink closing reference: %w", err)
		}
	}
	for _, r := range refs {
		i := r.db()
		if err := qtx.InsertClosingRefLink(ctx, db.InsertClosingRefLinkParams{
			PrForgeProvider: p.provider, PrForgeHost: p.host, PrRepo: p.repo, PrNumber: p.number,
			IssueForgeProvider: i.provider, IssueForgeHost: i.host, IssueRepo: i.repo, IssueNumber: i.number,
		}); err != nil {
			return nil, fmt.Errorf("store: link closing reference: %w", err)
		}
		if !seen[r] {
			seen[r] = true
			affected = append(affected, r)
		}
	}
	return affected, nil
}

// PullRequestsForIssues returns the PRs attached to each issue, oldest forge
// creation first, keyed by the normalized issue coordinate. A closing reference attaches a PR only when none of its
// explicit targets is a board issue.
func (s *Store) PullRequestsForIssues(ctx context.Context, issues []ForgeCoord) (map[ForgeCoord][]PullRequestRow, error) {
	want, err := normalizeCoords(issues)
	if err != nil {
		return nil, err
	}
	out := make(map[ForgeCoord][]PullRequestRow, len(want))
	if len(want) == 0 {
		return out, nil
	}
	arg := db.PullRequestsForIssuesParams{
		IssueProviders: make([]int16, 0, len(want)),
		IssueHosts:     make([]string, 0, len(want)),
		IssueRepos:     make([]string, 0, len(want)),
		IssueNumbers:   make([]int64, 0, len(want)),
	}
	for _, c := range want {
		d := c.db()
		arg.IssueProviders = append(arg.IssueProviders, d.provider)
		arg.IssueHosts = append(arg.IssueHosts, d.host)
		arg.IssueRepos = append(arg.IssueRepos, d.repo)
		arg.IssueNumbers = append(arg.IssueNumbers, d.number)
	}
	rows, err := s.q.PullRequestsForIssues(ctx, arg)
	if err != nil {
		return nil, fmt.Errorf("store: pull requests for issues: %w", err)
	}
	for _, r := range rows {
		issue := coordFromDB(r.IssueForgeProvider, r.IssueForgeHost, r.IssueRepo, r.IssueNumber)
		out[issue] = append(out[issue], PullRequestRow{
			Coord:     coordFromDB(r.ForgeProvider, r.ForgeHost, r.Repo, r.Number),
			State:     r.ForgeState,
			CreatedAt: r.ForgeCreatedAt.Time,
			UpdatedAt: r.ForgeUpdatedAt.Time,
			PR:        r.Pr,
		})
	}
	return out, nil
}

// FallbackIssuesForTarget returns the closing-reference issues of every PR whose
// explicit target is issue: the issues that gain or lose the PR when issue
// enters or leaves the board.
func (s *Store) FallbackIssuesForTarget(ctx context.Context, issue ForgeCoord) ([]ForgeCoord, error) {
	issue = issue.normalized()
	if err := issue.valid(); err != nil {
		return nil, err
	}
	i := issue.db()
	rows, err := s.q.FallbackIssuesForTarget(ctx, db.FallbackIssuesForTargetParams{
		IssueForgeProvider: i.provider, IssueForgeHost: i.host, IssueRepo: i.repo, IssueNumber: i.number,
	})
	if err != nil {
		return nil, fmt.Errorf("store: fallback issues for target: %w", err)
	}
	out := make([]ForgeCoord, 0, len(rows))
	for _, r := range rows {
		out = append(out, coordFromDB(r.IssueForgeProvider, r.IssueForgeHost, r.IssueRepo, r.IssueNumber))
	}
	return out, nil
}

// PullRequestUpdatedAt reads the stored forge_updated_at of pr; ok is false when
// the PR was never stored. The sweep hydrates only rows newer than this.
func (s *Store) PullRequestUpdatedAt(ctx context.Context, pr ForgeCoord) (time.Time, bool, error) {
	pr = pr.normalized()
	if err := pr.valid(); err != nil {
		return time.Time{}, false, err
	}
	p := pr.db()
	at, err := s.q.PullRequestForgeUpdatedAt(ctx, db.PullRequestForgeUpdatedAtParams{
		ForgeProvider: p.provider, ForgeHost: p.host, Repo: p.repo, Number: p.number,
	})
	if noRows(err) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: pull request updated at: %w", err)
	}
	return at.Time, true, nil
}
