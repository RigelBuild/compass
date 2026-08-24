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
	"encoding/base64"
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

// liveUpdateSkipMessage is the STABLE skip string for the regeneration lane:
// TestLiveUpdateFixtures runs ONLY under -update (it rewrites committed
// fixtures), so a bare tagged run skips it cleanly with this one-line literal.
const liveUpdateSkipMessage = "live update capture: -update unset; skipping fixture regeneration"

// Env contract (frozen design T2 interfaces).
const (
	envRepo     = "LIVEGITHUB_REPO"           // "owner/name" of the throwaway repo
	envAuthor   = "LIVEGITHUB_AUTHOR_TOKEN"   // test-only author bot PAT
	envReviewer = "LIVEGITHUB_REVIEWER_TOKEN" // test-only reviewer bot PAT
	envLinear   = "LINEAR_FORGE"              // test-only Linear token (dedicated test team)
	envTeam     = "LINEAR_FORGE_TEAM"         // test team key; no default (dead "SEA" dropped)
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

// requireLinear reads the Linear credential AND the test team key, t.Skipping
// independently of the GitHub trio when EITHER is unset (no-team-no-run,
// co-equal with the credential gate — the migrated workspace has no default
// team, so a TEAM-unset run would fail opaquely at resolveTeamID). Returns an
// env-backed token source and the test team key.
func requireLinear(t *testing.T) (token *fakeTokenSource, team string) {
	t.Helper()
	tok := os.Getenv(envLinear)
	team = os.Getenv(envTeam)
	if tok == "" || team == "" {
		t.Skip(liveLinearSkipMessage)
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
	got, err := createWithBackoff(ctx, func() (Issue, error) {
		return gh.CreateIssue(ctx, repo, CreateIssue{
			Title:  "compass-live-issue-" + newRunID(),
			Body:   f.Request.Input.body(),
			Labels: f.Request.Input.labels(),
		})
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	t.Cleanup(func() { closeGitHubIssue(t, author, repo, got.Number) })
	assertMatchesFixture(t, got, f.Response.Want)
}

// TestLiveGitHubCommentOnIssue comments on a freshly-created issue and asserts
// the decoded comment's shape DIRECTLY (non-zero ID, non-empty URL, and the
// sent Body echoed back): all four Comment fields are volatile, so a fixture
// compare would strip both sides to {} and pin nothing.
func TestLiveGitHubCommentOnIssue(t *testing.T) {
	repo, author, _ := requireLive(t)
	ctx := context.Background()
	gh := liveGitHub(author)

	issue, err := createWithBackoff(ctx, func() (Issue, error) {
		return gh.CreateIssue(ctx, repo, CreateIssue{Title: "compass-live-comment-" + newRunID()})
	})
	if err != nil {
		t.Fatalf("CreateIssue (setup): %v", err)
	}
	t.Cleanup(func() { closeGitHubIssue(t, author, repo, issue.Number) })

	body := "compass-live comment " + newRunID()
	got, err := createWithBackoff(ctx, func() (Comment, error) {
		return gh.CommentOnIssue(ctx, repo, issue.Number, body)
	})
	if err != nil {
		t.Fatalf("CommentOnIssue: %v", err)
	}
	if got.ID == 0 || got.URL == "" || got.Body != body {
		t.Errorf("CommentOnIssue decoded = %+v, want non-zero ID, non-empty URL, Body=%q", got, body)
	}
}

// TestLiveGitHubGetIssue creates then reads an issue back, asserting the decoded
// value matches the fixture under the allowlist.
func TestLiveGitHubGetIssue(t *testing.T) {
	repo, author, _ := requireLive(t)
	ctx := context.Background()
	gh := liveGitHub(author)

	f := liveFixture(t, providerGitHub, "get_issue")
	issue, err := createWithBackoff(ctx, func() (Issue, error) {
		return gh.CreateIssue(ctx, repo, CreateIssue{
			Title:  "compass-live-get-" + newRunID(),
			Body:   "compass-live get body " + newRunID(),
			Labels: []string{"bug", "p1"},
		})
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

	setup, err := createWithBackoff(ctx, func() (Issue, error) {
		return gh.CreateIssue(ctx, repo, CreateIssue{
			Title:  "compass-live-list-" + newRunID(),
			Labels: []string{"bug"},
		})
	})
	if err != nil {
		t.Fatalf("CreateIssue (setup): %v", err)
	}
	t.Cleanup(func() { closeGitHubIssue(t, author, repo, setup.Number) })

	f := liveFixture(t, providerGitHub, "list_issues")
	filter := IssueFilter{State: "open", Labels: []string{"bug"}}
	row, n, err := findListedIssueWithBackoff(ctx, gh, repo, filter, setup.Number)
	if err != nil {
		t.Fatalf("findListedIssueWithBackoff: %v", err)
	}
	if row.Number != setup.Number {
		t.Fatalf("ListIssues did not return the setup issue #%d among %d rows after backoff", setup.Number, n)
	}
	assertMatchesFixture(t, row, f.Response.firstWant(t))
}

// TestLiveGitHubCreatePullRequest opens a PR on a prepared head branch and
// asserts the decoded PR matches the fixture under the allowlist; t.Cleanup
// closes the PR and deletes the branch.
func TestLiveGitHubCreatePullRequest(t *testing.T) {
	repo, author, _ := requireLive(t)
	ctx := context.Background()
	gh := liveGitHub(author)

	head := "compass-live-" + newRunID()
	seedHeadBranch(t, ctx, author, repo, head)
	f := liveFixture(t, providerGitHub, "create_pull_request")
	got, err := createWithBackoff(ctx, func() (PullRequest, error) {
		return gh.CreatePullRequest(ctx, repo, CreatePR{
			Title:   "compass-live-pr-" + newRunID(),
			Body:    f.Request.Input.body(),
			HeadRef: head,
			BaseRef: "main",
			Draft:   true,
		})
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
	seedHeadBranch(t, ctx, author, repo, head)
	pr, err := createWithBackoff(ctx, func() (PullRequest, error) {
		return gh.CreatePullRequest(ctx, repo, CreatePR{
			Title:   "compass-live-prcomment-" + newRunID(),
			HeadRef: head,
			BaseRef: "main",
		})
	})
	if err != nil {
		t.Fatalf("CreatePullRequest (setup): %v", err)
	}
	t.Cleanup(func() { teardownGitHubPR(t, author, repo, pr.Number, head) })

	body := "compass-live pr comment " + newRunID()
	got, err := createWithBackoff(ctx, func() (Comment, error) {
		return gh.CommentOnPullRequest(ctx, repo, pr.Number, body)
	})
	if err != nil {
		t.Fatalf("CommentOnPullRequest: %v", err)
	}
	if got.ID == 0 || got.URL == "" || got.Body != body {
		t.Errorf("CommentOnPullRequest decoded = %+v, want non-zero ID, non-empty URL, Body=%q", got, body)
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
	seedHeadBranch(t, ctx, author, repo, head)
	pr, err := createWithBackoff(ctx, func() (PullRequest, error) {
		return authorGH.CreatePullRequest(ctx, repo, CreatePR{
			Title:   "compass-live-review-" + newRunID(),
			HeadRef: head,
			BaseRef: "main",
		})
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
	if rc.Verdict != "request_changes" {
		t.Errorf("request_changes verdict = %q, want request_changes (write-side echo)", rc.Verdict)
	}

	cm, err := reviewerGH.SubmitReview(ctx, repo, pr.Number, SubmitReview{
		Verdict: "comment",
		Body:    "compass-live note",
	})
	if err != nil {
		t.Fatalf("SubmitReview comment: %v", err)
	}
	if cm.Verdict != "comment" {
		t.Errorf("comment verdict = %q, want comment (write-side echo)", cm.Verdict)
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
	seedHeadBranch(t, ctx, author, repo, head)
	pr, err := createWithBackoff(ctx, func() (PullRequest, error) {
		return authorGH.CreatePullRequest(ctx, repo, CreatePR{
			Title:   "compass-live-f1-" + newRunID(),
			Body:    "raw <!--owner--> body",
			HeadRef: head,
			BaseRef: "main",
		})
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
	if got.Verdict != "approve" {
		t.Errorf("reviewer verdict = %q, want approve (write-side echo)", got.Verdict)
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
	t.Cleanup(func() { archiveLinearIssue(t, ln, ts, team, got.Number) })
	assertMatchesFixture(t, got, f.Response.Want)
}

// TestLiveLinearCommentOnIssue comments on a freshly-created Linear issue and
// asserts the decoded comment's shape DIRECTLY (non-empty URL and the sent Body
// echoed back): all four Comment fields are volatile, so a fixture compare
// would strip both sides to {} and pin nothing. Linear comment IDs are UUIDs
// that cannot fit forge.Comment.ID (uint64), so ID stays 0 and identity travels
// via URL (see linear.go linearComment.toComment).
func TestLiveLinearCommentOnIssue(t *testing.T) {
	ts, team := requireLinear(t)
	ctx := context.Background()
	ln := liveLinear(ts)

	issue, err := ln.CreateIssue(ctx, team, CreateIssue{Title: "compass-live-comment-" + newRunID()})
	if err != nil {
		t.Fatalf("CreateIssue (setup): %v", err)
	}
	t.Cleanup(func() { archiveLinearIssue(t, ln, ts, team, issue.Number) })

	body := "compass-live comment " + newRunID()
	got, err := ln.CommentOnIssue(ctx, team, issue.Number, body)
	if err != nil {
		t.Fatalf("CommentOnIssue: %v", err)
	}
	if got.URL == "" || got.Body != body {
		t.Errorf("CommentOnIssue decoded = %+v, want non-empty URL and Body=%q", got, body)
	}
}

// TestLiveLinearGetIssue creates then reads a Linear issue back, asserting the
// decoded value matches the fixture under the allowlist. The setup create sends
// no Labels: Linear's CreateIssue does not round-trip label NAMES (IssueCreateInput
// takes label UUIDs, and name->UUID resolution is out of the issues-write slice —
// see linear.go CreateIssue), so a fresh issue reads back with no labels and an
// open (unstarted) state, which is exactly what the fixture pins.
func TestLiveLinearGetIssue(t *testing.T) {
	ts, team := requireLinear(t)
	ctx := context.Background()
	ln := liveLinear(ts)

	f := liveFixture(t, providerLinear, "get_issue")
	issue, err := ln.CreateIssue(ctx, team, CreateIssue{
		Title: "compass-live-get-" + newRunID(),
	})
	if err != nil {
		t.Fatalf("CreateIssue (setup): %v", err)
	}
	t.Cleanup(func() { archiveLinearIssue(t, ln, ts, team, issue.Number) })

	got, err := ln.GetIssue(ctx, team, issue.Number)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	assertMatchesFixture(t, got, f.Response.Want)
}

// TestLiveLinearListIssues lists issues on the test team and asserts the first
// decoded row matches the fixture's row shape under the allowlist. The setup
// create sends no Labels (Linear's CreateIssue does not round-trip label names —
// see TestLiveLinearGetIssue), so the list filters on State alone: a label filter
// could never match a label-less issue. ListIssues is eventually consistent after
// a create, so it polls with the same bounded read-after-write backoff the GitHub
// leg uses.
func TestLiveLinearListIssues(t *testing.T) {
	ts, team := requireLinear(t)
	ctx := context.Background()
	ln := liveLinear(ts)

	setup, err := ln.CreateIssue(ctx, team, CreateIssue{
		Title: "compass-live-list-" + newRunID(),
	})
	if err != nil {
		t.Fatalf("CreateIssue (setup): %v", err)
	}
	t.Cleanup(func() { archiveLinearIssue(t, ln, ts, team, setup.Number) })

	f := liveFixture(t, providerLinear, "list_issues")
	row, n, err := findListedIssueWithBackoff(ctx, ln, team, IssueFilter{State: "open"}, setup.Number)
	if err != nil {
		t.Fatalf("findListedIssueWithBackoff: %v", err)
	}
	if row.Number != setup.Number {
		t.Fatalf("ListIssues did not return the setup issue #%d among %d rows after backoff", setup.Number, n)
	}
	assertMatchesFixture(t, row, f.Response.firstWant(t))
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

// createWithBackoff wraps ANY GitHub content-creating call with a single
// bounded backoff on GitHub's SECONDARY rate limit (403 abuse-detection on
// rapid content creation — issue/PR/comment creates and the H3 branch-seed
// commit all trip it). This is real live-API timing behavior on the network
// path: it never executes on the skip path, and it is a bounded one-shot
// ctx-aware backoff, NOT a retry loop masking a bug (rule://no-retries).
func createWithBackoff[T any](ctx context.Context, create func() (T, error)) (T, error) {
	got, err := create()
	if isSecondaryRateLimit(err) {
		// Back off once, then re-issue — GitHub's secondary limit clears quickly.
		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case <-time.After(30 * time.Second):
		}
		return create()
	}
	return got, err
}

// issueLister is the ListIssues read both live providers expose (GitHub and
// Linear satisfy it structurally), so the read-after-write backoff below serves
// either leg.
type issueLister interface {
	ListIssues(ctx context.Context, repo string, f IssueFilter) ([]Issue, error)
}

// findListedIssueWithBackoff polls ListIssues until the target issue appears,
// tolerating a provider's list read-after-write lag: the list index is eventually
// consistent, so a just-created issue can be absent for a few seconds. GitHub's
// REST /issues list exhibits this directly (the list index lags GetIssue-by-number);
// the same bounded gate defensively covers any propagation lag on Linear's filtered
// issues query. Like createWithBackoff this is real live-API timing behavior on the
// network path — a bounded, ctx-aware event-gate, NOT a retry loop masking a bug
// (rule://no-retries): a genuinely-absent issue still fails loud after the bound,
// and it never executes on the skip path.
// Returns the matching row, the row count observed on the last attempt (for the
// caller's diagnostic), and an error only on ctx cancellation or a ListIssues
// failure.
func findListedIssueWithBackoff(ctx context.Context, lister issueLister, repo string, f IssueFilter, want uint64) (Issue, int, error) {
	var lastLen int
	// A few attempts spanning the propagation window; total bound (~17s) stays
	// well under the oracle step budget, on the same scale as createWithBackoff.
	delays := []time.Duration{0, 2 * time.Second, 5 * time.Second, 10 * time.Second}
	for _, d := range delays {
		if d > 0 {
			select {
			case <-ctx.Done():
				return Issue{}, lastLen, ctx.Err()
			case <-time.After(d):
			}
		}
		got, err := lister.ListIssues(ctx, repo, f)
		if err != nil {
			return Issue{}, lastLen, err
		}
		lastLen = len(got)
		for i := range got {
			if got[i].Number == want {
				return got[i], lastLen, nil
			}
		}
	}
	return Issue{}, lastLen, nil // not found within bound; caller fails loud
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

// seedHeadBranch prepares a head branch for a PR create: GitHub 422s a
// CreatePullRequest ("No commits between main and <head>") unless the head ref
// exists AND diverges from main by at least one commit. It (1) GETs main's SHA,
// (2) creates refs/heads/<head> at that SHA, then (3) PUTs a unique file on
// <head> to make it diverge. Authored by the same identity that opens the PR.
// The create-content PUT is wrapped in the shared secondary-rate-limit backoff.
func seedHeadBranch(t *testing.T, ctx context.Context, ts *fakeTokenSource, repo, head string) {
	t.Helper()

	var mainRef struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := githubRESTJSON(ctx, ts, http.MethodGet,
		fmt.Sprintf("/repos/%s/git/ref/heads/main", repo), nil, &mainRef); err != nil {
		t.Fatalf("seed head branch: get main ref: %v", err)
	}

	createRef := fmt.Sprintf(`{"ref":"refs/heads/%s","sha":%q}`, head, mainRef.Object.SHA)
	if err := githubREST(ctx, ts, http.MethodPost,
		fmt.Sprintf("/repos/%s/git/refs", repo), strings.NewReader(createRef)); err != nil {
		t.Fatalf("seed head branch: create ref %q: %v", head, err)
	}

	path := "compass-live/" + newRunID() + ".txt"
	content := base64.StdEncoding.EncodeToString([]byte("compass-live seed " + head + "\n"))
	putBody := fmt.Sprintf(`{"message":"compass-live seed commit","content":%q,"branch":%q}`, content, head)
	if _, err := createWithBackoff(ctx, func() (struct{}, error) {
		return struct{}{}, githubREST(ctx, ts, http.MethodPut,
			fmt.Sprintf("/repos/%s/contents/%s", repo, path), strings.NewReader(putBody))
	}); err != nil {
		t.Fatalf("seed head branch: create diverging commit on %q: %v", head, err)
	}
}

// githubRESTJSON issues one authenticated REST call and decodes a 2xx JSON body
// into out (the read-side companion to githubREST, used by the H3 seed).
func githubRESTJSON(ctx context.Context, ts TokenSource, method, path string, body io.Reader, out any) error {
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
	defer func() { _ = resp.Body.Close() }() // response drain; close error not actionable
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: http %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(payload))
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}

// archiveLinearIssue archives a Linear issue via the GraphQL API for teardown.
// Linear's issueArchive takes the issue NODE UUID, not the team-scoped number,
// so this resolves the UUID first (reusing the client's own resolveIssueID) and
// archives by UUID — archiving by number is a permanent no-op that leaks every
// issue. A failure is logged, never fatal; the dedicated test team plus unique
// run-ids keep any leaked issue harmless.
func archiveLinearIssue(t *testing.T, ln *Linear, ts TokenSource, team string, number uint64) {
	t.Helper()
	ctx := context.Background()
	id, err := ln.resolveIssueID(ctx, team, number)
	if err != nil {
		t.Logf("teardown: resolve linear issue %s-%d node id: %v", team, number, err)
		return
	}
	token, err := ts.Token(ctx)
	if err != nil {
		t.Logf("teardown: resolve linear token: %v", err)
		return
	}
	query := fmt.Sprintf(`{"query":"mutation { issueArchive(id: %q) { success } }"}`, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.linear.app/graphql", strings.NewReader(query))
	if err != nil {
		t.Logf("teardown: build linear archive request: %v", err)
		return
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("teardown: archive linear issue %s-%d: %v", team, number, err)
		return
	}
	_ = resp.Body.Close() // teardown drain; close error not actionable
}

// --- -update live capture (RIG-2229 T2) --------------------------------------

// recordingRoundTripper wraps a real http.RoundTripper and records every
// response (status + raw body) in wire order. Only the RESPONSE is recorded:
// the request half of each fixture is re-derived (deriveFixtureHalves) by
// replaying the canonicalized responses through the real client, so a committed
// request matches what golden replay emits BY CONSTRUCTION instead of carrying
// live per-run coordinates. Bodies are buffered and re-wrapped so the real
// client still reads them intact.
type recordingRoundTripper struct {
	inner     http.RoundTripper
	responses []capturedResponse
	err       error // first capture error; surfaced by the caller
}

// RoundTrip delegates to the wrapped transport and records the response. A body
// read/re-wrap failure is latched (not swallowed) and surfaced to the test; the
// real response still proceeds so the live op is not perturbed.
func (rt *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.inner.RoundTrip(req)
	if err != nil {
		rt.record(capturedResponse{}, err)
		return resp, err
	}
	rec := capturedResponse{status: resp.StatusCode}
	if resp.Body != nil {
		buf, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close() // response body fully buffered; re-wrapped below
		if readErr != nil {
			rt.record(rec, readErr)
			return resp, fmt.Errorf("recordingRoundTripper: read response body: %w", readErr)
		}
		rec.body = json.RawMessage(buf)
		resp.Body = io.NopCloser(bytes.NewReader(buf))
	}
	rt.record(rec, nil)
	return resp, nil
}

// record appends a response and latches the first capture error.
func (rt *recordingRoundTripper) record(r capturedResponse, err error) {
	if err != nil && rt.err == nil {
		rt.err = err
	}
	rt.responses = append(rt.responses, r)
}

// captureSpec is one fixture-regeneration recipe: the committed fixture it
// rewrites, the number of leading responses that are prelude probes (resolve/
// actor round-trips before the asserted request), and a run func that drives the
// live op through a client built on the recording transport. run returns the
// fixtureRequest coordinates (op + inputs); both request halves and the responses
// are (re-)derived from the recording by assembleFixture.
type captureSpec struct {
	provider string
	name     string
	prelude  int
	run      func(t *testing.T, rt *recordingRoundTripper) fixtureRequest
}

// TestLiveUpdateFixtures is the -update regeneration lane (RIG-2229 T2). Under
// -update it re-runs each committed fixture's scenario against the REAL forge
// through a recording transport, canonicalizes the captured wire responses
// (reusing the oracle's volatileFields as the source of truth, via domainToWire
// / canonicalizeWire), derives both fixture halves by replaying the canonicalized
// responses through the real client (deriveFixtureHalves — so the committed
// request matches golden replay and decode(Body)==Want, both by construction),
// and rewrites testdata/<provider>/<name>.json via writeFixture. Absent -update
// it skips cleanly (it rewrites committed fixtures — never a default run). The
// credential gates (requireLive/requireLinear) skip each provider's legs
// independently when its creds are unset.
//
// context.Background() below is the test root — the sanctioned F-ttsr exemption
// (mirrors the sibling live scenarios).
func TestLiveUpdateFixtures(t *testing.T) {
	if !*update {
		t.Skip(liveUpdateSkipMessage)
	}

	for _, spec := range updateCaptureSpecs() {
		t.Run(spec.provider+"/"+spec.name, func(t *testing.T) {
			rt := &recordingRoundTripper{inner: http.DefaultTransport}
			coords := spec.run(t, rt)
			if rt.err != nil {
				t.Fatalf("capture transport error: %v", rt.err)
			}
			f := assembleFixture(t, spec.provider, spec.name, spec.prelude, coords, rt.responses)
			writeFixture(t, filepath.Join("testdata", spec.provider), f)
		})
	}
}

// updateCaptureSpecs is the capture table: one spec per committed fixture across
// both providers, split by provider so each half documents its own prelude-count
// grounding (githubUpdateSpecs / linearUpdateSpecs).
func updateCaptureSpecs() []captureSpec {
	return append(githubUpdateSpecs(), linearUpdateSpecs()...)
}

// githubUpdateSpecs is the GitHub half of the capture table (prelude 0 — GitHub
// writes/reads are single-shot; get_pull_request's reviews+checks legs are EXTRA
// after the asserted detail GET, not prelude).
func githubUpdateSpecs() []captureSpec {
	return []captureSpec{
		{provider: providerGitHub, name: "create_issue", prelude: 0,
			run: func(t *testing.T, rt *recordingRoundTripper) fixtureRequest {
				t.Helper()
				repo, author, _ := requireLive(t)
				ctx := context.Background()
				gh := recordingGitHub(author, rt)
				in := CreateIssue{Title: "compass-live-issue-" + newRunID(), Body: "stamped body", Labels: []string{"bug", "p1"}}
				got, err := createWithBackoff(ctx, func() (Issue, error) { return gh.CreateIssue(ctx, repo, in) })
				if err != nil {
					t.Fatalf("CreateIssue: %v", err)
				}
				t.Cleanup(func() { closeGitHubIssue(t, author, repo, got.Number) })
				return fixtureRequest{Op: "create_issue", Repo: repo,
					Input: &fixtureInput{Title: in.Title, Body: in.Body, Labels: in.Labels}}
			}},
		{provider: providerGitHub, name: "get_issue", prelude: 0,
			run: func(t *testing.T, rt *recordingRoundTripper) fixtureRequest {
				t.Helper()
				repo, author, _ := requireLive(t)
				ctx := context.Background()
				setup := setupGitHub(author)
				issue, err := createWithBackoff(ctx, func() (Issue, error) {
					return setup.CreateIssue(ctx, repo, CreateIssue{
						Title: "compass-live-get-" + newRunID(), Body: "raw <!--owner--> body", Labels: []string{"bug", "p1"}})
				})
				if err != nil {
					t.Fatalf("CreateIssue (setup): %v", err)
				}
				t.Cleanup(func() { closeGitHubIssue(t, author, repo, issue.Number) })
				gh := recordingGitHub(author, rt)
				if _, err := gh.GetIssue(ctx, repo, issue.Number); err != nil {
					t.Fatalf("GetIssue: %v", err)
				}
				return fixtureRequest{Op: "get_issue", Repo: repo, Number: issue.Number}
			}},
		{provider: providerGitHub, name: "list_issues", prelude: 0,
			run: func(t *testing.T, rt *recordingRoundTripper) fixtureRequest {
				t.Helper()
				repo, author, _ := requireLive(t)
				ctx := context.Background()
				setup := setupGitHub(author)
				got, err := createWithBackoff(ctx, func() (Issue, error) {
					return setup.CreateIssue(ctx, repo, CreateIssue{Title: "compass-live-list-" + newRunID(), Labels: []string{"bug"}})
				})
				if err != nil {
					t.Fatalf("CreateIssue (setup): %v", err)
				}
				t.Cleanup(func() { closeGitHubIssue(t, author, repo, got.Number) })
				filter := IssueFilter{State: "open", Labels: []string{"bug"}}
				gh := recordingGitHub(author, rt)
				if _, err := gh.ListIssues(ctx, repo, filter); err != nil {
					t.Fatalf("ListIssues: %v", err)
				}
				return fixtureRequest{Op: "list_issues", Repo: repo,
					Filter: &fixtureFilter{State: filter.State, Labels: filter.Labels}}
			}},
		{provider: providerGitHub, name: "create_pull_request", prelude: 0,
			run: func(t *testing.T, rt *recordingRoundTripper) fixtureRequest {
				t.Helper()
				repo, author, _ := requireLive(t)
				ctx := context.Background()
				head := "compass-live-" + newRunID()
				seedHeadBranch(t, ctx, author, repo, head)
				gh := recordingGitHub(author, rt)
				in := CreatePR{Title: "compass-live-pr-" + newRunID(), Body: "stamped body", HeadRef: head, BaseRef: "main", Draft: true}
				got, err := createWithBackoff(ctx, func() (PullRequest, error) { return gh.CreatePullRequest(ctx, repo, in) })
				if err != nil {
					t.Fatalf("CreatePullRequest: %v", err)
				}
				t.Cleanup(func() { teardownGitHubPR(t, author, repo, got.Number, head) })
				return fixtureRequest{Op: "create_pull_request", Repo: repo,
					Input: &fixtureInput{Title: in.Title, Body: in.Body, HeadRef: in.HeadRef, BaseRef: in.BaseRef, Draft: in.Draft}}
			}},
		{provider: providerGitHub, name: "get_pull_request", prelude: 0,
			run: func(t *testing.T, rt *recordingRoundTripper) fixtureRequest {
				t.Helper()
				repo, author, _ := requireLive(t)
				ctx := context.Background()
				head := "compass-live-" + newRunID()
				seedHeadBranch(t, ctx, author, repo, head)
				setup := setupGitHub(author)
				pr, err := createWithBackoff(ctx, func() (PullRequest, error) {
					return setup.CreatePullRequest(ctx, repo, CreatePR{
						Title: "compass-live-getpr-" + newRunID(), Body: "raw <!--owner--> pr body", HeadRef: head, BaseRef: "main", Draft: true})
				})
				if err != nil {
					t.Fatalf("CreatePullRequest (setup): %v", err)
				}
				t.Cleanup(func() { teardownGitHubPR(t, author, repo, pr.Number, head) })
				gh := recordingGitHub(author, rt)
				if _, err := gh.GetPullRequest(ctx, repo, pr.Number); err != nil {
					t.Fatalf("GetPullRequest: %v", err)
				}
				return fixtureRequest{Op: "get_pull_request", Repo: repo, Number: pr.Number}
			}},
		{provider: providerGitHub, name: "comment_on_issue", prelude: 0,
			run: func(t *testing.T, rt *recordingRoundTripper) fixtureRequest {
				t.Helper()
				repo, author, _ := requireLive(t)
				ctx := context.Background()
				setup := setupGitHub(author)
				issue, err := createWithBackoff(ctx, func() (Issue, error) {
					return setup.CreateIssue(ctx, repo, CreateIssue{Title: "compass-live-comment-" + newRunID()})
				})
				if err != nil {
					t.Fatalf("CreateIssue (setup): %v", err)
				}
				t.Cleanup(func() { closeGitHubIssue(t, author, repo, issue.Number) })
				body := "a reply"
				gh := recordingGitHub(author, rt)
				if _, err := createWithBackoff(ctx, func() (Comment, error) {
					return gh.CommentOnIssue(ctx, repo, issue.Number, body)
				}); err != nil {
					t.Fatalf("CommentOnIssue: %v", err)
				}
				return fixtureRequest{Op: "comment_on_issue", Repo: repo, Number: issue.Number,
					Input: &fixtureInput{Body: body}}
			}},
	}
}

// linearUpdateSpecs is the Linear half of the capture table. create/comment run
// resolveTeamID|resolveIssueID + the actor probe BEFORE the mutation (prelude 2);
// get/list issue reads are single-shot (prelude 0). Each run drives the SAME live
// op its sibling oracle scenario runs, with the same teardown hygiene.
func linearUpdateSpecs() []captureSpec {
	return []captureSpec{
		{provider: providerLinear, name: "create_issue", prelude: 2,
			run: func(t *testing.T, rt *recordingRoundTripper) fixtureRequest {
				t.Helper()
				ts, team := requireLinear(t)
				ctx := context.Background()
				ln := recordingLinear(ts, rt)
				in := CreateIssue{Title: "compass-live-issue-" + newRunID(), Body: "stamped body"}
				got, err := ln.CreateIssue(ctx, team, in)
				if err != nil {
					t.Fatalf("CreateIssue: %v", err)
				}
				t.Cleanup(func() { archiveLinearIssue(t, ln, ts, team, got.Number) })
				return fixtureRequest{Op: "create_issue", Repo: team,
					Input: &fixtureInput{Title: in.Title, Body: in.Body}}
			}},
		{provider: providerLinear, name: "get_issue", prelude: 0,
			run: func(t *testing.T, rt *recordingRoundTripper) fixtureRequest {
				t.Helper()
				ts, team := requireLinear(t)
				ctx := context.Background()
				setup := setupLinear(ts)
				issue, err := setup.CreateIssue(ctx, team, CreateIssue{Title: "compass-live-get-" + newRunID(), Body: "raw <!--owner--> body"})
				if err != nil {
					t.Fatalf("CreateIssue (setup): %v", err)
				}
				t.Cleanup(func() { archiveLinearIssue(t, setup, ts, team, issue.Number) })
				ln := recordingLinear(ts, rt)
				if _, err := ln.GetIssue(ctx, team, issue.Number); err != nil {
					t.Fatalf("GetIssue: %v", err)
				}
				return fixtureRequest{Op: "get_issue", Repo: team, Number: issue.Number}
			}},
		{provider: providerLinear, name: "list_issues", prelude: 0,
			run: func(t *testing.T, rt *recordingRoundTripper) fixtureRequest {
				t.Helper()
				ts, team := requireLinear(t)
				ctx := context.Background()
				setup := setupLinear(ts)
				issue, err := setup.CreateIssue(ctx, team, CreateIssue{Title: "compass-live-list-" + newRunID(), Body: "raw body"})
				if err != nil {
					t.Fatalf("CreateIssue (setup): %v", err)
				}
				t.Cleanup(func() { archiveLinearIssue(t, setup, ts, team, issue.Number) })
				filter := IssueFilter{State: "open"}
				ln := recordingLinear(ts, rt)
				if _, err := ln.ListIssues(ctx, team, filter); err != nil {
					t.Fatalf("ListIssues: %v", err)
				}
				return fixtureRequest{Op: "list_issues", Repo: team,
					Filter: &fixtureFilter{State: filter.State}}
			}},
		{provider: providerLinear, name: "comment_on_issue", prelude: 2,
			run: func(t *testing.T, rt *recordingRoundTripper) fixtureRequest {
				t.Helper()
				ts, team := requireLinear(t)
				ctx := context.Background()
				setup := setupLinear(ts)
				issue, err := setup.CreateIssue(ctx, team, CreateIssue{Title: "compass-live-comment-" + newRunID()})
				if err != nil {
					t.Fatalf("CreateIssue (setup): %v", err)
				}
				t.Cleanup(func() { archiveLinearIssue(t, setup, ts, team, issue.Number) })
				body := "a reply"
				ln := recordingLinear(ts, rt)
				if _, err := ln.CommentOnIssue(ctx, team, issue.Number, body); err != nil {
					t.Fatalf("CommentOnIssue: %v", err)
				}
				return fixtureRequest{Op: "comment_on_issue", Repo: team, Number: issue.Number,
					Input: &fixtureInput{Body: body}}
			}},
	}
}

// recordingGitHub builds a real GitHub client whose transport is wrapped by rt,
// so every exchange is captured while the client behaves exactly as in prod.
func recordingGitHub(ts TokenSource, rt *recordingRoundTripper) *GitHub {
	return NewGitHub(GitHubConfig{Host: "github.com", Token: ts, Client: &http.Client{Transport: rt}})
}

// recordingLinear builds a real Linear client whose transport is wrapped by rt.
func recordingLinear(ts TokenSource, rt *recordingRoundTripper) *Linear {
	return NewLinear(LinearConfig{Token: ts, Client: &http.Client{Transport: rt}})
}

// setupGitHub builds a real GitHub client for scenario SETUP (creating the
// fixture subject before the asserted op). Its exchanges are deliberately NOT
// recorded — only the asserted op's client feeds assembleFixture — so it takes
// no recorder (a nil cfg.Client gets NewGitHub's default).
func setupGitHub(ts TokenSource) *GitHub {
	return NewGitHub(GitHubConfig{Host: "github.com", Token: ts})
}

// setupLinear builds a real Linear client for scenario SETUP; like setupGitHub,
// its exchanges are not recorded.
func setupLinear(ts TokenSource) *Linear {
	return NewLinear(LinearConfig{Token: ts})
}
