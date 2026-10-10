package forge

// Forge reads the PR-to-issue linkage depends on: PR timestamps, closing
// references on the first GraphQL page only, and the open-PR list.

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

var (
	prCreated = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	prUpdated = time.Date(2026, 9, 2, 11, 30, 0, 0, time.UTC)
)

func TestCreatePullRequestDecodesTimestamps(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{{status: 201, body: `{"number":13,"state":"open",
		"created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-02T11:30:00Z","head":{"ref":"f"},"base":{"ref":"main"}}`}}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	got, err := g.CreatePullRequest(context.Background(), "org/repo", CreatePR{Title: "x", Body: "b", HeadRef: "f"})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	if !got.CreatedAt.Equal(prCreated) || !got.UpdatedAt.Equal(prUpdated) {
		t.Fatalf("times = %v / %v, want %v / %v", got.CreatedAt, got.UpdatedAt, prCreated, prUpdated)
	}
}

// closingRefs is one page of closingIssuesReferences.
func closingRefs(refs ...IssueRef) *ghGQLClosingRefs {
	c := &ghGQLClosingRefs{}
	for _, r := range refs {
		n := ghGQLClosingRef{Number: r.Number}
		n.Repository.NameWithOwner = r.Repo
		c.Nodes = append(c.Nodes, n)
	}
	return c
}

// GetPullRequest decodes the detail timestamps and reads closing references on
// the first GraphQL page only, not again on a thread page.
func TestGetPullRequestClosingRefsFirstPageOnly(t *testing.T) {
	refs := []IssueRef{{Repo: "org/repo", Number: 3}, {Repo: "Other/Repo", Number: 9}}
	page1 := gqlBody(t, ghGQLPullData{Repository: &ghGQLRepo{PullRequest: &ghGQLPull{
		ReviewThreads: threadsConn(true, "CUR1", thread("T1", "a.go", false, comment("x", "User", "one"))),
		Commits:       rollup(headSHA, false, "c"),
		ClosingRefs:   closingRefs(refs...),
	}}})
	page2 := pullPage(t, threadsConn(false, "CUR2"), nil)
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		ok200(`{"number":7,"state":"open","created_at":"2026-09-01T10:00:00Z","updated_at":"2026-09-02T11:30:00Z",
			"head":{"ref":"f","sha":"` + headSHA + `"},"base":{"ref":"main"},"user":{"login":"a"}}`),
		ok200(`[]`), ok200(`{"check_runs": []}`), ok200(`{"statuses": []}`),
		ok200(page1), ok200(page2),
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	got, err := g.GetPullRequest(context.Background(), "org/repo", 7)
	if err != nil {
		t.Fatalf("GetPullRequest: %v", err)
	}
	if !slices.Equal(got.ClosingRefs, refs) {
		t.Fatalf("ClosingRefs = %+v, want %+v", got.ClosingRefs, refs)
	}
	if !got.CreatedAt.Equal(prCreated) || !got.UpdatedAt.Equal(prUpdated) {
		t.Fatalf("times = %v / %v", got.CreatedAt, got.UpdatedAt)
	}
	if b := readReqBody(t, rt.requests[4]); !strings.Contains(b, `"refs":true`) {
		t.Errorf("page 1 must request closing refs: %s", b)
	}
	if b := readReqBody(t, rt.requests[5]); !strings.Contains(b, `"refs":false`) {
		t.Errorf("page 2 must not re-request closing refs: %s", b)
	}
}

// The checks-only read never asks for closing references.
func TestChecksSkipsClosingRefs(t *testing.T) {
	rt := &scriptedRoundTripper{responses: pullRESTLegs("", ok200(noRollupGraphQL(headSHA)))}
	rt.responses = append(rt.responses[:1], rt.responses[2:]...) // Checks reads no reviews
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	if _, err := g.Checks(context.Background(), "org/repo", 7); err != nil {
		t.Fatalf("Checks: %v", err)
	}
	if b := readReqBody(t, rt.requests[len(rt.requests)-1]); !strings.Contains(b, `"refs":false`) {
		t.Errorf("checks read requested closing refs: %s", b)
	}
}

// A GraphQL error on the closing-refs page fails the read rather than dropping links.
func TestGetPullRequestGraphQLErrorIsError(t *testing.T) {
	rt := &scriptedRoundTripper{responses: pullRESTLegs("", ok200(`{"errors":[{"message":"boom"}]}`))}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	if _, err := g.GetPullRequest(context.Background(), "org/repo", 7); err == nil {
		t.Fatal("GetPullRequest succeeded on a GraphQL error")
	}
}

func TestListOpenPullRequests(t *testing.T) {
	rt := &scriptedRoundTripper{responses: []scriptedResponse{
		{status: 200, body: `[{"number":9,"state":"open","updated_at":"2026-09-02T11:30:00Z"}]`,
			headers: map[string]string{"Link": `<https://api.github.com/x?page=2>; rel="next"`}},
		ok200(`[{"number":4,"state":"open","updated_at":"2026-09-01T10:00:00Z"}]`),
	}}
	g := newTestGitHub(rt, &fakeTokenSource{token: "t"})

	got, err := g.ListOpenPullRequests(context.Background(), "org/repo")
	if err != nil {
		t.Fatalf("ListOpenPullRequests: %v", err)
	}
	want := []UpdatedPull{{Number: 9, State: "open", UpdatedAt: prUpdated}, {Number: 4, State: "open", UpdatedAt: prCreated}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if q := rt.requests[0].URL.Query(); rt.requests[0].URL.Path != "/repos/org/repo/pulls" || q.Get("state") != "open" {
		t.Errorf("request = %s, want the open pulls list", rt.requests[0].URL)
	}
}
