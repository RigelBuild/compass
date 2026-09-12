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
	"errors"
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

	// gqlInputKey is the GraphQL variable name every Linear mutation in this
	// file binds its input object to. Extracted because goconst flags the
	// third occurrence; applied at ALL of them, so a raw "input" appearing
	// here later is a different key rather than a missed conversion.
	gqlInputKey = "input"

	// linearStaleStateMarker is the substring Linear's GraphQL error message
	// carries when a mutation names a workflow state that no longer exists.
	// It is the ONE HTTP-200 rejection invalidate-and-retry-once can fix, so
	// TransitionIssueState's retry gate keys on it rather than on any
	// GraphQL-level rejection.
	linearStaleStateMarker = "Entity not found: WorkflowState"

	// workflowStatePageCap bounds the single unpaginated workflowStates query.
	// 250 is far above any real board (Rigel has eight), so a FULL page is
	// read as truncation and rejected rather than resolved against: a
	// truncated list would make a by-name resolve reject a state that really
	// exists, and could make the default map resolve a candidate that is only
	// apparently sole — the silent wrong-pick §The cross-provider state model
	// exists to make structurally impossible.
	workflowStatePageCap = 250

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

	// Linear workflow-state `type` values the default mapping resolves
	// against. They are a subset of the SDL list quoted at
	// linearClosedStateTypes; only these three are default-map targets.
	linearTypeCompleted = "completed"
	linearTypeUnstarted = "unstarted"
	linearTypeBacklog   = "backlog"

	// workflowStateTTL bounds how long a team's workflow-state list is reused.
	// Unlike a team UUID, a workflow state is renamed, reordered and deleted
	// from the Linear UI, so this cache expires where teamIDs never does. The
	// TTL is a bound on how stale a resolution may be BEFORE the
	// invalidate-and-retry-once path recovers it, not the only recovery — so a
	// few minutes trades a rare extra query against a long staleness window.
	workflowStateTTL = 5 * time.Minute
)

// linearClosedStateTypes are the Linear workflow-state `type` values that map
// to the forge's "closed" truth. Every other type maps to "open". Verified
// against Linear SDL WorkflowState.type: "triage", "backlog", "unstarted",
// "started", "completed", "canceled", "duplicate". "duplicate" is its own
// system-managed terminal category (not a member of "completed"), applied when
// an issue is marked a duplicate; Linear's own Active view is unstarted+started
// only, so a duplicate is never live work (RIG-3590).
var linearClosedStateTypes = []string{"completed", "canceled", "duplicate"}

// LinearConfig configures a Linear client.
type LinearConfig struct {
	Host   string       // GraphQL endpoint URL; "" -> linearDefaultEndpoint
	Token  TokenSource  // required (the shared Linear OAuth client-credentials source, DL-052)
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

	// mu guards resetAt, teamIDs, workflowStates, and the actor-probe fields.
	// The client may be shared between the poll driver and write-RPC goroutines
	// (OQ-6), so all are concurrent read-modify-write; mu is held only around
	// the fast state touches, never across an HTTP round-trip.
	mu sync.Mutex

	// resetAt is the rate-budget gate (see GitHub.resetAt). Non-zero and before
	// now() -> the next call fails fast with ErrBudgetExhausted. Zero -> open.
	resetAt time.Time

	// teamIDs caches Linear team key -> team UUID; a key is resolved once via a
	// teams query and reused for every subsequent CreateIssue.
	teamIDs map[string]string

	// workflowStates caches a team key -> that team's workflow states, with a
	// TTL. It is DELIBERATELY separate from teamIDs: a team UUID is immutable,
	// so teamIDs never invalidates, while a workflow state is renamed,
	// reordered and deleted from the Linear UI. Reusing the invalidation-free
	// cache would wedge every later transition to a renamed state until the
	// process restarts.
	workflowStates map[string]workflowStateCacheEntry

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
		host:           cfg.Host,
		token:          cfg.Token,
		client:         client,
		log:            log,
		teamIDs:        make(map[string]string),
		workflowStates: make(map[string]workflowStateCacheEntry),
		now:            time.Now,
	}
}

// Compile-time proof that Linear satisfies the Provider interface.
var _ Provider = (*Linear)(nil)

// --- Provider: exported methods ----------------------------------------------

