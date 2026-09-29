package forge

// Unit tests for the GitHub GraphQL leg (github_graphql.go): paging, error
// branches, the per-resource rate gate, and the author mapping. Response bodies
// are built by marshaling the wire structs; TestGetPullRequestHappy and the
// golden fixture keep literal JSON, so a wrong json tag still fails there.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// headSHA is the REST head SHA of the scripted PRs; a contexts page for any
// other oid is a moved head.
const headSHA = "s7"

// noRollupGraphQL is a Checks GraphQL leg (threads excluded) whose last commit,
// head sha, has no status-check rollup, so nothing is required.
func noRollupGraphQL(sha string) string {
	return `{"data":{"repository":{"pullRequest":{"commits":{"nodes":[{"commit":{"oid":"` + sha + `","statusCheckRollup":null}}]}}}}}`
}

// emptyPullGraphQL is a GetPullRequest GraphQL leg with no threads and no
// status-check rollup on the last commit, head sha.
func emptyPullGraphQL(sha string) string {
	return `{"data":{"repository":{"pullRequest":{
	"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]},
	"commits":{"nodes":[{"commit":{"oid":"` + sha + `","statusCheckRollup":null}}]}}}}}`
}

// gqlBody marshals a GraphQL data value into a response body.
func gqlBody(t *testing.T, data any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		t.Fatalf("marshal graphql body: %v", err)
	}
	return string(b)
}

// pullPage builds one pullReadQuery page. A nil threads or contexts leaves that
// connection out, as @include(if: false) does; a nil rollup is a commit with no checks.
func pullPage(t *testing.T, threads *ghGQLThreads, commits *ghGQLPullCommits) string {
	t.Helper()
	return gqlBody(t, ghGQLPullData{Repository: &ghGQLRepo{PullRequest: &ghGQLPull{ReviewThreads: threads, Commits: commits}}})
}

// threadsConn is one page of review threads.
func threadsConn(next bool, cursor string, nodes ...ghGQLThread) *ghGQLThreads {
	return &ghGQLThreads{PageInfo: ghPageInfo{HasNextPage: next, EndCursor: cursor}, Nodes: nodes}
}

// thread is one review thread whose comments all fit on the first page.
func thread(id, path string, resolved bool, comments ...ghGQLComment) ghGQLThread {
	return ghGQLThread{ID: id, Path: path, IsResolved: resolved, Comments: ghGQLComments{Nodes: comments}}
}

// comment is one thread comment by login with GraphQL __typename kind.
func comment(login, kind, body string) ghGQLComment {
	return ghGQLComment{Author: &ghGQLActor{Login: login, Typename: kind}, Body: body}
}

// rollup is one page of status contexts on the PR's last commit, oid.
func rollup(oid string, next bool, cursor string, nodes ...ghGQLContext) *ghGQLPullCommits {
	var n ghGQLCommitNode
	n.Commit.OID = oid
	n.Commit.StatusCheckRollup = &ghGQLRollup{Contexts: ghGQLContexts{PageInfo: ghPageInfo{HasNextPage: next, EndCursor: cursor}, Nodes: nodes}}
	return &ghGQLPullCommits{Nodes: []ghGQLCommitNode{n}}
}

// checkRun is a CheckRun context; statusContext is a legacy StatusContext.
func checkRun(name string, required bool) ghGQLContext {
	return ghGQLContext{Typename: "CheckRun", Name: name, IsRequired: required}
}

func statusContext(name string, required bool) ghGQLContext {
	return ghGQLContext{Typename: "StatusContext", Context: name, IsRequired: required}
}

// pullRESTLegs scripts the four REST legs of GetPullRequest with the given
// check runs and no reviews, ahead of the GraphQL responses a test appends.
func pullRESTLegs(checkRuns string, gql ...scriptedResponse) []scriptedResponse {
	return append([]scriptedResponse{
		{status: 200, body: `{"number":7,"state":"open","head":{"ref":"f","sha":"` + headSHA + `"},"base":{"ref":"main"},"user":{"login":"a"}}`},
		{status: 200, body: `[]`},
		{status: 200, body: `{"check_runs": [` + checkRuns + `]}`},
		{status: 200, body: `{"statuses": []}`},
	}, gql...)
}

