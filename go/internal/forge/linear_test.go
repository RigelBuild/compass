package forge

// Unit tests for the hand-rolled net/http Linear GraphQL client, driven by a
// stubbed http.RoundTripper (no network). Covers the T6 test cycle: issueCreate
// and commentCreate request goldens (query + variables, incl. teamId and
// createAsUser when the actor probe passes), team-key->id resolve-once-and-cache,
// GetIssue / ListIssues read mapping incl. IssueFilter state, ErrUnsupported for
// all five PR/review ops, the 429 -> resource_exhausted mapping, a GraphQL
// errors-on-200 -> *StatusError, the actor-probe-fails degrade path (no
// createAsUser + the exact log line), and 401 -> TokenSource.Invalidate.
// context.Background() here is the test root — the sanctioned F-ttsr exemption.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- stubs -------------------------------------------------------------------

// newTestLinear wires a Linear client over a scripted transport, a fake token
// source, and a captured logger (so the degrade line is assertable).
func newTestLinear(rt *scriptedRoundTripper, ts *fakeTokenSource, log *slog.Logger) *Linear {
	return NewLinear(LinearConfig{
		Token:  ts,
		Client: &http.Client{Transport: rt},
		Log:    log,
	})
}

// capturingHandler records every log record for message assertions.
type capturingHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}
func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }
func (h *capturingHandler) has(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Contains(h.msgs, msg)
}
func (h *capturingHandler) count(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.msgs {
		if m == msg {
			n++
		}
	}
	return n
}

// probeResp is a scripted `viewer { app }` response with the given capability.
func probeResp(app bool) scriptedResponse {
	return scriptedResponse{status: 200, body: `{"data":{"viewer":{"app":` + strconv.FormatBool(app) + `}}}`}
}

// teamResp is a scripted team-key->id lookup response (the id is fixed; tests
// assert behavior, not the specific UUID).
var teamResp = scriptedResponse{status: 200, body: `{"data":{"teams":{"nodes":[{"id":"team-uuid-1"}]}}}`}

const degradeMsg = "linear: actor attribution unavailable; degrading to stamp-only"

// decodeGraphQLReq parses a recorded request body into its query + variables.
func decodeGraphQLReq(t *testing.T, body string) (string, map[string]any) {
	t.Helper()
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode graphql request: %v (body=%q)", err, body)
	}
	return req.Query, req.Variables
}

// --- item 1: issueCreate golden (probe passes -> createAsUser present) -------

