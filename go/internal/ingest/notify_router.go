package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// The notify hot path (RIG-2732 T4, design.md:808-887): one normalized
// forge.ForgeEvent in, notifications out, snapshot current. The store enters
// through package-local structural seams (the no-store rule, design.md:538-541);
// go/server binds the (provider, host) half onto *store.Store, the same
// (provider, host)-binding store-adapter pattern. The router NEVER advances delivered_revision:
// that rides the hub's ForgeNotificationAck arm (W3), never this path.

// NotifySubscriber is one subscriber a change fans out to — the package-local
// mirror of store.ForgeNotifySubscriber (the no-store rule keeps the store type
// out of this package). SubscriptionID is the ack correlation key; Project is
// the subscriber's own container project (set only for a Linear container sub,
// "" otherwise) so an OPENED event matches only its project's container subs.
type NotifySubscriber struct {
	SubscriptionID    string
	AgentAccountID    string
	DeliveredRevision string
	Project           string
}

// ArtifactCursor is the router's view of one shared per-artifact FETCH cursor:
// the last observed snapshot + its revision digest + the per-endpoint ETags the
// conditional GETs thread (issue/PR detail, comments, checks). Coordinate-bound:
// (provider, host) are bound by the server adapter, so this carries only (repo,
// kind, number). It mirrors the store.ForgeArtifactCursor half the router (checks
// etag) and the reconcile sweep (all three etags) read/write.
type ArtifactCursor struct {
	Repo         string
	Kind         compassv1internal.ForgeArtifactKind
	Number       uint64
	ETag         string // issue/PR detail endpoint ETag (sweep-threaded)
	CommentsETag string // comments endpoint ETag (sweep-threaded)
	ChecksETag   string // check-runs endpoint ETag (router + sweep)
	Revision     string
	Snapshot     []byte // canonical-JSON ArtifactSnapshot; nil when never observed
}

// NotifyTarget is one enumerated subscribed coordinate + its FETCH cursor and
// riding subscribers — the reconcile sweep's (T5) work-list item. Declared here
// because it is part of the shared NotifyStore seam; the router itself does not
// call ListNotifyTargets (the sweep does).
type NotifyTarget struct {
	Repo        string
	Kind        compassv1internal.ForgeArtifactKind
	Number      uint64
	Cursor      *ArtifactCursor
	Subscribers []NotifySubscriber
}

// NotifyStore is the durable surface the router + reconciler (T5) share — the
// server wiring adapts *store.Store and binds (provider, host), the
// forgePollStore pattern (serve.go:1082-1090). There is deliberately NO
// delivered-revision advance here: the advance rides the hub's
// ForgeNotificationAck arm in go/server (W3), never the router.
//
// RECONCILED INCONSISTENCY (surfaced): the frozen interface block
// (design.md:815-825) lists exactly three methods, but the frozen Route
// algorithm's step 1 (design.md:841, "load the coordinate's prior snapshot from
// the cursor via NotifyStore") requires a single-coordinate cursor load the
// listed methods cannot serve (ListNotifyTargets is the sweep's bulk enumerate,
// not a per-event point read). LoadArtifactCursor is added to satisfy the
// algorithm; its go/server adapter needs a store.LoadForgeArtifactCursor point
// read (the store today has only the bulk ListForgeNotifyTargets +
// UpsertForgeArtifactCursor) — a T3/T7 follow-up flagged for the driver.
type NotifyStore interface {
	// LoadArtifactCursor point-reads the coordinate's shared FETCH cursor, or
	// nil when the coordinate has never been observed.
	LoadArtifactCursor(ctx context.Context, repo string, kind compassv1internal.ForgeArtifactKind, number uint64) (*ArtifactCursor, error)
	// SubscribersForArtifact returns the subscribers a change fans out to:
	// opened=true adds the container-scope subs (project-matched); opened=false
	// is the exact-coordinate subs only (no fan-in).
	SubscribersForArtifact(ctx context.Context, repo string, kind compassv1internal.ForgeArtifactKind, number uint64, project string, opened bool) ([]NotifySubscriber, error)
	// ListNotifyTargets enumerates every subscribed coordinate — the reconcile
	// sweep's (T5) work list. Declared on the shared seam; unused by the router.
	ListNotifyTargets(ctx context.Context) ([]NotifyTarget, error)
	// UpsertArtifactCursor writes the coordinate's shared FETCH cursor (the new
	// snapshot + revision), BEFORE notify (fetch-side truth advances
	// unconditionally, DL-053's split).
	UpsertArtifactCursor(ctx context.Context, cur ArtifactCursor) error
}

