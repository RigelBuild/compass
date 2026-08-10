package forge

// Unit tests for the hand-rolled net/http GitHub read client, driven by a
// stubbed http.RoundTripper (no network). Covers the T1 test cycle: JSON
// parsing incl. UpdatedAt, conditional GET/304, the x-ratelimit budget gate,
// pagination + HasNext + the ListIssues walk, filter->query mapping,
// pull_request-row exclusion, StatusError mapping + the 403 disambiguation /
// TokenSource.Invalidate rule, bearer auth + token-error propagation, the
// malformed-header non-wedge, and ctx cancellation mid-walk.
// context.Background() here is the test root — the sanctioned F-ttsr exemption.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- stubs -------------------------------------------------------------------

// scriptedRoundTripper serves a queue of scripted responses (or a per-call
// func), recording each request for assertions and counting calls.
type scriptedRoundTripper struct {
	responses []scriptedResponse
	calls     int
	requests  []*http.Request
}

type scriptedResponse struct {
	status  int
	body    string
	headers map[string]string
}

func (rt *scriptedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.requests = append(rt.requests, req)
	i := rt.calls
	rt.calls++
	if i >= len(rt.responses) {
		return nil, errors.New("scriptedRoundTripper: no scripted response for call")
	}
	sr := rt.responses[i]
	h := http.Header{}
	for k, v := range sr.headers {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: sr.status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(sr.body)),
	}, nil
}

// fakeTokenSource yields a fixed token (or a scripted error) and counts
// Invalidate calls.
type fakeTokenSource struct {
	token       string
	err         error
	invalidated int
}

func (f *fakeTokenSource) Token(context.Context) (string, error) { return f.token, f.err }
func (f *fakeTokenSource) Invalidate()                           { f.invalidated++ }

func newTestGitHub(rt *scriptedRoundTripper, ts *fakeTokenSource) *GitHub {
	return NewGitHub(GitHubConfig{
		Host:   "github.com",
		Token:  ts,
		Client: &http.Client{Transport: rt},
	})
}

const oneIssueBody = `[{
	"number": 7,
	"title": "a bug",
	"body": "raw <!--owner--> body",
	"state": "open",
	"html_url": "https://github.com/org/repo/issues/7",
	"updated_at": "2026-08-01T12:30:00Z",
	"user": {"login": "octocat"},
	"labels": [{"name": "bug"}, {"name": "p1"}]
}]`

// --- item 1: 200 with issues JSON parsed field-by-field ----------------------

func TestListIssuesPageParsesFields(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: oneIssueBody, headers: map[string]string{"ETag": `"abc"`}},
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	page, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 1, "")
	if err != nil {
		t.Fatalf("ListIssuesPage: %v", err)
	}
	if len(page.Issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(page.Issues))
	}
	got := page.Issues[0]
	if got.Number != 7 || got.Title != "a bug" || got.State != "open" {
		t.Errorf("scalar fields wrong: %+v", got)
	}
	if got.Body != "raw <!--owner--> body" {
		t.Errorf("body not raw/untouched: %q", got.Body)
	}
	if got.URL != "https://github.com/org/repo/issues/7" {
		t.Errorf("URL = %q", got.URL)
	}
	if got.ForgeAccount != "octocat" {
		t.Errorf("ForgeAccount = %q", got.ForgeAccount)
	}
	if strings.Join(got.Labels, ",") != "bug,p1" {
		t.Errorf("Labels = %v", got.Labels)
	}
	want := time.Date(2026, 8, 1, 12, 30, 0, 0, time.UTC)
	if !got.UpdatedAt.Equal(want) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, want)
	}
	if page.ETag != `"abc"` {
		t.Errorf("ETag = %q", page.ETag)
	}
}

// --- item 2: conditional request / 304 --------------------------------------

