//go:build unix

// Fake forge webhook SENDERS for the RIG-2848 notification e2e matrix — the
// test doubles the existing coverage lacks. forge.FakeProvider models the
// read/write CLIENT; nothing models a forge EMITTING a signed webhook. These
// fakes do: given a high-level artifact mutation (a comment, a state flip, a
// review, a check-suite completion), they produce the exact signed HTTP shape
// GitHub / Linear post to the ingress, so a test drives the REAL landed ingress
// (NewGitHubWebhookHandler + VerifyGitHubSignature + ParseGitHubEvent /
// linearagent.VerifySignature + ParseLinearDataEvent) rather than a hand-forged
// ForgeEvent struct.
//
// They are deliberately reusable: the composed matrix (forge_notify_matrix_test)
// consumes them now against the landed ingress→router pipeline, and the
// full-stack e2e consumes the same senders once the T7 /webhooks mount +
// store-backed NotifyStore adapter land (RIG-2717, RIG-2732 T5/T7).
package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
)

// ---- GitHub fake webhook sender ----

// ghUserPayload is the actor sub-object (webhook wire shape, mirrors forge.whUser).
type ghUserPayload struct {
	Login string `json:"login"`
}

// ghCommentPayload mirrors forge.whComment on the wire.
type ghCommentPayload struct {
	ID      uint64        `json:"id"`
	HTMLURL string        `json:"html_url"`
	Body    string        `json:"body"`
	User    ghUserPayload `json:"user"`
}

// ghIssuePayload mirrors forge.whIssue; a non-nil PullRequest marker makes an
// issue_comment resolve to a PR comment (the discriminator ParseGitHubEvent reads).
type ghIssuePayload struct {
	Number      uint64          `json:"number"`
	HTMLURL     string          `json:"html_url"`
	State       string          `json:"state"`
	PullRequest json.RawMessage `json:"pull_request,omitempty"`
}