// NotifyDispatcher is the notify seam: resolve account -> live session ->
// DispatchControl, satisfied in go/server by a hub-backed adapter (T7). The
// dispatch never advances the delivery cursor (W3): success and failure alike
// leave it to the agent's ack; the reconcile sweep re-notifies from any durable
// gap. A dispatch error is logged and the router moves on (no crash).
type NotifyDispatcher interface {
	Notify(ctx context.Context, account string, n *compassv1internal.ForgeNotification) error
}

// ChecksRoller resolves the COMBINED checks roll-up for a CHECKS event's head
// SHA — a check_suite is per-App, never roll-up truth (design.md:834-837).
// Satisfied in go/server by the T5 NotifyReader's conditional checks read
// (NotifyReader.ChecksConditional), passing the cursor's checks_etag. The result
// is forge.ConditionalResult[forge.Checks] (T5 landed the real type; T4's local
// ChecksResult placeholder is collapsed into it): NotModified mirrors the 304
// arm — on NotModified the router carries the prior stored checks forward.
type ChecksRoller interface {
	RollUp(ctx context.Context, repo string, number uint64, headSHA, etag string) (forge.ConditionalResult[forge.Checks], error)
}

// NotifyRouter routes one normalized event: load the coordinate's snapshot,
// apply the event (snapshot mutation + new revision digest), upsert the cursor,
// then notify each matched subscriber. It never advances delivered_revision
// (W3 — the hub's ack arm does).
type NotifyRouter struct {
	store        NotifyStore
	dispatcher   NotifyDispatcher
	checksRoller ChecksRoller
	forgeRef     *compassv1.ForgeRef
	log          *slog.Logger
}

// NewNotifyRouter returns a router over the durable seam st, the notify seam
// disp, the roll-up seam checks, stamping forgeRef on every notification. A nil
// log defaults to slog.Default so the router never nil-panics on the log path.
func NewNotifyRouter(st NotifyStore, disp NotifyDispatcher, checks ChecksRoller, forgeRef *compassv1.ForgeRef, log *slog.Logger) *NotifyRouter {
	if log == nil {
		log = slog.Default()
	}
	return &NotifyRouter{store: st, dispatcher: disp, checksRoller: checks, forgeRef: forgeRef, log: log}
}