func TestLinearCreateIssueRequestGolden(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		teamResp,
		probeResp(true), // actor probe: capable
		{status: 200, body: `{"data":{"issueCreate":{"issue":{
			"number":42,"title":"a bug","description":"stamped body",
			"url":"https://linear.app/x/issue/RIG-42","state":{"name":"Todo","type":"unstarted"},
			"labels":{"nodes":[]},"creator":null,"updatedAt":"2026-08-01T12:30:00Z"}}}}`},
	}}
	l := newTestLinear(rt, &fakeTokenSource{token: "sekret"}, slog.New(&capturingHandler{}))

	got, err := l.CreateIssue(context.Background(), "SEA",
		CreateIssue{Title: "a bug", Body: "stamped body"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	// The create request is the 3rd (probe, team lookup, then create).
	createReq := rt.requests[2]
	if h := createReq.Header.Get("Authorization"); h != "Bearer sekret" {
		t.Errorf("Authorization = %q, want Bearer sekret", h)
	}
	query, vars := decodeGraphQLReq(t, readReqBody(t, createReq))
	if !strings.Contains(query, "issueCreate(input: $input)") {
		t.Errorf("query missing issueCreate mutation: %q", query)
	}
	input, ok := vars["input"].(map[string]any)
	if !ok {
		t.Fatalf("input variable missing/not an object: %#v", vars["input"])
	}
	if input["teamId"] != "team-uuid-1" {
		t.Errorf("teamId = %v, want team-uuid-1", input["teamId"])
	}
	if input["title"] != "a bug" || input["description"] != "stamped body" {
		t.Errorf("title/description wrong: %#v", input)
	}
	if input["createAsUser"] != attributionUser {
		t.Errorf("createAsUser = %v, want %q (probe passed)", input["createAsUser"], attributionUser)
	}
	if input["displayIconUrl"] != attributionIconURL {
		t.Errorf("displayIconUrl = %v, want %q", input["displayIconUrl"], attributionIconURL)
	}
	if got.Number != 42 || got.Title != "a bug" || got.Body != "stamped body" ||
		got.State != "open" || got.URL != "https://linear.app/x/issue/RIG-42" {
		t.Errorf("decoded Issue = %+v", got)
	}
}

// --- item 2: commentCreate golden --------------------------------------------

func TestLinearCommentOnIssueRequestGolden(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: `{"data":{"issues":{"nodes":[{"id":"issue-uuid-9"}]}}}`}, // resolve issue id
		probeResp(true), // actor probe
		{status: 200, body: `{"data":{"commentCreate":{"comment":{
			"id":"comment-uuid","url":"https://linear.app/x/issue/RIG-7#comment-1",
			"body":"a reply","user":null}}}}`},
	}}
	l := newTestLinear(rt, &fakeTokenSource{token: "t"}, slog.New(&capturingHandler{}))

	got, err := l.CommentOnIssue(context.Background(), "SEA", 7, "a reply")
	if err != nil {
		t.Fatalf("CommentOnIssue: %v", err)
	}

	commentReq := rt.requests[2]
	query, vars := decodeGraphQLReq(t, readReqBody(t, commentReq))
	if !strings.Contains(query, "commentCreate(input: $input)") {
		t.Errorf("query missing commentCreate mutation: %q", query)
	}
	input := vars["input"].(map[string]any)
	if input["issueId"] != "issue-uuid-9" {
		t.Errorf("issueId = %v, want issue-uuid-9", input["issueId"])
	}
	if input["body"] != "a reply" {
		t.Errorf("body = %v, want a reply", input["body"])
	}
	if input["createAsUser"] != attributionUser {
		t.Errorf("createAsUser = %v, want %q", input["createAsUser"], attributionUser)
	}
	if got.Body != "a reply" || got.URL != "https://linear.app/x/issue/RIG-7#comment-1" {
		t.Errorf("decoded Comment = %+v", got)
	}
}

// --- item 3: team-key -> id resolved once, then cached -----------------------

func TestLinearTeamIDResolvedOnceThenCached(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		teamResp,        // team lookup (once)
		probeResp(true), // probe (once)
		{status: 200, body: `{"data":{"issueCreate":{"issue":{"number":1,"state":{"type":"unstarted"},"labels":{"nodes":[]},"creator":null}}}}`},
		// Second CreateIssue: probe cached, team cached -> ONLY the create request.
		{status: 200, body: `{"data":{"issueCreate":{"issue":{"number":2,"state":{"type":"unstarted"},"labels":{"nodes":[]},"creator":null}}}}`},
	}}
	l := newTestLinear(rt, &fakeTokenSource{token: "t"}, slog.New(&capturingHandler{}))

	if _, err := l.CreateIssue(context.Background(), "SEA", CreateIssue{Title: "one"}); err != nil {
		t.Fatalf("CreateIssue 1: %v", err)
	}
	callsAfterFirst := rt.calls
	if callsAfterFirst != 3 {
		t.Fatalf("first CreateIssue issued %d requests, want 3 (probe+team+create)", callsAfterFirst)
	}

	if _, err := l.CreateIssue(context.Background(), "SEA", CreateIssue{Title: "two"}); err != nil {
		t.Fatalf("CreateIssue 2: %v", err)
	}
	if extra := rt.calls - callsAfterFirst; extra != 1 {
		t.Fatalf("second CreateIssue issued %d requests, want 1 (create only; team+probe cached)", extra)
	}
	// The second create's team id came from cache, not a new lookup.
	_, vars := decodeGraphQLReq(t, readReqBody(t, rt.requests[3]))
	if input := vars["input"].(map[string]any); input["teamId"] != "team-uuid-1" {
		t.Errorf("cached teamId = %v, want team-uuid-1", input["teamId"])
	}
}

