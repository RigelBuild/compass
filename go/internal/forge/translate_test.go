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

	if got.GetNumber() != 42 {
		t.Errorf("Number = %d, want 42", got.GetNumber())
	}
	if got.GetTitle() != "a title" {
		t.Errorf("Title = %q, want %q", got.GetTitle(), "a title")
	}
	if got.GetBody() != body {
		t.Errorf("Body = %q, want verbatim %q (must not strip owner header)", got.GetBody(), body)
	}
	if got.GetForgeState() != "open" {
		t.Errorf("ForgeState = %q, want %q", got.GetForgeState(), "open")
	}
	if got.GetUrl() != "https://forge/issues/42" {
		t.Errorf("Url = %q, want %q", got.GetUrl(), "https://forge/issues/42")
	}
	if got.GetForgeAccount() != "octocat" {
		t.Errorf("ForgeAccount = %q, want %q", got.GetForgeAccount(), "octocat")
	}
	if len(got.GetLabels()) != 2 || got.GetLabels()[0] != "bug" || got.GetLabels()[1] != "p1" {
		t.Errorf("Labels = %v, want [bug p1]", got.GetLabels())
	}
	if got.GetAgent() != attr {
		t.Errorf("Agent = %v, want passthrough of attr %v", got.GetAgent(), attr)
	}
}

func TestTranslateIssueLeavesCompassOwnedZero(t *testing.T) {
	got := TranslateIssue(Issue{Number: 1, Title: "t"}, nil)

	if got.GetId() != "" {
		t.Errorf("Id = %q, want empty (projection owns)", got.GetId())
	}
	if got.GetForge() != nil {
		t.Errorf("Forge = %v, want nil (projection owns)", got.GetForge())
	}
	if got.GetRepo() != "" {
		t.Errorf("Repo = %q, want empty (projection owns)", got.GetRepo())
	}
	if got.GetState() != compassv1.IssueState_ISSUE_STATE_UNSPECIFIED {
		t.Errorf("State = %v, want UNSPECIFIED (projection owns)", got.GetState())
	}
	if got.GetPriority() != "" {
		t.Errorf("Priority = %q, want empty (projection owns)", got.GetPriority())
	}
	if got.GetAssignee() != "" {
		t.Errorf("Assignee = %q, want empty (projection owns)", got.GetAssignee())
	}
	if got.GetSummary() != "" {
		t.Errorf("Summary = %q, want empty (projection owns)", got.GetSummary())
	}
	if got.GetBranch() != "" {
		t.Errorf("Branch = %q, want empty (projection owns)", got.GetBranch())
	}
	if got.GetPrs() != nil {
		t.Errorf("Prs = %v, want nil (projection owns)", got.GetPrs())
	}
	if got.GetTracker() != nil {
		t.Errorf("Tracker = %v, want nil (projection owns)", got.GetTracker())
	}
}

func TestTranslateIssueNilAttr(t *testing.T) {
	got := TranslateIssue(Issue{Number: 1}, nil)
	if got.GetAgent() != nil {
		t.Errorf("Agent = %v, want nil for nil attr (non-Compass author)", got.GetAgent())
	}
}

func TestTranslateIssueNilLabelsStayNil(t *testing.T) {
	got := TranslateIssue(Issue{Number: 1}, nil)
	if got.GetLabels() != nil {
		t.Errorf("Labels = %v, want nil for nil source", got.GetLabels())
	}
}