// ok200 is a scripted 200 response.
func ok200(body string) scriptedResponse { return scriptedResponse{status: 200, body: body} }

// GetPullRequest walks every review-thread page: page 2 is requested with the
// page-1 endCursor and asks only for threads (the contexts finished on page 1),
// and both pages' threads are returned in order.
func TestGetPullRequestThreadPagination(t *testing.T) {
	rt := &scriptedRoundTripper{responses: pullRESTLegs("",
		ok200(pullPage(t, threadsConn(true, "CUR1", thread("T1", "a.go", false, comment("x", "User", "one"))), rollup(headSHA, false, "c"))),
		ok200(pullPage(t, threadsConn(false, "CUR2", thread("T2", "b.go", true, comment("y", "User", "two"))), nil)),
	)}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	got, err := g.GetPullRequest(context.Background(), "org/repo", 7)
	if err != nil {
		t.Fatalf("GetPullRequest: %v", err)
	}
	if rt.calls != 6 {
		t.Fatalf("calls = %d, want 6 (4 REST + 2 GraphQL pages)", rt.calls)
	}
	if b := readReqBody(t, rt.requests[4]); !strings.Contains(b, `"threadsAfter":null`) {
		t.Errorf("page 1 must start with a null cursor: %s", b)
	}
	b2 := readReqBody(t, rt.requests[5])
	for _, want := range []string{`"threadsAfter":"CUR1"`, `"contexts":false`, `"threads":true`} {
		if !strings.Contains(b2, want) {
			t.Errorf("page 2 body missing %s: %s", want, b2)
		}
	}
	want := []ReviewThread{
		{Path: "a.go", Comments: []ThreadComment{{Author: "x", Body: "one"}}},
		{Path: "b.go", Resolved: true, Comments: []ThreadComment{{Author: "y", Body: "two"}}},
	}
	if !reflect.DeepEqual(got.Threads, want) {
		t.Errorf("Threads = %+v, want %+v", got.Threads, want)
	}
}

// Threads and contexts both page: page 2 carries both cursors, page 3 carries
// only the one still paging, and a required context on a later page still marks
// its check.
func TestGetPullRequestThreadsAndContextsPageTogether(t *testing.T) {
	runs := `{"name":"build","status":"completed","conclusion":"success","html_url":""},
		{"name":"rollup","status":"completed","conclusion":"success","html_url":""}`
	rt := &scriptedRoundTripper{responses: pullRESTLegs(runs,
		ok200(pullPage(t, threadsConn(true, "T-1", thread("A", "a.go", false, comment("x", "User", "1"))), rollup(headSHA, true, "C-1", checkRun("build", false)))),
		ok200(pullPage(t, threadsConn(true, "T-2", thread("B", "b.go", false, comment("x", "User", "2"))), rollup(headSHA, false, "C-2", checkRun("rollup", true)))),
		ok200(pullPage(t, threadsConn(false, "T-3", thread("C", "c.go", false, comment("x", "User", "3"))), nil)),
	)}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	got, err := g.GetPullRequest(context.Background(), "org/repo", 7)
	if err != nil {
		t.Fatalf("GetPullRequest: %v", err)
	}
	b2 := readReqBody(t, rt.requests[5])
	for _, want := range []string{`"threadsAfter":"T-1"`, `"contextsAfter":"C-1"`, `"threads":true`, `"contexts":true`} {
		if !strings.Contains(b2, want) {
			t.Errorf("page 2 body missing %s: %s", want, b2)
		}
	}
	b3 := readReqBody(t, rt.requests[6])
	for _, want := range []string{`"threadsAfter":"T-2"`, `"contexts":false`} {
		if !strings.Contains(b3, want) {
			t.Errorf("page 3 body missing %s: %s", want, b3)
		}
	}
	if len(got.Threads) != 3 || got.Threads[2].Path != "c.go" {
		t.Errorf("Threads = %+v, want 3 threads ending in c.go", got.Threads)
	}
	if got.Checks.Checks[0].Required || !got.Checks.Checks[1].Required {
		t.Errorf("Checks = %+v, want only rollup required", got.Checks.Checks)
	}
}

