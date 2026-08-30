package forge

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// actionCompleted is the GitHub webhook action for a finished check_suite.
const actionCompleted = "completed"

// VerifyGitHubSignature reports whether headerValue is the HMAC-SHA256 of
// rawBody under secret. headerValue is the X-Hub-Signature-256 header, shaped
// "sha256=<hex>". The comparison is constant-time; any missing prefix or
// hex-decode error returns false. Mirrors linearagent.VerifySignature
// (webhook.go:65-73), adapted to GitHub's prefixed header shape.
func VerifyGitHubSignature(secret, rawBody []byte, headerValue string) bool {
	hexPart, ok := strings.CutPrefix(headerValue, "sha256=")
	if !ok {
		return false
	}
	want, err := hex.DecodeString(hexPart)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(rawBody)
	return hmac.Equal(want, mac.Sum(nil))
}

// whUser is the actor sub-object GitHub attaches to comments and artifacts.
type whUser struct {
	Login string `json:"login"`
}

// whComment is a GitHub issue/PR/review comment (webhook shape).
type whComment struct {
	ID      uint64 `json:"id"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	User    whUser `json:"user"`
}

// whIssue is a GitHub issue (also the carrier on issue_comment). A non-empty
// PullRequest marker means the issue is actually a pull request.
type whIssue struct {
	Number      uint64          `json:"number"`
	HTMLURL     string          `json:"html_url"`
	State       string          `json:"state"`
	PullRequest json.RawMessage `json:"pull_request"`
}

// whPullRequest is a GitHub pull request (webhook shape).
type whPullRequest struct {
	Number  uint64 `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
}

// whCheckSuite is the check_suite sub-object.
type whCheckSuite struct {
	HeadSHA string `json:"head_sha"`
}

// whRepository is the repository sub-object (owner/name via full_name).
type whRepository struct {
	FullName string `json:"full_name"`
}