// --- item 4: read-query mapping (GetIssue + ListIssues incl. filter state) ---

func TestLinearGetIssueMapping(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: `{"data":{"issues":{"nodes":[{
			"number":7,"title":"a bug","description":"raw <!--owner--> body",
			"url":"https://linear.app/x/issue/RIG-7","state":{"name":"Done","type":"completed"},
			"labels":{"nodes":[{"name":"bug"},{"name":"p1"}]},
			"creator":{"displayName":"alice"},"updatedAt":"2026-08-01T12:30:00Z"}]}}}`},
	}}
	l := newTestLinear(rt, &fakeTokenSource{token: "t"}, slog.New(&capturingHandler{}))

	got, err := l.GetIssue(context.Background(), "SEA", 7)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if got.Number != 7 || got.Title != "a bug" {
		t.Errorf("scalars wrong: %+v", got)
	}
	if got.Body != "raw <!--owner--> body" {
		t.Errorf("body not raw/untouched: %q", got.Body)
	}
	if got.State != "closed" {
		t.Errorf("State = %q, want closed (type=completed)", got.State)
	}
	if got.ForgeAccount != "alice" {
		t.Errorf("ForgeAccount = %q, want alice", got.ForgeAccount)
	}
	if strings.Join(got.Labels, ",") != "bug,p1" {
		t.Errorf("Labels = %v", got.Labels)
	}
	want := time.Date(2026, 8, 1, 12, 30, 0, 0, time.UTC)
	if !got.UpdatedAt.Equal(want) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, want)
	}

	// The read query filters by team key + number.
	_, vars := decodeGraphQLReq(t, readReqBody(t, rt.requests[0]))
	filter := vars["filter"].(map[string]any)
	team := filter["team"].(map[string]any)["key"].(map[string]any)
	if team["eq"] != "SEA" {
		t.Errorf("team key filter = %v, want SEA", team["eq"])
	}
	if num := filter["number"].(map[string]any); num["eq"] != float64(7) {
		t.Errorf("number filter = %v, want 7", num["eq"])
	}
}

func TestLinearGetIssueNotFound(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: `{"data":{"issues":{"nodes":[]}}}`},
	}}
	l := newTestLinear(rt, &fakeTokenSource{token: "t"}, slog.New(&capturingHandler{}))

	_, err := l.GetIssue(context.Background(), "SEA", 999)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 404 {
		t.Fatalf("err = %v, want *StatusError 404", err)
	}
}

func TestLinearListIssuesFilterAndPagination(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: `{"data":{"issues":{
			"nodes":[{"number":1,"state":{"type":"started"},"labels":{"nodes":[]},"creator":null}],
			"pageInfo":{"hasNextPage":true,"endCursor":"CUR1"}}}}`},
		{status: 200, body: `{"data":{"issues":{
			"nodes":[{"number":2,"state":{"type":"completed"},"labels":{"nodes":[]},"creator":null}],
			"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`},
	}}
	l := newTestLinear(rt, &fakeTokenSource{token: "t"}, slog.New(&capturingHandler{}))

	got, err := l.ListIssues(context.Background(), "SEA", IssueFilter{State: "open", Labels: []string{"bug"}})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(got) != 2 || got[0].Number != 1 || got[1].Number != 2 {
		t.Fatalf("walk concatenation wrong: %+v", got)
	}
	if got[0].State != "open" || got[1].State != "closed" {
		t.Errorf("state mapping wrong: %q, %q", got[0].State, got[1].State)
	}

	// Page 1 request: open-state filter -> type nin closed; label AND sub-filter.
	_, vars := decodeGraphQLReq(t, readReqBody(t, rt.requests[0]))
	filter := vars["filter"].(map[string]any)
	stateType := filter["state"].(map[string]any)["type"].(map[string]any)
	if _, ok := stateType["nin"]; !ok {
		t.Errorf("open state should map to type nin closed; got %#v", stateType)
	}
	if _, ok := filter["and"]; !ok {
		t.Errorf("label filter should produce an `and` of some-sub-filters; got %#v", filter)
	}
	if _, ok := vars["after"]; ok {
		t.Errorf("page 1 should not send an after cursor; got %v", vars["after"])
	}

	// Page 2 request carries the endCursor from page 1.
	_, vars2 := decodeGraphQLReq(t, readReqBody(t, rt.requests[1]))
	if vars2["after"] != "CUR1" {
		t.Errorf("page 2 after = %v, want CUR1", vars2["after"])
	}
}