func TestListIssuesPageConditional(t *testing.T) {
	t.Run("non-empty etag sends If-None-Match and 304 -> NotModified", func(t *testing.T) {
		rt := &scriptedRoundTripper{responses: []scriptedResponse{
			{status: http.StatusNotModified, headers: map[string]string{"ETag": `"abc"`}},
		}}
		g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

		page, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 1, `"abc"`)
		if err != nil {
			t.Fatalf("ListIssuesPage: %v", err)
		}
		if !page.NotModified {
			t.Error("expected NotModified")
		}
		if page.Issues != nil {
			t.Errorf("expected nil Issues on 304, got %v", page.Issues)
		}
		if page.ETag != "" || page.HasNext {
			t.Errorf("expected zero ETag/HasNext on 304, got %q/%v", page.ETag, page.HasNext)
		}
		if got := rt.requests[0].Header.Get("If-None-Match"); got != `"abc"` {
			t.Errorf("If-None-Match = %q, want %q", got, `"abc"`)
		}
	})

	t.Run("empty etag sends no If-None-Match", func(t *testing.T) {
		rt := &scriptedRoundTripper{responses: []scriptedResponse{{status: 200, body: "[]"}}}
		g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

		if _, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 1, ""); err != nil {
			t.Fatalf("ListIssuesPage: %v", err)
		}
		if _, ok := rt.requests[0].Header["If-None-Match"]; ok {
			t.Error("expected no If-None-Match header on unconditional fetch")
		}
	})
}

// --- item 3: budget gate on zeroed remaining --------------------------------

func TestBudgetGateZeroRemaining(t *testing.T) {
	// A fake clock lets us drive reset->resume through the PUBLIC API with no
	// real sleeps: the gate arms until x-ratelimit-reset, then re-opens once the
	// clock passes it.
	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	clock := base
	resetUnix := strconv.FormatInt(base.Add(30*time.Second).Unix(), 10)

	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: "[]", headers: map[string]string{
			"X-RateLimit-Remaining": "0",
			"X-RateLimit-Reset":     resetUnix,
		}},
		{status: 200, body: "[]", headers: map[string]string{"X-RateLimit-Remaining": "5000"}},
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})
	g.now = func() time.Time { return clock }

	// Call 1 sees remaining=0 -> arms the gate until the reset instant.
	if _, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 1, ""); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	// Call 2, still before the reset, must fail fast WITHOUT issuing a request.
	_, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 2, "")
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("call 2 err = %v, want ErrBudgetExhausted", err)
	}
	if rt.calls != 1 {
		t.Fatalf("gate issued a request: RoundTripper calls = %d, want 1", rt.calls)
	}

	// Advance the fake clock past the reset window; the gate re-opens and the
	// next call issues a REAL request (call count increments) and succeeds.
	clock = base.Add(31 * time.Second)
	if _, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 2, ""); err != nil {
		t.Fatalf("resumed call: %v", err)
	}
	if rt.calls != 2 {
		t.Fatalf("resumed call did not issue request: calls = %d, want 2", rt.calls)
	}
}

// item 3 (cont.): the 403/429 Retry-After path arms the gate for the given
// seconds and self-clears once the fake clock advances past it.
func TestBudgetGateRetryAfterSelfClears(t *testing.T) {
	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	clock := base

	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 429, body: `{"message":"rate limited"}`, headers: map[string]string{"Retry-After": "60"}},
		{status: 200, body: "[]", headers: map[string]string{"X-RateLimit-Remaining": "5000"}},
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})
	g.now = func() time.Time { return clock }

	// Call 1: a 429 with Retry-After: 60 -> budget skip, arms the gate 60s out.
	_, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 1, "")
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("call 1 err = %v, want ErrBudgetExhausted", err)
	}
	// Call 2, still within the Retry-After window, fails fast with no request.
	clock = base.Add(59 * time.Second)
	_, err = g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 2, "")
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("call 2 err = %v, want ErrBudgetExhausted", err)
	}
	if rt.calls != 1 {
		t.Fatalf("gate issued a request early: RoundTripper calls = %d, want 1", rt.calls)
	}

	// Past the Retry-After window: the gate self-clears and the call resumes.
	clock = base.Add(61 * time.Second)
	if _, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 2, ""); err != nil {
		t.Fatalf("resumed call: %v", err)
	}
	if rt.calls != 2 {
		t.Fatalf("resumed call did not issue request: calls = %d, want 2", rt.calls)
	}
}

