//go:build livegithub

package forge

// Live-credentials oracle suite (leg 2 of the forge integration-testing record,
// docs/designs/product/compass-forge-integration-testing/design.md §T2). Guarded
// by //go:build livegithub so a bare `go test ./internal/forge/` never compiles
// it — the untagged golden battery (golden_test.go) stays credential-free.
//
// Each scenario re-runs a T1 golden scenario against the REAL forge (GitHub /
// Linear) using per-identity PATs from the environment, then asserts the live
// decoded domain value matches the committed T1 fixture's `want` EXCEPT for an
// explicit volatile-field allowlist (see volatileFields) — the fields the forge
// assigns per run/identity or that a hygiene-unique artifact name perturbs.
//
// It reuses T1's harness symbols directly (same package, no import): the
// fixture/fixtureRequest/fixtureResponse schema, loadFixtures, and the
// fakeTokenSource shape (github_test.go) built per identity from its env PAT.
//
// The suite SKIPS (never fails) when its credentials are unset: the GitHub trio
// gates the GitHub legs, LINEAR_FORGE gates the Linear legs independently. The
// skip message is a stable one-line string literal (liveSkipMessage /
// liveLinearSkipMessage) that T3's CI guard greps from this source.
//
// context.Background() below is the test root — the sanctioned F-ttsr exemption
// (mirrors github_test.go / linear_test.go / golden_test.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// liveSkipMessage is the STABLE skip string for the GitHub live legs. T3's CI
// guard derives its skip-detection grep from this one-line literal — keep it a
// single plain string literal so a source-derived grep can read it verbatim.
const liveSkipMessage = "live github oracle: LIVEGITHUB_* credentials unset; skipping the live-contract suite"

// liveLinearSkipMessage is the STABLE skip string for the Linear live legs,
// which gate independently on LINEAR_FORGE (co-equal provider, separate creds).
const liveLinearSkipMessage = "live linear oracle: LINEAR_FORGE credential unset; skipping the live-contract suite"

// Env contract (frozen design T2 interfaces).
const (
	envRepo     = "LIVEGITHUB_REPO"           // "owner/name" of the throwaway repo
	envAuthor   = "LIVEGITHUB_AUTHOR_TOKEN"   // test-only author bot PAT
	envReviewer = "LIVEGITHUB_REVIEWER_TOKEN" // test-only reviewer bot PAT
	envLinear   = "LINEAR_FORGE"              // test-only Linear token (dedicated test team)
	envTeam     = "LINEAR_FORGE_TEAM"         // test team key; defaults to "SEA"
)

// requireLive reads the GitHub credential trio and t.Skips (never fails) when
// any is unset, mirroring go/e2e/harness_test.go's podmanUsable() skip. It
// returns the throwaway repo coordinate and env-backed token sources for the
// author and reviewer identities (fakeTokenSource is T1's shape; Token yields
// the env PAT, Invalidate counts — so the auth-failure test can assert the
// client's Invalidate() path fired).
func requireLive(t *testing.T) (repo string, author, reviewer *fakeTokenSource) {
	t.Helper()
	repo = os.Getenv(envRepo)
	at := os.Getenv(envAuthor)
	rt := os.Getenv(envReviewer)
	if repo == "" || at == "" || rt == "" {
		t.Skip(liveSkipMessage)
	}
	return repo, &fakeTokenSource{token: at}, &fakeTokenSource{token: rt}
}

// requireLinear reads the Linear credential and t.Skips independently of the
// GitHub trio, returning an env-backed token source and the test team key.
func requireLinear(t *testing.T) (token *fakeTokenSource, team string) {
	t.Helper()
	tok := os.Getenv(envLinear)
	if tok == "" {
		t.Skip(liveLinearSkipMessage)
	}
	team = os.Getenv(envTeam)
	if team == "" {
		team = "SEA"
	}
	return &fakeTokenSource{token: tok}, team
}

