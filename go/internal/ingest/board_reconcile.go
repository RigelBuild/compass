package ingest

// The board reconciliation sweep (RIG-2883 T3, design.md:335-403): the
// reliability backstop for the forge webhook-ingestion path AND the
// cold-start/backfill path. It runs one immediate sweep at startup (healing any
// downtime window and backfilling a freshly deployed or reinstalled App) then on
// a slow ticker at the Backstop cadence (default 30 min). Per sweep it
// enumerates the enabled repos (the BoardStore.ListEnabledRepos seam),
// conditionally lists each repo's updated-order issues since its stored
// watermark (the updatedLister seam, satisfied by forge.GitHub.ListUpdatedIssues
// at T5), and sinks the rows through the SAME ingest pipeline (Ingester), then
// advances the durable watermark AFTER the rows sank (the advance-after-sink
// invariant). A zero/absent watermark = one full walk: the cold-start and
// App-reinstall backfill answer with no separate mechanism.
//
// Requests are paced within the sweep (anti-burst); ErrBudgetExhausted aborts
// the sweep (resumed next interval); a per-repo error is isolated (logged, sweep
// continues); ctx cancellation returns promptly.
//
// Poisoned-row livelock is bounded: IngestIssues stops on the first sink error
// (ingest.go:64-66), so a whole-repo advance-after-sink would let one
// persistently-rejected row pin the watermark and re-walk a growing window every
// sweep. The sweep instead sinks each row in isolation, skips-and-counts a
// poison row, and advances the watermark past the HEALTHY rows so the re-walk
// window stays bounded.

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/RigelBuild/compass/go/internal/forge"
)

// BoardStore is the durable enabled-repo + watermark seam. The server adapts
// *store.Store onto it at T5 (the forgePollStore pattern); this package owns
// only the narrow structural view.
type BoardStore interface {
	// ListEnabledRepos returns the repos with board ingestion enabled.
	ListEnabledRepos(ctx context.Context) ([]string, error)
	// LoadRepoWatermark returns the repo's updated-at watermark + list ETag
	// (zero values when the repo was never swept — the cold-start signal).
	LoadRepoWatermark(ctx context.Context, repo string) (time.Time, string, error)
	// StoreRepoWatermark persists the watermark + ETag AFTER the repo's rows
	// sank (advance-after-sink, the idempotency invariant).
	StoreRepoWatermark(ctx context.Context, repo string, mark time.Time, etag string) error
}

// updatedLister is the conditional updated-order list surface, satisfied by
// forge.GitHub.ListUpdatedIssues at T5. LOCAL + structural: this package never
// imports the concrete provider.
type updatedLister interface {
	ListUpdatedIssues(ctx context.Context, repo string, since time.Time, etag string) (forge.ConditionalResult[[]forge.Issue], error)
}

// BoardReconcileConfig configures the board reconciliation sweep.
type BoardReconcileConfig struct {
	// Backstop is the ticker cadence between sweeps; <= 0 uses defaultBackstop.
	Backstop time.Duration
	// Pace is the inter-repo delay within a sweep (anti-burst); 0 uses
	// defaultPace, negative disables pacing.
	Pace time.Duration
	// Log is the sweep logger; nil uses slog.Default().
	Log *slog.Logger
}

// BoardReconciler drives the backstop sweep over the enabled-repo set, listing
// each repo's updated-order issues through the updatedLister and sinking them
// through the shared Ingester (never reimplementing the strip/translate/sink
// pipeline — the sweep is a conditional-list CONSUMER for the ingest pipeline).
type BoardReconciler struct {
	lister   updatedLister
	ingester *Ingester
	store    BoardStore
	backstop time.Duration
	pace     time.Duration
	log      *slog.Logger
}

// NewBoardReconciler returns a reconciler listing through l, sinking through
// ing, enumerating + persisting through st. A nil log defaults to slog.Default.
func NewBoardReconciler(l updatedLister, ing *Ingester, st BoardStore, cfg BoardReconcileConfig) *BoardReconciler {
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
	return &BoardReconciler{
		lister:   l,
		ingester: ing,
		store:    st,
		backstop: backstop,
		pace:     pace,
		log:      log,
	}
}

