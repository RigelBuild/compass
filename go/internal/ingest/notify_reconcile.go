package ingest

// The reconciliation sweep (RIG-2732 T5, design.md:888-982): the reliability
// backstop that heals a missed webhook without reintroducing a poll. It runs one
// immediate sweep at startup (healing the downtime window) then on a ticker at
// the Backstop cadence (default 30 min). Per sweep it enumerates the subscribed
// coordinates (the NotifyStore.ListNotifyTargets seam), conditionally re-reads
// each via the forge.NotifyReader, diffs the observation against the stored
// snapshot (DetectChanges), feeds every detected change into the SAME
// NotifyRouter.Route as a synthetic ForgeEvent, and — for a subscriber whose
// delivered_revision trails the cursor with no pending diff — synthesizes ONE
// payload-free UPDATE (advance rides the ack, W3). Requests are paced within the
// sweep (anti-burst); ErrBudgetExhausted aborts the sweep (resumed next
// interval, the board driver's treatment, driver.go:134-136); a per-target error
// is isolated (logged, sweep continues); ctx cancellation returns promptly.

import (
	"context"
	"errors"
	"log/slog"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// defaultBackstop is the chosen backstop cadence (design.md:948-970): 30 min
// keeps the sweep unmistakably a backstop (tens-of-minutes) while staying ~30x
// under the prior poll design's issued-request rate; it is also the worst-case
// latency for a missed webhook. A deployment that weights that latency higher
// turns it down (10 min is still comfortably inside the rate bucket).
const defaultBackstop = 30 * time.Minute

// defaultPace is the anti-burst inter-request delay applied between target
// fetches within one sweep, so a 150-artifact sweep spreads its conditional GETs
// rather than firing them in a single burst. A zero ReconcileConfig.Pace uses
// this default; a negative value disables pacing (tests set it to 0-with-clock or
// a tiny value to avoid real sleeps).
const defaultPace = 200 * time.Millisecond

// ReconcileConfig configures the reconciliation sweep.
type ReconcileConfig struct {
	// Backstop is the ticker cadence between sweeps; <= 0 uses defaultBackstop.
	Backstop time.Duration
	// Pace is the inter-target delay within a sweep (anti-burst); 0 uses
	// defaultPace, negative disables pacing.
	Pace time.Duration
	// Log is the sweep logger; nil uses slog.Default().
	Log *slog.Logger
}

// NotifyReconciler drives the backstop sweep over the shared NotifyStore seam,
// re-reading each coordinate through the forge.NotifyReader and routing detected
// changes through the shared NotifyRouter (never reimplementing routing/notify/
// snapshot-apply — the sweep is a synthetic-event PRODUCER for the T4 router).
type NotifyReconciler struct {
	reader   forge.NotifyReader
	store    NotifyStore
	router   *NotifyRouter
	provider compassv1.ForgeProvider
	host     string
	backstop time.Duration
	pace     time.Duration
	log      *slog.Logger
}

// NewNotifyReconciler returns a reconciler reading through reader, enumerating +
// persisting through st, routing through router, over the (provider, host) the
// server adapter bound. A nil log defaults to slog.Default.
func NewNotifyReconciler(reader forge.NotifyReader, st NotifyStore, router *NotifyRouter, provider compassv1.ForgeProvider, host string, cfg ReconcileConfig) *NotifyReconciler {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	backstop := cfg.Backstop
	if backstop <= 0 {
		backstop = defaultBackstop
	}
	pace := cfg.Pace
	if pace == 0 {
		pace = defaultPace
	}
	return &NotifyReconciler{
		reader:   reader,
		store:    st,
		router:   router,
		provider: provider,
		host:     host,
		backstop: backstop,
		pace:     pace,
		log:      log,
	}
}

// Run performs one immediate sweep, then a sweep on every Backstop tick, until
// ctx is cancelled (then it returns nil — clean shutdown, driver.go:99-112). A
// sweep error is never returned: ErrBudgetExhausted aborts the current sweep and
// the next tick resumes; a per-target error is isolated inside the sweep.
func (rc *NotifyReconciler) Run(ctx context.Context) error {
	rc.sweep(ctx)

	t := time.NewTicker(rc.backstop)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			rc.sweep(ctx)
		}
	}
}

