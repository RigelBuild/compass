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

// FetchedArtifact is one coordinate's freshly-observed state, assembled by the
// reconcile sweep (T5) from its conditional reads, with any 304'd half ALREADY
// resolved to the carried-forward prior value (design.md:946: "a 304'd half
// carries prev's values forward"). DetectChanges diffs it against the stored
// snapshot. It is the fetch-side producer of the cross-producer canonicalization
// invariant (design.md:517-533): the snapshot DetectChanges rebuilds from a
// FetchedArtifact for a given state MUST be byte-identical (post-canonicalJSON)
// to what T4's webhook ApplyEvent built for that same state.
//
// An ARTIFACT target (Container=false) carries State/Comments/Checks; a CONTAINER
// target (Container=true, the number==0 reconcile row) carries NewArtifacts (the
// artifacts opened above the stored high-water) and leaves the artifact halves
// zero.
type FetchedArtifact struct {
	Provider  compassv1.ForgeProvider
	Host      string
	Repo      string
	Kind      compassv1internal.ForgeArtifactKind
	Number    uint64
	URL       string
	Container bool

	// Artifact-scope effective state (Container=false).
	State    string          // effective forge state (already domain-mapped)
	Comments []forge.Comment // effective full comment set (bodies RAW; stripped here)
	Checks   *forge.Checks   // effective combined roll-up (PR only; nil when none)

	// Container-scope (Container=true): the artifacts opened above the stored
	// high-water, newest-first, each carrying its own number/url/project.
	NewArtifacts []forge.Issue
}

// DetectChanges diffs the stored snapshot against a full-fetch observation,
// returning the synthetic ForgeEvents the changes map to, the rebuilt next
// snapshot, and its revision (design.md:943-946). It is PURE: it never touches
// the store or network. Contract:
//   - prev == nil is the BASELINE (first observation): it rebuilds next from
//     fetched and returns NO changes — the coordinate is recorded, not notified.
//   - Otherwise it emits: a COMMENT event per URL present in fetched but not in
//     prev; a STATE event when the state half changed; a CHECKS event when the
//     combined roll-up changed; an OPENED event per NewArtifact above the stored
//     high-water (container scope).
//   - next is the canonical rebuild of the fetched state, marshalled through the
//     SAME ArtifactSnapshot/canonicalJSON as ApplyEvent (labels excluded — the
//     invariant), so revision == SnapshotRevision(&next) matches the webhook arm
//     for identical state.
func DetectChanges(prev *ArtifactSnapshot, fetched FetchedArtifact) (changes []forge.ForgeEvent, next ArtifactSnapshot, revision string) {
	if fetched.Container {
		next, changes = detectContainer(prev, fetched)
	} else {
		next, changes = detectArtifact(prev, fetched)
	}
	// Canonicalize the rebuilt snapshot through the same codec ApplyEvent's
	// revision runs through, so an empty diff is byte-identical to the webhook
	// arm (the meeting-point invariant). Round-tripping next through
	// canonicalJSON pins the checks-slice ordering the digest depends on.
	var canon ArtifactSnapshot
	_ = json.Unmarshal(canonicalJSON(&next), &canon) // canonicalJSON emits well-formed JSON for a fixed shape; unmarshal cannot fail
	next = canon
	revision = SnapshotRevision(&next)
	return changes, next, revision
}

