package forge

// Unit tests for the NotifyReader conditional-read arm (RIG-2732 T5), driven by
// the stubbed http.RoundTripper (no network): the If-None-Match/304 short-circuit
// on every GitHub arm, the ListNewArtifacts PR-interleave filter + endpoint
// split, the >1-page container walk (never truncated to page 1), and the Linear
// arms' no-ETag / ErrUnsupported behavior. context.Background() here is the test
// root — the sanctioned F-ttsr exemption (mirrors github_test.go).

import (
	"context"
	"errors"
	"net/http"
	"testing"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

const (
	kindIssueR = compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE
	kindPRR    = compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST
)

// --- GitHub: conditional 304 short-circuits ---------------------------------

func TestGetIssueConditional304(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: http.StatusNotModified, headers: map[string]string{"ETag": `"i0"`}},
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	res, err := g.GetIssueConditional(context.Background(), "org/repo", 7, `"i0"`)
	if err != nil {
		t.Fatalf("GetIssueConditional: %v", err)
	}
	if !res.NotModified {
		t.Error("expected NotModified on a 304")
	}
	if got := rt.requests[0].Header.Get("If-None-Match"); got != `"i0"` {
		t.Errorf("If-None-Match = %q, want %q", got, `"i0"`)
	}
}

func TestGetIssueConditional200(t *testing.T) {
	body := `{"number":7,"title":"a bug","body":"b","state":"open","html_url":"u","user":{"login":"octocat"}}`
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: body, headers: map[string]string{"ETag": `"i1"`}},
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	res, err := g.GetIssueConditional(context.Background(), "org/repo", 7, "")
	if err != nil {
		t.Fatalf("GetIssueConditional: %v", err)
	}
	if res.NotModified {
		t.Fatal("unexpected NotModified on a 200")
	}
	if res.V.Number != 7 || res.V.State != "open" || res.ETag != `"i1"` {
		t.Errorf("decoded = %+v etag=%q, want #7/open/\"i1\"", res.V, res.ETag)
	}
}

func TestGetPullRequestConditional304(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: http.StatusNotModified, headers: map[string]string{"ETag": `"p0"`}},
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	res, err := g.GetPullRequestConditional(context.Background(), "org/repo", 42, `"p0"`)
	if err != nil {
		t.Fatalf("GetPullRequestConditional: %v", err)
	}
	if !res.NotModified {
		t.Error("expected NotModified on a 304")
	}
}

func TestGetPullRequestConditional200FoldsMerged(t *testing.T) {
	body := `{"number":42,"state":"closed","merged":true,"html_url":"u","head":{"ref":"f","sha":"abc"},"base":{"ref":"main"},"user":{"login":"o"}}`
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: body, headers: map[string]string{"ETag": `"p1"`}},
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	res, err := g.GetPullRequestConditional(context.Background(), "org/repo", 42, "")
	if err != nil {
		t.Fatalf("GetPullRequestConditional: %v", err)
	}
	if res.V.State != "merged" { // closed+merged -> merged (the invariant's shared mapping)
		t.Errorf("state = %q, want merged", res.V.State)
	}
}

func TestListComments304(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: http.StatusNotModified, headers: map[string]string{"ETag": `"c0"`}},
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	res, err := g.ListComments(context.Background(), "org/repo", kindIssueR, 7, `"c0"`)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if !res.NotModified {
		t.Error("expected NotModified: a 304 means no new comments in one request")
	}
	if rt.calls != 1 {
		t.Errorf("calls = %d, want 1 (a 304 short-circuits the walk)", rt.calls)
	}
}

// TestListCommentsWalksAllPages: a page-1 miss with a rel="next" Link walks the
// remaining pages UNCONDITIONALLY and concatenates the full set, returning page
// 1's ETag.
func TestListCommentsWalksAllPages(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: `[{"id":1,"html_url":"c1","body":"one","user":{"login":"a"}}]`, headers: map[string]string{
			"ETag": `"c1"`,
			"Link": `<https://api.github.com/x?page=2>; rel="next"`,
		}},
		{status: 200, body: `[{"id":2,"html_url":"c2","body":"two","user":{"login":"b"}}]`},
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	res, err := g.ListComments(context.Background(), "org/repo", kindIssueR, 7, "")
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(res.V) != 2 {
		t.Fatalf("comments = %d, want 2 (page 1 + page 2, not truncated)", len(res.V))
	}
	if res.ETag != `"c1"` {
		t.Errorf("ETag = %q, want page-1's \"c1\"", res.ETag)
	}
	// Page 2 is fetched unconditionally (no If-None-Match).
	if _, ok := rt.requests[1].Header["If-None-Match"]; ok {
		t.Error("page 2 carried an If-None-Match; the walk must be unconditional past page 1")
	}
}