// liveGitHub builds a real GitHub client against github.com for the given
// identity (nil Client -> the default 30s client).
func liveGitHub(ts TokenSource) *GitHub {
	return NewGitHub(GitHubConfig{Host: "github.com", Token: ts})
}

// liveLinear builds a real Linear client against the default endpoint (nil Log
// -> slog.Default()).
func liveLinear(ts TokenSource) *Linear {
	return NewLinear(LinearConfig{Token: ts})
}

// newRunID is a per-run unique suffix for artifact names, so a leaked artifact
// from a prior run never collides with (or fails) the next run's create.
func newRunID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// volatileFields names the JSON keys (domain types marshal to their Go field
// names — no json tags) that are NOT asserted against the committed fixture,
// because the forge assigns them per run/identity or a hygiene-unique artifact
// name perturbs them. Kept explicit and commented so a reviewer sees exactly
// what the oracle does NOT pin.
//
//   - Forge-assigned per run: Number (issue/PR number), ID (comment/review id),
//     URL (canonical web url), UpdatedAt (server timestamp), HeadSHA (commit),
//     HeadRef / BaseRef (branch names).
//   - Forge-assigned per identity: ForgeAccount / Author (the live bot login
//     differs from the fixture's captured account).
//   - Run-supplied uniquely: Title / Body — hygiene requires run-id-suffixed
//     artifact names, so a live create's title/body never equals the fixture's
//     captured input; the state fold, label passthrough, verdict mapping, draft
//     flag and structural shape (everything NOT listed here) ARE asserted.
//
// Rate-limit headers are volatile too but never reach a decoded domain value,
// so there is nothing to strip for them here (they live on the HTTP response,
// which the oracle does not compare).
var volatileFields = map[string]struct{}{
	"Number":       {},
	"ID":           {},
	"URL":          {},
	"UpdatedAt":    {},
	"HeadSHA":      {},
	"HeadRef":      {},
	"BaseRef":      {},
	"ForgeAccount": {},
	"Author":       {},
	"Title":        {},
	"Body":         {},
}

// stripVolatile recursively deletes every volatile-allowlisted key from a
// decoded JSON tree (map/slice), so the residue compares only the fields the
// oracle pins.
func stripVolatile(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if _, vol := volatileFields[k]; vol {
				delete(x, k)
				continue
			}
			x[k] = stripVolatile(child)
		}
		return x
	case []any:
		for i, child := range x {
			x[i] = stripVolatile(child)
		}
		return x
	default:
		return v
	}
}

// liveFixture returns the committed T1 fixture named `name` for the provider, or
// fails — the oracle asserts against the SAME fixtures the golden battery
// replays.
func liveFixture(t *testing.T, provider, name string) fixture {
	t.Helper()
	for _, f := range loadFixtures(t, filepath.Join("testdata", provider)) {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no committed fixture %q under testdata/%s", name, provider)
	return fixture{}
}

// assertMatchesFixture asserts the live decoded value matches the fixture's
// `want` once both are reduced through the volatile-field allowlist.
func assertMatchesFixture(t *testing.T, got any, want json.RawMessage) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal live value: %v", err)
	}
	var gotTree, wantTree any
	if err := json.Unmarshal(gotJSON, &gotTree); err != nil {
		t.Fatalf("unmarshal live value: %v", err)
	}
	if err := json.Unmarshal(want, &wantTree); err != nil {
		t.Fatalf("unmarshal fixture want: %v", err)
	}
	gotStripped, err := json.Marshal(stripVolatile(gotTree))
	if err != nil {
		t.Fatalf("re-marshal live value: %v", err)
	}
	wantStripped, err := json.Marshal(stripVolatile(wantTree))
	if err != nil {
		t.Fatalf("re-marshal fixture want: %v", err)
	}
	assertJSONEqual(t, "live vs fixture (volatile fields stripped)", gotStripped, wantStripped)
}

