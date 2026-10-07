package ingest

// The board reconciliation sweep (RIG-2883 T3): the reliability backstop for the
// forge webhook path AND the cold-start/backfill path. One immediate sweep at
// startup, then a slow ticker (Backstop cadence, default 30 min).

// Per sweep it enumerates enabled repos, conditionally lists updated issues since
// each stored watermark, sinks them through the SAME Ingester, then advances the
// watermark AFTER the sink. A zero watermark = one full walk (cold-start backfill).

// Requests are paced (anti-burst); ErrBudgetExhausted aborts the sweep; a per-
// repo error is isolated; ctx cancellation returns promptly.

// Poisoned-row livelock is bounded: a whole-repo advance-after-sink would let one
// persistently-rejected row pin the watermark and re-walk a growing window. The
// sweep instead sinks each row in isolation, skips-and-counts a poison row, and
// advances past the HEALTHY rows so the re-walk window stays bounded.

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/RigelBuild/compass/go/internal/forge"
)

// BoardStore is the durable enabled-repo + watermark seam. The server adapts
// *store.Store onto it at T5 (the boardReconcileStore adapter); this package owns
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
	// PullRequestUpdatedAt returns the stored PR row's forge updated_at; ok is
	// false when the PR was never stored.
	PullRequestUpdatedAt(ctx context.Context, repo string, number uint64) (time.Time, bool, error)
	// PRsBackfilledAt returns when the repo's PR backfill ran; ok is false if never.
	PRsBackfilledAt(ctx context.Context, repo string) (time.Time, bool, error)
	// MarkPRsBackfilled records that the repo's PR backfill ran at at.
	MarkPRsBackfilled(ctx context.Context, repo string, at time.Time) error
}

// prBackfillWindow bounds which closed PRs a cold start or backfill hydrates.
const prBackfillWindow = 30 * 24 * time.Hour

// updatedLister is the conditional updated-order list surface, satisfied by
// forge.GitHub.ListUpdatedIssues at T5. LOCAL + structural: this package never
// imports the concrete provider.
type updatedLister interface {
	ListUpdatedIssues(ctx context.Context, repo string, since time.Time, etag string) (forge.ConditionalResult[forge.UpdatedRows], error)
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
	// Pulls hydrates listed PR rows and runs the backfill; nil skips PR rows.
	Pulls *PullRequestHydrator
}