func TestLinearListIssuesClosedState(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`},
	}}
	l := newTestLinear(rt, &fakeTokenSource{token: "t"}, slog.New(&capturingHandler{}))

	if _, err := l.ListIssues(context.Background(), "SEA", IssueFilter{State: "closed"}); err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	_, vars := decodeGraphQLReq(t, readReqBody(t, rt.requests[0]))
	stateType := vars["filter"].(map[string]any)["state"].(map[string]any)["type"].(map[string]any)
	if _, ok := stateType["in"]; !ok {
		t.Errorf("closed state should map to type in closed; got %#v", stateType)
	}
}

// --- item 5: ErrUnsupported for the five PR/review ops -----------------------

func TestLinearUnsupportedOps(t *testing.T) {
	rt := &scriptedRoundTripper{}
	l := newTestLinear(rt, &fakeTokenSource{token: "t"}, slog.New(&capturingHandler{}))
	ctx := context.Background()

	if _, err := l.CreatePullRequest(ctx, "SEA", CreatePR{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("CreatePullRequest err = %v, want ErrUnsupported", err)
	}
	if _, err := l.CommentOnPullRequest(ctx, "SEA", 1, "x"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("CommentOnPullRequest err = %v, want ErrUnsupported", err)
	}
	if _, err := l.SubmitReview(ctx, "SEA", 1, SubmitReview{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("SubmitReview err = %v, want ErrUnsupported", err)
	}
	if _, err := l.GetPullRequest(ctx, "SEA", 1); !errors.Is(err, ErrUnsupported) {
		t.Errorf("GetPullRequest err = %v, want ErrUnsupported", err)
	}
	if _, err := l.Checks(ctx, "SEA", 1); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Checks err = %v, want ErrUnsupported", err)
	}
	// The unsupported ops make no wire call.
	if rt.calls != 0 {
		t.Errorf("unsupported ops issued %d requests, want 0", rt.calls)
	}
}

// --- item 6: 429 -> resource_exhausted (ErrBudgetExhausted) ------------------

func TestLinear429MapsToBudgetExhausted(t *testing.T) {
	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 429, body: `{"errors":[{"message":"rate limited"}]}`, headers: map[string]string{"Retry-After": "30"}},
	}}
	ts := &fakeTokenSource{token: "t"}
	l := newTestLinear(rt, ts, slog.New(&capturingHandler{}))
	l.now = func() time.Time { return base }

	// A read triggers the 429 directly (no probe on the read path).
	_, err := l.GetIssue(context.Background(), "SEA", 7)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if ts.invalidated != 0 {
		t.Errorf("rate-limit must not Invalidate; got %d", ts.invalidated)
	}
	// The gate is armed: the next call fails fast without a request.
	if _, err := l.GetIssue(context.Background(), "SEA", 8); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("gated call err = %v, want ErrBudgetExhausted", err)
	}
	if rt.calls != 1 {
		t.Errorf("gate issued a request: calls = %d, want 1", rt.calls)
	}
}

func TestLinearGraphQLRateLimitedCodeOn400(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 400, body: `{"errors":[{"message":"complex","extensions":{"code":"RATELIMITED"}}]}`},
	}}
	l := newTestLinear(rt, &fakeTokenSource{token: "t"}, slog.New(&capturingHandler{}))

	_, err := l.GetIssue(context.Background(), "SEA", 7)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted (RATELIMITED code)", err)
	}
}

// item 6 (cont.): the Linear rate-limit error carries the retry hint as a
// *RateLimitError while remaining errors.Is(ErrBudgetExhausted)-compatible.
// (a) live 429 + Retry-After: 60 -> 60s; (b) the X-Ratelimit-Requests-Reset
// epoch-ms fallback -> the epoch-derived duration; (c) a header-less RATELIMITED
// GraphQL rejection -> 0 with the gate armed; (d) the gated fail-fast call ->
// resetAt-now.
func TestLinearRateLimitHint(t *testing.T) {
	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

	t.Run("live 429 Retry-After carries hint", func(t *testing.T) {
		rt := &scriptedRoundTripper{responses: []scriptedResponse{
			{status: 429, body: `{"errors":[{"message":"rate limited"}]}`, headers: map[string]string{"Retry-After": "60"}},
		}}
		l := newTestLinear(rt, &fakeTokenSource{token: "t"}, slog.New(&capturingHandler{}))
		l.now = func() time.Time { return base }

		_, err := l.GetIssue(context.Background(), "SEA", 7)
		var rle *RateLimitError
		if !errors.As(err, &rle) {
			t.Fatalf("err = %v, want *RateLimitError", err)
		}
		if rle.RetryAfter != 60*time.Second {
			t.Errorf("RetryAfter = %v, want 60s", rle.RetryAfter)
		}
		if !errors.Is(err, ErrBudgetExhausted) {
			t.Error("err no longer matches ErrBudgetExhausted")
		}
	})

	t.Run("epoch-ms reset fallback carries hint", func(t *testing.T) {
		at := base.Add(90 * time.Second)
		rt := &scriptedRoundTripper{responses: []scriptedResponse{
			{status: 429, body: `{"errors":[{"message":"rate limited"}]}`, headers: map[string]string{
				"X-Ratelimit-Requests-Reset": strconv.FormatInt(at.UnixMilli(), 10),
			}},
		}}
		l := newTestLinear(rt, &fakeTokenSource{token: "t"}, slog.New(&capturingHandler{}))
		l.now = func() time.Time { return base }

		_, err := l.GetIssue(context.Background(), "SEA", 7)
		var rle *RateLimitError
		if !errors.As(err, &rle) {
			t.Fatalf("err = %v, want *RateLimitError", err)
		}
		if rle.RetryAfter != 90*time.Second {
			t.Errorf("RetryAfter = %v, want 90s (epoch-ms fallback)", rle.RetryAfter)
		}
	})

	t.Run("header-less RATELIMITED -> 0 hint, gate armed", func(t *testing.T) {
		clock := base
		rt := &scriptedRoundTripper{responses: []scriptedResponse{
			{status: 400, body: `{"errors":[{"message":"complex","extensions":{"code":"RATELIMITED"}}]}`},
		}}
		l := newTestLinear(rt, &fakeTokenSource{token: "t"}, slog.New(&capturingHandler{}))
		l.now = func() time.Time { return clock }

		_, err := l.GetIssue(context.Background(), "SEA", 7)
		var rle *RateLimitError
		if !errors.As(err, &rle) {
			t.Fatalf("err = %v, want *RateLimitError", err)
		}
		if rle.RetryAfter != 0 {
			t.Errorf("RetryAfter = %v, want 0 (no usable header)", rle.RetryAfter)
		}
		// The gate still arms with defaultSkip: the next call fails fast.
		clock = base.Add(time.Second)
		if _, err := l.GetIssue(context.Background(), "SEA", 8); !errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("gated call err = %v, want ErrBudgetExhausted", err)
		}
		if rt.calls != 1 {
			t.Errorf("gate did not arm: calls = %d, want 1", rt.calls)
		}
	})

	t.Run("fail-fast hint == resetAt - now", func(t *testing.T) {
		clock := base
		rt := &scriptedRoundTripper{responses: []scriptedResponse{
			{status: 429, body: `{"errors":[{"message":"rate limited"}]}`, headers: map[string]string{"Retry-After": "60"}},
		}}
		l := newTestLinear(rt, &fakeTokenSource{token: "t"}, slog.New(&capturingHandler{}))
		l.now = func() time.Time { return clock }

		if _, err := l.GetIssue(context.Background(), "SEA", 7); !errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("call 1 err = %v, want ErrBudgetExhausted", err)
		}
		clock = base.Add(59 * time.Second) // 1s remains of the window
		_, err := l.GetIssue(context.Background(), "SEA", 8)
		var rle *RateLimitError
		if !errors.As(err, &rle) {
			t.Fatalf("gated err = %v, want *RateLimitError", err)
		}
		if rle.RetryAfter != time.Second {
			t.Errorf("fail-fast RetryAfter = %v, want 1s (resetAt-now)", rle.RetryAfter)
		}
		if rt.calls != 1 {
			t.Errorf("gate issued a request: calls = %d, want 1", rt.calls)
		}
	})
}

// --- item 7: GraphQL errors on HTTP 200 -> *StatusError ----------------------

func TestLinearGraphQLErrorsOn200(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: `{"errors":[{"message":"Field bad"},{"message":"also bad"}]}`},
	}}
	l := newTestLinear(rt, &fakeTokenSource{token: "t"}, slog.New(&capturingHandler{}))

	_, err := l.GetIssue(context.Background(), "SEA", 7)
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *StatusError", err)
	}
	if se.Status != 200 {
		t.Errorf("Status = %d, want 200 (GraphQL error on a 200)", se.Status)
	}
	if se.Message != "Field bad; also bad" {
		t.Errorf("Message = %q, want joined messages", se.Message)
	}
}

// --- item 8: actor probe FAILS -> no createAsUser + the exact log line -------

func TestLinearActorProbeDegradesToStampOnly(t *testing.T) {
	cap := &capturingHandler{}
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		teamResp,
		probeResp(false), // probe: NOT an actor=app token
		{status: 200, body: `{"data":{"issueCreate":{"issue":{"number":1,"state":{"type":"unstarted"},"labels":{"nodes":[]},"creator":null}}}}`},
		// Second create: probe cached -> team+create only, still no createAsUser.
		{status: 200, body: `{"data":{"issueCreate":{"issue":{"number":2,"state":{"type":"unstarted"},"labels":{"nodes":[]},"creator":null}}}}`},
	}}
	l := newTestLinear(rt, &fakeTokenSource{token: "t"}, slog.New(cap))

	if _, err := l.CreateIssue(context.Background(), "SEA", CreateIssue{Title: "x"}); err != nil {
		t.Fatalf("CreateIssue 1: %v", err)
	}
	_, vars := decodeGraphQLReq(t, readReqBody(t, rt.requests[2]))
	input := vars["input"].(map[string]any)
	if _, ok := input["createAsUser"]; ok {
		t.Errorf("degraded write must NOT set createAsUser; got %#v", input)
	}
	if _, ok := input["displayIconUrl"]; ok {
		t.Errorf("degraded write must NOT set displayIconUrl; got %#v", input)
	}
	if !cap.has(degradeMsg) {
		t.Fatalf("expected the exact degrade log line %q; got %v", degradeMsg, cap.msgs)
	}

	// The probe (and its log line) fire exactly once, even across writes.
	if _, err := l.CreateIssue(context.Background(), "SEA", CreateIssue{Title: "y"}); err != nil {
		t.Fatalf("CreateIssue 2: %v", err)
	}
	if n := cap.count(degradeMsg); n != 1 {
		t.Errorf("degrade line logged %d times, want exactly 1", n)
	}
}

// --- item 9: 401 -> TokenSource.Invalidate + StatusError ---------------------

func TestLinear401Invalidates(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 401, body: `{"errors":[{"message":"Authentication required"}]}`},
	}}
	ts := &fakeTokenSource{token: "t"}
	l := newTestLinear(rt, ts, slog.New(&capturingHandler{}))

	_, err := l.GetIssue(context.Background(), "SEA", 7)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 401 {
		t.Fatalf("err = %v, want *StatusError 401", err)
	}
	if ts.invalidated != 1 {
		t.Errorf("401 must Invalidate; got %d", ts.invalidated)
	}
}

func TestLinearGraphQLAuthErrorCodeInvalidates(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: `{"errors":[{"message":"bad token","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`},
	}}
	ts := &fakeTokenSource{token: "t"}
	l := newTestLinear(rt, ts, slog.New(&capturingHandler{}))

	_, err := l.GetIssue(context.Background(), "SEA", 7)
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *StatusError", err)
	}
	if ts.invalidated != 1 {
		t.Errorf("AUTHENTICATION_ERROR must Invalidate; got %d", ts.invalidated)
	}
}

