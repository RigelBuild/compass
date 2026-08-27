package forge

// Linear is a hand-rolled net/http GraphQL client for the Linear issue tracker
// (design.md §5), a co-equal forge write target beside GitHub. It mirrors
// github.go's no-dependency posture (stdlib only, no go-github / GraphQL
// library) and shares its seams: a TokenSource for the credential, a
// mu-guarded fail-fast rate gate (a write burst respects the same reserve as
// the poll driver so it cannot starve it), and an injectable clock.
//
// Linear is ISSUES-ONLY (DL-051): the PR/review half of Provider returns
// ErrUnsupported. `repo` is the Linear TEAM KEY (e.g. "SEA"), not owner/name;
// the client resolves key -> team id once and caches it (mu-guarded).
//
// Attribution (design.md §5, OQ-5/OQ-8): writes set Linear's createAsUser +
// displayIconUrl to ONE constant shared Compass app identity, so native Linear
// display shows a single "via Application" identity for every agent while the
// fine-grained per-agent owner truth rides the Service's StampOwner header.
// Both channels are Server-chosen (DL-050 unforgeability). Whether the client
// may set createAsUser at all is governed by a one-time actor-capability probe
// (A4, a stated design INTENT, not an asserted API behavior): a token that is
// not an OAuth actor=app token degrades to stamp-only.
//
// Body handling matches the Provider contract: a Create/Comment body is
// PRE-stamped by the Service and sent verbatim; a read returns the body RAW
// (the Service strips/parses on read).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// linearDefaultEndpoint is the public Linear GraphQL endpoint; LinearConfig.Host
	// overrides it (the whole endpoint URL, not just a hostname).
	linearDefaultEndpoint = "https://api.linear.app/graphql"

	// linearBodyLimit is the max issue/comment body size (BYTES) the Service
	// enforces before a Linear write. Linear does not publish a single pinned
	// GraphQL body cap, so this is a CONSERVATIVE constant: 65536 bytes matches
	// GitHub.BodyLimit, keeping the Service's cross-provider ceiling uniform and
	// comfortably under any Linear description limit observed in practice. See
	// the T6 summary — value is a choice, not a documented Linear cap.
	linearBodyLimit = 65536

	// attributionUser and attributionIconURL are the ONE shared Compass app
	// identity every Linear write is attributed to via createAsUser /
	// displayIconUrl (design.md §5). They are deliberately coarse (not
	// per-agent); the per-agent owner truth lives in the StampOwner header.
	attributionUser    = "Compass"
	attributionIconURL = "https://compass.rigel.build/assets/compass-app.png"

	// forge state truths mapped from Linear workflow-state types.
	stateOpen   = "open"
	stateClosed = "closed"

	// GraphQL variable keys reused across queries.
	varKey    = "key"
	varTeam   = "team"
	varFilter = "filter"
	varNumber = "number"
)

// linearClosedStateTypes are the Linear workflow-state `type` values that map
// to the forge's "closed" truth. Every other type maps to "open". Verified
// against Linear SDL WorkflowState.type: "triage", "backlog", "unstarted",
// "started", "completed", "canceled", "duplicate".
var linearClosedStateTypes = []string{"completed", "canceled"}

// LinearConfig configures a Linear client.
type LinearConfig struct {
	Host   string       // GraphQL endpoint URL; "" -> linearDefaultEndpoint
	Token  TokenSource  // required (its own LINEAR_FORGE_TOKEN, DL-052)
	Client *http.Client // nil -> a default client with a sane timeout
	Log    *slog.Logger // nil -> slog.Default(); carries the degrade log line
}