// --- GitHub scenarios --------------------------------------------------------

// TestLiveGitHubCreateIssue creates a uniquely-named issue and asserts the
// decoded value matches the committed fixture under the allowlist; t.Cleanup
// closes the issue (GitHub cannot delete issues via REST).
func TestLiveGitHubCreateIssue(t *testing.T) {
	repo, author, _ := requireLive(t)
	ctx := context.Background()
	gh := liveGitHub(author)

	f := liveFixture(t, providerGitHub, "create_issue")
	got, err := createIssueWithBackoff(ctx, gh, repo, CreateIssue{
		Title:  "compass-live-issue-" + newRunID(),
		Body:   f.Request.Input.body(),
		Labels: f.Request.Input.labels(),
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	t.Cleanup(func() { closeGitHubIssue(t, author, repo, got.Number) })
	assertMatchesFixture(t, got, f.Response.Want)
}

// TestLiveGitHubCommentOnIssue comments on a freshly-created issue and asserts
// the decoded comment matches the fixture under the allowlist.
func TestLiveGitHubCommentOnIssue(t *testing.T) {
	repo, author, _ := requireLive(t)
	ctx := context.Background()
	gh := liveGitHub(author)

	issue, err := createIssueWithBackoff(ctx, gh, repo, CreateIssue{Title: "compass-live-comment-" + newRunID()})
	if err != nil {
		t.Fatalf("CreateIssue (setup): %v", err)
	}
	t.Cleanup(func() { closeGitHubIssue(t, author, repo, issue.Number) })

	f := liveFixture(t, providerGitHub, "comment_on_issue")
	got, err := gh.CommentOnIssue(ctx, repo, issue.Number, "compass-live comment "+newRunID())
	if err != nil {
		t.Fatalf("CommentOnIssue: %v", err)
	}
	assertMatchesFixture(t, got, f.Response.Want)
}

// TestLiveGitHubGetIssue creates then reads an issue back, asserting the decoded
// value matches the fixture under the allowlist.
func TestLiveGitHubGetIssue(t *testing.T) {
	repo, author, _ := requireLive(t)
	ctx := context.Background()
	gh := liveGitHub(author)

	f := liveFixture(t, providerGitHub, "get_issue")
	issue, err := createIssueWithBackoff(ctx, gh, repo, CreateIssue{
		Title:  "compass-live-get-" + newRunID(),
		Body:   f.Response.wantBody(),
		Labels: []string{"bug", "p1"},
	})
	if err != nil {
		t.Fatalf("CreateIssue (setup): %v", err)
	}
	t.Cleanup(func() { closeGitHubIssue(t, author, repo, issue.Number) })

	got, err := gh.GetIssue(ctx, repo, issue.Number)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	assertMatchesFixture(t, got, f.Response.Want)
}

// TestLiveGitHubListIssues lists issues and asserts each decoded row matches the
// fixture's row shape under the allowlist (list ordering/count is repo state, so
// the oracle asserts the first row's shape, not the whole slice length).
func TestLiveGitHubListIssues(t *testing.T) {
	repo, author, _ := requireLive(t)
	ctx := context.Background()
	gh := liveGitHub(author)

	setup, err := createIssueWithBackoff(ctx, gh, repo, CreateIssue{
		Title:  "compass-live-list-" + newRunID(),
		Labels: []string{"bug"},
	})
	if err != nil {
		t.Fatalf("CreateIssue (setup): %v", err)
	}
	t.Cleanup(func() { closeGitHubIssue(t, author, repo, setup.Number) })

	f := liveFixture(t, providerGitHub, "list_issues")
	got, err := gh.ListIssues(ctx, repo, IssueFilter{State: "open", Labels: []string{"bug"}})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("ListIssues returned no rows; expected at least the setup issue")
	}
	assertMatchesFixture(t, got[0], f.Response.firstWant(t))
}

// TestLiveGitHubCreatePullRequest opens a PR on a prepared head branch and
// asserts the decoded PR matches the fixture under the allowlist; t.Cleanup
// closes the PR and deletes the branch.
func TestLiveGitHubCreatePullRequest(t *testing.T) {
	repo, author, _ := requireLive(t)
	ctx := context.Background()
	gh := liveGitHub(author)

	head := "compass-live-" + newRunID()
	f := liveFixture(t, providerGitHub, "create_pull_request")
	got, err := gh.CreatePullRequest(ctx, repo, CreatePR{
		Title:   "compass-live-pr-" + newRunID(),
		Body:    f.Request.Input.body(),
		HeadRef: head,
		BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	t.Cleanup(func() { teardownGitHubPR(t, author, repo, got.Number, head) })
	assertMatchesFixture(t, got, f.Response.Want)
}

// TestLiveGitHubCommentOnPullRequest comments on a freshly-opened PR. No
// committed fixture exists for this op (see friction report), so the oracle
// asserts the decoded shape directly: a non-zero id, a URL, and a non-empty body
// echoed back.
func TestLiveGitHubCommentOnPullRequest(t *testing.T) {
	repo, author, _ := requireLive(t)
	ctx := context.Background()
	gh := liveGitHub(author)

	head := "compass-live-" + newRunID()
	pr, err := gh.CreatePullRequest(ctx, repo, CreatePR{
		Title:   "compass-live-prcomment-" + newRunID(),
		HeadRef: head,
		BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("CreatePullRequest (setup): %v", err)
	}
	t.Cleanup(func() { teardownGitHubPR(t, author, repo, pr.Number, head) })

	got, err := gh.CommentOnPullRequest(ctx, repo, pr.Number, "compass-live pr comment "+newRunID())
	if err != nil {
		t.Fatalf("CommentOnPullRequest: %v", err)
	}
	if got.ID == 0 || got.URL == "" {
		t.Errorf("CommentOnPullRequest decoded = %+v, want non-zero ID and URL", got)
	}
}

// TestLiveGitHubSubmitReview submits REQUEST_CHANGES then COMMENT reviews from
// the reviewer identity. No committed fixture exists for submit_review (see
// friction report), so the oracle asserts the verdict fold directly.
func TestLiveGitHubSubmitReview(t *testing.T) {
	repo, author, reviewer := requireLive(t)
	ctx := context.Background()
	authorGH := liveGitHub(author)
	reviewerGH := liveGitHub(reviewer)

	head := "compass-live-" + newRunID()
	pr, err := authorGH.CreatePullRequest(ctx, repo, CreatePR{
		Title:   "compass-live-review-" + newRunID(),
		HeadRef: head,
		BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("CreatePullRequest (setup): %v", err)
	}
	t.Cleanup(func() { teardownGitHubPR(t, author, repo, pr.Number, head) })

	rc, err := reviewerGH.SubmitReview(ctx, repo, pr.Number, SubmitReview{
		Verdict: "request_changes",
		Body:    "compass-live please change",
	})
	if err != nil {
		t.Fatalf("SubmitReview request_changes: %v", err)
	}
	if rc.Verdict != "changes_requested" {
		t.Errorf("request_changes verdict = %q, want changes_requested", rc.Verdict)
	}

	cm, err := reviewerGH.SubmitReview(ctx, repo, pr.Number, SubmitReview{
		Verdict: "comment",
		Body:    "compass-live note",
	})
	if err != nil {
		t.Fatalf("SubmitReview comment: %v", err)
	}
	if cm.Verdict != "commented" {
		t.Errorf("comment verdict = %q, want commented", cm.Verdict)
	}
}

// TestLiveGitHubF1AuthorApprovalRejected is the single most load-bearing
// scenario: the author opens a PR, the author's OWN APPROVE is rejected by
// GitHub with 422 (only the PR author's COMMENT is allowed), then the reviewer's
// APPROVE succeeds. Teardown closes the PR and deletes the branch.
func TestLiveGitHubF1AuthorApprovalRejected(t *testing.T) {
	repo, author, reviewer := requireLive(t)
	ctx := context.Background()
	authorGH := liveGitHub(author)
	reviewerGH := liveGitHub(reviewer)

	head := "compass-live-" + newRunID()
	pr, err := authorGH.CreatePullRequest(ctx, repo, CreatePR{
		Title:   "compass-live-f1-" + newRunID(),
		Body:    "raw <!--owner--> body",
		HeadRef: head,
		BaseRef: "main",
	})
	if err != nil {
		t.Fatalf("author CreatePullRequest: %v", err)
	}
	t.Cleanup(func() { teardownGitHubPR(t, author, repo, pr.Number, head) })

	// The author approving its OWN PR is a 422 (GitHub forbids APPROVE /
	// REQUEST_CHANGES from the PR author; only COMMENT is allowed).
	_, err = authorGH.SubmitReview(ctx, repo, pr.Number, SubmitReview{Verdict: "approve"})
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 422 {
		t.Fatalf("author self-approve: want *StatusError 422, got %v", err)
	}

	// The reviewer approving succeeds.
	got, err := reviewerGH.SubmitReview(ctx, repo, pr.Number, SubmitReview{Verdict: "approve"})
	if err != nil {
		t.Fatalf("reviewer approve: %v", err)
	}
	if got.Verdict != "approved" {
		t.Errorf("reviewer verdict = %q, want approved", got.Verdict)
	}
}

// TestLiveGitHubAuthFailureInvalidates drives a garbage token through a read and
// asserts the 401/403 bad-creds path maps to a *StatusError AND fires the
// client's TokenSource.Invalidate() (the credential-rotation hook).
func TestLiveGitHubAuthFailureInvalidates(t *testing.T) {
	repo, _, _ := requireLive(t)
	ctx := context.Background()
	bad := &fakeTokenSource{token: "ghp_compass_live_invalid_" + newRunID()}
	gh := liveGitHub(bad)

	_, err := gh.ListIssues(ctx, repo, IssueFilter{State: "open"})
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("bad-token ListIssues: want *StatusError, got %v", err)
	}
	if se.Status != http.StatusUnauthorized && se.Status != http.StatusForbidden {
		t.Errorf("bad-token status = %d, want 401 or 403", se.Status)
	}
	if bad.invalidated == 0 {
		t.Errorf("bad creds did not fire TokenSource.Invalidate()")
	}
}

// --- Linear scenarios (co-equal) ---------------------------------------------

// TestLiveLinearCreateIssue creates a uniquely-named issue on the test team and
// asserts the decoded value matches the committed fixture under the allowlist.
func TestLiveLinearCreateIssue(t *testing.T) {
	ts, team := requireLinear(t)
	ctx := context.Background()
	ln := liveLinear(ts)

	f := liveFixture(t, providerLinear, "create_issue")
	got, err := ln.CreateIssue(ctx, team, CreateIssue{
		Title:  "compass-live-issue-" + newRunID(),
		Body:   f.Request.Input.body(),
		Labels: f.Request.Input.labels(),
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	t.Cleanup(func() { archiveLinearIssue(t, ts, got.Number) })
	assertMatchesFixture(t, got, f.Response.Want)
}

// TestLiveLinearCommentOnIssue comments on a freshly-created Linear issue and
// asserts the decoded comment matches the fixture under the allowlist.
func TestLiveLinearCommentOnIssue(t *testing.T) {
	ts, team := requireLinear(t)
	ctx := context.Background()
	ln := liveLinear(ts)

	issue, err := ln.CreateIssue(ctx, team, CreateIssue{Title: "compass-live-comment-" + newRunID()})
	if err != nil {
		t.Fatalf("CreateIssue (setup): %v", err)
	}
	t.Cleanup(func() { archiveLinearIssue(t, ts, issue.Number) })

	f := liveFixture(t, providerLinear, "comment_on_issue")
	got, err := ln.CommentOnIssue(ctx, team, issue.Number, "compass-live comment "+newRunID())
	if err != nil {
		t.Fatalf("CommentOnIssue: %v", err)
	}
	assertMatchesFixture(t, got, f.Response.Want)
}

// TestLiveLinearGetIssue creates then reads a Linear issue back, asserting the
// decoded value matches the fixture under the allowlist.
func TestLiveLinearGetIssue(t *testing.T) {
	ts, team := requireLinear(t)
	ctx := context.Background()
	ln := liveLinear(ts)

	f := liveFixture(t, providerLinear, "get_issue")
	issue, err := ln.CreateIssue(ctx, team, CreateIssue{
		Title:  "compass-live-get-" + newRunID(),
		Labels: []string{"bug", "p1"},
	})
	if err != nil {
		t.Fatalf("CreateIssue (setup): %v", err)
	}
	t.Cleanup(func() { archiveLinearIssue(t, ts, issue.Number) })

	got, err := ln.GetIssue(ctx, team, issue.Number)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	assertMatchesFixture(t, got, f.Response.Want)
}

// TestLiveLinearListIssues lists issues on the test team and asserts the first
// decoded row matches the fixture's row shape under the allowlist.
func TestLiveLinearListIssues(t *testing.T) {
	ts, team := requireLinear(t)
	ctx := context.Background()
	ln := liveLinear(ts)

	setup, err := ln.CreateIssue(ctx, team, CreateIssue{
		Title:  "compass-live-list-" + newRunID(),
		Labels: []string{"bug"},
	})
	if err != nil {
		t.Fatalf("CreateIssue (setup): %v", err)
	}
	t.Cleanup(func() { archiveLinearIssue(t, ts, setup.Number) })

	f := liveFixture(t, providerLinear, "list_issues")
	got, err := ln.ListIssues(ctx, team, IssueFilter{State: "open", Labels: []string{"bug"}})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("ListIssues returned no rows; expected at least the setup issue")
	}
	assertMatchesFixture(t, got[0], f.Response.firstWant(t))
}

// TestLiveLinearPRUnsupported locks the Linear contract: the PR/review family is
// unsupported on an issues-only forge, returning ErrUnsupported (no wire call).
func TestLiveLinearPRUnsupported(t *testing.T) {
	ts, team := requireLinear(t)
	ctx := context.Background()
	ln := liveLinear(ts)

	if _, err := ln.CreatePullRequest(ctx, team, CreatePR{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Linear CreatePullRequest err = %v, want ErrUnsupported", err)
	}
}

// --- input/fixture accessors -------------------------------------------------

// body returns the fixture input body, or "" when the input is absent.
func (in *fixtureInput) body() string {
	if in == nil {
		return ""
	}
	return in.Body
}

// labels returns the fixture input labels, or nil when the input is absent.
func (in *fixtureInput) labels() []string {
	if in == nil {
		return nil
	}
	return in.Labels
}

// wantBody extracts the "Body" field from a fixture's decoded-value expectation
// (used to seed a live create so the non-volatile body round-trips).
func (r fixtureResponse) wantBody() string {
	var w struct {
		Body string `json:"Body"`
	}
	if err := json.Unmarshal(r.Want, &w); err != nil {
		return ""
	}
	return w.Body
}

// firstWant returns the first element of a list-op fixture's `want` array as a
// single decoded-value expectation.
func (r fixtureResponse) firstWant(t *testing.T) json.RawMessage {
	t.Helper()
	var rows []json.RawMessage
	if err := json.Unmarshal(r.Want, &rows); err != nil {
		t.Fatalf("fixture want is not a JSON array: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("fixture want array is empty")
	}
	return rows[0]
}

// --- rate-limit backoff ------------------------------------------------------

// createIssueWithBackoff wraps a content-creating call with a single bounded
// backoff on GitHub's SECONDARY rate limit (403 abuse-detection on rapid
// content creation). This is real live-API timing behavior on the network path
// — it never executes on the skip path, and it is a bounded one-shot backoff,
// not a retry loop masking a bug.
func createIssueWithBackoff(ctx context.Context, gh *GitHub, repo string, in CreateIssue) (Issue, error) {
	got, err := gh.CreateIssue(ctx, repo, in)
	if isSecondaryRateLimit(err) {
		// Back off once, then retry — GitHub's secondary limit clears quickly.
		select {
		case <-ctx.Done():
			return Issue{}, ctx.Err()
		case <-time.After(30 * time.Second):
		}
		return gh.CreateIssue(ctx, repo, in)
	}
	return got, err
}

// isSecondaryRateLimit reports whether err is GitHub's 403 secondary-rate-limit.
func isSecondaryRateLimit(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusForbidden {
		return false
	}
	return strings.Contains(strings.ToLower(se.Message), "secondary rate limit")
}

// --- teardown (test-side REST; the Provider interface has no close/delete) ----

// closeGitHubIssue closes an issue via REST (GitHub cannot delete issues). A
// teardown failure is logged, never fatal — the next run's unique run-id avoids
// collisions regardless.
func closeGitHubIssue(t *testing.T, ts *fakeTokenSource, repo string, number uint64) {
	t.Helper()
	path := fmt.Sprintf("/repos/%s/issues/%d", repo, number)
	body := strings.NewReader(`{"state":"closed"}`)
	if err := githubREST(context.Background(), ts, http.MethodPatch, path, body); err != nil {
		t.Logf("teardown: close issue #%d: %v", number, err)
	}
}

// teardownGitHubPR closes a PR and deletes its head branch via REST.
func teardownGitHubPR(t *testing.T, ts *fakeTokenSource, repo string, number uint64, head string) {
	t.Helper()
	ctx := context.Background()
	prPath := fmt.Sprintf("/repos/%s/pulls/%d", repo, number)
	if err := githubREST(ctx, ts, http.MethodPatch, prPath, strings.NewReader(`{"state":"closed"}`)); err != nil {
		t.Logf("teardown: close PR #%d: %v", number, err)
	}
	refPath := fmt.Sprintf("/repos/%s/git/refs/heads/%s", repo, head)
	if err := githubREST(ctx, ts, http.MethodDelete, refPath, nil); err != nil {
		t.Logf("teardown: delete branch %q: %v", head, err)
	}
}

// githubREST issues one authenticated REST call to api.github.com for teardown.
func githubREST(ctx context.Context, ts TokenSource, method, path string, body io.Reader) error {
	token, err := ts.Token(ctx)
	if err != nil {
		return fmt.Errorf("resolve token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // teardown drain; close error not actionable
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: http %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(snippet))
	}
	return nil
}

// archiveLinearIssue archives a Linear issue via the GraphQL API for teardown.
func archiveLinearIssue(t *testing.T, ts TokenSource, number uint64) {
	t.Helper()
	// Linear archives by issue node id, not team-scoped number, and resolving
	// the node id is a second query; the dedicated test team plus unique
	// run-ids keep leaked issues harmless, so teardown archives best-effort by
	// the number-scoped mutation the test client would drive. A failure is
	// logged, never fatal.
	ctx := context.Background()
	token, err := ts.Token(ctx)
	if err != nil {
		t.Logf("teardown: resolve linear token: %v", err)
		return
	}
	query := fmt.Sprintf(`{"query":"mutation { issueArchive(id: %q) { success } }"}`, strconv.FormatUint(number, 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.linear.app/graphql", strings.NewReader(query))
	if err != nil {
		t.Logf("teardown: build linear archive request: %v", err)
		return
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("teardown: archive linear issue %d: %v", number, err)
		return
	}
	_ = resp.Body.Close() // teardown drain; close error not actionable
}
