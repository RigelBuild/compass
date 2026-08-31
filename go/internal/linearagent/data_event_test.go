package linearagent

// Unit tests for the Linear data-change webhook arm: Issue/Comment payloads ->
// forge.ForgeEvent. Covers the T2 Linear test cycle (design.md:660-674): Issue
// create -> OPENED; Issue update -> STATE iff updatedFrom shows a workflow-state
// change, else UPDATE; Comment create -> COMMENT; the Issue payload's project id
// lands in Project; remove actions -> ok=false; StripOwner applied to bodies.

import (
	"encoding/json"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

func TestParseLinearDataEvent_Issue(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantOK     bool
		wantChange compassv1internal.ForgeNotificationKind
		wantState  string
	}{
		{
			name:       "create -> OPENED",
			body:       `{"type":"Issue","action":"create","data":{"number":42,"url":"iu","team":{"key":"SEA"},"projectId":"proj-1"}}`,
			wantOK:     true,
			wantChange: compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_OPENED,
		},
		{
			name:       "update with state change -> STATE",
			body:       `{"type":"Issue","action":"update","data":{"number":42,"url":"iu","team":{"key":"SEA"},"state":{"type":"completed"}},"updatedFrom":{"stateId":"old-state"}}`,
			wantOK:     true,
			wantChange: compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_STATE,
			wantState:  "closed",
		},
		{
			name:       "update without state change -> UPDATE",
			body:       `{"type":"Issue","action":"update","data":{"number":42,"url":"iu","team":{"key":"SEA"}},"updatedFrom":{"title":"old"}}`,
			wantOK:     true,
			wantChange: compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_UPDATE,
		},
		{
			name:   "remove -> dropped",
			body:   `{"type":"Issue","action":"remove","data":{"number":42,"team":{"key":"SEA"}}}`,
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok, err := ParseLinearDataEvent([]byte(tc.body))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if ev.Provider != compassv1.ForgeProvider_FORGE_PROVIDER_LINEAR {
				t.Errorf("Provider = %v, want LINEAR", ev.Provider)
			}
			if ev.Host != "linear.app" {
				t.Errorf("Host = %q, want linear.app", ev.Host)
			}
			if ev.Repo != "SEA" {
				t.Errorf("Repo = %q, want SEA (team key)", ev.Repo)
			}
			if ev.Kind != compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE {
				t.Errorf("Kind = %v, want ISSUE", ev.Kind)
			}
			if ev.Number != 42 {
				t.Errorf("Number = %d, want 42", ev.Number)
			}
			if ev.Change != tc.wantChange {
				t.Errorf("Change = %v, want %v", ev.Change, tc.wantChange)
			}
			if ev.State != tc.wantState {
				t.Errorf("State = %q, want %q", ev.State, tc.wantState)
			}
		})
	}
}

// TestParseLinearDataEvent_ProjectID asserts an Issue payload's project id lands
// in ForgeEvent.Project (Linear container matching, W2; design.md:663-664).
func TestParseLinearDataEvent_ProjectID(t *testing.T) {
	flat := `{"type":"Issue","action":"create","data":{"number":1,"team":{"key":"SEA"},"projectId":"proj-flat"}}`
	nested := `{"type":"Issue","action":"create","data":{"number":1,"team":{"key":"SEA"},"project":{"id":"proj-nested"}}}`

	ev, _, err := ParseLinearDataEvent([]byte(flat))
	if err != nil {
		t.Fatalf("flat: %v", err)
	}
	if ev.Project != "proj-flat" {
		t.Errorf("flat Project = %q, want proj-flat", ev.Project)
	}

	ev, _, err = ParseLinearDataEvent([]byte(nested))
	if err != nil {
		t.Fatalf("nested: %v", err)
	}
	if ev.Project != "proj-nested" {
		t.Errorf("nested Project = %q, want proj-nested", ev.Project)
	}
}