// Linear is a stdlib GraphQL client for a Linear forge. It is stateless about
// cursors (the caller owns durable poll state); the only in-memory state is the
// mu-guarded rate gate, team-id cache, and one-time actor-probe result.
type Linear struct {
	host   string
	token  TokenSource
	client *http.Client
	log    *slog.Logger

	// mu guards resetAt, teamIDs, and the actor-probe fields. The client may be
	// shared between the poll driver and write-RPC goroutines (OQ-6), so all
	// three are concurrent read-modify-write; mu is held only around the fast
	// state touches, never across an HTTP round-trip.
	mu sync.Mutex

	// resetAt is the rate-budget gate (see GitHub.resetAt). Non-zero and before
	// now() -> the next call fails fast with ErrBudgetExhausted. Zero -> open.
	resetAt time.Time

	// teamIDs caches Linear team key -> team UUID; a key is resolved once via a
	// teams query and reused for every subsequent CreateIssue.
	teamIDs map[string]string

	// probeDone/actorCapable cache the one-time actor-capability probe (A4).
	// Once probeDone, actorCapable governs whether writes set createAsUser.
	probeDone    bool
	actorCapable bool

	// now is the clock seam (defaults to time.Now); tests override it to drive
	// the reset-time gate deterministically.
	now func() time.Time
}

// NewLinear returns a Linear client. A nil cfg.Client gets a default client
// with a sane timeout; a nil cfg.Log falls back to slog.Default(). cfg.Token is
// required (the caller wires it — DL-052).
func NewLinear(cfg LinearConfig) *Linear {
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Linear{
		host:    cfg.Host,
		token:   cfg.Token,
		client:  client,
		log:     log,
		teamIDs: make(map[string]string),
		now:     time.Now,
	}
}

// Compile-time proof that Linear satisfies the Provider interface.
var _ Provider = (*Linear)(nil)

// --- Provider: exported methods ----------------------------------------------

// Name identifies this provider.
func (l *Linear) Name() string { return "linear" }

// CreateIssue creates an issue on the team keyed by repo. in.Body is PRE-stamped
// by the Service; it becomes the Linear description verbatim. createAsUser /
// displayIconUrl are set to the shared Compass identity when the actor probe
// passes. Labels are NOT sent: Linear's IssueCreateInput.labelIds takes UUIDs,
// not names, and name->UUID resolution is out of this slice (see T6 summary).
func (l *Linear) CreateIssue(ctx context.Context, repo string, in CreateIssue) (Issue, error) {
	teamID, err := l.resolveTeamID(ctx, repo)
	if err != nil {
		return Issue{}, fmt.Errorf("forge: linear create issue %q: %w", repo, err)
	}
	input := map[string]any{"teamId": teamID, "title": in.Title, "description": in.Body}
	l.applyAttribution(ctx, input)

	const query = `mutation CompassIssueCreate($input: IssueCreateInput!) {
  issueCreate(input: $input) {
    issue { ...CompassIssueFields }
  }
}` + issueFieldsFragment
	var out struct {
		IssueCreate struct {
			Issue linearIssue `json:"issue"`
		} `json:"issueCreate"`
	}
	if err := l.doGraphQL(ctx, query, map[string]any{"input": input}, &out); err != nil {
		return Issue{}, fmt.Errorf("forge: linear create issue %q: %w", repo, err)
	}
	return out.IssueCreate.Issue.toIssue(), nil
}

// CommentOnIssue posts a comment on issue number in the team keyed by repo. body
// is PRE-stamped. It resolves the issue UUID from (team, number), then runs
// commentCreate with createAsUser gated on the actor probe.
func (l *Linear) CommentOnIssue(ctx context.Context, repo string, number uint64, body string) (Comment, error) {
	issueID, err := l.resolveIssueID(ctx, repo, number)
	if err != nil {
		return Comment{}, fmt.Errorf("forge: linear comment on issue %q#%d: %w", repo, number, err)
	}
	input := map[string]any{"issueId": issueID, "body": body}
	l.applyAttribution(ctx, input)

	const query = `mutation CompassCommentCreate($input: CommentCreateInput!) {
  commentCreate(input: $input) {
    comment { id url body user { displayName } }
  }
}`
	var out struct {
		CommentCreate struct {
			Comment linearComment `json:"comment"`
		} `json:"commentCreate"`
	}
	if err := l.doGraphQL(ctx, query, map[string]any{"input": input}, &out); err != nil {
		return Comment{}, fmt.Errorf("forge: linear comment on issue %q#%d: %w", repo, number, err)
	}
	return out.CommentCreate.Comment.toComment(), nil
}