// item 3 (cont.): the budget floor is `remaining <= reserve` (reserve=10). A 200
// whose X-RateLimit-Remaining EQUALS the reserve arms the gate — this pins the
// boundary so a regression flipping the comparison to `<` (arming only strictly
// below reserve) goes red.
func TestBudgetGateRemainingEqualsReserve(t *testing.T) {
	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	resetUnix := strconv.FormatInt(base.Add(30*time.Second).Unix(), 10)

	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: "[]", headers: map[string]string{
			"X-RateLimit-Remaining": "10", // == reserve -> arms the gate
			"X-RateLimit-Reset":     resetUnix,
		}},
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})
	g.now = func() time.Time { return base }

	// Call 1 sees remaining == reserve -> arms the gate.
	if _, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 1, ""); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	// Call 2, still before the reset, must fail fast WITHOUT issuing a request.
	_, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 2, "")
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("call 2 err = %v, want ErrBudgetExhausted (reserve boundary)", err)
	}
	if rt.calls != 1 {
		t.Fatalf("gate did not arm at remaining==reserve: calls = %d, want 1", rt.calls)
	}
}

// item 3 (cont.): the 304 arm records the budget too (the steady-state happy
// path: most polls return 304). A 304 carrying x-ratelimit-remaining at/under
// the reserve arms the gate, so the NEXT call fails fast — this pins the 304-arm
// recordBudget call site so removing it goes red (the 2xx arm is covered by the
// remaining=0/=reserve tests above; without this the 304 arm was uncovered).
func TestBudgetGate304RecordsRemaining(t *testing.T) {
	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	resetUnix := strconv.FormatInt(base.Add(30*time.Second).Unix(), 10)

	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: http.StatusNotModified, headers: map[string]string{
			"X-RateLimit-Remaining": "0", // arms the gate from the 304 arm
			"X-RateLimit-Reset":     resetUnix,
		}},
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})
	g.now = func() time.Time { return base }

	// Call 1 is a 304 whose headers arm the gate.
	p, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 1, `"etag"`)
	if err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if !p.NotModified {
		t.Fatalf("call 1 NotModified = false, want true")
	}
	// Call 2, still before the reset, must fail fast WITHOUT issuing a request —
	// proving the 304 arm recorded the budget.
	_, err = g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 2, "")
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("call 2 err = %v, want ErrBudgetExhausted (304 arm must record budget)", err)
	}
	if rt.calls != 1 {
		t.Fatalf("304 arm did not record the budget: calls = %d, want 1", rt.calls)
	}
}

