package forge

// Golden-fixture replay (leg 1 of the forge integration-testing record,
// docs/designs/product/compass-forge-integration-testing/design.md §T1). A
// plain, untagged test that replays committed request/response fixtures from
// testdata/<provider>/ through the existing scriptedRoundTripper stub against
// the REAL forge clients, asserting BOTH halves of each exchange: the request
// our client emits (method, path, query, non-auth headers, body) AND the
// decoded domain value both match the captured fixture. Zero network, zero
// credentials.
//
// The schema (fixture/fixtureRequest/fixtureResponse), loadFixtures,
// writeFixture, and the -update flag are the seam T2's //go:build livegithub
// suite imports to run the SAME scenarios live and regenerate fixtures; the
// live-capture body of the -update path is T2's concern. T1 delivers and tests
// the replay/assert path (and writeFixture's round-trip).
//
// context.Background() here is the test root — the sanctioned F-ttsr exemption
// (mirrors github_test.go / linear_test.go).

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// update regenerates the testdata fixtures from the live throwaway repo. The
// live-capture path is T2's (//go:build livegithub) concern; T1 defines the
// flag so the seam is stable and exercises only writeFixture's serialization.
var update = flag.Bool("update", false, "regenerate testdata fixtures from the live throwaway repo (requires LIVEGITHUB_* env)")

// providerGitHub and providerLinear name the two forge providers the golden
// suite dispatches on. They live in one place (shared across the forge test
// files) so the identifiers are not repeated string literals (goconst).
const (
	providerGitHub = "github"
	providerLinear = "linear"
)

// fixture is one captured forge exchange: the request our client is expected to
// emit and the scripted response(s) to replay.
type fixture struct {
	Name     string          `json:"name"`
	Request  fixtureRequest  `json:"request"`  // op + inputs, and the expected emitted request
	Response fixtureResponse `json:"response"` // scripted response(s) + the expected decoded value
}

// fixtureRequest carries both the operation to drive (Op + coordinates/inputs)
// and the HTTP-level expectation for the request the client emits. Authorization
// is deliberately absent — a captured fixture never holds a real token, and the
// replay assertion skips it.
type fixtureRequest struct {
	// Op selects the client method to invoke (see dispatch).
	Op string `json:"op"`
	// Repo is the forge coordinate: "owner/name" (GitHub) or team key (Linear).
	Repo string `json:"repo"`
	// Number is the issue/PR number for the *_on_issue / get_* ops.
	Number uint64 `json:"number,omitempty"`
	// Input carries create-op inputs (issue/PR/comment).
	Input *fixtureInput `json:"input,omitempty"`
	// Filter narrows list_issues.
	Filter *fixtureFilter `json:"filter,omitempty"`

	// Expected emitted request (auth redacted):
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   map[string]string `json:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

// fixtureInput is the create-op input payload (a superset across issue/PR/
// comment; only the fields an op reads are populated).
type fixtureInput struct {
	Title   string   `json:"title,omitempty"`
	Body    string   `json:"body,omitempty"`
	Labels  []string `json:"labels,omitempty"`
	HeadRef string   `json:"headRef,omitempty"`
	BaseRef string   `json:"baseRef,omitempty"`
	Draft   bool     `json:"draft,omitempty"`
}

// fixtureFilter is the list_issues narrowing.
type fixtureFilter struct {
	State  string   `json:"state,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

// fixtureStep is one scripted HTTP response served by the replay transport.
type fixtureStep struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

// fixtureResponse is the scripted response for the asserted request plus, for
// multi-request operations, the helper responses served before (Prelude) and
// after (Extra) it, and the expected decoded domain value (Want). Prelude
// covers a provider's resolve/probe round-trips (Linear team-id + actor probe);
// Extra covers a composite read's follow-on fetches (GitHub GetPullRequest's
// reviews + checks legs). The asserted request is the one at index len(Prelude).
type fixtureResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"` // verbatim provider JSON
	Prelude []fixtureStep     `json:"prelude,omitempty"`
	Extra   []fixtureStep     `json:"extra,omitempty"`
	Want    json.RawMessage   `json:"want"` // expected decoded forge domain value
}

// loadFixtures reads every *.json in dir (one fixture per file) and returns them
// sorted by file name for deterministic order. A malformed or unreadable file is
// a hard failure — a golden that does not parse cannot be silently skipped.
func loadFixtures(t *testing.T, dir string) []fixture {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatalf("glob fixtures in %s: %v", dir, err)
	}
	sort.Strings(paths)
	fixtures := make([]fixture, 0, len(paths))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read fixture %s: %v", p, err)
		}
		var f fixture
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("malformed fixture %s: %v", p, err)
		}
		fixtures = append(fixtures, f)
	}
	return fixtures
}