// Run performs one immediate sweep, then a sweep on every Backstop tick, until
// ctx is cancelled (then it returns nil — clean shutdown). A sweep error is
// never returned: ErrBudgetExhausted aborts the current sweep and the next tick
// resumes; a per-repo error is isolated inside the sweep.
func (rc *BoardReconciler) Run(ctx context.Context) error {
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

// sweep runs one reconciliation pass over every enabled repo. ctx cancellation
// ends it promptly; a repo that returns ErrBudgetExhausted aborts the whole
// sweep (the bucket is shared — the next repo would fail too), to be resumed
// next interval; any other per-repo error is logged and skipped.
func (rc *BoardReconciler) sweep(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	repos, err := rc.store.ListEnabledRepos(ctx)
	if err != nil {
		rc.log.ErrorContext(ctx, "board reconcile: list enabled repos", "error", err)
		return
	}
	for i, repo := range repos {
		if ctx.Err() != nil {
			return
		}
		// Anti-burst pacing between repos (never before the first).
		if i > 0 && rc.pace > 0 {
			if !sleepCtx(ctx, rc.pace) {
				return // ctx cancelled during the pace wait
			}
		}
		if err := rc.reconcileRepo(ctx, repo); err != nil {
			if errors.Is(err, forge.ErrBudgetExhausted) {
				rc.log.WarnContext(ctx, "board reconcile: budget exhausted, aborting sweep (resumes next interval)",
					"repo", repo)
				return
			}
			rc.log.WarnContext(ctx, "board reconcile: repo failed (isolated)",
				"repo", repo, "error", err)
		}
	}
}

// reconcileRepo conditionally lists one repo's updated-order issues since its
// stored watermark, sinks them, and advances the watermark AFTER the sink. A 304
// costs no sink and leaves the watermark untouched (the stored ETag remains the
// truth). A zero/absent watermark lists everything (cold-start/backfill).
//
// Rows are sunk one at a time so a poison row is isolated: it is skipped and
// counted, the rest sink, and the watermark advances only past the rows that
// actually sank — the re-walk window stays bounded rather than growing every
// sweep behind a pinned watermark.
func (rc *BoardReconciler) reconcileRepo(ctx context.Context, repo string) error {
	since, etag, err := rc.store.LoadRepoWatermark(ctx, repo)
	if err != nil {
		return err
	}
	res, err := rc.lister.ListUpdatedIssues(ctx, repo, since, etag)
	if err != nil {
		return err
	}
	if res.NotModified {
		return nil // content unchanged — the stored watermark + ETag stay the truth
	}

	var maxMark time.Time
	poison := 0
	for _, row := range res.V {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if serr := rc.ingester.IngestIssues(ctx, repo, []forge.Issue{row}); serr != nil {
			// Isolate the poison row: skip + count it, keep sinking the rest,
			// and do NOT let its timestamp advance the watermark.
			poison++
			rc.log.WarnContext(ctx, "board reconcile: row sink failed (isolated)",
				"repo", repo, "number", row.Number, "error", serr)
			continue
		}
		if row.UpdatedAt.After(maxMark) {
			maxMark = row.UpdatedAt
		}
	}

	// Nothing sank (empty list, or every row poison): leave the watermark where
	// it is so a healthy row is re-listed next sweep. The window is bounded — it
	// never accumulates rows behind a pinned watermark.
	if maxMark.IsZero() {
		return nil
	}

	// Advance-after-sink. On a clean sweep carry the fresh list ETag so the next
	// sweep can 304; when a poison row was skipped, drop the ETag so the next
	// sweep re-lists unconditionally (a fresh ETag would 304-suppress the
	// still-unsunk row's retry) — bounded to the rows at/after maxMark.
	storeETag := res.ETag
	if poison > 0 {
		storeETag = ""
	}
	if serr := rc.store.StoreRepoWatermark(ctx, repo, maxMark, storeETag); serr != nil {
		return serr
	}
	return nil
}