// Checks walks every page of status contexts: a required context on page 2
// still marks its check, and page 2 is requested with the page-1 cursor.
func TestChecksRequiredContextPagination(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		ok200(`{"number":9,"head":{"sha":"` + headSHA + `"},"base":{"ref":"main"},"user":{"login":"a"}}`),
		ok200(`{"check_runs": [
			{"name":"build","status":"completed","conclusion":"success","html_url":""},
			{"name":"rollup","status":"completed","conclusion":"success","html_url":""}
		]}`),
		ok200(`{"statuses": []}`),
		ok200(pullPage(t, nil, rollup(headSHA, true, "CX1", checkRun("build", false)))),
		ok200(pullPage(t, nil, rollup(headSHA, false, "CX2", checkRun("rollup", true)))),
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	got, err := g.Checks(context.Background(), "org/repo", 9)
	if err != nil {
		t.Fatalf("Checks: %v", err)
	}
	if b := readReqBody(t, rt.requests[4]); !strings.Contains(b, `"contextsAfter":"CX1"`) {
		t.Errorf("page 2 body = %s, want contextsAfter CX1", b)
	}
	if got.Checks[0].Required || !got.Checks[1].Required {
		t.Errorf("Checks = %+v, want only rollup required", got.Checks)
	}
}

// A required StatusContext marks the legacy status of the same name, and a PR
// whose commits list is empty has nothing required.
func TestChecksRequiredContextShapes(t *testing.T) {
	detail := ok200(`{"number":9,"head":{"sha":"` + headSHA + `"},"base":{"ref":"main"},"user":{"login":"a"}}`)
	status := ok200(`{"statuses": [{"context":"legacy","state":"success","target_url":""}]}`)

	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		detail, ok200(`{"check_runs": []}`), status,
		ok200(pullPage(t, nil, rollup(headSHA, false, "c", statusContext("legacy", true)))),
	}}
	got, err := newTestGitHub(rt, &fakeTokenSource{token: "t"}).Checks(context.Background(), "org/repo", 9)
	if err != nil {
		t.Fatalf("Checks: %v", err)
	}
	if !got.Checks[0].Required {
		t.Errorf("legacy status not marked required: %+v", got.Checks)
	}

	rt = &scriptedRoundTripper{responses: []scriptedResponse{
		detail, ok200(`{"check_runs": []}`), status,
		ok200(pullPage(t, nil, &ghGQLPullCommits{})),
	}}
	got, err = newTestGitHub(rt, &fakeTokenSource{token: "t"}).Checks(context.Background(), "org/repo", 9)
	if err != nil {
		t.Fatalf("Checks (no commits): %v", err)
	}
	if got.Checks[0].Required {
		t.Errorf("empty commits list marked a check required: %+v", got.Checks)
	}
}

// A thread with more than one page of comments is completed through the
// thread-node query, so a long thread is never truncated.
func TestGetPullRequestThreadCommentPagination(t *testing.T) {
	long := thread("T1", "a.go", false, comment("x", "User", "first"))
	long.Comments.PageInfo = ghPageInfo{HasNextPage: true, EndCursor: "C1"}
	more := gqlBody(t, map[string]any{"node": map[string]any{"comments": ghGQLComments{
		PageInfo: ghPageInfo{EndCursor: "C2"},
		Nodes:    []ghGQLComment{comment("y", ghTypeBot, "second")},
	}}})
	rt := &scriptedRoundTripper{responses: pullRESTLegs("",
		ok200(pullPage(t, threadsConn(false, "t", long), rollup(headSHA, false, "c"))),
		ok200(more),
	)}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	got, err := g.GetPullRequest(context.Background(), "org/repo", 7)
	if err != nil {
		t.Fatalf("GetPullRequest: %v", err)
	}
	if b := readReqBody(t, rt.requests[5]); !strings.Contains(b, `"id":"T1"`) || !strings.Contains(b, `"after":"C1"`) {
		t.Errorf("comment continuation body = %s, want id T1 after C1", b)
	}
	want := []ThreadComment{{Author: "x", Body: "first"}, {Author: "y[bot]", IsBot: true, Body: "second"}}
	if len(got.Threads) != 1 || !slices.Equal(got.Threads[0].Comments, want) {
		t.Errorf("Threads = %+v, want one thread with %+v", got.Threads, want)
	}
}

