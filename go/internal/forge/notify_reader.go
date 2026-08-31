package forge

// The reconcile sweep's conditional-read capability (RIG-2732 T5,
// design.md:894-942). NotifyReader is a CAPABILITY interface, deliberately NOT
// a Provider widening: Provider carries a //nolint:interfacebloat waiver and
// deliberately unconditional reads (provider.go:218-243); the conditional reads
// the backstop sweep needs (If-None-Match / 304, the container LIST) are their
// own seam, satisfied structurally by *GitHub and *Linear (the board driver's
// structural pageLister precedent, driver.go:33-37).
//
// The GitHub arm is the real conditional path: a sibling of getJSON carrying
// If-None-Match, a 304 short-circuit (budget recorded — a 304 on an authorized
// request is NOT charged, github.go:60-67), and the getAllPages Link-chain walk
// for the >1-page cases. The Linear arm has no ETags (GraphQL): its reads return
// 200-equivalents with an empty ETag (a documented limitation, acceptable at the
// tens-of-minutes backstop cadence against Linear's separate rate bucket); the
// PR/checks arms are ErrUnsupported (Linear is issues-only, linear.go:277-302).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// ConditionalResult is one conditional read's outcome. NotModified true is a 304
// (V is the zero value and ETag is empty — the caller keeps its stored value and
// ETag); NotModified false carries the fresh V and the new ETag to re-store. It
// is the REAL type T4's ChecksResult placeholder stood in for
// (notify_router.go, collapsed by T5); the ChecksRoller seam is
// ConditionalResult[Checks].
type ConditionalResult[V any] struct {
	V           V
	ETag        string
	NotModified bool
}

// NotifyReader is the per-endpoint conditional-read surface the reconcile sweep
// (T5) drives, and the go/server ChecksRoller adapter (T7) binds
// ChecksConditional onto. Every read is conditioned on the caller's stored ETag;
// a 304 returns NotModified with no body parse.
type NotifyReader interface {
	// GetIssueConditional conditionally reads one issue's current state.
	GetIssueConditional(ctx context.Context, repo string, number uint64, etag string) (ConditionalResult[Issue], error)
	// GetPullRequestConditional conditionally reads one PR's detail (state,
	// coordinate). Checks ride ChecksConditional; comments ride ListComments.
	GetPullRequestConditional(ctx context.Context, repo string, number uint64, etag string) (ConditionalResult[PullRequest], error)
	// ListComments reads the artifact's comment set, page-1 conditioned and
	// NEWEST-first (sort=created&direction=desc), so a new comment always
	// changes page 1: a 304 = no new comments in one request; a miss walks the
	// remaining pages (the getAllPages idiom) for the full set.
	ListComments(ctx context.Context, repo string, kind compassv1internal.ForgeArtifactKind, number uint64, etag string) (ConditionalResult[[]Comment], error)
	// ChecksConditional conditionally reads the COMBINED CI/status roll-up for a
	// PR head. headSHA "" resolves it from the pull detail first (the sweep has
	// no webhook SHA); a non-empty headSHA (the webhook ChecksRoller path) skips
	// that fetch.
	ChecksConditional(ctx context.Context, repo string, number uint64, headSHA, etag string) (ConditionalResult[Checks], error)
	// ListNewArtifacts reads the container-scope artifacts of kind opened above
	// sinceNumber, newest-first. Two contract points (design.md:915-928):
	//  (1) GitHub's /repos/{repo}/issues interleaves PRs with issues (each PR
	//      row carries a pull_request marker); kind=ISSUE filters them out,
	//      kind=PULL_REQUEST uses /repos/{repo}/pulls (a different endpoint).
	//  (2) The ETag conditions page 1 only; on a miss the walk continues until a
	//      page's OLDEST number is <= sinceNumber, so a >1-page burst is never
	//      truncated to page 1.
	ListNewArtifacts(ctx context.Context, repo string, kind compassv1internal.ForgeArtifactKind, sinceNumber uint64, etag string) (ConditionalResult[[]Issue], error)
}