// GetIssue fetches one issue by its per-team number in the team keyed by repo.
// Body is returned RAW. Resolved via the issues query (team key + number),
// which accepts the human coordinate without a prior UUID lookup.
func (l *Linear) GetIssue(ctx context.Context, repo string, number uint64) (Issue, error) {
	const query = `query CompassIssueGet($filter: IssueFilter!) {
  issues(filter: $filter, first: 1) {
    nodes { ...CompassIssueFields }
  }
}` + issueFieldsFragment
	filter := map[string]any{
		varTeam:   map[string]any{varKey: map[string]any{"eq": repo}},
		varNumber: map[string]any{"eq": float64(number)},
	}
	var out struct {
		Issues struct {
			Nodes []linearIssue `json:"nodes"`
		} `json:"issues"`
	}
	if err := l.doGraphQL(ctx, query, map[string]any{varFilter: filter}, &out); err != nil {
		return Issue{}, fmt.Errorf("forge: linear get issue %q#%d: %w", repo, number, err)
	}
	if len(out.Issues.Nodes) == 0 {
		return Issue{}, &StatusError{Status: http.StatusNotFound, Message: fmt.Sprintf("no issue %s-%d", repo, number)}
	}
	return out.Issues.Nodes[0].toIssue(), nil
}

// ListIssues walks every issue in the team keyed by repo, narrowed by f, across
// all pages (Linear paginates at 50; the loop follows pageInfo). Bodies are RAW.
func (l *Linear) ListIssues(ctx context.Context, repo string, f IssueFilter) ([]Issue, error) {
	const query = `query CompassIssueList($filter: IssueFilter!, $after: String) {
  issues(filter: $filter, first: 50, after: $after) {
    nodes { ...CompassIssueFields }
    pageInfo { hasNextPage endCursor }
  }
}` + issueFieldsFragment
	filter := teamIssueFilter(repo, f)

	var all []Issue
	var after string
	for {
		vars := map[string]any{varFilter: filter}
		if after != "" {
			vars["after"] = after
		}
		var out struct {
			Issues struct {
				Nodes    []linearIssue `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"issues"`
		}
		if err := l.doGraphQL(ctx, query, vars, &out); err != nil {
			return nil, fmt.Errorf("forge: linear list issues %q: %w", repo, err)
		}
		for _, n := range out.Issues.Nodes {
			all = append(all, n.toIssue())
		}
		next := out.Issues.PageInfo.EndCursor
		// Terminate on end-of-pages OR a malformed page (hasNextPage with an
		// empty cursor): advancing on an empty cursor would drop the `after`
		// variable and refetch page 1 forever.
		if !out.Issues.PageInfo.HasNextPage || next == "" {
			break
		}
		after = next
	}
	return all, nil
}

// CreatePullRequest is unsupported: Linear has no pull-request concept
// (design.md §5). The Service maps ErrUnsupported to the in-band `unimplemented`.
func (l *Linear) CreatePullRequest(ctx context.Context, repo string, in CreatePR) (PullRequest, error) {
	return PullRequest{}, ErrUnsupported
}

// CommentOnPullRequest is unsupported (Linear has no PRs).
func (l *Linear) CommentOnPullRequest(ctx context.Context, repo string, number uint64, body string) (Comment, error) {
	return Comment{}, ErrUnsupported
}

// SubmitReview is unsupported (Linear has no review concept).
func (l *Linear) SubmitReview(ctx context.Context, repo string, number uint64, in SubmitReview) (SubmittedReview, error) {
	return SubmittedReview{}, ErrUnsupported
}

// GetPullRequest is unsupported (Linear has no PRs); the canonical PullRequest
// surface is never fabricated on a Linear coordinate.
func (l *Linear) GetPullRequest(ctx context.Context, repo string, number uint64) (PullRequest, error) {
	return PullRequest{}, ErrUnsupported
}

// Checks is unsupported (Linear has no PR head checks).
func (l *Linear) Checks(ctx context.Context, repo string, number uint64) (Checks, error) {
	return Checks{}, ErrUnsupported
}

// BodyLimit is the max body size (BYTES) the Service enforces before a write.
// See linearBodyLimit for the conservative-constant rationale.
func (l *Linear) BodyLimit() int { return linearBodyLimit }

// --- Provider: unexported plumbing -------------------------------------------

// doGraphQL carries the write- and read-path plumbing once: the resetAt
// fail-fast gate, token auth, the JSON POST of {query, variables}, and response
// classification. It decodes the `data` object into out on success.
func (l *Linear) doGraphQL(ctx context.Context, query string, variables map[string]any, out any) error {
	if l.gateBlocked() {
		return fmt.Errorf("linear graphql: %w", ErrBudgetExhausted)
	}

	token, err := l.token.Token(ctx)
	if err != nil {
		return fmt.Errorf("resolve token: %w", err)
	}

	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return fmt.Errorf("marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.endpoint(), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	// Linear OAuth tokens (the actor=app token this provider uses, DL-052) are
	// bearer tokens. See the T6 summary: the "Bearer " scheme is the grounded
	// choice for an OAuth token; a raw personal API key would omit it.
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // body fully read below; a close error on a drained read body is not actionable

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	return l.handleResponse(resp, body, out)
}

// handleResponse classifies a Linear response and, on success, decodes data
// into out. Precedence: rate limit (HTTP 429 or a RATELIMITED code) -> auth
// (HTTP 401 or AUTHENTICATION_ERROR code, which invalidates the token) -> any
// other GraphQL errors (even on HTTP 200) -> a non-2xx with no usable envelope
// -> success.
func (l *Linear) handleResponse(resp *http.Response, body []byte, out any) error {
	var gr graphQLResponse
	decodeErr := json.Unmarshal(body, &gr)
	status := resp.StatusCode

	// Rate limit: a 429, or Linear's GraphQL-level complexity/rate rejection
	// (HTTP 400 carrying extensions.code == "RATELIMITED"). Arms the gate; no
	// token re-resolve.
	if status == http.StatusTooManyRequests || (decodeErr == nil && hasErrorCode(gr.Errors, "RATELIMITED")) {
		l.mu.Lock()
		l.armGate(l.rateLimitReset(resp))
		l.mu.Unlock()
		return fmt.Errorf("linear graphql http %d: %w", status, ErrBudgetExhausted)
	}

	// Auth failure: a 401, or a GraphQL AUTHENTICATION_ERROR. Drop the cached
	// token so the next batch re-resolves.
	if status == http.StatusUnauthorized || (decodeErr == nil && hasErrorCode(gr.Errors, "AUTHENTICATION_ERROR")) {
		l.token.Invalidate()
		return &StatusError{Status: statusOr(status, http.StatusUnauthorized), Message: joinErrors(gr.Errors)}
	}

	// Any other GraphQL top-level errors — Linear returns these on HTTP 200.
	if decodeErr == nil && len(gr.Errors) > 0 {
		return &StatusError{Status: statusOr(status, http.StatusOK), Message: joinErrors(gr.Errors)}
	}

	// A non-2xx with no parseable GraphQL error envelope.
	if status < 200 || status >= 300 {
		return &StatusError{Status: status, Message: strings.TrimSpace(string(body))}
	}

	// Success. A 2xx body that would not parse as the envelope is a decode failure.
	if decodeErr != nil {
		return fmt.Errorf("decode response: %w", decodeErr)
	}
	l.recordBudget(resp)
	if out != nil {
		if err := json.Unmarshal(gr.Data, out); err != nil {
			return fmt.Errorf("decode data: %w", err)
		}
	}
	return nil
}

// endpoint is the configured GraphQL endpoint, defaulting to Linear's public one.
func (l *Linear) endpoint() string {
	if l.host == "" {
		return linearDefaultEndpoint
	}
	return l.host
}

// gateBlocked reports whether the fail-fast budget gate is armed, clearing a
// gate whose reset instant has passed as a side effect. Guarded by mu.
func (l *Linear) gateBlocked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.resetAt.IsZero() {
		return false
	}
	if l.now().Before(l.resetAt) {
		return true
	}
	l.resetAt = time.Time{}
	return false
}

// armGate sets the reset-time gate; a zero at falls back to a bounded skip so
// the gate self-clears rather than wedging. Caller MUST hold l.mu.
func (l *Linear) armGate(at time.Time) {
	if at.IsZero() {
		at = l.now().Add(defaultSkip)
	}
	l.resetAt = at
}

// recordBudget updates the fail-fast gate from Linear's X-RateLimit-Requests-*
// headers on a successful response. remaining <= reserve arms the gate until
// the reset instant; absent/malformed headers leave it open (never wedge).
func (l *Linear) recordBudget(resp *http.Response) {
	l.mu.Lock()
	defer l.mu.Unlock()
	raw := resp.Header.Get("X-Ratelimit-Requests-Remaining")
	if raw == "" {
		l.resetAt = time.Time{}
		return
	}
	remaining, err := strconv.Atoi(raw)
	if err != nil {
		l.resetAt = time.Time{}
		return
	}
	if remaining > reserve {
		l.resetAt = time.Time{}
		return
	}
	l.armGate(linearResetFromHeader(resp.Header.Get("X-Ratelimit-Requests-Reset")))
}

// rateLimitReset derives a reset instant from a rate-limited response: a
// Retry-After (delta-seconds or HTTP-date) wins, else X-RateLimit-Requests-Reset
// (UTC epoch MILLISECONDS per Linear's docs). A zero time lets armGate fall back
// to the bounded default skip.
func (l *Linear) rateLimitReset(resp *http.Response) time.Time {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			return l.now().Add(time.Duration(secs) * time.Second)
		}
		if at, err := http.ParseTime(ra); err == nil {
			return at
		}
	}
	return linearResetFromHeader(resp.Header.Get("X-Ratelimit-Requests-Reset"))
}

