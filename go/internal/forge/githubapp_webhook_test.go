package forge

// Unit tests for the GitHub App webhook ingress: the constant-time signature
// verifier and the (event, body) -> ForgeEvent normalizer. Covers the T2 test
// cycle (design.md:667-674): signature vectors (valid/tampered/missing), every
// Approach event-table row (design.md:131-142) parses to the right
// kind/coordinate/payload, ignored actions -> ok=false, PR-vs-issue comment
// discrimination, check_suite.completed -> HeadSHA set + Checks nil, and
// StripOwner applied at normalize.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

func sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyGitHubSignature(t *testing.T) {
	secret := []byte("s3cr3t")
	body := []byte(`{"hello":"world"}`)
	valid := sign(secret, body)

	cases := []struct {
		name   string
		header string
		body   []byte
		want   bool
	}{
		{"valid", valid, body, true},
		{"tampered body", valid, []byte(`{"hello":"mars"}`), false},
		{"tampered header", "sha256=deadbeef", body, false},
		{"missing prefix", hex.EncodeToString([]byte("x")), body, false},
		{"empty header", "", body, false},
		{"non-hex", "sha256=zzzz", body, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerifyGitHubSignature(secret, tc.body, tc.header); got != tc.want {
				t.Fatalf("VerifyGitHubSignature = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseGitHubEvent_Table(t *testing.T) { //nolint:funlen // one row per Approach event-table entry; splitting the fixtures would scatter the mapping contract
	const (
		issueK = compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE
		prK    = compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST
	)

	cases := []struct {
		name       string
		event      string
		body       string
		wantOK     bool
		wantKind   compassv1internal.ForgeArtifactKind
		wantChange compassv1internal.ForgeNotificationKind
		wantNumber uint64
		wantState  string
	}{
		{
			name:       "issues opened -> OPENED",
			event:      "issues",
			body:       `{"action":"opened","issue":{"number":12,"html_url":"u","state":"open"},"repository":{"full_name":"o/r"}}`,
			wantOK:     true,
			wantKind:   issueK,
			wantChange: compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_OPENED,
			wantNumber: 12,
		},
		{
			name:       "issues closed -> STATE",
			event:      "issues",
			body:       `{"action":"closed","issue":{"number":3,"html_url":"u","state":"closed"},"repository":{"full_name":"o/r"}}`,
			wantOK:     true,
			wantKind:   issueK,
			wantChange: compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_STATE,
			wantNumber: 3,
			wantState:  "closed",
		},
		{
			name:       "issues reopened -> STATE",
			event:      "issues",
			body:       `{"action":"reopened","issue":{"number":5,"html_url":"u","state":"open"},"repository":{"full_name":"o/r"}}`,
			wantOK:     true,
			wantKind:   issueK,
			wantChange: compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_STATE,
			wantNumber: 5,
			wantState:  "open",
		},
		{
			name:       "issues edited -> UPDATE",
			event:      "issues",
			body:       `{"action":"edited","issue":{"number":4,"html_url":"u"},"repository":{"full_name":"o/r"}}`,
			wantOK:     true,
			wantKind:   issueK,
			wantChange: compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_UPDATE,
			wantNumber: 4,
		},
		{
			name:       "pull_request opened -> OPENED",
			event:      "pull_request",
			body:       `{"action":"opened","pull_request":{"number":9,"html_url":"u","state":"open"},"repository":{"full_name":"o/r"}}`,
			wantOK:     true,
			wantKind:   prK,
			wantChange: compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_OPENED,
			wantNumber: 9,
		},
		{
			name:       "pull_request merged -> STATE merged",
			event:      "pull_request",
			body:       `{"action":"closed","pull_request":{"number":9,"html_url":"u","state":"closed","merged":true},"repository":{"full_name":"o/r"}}`,
			wantOK:     true,
			wantKind:   prK,
			wantChange: compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_STATE,
			wantNumber: 9,
			wantState:  "merged",
		},
		{
			name:       "pull_request labeled -> UPDATE",
			event:      "pull_request",
			body:       `{"action":"labeled","pull_request":{"number":9,"html_url":"u"},"repository":{"full_name":"o/r"}}`,
			wantOK:     true,
			wantKind:   prK,
			wantChange: compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_UPDATE,
			wantNumber: 9,
		},
		{
			name:       "pull_request unlabeled -> UPDATE",
			event:      "pull_request",
			body:       `{"action":"unlabeled","pull_request":{"number":9,"html_url":"u"},"repository":{"full_name":"o/r"}}`,
			wantOK:     true,
			wantKind:   prK,
			wantChange: compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_UPDATE,
			wantNumber: 9,
		},
		{
			name:       "pull_request_review submitted -> REVIEW",
			event:      "pull_request_review",
			body:       `{"action":"submitted","pull_request":{"number":9,"html_url":"u"},"review":{"id":77,"html_url":"ru","body":"lgtm","state":"approved","user":{"login":"rev"}},"repository":{"full_name":"o/r"}}`,
			wantOK:     true,
			wantKind:   prK,
			wantChange: compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_REVIEW,
			wantNumber: 9,
			wantState:  "approved",
		},
		{
			name:       "pull_request_review_comment created -> COMMENT",
			event:      "pull_request_review_comment",
			body:       `{"action":"created","pull_request":{"number":9,"html_url":"u"},"comment":{"id":5,"html_url":"cu","body":"nit","user":{"login":"c"}},"repository":{"full_name":"o/r"}}`,
			wantOK:     true,
			wantKind:   prK,
			wantChange: compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_COMMENT,
			wantNumber: 9,
		},
		{
			name:   "issues assigned -> ignored",
			event:  "issues",
			body:   `{"action":"assigned","issue":{"number":1},"repository":{"full_name":"o/r"}}`,
			wantOK: false,
		},
		{
			name:   "unknown event -> ignored",
			event:  "push",
			body:   `{"ref":"refs/heads/main"}`,
			wantOK: false,
		},
		{
			name:   "issue_comment non-created -> ignored",
			event:  "issue_comment",
			body:   `{"action":"deleted","issue":{"number":1},"comment":{"id":1},"repository":{"full_name":"o/r"}}`,
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok, err := ParseGitHubEvent(tc.event, []byte(tc.body))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if ev.Provider != compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB {
				t.Errorf("Provider = %v, want GITHUB", ev.Provider)
			}
			if ev.Host != "github.com" {
				t.Errorf("Host = %q, want github.com", ev.Host)
			}
			if ev.Repo != "o/r" {
				t.Errorf("Repo = %q, want o/r", ev.Repo)
			}
			if ev.Kind != tc.wantKind {
				t.Errorf("Kind = %v, want %v", ev.Kind, tc.wantKind)
			}
			if ev.Change != tc.wantChange {
				t.Errorf("Change = %v, want %v", ev.Change, tc.wantChange)
			}
			if ev.Number != tc.wantNumber {
				t.Errorf("Number = %d, want %d", ev.Number, tc.wantNumber)
			}
			if ev.State != tc.wantState {
				t.Errorf("State = %q, want %q", ev.State, tc.wantState)
			}
		})
	}
}

// TestParseGitHubEvent_CommentPRvsIssue asserts the issue.pull_request marker
// discriminates a PR conversation comment (Kind=PR) from a plain issue comment
// (Kind=ISSUE) on the single issue_comment event (design.md:136, 653-654).
func TestParseGitHubEvent_CommentPRvsIssue(t *testing.T) {
	issueBody := `{"action":"created","issue":{"number":8,"html_url":"iu"},"comment":{"id":2,"html_url":"cu","body":"hi","user":{"login":"c"}},"repository":{"full_name":"o/r"}}`
	prBody := `{"action":"created","issue":{"number":8,"html_url":"iu","pull_request":{"url":"p"}},"comment":{"id":2,"html_url":"cu","body":"hi","user":{"login":"c"}},"repository":{"full_name":"o/r"}}`

	ev, ok, err := ParseGitHubEvent("issue_comment", []byte(issueBody))
	if err != nil || !ok {
		t.Fatalf("issue comment parse: ok=%v err=%v", ok, err)
	}
	if ev.Kind != compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE {
		t.Errorf("issue comment Kind = %v, want ISSUE", ev.Kind)
	}
	if ev.Comment == nil || ev.Comment.GetForgeAccount() != "c" {
		t.Errorf("issue comment ref = %+v, want forge_account c", ev.Comment)
	}

	ev, ok, err = ParseGitHubEvent("issue_comment", []byte(prBody))
	if err != nil || !ok {
		t.Fatalf("pr comment parse: ok=%v err=%v", ok, err)
	}
	if ev.Kind != compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST {
		t.Errorf("pr comment Kind = %v, want PULL_REQUEST", ev.Kind)
	}
}

// TestParseGitHubEvent_CheckSuite asserts check_suite.completed carries HeadSHA
// and a NIL ChecksSummary — the roll-up is the router's fetch, never parse-time
// (design.md:144-155, 655-657).
func TestParseGitHubEvent_CheckSuite(t *testing.T) {
	body := `{"action":"completed","check_suite":{"head_sha":"abc123"},"repository":{"full_name":"o/r"}}`
	ev, ok, err := ParseGitHubEvent("check_suite", []byte(body))
	if err != nil || !ok {
		t.Fatalf("check_suite parse: ok=%v err=%v", ok, err)
	}
	if ev.Change != compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_CHECKS {
		t.Errorf("Change = %v, want CHECKS", ev.Change)
	}
	if ev.HeadSHA != "abc123" {
		t.Errorf("HeadSHA = %q, want abc123", ev.HeadSHA)
	}
	if ev.Checks != nil {
		t.Errorf("Checks = %v, want nil (router fills it)", ev.Checks)
	}

	// A non-completed check_suite action is ignored.
	if _, ok, _ := ParseGitHubEvent("check_suite", []byte(`{"action":"requested","check_suite":{"head_sha":"x"},"repository":{"full_name":"o/r"}}`)); ok {
		t.Error("check_suite.requested ok = true, want false")
	}
}

// TestParseGitHubEvent_StripsOwner asserts a commented body carrying a single
// well-formed owner header is stripped at normalize and the agent claim
// surfaced (design.md:554-557, 657-659).
func TestParseGitHubEvent_StripsOwner(t *testing.T) {
	stamped, err := StampOwner("the real comment", Author{AgentHandle: "agent-x", OwnerHandle: "owner-y", SessionID: "sess-1"}, 0)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	// Embed the stamped body as a JSON string.
	bodyJSON, err := json.Marshal(stamped)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	payload := `{"action":"created","issue":{"number":8,"html_url":"iu"},"comment":{"id":2,"html_url":"cu","body":` + string(bodyJSON) + `,"user":{"login":"c"}},"repository":{"full_name":"o/r"}}`

	ev, ok, err := ParseGitHubEvent("issue_comment", []byte(payload))
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}
	if ev.Comment.GetBody() != "the real comment" {
		t.Errorf("stripped body = %q, want %q", ev.Comment.GetBody(), "the real comment")
	}
	if ev.Comment.GetAgent().GetAgentHandle() != "agent-x" {
		t.Errorf("agent claim = %q, want agent-x", ev.Comment.GetAgent().GetAgentHandle())
	}
}
