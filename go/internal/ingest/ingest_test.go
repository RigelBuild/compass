package ingest

import (
	"context"
	"errors"
	"strings"
	"testing"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/forge"
)

// recordingSink records every Issue it receives and can be scripted to fail on
// the Nth PublishIssueUpdate call (1-based; 0 = never fail).
type recordingSink struct {
	got     []*compassv1.Issue
	failOn  int
	failErr error
}

func (s *recordingSink) PublishIssueUpdate(_ context.Context, issue *compassv1.Issue) error {
	s.got = append(s.got, issue)
	if s.failOn != 0 && len(s.got) == s.failOn {
		return s.failErr
	}
	return nil
}

func testForgeRef() *compassv1.ForgeRef {
	return &compassv1.ForgeRef{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
		Host:     "github.com",
	}
}

// newHarness wires a real FakeProvider scripted with issues and a recording
// sink into an Ingester.
func newHarness(t *testing.T, issues []forge.Issue) (*forge.FakeProvider, *recordingSink, *Ingester) {
	t.Helper()
	p := forge.NewFakeProvider("gh")
	p.ListIssuesResult = issues
	sink := &recordingSink{}
	in := NewIngester(p, sink, testForgeRef())
	return p, sink, in
}

func stamped(t *testing.T, body string, author forge.Author) string {
	t.Helper()
	out, err := forge.StampOwner(body, author, 0)
	if err != nil {
		t.Fatalf("StampOwner: %v", err)
	}
	return out
}

func TestStripsOwnerHeaderFromBody(t *testing.T) {
	body := stamped(t, "real body", forge.Author{AgentHandle: "atlas", OwnerHandle: "matt", SessionID: "s1"})
	_, sink, in := newHarness(t, []forge.Issue{{Number: 1, Body: body}})

	if err := in.Ingest(context.Background(), "owner/repo"); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(sink.got) != 1 {
		t.Fatalf("sink got %d issues, want 1", len(sink.got))
	}
	gotBody := sink.got[0].GetBody()
	if gotBody != "real body" {
		t.Fatalf("Body = %q, want %q", gotBody, "real body")
	}
	if strings.Contains(gotBody, "🧭") {
		t.Errorf("Body still carries the 🧭 attribution line: %q", gotBody)
	}
	if strings.Contains(gotBody, "compass:owner") {
		t.Errorf("Body still carries the compass:owner sentinel: %q", gotBody)
	}
	if strings.Contains(gotBody, "---") {
		t.Errorf("Body still carries the --- rule: %q", gotBody)
	}
}

func TestParsesAttribution(t *testing.T) {
	body := stamped(t, "real body", forge.Author{AgentHandle: "atlas", OwnerHandle: "matt", SessionID: "s1"})
	_, sink, in := newHarness(t, []forge.Issue{{Number: 1, Body: body}})

	if err := in.Ingest(context.Background(), "owner/repo"); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if got := sink.got[0].GetAgent().GetAgentHandle(); got != "atlas" {
		t.Fatalf("Agent.AgentHandle = %q, want %q", got, "atlas")
	}
}

func TestHumanAuthorNoAttribution(t *testing.T) {
	_, sink, in := newHarness(t, []forge.Issue{{Number: 1, Body: "just a plain human body"}})

	if err := in.Ingest(context.Background(), "owner/repo"); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if agent := sink.got[0].GetAgent(); agent != nil {
		t.Fatalf("Agent = %v, want nil for a non-Compass author", agent)
	}
}

func TestDuplicateHeaderNoAttributionStillStripped(t *testing.T) {
	// Two owner headers → StripOwner returns ok=false (refuse to choose between
	// competing claims) but STILL removes every block. Ingestion must yield no
	// attribution yet still hand a scrubbed body to the sink — the ok=false
	// contract at the layer that consumes it.
	first := stamped(t, "real body", forge.Author{AgentHandle: "atlas", OwnerHandle: "matt", SessionID: "s1"})
	second := stamped(t, "second", forge.Author{AgentHandle: "nomad", OwnerHandle: "alex", SessionID: "s2"})
	_, sink, in := newHarness(t, []forge.Issue{{Number: 1, Body: first + "\n" + second}})

	if err := in.Ingest(context.Background(), "owner/repo"); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if agent := sink.got[0].GetAgent(); agent != nil {
		t.Fatalf("Agent = %v, want nil for a duplicate-header (ok=false) body", agent)
	}
	gotBody := sink.got[0].GetBody()
	if strings.Contains(gotBody, "compass:owner") || strings.Contains(gotBody, "🧭") {
		t.Errorf("Body still carries owner-header scaffold: %q", gotBody)
	}
}

