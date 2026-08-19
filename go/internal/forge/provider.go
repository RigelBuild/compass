// Package forge is the Server-side adapter to an external git forge/tracker
// (the issue/PR host the Compass Server drives on an owner's behalf). It owns
// the RAW forge value types (server-internal Go, never on the wire), the
// Provider interface one forge backend implements, an exported fake for
// downstream tests, and the security-critical owner-header stamp/strip
// chokepoint (owner.go).
//
// The split of concerns (#995 ownership-layer, Decisions 2 & 3):
//   - this package speaks a forge's native shapes and holds the write-time
//     attribution chokepoint; a Provider (github.go, a LATER slice) implements
//     the interface over the wire.
//   - a later Service layer stamps agent bodies before they reach a Provider,
//     strips/parses on read, and flattens transport faults (403/404) into
//     one in-band error — it consumes the value types and errors defined here.
//   - a later ingestion translation maps these raw forge types to the canonical
//     compass.v1 issue model (#1018); the raw types therefore carry every
//     forge-sourced field that translation needs, and no Compass machinery
//     (id/state-lifecycle/priority/assignee/tracker — those are canonical-only).
//
// Body handling is the Provider's contract, not this package's: a Create/Comment
// method receives a body ALREADY stamped by the Service, and a read method
// returns the body RAW — the Service strips and parses (DL-050). The owner
// header a parsed body carries is a plain display CLAIM only, never a verified
// fact, and never reaches an authz/routing/ownership decision (DL-094 / DL-050).
package forge

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Issue is the raw forge issue (server-internal; never a wire type). The later
// ingestion translation maps these forge fields to the canonical compass.v1
// Issue; Compass machinery (id/state-lifecycle/priority/assignee/prs/tracker) is
// added there, not carried here.
type Issue struct {
	// Number is the forge's own issue number.
	Number uint64
	// Title is the issue title as the forge stores it.
	Title string
	// Body is the RAW issue body: the owner header is NOT stripped here — the
	// Service strips on read (DL-050).
	Body string
	// State is the forge's truth ("open" | "closed"), not the Compass lifecycle.
	State string
	// URL is the forge's canonical web URL for the issue.
	URL string
	// ForgeAccount is the native forge login that authored the issue; always set.
	ForgeAccount string
	// Labels are the forge label names on the issue.
	Labels []string
	// UpdatedAt is the forge's last-updated timestamp for the issue, parsed from
	// the LIST row's updated_at. PR-C's DL-129 recency guard reads it off this
	// same fetch (one-fetch-path guarantee); the board sink ignores it.
	UpdatedAt time.Time
}

// Comment is a raw forge comment on an issue or pull request.
type Comment struct {
	// ID is the forge's own comment identifier.
	ID uint64
	// URL is the forge's canonical web URL for the comment.
	URL string
	// Body is the RAW comment body (owner header not stripped here).
	Body string
	// ForgeAccount is the native forge login that authored the comment.
	ForgeAccount string
}

// PullRequest is the raw forge pull request (server-internal; never a wire type).
type PullRequest struct {
	// Number is the forge's own PR number.
	Number uint64
	// Title is the PR title as the forge stores it.
	Title string
	// Body is the RAW PR body (owner header not stripped here).
	Body string
	// State is the forge's truth ("open" | "closed" | "merged").
	State string
	// URL is the forge's canonical web URL for the PR.
	URL string
	// HeadRef is the source branch name.
	HeadRef string
	// BaseRef is the target branch name.
	BaseRef string
	// ForgeAccount is the native forge login that authored the PR.
	ForgeAccount string
	// Draft reports whether the PR is a draft on the forge.
	Draft bool
	// Changed is the diff-size roll-up for the PR.
	Changed ChangedStats
	// Checks is the rolled-up CI/status state for the PR head.
	Checks Checks
	// Reviews are the PR-level review verdicts.
	Reviews []Review
	// Threads are the inline (and PR-level) review comment threads.
	Threads []ReviewThread
}

// ChangedStats is the diff-size roll-up of a pull request.
type ChangedStats struct{ Files, Additions, Deletions uint32 }

// Checks is the rolled-up CI/status state for a pull-request head SHA.
type Checks struct {
	// HeadSHA is the commit the checks ran against.
	HeadSHA string
	// State is the roll-up across all checks ("pending" | "success" | "failure").
	State string
	// Checks are the individual check runs contributing to the roll-up.
	Checks []Check
}

// Check is one CI/status check run on a pull-request head.
type Check struct {
	// Name is the check's display name.
	Name string
	// State is the check outcome
	// ("queued"|"in_progress"|"success"|"failure"|"neutral"|"cancelled").
	State string
	// URL is the forge's web URL for the check's details.
	URL string
	// Required reports whether the check is a required merge gate.
	Required bool
}

// Review is one pull-request review verdict.
type Review struct {
	// Author is the reviewer's forge account or a Compass agent handle.
	Author string
	// IsBot reports whether the forge marks the reviewer as a bot account.
	IsBot bool
	// Verdict is the review outcome
	// ("approved" | "changes_requested" | "commented").
	Verdict string
	// Body is the review's summary body.
	Body string
}