// Route runs the frozen algorithm (design.md:841-872) for one event:
//  1. Load the coordinate's prior snapshot (from the cursor).
//  2. For CHECKS, resolve the combined roll-up via ChecksRoller BEFORE apply
//     (a check_suite is per-App, never roll-up truth), passing the cursor's
//     checks_etag; a 304 carries the prior stored checks forward.
//  3. ApplyEvent -> next snapshot; revision = SnapshotRevision(next).
//  4. Upsert the cursor BEFORE notify (fetch-side truth advances
//     unconditionally, DL-053's split) — the new snapshot + revision.
//  5. Resolve subscribers: OPENED -> container-scope only; per-artifact ->
//     exact-coordinate only (no fan-in).
//  6. Build + dispatch a ForgeNotification per subscriber, carrying revision.
//     Never advances delivered_revision (W3). A per-subscriber dispatch error
//     is logged and skipped; a vanished subscription never crashes the route.
func (r *NotifyRouter) Route(ctx context.Context, ev forge.ForgeEvent) error {
	if ev.Provider == compassv1.ForgeProvider_FORGE_PROVIDER_UNSPECIFIED ||
		ev.Kind == compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_UNSPECIFIED ||
		ev.Number == 0 {
		return fmt.Errorf("ingest: route: zero provider/kind/number: %w", errInvalidEvent)
	}

	// 1. Prior snapshot from the cursor.
	cur, err := r.store.LoadArtifactCursor(ctx, ev.Repo, ev.Kind, ev.Number)
	if err != nil {
		return fmt.Errorf("ingest: route: load cursor %s#%d: %w", ev.Repo, ev.Number, err)
	}
	prev := decodeSnapshot(cur)
	checksETag := ""
	if cur != nil {
		checksETag = cur.ChecksETag
	}

	// 2. CHECKS: resolve the combined roll-up BEFORE apply — UNLESS the caller
	// already resolved it. A webhook CHECKS event carries only a head SHA
	// (ev.Checks nil), so the router fetches the combined roll-up via the
	// ChecksRoller seam (passing the cursor's checks_etag); a 304 carries the
	// prior stored checks forward. The reconcile sweep (T5) already fetched the
	// roll-up when it built the synthetic event, so it passes ev.Checks set and
	// the router skips the second read (the sweep's cost model is one checks GET
	// per PR per sweep, not two).
	if ev.Change == compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_CHECKS && ev.Checks == nil {
		res, rerr := r.checksRoller.RollUp(ctx, ev.Repo, ev.Number, ev.HeadSHA, checksETag)
		if rerr != nil {
			return fmt.Errorf("ingest: route: checks roll-up %s#%d: %w", ev.Repo, ev.Number, rerr)
		}
		if res.NotModified {
			// Checks unchanged: carry the prior combined truth forward as the
			// snapshot half (ApplyEvent's CHECKS arm is a no-op on nil Checks)
			// and the notification payload.
			if prev != nil {
				ev.Checks = summaryFromChecks(prev.Checks)
			}
		} else {
			ev.Checks = checksSummaryFromForge(res.V)
			checksETag = res.ETag
		}
	}

	// 3. Apply -> next snapshot + revision.
	next := ApplyEvent(prev, ev)
	revision := SnapshotRevision(&next)
	// 4. Upsert the cursor BEFORE notify (fetch-side truth advances
	// unconditionally). encoding/json marshal of a fixed scalar/map shape. The
	// issue/PR + comments ETags a prior sweep stored are carried forward
	// UNCHANGED (a webhook carries none; the next sweep re-conditions on them):
	// zeroing them would force an uncharged-but-pointless full re-fetch.
	snapBytes, merr := json.Marshal(&next)
	if merr != nil {
		return fmt.Errorf("ingest: route: marshal snapshot %s#%d: %w", ev.Repo, ev.Number, merr)
	}
	detailETag, commentsETag := "", ""
	if cur != nil {
		detailETag, commentsETag = cur.ETag, cur.CommentsETag
	}
	if err := r.store.UpsertArtifactCursor(ctx, ArtifactCursor{
		Repo:         ev.Repo,
		Kind:         ev.Kind,
		Number:       ev.Number,
		ETag:         detailETag,
		CommentsETag: commentsETag,
		ChecksETag:   checksETag,
		Revision:     revision,
		Snapshot:     snapBytes,
	}); err != nil {
		return fmt.Errorf("ingest: route: upsert cursor %s#%d: %w", ev.Repo, ev.Number, err)
	}

	// 5. Resolve subscribers: OPENED -> container-scope; else exact-coordinate.
	opened := ev.Change == compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_OPENED
	subs, err := r.store.SubscribersForArtifact(ctx, ev.Repo, ev.Kind, ev.Number, ev.Project, opened)
	if err != nil {
		return fmt.Errorf("ingest: route: resolve subscribers %s#%d: %w", ev.Repo, ev.Number, err)
	}

	// 6. Build + dispatch a notification per subscriber, carrying revision.
	for _, sub := range subs {
		n := r.notification(ev, sub.SubscriptionID, revision)
		if derr := r.dispatcher.Notify(ctx, sub.AgentAccountID, n); derr != nil {
			// A vanished subscription / dropped session is logged, not fatal —
			// the reconcile sweep re-notifies from any durable gap (W3).
			r.log.WarnContext(ctx, "forge notify dispatch failed",
				"subscription_id", sub.SubscriptionID,
				"account", sub.AgentAccountID,
				"repo", ev.Repo, "number", ev.Number,
				"error", derr)
			continue
		}
	}
	return nil
}