func TestStampsForgeCoordinate(t *testing.T) {
	_, sink, in := newHarness(t, []forge.Issue{{Number: 1, Body: "body"}})

	if err := in.Ingest(context.Background(), "owner/repo"); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	forgeRef := sink.got[0].GetForge()
	if forgeRef.GetProvider() != compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB {
		t.Errorf("Forge.Provider = %v, want GITHUB", forgeRef.GetProvider())
	}
	if forgeRef.GetHost() != "github.com" {
		t.Errorf("Forge.Host = %q, want %q", forgeRef.GetHost(), "github.com")
	}
	if got := sink.got[0].GetRepo(); got != "owner/repo" {
		t.Errorf("Repo = %q, want %q", got, "owner/repo")
	}
}

func TestTranslatesForgeFields(t *testing.T) {
	raw := forge.Issue{
		Number: 42,
		Title:  "a bug",
		Body:   "body",
		State:  "open",
		URL:    "https://github.com/owner/repo/issues/42",
		Labels: []string{"bug", "p1"},
	}
	_, sink, in := newHarness(t, []forge.Issue{raw})

	if err := in.Ingest(context.Background(), "owner/repo"); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	got := sink.got[0]
	if got.GetNumber() != 42 {
		t.Errorf("Number = %d, want 42", got.GetNumber())
	}
	if got.GetTitle() != "a bug" {
		t.Errorf("Title = %q, want %q", got.GetTitle(), "a bug")
	}
	if got.GetForgeState() != "open" {
		t.Errorf("ForgeState = %q, want %q", got.GetForgeState(), "open")
	}
	if got.GetUrl() != raw.URL {
		t.Errorf("Url = %q, want %q", got.GetUrl(), raw.URL)
	}
	if strings.Join(got.GetLabels(), ",") != "bug,p1" {
		t.Errorf("Labels = %v, want [bug p1]", got.GetLabels())
	}
}

func TestSinksEveryIssue(t *testing.T) {
	issues := []forge.Issue{
		{Number: 1, Title: "one", Body: "b1"},
		{Number: 2, Title: "two", Body: "b2"},
		{Number: 3, Title: "three", Body: "b3"},
	}
	_, sink, in := newHarness(t, issues)

	if err := in.Ingest(context.Background(), "owner/repo"); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(sink.got) != 3 {
		t.Fatalf("sink got %d issues, want 3", len(sink.got))
	}
	for i, want := range []uint32{1, 2, 3} {
		if got := sink.got[i].GetNumber(); got != want {
			t.Errorf("issue[%d].Number = %d, want %d (order not preserved)", i, got, want)
		}
	}
}

var errBoom = errors.New("boom")

func TestProviderErrorPropagates(t *testing.T) {
	p, sink, in := newHarness(t, []forge.Issue{{Number: 1, Body: "b"}})
	p.SetError("ListIssues", errBoom)

	err := in.Ingest(context.Background(), "owner/repo")
	if !errors.Is(err, errBoom) {
		t.Fatalf("Ingest err = %v, want errors.Is(errBoom)", err)
	}
	if len(sink.got) != 0 {
		t.Fatalf("sink got %d issues, want 0 on a provider error", len(sink.got))
	}
}

func TestSinkErrorPropagates(t *testing.T) {
	issues := []forge.Issue{
		{Number: 1, Body: "b1"},
		{Number: 2, Body: "b2"},
		{Number: 3, Body: "b3"},
	}
	p := forge.NewFakeProvider("gh")
	p.ListIssuesResult = issues
	sink := &recordingSink{failOn: 2, failErr: errBoom}
	in := NewIngester(p, sink, testForgeRef())

	err := in.Ingest(context.Background(), "owner/repo")
	if !errors.Is(err, errBoom) {
		t.Fatalf("Ingest err = %v, want errors.Is(errBoom)", err)
	}
	// STOP on first sink error: the 3rd issue is never sinked.
	if len(sink.got) != 2 {
		t.Fatalf("sink got %d issues, want 2 (stopped after the failing 2nd)", len(sink.got))
	}
}

func TestEmptyRepoIngestsNothing(t *testing.T) {
	_, sink, in := newHarness(t, nil)

	if err := in.Ingest(context.Background(), "owner/repo"); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(sink.got) != 0 {
		t.Fatalf("sink got %d issues, want 0", len(sink.got))
	}
}