// detectArtifact rebuilds the artifact-scope snapshot (state + comment set +
// checks) from the fetched observation and emits the per-kind change events.
func detectArtifact(prev *ArtifactSnapshot, fetched FetchedArtifact) (ArtifactSnapshot, []forge.ForgeEvent) {
	next := ArtifactSnapshot{State: fetched.State}
	if len(fetched.Comments) > 0 {
		next.Comments = make(map[string]SnapshotComment, len(fetched.Comments))
	}
	for _, c := range fetched.Comments {
		if c.URL == "" {
			continue
		}
		clean, _, _ := forge.StripOwner(c.Body) // strip the owner header, same as the webhook normalize (design.md:554-557)
		next.Comments[c.URL] = SnapshotComment{URL: c.URL, Body: clean, ForgeAccount: c.ForgeAccount}
	}
	if fetched.Checks != nil {
		next.Checks = checksFromSummary(checksSummaryFromForge(*fetched.Checks))
	}
	if prev != nil {
		next.HighWaterNumber = prev.HighWaterNumber // artifact reads never touch the container high-water
	}

	if prev == nil {
		return next, nil // baseline: record, do not notify
	}

	var changes []forge.ForgeEvent
	// New comments: a URL in fetched not in prev (at-least-once; deletions are
	// not in the event alphabet, so a dropped comment heals the snapshot silently).
	for _, c := range fetched.Comments {
		if c.URL == "" {
			continue
		}
		if _, had := prev.Comments[c.URL]; had {
			continue
		}
		clean, author, ok := forge.StripOwner(c.Body)
		ref := &compassv1internal.CommentRef{Url: c.URL, Body: clean, ForgeAccount: c.ForgeAccount}
		if ok {
			ref.Agent = &compassv1.AgentAttribution{AgentHandle: author.AgentHandle}
		}
		changes = append(changes, forge.ForgeEvent{
			Provider: fetched.Provider, Host: fetched.Host, Repo: fetched.Repo,
			Kind: fetched.Kind, Number: fetched.Number, URL: c.URL,
			Change:  compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_COMMENT,
			Comment: ref,
		})
	}
	// State change.
	if fetched.State != prev.State {
		changes = append(changes, forge.ForgeEvent{
			Provider: fetched.Provider, Host: fetched.Host, Repo: fetched.Repo,
			Kind: fetched.Kind, Number: fetched.Number, URL: fetched.URL,
			Change: compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_STATE,
			State:  fetched.State,
		})
	}
	// Checks change: compare the canonical checks halves (digest equality). A
	// non-nil fetched roll-up carries its summary + head SHA; a checks half that
	// vanished (fetched.Checks nil while prev had one) still heals the snapshot
	// but emits no CHECKS event (a "checks gone" has no place in the alphabet).
	if fetched.Checks != nil && checksChanged(prev.Checks, next.Checks) {
		changes = append(changes, forge.ForgeEvent{
			Provider: fetched.Provider, Host: fetched.Host, Repo: fetched.Repo,
			Kind: fetched.Kind, Number: fetched.Number, URL: fetched.URL,
			Change:  compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_CHECKS,
			Checks:  checksSummaryFromForge(*fetched.Checks),
			HeadSHA: fetched.Checks.HeadSHA,
		})
	}
	return next, changes
}

// detectContainer rebuilds the container-scope snapshot (high-water only) and
// emits an OPENED event per newly-opened artifact above the stored high-water.
func detectContainer(prev *ArtifactSnapshot, fetched FetchedArtifact) (ArtifactSnapshot, []forge.ForgeEvent) {
	base := uint64(0)
	if prev != nil {
		base = prev.HighWaterNumber
	}
	next := ArtifactSnapshot{HighWaterNumber: base}
	for _, a := range fetched.NewArtifacts {
		if a.Number > next.HighWaterNumber {
			next.HighWaterNumber = a.Number
		}
	}
	if prev == nil {
		return next, nil // baseline: record the high-water, do not notify
	}
	var changes []forge.ForgeEvent
	for _, a := range fetched.NewArtifacts {
		if a.Number <= base {
			continue
		}
		changes = append(changes, forge.ForgeEvent{
			Provider: fetched.Provider, Host: fetched.Host, Repo: fetched.Repo,
			Kind: fetched.Kind, Number: a.Number, URL: a.URL, Project: a.Project,
			Change: compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_OPENED,
		})
	}
	return next, changes
}

// checksChanged reports whether two canonical checks halves differ, by comparing
// their canonical-JSON bytes (the same ordering the digest uses).
func checksChanged(a, b *ChecksSnapshot) bool {
	sa := canonicalJSON(&ArtifactSnapshot{Checks: a})
	sb := canonicalJSON(&ArtifactSnapshot{Checks: b})
	return string(sa) != string(sb)
}
