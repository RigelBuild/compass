package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrBudgetExhausted is returned (wrapped) when the rate budget is at or under
// the reserve, or a 403/429 signals a rate-limit skip; the caller skips the
// cycle and retries next tick. The client never sleeps in-process — backoff is
// the driver's ticker.
var ErrBudgetExhausted = errors.New("forge: rate budget exhausted")

// TokenSource yields the current forge token and lets the client drop a cached
// value when it observes an auth failure, so the next batch re-resolves. Token
// is called per fetch batch (not per request); the client calls Invalidate on a
// 401 / bad-creds-403.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate()
}

// GitHubConfig configures a GitHub read client.
type GitHubConfig struct {
	Host   string       // "github.com" or a GHES host; API base derives from it
	Token  TokenSource  // required
	Client *http.Client // nil -> a default client with a sane timeout
}

// ListPage is one conditional page fetch result. On NotModified, Issues is nil
// and ETag/HasNext are zero — the caller's stored cursor row remains the truth.
type ListPage struct {
	Issues      []Issue
	ETag        string // the response ETag to store on sink success
	HasNext     bool   // an RFC-5988 Link rel="next" was present
	NotModified bool   // 304: content unchanged vs the etag argument
	// RateLimitRemaining is the x-ratelimit-remaining header from this response
	// (the poll driver logs it as an observability signal). -1 when the header
	// is absent or unparseable (an unauthenticated or GHES-variant response).
	RateLimitRemaining int
}

// GitHub is a hand-rolled net/http read client for a GitHub (or GHES) forge
// (OQ-6: no go-github dependency, stdlib only — the high-level library lacks
// the conditional-request + budget hook this driver's core mechanism needs).
// It is STATELESS about cursors: the page-level primitive takes the caller's
// ETag and reports NotModified; no in-memory ETag/response/Link cache exists.
// The caller (the DL-053 poll driver) owns cursor state durably.
//
// Conditional requests are load-bearing for the budget math: a 304 Not Modified
// is NOT charged against the primary (core) rate limit when the request carried
// a valid Authorization header. Reverified against current GitHub REST docs on
// 2026-08-09:
// https://docs.github.com/en/rest/using-the-rest-api/best-practices-for-using-the-rest-api#use-conditional-requests
// ("Making a conditional request does not count against your primary rate limit
// if a 304 response is returned and the request was made while correctly
// authorized with an Authorization header.").
type GitHub struct {
	host   string
	token  TokenSource
	client *http.Client

	// resetAt is the rate-budget gate: when non-zero and now() is before it,
	// the next call fails fast with ErrBudgetExhausted rather than burning the
	// tail of the window. It is derived from the last response's
	// x-ratelimit-reset (or a 403/429 Retry-After / reset signal). A zero value
	// means the gate is OPEN. Absent/malformed headers leave it open (treat
	// unknown budget as available — never wedge the gate). Once now() passes
	// resetAt the gate re-opens, so a wedged window self-clears after the reset.
	// The driver polls single-goroutine per the frozen design, so no mutex.
	resetAt time.Time

	// now is the clock seam (defaults to time.Now in NewGitHub); tests override
	// it to drive the reset-time gate deterministically without real sleeps.
	now func() time.Time
}

// reserve is the rate-budget floor: when x-ratelimit-remaining is at or under
// this, the next call fails fast with ErrBudgetExhausted rather than spending
// the tail of the window.
const reserve = 10

// perPage is the fixed page size for list requests (stable across polls so more
// ticks return 304 — GitHub best-practices "make requests that can be cached").
const perPage = 100

// defaultSkip is the reset-window fallback: when a 403/429 rate-limit signal
// carries no usable reset time (no Retry-After, no x-ratelimit-reset), the gate
// arms for this bounded duration so it still self-clears rather than wedging.
const defaultSkip = time.Minute

// NewGitHub returns a GitHub read client. A nil cfg.Client gets a default client
// with a sane timeout; cfg.Token is required (the caller wires it — DL-052).
func NewGitHub(cfg GitHubConfig) *GitHub {
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &GitHub{host: cfg.Host, token: cfg.Token, client: client, now: time.Now}
}