// writeFixture serializes one fixture to dir/<name>.json with indentation. It is
// the sink of T2's -update live-capture path; T1 uses it only to prove the
// schema round-trips through loadFixtures.
func writeFixture(t *testing.T, dir string, f fixture) {
	t.Helper()
	// Tab indent, matching the repo Biome formatter (indentStyle: tab) that
	// lints testdata JSON — so a regenerated fixture stays gate-clean.
	raw, err := json.MarshalIndent(f, "", "\t")
	if err != nil {
		t.Fatalf("marshal fixture %q: %v", f.Name, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, f.Name+".json")
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}

// TestGoldenFixtures replays every committed fixture for both providers,
// asserting the emitted request and decoded value each match the capture.
func TestGoldenFixtures(t *testing.T) {
	if *update {
		// Live capture requires the throwaway-repo credentials and belongs to
		// T2's //go:build livegithub suite; the untagged leg cannot regenerate.
		t.Skip("-update live capture is the //go:build livegithub suite's job (T2)")
	}
	for _, provider := range []string{providerGitHub, providerLinear} {
		t.Run(provider, func(t *testing.T) {
			dir := filepath.Join("testdata", provider)
			fixtures := loadFixtures(t, dir)
			if len(fixtures) == 0 {
				t.Fatalf("no fixtures under %s", dir)
			}
			for _, f := range fixtures {
				t.Run(f.Name, func(t *testing.T) {
					replayFixture(t, provider, f)
				})
			}
		})
	}
}

// replayFixture drives one fixture through a scripted transport and asserts both
// halves.
func replayFixture(t *testing.T, provider string, f fixture) {
	t.Helper()

	// Script the transport: prelude responses, the asserted-request response,
	// then extra responses (composite reads), in wire order.
	responses := make([]scriptedResponse, 0, len(f.Response.Prelude)+1+len(f.Response.Extra))
	for _, s := range f.Response.Prelude {
		responses = append(responses, toScripted(s))
	}
	responses = append(responses, scriptedResponse{
		status:  f.Response.Status,
		body:    string(f.Response.Body),
		headers: f.Response.Headers,
	})
	for _, s := range f.Response.Extra {
		responses = append(responses, toScripted(s))
	}
	rt := &scriptedRoundTripper{responses: responses}
	ts := &fakeTokenSource{token: "test-token"}

	got := invoke(t, provider, rt, ts, f.Request)

	// (a) Request half: the request the client emitted at index len(Prelude).
	idx := len(f.Response.Prelude)
	if idx >= len(rt.requests) {
		t.Fatalf("op %q emitted %d requests, want at least %d", f.Request.Op, len(rt.requests), idx+1)
	}
	assertRequest(t, rt.requests[idx], f.Request)

	// (b) Value half: the decoded domain value matches the captured expectation.
	assertJSONEqual(t, "decoded value", mustMarshal(t, got), f.Response.Want)
}

// invoke calls the client method the fixture names and returns the decoded
// domain value for the value-half assertion.
func invoke(t *testing.T, provider string, rt *scriptedRoundTripper, ts *fakeTokenSource, req fixtureRequest) any {
	t.Helper()
	ctx := context.Background()
	in := req.Input
	if in == nil {
		in = &fixtureInput{}
	}
	var filter IssueFilter
	if req.Filter != nil {
		filter = IssueFilter{State: req.Filter.State, Labels: req.Filter.Labels}
	}

	switch provider {
	case providerGitHub:
		g := newTestGitHub(rt, ts)
		switch req.Op {
		case "create_issue":
			v, err := g.CreateIssue(ctx, req.Repo, CreateIssue{Title: in.Title, Body: in.Body, Labels: in.Labels})
			return must(t, v, err)
		case "comment_on_issue":
			v, err := g.CommentOnIssue(ctx, req.Repo, req.Number, in.Body)
			return must(t, v, err)
		case "get_issue":
			v, err := g.GetIssue(ctx, req.Repo, req.Number)
			return must(t, v, err)
		case "list_issues":
			v, err := g.ListIssues(ctx, req.Repo, filter)
			return must(t, v, err)
		case "create_pull_request":
			v, err := g.CreatePullRequest(ctx, req.Repo, CreatePR{Title: in.Title, Body: in.Body, HeadRef: in.HeadRef, BaseRef: in.BaseRef, Draft: in.Draft})
			return must(t, v, err)
		case "get_pull_request":
			v, err := g.GetPullRequest(ctx, req.Repo, req.Number)
			return must(t, v, err)
		}
	case providerLinear:
		l := newTestLinear(rt, ts, slog.New(&capturingHandler{}))
		switch req.Op {
		case "create_issue":
			v, err := l.CreateIssue(ctx, req.Repo, CreateIssue{Title: in.Title, Body: in.Body, Labels: in.Labels})
			return must(t, v, err)
		case "comment_on_issue":
			v, err := l.CommentOnIssue(ctx, req.Repo, req.Number, in.Body)
			return must(t, v, err)
		case "get_issue":
			v, err := l.GetIssue(ctx, req.Repo, req.Number)
			return must(t, v, err)
		case "list_issues":
			v, err := l.ListIssues(ctx, req.Repo, filter)
			return must(t, v, err)
		}
	}
	t.Fatalf("unknown provider/op: %s/%s", provider, req.Op)
	return nil
}

// assertRequest checks the emitted request against the fixture's expectation:
// method, path, query, and any named non-auth headers; the body is compared as
// semantic JSON when the fixture pins one. Authorization is never asserted.
func assertRequest(t *testing.T, got *http.Request, want fixtureRequest) {
	t.Helper()
	if got.Method != want.Method {
		t.Errorf("method = %s, want %s", got.Method, want.Method)
	}
	if got.URL.Path != want.Path {
		t.Errorf("path = %s, want %s", got.URL.Path, want.Path)
	}
	gotQuery := got.URL.Query()
	for k, v := range want.Query {
		if g := gotQuery.Get(k); g != v {
			t.Errorf("query %q = %q, want %q", k, g, v)
		}
	}
	for k, v := range want.Headers {
		if http.CanonicalHeaderKey(k) == "Authorization" {
			continue // never assert on a token
		}
		if g := got.Header.Get(k); g != v {
			t.Errorf("header %q = %q, want %q", k, g, v)
		}
	}
	if len(want.Body) > 0 {
		assertJSONEqual(t, "request body", []byte(readReqBody(t, got)), want.Body)
	}
}

// toScripted converts a JSON fixture step to the transport's scriptedResponse.
func toScripted(s fixtureStep) scriptedResponse {
	return scriptedResponse{status: s.Status, body: string(s.Body), headers: s.Headers}
}

// assertJSONEqual compares two JSON documents structurally (key order and
// whitespace irrelevant), so a golden stays readable without pinning field
// order to the client's marshaler.
func assertJSONEqual(t *testing.T, what string, got, want []byte) {
	t.Helper()
	var gv, wv any
	if err := json.Unmarshal(got, &gv); err != nil {
		t.Fatalf("%s: unmarshal actual: %v (%s)", what, err, got)
	}
	if err := json.Unmarshal(want, &wv); err != nil {
		t.Fatalf("%s: unmarshal expected: %v (%s)", what, err, want)
	}
	if !reflect.DeepEqual(gv, wv) {
		t.Errorf("%s mismatch:\n got:  %s\n want: %s", what, got, want)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal decoded value: %v", err)
	}
	return raw
}

// must fails the test on a client-method error and returns the value for the
// value-half assertion.
func must[T any](t *testing.T, v T, err error) T {
	t.Helper()
	if err != nil {
		t.Fatalf("client method: %v", err)
	}
	return v
}

// TestFixtureRoundTrip proves the schema round-trips through writeFixture and
// loadFixtures (the seam T2's -update path writes through).
func TestFixtureRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := fixture{
		Name: "roundtrip",
		Request: fixtureRequest{
			Op:     "get_issue",
			Repo:   "org/repo",
			Number: 7,
			Method: http.MethodGet,
			Path:   "/repos/org/repo/issues/7",
		},
		Response: fixtureResponse{
			Status: 200,
			Body:   json.RawMessage(`{"number":7}`),
			Want:   json.RawMessage(`{"Number":7}`),
		},
	}
	writeFixture(t, dir, want)

	got := loadFixtures(t, dir)
	if len(got) != 1 {
		t.Fatalf("loaded %d fixtures, want 1", len(got))
	}
	if got[0].Name != want.Name || got[0].Request.Op != want.Request.Op ||
		got[0].Request.Number != want.Request.Number || got[0].Response.Status != want.Response.Status {
		t.Errorf("round-tripped fixture = %+v, want %+v", got[0], want)
	}
}