// sweep runs one reconciliation pass over every enumerated target. ctx
// cancellation ends it promptly; a target that returns ErrBudgetExhausted aborts
// the whole sweep (the bucket is shared — the next target would fail too), to be
// resumed next interval; any other per-target error is logged and skipped.
func (rc *NotifyReconciler) sweep(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	targets, err := rc.store.ListNotifyTargets(ctx)
	if err != nil {
		rc.log.ErrorContext(ctx, "forge reconcile: list targets", "error", err)
		return
	}
	for i, tgt := range targets {
		if ctx.Err() != nil {
			return
		}
		// Anti-burst pacing between targets (never before the first).
		if i > 0 && rc.pace > 0 {
			if !sleepCtx(ctx, rc.pace) {
				return // ctx cancelled during the pace wait
			}
		}
		if err := rc.reconcileTarget(ctx, tgt); err != nil {
			if errors.Is(err, forge.ErrBudgetExhausted) {
				rc.log.WarnContext(ctx, "forge reconcile: budget exhausted, aborting sweep (resumes next interval)",
					"repo", tgt.Repo, "number", tgt.Number)
				return
			}
			rc.log.WarnContext(ctx, "forge reconcile: target failed (isolated)",
				"repo", tgt.Repo, "kind", int32(tgt.Kind), "number", tgt.Number, "error", err)
		}
	}
}

// reconcileTarget re-reads one coordinate, diffs it, routes any changes, then
// handles a lagging subscriber. A container target (number == 0) walks
// ListNewArtifacts; an artifact target re-reads its state/comments/checks.
func (rc *NotifyReconciler) reconcileTarget(ctx context.Context, tgt NotifyTarget) error {
	prev, prevETags := snapshotAndETags(tgt.Cursor)

	fetched, etags, err := rc.fetch(ctx, tgt, prevETags)
	if err != nil {
		return err
	}

	changes, next, revision := DetectChanges(prev, fetched)

	// Persist the rebuilt snapshot + fresh ETags BEFORE notify (fetch-side truth
	// advances unconditionally — the router does the same for a webhook, but the
	// sweep also refreshes the issue/comments ETags a webhook cannot). This is
	// the invariant's "re-stores fresh ETags" step (design.md:532-533, 980-981).
	if err := rc.store.UpsertArtifactCursor(ctx, ArtifactCursor{
		Repo:         tgt.Repo,
		Kind:         tgt.Kind,
		Number:       tgt.Number,
		ETag:         etags.detail,
		CommentsETag: etags.comments,
		ChecksETag:   etags.checks,
		Revision:     revision,
		Snapshot:     mustMarshalSnapshot(&next),
	}); err != nil {
		return err
	}

	// Route each detected change as a synthetic event through the SHARED router
	// (it re-loads the just-upserted cursor, re-applies idempotently — the
	// comment set is keyed by the stable comment key, state/checks overwrite,
	// OPENED is max — so every
	// notification carries the same final revision).
	for _, ev := range changes {
		if err := rc.router.Route(ctx, ev); err != nil {
			if errors.Is(err, forge.ErrBudgetExhausted) {
				return err
			}
			rc.log.WarnContext(ctx, "forge reconcile: route synthetic event failed (isolated)",
				"repo", ev.Repo, "number", ev.Number, "change", int32(ev.Change), "error", err)
		}
	}

	// Lagging-subscriber recovery: when the diff was EMPTY (no synthetic change
	// re-notified anyone) but a subscriber's delivered_revision trails the
	// current cursor revision, the snapshot is current yet that subscriber never
	// got acked up to it — synthesize exactly ONE payload-free UPDATE (advance
	// rides the ack, W3). A non-empty diff already re-notified the exact-
	// coordinate subscribers, so nothing is synthesized then.
	if len(changes) == 0 {
		for _, sub := range tgt.Subscribers {
			if sub.DeliveredRevision != revision {
				rc.router.SynthesizeUpdate(ctx, sub, tgt.Repo, tgt.Kind, tgt.Number, fetched.URL, revision)
			}
		}
	}
	return nil
}

// fetchETags carries the three per-endpoint ETags a fetch reads with and writes
// back (fresh on a 200, carried-forward on a 304).
type fetchETags struct {
	detail, comments, checks string
}