// whReview is the pull_request_review sub-object.
type whReview struct {
	ID      uint64 `json:"id"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	State   string `json:"state"`
	User    whUser `json:"user"`
}

// whPayload is the union of the GitHub webhook payload fields this arm reads.
// A single struct covers all event types (only the relevant sub-objects are
// populated per event), matching the flat json.Unmarshal shape the arm needs.
type whPayload struct {
	Action      string         `json:"action"`
	Issue       *whIssue       `json:"issue"`
	PullRequest *whPullRequest `json:"pull_request"`
	Comment     *whComment     `json:"comment"`
	CheckSuite  *whCheckSuite  `json:"check_suite"`
	Review      *whReview      `json:"review"`
	Repository  whRepository   `json:"repository"`
}

// ParseGitHubEvent maps (X-GitHub-Event, raw body) to a normalized ForgeEvent,
// or ok=false for an event/action this arm ignores (counted-and-dropped by the
// caller, never an error). The mapping follows the frozen Approach event table
// (design.md:131-142). Bodies run StripOwner here — normalize is the one strip
// point (design.md:554-557, 657-659).
func ParseGitHubEvent(event string, body []byte) (ev ForgeEvent, ok bool, err error) {
	var wh whPayload
	if uerr := json.Unmarshal(body, &wh); uerr != nil {
		return ForgeEvent{}, false, fmt.Errorf("forge: parse github %s event: %w", event, uerr)
	}

	base := ForgeEvent{
		Provider: compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB,
		Host:     hostGitHub,
		Repo:     wh.Repository.FullName,
	}

	switch event {
	case "issues":
		return parseGitHubIssues(base, wh)
	case "issue_comment":
		return parseGitHubIssueComment(base, wh)
	case "pull_request":
		return parseGitHubPullRequest(base, wh)
	case "pull_request_review":
		return parseGitHubReview(base, wh)
	case "pull_request_review_comment":
		return parseGitHubReviewComment(base, wh)
	case "check_suite":
		return parseGitHubCheckSuite(base, wh)
	default:
		return ForgeEvent{}, false, nil
	}
}

func parseGitHubIssues(base ForgeEvent, wh whPayload) (ForgeEvent, bool, error) {
	if wh.Issue == nil {
		return ForgeEvent{}, false, nil
	}
	change, ok := gitHubStateOrUpdateKind(wh.Action)
	if !ok {
		return ForgeEvent{}, false, nil
	}
	base.Kind = compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE
	base.Number = wh.Issue.Number
	base.URL = wh.Issue.HTMLURL
	base.Change = change
	if change == compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_STATE {
		base.State = wh.Issue.State
	}
	return base, true, nil
}

func parseGitHubPullRequest(base ForgeEvent, wh whPayload) (ForgeEvent, bool, error) {
	if wh.PullRequest == nil {
		return ForgeEvent{}, false, nil
	}
	change, ok := gitHubStateOrUpdateKind(wh.Action)
	if !ok {
		return ForgeEvent{}, false, nil
	}
	base.Kind = compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST
	base.Number = wh.PullRequest.Number
	base.URL = wh.PullRequest.HTMLURL
	base.Change = change
	if change == compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_STATE {
		base.State = gitHubPRState(wh.PullRequest)
	}
	return base, true, nil
}

// gitHubStateOrUpdateKind maps an issues/pull_request action to its
// notification kind per the event table: opened->OPENED,
// closed/reopened->STATE, edited/labeled/unlabeled->UPDATE. Any other action
// (assigned, milestoned, …) is ignored (ok=false).
func gitHubStateOrUpdateKind(action string) (compassv1internal.ForgeNotificationKind, bool) {
	switch action {
	case "opened":
		return compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_OPENED, true
	case "closed", "reopened":
		return compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_STATE, true
	case "edited", "labeled", "unlabeled":
		return compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_UPDATE, true
	default:
		return compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_UNSPECIFIED, false
	}
}

// gitHubPRState renders the PR's forge state: "merged" if merged, else the raw
// state ("closed"/"open").
func gitHubPRState(pr *whPullRequest) string {
	if pr.Merged {
		return "merged"
	}
	return pr.State
}

func parseGitHubIssueComment(base ForgeEvent, wh whPayload) (ForgeEvent, bool, error) {
	if wh.Action != "created" || wh.Issue == nil || wh.Comment == nil {
		return ForgeEvent{}, false, nil
	}
	// GitHub serves PR conversation comments on this event too; the issue's
	// pull_request marker discriminates PR from issue (design.md:136, 653-654).
	base.Kind = compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE
	if len(wh.Issue.PullRequest) > 0 {
		base.Kind = compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST
	}
	base.Number = wh.Issue.Number
	base.URL = wh.Comment.HTMLURL
	base.Change = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_COMMENT
	base.Comment = gitHubCommentRef(wh.Comment)
	return base, true, nil
}

func parseGitHubReviewComment(base ForgeEvent, wh whPayload) (ForgeEvent, bool, error) {
	if wh.Action != "created" || wh.PullRequest == nil || wh.Comment == nil {
		return ForgeEvent{}, false, nil
	}
	base.Kind = compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST
	base.Number = wh.PullRequest.Number
	base.URL = wh.Comment.HTMLURL
	base.Change = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_COMMENT
	base.Comment = gitHubCommentRef(wh.Comment)
	return base, true, nil
}

func parseGitHubReview(base ForgeEvent, wh whPayload) (ForgeEvent, bool, error) {
	if wh.Action != "submitted" || wh.PullRequest == nil || wh.Review == nil {
		return ForgeEvent{}, false, nil
	}
	base.Kind = compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST
	base.Number = wh.PullRequest.Number
	base.URL = wh.Review.HTMLURL
	base.Change = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_REVIEW
	base.State = wh.Review.State
	base.Comment = &compassv1internal.CommentRef{
		Url:          wh.Review.HTMLURL,
		CommentId:    wh.Review.ID,
		CommentKey:   strconv.FormatUint(wh.Review.ID, 10),
		ForgeAccount: wh.Review.User.Login,
	}
	base.Comment.Body, base.Comment.Agent = stripBodyToRef(wh.Review.Body)
	return base, true, nil
}

func parseGitHubCheckSuite(base ForgeEvent, wh whPayload) (ForgeEvent, bool, error) {
	if wh.Action != actionCompleted || wh.CheckSuite == nil {
		return ForgeEvent{}, false, nil
	}
	// A check_suite has no artifact number and carries NO ChecksSummary — a
	// suite is per-App, never roll-up truth; T4's router fetches the combined
	// roll-up for the head SHA (design.md:144-155, 655-657).
	base.Kind = compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST
	base.Change = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_CHECKS
	base.HeadSHA = wh.CheckSuite.HeadSHA
	return base, true, nil
}

// gitHubCommentRef builds a CommentRef from a GitHub comment, running
// StripOwner on the body (the one strip point) and surfacing the parsed agent
// claim only when a single well-formed header was present.
func gitHubCommentRef(c *whComment) *compassv1internal.CommentRef {
	ref := &compassv1internal.CommentRef{
		Url:          c.HTMLURL,
		CommentId:    c.ID,
		CommentKey:   strconv.FormatUint(c.ID, 10),
		ForgeAccount: c.User.Login,
	}
	ref.Body, ref.Agent = stripBodyToRef(c.Body)
	return ref
}

// stripBodyToRef runs StripOwner over a raw forge body and returns the cleaned
// body plus the agent attribution claim (nil unless a single well-formed v1
// header was present). The owner claim is display-only (DL-050/DL-094).
func stripBodyToRef(raw string) (string, *compassv1.AgentAttribution) {
	clean, author, ok := StripOwner(raw)
	if !ok {
		return clean, nil
	}
	return clean, &compassv1.AgentAttribution{AgentHandle: author.AgentHandle}
}
