package ingest

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sealedsecurity/compass/go/internal/forge"
)

// ListPageCursor is the driver's view of one durable page-cursor row.
type ListPageCursor struct {
	Page    int
	ETag    string
	HasNext bool
}

// PollStore is the durable target + FETCH-cursor surface the driver needs — a
// narrow, repo-keyed structural seam (the server wiring adapts *store.Store and
// binds provider+host), keeping this package's no-store property
// (ingest.go:7-8). Named for the whole surface the poller uses: its first
// method is target enumeration, not cursor state. ListEnabledRepos is the
// per-pass target enumeration (forge_repo_subscriptions WHERE enabled); the
// three cursor methods are the page-cursor surface.
type PollStore interface {
	ListEnabledRepos(ctx context.Context) ([]string, error)
	ListCursor(ctx context.Context, repo string) ([]ListPageCursor, error)
	UpsertListCursorPage(ctx context.Context, repo string, cur ListPageCursor) error
	PruneListCursorPages(ctx context.Context, repo string, maxPage int) error
}

// pageLister is the fetch seam: the page-level conditional read of the forge
// client (satisfied by *forge.GitHub structurally; faked in tests).
type pageLister interface {
	ListIssuesPage(ctx context.Context, repo string, f forge.IssueFilter, page int, etag string) (forge.ListPage, error)
}

// DriverConfig configures the board-ingestion poll driver. The target repos are
// NOT config — the driver reads them from PollStore.ListEnabledRepos at the top
// of every pass.
type DriverConfig struct {
	Interval time.Duration // > 0; the caller defaults it
	Log      *slog.Logger  // nil -> slog.Default()
}

// Driver runs the DL-053 conditional-poll loop: each pass enumerates the enabled
// targets, walks each repo's pages against its stored fetch cursors, sinks each
// 200 page through the Ingester, and advances the durable cursor ONLY after the
// page's issues have sunk (the Idempotency invariant).
type Driver struct {
	client  pageLister
	ing     *Ingester
	cursors PollStore
	log     *slog.Logger
	every   time.Duration
}

// NewDriver returns a Driver polling client's pages, sinking through ing, and
// reading/advancing cursors + targets through cursors.
func NewDriver(client pageLister, ing *Ingester, cursors PollStore, cfg DriverConfig) *Driver {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Driver{
		client:  client,
		ing:     ing,
		cursors: cursors,
		log:     log,
		every:   cfg.Interval,
	}
}

// Run performs an immediate pass, then a pass on every Interval tick, until ctx
// is cancelled — then it returns nil (clean shutdown). Per-repo errors are
// logged and retried next tick, never returned — a re-poll is idempotent on the
// coordinate (ingest.go:48-52).
func (d *Driver) Run(ctx context.Context) error {
	d.pass(ctx)

	t := time.NewTicker(d.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			d.pass(ctx)
		}
	}
}

// pass enumerates the enabled targets and polls each in turn. A repo failure is
// isolated: the pass continues to the next repo. ctx cancellation ends the pass
// promptly (the per-repo walk is deadline-gated through the fetch).
func (d *Driver) pass(ctx context.Context) {
	repos, err := d.cursors.ListEnabledRepos(ctx)
	if err != nil {
		d.log.Error("forge poll: list enabled repos", "err", err)
		return
	}
	for _, repo := range repos {
		if ctx.Err() != nil {
			return
		}
		d.pollRepo(ctx, repo)
	}
}

// pollRepo walks repo's pages against its stored cursors. It returns without
// pruning if the pass aborts (fetch error, budget, or a mid-page sink failure);
// on a walk that completes, it prunes any cursor rows past the last page walked.
func (d *Driver) pollRepo(ctx context.Context, repo string) {
	start := time.Now()

	stored, err := d.cursors.ListCursor(ctx, repo)
	if err != nil {
		d.log.Error("forge poll: list cursor", "repo", repo, "err", err)
		return
	}
	etags := make(map[int]ListPageCursor, len(stored))
	for _, c := range stored {
		etags[c.Page] = c
	}

	var issues, pages int
	page := 1
	for {
		cur := etags[page]
		res, err := d.client.ListIssuesPage(ctx, repo, forge.IssueFilter{}, page, cur.ETag)
		if err != nil {
			if errors.Is(err, forge.ErrBudgetExhausted) {
				d.log.Warn("forge poll: budget exhausted", "repo", repo, "page", page)
				return
			}
			d.log.Error("forge poll: list page", "repo", repo, "page", page, "err", err)
			return
		}

		pages++

		var hasNext bool
		if res.NotModified {
			// 304: the stored cursor row remains truth; the chain continues on
			// the STORED HasNext, not the response.
			hasNext = cur.HasNext
		} else {
			if err := d.ing.IngestIssues(ctx, repo, res.Issues); err != nil {
				// Mid-page sink failure: do NOT advance this page's cursor and
				// abort the repo's pass. Next tick re-fetches with the OLD etag.
				d.log.Error("forge poll: ingest page", "repo", repo, "page", page, "err", err)
				return
			}
			issues += len(res.Issues)
			if err := d.cursors.UpsertListCursorPage(ctx, repo, ListPageCursor{
				Page:    page,
				ETag:    res.ETag,
				HasNext: res.HasNext,
			}); err != nil {
				d.log.Error("forge poll: upsert cursor", "repo", repo, "page", page, "err", err)
				return
			}
			hasNext = res.HasNext
		}

		if !hasNext {
			break
		}
		page++
	}

	if err := d.cursors.PruneListCursorPages(ctx, repo, page); err != nil {
		d.log.Error("forge poll: prune cursors", "repo", repo, "max_page", page, "err", err)
		return
	}

	d.log.Info("forge poll: repo polled",
		"repo", repo, "issues", issues, "pages", pages, "dur", time.Since(start))
}