// RateLimitRemaining is carried on ListPage from x-ratelimit-remaining for the
// driver's observability log: populated on a 200 and a 304, and -1 (a
// "no signal" sentinel, distinct from a genuine 0) when the header is absent.
func TestListPageCarriesRateLimitRemaining(t *testing.T) {
	t.Run("200 carries remaining", func(t *testing.T) {
		rt := &scriptedRoundTripper{responses: []scriptedResponse{
			{status: 200, body: "[]", headers: map[string]string{"X-RateLimit-Remaining": "4321"}},
		}}
		g := newTestGitHub(rt, &fakeTokenSource{token: "t"})
		p, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 1, "")
		if err != nil {
			t.Fatalf("ListIssuesPage: %v", err)
		}
		if p.RateLimitRemaining != 4321 {
			t.Errorf("RateLimitRemaining = %d, want 4321", p.RateLimitRemaining)
		}
	})
	t.Run("304 carries remaining", func(t *testing.T) {
		rt := &scriptedRoundTripper{responses: []scriptedResponse{
			{status: http.StatusNotModified, headers: map[string]string{"X-RateLimit-Remaining": "77"}},
		}}
		g := newTestGitHub(rt, &fakeTokenSource{token: "t"})
		p, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 1, `"e"`)
		if err != nil {
			t.Fatalf("ListIssuesPage: %v", err)
		}
		if p.RateLimitRemaining != 77 {
			t.Errorf("RateLimitRemaining = %d, want 77", p.RateLimitRemaining)
		}
	})
	t.Run("absent header -> -1 sentinel", func(t *testing.T) {
		rt := &scriptedRoundTripper{responses: []scriptedResponse{
			{status: 200, body: "[]"},
		}}
		g := newTestGitHub(rt, &fakeTokenSource{token: "t"})
		p, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 1, "")
		if err != nil {
			t.Fatalf("ListIssuesPage: %v", err)
		}
		if p.RateLimitRemaining != -1 {
			t.Errorf("RateLimitRemaining = %d, want -1 (absent sentinel)", p.RateLimitRemaining)
		}
	})
}

// --- item 4: HasNext, per_page/page on the wire, ListIssues two-page walk ----

func TestPaginationAndWalk(t *testing.T) {
	t.Run("HasNext reflects Link rel=next", func(t *testing.T) {
		rt := &scriptedRoundTripper{responses: []scriptedResponse{
			{status: 200, body: "[]", headers: map[string]string{
				"Link": `<https://api.github.com/repos/org/repo/issues?page=2>; rel="next"`,
			}},
			{status: 200, body: "[]"},
		}}
		g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

		p1, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 1, "")
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		if !p1.HasNext {
			t.Error("expected HasNext true when Link rel=next present")
		}
		q := rt.requests[0].URL.Query()
		if q.Get("per_page") != "100" {
			t.Errorf("per_page = %q, want 100", q.Get("per_page"))
		}
		if q.Get("page") != "1" {
			t.Errorf("page = %q, want 1", q.Get("page"))
		}

		p2, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 2, "")
		if err != nil {
			t.Fatalf("page 2: %v", err)
		}
		if p2.HasNext {
			t.Error("expected HasNext false when no Link rel=next")
		}
	})

	t.Run("ListIssues concatenates a two-page walk in order", func(t *testing.T) {
		page1 := `[{"number":1,"user":{"login":"a"}},{"number":2,"user":{"login":"a"}}]`
		page2 := `[{"number":3,"user":{"login":"a"}}]`
		rt := &scriptedRoundTripper{responses: []scriptedResponse{
			{status: 200, body: page1, headers: map[string]string{
				"Link": `<https://api.github.com/repos/org/repo/issues?page=2>; rel="next"`,
			}},
			{status: 200, body: page2},
		}}
		g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

		issues, err := g.ListIssues(context.Background(), "org/repo", IssueFilter{})
		if err != nil {
			t.Fatalf("ListIssues: %v", err)
		}
		if len(issues) != 3 {
			t.Fatalf("got %d issues, want 3", len(issues))
		}
		if issues[0].Number != 1 || issues[1].Number != 2 || issues[2].Number != 3 {
			t.Errorf("out of order: %d,%d,%d", issues[0].Number, issues[1].Number, issues[2].Number)
		}
		if rt.requests[1].URL.Query().Get("page") != "2" {
			t.Errorf("second walk page = %q, want 2", rt.requests[1].URL.Query().Get("page"))
		}
	})
}

// --- item 5: filter -> query mapping ----------------------------------------

