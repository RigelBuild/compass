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
// "" otherwise) so an OPENED event matches only its project's container subs;
// Scope is the subscription's own scope, which self-origin suppression reads to
// keep the cursor advance artifact-scope-only (a container-scope sub is skipped
// but its cursor is never advanced).
type NotifySubscriber struct {
	SubscriptionID    string
	AgentAccountID    string
	DeliveredRevision string
	Project           string
	Scope             compassv1internal.ForgeSubscriptionScope
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

// PullNumberResolver resolves a CHECKS event's head SHA to its PR number — the
// coordinate a check_suite webhook does NOT carry (RIG-2869). GitHub's
// check_suite payload is head-SHA-keyed, so parseGitHubCheckSuite
// (githubapp_webhook.go) sets HeadSHA and leaves Number 0, and Route's
// zero-number guard rejected the whole CHECKS-via-check_suite kind as
// unroutable. Satisfied in go/server by a TTL-cached adapter over the shared
// GitHub client's PullRequestForSHA (forge.PullRequestResolver), so repeated
// check_suite events for one head cost one API call.
//
// A nil resolver is TOLERATED and means "do not resolve": Route then behaves
// exactly as before (the guard rejects Number==0). The Linear lane passes nil —
// Linear is issues-only and never produces a CHECKS event.
//
// A commit with no associated PR is forge.ErrNoPullRequestForSHA, which Route
// distinguishes from an infrastructure error: the former fails the route closed
// (there is no coordinate to notify against), the latter propagates.
type PullNumberResolver interface {
	PullNumberForSHA(ctx context.Context, repo, headSHA string) (uint64, error)
}

// Handle is an owner-qualified Compass identity: the owning user's handle plus
// the agent's handle. Two handles match iff both owners and both agents are
// non-empty and equal — a bare agent handle is unique only per owner, so the
// owner leg is load-bearing, not decorative.
type Handle struct{ Owner, Agent string }

// qualified reports whether both components are non-empty — the precondition for
// any positive match (an unqualified handle on either side fails open).
func (h Handle) qualified() bool { return h.Owner != "" && h.Agent != "" }

// IdentityResolver resolves owner-qualified Compass handles for self-origin
// suppression. A zero Handle with a nil error is a clean miss; the caller MUST
// fail open (deliver). A non-nil error is a store fault — log and fail open. A
// nil IdentityResolver disables suppression entirely (the zero value is the
// fail-open posture).
type IdentityResolver interface {
	// HandleForAccount resolves an agent account id to its owner-qualified
	// Compass handle.
	HandleForAccount(ctx context.Context, accountID string) (Handle, error)
	// AuthorHandle resolves the recorded authoring agent at a coordinate (the
	// DL-055 ownership row) to its owner-qualified handle. (provider, host) are
	// bound by the server adapter, like NotifyStore.
	AuthorHandle(ctx context.Context, repo string, kind compassv1internal.ForgeArtifactKind, number uint64) (Handle, error)
}

// NotifyRouter routes one normalized event: load the coordinate's snapshot,
// apply the event (snapshot mutation + new revision digest), upsert the cursor,
// then notify each matched subscriber. It never advances delivered_revision
// (W3 — the hub's ack arm does).
type NotifyRouter struct {
	store        NotifyStore
	dispatcher   NotifyDispatcher
	checksRoller ChecksRoller
	pullNumbers  PullNumberResolver
	identities   IdentityResolver
	forgeRef     *compassv1.ForgeRef
	log          *slog.Logger
}

// NewNotifyRouter returns a router over the durable seam st, the notify seam
// disp, the roll-up seam checks, the head_sha->number resolution seam pulls, and
// the identity seam ids, stamping forgeRef on every notification. A nil pulls
// disables step 0 (a CHECKS event with no number then fails the guard, the
// pre-RIG-2869 behavior). A nil ids disables self-origin suppression entirely
// (every dispatch delivered). A nil log defaults to slog.Default so the router
// never nil-panics on the log path.
func NewNotifyRouter(st NotifyStore, disp NotifyDispatcher, checks ChecksRoller, pulls PullNumberResolver, ids IdentityResolver, forgeRef *compassv1.ForgeRef, log *slog.Logger) *NotifyRouter {
	if log == nil {
		log = slog.Default()
	}
	return &NotifyRouter{store: st, dispatcher: disp, checksRoller: checks, pullNumbers: pulls, identities: ids, forgeRef: forgeRef, log: log}
}

// Route runs the frozen algorithm (design.md:841-872) for one event:
//  0. For a CHECKS event carrying a head SHA but NO number (a GitHub
//     check_suite webhook — the payload is head-SHA-keyed), resolve the SHA to
//     its PR number via PullNumberResolver BEFORE the zero-number guard, so the
//     coordinate every later step keys on exists. A nil resolver skips this (the
//     guard then rejects); a commit with no associated PR still fails closed.
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
	// 0. head_sha -> PR number (RIG-2869), BEFORE the guard: a check_suite
	// webhook is head-SHA-keyed and carries no artifact number, so without this
	// the guard below rejects the whole CHECKS-via-check_suite kind. Narrow by
	// construction: only a GITHUB CHECKS event that has a SHA, has no number,
	// and has a resolver wired. A non-CHECKS zero-number event never consults
	// the resolver — it is malformed, not under-specified.
	//
	// The provider term is what makes "by construction" true rather than
	// incidental. Today no Linear event could reach here (Linear emits no CHECKS
	// event and its lane wires no resolver), so the term is defense in depth —
	// but without it the narrowing lives in the WIRING, and a later
	// provider-agnostic lane would hand a Linear team key to a GitHub
	// commits/{sha}/pulls read: a guaranteed 404 per event against the shared
	// App budget, reported as a confusing GitHub error instead of a clean
	// zero-provider rejection.
	if ev.Number == 0 &&
		ev.Provider == compassv1.ForgeProvider_FORGE_PROVIDER_GITHUB &&
		ev.Change == compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_CHECKS &&
		ev.HeadSHA != "" && r.pullNumbers != nil {
		num, nerr := r.pullNumbers.PullNumberForSHA(ctx, ev.Repo, ev.HeadSHA)
		switch {
		case errors.Is(nerr, forge.ErrNoPullRequestForSHA):
			// No PR carries this head: there is no coordinate to notify
			// against, so fail CLOSED naming the SHA (a push to a PR-less
			// branch is the common, benign cause).
			return fmt.Errorf("ingest: route: checks %s@%s: no pull request for head sha: %w", ev.Repo, ev.HeadSHA, errInvalidEvent)
		case nerr != nil:
			return fmt.Errorf("ingest: route: resolve pull number %s@%s: %w", ev.Repo, ev.HeadSHA, nerr)
		}
		ev.Number = num
	}

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
	// Self-origin suppression: the event's actor handle is a property of the
	// event, so resolve it once here; the per-subscriber handle is resolved in
	// selfOrigin, memoized per route. A nil identity seam leaves actor a zero
	// Handle and every selfOrigin call false (suppression disabled).
	actor := r.actorHandle(ctx, ev)
	subMemo := map[string]Handle{}
	for _, sub := range subs {
		if r.selfOrigin(ctx, actor, sub, subMemo) {
			continue
		}
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

// actorHandle resolves the event's owner-qualified ACTOR handle per the
// suppress/keep matrix. A zero Handle means no actor evidence or an unresolvable
// one — the caller fails open. A nil identity seam short-circuits every arm to
// the zero Handle, so suppression is disabled wholesale.
//
// COMMENT/REVIEW read the actor straight off the header-stamped CommentRef
// (unset for a human commenter -> zero Handle). OPENED resolves the DL-055
// ownership row's recorded author, which IS the actor by construction. STATE's
// actor rides RIG-3331's forge_state_transitions memo, which is not reachable
// through this two-method seam, so STATE resolves the zero Handle here and
// delivers (the safe interim documented in §STATE) until the memo consumer is
// wired. CHECKS and UPDATE carry no actor and never suppress.
func (r *NotifyRouter) actorHandle(ctx context.Context, ev forge.ForgeEvent) Handle {
	if r.identities == nil {
		return Handle{}
	}
	switch ev.Change {
	case compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_COMMENT,
		compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_REVIEW:
		agent := ev.Comment.GetAgent()
		return Handle{Owner: agent.GetOwnerHandle(), Agent: agent.GetAgentHandle()}
	case compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_OPENED:
		h, err := r.identities.AuthorHandle(ctx, ev.Repo, ev.Kind, ev.Number)
		if err != nil {
			r.log.WarnContext(ctx, "forge notify author handle resolve failed",
				"repo", ev.Repo, "number", ev.Number, "error", err)
			return Handle{}
		}
		return h
	default:
		// STATE (interim), CHECKS, UPDATE: no reachable actor.
		return Handle{}
	}
}

// selfOrigin reports whether dispatch to sub must be skipped: the event's actor
// and this subscriber's handles are both fully owner-qualified and equal. An
// unqualified actor short-circuits before any store read (the common no-actor
// path). The subscriber handle is resolved through the identity seam, memoized
// per route by account id; a resolver fault is logged and treated as a miss
// (fail open — deliver).
func (r *NotifyRouter) selfOrigin(ctx context.Context, actor Handle, sub NotifySubscriber, memo map[string]Handle) bool {
	if !actor.qualified() {
		return false
	}
	subHandle, ok := memo[sub.AgentAccountID]
	if !ok {
		h, err := r.identities.HandleForAccount(ctx, sub.AgentAccountID)
		if err != nil {
			r.log.WarnContext(ctx, "forge notify subscriber handle resolve failed",
				"account", sub.AgentAccountID, "error", err)
			h = Handle{}
		}
		memo[sub.AgentAccountID] = h
		subHandle = h
	}
	return subHandle.qualified() && actor == subHandle
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