// Author mapping: GraphQL drops the "[bot]" suffix REST logins carry, so a Bot
// author gets it back; a null author (a deleted account) is an empty Author.
func TestToThreadComment(t *testing.T) {
	cases := []struct {
		name string
		in   ghGQLComment
		want ThreadComment
	}{
		{"user", comment("carol", "User", "b"), ThreadComment{Author: "carol", Body: "b"}},
		{"bot gets the REST suffix", comment("dependabot", ghTypeBot, "b"), ThreadComment{Author: "dependabot[bot]", IsBot: true, Body: "b"}},
		{"null author", ghGQLComment{Body: "ghost"}, ThreadComment{Body: "ghost"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toThreadComment(tc.in); got != tc.want {
				t.Errorf("toThreadComment = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Every malformed or dead-end response is an error, never a silent empty or
// truncated result.
func TestPullGraphQLErrorBranches(t *testing.T) {
	longThread := thread("T1", "a.go", false, comment("x", "User", "1"))
	longThread.Comments.PageInfo = ghPageInfo{HasNextPage: true, EndCursor: "C1"}
	cases := []struct {
		name string
		gql  []scriptedResponse
		want string
	}{
		{"errors array", []scriptedResponse{ok200(`{"data":null,"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a PullRequest with the number of 7."}]}`)}, "Could not resolve to a PullRequest"},
		{"null repository", []scriptedResponse{ok200(`{"data":{"repository":null}}`)}, "repository"},
		{"null pull request", []scriptedResponse{ok200(`{"data":{"repository":{"pullRequest":null}}}`)}, "pull request"},
		{"no threads connection", []scriptedResponse{ok200(pullPage(t, nil, rollup(headSHA, false, "c")))}, "review threads connection"},
		{"no commits connection", []scriptedResponse{ok200(pullPage(t, threadsConn(false, "t"), nil))}, "commits connection"},
		{"next page without a cursor", []scriptedResponse{ok200(pullPage(t, threadsConn(true, ""), rollup(headSHA, false, "c")))}, "without an endCursor"},
		{"repeated cursor", []scriptedResponse{
			ok200(pullPage(t, threadsConn(true, "SAME"), rollup(headSHA, false, "c"))),
			ok200(pullPage(t, threadsConn(true, "SAME"), nil)),
		}, "repeats the previous cursor"},
		{"null thread node", []scriptedResponse{
			ok200(pullPage(t, threadsConn(false, "t", longThread), rollup(headSHA, false, "c"))),
			ok200(`{"data":{"node":null}}`),
		}, "review thread"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &scriptedRoundTripper{responses: pullRESTLegs("", tc.gql...)}
			_, err := newTestGitHub(rt, &fakeTokenSource{token: "t"}).GetPullRequest(context.Background(), "org/repo", 7)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
			if rt.calls != 4+len(tc.gql) {
				t.Errorf("calls = %d, want %d", rt.calls, 4+len(tc.gql))
			}
		})
	}
}

// The contexts must describe the commit the REST checks were read at. A moved
// head, on page 1 or mid-walk, is errHeadMoved naming both SHAs, never a
// Required set joined from another commit. A null rollup is checked too, so a
// new head with no checks cannot clear the old head's required flags.
func TestPullGraphQLHeadMoved(t *testing.T) {
	var moved ghGQLCommitNode
	moved.Commit.OID = "newhead"
	cases := []struct {
		name string
		gql  []scriptedResponse
	}{
		{"page 1 at another commit", []scriptedResponse{
			ok200(pullPage(t, threadsConn(false, "t"), rollup("newhead", false, "c", checkRun("build", true)))),
		}},
		{"page 1 null rollup at another commit", []scriptedResponse{
			ok200(pullPage(t, threadsConn(false, "t"), &ghGQLPullCommits{Nodes: []ghGQLCommitNode{moved}})),
		}},
		{"head changes on page 2", []scriptedResponse{
			ok200(pullPage(t, threadsConn(false, "t"), rollup(headSHA, true, "C1", checkRun("build", false)))),
			ok200(pullPage(t, nil, rollup("newhead", false, "C2", checkRun("rollup", true)))),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &scriptedRoundTripper{responses: pullRESTLegs("", tc.gql...)}
			_, err := newTestGitHub(rt, &fakeTokenSource{token: "t"}).GetPullRequest(context.Background(), "org/repo", 7)
			if !errors.Is(err, errHeadMoved) {
				t.Fatalf("err = %v, want errHeadMoved", err)
			}
			if msg := err.Error(); !strings.Contains(msg, headSHA) || !strings.Contains(msg, "newhead") {
				t.Errorf("err = %q, want both SHAs", msg)
			}
			if rt.calls != 4+len(tc.gql) {
				t.Errorf("calls = %d, want %d (no retry, no fallback)", rt.calls, 4+len(tc.gql))
			}
		})
	}
}

// nextPage refuses to loop: no cursor, or the cursor that fetched this page.
func TestNextPage(t *testing.T) {
	cases := []struct {
		name     string
		p        ghPageInfo
		prev     any
		wantMore bool
		wantErr  bool
	}{
		{"last page", ghPageInfo{EndCursor: "A"}, nil, false, false},
		{"first to second", ghPageInfo{HasNextPage: true, EndCursor: "A"}, nil, true, false},
		{"advancing", ghPageInfo{HasNextPage: true, EndCursor: "B"}, "A", true, false},
		{"no cursor", ghPageInfo{HasNextPage: true}, nil, false, true},
		{"repeated cursor", ghPageInfo{HasNextPage: true, EndCursor: "A"}, "A", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			more, _, err := nextPage(tc.p, tc.prev)
			if more != tc.wantMore || (err != nil) != tc.wantErr {
				t.Errorf("nextPage = (%v, %v), want more=%v err=%v", more, err, tc.wantMore, tc.wantErr)
			}
		})
	}
}

// A GraphQL RATE_LIMITED error on HTTP 200 is the budget skip: a
// *RateLimitError carrying the reset hint, with the GraphQL gate armed.
func TestGraphQLRateLimitedOn200(t *testing.T) {
	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	reset := strconv.FormatInt(base.Add(40*time.Second).Unix(), 10)
	rt := &scriptedRoundTripper{responses: pullRESTLegs("", scriptedResponse{
		status:  200,
		body:    `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`,
		headers: map[string]string{"X-Ratelimit-Resource": "graphql", "X-Ratelimit-Remaining": "0", "X-Ratelimit-Reset": reset},
	})}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})
	g.now = func() time.Time { return base }

	_, err := g.GetPullRequest(context.Background(), "org/repo", 7)
	rle, ok := errors.AsType[*RateLimitError](err)
	if !ok || !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want a *RateLimitError", err)
	}
	if rle.RetryAfter != 40*time.Second {
		t.Errorf("RetryAfter = %v, want 40s", rle.RetryAfter)
	}
	if _, blocked := g.gateBlocked(resourceGraphQL); !blocked {
		t.Error("GraphQL gate not armed after RATE_LIMITED")
	}
	if _, blocked := g.gateBlocked(resourceCore); blocked {
		t.Error("core gate armed by a GraphQL rate limit")
	}
}

// With no usable reset header, RATE_LIMITED still arms the GraphQL gate (the
// bounded default skip), so the next GraphQL call fails fast.
func TestGraphQLRateLimitedNoResetArmsDefault(t *testing.T) {
	rt := &scriptedRoundTripper{responses: pullRESTLegs("", ok200(`{"errors":[{"type":"RATE_LIMITED","message":"slow down"}]}`))}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	if _, err := g.GetPullRequest(context.Background(), "org/repo", 7); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if _, blocked := g.gateBlocked(resourceGraphQL); !blocked {
		t.Error("GraphQL gate not armed with the default skip")
	}
}

// The REST core and GraphQL buckets have separate gates: draining one does not
// block the other, and each still blocks itself.
func TestRateGatePerResource(t *testing.T) {
	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	reset := strconv.FormatInt(base.Add(30*time.Second).Unix(), 10)

	t.Run("graphql drained does not block REST", func(t *testing.T) {
		rt := &scriptedRoundTripper{responses: []scriptedResponse{
			{status: 200, body: `{"data":{}}`, headers: map[string]string{"X-Ratelimit-Resource": "graphql", "X-Ratelimit-Remaining": "0", "X-Ratelimit-Reset": reset}},
			ok200(`{"number":7,"user":{"login":"a"}}`),
		}}
		g := newTestGitHub(rt, &fakeTokenSource{token: "t"})
		g.now = func() time.Time { return base }

		if _, err := graphQL[struct{}](context.Background(), g, "query{viewer{login}}", nil); err != nil {
			t.Fatalf("graphQL: %v", err)
		}
		if _, err := g.GetIssue(context.Background(), "org/repo", 7); err != nil {
			t.Fatalf("REST GET after a drained GraphQL bucket: %v", err)
		}
		if _, err := graphQL[struct{}](context.Background(), g, "query{viewer{login}}", nil); !errors.Is(err, ErrBudgetExhausted) {
			t.Errorf("second graphQL err = %v, want the GraphQL gate to block itself", err)
		}
		if rt.calls != 2 {
			t.Errorf("calls = %d, want 2 (the blocked GraphQL call sends nothing)", rt.calls)
		}
	})

	t.Run("REST rate-limit 403 does not block GraphQL", func(t *testing.T) {
		rt := &scriptedRoundTripper{responses: []scriptedResponse{
			{status: 403, body: `{"message":"rate limited"}`, headers: map[string]string{"Retry-After": "60"}},
			ok200(`{"data":{}}`),
		}}
		g := newTestGitHub(rt, &fakeTokenSource{token: "t"})
		g.now = func() time.Time { return base }

		if _, err := g.GetIssue(context.Background(), "org/repo", 7); !errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("GetIssue err = %v, want ErrBudgetExhausted", err)
		}
		if _, err := graphQL[struct{}](context.Background(), g, "query{viewer{login}}", nil); err != nil {
			t.Fatalf("graphQL after a REST rate limit: %v", err)
		}
		if _, err := g.GetIssue(context.Background(), "org/repo", 7); !errors.Is(err, ErrBudgetExhausted) {
			t.Errorf("second GetIssue err = %v, want the core gate to block itself", err)
		}
		if rt.calls != 2 {
			t.Errorf("calls = %d, want 2 (the blocked REST call sends nothing)", rt.calls)
		}
	})

	t.Run("resource header routes a GraphQL POST's 403 to its own gate", func(t *testing.T) {
		rt := &scriptedRoundTripper{responses: []scriptedResponse{
			{status: 403, body: `{"message":"rate limited"}`, headers: map[string]string{"Retry-After": "60", "X-Ratelimit-Resource": "graphql"}},
			ok200(`{"number":7,"user":{"login":"a"}}`),
		}}
		g := newTestGitHub(rt, &fakeTokenSource{token: "t"})
		g.now = func() time.Time { return base }

		if _, err := graphQL[struct{}](context.Background(), g, "query{viewer{login}}", nil); !errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("graphQL err = %v, want ErrBudgetExhausted", err)
		}
		if _, err := g.GetIssue(context.Background(), "org/repo", 7); err != nil {
			t.Fatalf("REST GET after a GraphQL 403: %v", err)
		}
	})
}