// resolveTeamID maps a Linear team key to its UUID, caching the result. The
// cache is checked and stored under mu without holding it across the network
// call; a concurrent miss issues a redundant (idempotent) lookup at worst.
func (l *Linear) resolveTeamID(ctx context.Context, key string) (string, error) {
	l.mu.Lock()
	if id, ok := l.teamIDs[key]; ok {
		l.mu.Unlock()
		return id, nil
	}
	l.mu.Unlock()

	const query = `query CompassTeamByKey($key: String!) {
  teams(filter: {key: {eq: $key}}, first: 1) {
    nodes { id }
  }
}`
	var out struct {
		Teams struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	if err := l.doGraphQL(ctx, query, map[string]any{varKey: key}, &out); err != nil {
		return "", err
	}
	if len(out.Teams.Nodes) == 0 {
		return "", &StatusError{Status: http.StatusNotFound, Message: fmt.Sprintf("no team with key %q", key)}
	}
	id := out.Teams.Nodes[0].ID

	l.mu.Lock()
	l.teamIDs[key] = id
	l.mu.Unlock()
	return id, nil
}

// resolveIssueID maps a (team key, per-team number) pair to a Linear issue UUID
// via the issues query, for CommentOnIssue's issueId. Not cached — an issue
// number is written to at most a handful of times per session.
func (l *Linear) resolveIssueID(ctx context.Context, repo string, number uint64) (string, error) {
	const query = `query CompassIssueIDByNumber($filter: IssueFilter!) {
  issues(filter: $filter, first: 1) {
    nodes { id }
  }
}`
	filter := map[string]any{
		varTeam:   map[string]any{varKey: map[string]any{"eq": repo}},
		varNumber: map[string]any{"eq": float64(number)},
	}
	var out struct {
		Issues struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"issues"`
	}
	if err := l.doGraphQL(ctx, query, map[string]any{varFilter: filter}, &out); err != nil {
		return "", err
	}
	if len(out.Issues.Nodes) == 0 {
		return "", &StatusError{Status: http.StatusNotFound, Message: fmt.Sprintf("no issue %s-%d", repo, number)}
	}
	return out.Issues.Nodes[0].ID, nil
}

// actorAttribution reports whether writes may set createAsUser, running the
// capability probe on first call and caching an AUTHORITATIVE result. The probe
// queries `viewer { app }`: an actor=app OAuth token authenticates AS the app,
// so viewer.app is true; a plain user/API-key token reports false. The probe is
// meant to reflect the token's NATURE, not a transient runtime state — so a
// probe that ERRORS (network blip, HTTP 5xx, or an already-armed rate gate)
// degrades THIS write to stamp-only WITHOUT caching, letting a later write
// re-probe once the transient condition clears; only a clean answer
// (probeErr == nil) is cached. On the first degrade it emits the named log line.
func (l *Linear) actorAttribution(ctx context.Context) bool {
	l.mu.Lock()
	if l.probeDone {
		capable := l.actorCapable
		l.mu.Unlock()
		return capable
	}
	l.mu.Unlock()

	const query = `query CompassActorProbe {
  viewer { app }
}`
	var out struct {
		Viewer struct {
			App bool `json:"app"`
		} `json:"viewer"`
	}
	probeErr := l.doGraphQL(ctx, query, nil, &out)

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.probeDone {
		// A concurrent caller finished an authoritative probe first; honor it.
		return l.actorCapable
	}
	if probeErr != nil {
		// Transient failure — degrade this write but do NOT cache, so a later
		// write re-probes. Log the degrade line once per transient occurrence.
		l.log.Warn("linear: actor attribution unavailable; degrading to stamp-only", "probe_error", probeErr)
		return false
	}
	// Authoritative answer: cache it. A definitive not-capable also degrades.
	l.probeDone = true
	l.actorCapable = out.Viewer.App
	if !l.actorCapable {
		l.log.Warn("linear: actor attribution unavailable; degrading to stamp-only")
	}
	return l.actorCapable
}

// applyAttribution sets createAsUser/displayIconUrl on a mutation input when the
// actor probe reports the token is capable; otherwise it is a no-op (stamp-only
// degradation). Both values are the constant shared Compass app identity.
func (l *Linear) applyAttribution(ctx context.Context, input map[string]any) {
	if l.actorAttribution(ctx) {
		input["createAsUser"] = attributionUser
		input["displayIconUrl"] = attributionIconURL
	}
}

// --- GraphQL wire envelope + helpers -----------------------------------------

// graphQLResponse is the standard GraphQL response envelope. Linear returns
// HTTP 200 with a top-level `errors` array for most failures (and HTTP 400 for
// GraphQL-level rate limits, still carrying the errors array), so both fields
// are always decoded.
type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphQLError  `json:"errors"`
}

// graphQLError is one entry of the GraphQL `errors` array. The extensions.code
// discriminates a rate-limit ("RATELIMITED") or auth ("AUTHENTICATION_ERROR")
// failure from an ordinary one.
type graphQLError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

func hasErrorCode(errs []graphQLError, code string) bool {
	for _, e := range errs {
		if e.Extensions.Code == code {
			return true
		}
	}
	return false
}

func joinErrors(errs []graphQLError) string {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Message)
	}
	return strings.Join(msgs, "; ")
}

// statusOr returns status when it is a real HTTP error status (>=400), else the
// fallback. A GraphQL-level auth/error on an HTTP 200 thus surfaces a meaningful
// StatusError status (401 for auth, 200 for a plain query error) to the Service.
func statusOr(status, fallback int) int {
	if status >= 400 {
		return status
	}
	return fallback
}

// linearResetFromHeader parses an X-RateLimit-Requests-Reset value (UTC epoch
// MILLISECONDS) into an absolute instant; absent/malformed yields the zero time.
func linearResetFromHeader(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// --- issue/comment wire types + mapping --------------------------------------

// issueFieldsFragment is the shared selection set for reading a Linear issue
// into forge.Issue. Body is Description (returned RAW; the Service strips).
const issueFieldsFragment = `
fragment CompassIssueFields on Issue {
  number
  title
  description
  url
  state { name type }
  labels { nodes { name } }
  creator { displayName }
  updatedAt
}`

// linearIssue is the wire shape of a Linear issue (only the forge.Issue fields
// are decoded). number is a GraphQL Float; creator is null for app/bot-created
// issues.
type linearIssue struct {
	Number      float64 `json:"number"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	URL         string  `json:"url"`
	State       struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Creator *struct {
		DisplayName string `json:"displayName"`
	} `json:"creator"`
	UpdatedAt string `json:"updatedAt"`
}

// toIssue maps a decoded Linear issue to forge.Issue. Body is RAW; State is
// mapped to the forge's open/closed truth; UpdatedAt parses RFC-3339 (an
// unparseable value leaves the zero time).
func (r linearIssue) toIssue() Issue {
	labels := make([]string, 0, len(r.Labels.Nodes))
	for _, n := range r.Labels.Nodes {
		labels = append(labels, n.Name)
	}
	var updated time.Time
	if r.UpdatedAt != "" {
		if t, err := time.Parse(time.RFC3339, r.UpdatedAt); err == nil {
			updated = t
		}
	}
	account := ""
	if r.Creator != nil {
		account = r.Creator.DisplayName
	}
	return Issue{
		Number:       uint64(r.Number),
		Title:        r.Title,
		Body:         r.Description,
		State:        mapLinearState(r.State.Type),
		URL:          r.URL,
		ForgeAccount: account,
		Labels:       labels,
		UpdatedAt:    updated,
	}
}

// mapLinearState maps a Linear workflow-state type to the forge's open/closed
// truth (see linearClosedStateTypes).
func mapLinearState(stateType string) string {
	if slices.Contains(linearClosedStateTypes, stateType) {
		return stateClosed
	}
	return stateOpen
}

// linearComment is the wire shape of a Linear comment. Its id is a UUID, which
// forge.Comment.ID (uint64) cannot carry — see toComment.
type linearComment struct {
	ID   string `json:"id"`
	URL  string `json:"url"`
	Body string `json:"body"`
	User *struct {
		DisplayName string `json:"displayName"`
	} `json:"user"`
}

// toComment maps a decoded Linear comment to forge.Comment. Linear comment IDs
// are UUIDs, so ID stays zero and identity travels via URL (see the T6 summary
// flag: forge.Comment.ID is uint64 and cannot hold a Linear UUID).
func (r linearComment) toComment() Comment {
	account := ""
	if r.User != nil {
		account = r.User.DisplayName
	}
	return Comment{URL: r.URL, Body: r.Body, ForgeAccount: account}
}

// teamIssueFilter builds the Linear IssueFilter for a team's issues, narrowed by
// the forge IssueFilter. State maps to the workflow-state type ("open" ->
// type nin closed, "closed" -> type in closed; "all"/"" -> unfiltered). Labels
// require ALL given names (GitHub's AND semantics): an `and` of per-label
// `some` sub-filters.
func teamIssueFilter(key string, f IssueFilter) map[string]any {
	filter := map[string]any{
		varTeam: map[string]any{varKey: map[string]any{"eq": key}},
	}
	switch f.State {
	case stateOpen:
		filter["state"] = map[string]any{"type": map[string]any{"nin": linearClosedStateTypes}}
	case stateClosed:
		filter["state"] = map[string]any{"type": map[string]any{"in": linearClosedStateTypes}}
	}
	if len(f.Labels) > 0 {
		ands := make([]any, 0, len(f.Labels))
		for _, name := range f.Labels {
			ands = append(ands, map[string]any{
				"labels": map[string]any{"some": map[string]any{"name": map[string]any{"eq": name}}},
			})
		}
		filter["and"] = ands
	}
	return filter
}