func TestFilterMapping(t *testing.T) {
	t.Run("empty state -> state=all", func(t *testing.T) {
		rt := &scriptedRoundTripper{responses: []scriptedResponse{{status: 200, body: "[]"}}}
		g := newTestGitHub(rt, &fakeTokenSource{token: "t"})
		if _, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 1, ""); err != nil {
			t.Fatalf("ListIssuesPage: %v", err)
		}
		if got := rt.requests[0].URL.Query().Get("state"); got != "all" {
			t.Errorf("state = %q, want all", got)
		}
	})

	t.Run("labels join and explicit state pass through", func(t *testing.T) {
		rt := &scriptedRoundTripper{responses: []scriptedResponse{{status: 200, body: "[]"}}}
		g := newTestGitHub(rt, &fakeTokenSource{token: "t"})
		f := IssueFilter{State: "open", Labels: []string{"bug", "p1"}}
		if _, err := g.ListIssuesPage(context.Background(), "org/repo", f, 1, ""); err != nil {
			t.Fatalf("ListIssuesPage: %v", err)
		}
		q := rt.requests[0].URL.Query()
		if q.Get("state") != "open" {
			t.Errorf("state = %q, want open", q.Get("state"))
		}
		if q.Get("labels") != "bug,p1" {
			t.Errorf("labels = %q, want bug,p1", q.Get("labels"))
		}
	})
}

// --- item 6: pull_request-keyed row dropped ---------------------------------

func TestPullRequestRowDropped(t *testing.T) {
	body := `[
		{"number":1,"user":{"login":"a"}},
		{"number":2,"user":{"login":"a"},"pull_request":{"url":"https://x"}}
	]`
	rt := &scriptedRoundTripper{responses: []scriptedResponse{{status: 200, body: body}}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	page, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 1, "")
	if err != nil {
		t.Fatalf("ListIssuesPage: %v", err)
	}
	if len(page.Issues) != 1 || page.Issues[0].Number != 1 {
		t.Fatalf("expected only issue #1, got %+v", page.Issues)
	}
}

// --- item 7: StatusError mapping + Invalidate on bad-creds 403 --------------

func TestErrorMappingAndInvalidate(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		headers     map[string]string
		wantInvalid int
	}{
		{name: "404", status: 404, body: `{"message":"Not Found"}`, wantInvalid: 0},
		{name: "500", status: 500, body: `{"message":"boom"}`, wantInvalid: 0},
		{
			name:        "403 bad-creds (no rate headers)",
			status:      403,
			body:        `{"message":"Bad credentials"}`,
			wantInvalid: 1,
		},
		{
			name:        "401 bad-creds",
			status:      401,
			body:        `{"message":"Bad credentials"}`,
			wantInvalid: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &scriptedRoundTripper{responses: []scriptedResponse{
				{status: tc.status, body: tc.body, headers: tc.headers},
			}}
			ts := &fakeTokenSource{token: "t"}
			g := newTestGitHub(rt, ts)

			_, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 1, "")
			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want *StatusError", err)
			}
			if se.Status != tc.status {
				t.Errorf("Status = %d, want %d", se.Status, tc.status)
			}
			wantMsg := map[int]string{401: "Bad credentials", 404: "Not Found", 500: "boom", 403: "Bad credentials"}[tc.status]
			if se.Message != wantMsg {
				t.Errorf("Message = %q, want %q", se.Message, wantMsg)
			}
			if ts.invalidated != tc.wantInvalid {
				t.Errorf("Invalidate called %d times, want %d", ts.invalidated, tc.wantInvalid)
			}
		})
	}
}

// --- item 8: bearer auth + token-error propagation --------------------------