// TestChecksConditional304: a 304 on the check-runs page-1 probe short-circuits
// (no re-fold), with the head SHA passed directly (no pull-detail fetch).
func TestChecksConditional304(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: http.StatusNotModified, headers: map[string]string{"ETag": `"ck0"`}},
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	res, err := g.ChecksConditional(context.Background(), "org/repo", 42, "abc", `"ck0"`)
	if err != nil {
		t.Fatalf("ChecksConditional: %v", err)
	}
	if !res.NotModified || rt.calls != 1 {
		t.Errorf("NotModified=%v calls=%d, want true/1 (SHA supplied, 304 short-circuits)", res.NotModified, rt.calls)
	}
}

// TestChecksConditionalResolvesHeadSHA: an empty head SHA resolves it from the
// pull detail first, then folds the combined roll-up on a page-1 miss.
func TestChecksConditionalResolvesHeadSHA(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		// pull detail (unconditional getJSON) -> head SHA
		{status: 200, body: `{"number":42,"state":"open","head":{"sha":"abc"},"base":{},"user":{}}`},
		// check-runs page-1 conditional probe (miss)
		{status: 200, body: `{"check_runs":[{"name":"build","status":"completed","conclusion":"success","html_url":"b"}]}`, headers: map[string]string{"ETag": `"ck1"`}},
		// checksForSHA re-fold: check-runs full walk
		{status: 200, body: `{"check_runs":[{"name":"build","status":"completed","conclusion":"success","html_url":"b"}]}`},
		// checksForSHA re-fold: combined-status
		{status: 200, body: `{"statuses":[]}`},
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	res, err := g.ChecksConditional(context.Background(), "org/repo", 42, "", "")
	if err != nil {
		t.Fatalf("ChecksConditional: %v", err)
	}
	if res.NotModified {
		t.Fatal("unexpected NotModified on a page-1 miss")
	}
	if res.V.HeadSHA != "abc" || res.V.State != "success" || res.ETag != `"ck1"` {
		t.Errorf("roll-up = %+v etag=%q, want abc/success/\"ck1\"", res.V, res.ETag)
	}
}

// --- GitHub: ListNewArtifacts contract points --------------------------------

// TestListNewArtifactsIssueFiltersPRs: /repos/{repo}/issues interleaves PR rows
// (pull_request marker); a kind=ISSUE sweep filters them out, keeping only
// issue-shaped rows above sinceNumber.
func TestListNewArtifactsIssueFiltersPRs(t *testing.T) {
	body := `[
		{"number":45,"state":"open","html_url":"u45","pull_request":{"url":"pr"}},
		{"number":44,"state":"open","html_url":"u44"},
		{"number":43,"state":"open","html_url":"u43"}
	]`
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: body, headers: map[string]string{"ETag": `"n1"`}},
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	res, err := g.ListNewArtifacts(context.Background(), "org/repo", kindIssueR, 43, "")
	if err != nil {
		t.Fatalf("ListNewArtifacts: %v", err)
	}
	// 45 is a PR (filtered), 43 is at/below sinceNumber (excluded), only 44 kept.
	if len(res.V) != 1 || res.V[0].Number != 44 {
		t.Fatalf("kept %+v, want only issue #44 (PR #45 filtered, #43 not above since)", res.V)
	}
	if got := rt.requests[0].URL.Path; got != "/repos/org/repo/issues" {
		t.Errorf("path = %q, want the issues endpoint", got)
	}
}

// TestListNewArtifactsPRUsesPullsEndpoint: kind=PULL_REQUEST uses
// /repos/{repo}/pulls (a different endpoint), NOT the issues endpoint.
func TestListNewArtifactsPRUsesPullsEndpoint(t *testing.T) {
	body := `[{"number":42,"state":"open","html_url":"u42"}]`
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: body, headers: map[string]string{"ETag": `"n1"`}},
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	res, err := g.ListNewArtifacts(context.Background(), "org/repo", kindPRR, 40, "")
	if err != nil {
		t.Fatalf("ListNewArtifacts: %v", err)
	}
	if len(res.V) != 1 || res.V[0].Number != 42 {
		t.Fatalf("kept %+v, want PR #42", res.V)
	}
	if got := rt.requests[0].URL.Path; got != "/repos/org/repo/pulls" {
		t.Errorf("path = %q, want the pulls endpoint (not issues)", got)
	}
}