// TokenSourceForTest exposes the TokenSource this client was built over, so a
// server-package test can assert two independently-built Linear clients ride
// ONE shared source (the one-instance rule, DEC-4). Production reads it never.
// The mint singleflight coalesces only WITHIN an instance, so a second source
// mints independently against the same app; whether concurrent same-scope
// mints from independent instances coexist is unverified, and sharing one
// instance removes the question (RIG-3135).
func (l *Linear) TokenSourceForTest() TokenSource { return l.token }

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
	if err := l.doGraphQL(ctx, query, map[string]any{gqlInputKey: input}, &out); err != nil {
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
	if err := l.doGraphQL(ctx, query, map[string]any{gqlInputKey: input}, &out); err != nil {
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

// TransitionIssueState moves issue number in the team keyed by repo to the
// workflow state in resolves to, returning the UPDATED issue (the mutation
// response IS the new truth). Resolution order is team -> the team's workflow
// states -> the target state (by NAME when in.WorkflowState is set, by the
// default mapping otherwise) -> the issue UUID -> the issueUpdate mutation, so
// every rejection arm fails BEFORE the issue is touched. in.CloseReason is the
// GitHub refinement, screened at the server arm and ignored here.
//
// A mutation that fails against a state list served from cache is retried ONCE
// against a freshly fetched list: a state renamed or deleted in the Linear UI
// between the resolve and the write is exactly the staleness the TTL cache
// cannot rule out, and re-resolving recovers it in-flight.
func (l *Linear) TransitionIssueState(ctx context.Context, repo string, number uint64, in TransitionState) (Issue, error) {
	fail := func(err error) (Issue, error) {
		return Issue{}, fmt.Errorf("forge: linear transition issue %q#%d: %w", repo, number, err)
	}

	states, cached, err := l.workflowStatesFor(ctx, repo)
	if err != nil {
		return fail(err)
	}
	stateID, err := resolveWorkflowState(repo, states, in)
	if err != nil {
		return fail(err)
	}
	issueID, err := l.resolveIssueID(ctx, repo, number)
	if err != nil {
		return fail(err)
	}

	issue, err := l.issueUpdateState(ctx, issueID, stateID)
	if err == nil {
		return issue, nil
	}
	// Staleness recovery: the ONLY rejection this retry can fix is a state id
	// Linear no longer knows, against a CACHED resolution. Linear answers that
	// on HTTP 200 with linearStaleStateMarker in the message; every OTHER
	// HTTP-200 GraphQL rejection (a permission denial, an issue-validation
	// error, a Linear-side internal error) is refused for a reason a refetch
	// cannot change, and — like a rate limit, an auth failure or a transport
	// fault — must never burn the one retry on a re-issued mutation Linear
	// already declined.
	se, isStatus := errors.AsType[*StatusError](err)
	if !cached || !isStatus || se.Status != http.StatusOK ||
		!strings.Contains(se.Message, linearStaleStateMarker) {
		return fail(err)
	}
	l.invalidateWorkflowStates(repo)
	fresh, _, err := l.workflowStatesFor(ctx, repo)
	if err != nil {
		return fail(err)
	}
	stateID, err = resolveWorkflowState(repo, fresh, in)
	if err != nil {
		return fail(err)
	}
	issue, err = l.issueUpdateState(ctx, issueID, stateID)
	if err != nil {
		return fail(err)
	}
	return issue, nil
}

// TransitionPullRequestState is unsupported (Linear has no PRs) — the
// issues-only-forge case ErrUnsupported was minted for.
func (l *Linear) TransitionPullRequestState(ctx context.Context, repo string, number uint64, in TransitionState) (PullRequest, error) {
	return PullRequest{}, ErrUnsupported
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
	if hint, blocked := l.gateBlocked(); blocked {
		return fmt.Errorf("linear graphql: %w", &RateLimitError{RetryAfter: hint})
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
		// Compute the reset instant once: it arms the gate AND (when a usable
		// header gave a non-zero reset) yields the retry hint. No usable header
		// -> a 0 hint ("no hint"), while armGate still self-arms with the bounded
		// defaultSkip internally.
		reset := l.rateLimitReset(resp)
		l.mu.Lock()
		l.armGate(reset)
		l.mu.Unlock()
		var hint time.Duration
		if !reset.IsZero() {
			if d := reset.Sub(l.now()); d > 0 {
				hint = d
			}
		}
		return fmt.Errorf("linear graphql http %d: %w", status, &RateLimitError{RetryAfter: hint})
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

// gateBlocked reports whether the fail-fast budget gate is armed and, when
// armed, the remaining wait until it re-opens (resetAt-now, clamped >= 0) so the
// caller can surface the hint. It clears a gate whose reset instant has passed
// as a side effect. Guarded by mu.
func (l *Linear) gateBlocked() (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.resetAt.IsZero() {
		return 0, false
	}
	now := l.now()
	if now.Before(l.resetAt) {
		return l.resetAt.Sub(now), true
	}
	l.resetAt = time.Time{}
	return 0, false
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

// workflowState is one of a team's workflow states, as the transition path
// needs it: the id to write, the name a caller may target, and the type the
// default mapping and the consistency check read.
type workflowState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// workflowStateCacheEntry is one team's cached state list plus the instant it
// expires. Expiry is stored (not the fetch time) so a read is one comparison.
type workflowStateCacheEntry struct {
	states    []workflowState
	expiresAt time.Time
}

// workflowStatesQuery is the team workflow-state selection, built once so the
// page cap has ONE source (workflowStatePageCap) shared by the query and the
// truncation guard below rather than a literal repeated in both.
var workflowStatesQuery = fmt.Sprintf(`query CompassTeamWorkflowStates($team: String!) {
  workflowStates(filter: {team: {id: {eq: $team}}}, first: %d) {
    nodes { id name type }
  }
}`, workflowStatePageCap)

// workflowStatesFor returns the workflow states of the team keyed by repo, and
// whether they came from cache (the discriminator TransitionIssueState's
// retry-once arm keys on — a FRESH list that a mutation still rejects is not a
// staleness the cache can fix). An expired or absent entry is refetched. The
// cache is read and written under mu without holding it across the query; a
// concurrent miss issues a redundant (idempotent) fetch at worst.
func (l *Linear) workflowStatesFor(ctx context.Context, repo string) ([]workflowState, bool, error) {
	l.mu.Lock()
	entry, ok := l.workflowStates[repo]
	fresh := ok && l.now().Before(entry.expiresAt)
	l.mu.Unlock()
	if fresh {
		return entry.states, true, nil
	}

	// The states are filtered by team UUID off the existing (immutable,
	// invalidation-free) teamIDs cache, so only the mutable half — the state
	// list itself — rides the TTL.
	teamID, err := l.resolveTeamID(ctx, repo)
	if err != nil {
		return nil, false, err
	}

	var out struct {
		WorkflowStates struct {
			Nodes []workflowState `json:"nodes"`
		} `json:"workflowStates"`
	}
	if err := l.doGraphQL(ctx, workflowStatesQuery, map[string]any{varTeam: teamID}, &out); err != nil {
		return nil, false, err
	}
	states := out.WorkflowStates.Nodes
	if len(states) == 0 {
		return nil, false, &StatusError{Status: http.StatusNotFound, Message: fmt.Sprintf("no workflow states on team %q", repo)}
	}
	// A FULL page is read as truncation: the query is unpaginated, so a list at
	// the cap may be missing states, and resolving against it would reject a
	// name that really exists or default-map to an only-apparently-sole
	// candidate. Fail loud instead — the cap is far above any real board, so
	// hitting it is a Linear-side surprise a caller must be told about, not a
	// pagination loop worth carrying.
	if len(states) >= workflowStatePageCap {
		return nil, false, invalidWorkflowState(
			"team %q returned %d workflow states, the %d-state page cap: the list may be truncated, so no state can be resolved safely; pass an explicit workflow state",
			repo, len(states), workflowStatePageCap)
	}

	l.mu.Lock()
	l.workflowStates[repo] = workflowStateCacheEntry{states: states, expiresAt: l.now().Add(workflowStateTTL)}
	l.mu.Unlock()
	return states, false, nil
}

// invalidateWorkflowStates drops a team's cached state list so the next
// resolution refetches. It is the recovery half of the TTL cache: a state
// renamed or deleted between resolve and write is corrected in-flight rather
// than wedging every transition until the TTL lapses.
func (l *Linear) invalidateWorkflowStates(repo string) {
	l.mu.Lock()
	delete(l.workflowStates, repo)
	l.mu.Unlock()
}

// invalidWorkflowState builds the rejection every workflow-state resolution
// failure returns. The status is 422 because that is the ONE status the
// Service's flattening maps to an in-band `invalid_argument` carrying the
// message (server/forge.go mapForgeError) — the code §The cross-provider state
// model requires for an unknown name, an ambiguous name, a type contradiction
// and a multi-candidate default. Every message names the team and what the
// caller must do differently, since the caller cannot see the board.
func invalidWorkflowState(format string, args ...any) error {
	return &StatusError{Status: http.StatusUnprocessableEntity, Message: fmt.Sprintf(format, args...)}
}

// resolveWorkflowState picks the target workflow-state id from a team's states,
// per §The cross-provider state model. It never touches the network, so every
// rejection lands before the mutation.
//
// With in.WorkflowState set it resolves BY NAME: an unknown name, a name
// matching two states on the one team (Linear does not enforce name uniqueness),
// and a named state whose type contradicts the portable in.State target are each
// a rejection, never a guess. With it empty it default-maps to the SOLE state of
// the target type, and rejects when the team has more than one — naming every
// candidate, so the caller knows exactly which name to pass.
func resolveWorkflowState(repo string, states []workflowState, in TransitionState) (string, error) {
	if in.WorkflowState == "" {
		return defaultWorkflowState(repo, states, in.State)
	}

	matches := make([]workflowState, 0, 1)
	for _, s := range states {
		if s.Name == in.WorkflowState {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
		return "", invalidWorkflowState("team %q has no workflow state named %q", repo, in.WorkflowState)
	case 1:
	default:
		return "", invalidWorkflowState("team %q has %d workflow states named %q; the name does not identify one",
			repo, len(matches), in.WorkflowState)
	}

	// Consistency: the named state's type must agree with the portable target,
	// so `state: closed` can never land on an open-typed column (or the reverse)
	// just because the caller named it.
	if got := mapLinearState(matches[0].Type); got != in.State {
		return "", invalidWorkflowState("workflow state %q on team %q is of type %q (a %s state), which contradicts the requested state %q",
			in.WorkflowState, repo, matches[0].Type, got, in.State)
	}
	return matches[0].ID, nil
}

// defaultWorkflowState maps the portable target to the team's sole state of the
// corresponding type. A close targets `completed` — NEVER `canceled`, the
// deliberate asymmetry against the read-side fold: an agent closing its issue
// means "done", and canceled stays reachable only by naming it. An open targets
// `unstarted`, falling back to `backlog` for a team with no unstarted state.
//
// Two or more candidates is a rejection naming every one of them, not a
// positional guess: silently picking a human-visible board column is the
// behaviour this rule exists to make structurally impossible.
func defaultWorkflowState(repo string, states []workflowState, target string) (string, error) {
	types := []string{linearTypeUnstarted, linearTypeBacklog}
	if target == stateClosed {
		types = []string{linearTypeCompleted}
	}

	for _, want := range types {
		candidates := make([]workflowState, 0, 1)
		for _, s := range states {
			if s.Type == want {
				candidates = append(candidates, s)
			}
		}
		switch len(candidates) {
		case 0:
			continue // an open target falls back from unstarted to backlog
		case 1:
			return candidates[0].ID, nil
		default:
			names := make([]string, 0, len(candidates))
			for _, c := range candidates {
				names = append(names, strconv.Quote(c.Name))
			}
			return "", invalidWorkflowState("team %q has %d workflow states of type %q (%s); pass an explicit workflow state to choose one",
				repo, len(candidates), want, strings.Join(names, ", "))
		}
	}
	return "", invalidWorkflowState("team %q has no workflow state of type %s to map the requested state %q onto",
		repo, strings.Join(types, " or "), target)
}

// issueUpdateState runs the issueUpdate mutation moving issueID to stateID and
// decodes the updated issue through the shared issueFieldsFragment — the same
// decode every read uses, so the returned truth is shaped identically. No
// attribution is applied: createAsUser attributes AUTHORSHIP of created content
// (an issue, a comment), and a transition creates none.
func (l *Linear) issueUpdateState(ctx context.Context, issueID, stateID string) (Issue, error) {
	const query = `mutation CompassIssueStateUpdate($id: String!, $input: IssueUpdateInput!) {
  issueUpdate(id: $id, input: $input) {
    issue { ...CompassIssueFields }
  }
}` + issueFieldsFragment
	var out struct {
		IssueUpdate struct {
			Issue linearIssue `json:"issue"`
		} `json:"issueUpdate"`
	}
	vars := map[string]any{"id": issueID, gqlInputKey: map[string]any{"stateId": stateID}}
	if err := l.doGraphQL(ctx, query, vars, &out); err != nil {
		return Issue{}, err
	}
	return out.IssueUpdate.Issue.toIssue(), nil
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
// forge.Comment.ID (uint64) cannot carry — it travels in Comment.Key instead
// (see toComment).
type linearComment struct {
	ID   string `json:"id"`
	URL  string `json:"url"`
	Body string `json:"body"`
	User *struct {
		DisplayName string `json:"displayName"`
	} `json:"user"`
}

// toComment maps a decoded Linear comment to forge.Comment. Linear comment IDs
// are UUIDs, so ID (uint64) stays zero and the stable identity travels via Key
// (the cross-producer snapshot key; the webhook producer carries the same UUID).
func (r linearComment) toComment() Comment {
	account := ""
	if r.User != nil {
		account = r.User.DisplayName
	}
	return Comment{Key: r.ID, URL: r.URL, Body: r.Body, ForgeAccount: account}
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