// A repo that is not exactly owner/name is rejected before any request.
func TestGetPullRequestMalformedRepo(t *testing.T) {
	for _, repo := range []string{"repo", "/repo", "org/", "org/repo/extra"} {
		rt := &scriptedRoundTripper{}
		g := newTestGitHub(rt, &fakeTokenSource{token: "t"})
		if _, err := g.GetPullRequest(context.Background(), repo, 1); !errors.Is(err, errMalformedRepo) {
			t.Errorf("repo %q: err = %v, want errMalformedRepo", repo, err)
		}
		if _, err := g.Checks(context.Background(), repo, 1); !errors.Is(err, errMalformedRepo) {
			t.Errorf("Checks repo %q: err = %v, want errMalformedRepo", repo, err)
		}
		if rt.calls != 0 {
			t.Errorf("repo %q: calls = %d, want 0", repo, rt.calls)
		}
	}
}

// The GHES GraphQL endpoint is /api/graphql on the configured host.
func TestGraphQLURLGHES(t *testing.T) {
	g := NewGitHub(GitHubConfig{Host: "ghe.example.com", Token: &fakeTokenSource{token: "t"}, Client: &http.Client{}})
	if got := g.graphQLURL(); got != "https://ghe.example.com/api/graphql" {
		t.Errorf("graphQLURL = %s", got)
	}
}