func TestBearerAuthAndTokenError(t *testing.T) {
	t.Run("token lands as Authorization: Bearer", func(t *testing.T) {
		rt := &scriptedRoundTripper{responses: []scriptedResponse{{status: 200, body: "[]"}}}
		g := newTestGitHub(rt, &fakeTokenSource{token: "sekret"})
		if _, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 1, ""); err != nil {
			t.Fatalf("ListIssuesPage: %v", err)
		}
		if got := rt.requests[0].Header.Get("Authorization"); got != "Bearer sekret" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer sekret")
		}
	})

	t.Run("token error propagates without a request", func(t *testing.T) {
		rt := &scriptedRoundTripper{}
		tokErr := errors.New("resolve failed")
		g := newTestGitHub(rt, &fakeTokenSource{err: tokErr})
		_, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 1, "")
		if !errors.Is(err, tokErr) {
			t.Fatalf("err = %v, want token error", err)
		}
		if rt.calls != 0 {
			t.Errorf("issued a request despite token error: calls = %d", rt.calls)
		}
	})
}

// --- item 9: 403/429 rate-limit -> budget path, no Invalidate ---------------

func TestRateLimit403And429(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		headers map[string]string
	}{
		{name: "403 with retry-after", status: 403, headers: map[string]string{"Retry-After": "60"}},
		{name: "429 with retry-after", status: 429, headers: map[string]string{"Retry-After": "60"}},
		{name: "403 zeroed remaining", status: 403, headers: map[string]string{"X-RateLimit-Remaining": "0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &scriptedRoundTripper{responses: []scriptedResponse{
				{status: tc.status, body: `{"message":"rate limited"}`, headers: tc.headers},
			}}
			ts := &fakeTokenSource{token: "t"}
			g := newTestGitHub(rt, ts)

			// Response 1 is the rate-limit signal.
			_, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 1, "")
			if !errors.Is(err, ErrBudgetExhausted) {
				t.Fatalf("response err = %v, want ErrBudgetExhausted", err)
			}
			if ts.invalidated != 0 {
				t.Errorf("rate-limit 403/429 must NOT Invalidate; got %d", ts.invalidated)
			}
			// Next call fails fast without issuing a request.
			_, err = g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, 2, "")
			if !errors.Is(err, ErrBudgetExhausted) {
				t.Fatalf("next-call err = %v, want ErrBudgetExhausted", err)
			}
			if rt.calls != 1 {
				t.Errorf("gate issued a request: calls = %d, want 1", rt.calls)
			}
		})
	}
}

// --- item 10: absent/malformed rate headers do not wedge the gate -----------

func TestMalformedRateHeadersDoNotWedge(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: "[]"}, // absent headers
		{status: 200, body: "[]", headers: map[string]string{"X-RateLimit-Remaining": "not-a-number"}},
		{status: 200, body: "[]"},
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	for i := 1; i <= 3; i++ {
		if _, err := g.ListIssuesPage(context.Background(), "org/repo", IssueFilter{}, i, ""); err != nil {
			t.Fatalf("call %d unexpectedly gated/failed: %v", i, err)
		}
	}
	if rt.calls != 3 {
		t.Errorf("gate wedged on unknown budget: calls = %d, want 3", rt.calls)
	}
}

// --- item 11: ctx cancellation mid-walk -------------------------------------

func TestListIssuesContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: `[{"number":1,"user":{"login":"a"}}]`, headers: map[string]string{
			"Link": `<https://api.github.com/repos/org/repo/issues?page=2>; rel="next"`,
		}},
		{status: 200, body: `[{"number":2,"user":{"login":"a"}}]`},
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	// Cancel after the first request is recorded via a transport wrapper.
	g.client = &http.Client{Transport: cancelAfterFirst{inner: rt, cancel: cancel}}

	_, err := g.ListIssues(ctx, "org/repo", IssueFilter{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if rt.calls != 1 {
		t.Errorf("walk continued past cancellation: calls = %d, want 1", rt.calls)
	}
}

// cancelAfterFirst cancels the context immediately after serving the first
// response, so the ListIssues walk observes ctx.Err() before fetching page 2.
type cancelAfterFirst struct {
	inner  *scriptedRoundTripper
	cancel context.CancelFunc
}

func (c cancelAfterFirst) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.inner.RoundTrip(req)
	c.cancel()
	return resp, err
}