// TestListNewArtifactsWalksPastPageOne: an ETag miss on page 1 with a full page
// all above sinceNumber walks page 2 (a >1-page burst is never truncated), until
// a page's oldest number is <= sinceNumber.
func TestListNewArtifactsWalksPastPageOne(t *testing.T) {
	page1 := `[{"number":50,"state":"open","html_url":"u50"},{"number":49,"state":"open","html_url":"u49"}]`
	page2 := `[{"number":48,"state":"open","html_url":"u48"},{"number":40,"state":"open","html_url":"u40"}]`
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: page1, headers: map[string]string{"ETag": `"n1"`, "Link": `<https://api.github.com/x?page=2>; rel="next"`}},
		{status: 200, body: page2},
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	res, err := g.ListNewArtifacts(context.Background(), "org/repo", kindIssueR, 45, "")
	if err != nil {
		t.Fatalf("ListNewArtifacts: %v", err)
	}
	// 50,49,48 kept; 40 is <= sinceNumber so the walk stops on page 2.
	if len(res.V) != 3 {
		t.Fatalf("kept %d, want 3 (50,49,48 across two pages; 40 stops the walk)", len(res.V))
	}
	if rt.calls != 2 {
		t.Errorf("calls = %d, want 2 (the walk stopped once a page's oldest <= since)", rt.calls)
	}
}

// TestListNewArtifactsPage1_304: a 304 on page 1 returns NotModified without
// walking (no new artifacts since the last sweep).
func TestListNewArtifactsPage1_304(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: http.StatusNotModified, headers: map[string]string{"ETag": `"n0"`}},
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	res, err := g.ListNewArtifacts(context.Background(), "org/repo", kindIssueR, 45, `"n0"`)
	if err != nil {
		t.Fatalf("ListNewArtifacts: %v", err)
	}
	if !res.NotModified || rt.calls != 1 {
		t.Errorf("NotModified=%v calls=%d, want true/1", res.NotModified, rt.calls)
	}
}

// --- Linear arms -------------------------------------------------------------

// TestLinearReaderNoETags: a Linear issue read returns a 200-equivalent with an
// EMPTY ETag (GraphQL has no ETags — the documented backstop limitation).
func TestLinearReaderNoETags(t *testing.T) {
	body := `{"data":{"issues":{"nodes":[{"number":7,"title":"t","description":"d","url":"https://linear.app/x/SEA-7","state":{"name":"Todo","type":"unstarted"},"labels":{"nodes":[]},"creator":null,"updatedAt":""}]}}}`
	rt := &scriptedRoundTripper{responses: []scriptedResponse{{status: 200, body: body}}}
	l := newTestLinear(rt, &fakeTokenSource{token: "t"}, nil)

	res, err := l.GetIssueConditional(context.Background(), "SEA", 7, "any-prior-etag")
	if err != nil {
		t.Fatalf("GetIssueConditional: %v", err)
	}
	if res.NotModified {
		t.Error("Linear read must be a 200-equivalent, never a 304 (no ETags)")
	}
	if res.ETag != "" {
		t.Errorf("ETag = %q, want empty (GraphQL has no ETags)", res.ETag)
	}
	if res.V.Number != 7 || res.V.State != "open" {
		t.Errorf("decoded = %+v, want #7/open", res.V)
	}
}

// TestLinearReaderPRAndChecksUnsupported: the PR + checks arms return
// ErrUnsupported (Linear is issues-only, DL-051).
func TestLinearReaderPRAndChecksUnsupported(t *testing.T) {
	l := newTestLinear(&scriptedRoundTripper{}, &fakeTokenSource{token: "t"}, nil)

	if _, err := l.GetPullRequestConditional(context.Background(), "SEA", 1, ""); !errors.Is(err, ErrUnsupported) {
		t.Errorf("GetPullRequestConditional err = %v, want ErrUnsupported", err)
	}
	if _, err := l.ChecksConditional(context.Background(), "SEA", 1, "", ""); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ChecksConditional err = %v, want ErrUnsupported", err)
	}
	if _, err := l.ListComments(context.Background(), "SEA", kindPRR, 1, ""); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ListComments(PR) err = %v, want ErrUnsupported", err)
	}
	if _, err := l.ListNewArtifacts(context.Background(), "SEA", kindPRR, 0, ""); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ListNewArtifacts(PR) err = %v, want ErrUnsupported", err)
	}
}

// TestLinearListNewArtifactsCarriesProject: the container walk reads each issue's
// project id so a routed OPENED event carries ForgeEvent.Project (W2).
func TestLinearListNewArtifactsCarriesProject(t *testing.T) {
	body := `{"data":{"issues":{"nodes":[
		{"number":42,"url":"u42","state":{"type":"unstarted"},"project":{"id":"proj-A"}}
	],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`
	rt := &scriptedRoundTripper{responses: []scriptedResponse{{status: 200, body: body}}}
	l := newTestLinear(rt, &fakeTokenSource{token: "t"}, nil)

	res, err := l.ListNewArtifacts(context.Background(), "SEA", kindIssueR, 40, "")
	if err != nil {
		t.Fatalf("ListNewArtifacts: %v", err)
	}
	if len(res.V) != 1 || res.V[0].Number != 42 || res.V[0].Project != "proj-A" {
		t.Fatalf("kept %+v, want #42 with project proj-A", res.V)
	}
}