// ReviewThread is one inline (or PR-level) review comment thread.
type ReviewThread struct {
	// Path is the file the thread anchors to; empty for a PR-level thread.
	Path string
	// Resolved reports whether the thread is marked resolved on the forge.
	Resolved bool
	// Comments are the thread's comments in order.
	Comments []ThreadComment
}

// ThreadComment is one comment within a review thread.
type ThreadComment struct {
	// Author is the commenter's forge account or a Compass agent handle.
	Author string
	// IsBot reports whether the forge marks the commenter as a bot account.
	IsBot bool
	// Body is the comment body.
	Body string
}

// CreateIssue is the input to Provider.CreateIssue.
type CreateIssue struct {
	// Title is the new issue's title.
	Title string
	// Body is the new issue's body, PRE-stamp: the Service stamps it via
	// StampOwner before the Provider ever sees it.
	Body string
	// Labels are the label names to apply on creation.
	Labels []string
}

// CreatePR is the input to Provider.CreatePullRequest.
type CreatePR struct {
	// Title is the new PR's title.
	Title string
	// Body is the new PR's body, PRE-stamp (see CreateIssue.Body).
	Body string
	// HeadRef is the source branch name.
	HeadRef string
	// BaseRef is the target branch name.
	BaseRef string
	// Draft requests a draft PR.
	Draft bool
}

// SubmitReview is the input to Provider.SubmitReview. Body is PRE-stamp
// (the Service stamps it); Comments ride unstamped inside the stamped review.
type SubmitReview struct {
	Verdict  string // "approve" | "request_changes" | "comment"
	Body     string
	Comments []ReviewCommentInput
}

// ReviewCommentInput is one inline review comment carried inside a SubmitReview.
type ReviewCommentInput struct {
	Path string
	Line uint32
	Side string // "LEFT" | "RIGHT"; "" = RIGHT
	Body string
}

// SubmittedReview is the write ack a provider returns.
type SubmittedReview struct {
	ID      uint64
	URL     string
	Verdict string
}

// IssueFilter narrows Provider.ListIssues.
type IssueFilter struct {
	// State selects by forge state ("open" | "closed" | "all"); empty means the
	// provider's default.
	State string
	// Labels restricts to issues carrying all of these label names.
	Labels []string
}

// Provider is one forge backend. Every method is a network call against the
// provider's API using the Server-held credential; none accept a credential
// argument (the provider closes over its own). Body handling is the PROVIDER'S
// contract: a Create/Comment method receives a body already stamped by the
// Service, and a read method returns the body RAW — the Service strips/parses.
type Provider interface { //nolint:interfacebloat // one method per forge operation the Server drives; the surface is the forge contract, not incidental sprawl
	Name() string
	CreateIssue(ctx context.Context, repo string, in CreateIssue) (Issue, error)
	CommentOnIssue(ctx context.Context, repo string, number uint64, body string) (Comment, error)
	GetIssue(ctx context.Context, repo string, number uint64) (Issue, error)
	ListIssues(ctx context.Context, repo string, f IssueFilter) ([]Issue, error)
	CreatePullRequest(ctx context.Context, repo string, in CreatePR) (PullRequest, error)
	CommentOnPullRequest(ctx context.Context, repo string, number uint64, body string) (Comment, error)
	// SubmitReview submits a pull-request review (verdict + optional body +
	// optional inline comments). in.Body is PRE-stamped by the Service. An
	// unknown verdict is rejected before any wire call.
	SubmitReview(ctx context.Context, repo string, number uint64, in SubmitReview) (SubmittedReview, error)
	GetPullRequest(ctx context.Context, repo string, number uint64) (PullRequest, error)
	// Checks returns the rolled-up CI/status state for a PR head. Separated from
	// GetPullRequest because the subscription poller needs it alone (#995 Decision 5).
	Checks(ctx context.Context, repo string, number uint64) (Checks, error)
	// BodyLimit is the maximum body size (in BYTES) the Service enforces before
	// a write. Zero means unlimited (the fake's default). See GitHub.BodyLimit
	// for the byte-vs-character-cap rationale (A9).
	BodyLimit() int
}

// ErrUnsupported is returned by a provider for an operation it cannot serve
// (e.g. an issues-only forge for the PR half). #995 Decision 3.
var ErrUnsupported = errors.New("forge: operation unsupported by this provider")

// StatusError carries the HTTP status a forge returned, so the Service layer can
// flatten 403/404 into one byte-identical in-band error without inspecting the
// wire. A provider (github.go, PR2) returns it; the fake can script it.
type StatusError struct {
	// Status is the HTTP status code the forge returned.
	Status int
	// Message is the forge's accompanying message (for diagnosis, not the wire).
	Message string
}

// Error renders the status and message in a stable "forge: http <status>:
// <message>" form.
func (e *StatusError) Error() string {
	return fmt.Sprintf("forge: http %d: %s", e.Status, e.Message)
}