// --- GitHub arm --------------------------------------------------------------

// Compile-time proof the GitHub client satisfies the conditional-read surface.
var _ NotifyReader = (*GitHub)(nil)

// getJSONCond is the conditional sibling of getJSON: it sends If-None-Match when
// etag != "", short-circuits a 304 (recording budget, no body parse — a 304 on
// an authorized request is uncharged, github.go:60-67), and on a 2xx decodes
// into out and reports the fresh ETag plus whether an RFC-5988 rel="next" Link
// is present. On error it owns the budget decision via mapErrorResponse (no
// budget record on error — the ListIssuesPage/getJSON rule).
func (g *GitHub) getJSONCond(ctx context.Context, url, etag string, out any) (notModified bool, newETag string, hasNext bool, err error) {
	if hint, blocked := g.gateBlocked(); blocked {
		return false, "", false, fmt.Errorf("GET %s: %w", url, &RateLimitError{RetryAfter: hint})
	}
	token, terr := g.token.Token(ctx)
	if terr != nil {
		return false, "", false, fmt.Errorf("resolve token: %w", terr)
	}
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if rerr != nil {
		return false, "", false, fmt.Errorf("build request: %w", rerr)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, derr := g.client.Do(req)
	if derr != nil {
		return false, "", false, fmt.Errorf("do request: %w", derr)
	}
	defer func() { _ = resp.Body.Close() }() // read-only GET; body drained/closed below, no actionable close error

	switch {
	case resp.StatusCode == http.StatusNotModified:
		g.recordBudget(resp)
		return true, "", false, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		g.recordBudget(resp)
	default:
		return false, "", false, g.mapErrorResponse(resp)
	}

	body, berr := io.ReadAll(resp.Body)
	if berr != nil {
		return false, "", false, fmt.Errorf("read body: %w", berr)
	}
	if uerr := json.Unmarshal(body, out); uerr != nil {
		return false, "", false, fmt.Errorf("decode response: %w", uerr)
	}
	return false, resp.Header.Get("ETag"), hasNextLink(resp.Header.Get("Link")), nil
}

// GetIssueConditional conditionally reads one issue (body RAW).
func (g *GitHub) GetIssueConditional(ctx context.Context, repo string, number uint64, etag string) (ConditionalResult[Issue], error) {
	url := g.apiBase() + "/repos/" + repo + "/issues/" + strconv.FormatUint(number, 10)
	var row ghIssue
	notMod, newETag, _, err := g.getJSONCond(ctx, url, etag, &row)
	if err != nil {
		return ConditionalResult[Issue]{}, fmt.Errorf("forge: github get issue conditional %q#%d: %w", repo, number, err)
	}
	if notMod {
		return ConditionalResult[Issue]{NotModified: true}, nil
	}
	return ConditionalResult[Issue]{V: row.toIssue(), ETag: newETag}, nil
}

// GetPullRequestConditional conditionally reads a PR's detail (State folded to
// the domain's open|closed|merged). Reviews/checks/threads are NOT composited
// here: the snapshot's checks half rides ChecksConditional and its comment half
// rides ListComments, so this conditional pays one GET, not three.
func (g *GitHub) GetPullRequestConditional(ctx context.Context, repo string, number uint64, etag string) (ConditionalResult[PullRequest], error) {
	url := g.apiBase() + "/repos/" + repo + "/pulls/" + strconv.FormatUint(number, 10)
	var detail ghPullDetail
	notMod, newETag, _, err := g.getJSONCond(ctx, url, etag, &detail)
	if err != nil {
		return ConditionalResult[PullRequest]{}, fmt.Errorf("forge: github get pull request conditional %q#%d: %w", repo, number, err)
	}
	if notMod {
		return ConditionalResult[PullRequest]{NotModified: true}, nil
	}
	return ConditionalResult[PullRequest]{V: detail.toPullRequest(), ETag: newETag}, nil
}

// ListComments reads an issue/PR's comment set. GitHub serves PR conversation
// comments on the same issues/{n}/comments endpoint, so kind is informational.
// Page 1 is conditioned (newest-first); a 304 short-circuits (no new comments).
// A miss walks the remaining pages UNCONDITIONALLY (page 1 is not re-fetched) so
// the returned set is complete, and returns page 1's ETag to re-store.
func (g *GitHub) ListComments(ctx context.Context, repo string, _ compassv1internal.ForgeArtifactKind, number uint64, etag string) (ConditionalResult[[]Comment], error) {
	base := g.apiBase() + "/repos/" + repo + "/issues/" + strconv.FormatUint(number, 10) + "/comments?sort=created&direction=desc"
	p1 := base + "&per_page=" + strconv.Itoa(perPage) + "&page=1"
	var first []ghComment
	notMod, newETag, hasNext, err := g.getJSONCond(ctx, p1, etag, &first)
	if err != nil {
		return ConditionalResult[[]Comment]{}, fmt.Errorf("forge: github list comments %q#%d: %w", repo, number, err)
	}
	if notMod {
		return ConditionalResult[[]Comment]{NotModified: true}, nil
	}
	out := make([]Comment, 0, len(first))
	for _, c := range first {
		out = append(out, c.toComment())
	}
	for page := 2; hasNext; page++ {
		u := base + "&per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page)
		var rows []ghComment
		_, _, hn, perr := g.getJSONCond(ctx, u, "", &rows)
		if perr != nil {
			return ConditionalResult[[]Comment]{}, fmt.Errorf("forge: github list comments %q#%d: %w", repo, number, perr)
		}
		for _, c := range rows {
			out = append(out, c.toComment())
		}
		hasNext = hn
	}
	return ConditionalResult[[]Comment]{V: out, ETag: newETag}, nil
}

// ChecksConditional conditionally reads the COMBINED roll-up for a PR head. It
// probes the check-runs page-1 endpoint conditionally (newest CI activity
// changes page 1); a 304 short-circuits. On a miss it re-folds the full roll-up
// via checksForSHA (both the modern check-runs and legacy combined-status
// sources, each walked to completion) so a later-page or status-only change is
// never truncated. headSHA "" is resolved from the pull detail first.
func (g *GitHub) ChecksConditional(ctx context.Context, repo string, number uint64, headSHA, etag string) (ConditionalResult[Checks], error) {
	sha := headSHA
	if sha == "" {
		var detail ghPullDetail
		u := g.apiBase() + "/repos/" + repo + "/pulls/" + strconv.FormatUint(number, 10)
		if _, err := g.getJSON(ctx, u, &detail); err != nil {
			return ConditionalResult[Checks]{}, fmt.Errorf("forge: github checks conditional %q#%d: %w", repo, number, err)
		}
		sha = detail.Head.SHA
	}
	p1 := g.apiBase() + "/repos/" + repo + "/commits/" + sha + "/check-runs?per_page=" + strconv.Itoa(perPage) + "&page=1"
	var firstRuns ghCheckRuns
	notMod, newETag, _, err := g.getJSONCond(ctx, p1, etag, &firstRuns)
	if err != nil {
		return ConditionalResult[Checks]{}, fmt.Errorf("forge: github checks conditional %q#%d: %w", repo, number, err)
	}
	if notMod {
		return ConditionalResult[Checks]{NotModified: true}, nil
	}
	checks, ferr := g.checksForSHA(ctx, repo, sha)
	if ferr != nil {
		return ConditionalResult[Checks]{}, fmt.Errorf("forge: github checks conditional %q#%d: %w", repo, number, ferr)
	}
	return ConditionalResult[Checks]{V: checks, ETag: newETag}, nil
}

// ListNewArtifacts reads the container's artifacts opened above sinceNumber. For
// kind=ISSUE it walks /repos/{repo}/issues (filtering out the PR rows GitHub
// interleaves, discriminated by the pull_request marker); for kind=PULL_REQUEST
// it walks /repos/{repo}/pulls (a different endpoint). Page 1 is conditioned;
// the walk continues until a page's oldest number is <= sinceNumber.
func (g *GitHub) ListNewArtifacts(ctx context.Context, repo string, kind compassv1internal.ForgeArtifactKind, sinceNumber uint64, etag string) (ConditionalResult[[]Issue], error) {
	switch kind {
	case compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE:
		base := g.apiBase() + "/repos/" + repo + "/issues?state=all&sort=created&direction=desc"
		return ghWalkNewArtifacts(ctx, g, base, sinceNumber, etag,
			func(r ghIssue) uint64 { return r.Number },
			func(r ghIssue) (Issue, bool) {
				// Drop the PR rows GitHub interleaves into /issues (a *json.Raw
				// unmarshals an explicit "pull_request": null to a non-nil
				// RawMessage("null") — guard that, mirroring ListIssuesPage).
				if raw := r.PullRequest; raw != nil && len(*raw) > 0 && string(*raw) != jsonNull {
					return Issue{}, false
				}
				return r.toIssue(), true
			})
	case compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST:
		base := g.apiBase() + "/repos/" + repo + "/pulls?state=all&sort=created&direction=desc"
		return ghWalkNewArtifacts(ctx, g, base, sinceNumber, etag,
			func(r ghPull) uint64 { return r.Number },
			func(r ghPull) (Issue, bool) {
				return Issue{Number: r.Number, State: r.State, URL: r.HTMLURL}, true
			})
	default:
		return ConditionalResult[[]Issue]{}, fmt.Errorf("forge: github list new artifacts %q: %w", repo, ErrUnsupported)
	}
}

// ghWalkNewArtifacts walks base newest-first (page 1 conditioned on etag),
// collecting rows whose number is above sinceNumber and mapped/kept by keep,
// until a page's oldest number is <= sinceNumber (or no rel="next" remains). A
// page-1 304 returns NotModified. It returns page 1's ETag to re-store.
func ghWalkNewArtifacts[R any](ctx context.Context, g *GitHub, base string, sinceNumber uint64, etag string, num func(R) uint64, keep func(R) (Issue, bool)) (ConditionalResult[[]Issue], error) {
	var out []Issue
	pageETag := ""
	for page := 1; ; page++ {
		u := base + "&per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page)
		sendETag := ""
		if page == 1 {
			sendETag = etag
		}
		var rows []R
		notMod, e, hasNext, err := g.getJSONCond(ctx, u, sendETag, &rows)
		if err != nil {
			return ConditionalResult[[]Issue]{}, err
		}
		if page == 1 {
			if notMod {
				return ConditionalResult[[]Issue]{NotModified: true}, nil
			}
			pageETag = e
		}
		reachedOld := false
		for _, r := range rows {
			if num(r) <= sinceNumber {
				// Newest-first: this and everything after it is old.
				reachedOld = true
				continue
			}
			if iss, ok := keep(r); ok {
				out = append(out, iss)
			}
		}
		if reachedOld || !hasNext {
			break
		}
	}
	return ConditionalResult[[]Issue]{V: out, ETag: pageETag}, nil
}

// ListUpdatedIssues walks /repos/{repo}/issues?state=all&sort=updated&
// direction=desc newest-updated-first (page 1 conditioned on etag; a 304 =>
// NotModified), collecting issue rows (PR rows dropped by the pull_request
// marker, mirroring ListNewArtifacts) until a page's oldest updated_at is
// strictly < since, or no rel="next" remains. Rows with updated_at == since are
// RE-included: GitHub's updated_at is second-granularity, so a <= stop would
// permanently exclude an issue updated in the same second as the stored
// watermark after the sweep read it; the duplicates are free by coordinate
// idempotency. A zero since walks ALL pages (cold start). It returns page 1's
// ETag to re-store. This is the updated-order sibling of ListNewArtifacts
// (created-order, number-keyed): the reconcile/backfill read cannot see updates
// to existing issues via the created-order walk.
func (g *GitHub) ListUpdatedIssues(ctx context.Context, repo string, since time.Time, etag string) (ConditionalResult[[]Issue], error) {
	base := g.apiBase() + "/repos/" + repo + "/issues?state=all&sort=updated&direction=desc"
	var out []Issue
	pageETag := ""
	for page := 1; ; page++ {
		u := base + "&per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page)
		sendETag := ""
		if page == 1 {
			sendETag = etag
		}
		var rows []ghIssue
		notMod, e, hasNext, err := g.getJSONCond(ctx, u, sendETag, &rows)
		if err != nil {
			return ConditionalResult[[]Issue]{}, fmt.Errorf("forge: github list updated issues %q: %w", repo, err)
		}
		if page == 1 {
			if notMod {
				return ConditionalResult[[]Issue]{NotModified: true}, nil
			}
			pageETag = e
		}
		reachedOld := false
		for _, r := range rows {
			iss := r.toIssue()
			if !iss.UpdatedAt.IsZero() && iss.UpdatedAt.Before(since) {
				// Newest-updated-first: strictly older than the watermark, so this
				// and everything after it is old. A row == since is NOT Before it,
				// so it is re-included (second-granularity dedup safety). A row
				// whose updated_at failed to parse (zero time) is NOT a stop
				// signal — treating it as one would let a single malformed row
				// truncate the whole sweep persistently; skip it and keep walking.
				reachedOld = true
				continue
			}
			if iss.UpdatedAt.IsZero() {
				continue
			}
			// Drop the PR rows GitHub interleaves into /issues (mirroring
			// ListNewArtifacts / ListIssuesPage's pull_request-marker guard).
			if raw := r.PullRequest; raw != nil && len(*raw) > 0 && string(*raw) != jsonNull {
				continue
			}
			out = append(out, iss)
		}
		if reachedOld || !hasNext {
			break
		}
	}
	return ConditionalResult[[]Issue]{V: out, ETag: pageETag}, nil
}

// --- Linear arm --------------------------------------------------------------

// Compile-time proof the Linear client satisfies the conditional-read surface.
var _ NotifyReader = (*Linear)(nil)

// GetIssueConditional reads one issue. Linear's GraphQL has no ETags, so this is
// a 200-equivalent with an empty ETag (a documented backstop limitation): the
// sweep's diff still detects a state change, it just cannot 304 to save the read.
func (l *Linear) GetIssueConditional(ctx context.Context, repo string, number uint64, _ string) (ConditionalResult[Issue], error) {
	iss, err := l.GetIssue(ctx, repo, number)
	if err != nil {
		return ConditionalResult[Issue]{}, err
	}
	return ConditionalResult[Issue]{V: iss}, nil
}

// GetPullRequestConditional is unsupported (Linear is issues-only, DL-051).
func (l *Linear) GetPullRequestConditional(_ context.Context, _ string, _ uint64, _ string) (ConditionalResult[PullRequest], error) {
	return ConditionalResult[PullRequest]{}, ErrUnsupported
}

// ListComments reads an issue's comment set (no PRs on Linear). No ETags, so
// this is a 200-equivalent with an empty ETag; comment identity is Key-keyed
// (Linear comment ids are UUIDs, toComment sets Comment.Key, leaves ID zero).
// Paginated.
func (l *Linear) ListComments(ctx context.Context, repo string, kind compassv1internal.ForgeArtifactKind, number uint64, _ string) (ConditionalResult[[]Comment], error) {
	if kind == compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST {
		return ConditionalResult[[]Comment]{}, ErrUnsupported
	}
	const query = `query CompassIssueComments($filter: IssueFilter!, $after: String) {
  issues(filter: $filter, first: 1) {
    nodes {
      comments(first: 50, after: $after) {
        nodes { id url body user { displayName } }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`
	filter := map[string]any{
		varTeam:   map[string]any{varKey: map[string]any{"eq": repo}},
		varNumber: map[string]any{"eq": float64(number)},
	}
	var out []Comment
	var after string
	for {
		vars := map[string]any{varFilter: filter}
		if after != "" {
			vars["after"] = after
		}
		var resp struct {
			Issues struct {
				Nodes []struct {
					Comments struct {
						Nodes    []linearComment `json:"nodes"`
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"comments"`
				} `json:"nodes"`
			} `json:"issues"`
		}
		if err := l.doGraphQL(ctx, query, vars, &resp); err != nil {
			return ConditionalResult[[]Comment]{}, fmt.Errorf("forge: linear list comments %q#%d: %w", repo, number, err)
		}
		if len(resp.Issues.Nodes) == 0 {
			break
		}
		conn := resp.Issues.Nodes[0].Comments
		for _, c := range conn.Nodes {
			out = append(out, c.toComment())
		}
		if !conn.PageInfo.HasNextPage || conn.PageInfo.EndCursor == "" {
			break
		}
		after = conn.PageInfo.EndCursor
	}
	return ConditionalResult[[]Comment]{V: out}, nil
}

// ChecksConditional is unsupported (Linear has no PR head checks).
func (l *Linear) ChecksConditional(_ context.Context, _ string, _ uint64, _, _ string) (ConditionalResult[Checks], error) {
	return ConditionalResult[Checks]{}, ErrUnsupported
}

// ListNewArtifacts walks the team-keyed issues above sinceNumber, newest-first.
// The read selection gains the issue's project id so a routed OPENED event
// carries ForgeEvent.Project for the container project match (W2). No ETags, so
// this is a 200-equivalent with an empty ETag; PRs are unsupported on Linear.
func (l *Linear) ListNewArtifacts(ctx context.Context, repo string, kind compassv1internal.ForgeArtifactKind, sinceNumber uint64, _ string) (ConditionalResult[[]Issue], error) {
	if kind == compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST {
		return ConditionalResult[[]Issue]{}, ErrUnsupported
	}
	const query = `query CompassNewIssues($filter: IssueFilter!, $after: String) {
  issues(filter: $filter, first: 50, after: $after) {
    nodes {
      number
      url
      state { name type }
      project { id }
    }
    pageInfo { hasNextPage endCursor }
  }
}`
	filter := map[string]any{
		varTeam:   map[string]any{varKey: map[string]any{"eq": repo}},
		varNumber: map[string]any{"gt": float64(sinceNumber)},
	}
	var out []Issue
	var after string
	for {
		vars := map[string]any{varFilter: filter}
		if after != "" {
			vars["after"] = after
		}
		var resp struct {
			Issues struct {
				Nodes []struct {
					Number float64 `json:"number"`
					URL    string  `json:"url"`
					State  struct {
						Type string `json:"type"`
					} `json:"state"`
					Project *struct {
						ID string `json:"id"`
					} `json:"project"`
				} `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"issues"`
		}
		if err := l.doGraphQL(ctx, query, vars, &resp); err != nil {
			return ConditionalResult[[]Issue]{}, fmt.Errorf("forge: linear list new artifacts %q: %w", repo, err)
		}
		for _, n := range resp.Issues.Nodes {
			iss := Issue{Number: uint64(n.Number), URL: n.URL, State: mapLinearState(n.State.Type)}
			if n.Project != nil {
				iss.Project = n.Project.ID
			}
			out = append(out, iss)
		}
		if !resp.Issues.PageInfo.HasNextPage || resp.Issues.PageInfo.EndCursor == "" {
			break
		}
		after = resp.Issues.PageInfo.EndCursor
	}
	return ConditionalResult[[]Issue]{V: out}, nil
}