// ghIssue is the wire shape of one GitHub /issues list row. Only the fields the
// forge.Issue domain needs are decoded; the pullRequest key discriminates a PR
// row (GitHub's issues endpoint returns PRs as issues — they are dropped).
type ghIssue struct {
	Number    uint64 `json:"number"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	State     string `json:"state"`
	HTMLURL   string `json:"html_url"`
	UpdatedAt string `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest *json.RawMessage `json:"pull_request"`
}

// ghError is the wire shape of a GitHub error body (the message field feeds
// StatusError.Message).
type ghError struct {
	Message string `json:"message"`
}

// ListIssuesPage fetches one page (1-based) of a repo's issues conditionally.
// etag == "" is an unconditional fetch (no If-None-Match); a non-empty etag
// sends If-None-Match, and a 304 maps to ListPage{NotModified: true} with nil
// Issues and zero ETag/HasNext (no body parse). The client holds NO cursor
// state — the caller owns it.
func (g *GitHub) ListIssuesPage(ctx context.Context, repo string, f IssueFilter, page int, etag string) (ListPage, error) {
	// Gate check: an armed gate blocks until the injected clock passes resetAt,
	// then re-opens so the next call issues a real request (whose response
	// re-records the budget). A zero resetAt means the gate is open.
	if !g.resetAt.IsZero() {
		if g.now().Before(g.resetAt) {
			return ListPage{}, fmt.Errorf("forge: github list %q page %d: %w", repo, page, ErrBudgetExhausted)
		}
		g.resetAt = time.Time{}
	}

	token, err := g.token.Token(ctx)
	if err != nil {
		return ListPage{}, fmt.Errorf("forge: github resolve token: %w", err)
	}

	// repo is a trusted internal coordinate ("owner/name" from the subscription
	// store, validated at ingest), never attacker-controlled, so it is
	// interpolated into the path directly; queryParams escapes the filter args.
	u := g.apiBase() + "/repos/" + repo + "/issues?" + g.queryParams(f, page).Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ListPage{}, fmt.Errorf("forge: github build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return ListPage{}, fmt.Errorf("forge: github list %q page %d: %w", repo, page, err)
	}
	defer func() { _ = resp.Body.Close() }() // read-only GET; body drained/closed, no actionable close error

	switch {
	case resp.StatusCode == http.StatusNotModified:
		// A 304's x-ratelimit-* headers reflect a healthy authorized bucket
		// (the conditional request was not charged against the primary limit);
		// record it for the NEXT call, then short-circuit before any body parse.
		g.recordBudget(resp)
		return ListPage{NotModified: true, RateLimitRemaining: remainingHeader(resp)}, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// A 2xx's headers reflect a healthy bucket; record for the next call.
		g.recordBudget(resp)
		// fallthrough to body parse below
	default:
		// Error responses: mapErrorResponse owns the budget decision (it arms
		// the gate on a true rate-limit signal). A bad-creds 403 in the
		// unauthenticated bucket carries a low nonzero remaining; recording it
		// here would arm the gate against the token we are about to invalidate,
		// suppressing the fresh-token retry the next batch is meant to make.
		return ListPage{}, g.mapErrorResponse(resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ListPage{}, fmt.Errorf("forge: github read body: %w", err)
	}

	var rows []ghIssue
	if err := json.Unmarshal(body, &rows); err != nil {
		return ListPage{}, fmt.Errorf("forge: github decode issues: %w", err)
	}

	issues := make([]Issue, 0, len(rows))
	for _, r := range rows {
		// GitHub returns PRs as issues; drop them (issue-shaped only). GitHub
		// OMITS the pull_request key for a plain issue, but a *json.RawMessage
		// unmarshals an explicit "pull_request": null to a non-nil
		// RawMessage("null") — guard that so a null never drops a real issue.
		if raw := r.PullRequest; raw != nil && len(*raw) > 0 && string(*raw) != "null" {
			continue
		}
		issues = append(issues, r.toIssue())
	}

	return ListPage{
		Issues:             issues,
		ETag:               resp.Header.Get("ETag"),
		HasNext:            hasNextLink(resp.Header.Get("Link")),
		RateLimitRemaining: remainingHeader(resp),
	}, nil
}

// ListIssues is the unconditional full walk (concatenated pages), built on
// ListIssuesPage. It follows the RFC-5988 Link rel="next" chain. Satisfies
// ingest.forgeReader structurally and is the read half of forge.Provider.
func (g *GitHub) ListIssues(ctx context.Context, repo string, f IssueFilter) ([]Issue, error) {
	var all []Issue
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, err := g.ListIssuesPage(ctx, repo, f, page, "")
		if err != nil {
			return nil, err
		}
		all = append(all, p.Issues...)
		if !p.HasNext {
			return all, nil
		}
	}
}