// BoardReconciler drives the backstop sweep over the enabled-repo set, listing
// each repo's updated-order issues through the updatedLister and sinking them
// through the shared Ingester (never reimplementing the strip/translate/sink
// pipeline — the sweep is a conditional-list CONSUMER for the ingest pipeline).
type BoardReconciler struct {
	lister   updatedLister
	ingester *Ingester
	store    BoardStore
	pulls    *PullRequestHydrator
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
		pulls:    cfg.Pulls,
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

// reconcileRepo conditionally lists one repo's updated-order rows since its
// stored watermark, sinks them, and advances the watermark AFTER the sink. A 304
// costs no sink and leaves the watermark untouched (the stored ETag remains the
// truth). A zero/absent watermark lists everything (cold-start/backfill). The
// PR backfill runs after the walk, whether or not it was a 304.
func (rc *BoardReconciler) reconcileRepo(ctx context.Context, repo string) error {
	since, etag, err := rc.store.LoadRepoWatermark(ctx, repo)
	if err != nil {
		return err
	}
	res, err := rc.lister.ListUpdatedIssues(ctx, repo, since, etag)
	if err != nil {
		return err
	}
	pullsFailed := 0
	if !res.NotModified {
		if pullsFailed, err = rc.sinkRows(ctx, repo, since, res); err != nil {
			return err
		}
	}
	return rc.backfillPRs(ctx, repo, since, pullsFailed)
}

// sinkRows sinks one listed window, isolating each row so a poison row is
// skipped and counted, and advances the watermark only past the rows that sank.
// A PR hydrate that runs out of budget is not poison: the watermark stays at
// since with no ETag, so the next sweep re-lists the whole window. It returns
// the count of PR rows that failed to hydrate.
//
// Tradeoff (deliberate, do not "fix"): the watermark advances to the max
// timestamp over the HEALTHY rows, so a row that fails only TRANSIENTLY while
// co-batched with a newer healthy row is left below the advanced watermark and
// is dropped until its next forge update re-lists it. Capping the advance below
// the min failed-row timestamp instead would re-introduce the poison-pin
// livelock this isolation exists to prevent.
func (rc *BoardReconciler) sinkRows(ctx context.Context, repo string, since time.Time, res forge.ConditionalResult[forge.UpdatedRows]) (int, error) {
	var maxMark time.Time
	poison, pullsFailed := 0, 0
	for _, row := range res.V.Issues {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if serr := rc.ingester.IngestIssues(ctx, repo, []forge.Issue{row}); serr != nil {
			poison++
			rc.log.WarnContext(ctx, "board reconcile: row sink failed (isolated)",
				"repo", repo, "number", row.Number, "error", serr)
			continue
		}
		maxMark = later(maxMark, row.UpdatedAt)
	}
	if rc.pulls != nil {
		for _, row := range res.V.Pulls {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			if !since.IsZero() || inBackfillWindow(row, time.Now()) {
				if herr := rc.hydrateIfNewer(ctx, repo, row); herr != nil {
					if errors.Is(herr, forge.ErrBudgetExhausted) {
						return 0, rc.abortWindow(ctx, repo, since, herr)
					}
					poison++
					pullsFailed++
					rc.log.WarnContext(ctx, "board reconcile: pull request hydrate failed (isolated)",
						"repo", repo, "number", row.Number, "error", herr)
					continue
				}
			}
			maxMark = later(maxMark, row.UpdatedAt)
		}
	}

	// Nothing sank (empty list, or every row poison): leave the watermark where
	// it is so a healthy row is re-listed next sweep.
	if maxMark.IsZero() {
		return pullsFailed, nil
	}
	// On a clean sweep carry the fresh list ETag so the next sweep can 304; when
	// a poison row was skipped, drop it so the next sweep re-lists.
	storeETag := res.ETag
	if poison > 0 {
		storeETag = ""
	}
	return pullsFailed, rc.store.StoreRepoWatermark(ctx, repo, maxMark, storeETag)
}

// abortWindow keeps the watermark at since and clears the ETag, so the next
// sweep re-lists every row of the window instead of getting a 304, then
// returns cause.
func (rc *BoardReconciler) abortWindow(ctx context.Context, repo string, since time.Time, cause error) error {
	if err := rc.store.StoreRepoWatermark(ctx, repo, since, ""); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// hydrateIfNewer hydrates a listed PR row unless the stored row is as new.
func (rc *BoardReconciler) hydrateIfNewer(ctx context.Context, repo string, row forge.UpdatedPull) error {
	stored, ok, err := rc.store.PullRequestUpdatedAt(ctx, repo, row.Number)
	if err != nil {
		return err
	}
	if ok && !row.UpdatedAt.After(stored) {
		return nil
	}
	return rc.pulls.Hydrate(ctx, repo, row.Number)
}

// backfillPRs runs once per repo: it hydrates every open PR and every PR row
// updated in the window before the watermark, then marks the repo. On a cold
// start the main walk covered that window, so its PR failures hold the mark.
func (rc *BoardReconciler) backfillPRs(ctx context.Context, repo string, since time.Time, walkFailed int) error {
	if rc.pulls == nil {
		return nil
	}
	if _, done, err := rc.store.PRsBackfilledAt(ctx, repo); err != nil || done {
		return err
	}
	open, err := rc.pulls.listOpen(ctx, repo)
	if err != nil {
		return err
	}
	rows := open
	if !since.IsZero() {
		win, err := rc.lister.ListUpdatedIssues(ctx, repo, since.Add(-prBackfillWindow), "")
		if err != nil {
			return err
		}
		rows = append(rows, win.V.Pulls...)
	}
	failed := walkFailed
	for _, row := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := rc.hydrateIfNewer(ctx, repo, row); err != nil {
			if errors.Is(err, forge.ErrBudgetExhausted) {
				return err
			}
			failed++
			rc.log.WarnContext(ctx, "board reconcile: backfill hydrate failed (isolated)",
				"repo", repo, "number", row.Number, "error", err)
		}
	}
	// A failed row would never be re-listed, so retry the pass next sweep; the
	// updated_at gate makes the rows that sank cheap to skip.
	if failed > 0 {
		return nil
	}
	return rc.store.MarkPRsBackfilled(ctx, repo, time.Now())
}

// inBackfillWindow reports whether a cold start hydrates the row: open, or
// updated within the backfill window.
func inBackfillWindow(row forge.UpdatedPull, now time.Time) bool {
	return row.State == "open" || row.UpdatedAt.After(now.Add(-prBackfillWindow))
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
