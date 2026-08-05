package forge

// Contracts for the pure forge→canonical mappers (S1b). Each test defends an
// observable mapping contract and must fail on a plausible bug: every
// forge-sourced field lands in the right canonical field, attribution is passed
// through untouched, bodies are copied VERBATIM (no owner-header stripping —
// the mapper is pure), Compass-owned/projection fields stay zero, the uint64→
// uint32 number narrow clamps at the boundary, and empty inputs yield nil
// slices. Stdlib testing only — matching provider_test.go / owner_test.go.

import (
	"math"
	"testing"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
)

func TestTranslateIssueRoundTrip(t *testing.T) {
	attr := &compassv1.AgentAttribution{AgentHandle: "agent-x"}
	// A body carrying an owner-header-looking prefix: a PURE mapper copies it
	// verbatim and must NOT strip it.
	body := "<!-- compass-owner: agent-x -->\nthe issue body"
	in := Issue{
		Number:       42,
		Title:        "a title",
		Body:         body,
		State:        "open",
		URL:          "https://forge/issues/42",
		ForgeAccount: "octocat",
		Labels:       []string{"bug", "p1"},
	}

	got := TranslateIssue(in, attr)

	if got.Number != 42 {
		t.Errorf("Number = %d, want 42", got.Number)
	}
	if got.Title != "a title" {
		t.Errorf("Title = %q, want %q", got.Title, "a title")
	}
	if got.Body != body {
		t.Errorf("Body = %q, want verbatim %q (must not strip owner header)", got.Body, body)
	}
	if got.ForgeState != "open" {
		t.Errorf("ForgeState = %q, want %q", got.ForgeState, "open")
	}
	if got.Url != "https://forge/issues/42" {
		t.Errorf("Url = %q, want %q", got.Url, "https://forge/issues/42")
	}
	if got.ForgeAccount != "octocat" {
		t.Errorf("ForgeAccount = %q, want %q", got.ForgeAccount, "octocat")
	}
	if len(got.Labels) != 2 || got.Labels[0] != "bug" || got.Labels[1] != "p1" {
		t.Errorf("Labels = %v, want [bug p1]", got.Labels)
	}
	if got.Agent != attr {
		t.Errorf("Agent = %v, want passthrough of attr %v", got.Agent, attr)
	}
}

func TestTranslateIssueLeavesCompassOwnedZero(t *testing.T) {
	got := TranslateIssue(Issue{Number: 1, Title: "t"}, nil)

	if got.Id != "" {
		t.Errorf("Id = %q, want empty (projection owns)", got.Id)
	}
	if got.Forge != nil {
		t.Errorf("Forge = %v, want nil (projection owns)", got.Forge)
	}
	if got.Repo != "" {
		t.Errorf("Repo = %q, want empty (projection owns)", got.Repo)
	}
	if got.State != compassv1.IssueState_ISSUE_STATE_UNSPECIFIED {
		t.Errorf("State = %v, want UNSPECIFIED (projection owns)", got.State)
	}
	if got.Priority != "" {
		t.Errorf("Priority = %q, want empty (projection owns)", got.Priority)
	}
	if got.Assignee != "" {
		t.Errorf("Assignee = %q, want empty (projection owns)", got.Assignee)
	}
	if got.Summary != "" {
		t.Errorf("Summary = %q, want empty (projection owns)", got.Summary)
	}
	if got.Branch != "" {
		t.Errorf("Branch = %q, want empty (projection owns)", got.Branch)
	}
	if got.Prs != nil {
		t.Errorf("Prs = %v, want nil (projection owns)", got.Prs)
	}
	if got.Tracker != nil {
		t.Errorf("Tracker = %v, want nil (projection owns)", got.Tracker)
	}
}

func TestTranslateIssueNilAttr(t *testing.T) {
	got := TranslateIssue(Issue{Number: 1}, nil)
	if got.Agent != nil {
		t.Errorf("Agent = %v, want nil for nil attr (non-Compass author)", got.Agent)
	}
}

