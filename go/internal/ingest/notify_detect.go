package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"maps"
	"slices"
)

// ArtifactSnapshot is the whole-artifact state the notify path digests into a
// revision (RIG-2732 T4, design.md:850-866). It is a CANONICAL FORM shared by
// two producers — T4's webhook ApplyEvent (this file) and T5's sweep full-fetch
// rebuild — and its digest (SnapshotRevision) MUST be byte-identical across both
// for the same artifact state (the cross-producer canonicalization invariant,
// design.md:517-533). The field set is grounded strictly on what the frozen
// forge.ForgeEvent currency (T2) carries, so both producers can reproduce it:
//   - State: the forge state string, ALREADY canonicalized at parse time
//     (closed+merged -> "merged" in both arms: gitHubPRState, mapLinearState).
//   - Comments: the comment set keyed by canonical comment URL (forge comment
//     ids are unreliable — Linear UUIDs, GitHub numeric — so URL is the stable
//     cross-producer key; both providers populate Comment.URL, provider.go:64).
//   - Checks: the COMBINED CI/status roll-up (never one suite's conclusion;
//     resolved via the ChecksRoller seam before apply).
//   - HighWaterNumber: the container-scope high-water artifact number (OPENED).
//
// FORK (surfaced, not invented): the design prose also lists "labels, etc." and
// "label ordering / truncation" in the snapshot. The frozen ForgeEvent currency
// carries NO title/body/labels (notify_event.go:17-49), so the webhook arm
// cannot populate them — UPDATE (and REVIEW) are therefore snapshot-NEUTRAL here
// (notify-only, like a duplicate comment). When T5's full-fetch rebuild adds
// label tracking it MUST either exclude labels from the digest or the ForgeEvent
// currency must gain a labels field, or the invariant breaks. This is the
// sharpest T4/T5 meeting-point decision and is left for the driver/Matt.
type ArtifactSnapshot struct {
	State           string                     `json:"state,omitempty"`
	Comments        map[string]SnapshotComment `json:"comments,omitempty"`
	Checks          *ChecksSnapshot            `json:"checks,omitempty"`
	HighWaterNumber uint64                     `json:"highWaterNumber,omitempty"`
}

// SnapshotComment is one comment in the URL-keyed comment set. Body is the
// header-stripped, author-attributed body (the ForgeEvent currency already
// stripped it at normalize, design.md:554-557).
type SnapshotComment struct {
	URL          string `json:"url"`
	Body         string `json:"body,omitempty"`
	ForgeAccount string `json:"forgeAccount,omitempty"`
}

// ChecksSnapshot is the combined CI/status roll-up half of the snapshot. Checks
// is held sorted by (Name, URL) so the digest is order-independent.
type ChecksSnapshot struct {
	HeadSHA string          `json:"headSha,omitempty"`
	State   string          `json:"state,omitempty"`
	Checks  []CheckSnapshot `json:"checks,omitempty"`
}

// CheckSnapshot is one check run contributing to the roll-up.
type CheckSnapshot struct {
	Name     string `json:"name"`
	State    string `json:"state,omitempty"`
	URL      string `json:"url,omitempty"`
	Required bool   `json:"required,omitempty"`
}

// ApplyEvent applies one normalized event to the prior snapshot, returning the
// next snapshot (pure — it never mutates prev; a nil prev is the never-observed
// zero snapshot). Per-kind (design.md:850-866):
//   - COMMENT: adds/overwrites the URL-keyed comment. Re-applying the SAME URL
//     is idempotent on the snapshot (dedup is NOT content-based — a duplicate
//     COMMENT leaves the snapshot unchanged but the router STILL notifies,
//     at-least-once, design.md:878-880).
//   - STATE: overwrites the state half with ev.State (already canonicalized).
//   - CHECKS: overwrites the checks half with ev.Checks (the COMBINED roll-up
//     the router resolved via ChecksRoller — never a single suite).
//   - OPENED: bumps the container-scope high-water number.
//   - UPDATE / REVIEW: snapshot-NEUTRAL (the currency carries no observable
//     delta; see the ArtifactSnapshot FORK note). Still routed + notified.
func ApplyEvent(prev *ArtifactSnapshot, ev forge.ForgeEvent) ArtifactSnapshot {
	next := ArtifactSnapshot{}
	if prev != nil {
		next.State = prev.State
		next.HighWaterNumber = prev.HighWaterNumber
		next.Checks = cloneChecks(prev.Checks)
		if len(prev.Comments) > 0 {
			next.Comments = make(map[string]SnapshotComment, len(prev.Comments)+1)
			maps.Copy(next.Comments, prev.Comments)
		}
	}

	switch ev.Change {
	case compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_COMMENT:
		if url := ev.Comment.GetUrl(); url != "" {
			if next.Comments == nil {
				next.Comments = make(map[string]SnapshotComment, 1)
			}
			next.Comments[url] = SnapshotComment{
				URL:          url,
				Body:         ev.Comment.GetBody(),
				ForgeAccount: ev.Comment.GetForgeAccount(),
			}
		}
	case compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_STATE:
		next.State = ev.State
	case compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_CHECKS:
		if ev.Checks != nil {
			next.Checks = checksFromSummary(ev.Checks)
		}
	case compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_OPENED:
		if ev.Number > next.HighWaterNumber {
			next.HighWaterNumber = ev.Number
		}
	default:
		// UPDATE / REVIEW / UNSPECIFIED: snapshot-neutral (see the FORK note).
	}
	return next
}