func TestTranslateIssueEmptyLabelsBecomeNil(t *testing.T) {
	got := TranslateIssue(Issue{Number: 1, Labels: []string{}}, nil)
	if got.GetLabels() != nil {
		t.Errorf("Labels = %v (len %d), want nil for empty-but-non-nil source", got.GetLabels(), len(got.GetLabels()))
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
	if got.GetNumber() != math.MaxUint32 {
		t.Errorf("Number = %d, want clamp to %d", got.GetNumber(), uint32(math.MaxUint32))
	}
}

func assertPRChanged(t *testing.T, got *compassv1.PullRequest) {
	t.Helper()
	if got.GetChanged() == nil {
		t.Fatal("Changed = nil, want set")
	}
	if got.GetChanged().GetFiles() != 3 || got.GetChanged().GetAdditions() != 10 || got.GetChanged().GetDeletions() != 2 {
		t.Errorf("Changed = %+v, want {Files:3 Additions:10 Deletions:2}", got.GetChanged())
	}
}

func assertPRChecks(t *testing.T, got *compassv1.PullRequest) {
	t.Helper()
	if got.GetChecks() == nil {
		t.Fatal("Checks = nil, want set")
	}
	if got.GetChecks().GetHeadSha() != "abc123" || got.GetChecks().GetState() != "success" {
		t.Errorf("Checks head/state = %q/%q, want abc123/success", got.GetChecks().GetHeadSha(), got.GetChecks().GetState())
	}
	if len(got.GetChecks().GetChecks()) != 1 {
		t.Fatalf("Checks.Checks len = %d, want 1", len(got.GetChecks().GetChecks()))
	}
	c := got.GetChecks().GetChecks()[0]
	if c.GetName() != "ci" || c.GetState() != "success" || c.GetUrl() != "https://ci/1" || !c.GetRequired() {
		t.Errorf("Check = %+v, want {ci success https://ci/1 required}", c)
	}
}

func assertPRReviews(t *testing.T, got *compassv1.PullRequest) {
	t.Helper()
	if len(got.GetReviews()) != 1 {
		t.Fatalf("Reviews len = %d, want 1", len(got.GetReviews()))
	}
	r := got.GetReviews()[0]
	if r.GetAuthor() != "rev" || !r.GetIsBot() || r.GetVerdict() != "approved" || r.GetBody() != "lgtm" {
		t.Errorf("Review = %+v, want {rev bot approved lgtm}", r)
	}
}

func assertPRThreads(t *testing.T, got *compassv1.PullRequest) {
	t.Helper()
	if len(got.GetThreads()) != 1 {
		t.Fatalf("Threads len = %d, want 1", len(got.GetThreads()))
	}
	th := got.GetThreads()[0]
	if th.GetPath() != "file.go" || !th.GetResolved() {
		t.Errorf("Thread = %+v, want path file.go resolved", th)
	}
	if len(th.GetComments()) != 1 {
		t.Fatalf("Thread.Comments len = %d, want 1", len(th.GetComments()))
	}
	tc := th.GetComments()[0]
	if tc.GetAuthor() != "c" || tc.GetIsBot() || tc.GetBody() != "nit" {
		t.Errorf("Comment = %+v, want {c false nit}", tc)
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

	if got.GetNumber() != 7 {
		t.Errorf("Number = %d, want 7", got.GetNumber())
	}
	if got.GetTitle() != "a pr" {
		t.Errorf("Title = %q, want %q", got.GetTitle(), "a pr")
	}
	if got.GetForgeState() != "merged" {
		t.Errorf("ForgeState = %q, want %q", got.GetForgeState(), "merged")
	}
	if got.GetUrl() != "https://forge/pull/7" {
		t.Errorf("Url = %q, want %q", got.GetUrl(), "https://forge/pull/7")
	}
	if got.GetHeadRef() != "feature" {
		t.Errorf("HeadRef = %q, want %q", got.GetHeadRef(), "feature")
	}
	if got.GetBaseRef() != "main" {
		t.Errorf("BaseRef = %q, want %q", got.GetBaseRef(), "main")
	}
	if got.GetForgeAccount() != "octocat" {
		t.Errorf("ForgeAccount = %q, want %q", got.GetForgeAccount(), "octocat")
	}
	if !got.GetDraft() {
		t.Errorf("Draft = %v, want true", got.GetDraft())
	}
	if got.GetAgent() != attr {
		t.Errorf("Agent = %v, want passthrough of attr", got.GetAgent())
	}

	assertPRChanged(t, got)
	assertPRChecks(t, got)
	assertPRReviews(t, got)
	assertPRThreads(t, got)

	// Compass-owned fields stay zero.
	if got.GetForge() != nil {
		t.Errorf("Forge = %v, want nil (caller/projection supplies)", got.GetForge())
	}
	if got.GetRepo() != "" {
		t.Errorf("Repo = %q, want empty (caller/projection supplies)", got.GetRepo())
	}
}

func TestTranslatePullRequestNilAttr(t *testing.T) {
	got := TranslatePullRequest(PullRequest{Number: 1}, nil)
	if got.GetAgent() != nil {
		t.Errorf("Agent = %v, want nil for nil attr", got.GetAgent())
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

	if got.GetHeadSha() != "sha" {
		t.Errorf("HeadSha = %q, want %q", got.GetHeadSha(), "sha")
	}
	if got.GetState() != "failure" {
		t.Errorf("State = %q, want %q", got.GetState(), "failure")
	}
	if len(got.GetChecks()) != 1 {
		t.Fatalf("Checks len = %d, want 1", len(got.GetChecks()))
	}
	c := got.GetChecks()[0]
	if c.GetName() != "lint" || c.GetState() != "failure" || c.GetUrl() != "https://ci/lint" || !c.GetRequired() {
		t.Errorf("Check = %+v, want {lint failure https://ci/lint required}", c)
	}
}

func TestTranslateEmptySlicesYieldNil(t *testing.T) {
	pr := TranslatePullRequest(PullRequest{Number: 1}, nil)
	if pr.GetReviews() != nil {
		t.Errorf("Reviews = %v, want nil for empty source", pr.GetReviews())
	}
	if pr.GetThreads() != nil {
		t.Errorf("Threads = %v, want nil for empty source", pr.GetThreads())
	}
	// A PR with no checks still gets a ChecksSummary (value struct), but its
	// Checks slice must be nil for an empty source.
	if pr.GetChecks() != nil && pr.GetChecks().GetChecks() != nil {
		t.Errorf("Checks.Checks = %v, want nil for empty source", pr.GetChecks().GetChecks())
	}

	cs := TranslateChecks(Checks{HeadSHA: "s"})
	if cs.GetChecks() != nil {
		t.Errorf("ChecksSummary.Checks = %v, want nil for empty source", cs.GetChecks())
	}
}