// fetch assembles the FetchedArtifact for one target from the NotifyReader,
// resolving each 304'd half to its carried-forward prior value, and returns the
// fresh (or carried-forward) ETags to re-store. A container target (number == 0)
// reads ListNewArtifacts; an artifact target reads state (+ comments, + checks
// for a PR).
func (rc *NotifyReconciler) fetch(ctx context.Context, tgt NotifyTarget, prev fetchETags) (FetchedArtifact, fetchETags, error) {
	fa := FetchedArtifact{
		Provider: rc.provider,
		Host:     rc.host,
		Repo:     tgt.Repo,
		Kind:     tgt.Kind,
		Number:   tgt.Number,
	}
	etags := prev

	if tgt.Number == 0 {
		// Container-scope: the artifacts opened above the stored high-water.
		fa.Container = true
		since := uint64(0)
		if snap := decodeSnapshot(tgt.Cursor); snap != nil {
			since = snap.HighWaterNumber
		}
		res, err := rc.reader.ListNewArtifacts(ctx, tgt.Repo, tgt.Kind, since, prev.detail)
		if err != nil {
			return FetchedArtifact{}, fetchETags{}, err
		}
		if !res.NotModified {
			fa.NewArtifacts = res.V
			etags.detail = res.ETag
		}
		return fa, etags, nil
	}

	prevSnap := decodeSnapshot(tgt.Cursor)

	// Detail (issue or PR): state + URL.
	switch tgt.Kind {
	case compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE:
		res, err := rc.reader.GetIssueConditional(ctx, tgt.Repo, tgt.Number, prev.detail)
		if err != nil {
			return FetchedArtifact{}, fetchETags{}, err
		}
		if res.NotModified {
			if prevSnap != nil {
				fa.State = prevSnap.State
			}
		} else {
			fa.State = res.V.State
			fa.URL = res.V.URL
			etags.detail = res.ETag
		}
	case compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST:
		res, err := rc.reader.GetPullRequestConditional(ctx, tgt.Repo, tgt.Number, prev.detail)
		if err != nil {
			return FetchedArtifact{}, fetchETags{}, err
		}
		if res.NotModified {
			if prevSnap != nil {
				fa.State = prevSnap.State
			}
		} else {
			fa.State = res.V.State
			fa.URL = res.V.URL
			etags.detail = res.ETag
		}
	default:
		return FetchedArtifact{}, fetchETags{}, errInvalidEvent
	}

	// Comments (issue + PR conversation).
	cres, err := rc.reader.ListComments(ctx, tgt.Repo, tgt.Kind, tgt.Number, prev.comments)
	if err != nil {
		return FetchedArtifact{}, fetchETags{}, err
	}
	if cres.NotModified {
		// Carry the prior comment set forward (a 304 = no new comments).
		fa.Comments = commentsFromSnapshot(prevSnap)
	} else {
		fa.Comments = cres.V
		etags.comments = cres.ETag
	}

	// Checks (PR only).
	if tgt.Kind == compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST {
		chres, err := rc.reader.ChecksConditional(ctx, tgt.Repo, tgt.Number, "", prev.checks)
		if err != nil {
			// Linear PRs are unsupported, but a PR target is GitHub-only here;
			// any real error propagates (budget aborts, others isolate).
			return FetchedArtifact{}, fetchETags{}, err
		}
		if chres.NotModified {
			if c := checksFromPrevSnapshot(prevSnap); c != nil {
				fa.Checks = c
			}
		} else {
			checks := chres.V
			fa.Checks = &checks
			etags.checks = chres.ETag
		}
	}
	return fa, etags, nil
}

// snapshotAndETags decodes a cursor's stored snapshot and ETags (all zero when
// the cursor is nil / never observed).
func snapshotAndETags(cur *ArtifactCursor) (*ArtifactSnapshot, fetchETags) {
	if cur == nil {
		return nil, fetchETags{}
	}
	return decodeSnapshot(cur), fetchETags{detail: cur.ETag, comments: cur.CommentsETag, checks: cur.ChecksETag}
}

// commentsFromSnapshot reconstructs the forge.Comment set from a stored snapshot
// (for a 304'd comments half — the bodies are already stripped in the snapshot,
// so DetectChanges' re-strip is a no-op on them).
func commentsFromSnapshot(snap *ArtifactSnapshot) []forge.Comment {
	if snap == nil || len(snap.Comments) == 0 {
		return nil
	}
	out := make([]forge.Comment, 0, len(snap.Comments))
	for _, c := range snap.Comments {
		out = append(out, forge.Comment{Key: c.Key, Body: c.Body, ForgeAccount: c.ForgeAccount})
	}
	return out
}

// checksFromPrevSnapshot rebuilds a forge.Checks from a stored snapshot's checks
// half (for a 304'd checks half — carry the prior combined roll-up forward).
func checksFromPrevSnapshot(snap *ArtifactSnapshot) *forge.Checks {
	if snap == nil || snap.Checks == nil {
		return nil
	}
	out := forge.Checks{HeadSHA: snap.Checks.HeadSHA, State: snap.Checks.State}
	for _, c := range snap.Checks.Checks {
		out.Checks = append(out.Checks, forge.Check{Name: c.Name, State: c.State, URL: c.URL, Required: c.Required})
	}
	return &out
}

// mustMarshalSnapshot marshals a snapshot to its canonical JSON for the cursor
// row. The shape is a fixed set of JSON-safe scalars/maps, so Marshal cannot
// fail (same guarantee as canonicalJSON's errchkjson waiver).
func mustMarshalSnapshot(snap *ArtifactSnapshot) []byte {
	return canonicalJSON(snap)
}

// sleepCtx sleeps for d or until ctx is cancelled, reporting true if the full
// duration elapsed (false on cancellation).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