// ghPRPayload mirrors forge.whPullRequest.
type ghPRPayload struct {
	Number  uint64 `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
}

// ghCheckSuitePayload mirrors forge.whCheckSuite.
type ghCheckSuitePayload struct {
	HeadSHA string `json:"head_sha"`
}

// ghReviewPayload mirrors forge.whReview.
type ghReviewPayload struct {
	ID      uint64        `json:"id"`
	HTMLURL string        `json:"html_url"`
	Body    string        `json:"body"`
	State   string        `json:"state"`
	User    ghUserPayload `json:"user"`
}

// ghRepoPayload mirrors forge.whRepository.
type ghRepoPayload struct {
	FullName string `json:"full_name"`
}

// ghWebhookPayload is the union GitHub posts; only the sub-objects relevant to
// an event are set, matching the flat shape ParseGitHubEvent unmarshals.
type ghWebhookPayload struct {
	Action      string               `json:"action"`
	Issue       *ghIssuePayload      `json:"issue,omitempty"`
	PullRequest *ghPRPayload         `json:"pull_request,omitempty"`
	Comment     *ghCommentPayload    `json:"comment,omitempty"`
	CheckSuite  *ghCheckSuitePayload `json:"check_suite,omitempty"`
	Review      *ghReviewPayload     `json:"review,omitempty"`
	Repository  ghRepoPayload        `json:"repository"`
}

// fakeGitHubForge emits signed GitHub webhooks for one repo. It is the missing
// double: a forge that SENDS events, not one that answers reads.
type fakeGitHubForge struct {
	secret       []byte
	repo         string // owner/name
	nextDelivery int
}

func newFakeGitHubForge(secret []byte, repo string) *fakeGitHubForge {
	return &fakeGitHubForge{secret: secret, repo: repo}
}

// signedWebhook is one emitted, signed webhook ready to POST at the ingress.
type signedWebhook struct {
	event    string
	delivery string
	body     []byte
	sig      string
}

// emit marshals payload, assigns a fresh delivery id, and signs the raw body
// exactly as GitHub does (X-Hub-Signature-256 over the bytes on the wire).
func (f *fakeGitHubForge) emit(t *testing.T, event string, p ghWebhookPayload) signedWebhook {
	t.Helper()
	p.Repository = ghRepoPayload{FullName: f.repo}
	body, err := json.Marshal(&p)
	if err != nil {
		t.Fatalf("fakeGitHubForge.emit marshal %s: %v", event, err)
	}
	f.nextDelivery++
	return signedWebhook{
		event:    event,
		delivery: fmt.Sprintf("gh-delivery-%d", f.nextDelivery),
		body:     body,
		sig:      ghSign(f.secret, body),
	}
}

// openIssue emits issues.opened for a new issue #n (OPENED, container fan-out).
func (f *fakeGitHubForge) openIssue(t *testing.T, n uint64, url string) signedWebhook {
	t.Helper()
	return f.emit(t, "issues", ghWebhookPayload{
		Action: "opened",
		Issue:  &ghIssuePayload{Number: n, HTMLURL: url, State: "open"},
	})
}

// closeIssue emits issues.closed for issue #n (STATE, state="closed").
func (f *fakeGitHubForge) closeIssue(t *testing.T, n uint64, url string) signedWebhook {
	t.Helper()
	return f.emit(t, "issues", ghWebhookPayload{
		Action: "closed",
		Issue:  &ghIssuePayload{Number: n, HTMLURL: url, State: "closed"},
	})
}

// editIssue emits issues.edited for issue #n (UPDATE).
func (f *fakeGitHubForge) editIssue(t *testing.T, n uint64, url string) signedWebhook {
	t.Helper()
	return f.emit(t, "issues", ghWebhookPayload{
		Action: "edited",
		Issue:  &ghIssuePayload{Number: n, HTMLURL: url, State: "open"},
	})
}

// commentOnIssue emits issue_comment.created on issue #n (COMMENT, kind=ISSUE).
func (f *fakeGitHubForge) commentOnIssue(t *testing.T, n uint64, commentURL, body, author string) signedWebhook {
	t.Helper()
	return f.emit(t, "issue_comment", ghWebhookPayload{
		Action:  "created",
		Issue:   &ghIssuePayload{Number: n, HTMLURL: "issue-url", State: "open"},
		Comment: &ghCommentPayload{ID: 1, HTMLURL: commentURL, Body: body, User: ghUserPayload{Login: author}},
	})
}

// commentOnPR emits issue_comment.created on a PR (COMMENT, kind=PR) — the
// pull_request marker on the issue carrier is what discriminates PR from issue.
func (f *fakeGitHubForge) commentOnPR(t *testing.T, n uint64, commentURL, body, author string) signedWebhook {
	t.Helper()
	return f.emit(t, "issue_comment", ghWebhookPayload{
		Action:  "created",
		Issue:   &ghIssuePayload{Number: n, HTMLURL: "pr-url", State: "open", PullRequest: json.RawMessage(`{"url":"pr"}`)},
		Comment: &ghCommentPayload{ID: 2, HTMLURL: commentURL, Body: body, User: ghUserPayload{Login: author}},
	})
}

// openPR emits pull_request.opened for a new PR #n (OPENED).
func (f *fakeGitHubForge) openPR(t *testing.T, n uint64, url string) signedWebhook {
	t.Helper()
	return f.emit(t, "pull_request", ghWebhookPayload{
		Action:      "opened",
		PullRequest: &ghPRPayload{Number: n, HTMLURL: url, State: "open"},
	})
}

// mergePR emits pull_request.closed with merged=true (STATE, state="merged").
func (f *fakeGitHubForge) mergePR(t *testing.T, n uint64, url string) signedWebhook {
	t.Helper()
	return f.emit(t, "pull_request", ghWebhookPayload{
		Action:      "closed",
		PullRequest: &ghPRPayload{Number: n, HTMLURL: url, State: "closed", Merged: true},
	})
}

// closePR emits pull_request.closed with merged=false (STATE, state="closed").
func (f *fakeGitHubForge) closePR(t *testing.T, n uint64, url string) signedWebhook {
	t.Helper()
	return f.emit(t, "pull_request", ghWebhookPayload{
		Action:      "closed",
		PullRequest: &ghPRPayload{Number: n, HTMLURL: url, State: "closed"},
	})
}

// editPR emits pull_request.edited (UPDATE — title/body edit).
func (f *fakeGitHubForge) editPR(t *testing.T, n uint64, url string) signedWebhook {
	t.Helper()
	return f.emit(t, "pull_request", ghWebhookPayload{
		Action:      "edited",
		PullRequest: &ghPRPayload{Number: n, HTMLURL: url, State: "open"},
	})
}

// reviewPR emits pull_request_review.submitted (REVIEW, verdict=state).
func (f *fakeGitHubForge) reviewPR(t *testing.T, n uint64, reviewURL, body, verdict, author string) signedWebhook {
	t.Helper()
	return f.emit(t, "pull_request_review", ghWebhookPayload{
		Action:      "submitted",
		PullRequest: &ghPRPayload{Number: n, HTMLURL: "pr-url", State: "open"},
		Review:      &ghReviewPayload{ID: 7, HTMLURL: reviewURL, Body: body, State: verdict, User: ghUserPayload{Login: author}},
	})
}

// reviewCommentOnPR emits pull_request_review_comment.created (COMMENT, kind=PR).
func (f *fakeGitHubForge) reviewCommentOnPR(t *testing.T, n uint64, commentURL, body, author string) signedWebhook {
	t.Helper()
	return f.emit(t, "pull_request_review_comment", ghWebhookPayload{
		Action:      "created",
		PullRequest: &ghPRPayload{Number: n, HTMLURL: "pr-url", State: "open"},
		Comment:     &ghCommentPayload{ID: 3, HTMLURL: commentURL, Body: body, User: ghUserPayload{Login: author}},
	})
}

// completeCheckSuite emits check_suite.completed (CHECKS; carries head_sha, no
// artifact number — the roll-up is the router's fetch, not parse-time).
func (f *fakeGitHubForge) completeCheckSuite(t *testing.T, headSHA string) signedWebhook {
	t.Helper()
	return f.emit(t, "check_suite", ghWebhookPayload{
		Action:     "completed",
		CheckSuite: &ghCheckSuitePayload{HeadSHA: headSHA},
	})
}

// ---- Linear fake webhook sender ----

// lnTeamPayload / lnStatePayload / lnProjectPayload / lnUserPayload / lnIssueRef
// mirror the data-change sub-objects ParseLinearDataEvent reads
// (linearagent/data_event.go). json tags are Linear's camelCase.
type lnTeamPayload struct {
	Key string `json:"key"`
}

type lnStatePayload struct {
	Type string `json:"type"`
}

type lnProjectPayload struct {
	ID string `json:"id"`
}

type lnUserPayload struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

// lnIssueRef is the issue a Comment is attached to.
type lnIssueRef struct {
	Number    uint64           `json:"number"`
	URL       string           `json:"url"`
	Team      lnTeamPayload    `json:"team"`
	ProjectID string           `json:"projectId,omitempty"`
	Project   lnProjectPayload `json:"project"`
}

// lnDataPayload is the union of Issue/Comment fields the arm reads.
type lnDataPayload struct {
	Number     uint64           `json:"number,omitempty"`
	URL        string           `json:"url,omitempty"`
	Identifier string           `json:"identifier,omitempty"`
	Team       lnTeamPayload    `json:"team"`
	State      lnStatePayload   `json:"state"`
	ProjectID  string           `json:"projectId,omitempty"`
	Project    lnProjectPayload `json:"project"`

	ID    string        `json:"id,omitempty"`
	Body  string        `json:"body,omitempty"`
	User  lnUserPayload `json:"user"`
	Issue *lnIssueRef   `json:"issue,omitempty"`
}

// lnDataEvent is the data-change envelope (type + action + data + updatedFrom).
type lnDataEvent struct {
	Type        string          `json:"type"`
	Action      string          `json:"action"`
	Data        lnDataPayload   `json:"data"`
	UpdatedFrom json.RawMessage `json:"updatedFrom,omitempty"`
}

// fakeLinearForge emits signed Linear data-change webhooks for one team/project.
// Linear signs the raw body with a plain hex HMAC-SHA256 (no "sha256=" prefix)
// under the webhook signing secret (linearagent.VerifySignature).
type fakeLinearForge struct {
	secret       []byte
	teamKey      string
	project      string
	nextDelivery int
}

func newFakeLinearForge(secret []byte, teamKey, project string) *fakeLinearForge {
	return &fakeLinearForge{secret: secret, teamKey: teamKey, project: project}
}

// emit marshals the envelope and signs the raw body as Linear does (bare hex).
func (f *fakeLinearForge) emit(t *testing.T, ev lnDataEvent) signedLinearWebhook {
	t.Helper()
	body, err := json.Marshal(&ev)
	if err != nil {
		t.Fatalf("fakeLinearForge.emit marshal %s: %v", ev.Type, err)
	}
	f.nextDelivery++
	return signedLinearWebhook{
		body: body,
		sig:  lnSign(f.secret, body),
	}
}

// openIssue emits Issue/create (OPENED; project carried for container match).
func (f *fakeLinearForge) openIssue(t *testing.T, n uint64, url string) signedLinearWebhook {
	t.Helper()
	return f.emit(t, lnDataEvent{
		Type:   "Issue",
		Action: "create",
		Data: lnDataPayload{
			Number: n, URL: url, Team: lnTeamPayload{Key: f.teamKey},
			ProjectID: f.project,
		},
	})
}

// changeIssueState emits Issue/update with a prior stateId in updatedFrom, so
// linearStateChanged→true (STATE; verdict via MapLinearState(stateType)).
func (f *fakeLinearForge) changeIssueState(t *testing.T, n uint64, url, stateType string) signedLinearWebhook {
	t.Helper()
	return f.emit(t, lnDataEvent{
		Type:   "Issue",
		Action: "update",
		Data: lnDataPayload{
			Number: n, URL: url, Team: lnTeamPayload{Key: f.teamKey},
			ProjectID: f.project, State: lnStatePayload{Type: stateType},
		},
		UpdatedFrom: json.RawMessage(`{"stateId":"prev-state"}`),
	})
}

// editIssue emits Issue/update with NO stateId change (UPDATE, not STATE).
func (f *fakeLinearForge) editIssue(t *testing.T, n uint64, url string) signedLinearWebhook {
	t.Helper()
	return f.emit(t, lnDataEvent{
		Type:   "Issue",
		Action: "update",
		Data: lnDataPayload{
			Number: n, URL: url, Team: lnTeamPayload{Key: f.teamKey},
			ProjectID: f.project,
		},
		UpdatedFrom: json.RawMessage(`{"title":"old title"}`),
	})
}

// commentOnIssue emits Comment/create on issue #n (COMMENT).
func (f *fakeLinearForge) commentOnIssue(t *testing.T, n uint64, issueURL, body, author string) signedLinearWebhook {
	t.Helper()
	return f.emit(t, lnDataEvent{
		Type:   "Comment",
		Action: "create",
		Data: lnDataPayload{
			ID: "cmt-1", Body: body, User: lnUserPayload{DisplayName: author},
			Issue: &lnIssueRef{
				Number: n, URL: issueURL, Team: lnTeamPayload{Key: f.teamKey},
				ProjectID: f.project,
			},
		},
	})
}

// removeIssue emits Issue/remove — counted-and-dropped (no notification kind
// models deletion); the sender exists so the matrix can assert ok=false.
func (f *fakeLinearForge) removeIssue(t *testing.T, n uint64, url string) signedLinearWebhook {
	t.Helper()
	return f.emit(t, lnDataEvent{
		Type:   "Issue",
		Action: "remove",
		Data: lnDataPayload{
			Number: n, URL: url, Team: lnTeamPayload{Key: f.teamKey}, ProjectID: f.project,
		},
	})
}

// signedLinearWebhook is one emitted Linear webhook: raw body + bare-hex sig.
type signedLinearWebhook struct {
	body []byte
	sig  string
}

// lnSign renders Linear's Linear-Signature header value: bare hex HMAC-SHA256
// of the raw body under the signing secret (no "sha256=" prefix — the shape
// linearagent.VerifySignature hex-decodes and constant-time compares).
func lnSign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