// --- misc: token error short-circuits before any request --------------------

func TestLinearTokenErrorNoWire(t *testing.T) {
	rt := &scriptedRoundTripper{}
	tokErr := errors.New("resolve failed")
	l := newTestLinear(rt, &fakeTokenSource{err: tokErr}, slog.New(&capturingHandler{}))

	_, err := l.GetIssue(context.Background(), "SEA", 7)
	if !errors.Is(err, tokErr) {
		t.Fatalf("err = %v, want token error", err)
	}
	if rt.calls != 0 {
		t.Errorf("issued a request despite token error: calls = %d", rt.calls)
	}
}

func TestLinearBodyLimit(t *testing.T) {
	l := newTestLinear(&scriptedRoundTripper{}, &fakeTokenSource{token: "t"}, slog.New(&capturingHandler{}))
	if got := l.BodyLimit(); got != linearBodyLimit {
		t.Errorf("BodyLimit() = %d, want %d", got, linearBodyLimit)
	}
}

func TestLinearName(t *testing.T) {
	l := newTestLinear(&scriptedRoundTripper{}, &fakeTokenSource{token: "t"}, slog.New(&capturingHandler{}))
	if got := l.Name(); got != providerLinear {
		t.Errorf("Name() = %q, want linear", got)
	}
}