// ghComment is the wire shape of a GitHub issue/PR comment (the create response
// and the read shape share this envelope). Only the fields forge.Comment needs
// are decoded.
type ghComment struct {
	ID      uint64 `json:"id"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
}

// toComment maps a decoded wire comment to the raw forge.Comment (body RAW).
func (r ghComment) toComment() Comment {
	return Comment{ID: r.ID, URL: r.HTMLURL, Body: r.Body, ForgeAccount: r.User.Login}
}

// ghPull is the wire shape of a GitHub pull request (the create response). Only
// the fields forge.PullRequest needs at create time are decoded; the read-side
// roll-ups (Changed/Checks/Reviews/Threads) belong to RIG-1728's GetPullRequest.
type ghPull struct {
	Number  uint64 `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	Draft   bool   `json:"draft"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

// toPullRequest maps a decoded wire PR to the raw forge.PullRequest. Only the
// create-time fields are populated (body RAW); read roll-ups stay zero.
func (r ghPull) toPullRequest() PullRequest {
	return PullRequest{
		Number:       r.Number,
		Title:        r.Title,
		Body:         r.Body,
		State:        r.State,
		URL:          r.HTMLURL,
		HeadRef:      r.Head.Ref,
		BaseRef:      r.Base.Ref,
		ForgeAccount: r.User.Login,
		Draft:        r.Draft,
	}
}

// CreateIssue creates an issue on repo. in.Body is PRE-stamped by the Service
// (DL-050); the Provider sends it verbatim. Returns the created forge.Issue.
func (g *GitHub) CreateIssue(ctx context.Context, repo string, in CreateIssue) (Issue, error) {
	body := struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels,omitempty"`
	}{Title: in.Title, Body: in.Body, Labels: in.Labels}
	var out ghIssue
	if err := g.doJSON(ctx, g.apiBase()+"/repos/"+repo+"/issues", body, &out); err != nil {
		return Issue{}, fmt.Errorf("forge: github create issue %q: %w", repo, err)
	}
	return out.toIssue(), nil
}

// CommentOnIssue posts a comment on issue number in repo. body is PRE-stamped.
func (g *GitHub) CommentOnIssue(ctx context.Context, repo string, number uint64, body string) (Comment, error) {
	in := struct {
		Body string `json:"body"`
	}{Body: body}
	url := g.apiBase() + "/repos/" + repo + "/issues/" + strconv.FormatUint(number, 10) + "/comments"
	var out ghComment
	if err := g.doJSON(ctx, url, in, &out); err != nil {
		return Comment{}, fmt.Errorf("forge: github comment on issue %q#%d: %w", repo, number, err)
	}
	return out.toComment(), nil
}

// CreatePullRequest opens a pull request on repo. in.Body is PRE-stamped.
func (g *GitHub) CreatePullRequest(ctx context.Context, repo string, in CreatePR) (PullRequest, error) {
	body := struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Draft bool   `json:"draft"`
	}{Title: in.Title, Body: in.Body, Head: in.HeadRef, Base: in.BaseRef, Draft: in.Draft}
	var out ghPull
	if err := g.doJSON(ctx, g.apiBase()+"/repos/"+repo+"/pulls", body, &out); err != nil {
		return PullRequest{}, fmt.Errorf("forge: github create pull request %q: %w", repo, err)
	}
	return out.toPullRequest(), nil
}

// CommentOnPullRequest posts a conversation comment on PR number in repo. GitHub
// models PR conversation comments as issue comments, so this targets the issues
// comments endpoint. body is PRE-stamped.
func (g *GitHub) CommentOnPullRequest(ctx context.Context, repo string, number uint64, body string) (Comment, error) {
	in := struct {
		Body string `json:"body"`
	}{Body: body}
	url := g.apiBase() + "/repos/" + repo + "/issues/" + strconv.FormatUint(number, 10) + "/comments"
	var out ghComment
	if err := g.doJSON(ctx, url, in, &out); err != nil {
		return Comment{}, fmt.Errorf("forge: github comment on pull request %q#%d: %w", repo, number, err)
	}
	return out.toComment(), nil
}