// TestParseLinearDataEvent_Comment asserts Comment create -> COMMENT with the
// body stripped through forge.StripOwner and the agent claim surfaced.
func TestParseLinearDataEvent_Comment(t *testing.T) {
	stamped, err := forge.StampOwner("real body", forge.Author{AgentHandle: "agent-x", OwnerHandle: "owner-y", SessionID: "sess-1"}, 0)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	bodyBytes, err := json.Marshal(stamped)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(bodyBytes)
	payload := `{"type":"Comment","action":"create","data":{"id":"c1","body":` + body + `,"user":{"displayName":"Alice"},"issue":{"number":7,"url":"iu","team":{"key":"SEA"},"projectId":"proj-1"}}}`

	ev, ok, err := ParseLinearDataEvent([]byte(payload))
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	if ev.Change != compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_COMMENT {
		t.Errorf("Change = %v, want COMMENT", ev.Change)
	}
	if ev.Number != 7 {
		t.Errorf("Number = %d, want 7 (issue number)", ev.Number)
	}
	if ev.Project != "proj-1" {
		t.Errorf("Project = %q, want proj-1", ev.Project)
	}
	if ev.Comment == nil {
		t.Fatal("Comment = nil, want set")
	}
	if ev.Comment.GetBody() != "real body" {
		t.Errorf("body = %q, want %q", ev.Comment.GetBody(), "real body")
	}
	if ev.Comment.GetAgent().GetAgentHandle() != "agent-x" {
		t.Errorf("agent claim = %q, want agent-x", ev.Comment.GetAgent().GetAgentHandle())
	}
	if ev.Comment.GetForgeAccount() != "Alice" {
		t.Errorf("forge_account = %q, want Alice", ev.Comment.GetForgeAccount())
	}
	if ev.Comment.GetCommentKey() != "c1" {
		t.Errorf("comment_key = %q, want c1 (the comment id from the payload)", ev.Comment.GetCommentKey())
	}

	// A non-create comment action is dropped.
	if _, ok, _ := ParseLinearDataEvent([]byte(`{"type":"Comment","action":"update","data":{"id":"c1","issue":{"number":7}}}`)); ok {
		t.Error("comment update ok = true, want false")
	}
}

// TestLinearCommentForgeAccountIsDisplayNameOnly pins the RIG-2732 Fork-1
// producer-symmetry decision (DL-304): the webhook comment producer resolves
// ForgeAccount to displayName ONLY, never a displayName||name fallback, so it
// stays byte-identical with the sweep producer (forge.linearComment.toComment,
// which fetches and reads only displayName). A re-added name fallback would
// diverge the digested SnapshotComment across producers and reintroduce the
// phantom-diff heartbeat on the account field — this test reds if that happens.
func TestLinearCommentForgeAccountIsDisplayNameOnly(t *testing.T) {
	// displayName present -> used.
	withDisplay := `{"type":"Comment","action":"create","data":{"id":"c1","body":"b","user":{"name":"login-name","displayName":"Alice"},"issue":{"number":7,"url":"iu","team":{"key":"RIG"}}}}`
	ev, ok, err := ParseLinearDataEvent([]byte(withDisplay))
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	if got := ev.Comment.GetForgeAccount(); got != "Alice" {
		t.Errorf("forge_account = %q, want Alice (displayName)", got)
	}
	// displayName absent -> forge_account is empty, NOT the name fallback: the
	// sweep producer cannot see name, so the webhook must not either.
	noDisplay := `{"type":"Comment","action":"create","data":{"id":"c2","body":"b","user":{"name":"login-name"},"issue":{"number":7,"url":"iu","team":{"key":"RIG"}}}}`
	ev2, ok2, err2 := ParseLinearDataEvent([]byte(noDisplay))
	if err2 != nil || !ok2 {
		t.Fatalf("parse: ok=%v err=%v", ok2, err2)
	}
	if got := ev2.Comment.GetForgeAccount(); got != "" {
		t.Errorf("forge_account = %q, want \"\" (no displayName||name fallback — producer symmetry)", got)
	}
}