// linearIssueNodeResp is a minimal valid issues-query response (GetIssue reads
// the first node); enough fields to decode through toIssue without error.
func linearIssueNodeResp(number int) string {
	return `{"data":{"issues":{"nodes":[{"number":` + strconv.Itoa(number) +
		`,"state":{"type":"unstarted"},"labels":{"nodes":[]},"creator":null}]}}}`
}

// --- budget recording on the SUCCESS path (proactive gate) -------------------

// A 200 whose X-Ratelimit-Requests-Remaining EQUALS the reserve arms the gate,
// so the next call fails fast without a wire request. Pins the `remaining >
// reserve` boundary, the recordBudget call site, and the epoch-ms reset parse.
func TestLinearBudgetGateRemainingAtReserveArms(t *testing.T) {
	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	resetMS := strconv.FormatInt(base.Add(time.Minute).UnixMilli(), 10)
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: linearIssueNodeResp(7), headers: map[string]string{
			"X-Ratelimit-Requests-Remaining": strconv.Itoa(reserve), // == reserve -> arms
			"X-Ratelimit-Requests-Reset":     resetMS,
		}},
	}}
	l := newTestLinear(rt, &fakeTokenSource{token: "t"}, slog.New(&capturingHandler{}))
	l.now = func() time.Time { return base }

	if _, err := l.GetIssue(context.Background(), "SEA", 7); err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if _, err := l.GetIssue(context.Background(), "SEA", 8); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("gated call err = %v, want ErrBudgetExhausted", err)
	}
	if rt.calls != 1 {
		t.Errorf("gate issued a request: calls = %d, want 1", rt.calls)
	}
}