// BodyLimit is the max issue/comment/PR body size the Service enforces before a
// write, in BYTES. GitHub caps these bodies at 65536 CHARACTERS; because a
// UTF-8 string's character count never exceeds its byte count, enforcing 65536
// BYTES is strictly conservative under the character cap (A9: the unit is pinned
// as bytes — StampOwner reserves bytes via len — with no conversion anywhere).
func (g *GitHub) BodyLimit() int { return 65536 }

// doJSON carries the write-path plumbing once for all four write methods: the
// resetAt fail-fast gate (a write burst respects the same reserve as the poll
// driver, so it cannot starve it), token auth, budget recording, and error
// mapping. It marshals in to a JSON request body and decodes a 2xx response
// into out. The read path (ListIssuesPage) is intentionally NOT refactored onto
// this in this slice (no RIG-1728 rework).
func (g *GitHub) doJSON(ctx context.Context, url string, in, out any) error {
	// Gate check mirrors ListIssuesPage: an armed gate short-circuits without a
	// request until the injected clock passes resetAt, then re-opens.
	if !g.resetAt.IsZero() {
		if g.now().Before(g.resetAt) {
			return fmt.Errorf("forge: github POST %s: %w", url, ErrBudgetExhausted)
		}
		g.resetAt = time.Time{}
	}

	token, err := g.token.Token(ctx)
	if err != nil {
		return fmt.Errorf("forge: github resolve token: %w", err)
	}

	payload, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("forge: github marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("forge: github build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("forge: github do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // response body drained/closed; no actionable close error on a read

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Error responses: mapErrorResponse owns the budget decision (it arms
		// the gate on a true rate-limit signal). A bad-creds 403 carries a low
		// nonzero remaining; recording it here would arm the gate against the
		// token we are about to invalidate (same reasoning as the read path).
		return g.mapErrorResponse(resp)
	}
	g.recordBudget(resp)

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("forge: github read body: %w", err)
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("forge: github decode response: %w", err)
	}
	return nil
}

// apiBase derives the REST API base URL from the configured host. github.com
// maps to api.github.com; a GHES host maps to https://<host>/api/v3.
func (g *GitHub) apiBase() string {
	if g.host == "" || g.host == "github.com" {
		return "https://api.github.com"
	}
	return "https://" + g.host + "/api/v3"
}

// toIssue maps a decoded wire row to the raw forge.Issue. Body is returned RAW
// (the owner-header strip belongs to ingestion, never the provider). UpdatedAt
// is parsed from the row's updated_at (RFC-3339); an unparseable value leaves
// the zero time.
func (r ghIssue) toIssue() Issue {
	labels := make([]string, 0, len(r.Labels))
	for _, l := range r.Labels {
		labels = append(labels, l.Name)
	}
	var updated time.Time
	if r.UpdatedAt != "" {
		if t, err := time.Parse(time.RFC3339, r.UpdatedAt); err == nil {
			updated = t
		}
	}
	return Issue{
		Number:       r.Number,
		Title:        r.Title,
		Body:         r.Body,
		State:        r.State,
		URL:          r.HTMLURL,
		ForgeAccount: r.User.Login,
		Labels:       labels,
		UpdatedAt:    updated,
	}
}

// queryParams maps the IssueFilter to GitHub list-issues query params. An empty
// State maps to state=all (this provider's documented default); labels join
// comma-separated. per_page and page are always set.
func (g *GitHub) queryParams(f IssueFilter, page int) url.Values {
	q := url.Values{}
	state := f.State
	if state == "" {
		state = "all"
	}
	q.Set("state", state)
	if len(f.Labels) > 0 {
		q.Set("labels", strings.Join(f.Labels, ","))
	}
	q.Set("per_page", strconv.Itoa(perPage))
	q.Set("page", strconv.Itoa(page))
	return q
}

// recordBudget updates the fail-fast gate from a response's x-ratelimit-*
// headers. remaining <= reserve arms the gate until x-ratelimit-reset (unix
// seconds); absent/malformed headers leave it open (treat unknown budget as
// available — never wedge the gate). A missing/unparseable reset with a
// low remaining falls back to a bounded skip so the gate still self-clears.
func (g *GitHub) recordBudget(resp *http.Response) {
	raw := resp.Header.Get("X-Ratelimit-Remaining")
	if raw == "" {
		g.resetAt = time.Time{}
		return
	}
	remaining, err := strconv.Atoi(raw)
	if err != nil {
		g.resetAt = time.Time{}
		return
	}
	if remaining > reserve {
		g.resetAt = time.Time{}
		return
	}
	g.armGate(resetFromHeader(resp.Header.Get("X-Ratelimit-Reset")))
}

// remainingHeader parses the x-ratelimit-remaining header into the observability
// value carried on ListPage. It returns -1 when the header is absent or
// unparseable (an unauthenticated response, or a GHES variant that omits it), so
// a "no signal" reading is distinguishable from a genuine zero.
func remainingHeader(resp *http.Response) int {
	raw := resp.Header.Get("X-Ratelimit-Remaining")
	if raw == "" {
		return -1
	}
	remaining, err := strconv.Atoi(raw)
	if err != nil {
		return -1
	}
	return remaining
}

// armGate sets the reset-time gate. A zero at (no usable reset time) falls back
// to a bounded skip from now() so the gate self-clears rather than wedging.
func (g *GitHub) armGate(at time.Time) {
	if at.IsZero() {
		at = g.now().Add(defaultSkip)
	}
	g.resetAt = at
}

// resetFromHeader parses an x-ratelimit-reset value (unix seconds) into an
// absolute reset instant; an absent/malformed value yields the zero time.
func resetFromHeader(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

// mapErrorResponse classifies a non-2xx/non-304 response. A 403/429 carrying
// retry-after OR a zeroed x-ratelimit-remaining is a rate-limit skip
// (ErrBudgetExhausted, arms the gate, no token re-resolve). A 401, or a 403
// without rate-limit headers (bad-credentials/permission), is a *StatusError
// AND invalidates the TokenSource so the next batch re-resolves. Any other
// non-2xx is a *StatusError only.
func (g *GitHub) mapErrorResponse(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body) // best-effort: message is diagnostic; a read error just yields an empty message
	var ge ghError
	_ = json.Unmarshal(body, &ge) // best-effort decode of the diagnostic message; malformed body -> empty message

	status := resp.StatusCode
	if status == http.StatusForbidden || status == http.StatusTooManyRequests {
		if isRateLimited(resp) {
			g.armGate(g.rateLimitReset(resp))
			return fmt.Errorf("forge: github http %d: %w", status, ErrBudgetExhausted)
		}
	}

	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// 401, or a 403 that is NOT a rate-limit signal -> bad credentials /
		// permission: drop the cached token so the next batch re-resolves.
		g.token.Invalidate()
	}

	return &StatusError{Status: status, Message: ge.Message}
}