func TestTranslateIssueNilLabelsStayNil(t *testing.T) {
	got := TranslateIssue(Issue{Number: 1}, nil)
	if got.Labels != nil {
		t.Errorf("Labels = %v, want nil for nil source", got.Labels)
	}
}

func TestTranslateIssueEmptyLabelsBecomeNil(t *testing.T) {
	got := TranslateIssue(Issue{Number: 1, Labels: []string{}}, nil)
	if got.Labels != nil {
		t.Errorf("Labels = %v (len %d), want nil for empty-but-non-nil source", got.Labels, len(got.Labels))
	}
}

func TestNarrowNumber(t *testing.T) {
	tests := []struct {
		name string
		in   uint64
		want uint32
	}{
		{"zero", 0, 0},
		{"normal", 12345, 12345},
		{"max uint32 exact", math.MaxUint32, math.MaxUint32},
		{"over max clamps", math.MaxUint32 + 1, math.MaxUint32},
		{"max uint64 clamps", math.MaxUint64, math.MaxUint32},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := narrowNumber(tt.in); got != tt.want {
				t.Errorf("narrowNumber(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestTranslateIssueNumberNarrowsThroughMapper(t *testing.T) {
	got := TranslateIssue(Issue{Number: math.MaxUint32 + 1}, nil)
	if got.Number != math.MaxUint32 {
		t.Errorf("Number = %d, want clamp to %d", got.Number, uint32(math.MaxUint32))
	}
}

func TestTranslatePullRequestRoundTrip(t *testing.T) {
	attr := &compassv1.AgentAttribution{AgentHandle: "agent-pr"}
	in := PullRequest{
		Number:       7,
		Title:        "a pr",
		Body:         "<!-- compass-owner: agent-pr -->\npr body",
		State:        "merged",
		URL:          "https://forge/pull/7",
		HeadRef:      "feature",
		BaseRef:      "main",
		ForgeAccount: "octocat",
		Draft:        true,
		Changed:      ChangedStats{Files: 3, Additions: 10, Deletions: 2},
		Checks: Checks{
			HeadSHA: "abc123",
			State:   "success",
			Checks:  []Check{{Name: "ci", State: "success", URL: "https://ci/1", Required: true}},
		},
		Reviews: []Review{{Author: "rev", IsBot: true, Verdict: "approved", Body: "lgtm"}},
		Threads: []ReviewThread{{
			Path:     "file.go",
			Resolved: true,
			Comments: []ThreadComment{{Author: "c", IsBot: false, Body: "nit"}},
		}},
	}

	got := TranslatePullRequest(in, attr)

	if got.Number != 7 {
		t.Errorf("Number = %d, want 7", got.Number)
	}
	if got.Title != "a pr" {
		t.Errorf("Title = %q, want %q", got.Title, "a pr")
	}
	if got.ForgeState != "merged" {
		t.Errorf("ForgeState = %q, want %q", got.ForgeState, "merged")
	}
	if got.Url != "https://forge/pull/7" {
		t.Errorf("Url = %q, want %q", got.Url, "https://forge/pull/7")
	}
	if got.HeadRef != "feature" {
		t.Errorf("HeadRef = %q, want %q", got.HeadRef, "feature")
	}
	if got.BaseRef != "main" {
		t.Errorf("BaseRef = %q, want %q", got.BaseRef, "main")
	}
	if got.ForgeAccount != "octocat" {
		t.Errorf("ForgeAccount = %q, want %q", got.ForgeAccount, "octocat")
	}
	if !got.Draft {
		t.Errorf("Draft = %v, want true", got.Draft)
	}
	if got.Agent != attr {
		t.Errorf("Agent = %v, want passthrough of attr", got.Agent)
	}

	// Nested Changed.
	if got.Changed == nil {
		t.Fatal("Changed = nil, want set")
	}
	if got.Changed.Files != 3 || got.Changed.Additions != 10 || got.Changed.Deletions != 2 {
		t.Errorf("Changed = %+v, want {Files:3 Additions:10 Deletions:2}", got.Changed)
	}

	// Nested Checks.
	if got.Checks == nil {
		t.Fatal("Checks = nil, want set")
	}
	if got.Checks.HeadSha != "abc123" || got.Checks.State != "success" {
		t.Errorf("Checks head/state = %q/%q, want abc123/success", got.Checks.HeadSha, got.Checks.State)
	}
	if len(got.Checks.Checks) != 1 {
		t.Fatalf("Checks.Checks len = %d, want 1", len(got.Checks.Checks))
	}
	c := got.Checks.Checks[0]
	if c.Name != "ci" || c.State != "success" || c.Url != "https://ci/1" || !c.Required {
		t.Errorf("Check = %+v, want {ci success https://ci/1 required}", c)
	}

	// Nested Reviews.
	if len(got.Reviews) != 1 {
		t.Fatalf("Reviews len = %d, want 1", len(got.Reviews))
	}
	r := got.Reviews[0]
	if r.Author != "rev" || !r.IsBot || r.Verdict != "approved" || r.Body != "lgtm" {
		t.Errorf("Review = %+v, want {rev bot approved lgtm}", r)
	}

	// Nested Threads + comments.
	if len(got.Threads) != 1 {
		t.Fatalf("Threads len = %d, want 1", len(got.Threads))
	}
	th := got.Threads[0]
	if th.Path != "file.go" || !th.Resolved {
		t.Errorf("Thread = %+v, want path file.go resolved", th)
	}
	if len(th.Comments) != 1 {
		t.Fatalf("Thread.Comments len = %d, want 1", len(th.Comments))
	}
	tc := th.Comments[0]
	if tc.Author != "c" || tc.IsBot || tc.Body != "nit" {
		t.Errorf("Comment = %+v, want {c false nit}", tc)
	}

	// Compass-owned fields stay zero.
	if got.Forge != nil {
		t.Errorf("Forge = %v, want nil (caller/projection supplies)", got.Forge)
	}
	if got.Repo != "" {
		t.Errorf("Repo = %q, want empty (caller/projection supplies)", got.Repo)
	}
}

func TestTranslatePullRequestNilAttr(t *testing.T) {
	got := TranslatePullRequest(PullRequest{Number: 1}, nil)
	if got.Agent != nil {
		t.Errorf("Agent = %v, want nil for nil attr", got.Agent)
	}
}

func TestTranslateChecksRoundTrip(t *testing.T) {
	in := Checks{
		HeadSHA: "sha",
		State:   "failure",
		Checks: []Check{
			{Name: "lint", State: "failure", URL: "https://ci/lint", Required: true},
		},
	}

	got := TranslateChecks(in)

	if got.HeadSha != "sha" {
		t.Errorf("HeadSha = %q, want %q", got.HeadSha, "sha")
	}
	if got.State != "failure" {
		t.Errorf("State = %q, want %q", got.State, "failure")
	}
	if len(got.Checks) != 1 {
		t.Fatalf("Checks len = %d, want 1", len(got.Checks))
	}
	c := got.Checks[0]
	if c.Name != "lint" || c.State != "failure" || c.Url != "https://ci/lint" || !c.Required {
		t.Errorf("Check = %+v, want {lint failure https://ci/lint required}", c)
	}
}

func TestTranslateEmptySlicesYieldNil(t *testing.T) {
	pr := TranslatePullRequest(PullRequest{Number: 1}, nil)
	if pr.Reviews != nil {
		t.Errorf("Reviews = %v, want nil for empty source", pr.Reviews)
	}
	if pr.Threads != nil {
		t.Errorf("Threads = %v, want nil for empty source", pr.Threads)
	}
	// A PR with no checks still gets a ChecksSummary (value struct), but its
	// Checks slice must be nil for an empty source.
	if pr.Checks != nil && pr.Checks.Checks != nil {
		t.Errorf("Checks.Checks = %v, want nil for empty source", pr.Checks.Checks)
	}

	cs := TranslateChecks(Checks{HeadSHA: "s"})
	if cs.Checks != nil {
		t.Errorf("ChecksSummary.Checks = %v, want nil for empty source", cs.Checks)
	}
}
