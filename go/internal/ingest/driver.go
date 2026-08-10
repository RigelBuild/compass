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
	// ProbeEveryAll304 sets how often a repo whose walk served only 304s
	// triggers a lastPage+1 boundary probe (M1).
	ProbeEveryAll304 int // every Nth consecutive all-304 walk triggers a lastPage+1 boundary probe (M1); <=0 -> default 60
}

// defaultAll304ProbeEvery is the default M1 boundary-probe cadence: every 60th
// consecutive all-304 walk of a repo triggers one lastPage+1 probe (~hourly at
// the 1m poll interval).
const defaultAll304ProbeEvery = 60

// Driver runs the DL-053 conditional-poll loop: each pass enumerates the enabled
// targets, walks each repo's pages against its stored fetch cursors, sinks each
// 200 page through the Ingester, and advances the durable cursor ONLY after the
// page's issues have sunk (the Idempotency invariant).
type Driver struct {
	client     pageLister
	ing        *Ingester
	cursors    PollStore
	log        *slog.Logger
	every      time.Duration
	probeEvery int            // M1 boundary-probe cadence (see defaultAll304ProbeEvery)
	all304     map[string]int // per-repo consecutive all-304 walk counter
}

// NewDriver returns a Driver polling client's pages, sinking through ing, and
// reading/advancing cursors + targets through cursors.
func NewDriver(client pageLister, ing *Ingester, cursors PollStore, cfg DriverConfig) *Driver {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	probeEvery := cfg.ProbeEveryAll304
	if probeEvery <= 0 {
		probeEvery = defaultAll304ProbeEvery
	}
	return &Driver{
		client:     client,
		ing:        ing,
		cursors:    cursors,
		log:        log,
		every:      interval,
		probeEvery: probeEvery,
		all304:     map[string]int{},
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
	if ctx.Err() != nil {
		return
	}
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

	var issues, pages, lastRemaining int
	advanced := false // any 200 page whose issues sank this walk
	probing := false  // this fetch is the M1 boundary probe (force etag="")
	probed := false   // the probe already ran this walk (never probe twice)
	probeAnchor := 0  // the stored tail page a probe extended (0 = no probe fired)
	page := 1
	for {
		cur := etags[page]
		etag := cur.ETag
		if probing {
			// M1 probe of lastPage+1: an unconditional GET (a never-stored page
			// already has an empty etag, but force it so a stray stored row
			// cannot make the probe conditional).
			etag = ""
			probing = false
		}
		res, err := d.client.ListIssuesPage(ctx, repo, forge.IssueFilter{}, page, etag)
		if err != nil {
			if errors.Is(err, forge.ErrBudgetExhausted) {
				d.log.Warn("forge poll: budget exhausted", "repo", repo, "page", page)
				return
			}
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				// Shutdown mid-walk: return cleanly, no spurious Error (LOW-1).
				return
			}
			d.log.Error("forge poll: list page", "repo", repo, "page", page, "err", err)
			return
		}

		pages++
		lastRemaining = res.RateLimitRemaining

		var hasNext bool
		if res.NotModified {
			// 304: the stored cursor row remains truth; the chain continues on
			// the STORED HasNext, not the response.
			hasNext = cur.HasNext
		} else {
			// Zero-issue upsert-skip (design.md:557-560): a 200 with no issues
			// sinks nothing, writes NO cursor row, and does not count as an
			// advance — it still drives the walk on res.HasNext. This keeps an
			// empty probe page from persisting a tail row that would 304 forever
			// and sit outside PruneListCursorPages' post-walk drop.
			if len(res.Issues) > 0 {
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
				advanced = true
			}
			hasNext = res.HasNext
		}

		if hasNext {
			page++
			continue
		}

		// The walk reached its tail at `page` (== lastPage). Decide the M1
		// boundary probe (design.md:545-561).
		if !probed && !advanced {
			// A true all-304 walk: bump the per-repo streak and, every Nth such
			// consecutive walk, probe lastPage+1 once to catch content that
			// entered sorted beyond the stored boundary.
			d.all304[repo]++
			if d.all304[repo] >= d.probeEvery {
				d.all304[repo] = 0
				probed = true
				probing = true
				probeAnchor = page // the stored tail; promote it if the probe re-anchors
				page++
				continue
			}
			break
		}
		if advanced {
			// Any content advance (including a probe that re-anchored the tail)
			// resets the streak.
			d.all304[repo] = 0
		}
		break
	}

	if err := d.reanchorTail(ctx, repo, etags[probeAnchor], advanced); err != nil {
		d.log.Error("forge poll: re-anchor cursor", "repo", repo, "page", probeAnchor, "err", err)
		return
	}

	if err := d.cursors.PruneListCursorPages(ctx, repo, page); err != nil {
		d.log.Error("forge poll: prune cursors", "repo", repo, "max_page", page, "err", err)
		return
	}

	// not_modified = !advanced follows the design's OPERATIONAL definition of an
	// all-304 walk ("no page advanced", design.md:550): a plain 200 with zero
	// issues sinks nothing, so it reads not_modified=true and counts toward the
	// all-304 probe streak. That is intentional — a zero-issue page-1 implies no
	// tail, so the streak cannot miss real content — not a bug to "fix" to a
	// literal saw-a-200 bool.
	d.log.Info("forge poll: repo polled",
		"repo", repo, "issues", issues, "pages", pages,
		"not_modified", !advanced, "dur", time.Since(start),
		"ratelimit_remaining", lastRemaining)
}

// reanchorTail durably re-anchors an M1 boundary probe (design.md:555-556). A
// probe that found content beyond the stored boundary (advanced) extended the
// tail, but the anchor page it grew from was served a 304 this walk, so its
// stored HasNext is still false. Promoting the anchor to HasNext=true
// (preserving its ETag) makes the next conditional walk thread the 304 chain
// THROUGH the grown tail rather than stopping at the anchor — which would prune
// the freshly-sunk pages and force a re-probe + full-tail re-sink every
// probeEvery walks. A no-op unless a probe re-anchored: anchor is the zero
// cursor (empty ETag) when no probe fired, and advanced is false when the probe
// found nothing.
func (d *Driver) reanchorTail(ctx context.Context, repo string, anchor ListPageCursor, advanced bool) error {
	if !advanced || anchor.ETag == "" {
		return nil
	}
	anchor.HasNext = true
	return d.cursors.UpsertListCursorPage(ctx, repo, anchor)
}