// SnapshotRevision is the deterministic digest that keys a snapshot: sha256 over
// its CANONICAL JSON (deterministic key order — encoding/json sorts map keys, so
// the URL-keyed comment set is stable; the checks slice is sorted). Shared with
// T5: the same state MUST produce the same revision in both arms.
func SnapshotRevision(snap *ArtifactSnapshot) string {
	sum := sha256.Sum256(canonicalJSON(snap))
	return hex.EncodeToString(sum[:])
}

// canonicalJSON renders a snapshot to its canonical bytes: comment keys sort via
// encoding/json's map ordering; the checks slice is sorted defensively by
// (Name, URL) so a snapshot built in any order digests identically. It is the
// meeting-point contract the T4 apply and the T5 rebuild both marshal through;
// an empty diff between two states is byte equality of their canonicalJSON.
func canonicalJSON(snap *ArtifactSnapshot) []byte {
	c := ArtifactSnapshot{
		State:           snap.State,
		Comments:        snap.Comments,
		HighWaterNumber: snap.HighWaterNumber,
	}
	if snap.Checks != nil {
		checks := slices.Clone(snap.Checks.Checks)
		slices.SortFunc(checks, func(a, b CheckSnapshot) int {
			if a.Name != b.Name {
				return cmpString(a.Name, b.Name)
			}
			return cmpString(a.URL, b.URL)
		})
		c.Checks = &ChecksSnapshot{HeadSHA: snap.Checks.HeadSHA, State: snap.Checks.State, Checks: checks}
	}
	b, _ := json.Marshal(&c) //nolint:errchkjson // ArtifactSnapshot is a fixed set of JSON-safe scalars/maps; Marshal cannot fail.
	return b
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// checksFromSummary projects the canonical gen ChecksSummary (the router's
// roll-up result + the ForgeNotification payload type) into the snapshot's
// plain checks half, sorted by (Name, URL) for a stable digest.
func checksFromSummary(cs *compassv1.ChecksSummary) *ChecksSnapshot {
	out := &ChecksSnapshot{HeadSHA: cs.GetHeadSha(), State: cs.GetState()}
	for _, c := range cs.GetChecks() {
		out.Checks = append(out.Checks, CheckSnapshot{
			Name:     c.GetName(),
			State:    c.GetState(),
			URL:      c.GetUrl(),
			Required: c.GetRequired(),
		})
	}
	slices.SortFunc(out.Checks, func(a, b CheckSnapshot) int {
		if a.Name != b.Name {
			return cmpString(a.Name, b.Name)
		}
		return cmpString(a.URL, b.URL)
	})
	return out
}

// summaryFromChecks is the inverse projection: rebuild a gen ChecksSummary from
// the snapshot's stored checks half. The router uses it to carry a prior
// combined roll-up forward as the notification payload when a CHECKS roll-up
// comes back NotModified (304) — the checks are unchanged, so the last stored
// combined truth is what the notification must carry.
func summaryFromChecks(cs *ChecksSnapshot) *compassv1.ChecksSummary {
	if cs == nil {
		return nil
	}
	out := &compassv1.ChecksSummary{HeadSha: cs.HeadSHA, State: cs.State}
	for _, c := range cs.Checks {
		out.Checks = append(out.Checks, &compassv1.Check{
			Name:     c.Name,
			State:    c.State,
			Url:      c.URL,
			Required: c.Required,
		})
	}
	return out
}

func cloneChecks(cs *ChecksSnapshot) *ChecksSnapshot {
	if cs == nil {
		return nil
	}
	return &ChecksSnapshot{HeadSHA: cs.HeadSHA, State: cs.State, Checks: slices.Clone(cs.Checks)}
}
