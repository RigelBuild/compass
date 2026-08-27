package forge

import (
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// ForgeEvent is the pipeline's single normalized currency: the shape both the
// GitHub webhook arm (githubapp_webhook.go) and the Linear data-change arm
// (internal/linearagent) produce, and the router (T4) consumes. It is a
// server-internal value; it is not a wire type.
//
// Field types are grounded on the frozen wire currency (the ForgeNotification
// gen message, internal/gen/compass/v1/forge.pb.go:326-341): Change is the
// notification kind, Comment a *CommentRef, Checks a *v1.ChecksSummary, Kind
// the artifact kind. Provider is the compass.v1 ForgeProvider enum.
type ForgeEvent struct {
	// Provider is the forge this event came from (GITHUB / LINEAR).
	Provider compassv1.ForgeProvider
	// Host is the forge host ("github.com", a GHES host, "linear.app").
	Host string
	// Repo is the GitHub owner/name, or the Linear team key.
	Repo string
	// Kind is the artifact kind: issue(1) or pull_request(2).
	Kind compassv1internal.ForgeArtifactKind
	// Number is the artifact's number (always set; on OPENED it is the NEW
	// artifact's number).
	Number uint64
	// Project is the Linear issue's project id (container matching, W2); "" on
	// GitHub events.
	Project string
	// URL is the artifact's (or comment's) canonical web URL.
	URL string
	// Change is the notification kind this event maps to.
	Change compassv1internal.ForgeNotificationKind
	// Comment is set for COMMENT / REVIEW: the new comment, header-stripped and
	// author-attributed.
	Comment *compassv1internal.CommentRef
	// Checks is set for CHECKS, but only by the router's roll-up fetch (T4),
	// never at parse time — a check_suite is per-App, not roll-up truth.
	Checks *compassv1.ChecksSummary
	// HeadSHA is set for CHECKS: the completed suite's head SHA.
	HeadSHA string
	// State is the new forge state string for STATE / the verdict for REVIEW.
	State string
	// DeliveryID is the provider's delivery UUID (X-GitHub-Delivery /
	// Linear-Delivery), used by the mount's dedup LRU.
	DeliveryID string
}

// MapLinearState exposes the package's Linear workflow-state -> forge
// open/closed truth mapping (linear.go:730) to the linearagent data-change
// arm, which normalizes a STATE event's verdict through the same mapping.
func MapLinearState(stateType string) string {
	return mapLinearState(stateType)
}