// A 200 with remaining ABOVE the reserve leaves the gate open: the next call
// issues a real request. Guards against a `>`→`>=` regression at the boundary.
func TestLinearBudgetGateRemainingAboveReserveStaysOpen(t *testing.T) {
	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	resetMS := strconv.FormatInt(base.Add(time.Minute).UnixMilli(), 10)
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: linearIssueNodeResp(7), headers: map[string]string{
			"X-Ratelimit-Requests-Remaining": strconv.Itoa(reserve + 1), // > reserve -> open
			"X-Ratelimit-Requests-Reset":     resetMS,
		}},
		{status: 200, body: linearIssueNodeResp(8)},
	}}
	l := newTestLinear(rt, &fakeTokenSource{token: "t"}, slog.New(&capturingHandler{}))
	l.now = func() time.Time { return base }

	if _, err := l.GetIssue(context.Background(), "SEA", 7); err != nil {
		t.Fatalf("GetIssue 1: %v", err)
	}
	if _, err := l.GetIssue(context.Background(), "SEA", 8); err != nil {
		t.Fatalf("GetIssue 2 (gate should be open): %v", err)
	}
	if rt.calls != 2 {
		t.Errorf("calls = %d, want 2 (gate stayed open)", rt.calls)
	}
}

// --- rateLimitReset: both non-delta-seconds branches -------------------------