// isRateLimited reports whether a 403/429 is a rate-limit signal: a retry-after
// header, or a zeroed x-ratelimit-remaining. The presence of these two is the
// ONLY discriminator (a dead token is never mistaken for a rate-limit skip).
func isRateLimited(resp *http.Response) bool {
	if resp.Header.Get("Retry-After") != "" {
		return true
	}
	if raw := resp.Header.Get("X-Ratelimit-Remaining"); raw != "" {
		if remaining, err := strconv.Atoi(raw); err == nil && remaining == 0 {
			return true
		}
	}
	return false
}

// rateLimitReset derives a reset instant from a 403/429 rate-limit response: a
// Retry-After value maps to now()+seconds (RFC-7231 delta-seconds) or the
// parsed HTTP-date; otherwise an x-ratelimit-reset (unix seconds) is used. A
// zero time (neither present/usable) lets armGate fall back to the bounded
// default skip.
func (g *GitHub) rateLimitReset(resp *http.Response) time.Time {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			return g.now().Add(time.Duration(secs) * time.Second)
		}
		if at, err := http.ParseTime(ra); err == nil {
			return at
		}
	}
	return resetFromHeader(resp.Header.Get("X-Ratelimit-Reset"))
}

// hasNextLink reports whether an RFC-5988 Link header carries a rel="next".
func hasNextLink(link string) bool {
	if link == "" {
		return false
	}
	for part := range strings.SplitSeq(link, ",") {
		if strings.Contains(part, `rel="next"`) {
			return true
		}
	}
	return false
}