// SynthesizeUpdate dispatches ONE payload-free UPDATE notification to a lagging
// subscriber — the reconcile sweep's (T5) restart-safe recovery for a subscriber
// whose delivered_revision trails the shared cursor's revision on an artifact
// with NO pending diff (the snapshot is current, but this subscriber never got
// acked up to it). It carries only the coordinate + url + the current revision
// (the agent's ack advances delivered_revision — W3), so the agent re-reads and
// re-acks. A dispatch error is logged and swallowed (a vanished session heals on
// the next sweep), mirroring Route's per-subscriber isolation.
func (r *NotifyRouter) SynthesizeUpdate(ctx context.Context, sub NotifySubscriber, repo string, kind compassv1internal.ForgeArtifactKind, number uint64, url, revision string) {
	n := &compassv1internal.ForgeNotification{
		SubscriptionId: sub.SubscriptionID,
		Forge:          r.forgeRef,
		Repo:           repo,
		Kind:           kind,
		Number:         number,
		Url:            url,
		Change:         compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_UPDATE,
		Revision:       revision,
	}
	if derr := r.dispatcher.Notify(ctx, sub.AgentAccountID, n); derr != nil {
		r.log.WarnContext(ctx, "forge notify synthesized update dispatch failed",
			"subscription_id", sub.SubscriptionID,
			"account", sub.AgentAccountID,
			"repo", repo, "number", number,
			"error", derr)
	}
}

// notification builds the wire ForgeNotification for one subscriber: the
// coordinate + the per-kind payload (comment / checks / state) + the snapshot
// revision the agent echoes in its ack (design.md:341-345). It never sets
// delivered_revision — that is server-side, ack-driven (W3).
func (r *NotifyRouter) notification(ev forge.ForgeEvent, subscriptionID, revision string) *compassv1internal.ForgeNotification {
	n := &compassv1internal.ForgeNotification{
		SubscriptionId: subscriptionID,
		Forge:          r.forgeRef,
		Repo:           ev.Repo,
		Kind:           ev.Kind,
		Number:         ev.Number,
		Url:            ev.URL,
		Change:         ev.Change,
		Revision:       revision,
	}
	switch ev.Change {
	case compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_COMMENT,
		compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_REVIEW:
		n.Comment = ev.Comment
		if ev.Change == compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_REVIEW {
			n.State = ev.State
		}
	case compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_CHECKS:
		n.Checks = ev.Checks
	case compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_STATE:
		n.State = ev.State
	default:
		// UPDATE / OPENED carry only the coordinate + revision.
	}
	return n
}

// decodeSnapshot deserializes the cursor's stored canonical-JSON snapshot, or
// nil when the coordinate has never been observed (or holds no snapshot).
func decodeSnapshot(cur *ArtifactCursor) *ArtifactSnapshot {
	if cur == nil || len(cur.Snapshot) == 0 {
		return nil
	}
	var snap ArtifactSnapshot
	if err := json.Unmarshal(cur.Snapshot, &snap); err != nil {
		return nil
	}
	return &snap
}

// checksSummaryFromForge projects a forge.Checks roll-up into the canonical gen
// ChecksSummary the ForgeNotification payload + ApplyEvent's CHECKS arm consume.
func checksSummaryFromForge(c forge.Checks) *compassv1.ChecksSummary {
	out := &compassv1.ChecksSummary{HeadSha: c.HeadSHA, State: c.State}
	for _, ck := range c.Checks {
		out.Checks = append(out.Checks, &compassv1.Check{
			Name:     ck.Name,
			State:    ck.State,
			Url:      ck.URL,
			Required: ck.Required,
		})
	}
	return out
}

var errInvalidEvent = errors.New("invalid forge event")
