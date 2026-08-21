//go:build unix

package board

// Default-lane (no database) tests for the IssueProjection's pure mapping edge:
// issueToProto and protoToForgeFields, the ONLY place store.Issue and the wire
// *compassv1.Issue meet. PublishIssueUpdate/Rehydrate hit the store and so live
// in the pgtest suite; these prove the two mapping funcs field-by-field,
// including the compile-time guarantee that the forge-only upsert input carries
// no machinery.

import (
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// TestIssueToProtoMapsForgeAndMachinery pins that a fully-populated store.Issue
// maps to the wire Issue with the enums translated by value, the forge coordinate
// rebuilt, machinery carried through, the agent attribution set, and tracker/prs
// left nil (their producing slices own them).
func TestIssueToProtoMapsForgeAndMachinery(t *testing.T) {
	si := store.Issue{
		ID:            "iss-1",
		ForgeProvider: store.ForgeProviderGitHub,
		ForgeHost:     "github.com",
		Repo:          "RigelBuild/compass",
		Number:        42,
		Title:         "a bug",
		Body:          "it broke",
		ForgeState:    "open",
		URL:           "https://github.com/RigelBuild/compass/issues/42",
		ForgeAccount:  "octocat",
		Labels:        []string{"bug", "p1"},
		AgentHandle:   "agent-smith",
		State:         store.IssueStateInProgress,
		Priority:      "high",
		Assignee:      "agent-1",
		Summary:       "working it",
		Branch:        "fix/42",
	}

	got := issueToProto(si)

	if got.GetId() != "iss-1" {
		t.Errorf("id = %q, want iss-1", got.GetId())
	}
	if got.GetState() != compassv1.IssueState_ISSUE_STATE_IN_PROGRESS {
		t.Errorf("state = %v, want IN_PROGRESS", got.GetState())
	}
	if got.GetForge().GetProvider() != compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB {
		t.Errorf("provider = %v, want GITHUB", got.GetForge().GetProvider())
	}
	if got.GetForge().GetHost() != "github.com" {
		t.Errorf("host = %q, want github.com", got.GetForge().GetHost())
	}
	if got.GetRepo() != "RigelBuild/compass" || got.GetNumber() != 42 {
		t.Errorf("coordinate = %q#%d, want RigelBuild/compass#42", got.GetRepo(), got.GetNumber())
	}
	if got.GetTitle() != "a bug" || got.GetBody() != "it broke" ||
		got.GetForgeState() != "open" || got.GetUrl() != si.URL ||
		got.GetForgeAccount() != "octocat" {
		t.Errorf("forge fields mismatch: %+v", got)
	}
	if got.GetPriority() != "high" || got.GetAssignee() != "agent-1" ||
		got.GetSummary() != "working it" || got.GetBranch() != "fix/42" {
		t.Errorf("machinery fields mismatch: %+v", got)
	}
	if labels := got.GetLabels(); len(labels) != 2 || labels[0] != "bug" || labels[1] != "p1" {
		t.Errorf("labels = %v, want [bug p1]", labels)
	}
	if got.GetAgent().GetAgentHandle() != "agent-smith" {
		t.Errorf("agent_handle = %q, want agent-smith", got.GetAgent().GetAgentHandle())
	}
	if got.GetTracker() != nil {
		t.Errorf("tracker = %v, want nil (its slice owns it)", got.GetTracker())
	}
	if got.GetPrs() != nil {
		t.Errorf("prs = %v, want nil (its slice owns it)", got.GetPrs())
	}
}

// TestIssueToProtoEmptyLabelsAndHumanAuthor pins the empty->nil contract on the
// mapping edge: no labels round-trips as nil (not an empty slice), and a human
// author (empty agent_handle) leaves the AgentAttribution unset.
func TestIssueToProtoEmptyLabelsAndHumanAuthor(t *testing.T) {
	got := issueToProto(store.Issue{
		ID:            "iss-2",
		ForgeProvider: store.ForgeProviderGitHub,
		ForgeHost:     "github.com",
		Repo:          "RigelBuild/compass",
		Number:        7,
	})
	if got.GetLabels() != nil {
		t.Errorf("labels = %v, want nil for no labels", got.GetLabels())
	}
	if got.GetAgent() != nil {
		t.Errorf("agent = %v, want nil for a human author", got.GetAgent())
	}
}

// TestProtoToForgeFieldsDropsMachinery pins that a proto Issue carrying a State +
// assignee (machinery) maps to a forge-only upsert input that carries NONE of it
// — the type has no such field (compile-time), and the coordinate + forge fields
// map through. This proves the ingestion upsert cannot clobber a human-set state.
func TestProtoToForgeFieldsDropsMachinery(t *testing.T) {
	p := &compassv1.Issue{
		Id: "should-be-ignored",
		Forge: &compassv1.ForgeRef{
			Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
			Host:     "github.com",
		},
		Repo:         "RigelBuild/compass",
		Number:       99,
		Title:        "title",
		Body:         "body",
		ForgeState:   "closed",
		Url:          "https://github.com/RigelBuild/compass/issues/99",
		ForgeAccount: "human",
		Labels:       []string{"triage"},
		Agent:        &compassv1.AgentAttribution{AgentHandle: "agent-x"},
		// Machinery — must NOT survive into IssueForgeFields:
		State:    compassv1.IssueState_ISSUE_STATE_IN_PROGRESS,
		Assignee: "agent-1",
		Priority: "high",
		Summary:  "sum",
		Branch:   "br",
	}

	got := protoToForgeFields(p)

	want := store.IssueForgeFields{
		ForgeProvider: store.ForgeProviderGitHub,
		ForgeHost:     "github.com",
		Repo:          "RigelBuild/compass",
		Number:        99,
		Title:         "title",
		Body:          "body",
		ForgeState:    "closed",
		URL:           "https://github.com/RigelBuild/compass/issues/99",
		ForgeAccount:  "human",
		Labels:        []string{"triage"},
		AgentHandle:   "agent-x",
	}
	if got.ForgeProvider != want.ForgeProvider || got.ForgeHost != want.ForgeHost ||
		got.Repo != want.Repo || got.Number != want.Number {
		t.Errorf("coordinate = %+v, want %+v", got, want)
	}
	if got.Title != want.Title || got.Body != want.Body || got.ForgeState != want.ForgeState ||
		got.URL != want.URL || got.ForgeAccount != want.ForgeAccount || got.AgentHandle != want.AgentHandle {
		t.Errorf("forge fields = %+v, want %+v", got, want)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "triage" {
		t.Errorf("labels = %v, want [triage]", got.Labels)
	}
}

// TestRoundTripCoordinate pins that the forge coordinate survives a
// store -> proto -> forge-fields round trip unchanged: the coordinate is the
// idempotency key, so a re-poll addresses the same row.
func TestRoundTripCoordinate(t *testing.T) {
	orig := store.Issue{
		ForgeProvider: store.ForgeProviderForgejo,
		ForgeHost:     "codeberg.org",
		Repo:          "acme/widgets",
		Number:        123,
	}

	back := protoToForgeFields(issueToProto(orig))

	if back.ForgeProvider != orig.ForgeProvider || back.ForgeHost != orig.ForgeHost ||
		back.Repo != orig.Repo || back.Number != orig.Number {
		t.Errorf("round-trip coordinate = {%v %q %q %d}, want {%v %q %q %d}",
			back.ForgeProvider, back.ForgeHost, back.Repo, back.Number,
			orig.ForgeProvider, orig.ForgeHost, orig.Repo, orig.Number)
	}
}

// recordPublishTimeout bounds the live-fan-out wait; a safety net, never a sleep.
const recordPublishTimeout = 2 * time.Second

// TestRecordAndPublishFansAndRecordsCommittedState pins the STATE-ONLY
// record+publish the write-path executor drives after it has committed and read
// a transition back: a subscriber registered before the call receives the
// committed issue as the issue=16 variant, AND a later Snapshot reads the same
// recorded state. No store is touched (nil store here) — RecordAndPublish maps
// the passed committed row and only records + fans it. A regression that reached
// for the store would nil-panic; one that skipped the map-record would leave the
// Snapshot empty.
func TestRecordAndPublishFansAndRecordsCommittedState(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	p := NewIssueProjection(bus, nil) // nil store: RecordAndPublish must not touch it

	sub, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	committed := store.Issue{
		ID:            "iss-1",
		ForgeProvider: store.ForgeProviderGitHub,
		ForgeHost:     "github.com",
		Repo:          "RigelBuild/compass",
		Number:        42,
		State:         store.IssueStateInProgress,
	}
	p.RecordAndPublish(committed)

	select {
	case e, ok := <-sub.Live:
		if !ok {
			t.Fatal("live channel closed before an event arrived")
		}
		got := e.Payload.GetIssue()
		if got == nil {
			t.Fatalf("live event carried a non-Issue payload: %v", e.Payload)
		}
		if got.GetId() != "iss-1" || got.GetState() != compassv1.IssueState_ISSUE_STATE_IN_PROGRESS {
			t.Fatalf("fanned issue = %s/%v, want iss-1/IN_PROGRESS", got.GetId(), got.GetState())
		}
	case <-time.After(recordPublishTimeout):
		t.Fatal("timed out waiting for the fanned issue")
	}

	snap := p.Snapshot()
	if len(snap) != 1 || snap[0].GetId() != "iss-1" ||
		snap[0].GetState() != compassv1.IssueState_ISSUE_STATE_IN_PROGRESS {
		t.Fatalf("Snapshot = %+v, want one iss-1/IN_PROGRESS entry", snap)
	}
}