// A 429 with Retry-After as an HTTP-date arms the gate at exactly that instant.
func TestLinearRateLimitResetHTTPDate(t *testing.T) {
	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	at := base.Add(45 * time.Second)
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 429, body: `{"errors":[{"message":"rate limited"}]}`, headers: map[string]string{
			"Retry-After": at.Format(http.TimeFormat),
		}},
	}}
	l := newTestLinear(rt, &fakeTokenSource{token: "t"}, slog.New(&capturingHandler{}))
	l.now = func() time.Time { return base }

	if _, err := l.GetIssue(context.Background(), "SEA", 7); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if !l.resetAt.Equal(at) {
		t.Errorf("resetAt = %v, want %v (Retry-After HTTP-date)", l.resetAt, at)
	}
}

// A 429 with no Retry-After falls back to X-Ratelimit-Requests-Reset (epoch ms).
func TestLinearRateLimitResetHeaderFallback(t *testing.T) {
	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	at := base.Add(90 * time.Second)
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 429, body: `{"errors":[{"message":"rate limited"}]}`, headers: map[string]string{
			"X-Ratelimit-Requests-Reset": strconv.FormatInt(at.UnixMilli(), 10),
		}},
	}}
	l := newTestLinear(rt, &fakeTokenSource{token: "t"}, slog.New(&capturingHandler{}))
	l.now = func() time.Time { return base }

	if _, err := l.GetIssue(context.Background(), "SEA", 7); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if !l.resetAt.Equal(time.UnixMilli(at.UnixMilli())) {
		t.Errorf("resetAt = %v, want %v (epoch-ms fallback)", l.resetAt, at)
	}
}

// --- actor probe: a TRANSIENT error is not cached; a later write re-probes ---

func TestLinearActorProbeTransientErrorReprobed(t *testing.T) {
	cap := &capturingHandler{}
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		teamResp,
		// Write 1's probe hits a transient 500 -> degrade this write, do NOT cache.
		{status: 500, body: `{"errors":[{"message":"internal"}]}`},
		{status: 200, body: `{"data":{"issueCreate":{"issue":{"number":1,"state":{"type":"unstarted"},"labels":{"nodes":[]},"creator":null}}}}`},
		// Write 2 re-probes (transient cleared) -> capable -> createAsUser set.
		probeResp(true),
		{status: 200, body: `{"data":{"issueCreate":{"issue":{"number":2,"state":{"type":"unstarted"},"labels":{"nodes":[]},"creator":null}}}}`},
	}}
	l := newTestLinear(rt, &fakeTokenSource{token: "t"}, slog.New(cap))

	if _, err := l.CreateIssue(context.Background(), "SEA", CreateIssue{Title: "x"}); err != nil {
		t.Fatalf("CreateIssue 1: %v", err)
	}
	_, vars1 := decodeGraphQLReq(t, readReqBody(t, rt.requests[2]))
	if _, ok := vars1["input"].(map[string]any)["createAsUser"]; ok {
		t.Errorf("transient-probe-fail write must NOT set createAsUser")
	}
	if !cap.has(degradeMsg) {
		t.Errorf("transient probe failure must emit the degrade line")
	}

	if _, err := l.CreateIssue(context.Background(), "SEA", CreateIssue{Title: "y"}); err != nil {
		t.Fatalf("CreateIssue 2: %v", err)
	}
	_, vars2 := decodeGraphQLReq(t, readReqBody(t, rt.requests[4]))
	if vars2["input"].(map[string]any)["createAsUser"] != attributionUser {
		t.Errorf("re-probe capable write must set createAsUser=%q (proves no permanent cache); got %#v",
			attributionUser, vars2["input"])
	}
}
