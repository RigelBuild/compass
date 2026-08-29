package ingest

// The board webhook arm (RIG-2883 T1, design.md:227-302): the second consumer
// behind the one GitHub ingress. The webhook handler fans each accepted event
// to this arm's Enqueue (a non-blocking channel try-send, github_webhook.go:44-51),
// and a single drain goroutine hydrates each changed coordinate via a
// conditional GET and sinks the fresh issue through the shared Ingester — the
// exact poll-path normalization (ingest.go:82-99), never re-implemented.
//
// The webhook payload carries only Number/HTMLURL/State (whIssue,
// githubapp_webhook.go:52-57), not the Title/Body/Labels TranslateIssue maps,
// so the arm HYDRATES on event (OQ-4): one conditional GET per DISTINCT changed
// coordinate. The drain COALESCES per coordinate first (design.md:269-280): an
// edit storm of N rapid events on one issue costs ONE GET, not N — the event
// only proves "changed", so one fresh read serves the whole burst.

import (
	"context"
	"errors"
	"expvar"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// boardWebhookDrops is the exported queue-full/drop metric (design.md:288-292):
// a sustained queue-full silently degrades the hot path to a de-facto 30-min
// poll (the reconciler heal ceiling), so the drop is scrapeable for an alerting
// threshold — a counter+Warn alone is not enough to notice the degradation.
var boardWebhookDrops = expvar.NewInt("compass_board_webhook_drops")

// defaultBoardQueueSize is the bounded drain-queue depth. Sized to absorb a
// normal edit-storm burst between drains; a sustained overflow (the drain paused
// on ErrBudgetExhausted while events keep arriving) drops with the metric+Warn
// and is healed by the T3 reconciler.
const defaultBoardQueueSize = 1024

// issueHydrator is the conditional point-read seam (satisfied structurally by
// *forge.GitHub via GetIssueConditional, notify_reader.go:129). Defined locally
// so this package imports no concrete forge client on the hydrate path.
type issueHydrator interface {
	GetIssueConditional(ctx context.Context, repo string, number uint64, etag string) (forge.ConditionalResult[forge.Issue], error)
}

// TargetChecker gates events to subscribed repos (forge_repo_subscriptions
// WHERE enabled) — the DL-162 target model, point-checked per event. A local
// interface; *store.Store satisfies it at the T5 wiring (this package imports no
// store, ingest.go:7-8).
type TargetChecker interface {
	IsEnabledRepo(ctx context.Context, repo string) (bool, error)
}

// BoardArmConfig configures the board webhook arm.
type BoardArmConfig struct {
	// QueueSize is the bounded drain-queue depth; <= 0 uses defaultBoardQueueSize.
	QueueSize int
	// Log is the arm logger; nil uses slog.Default().
	Log *slog.Logger
}

// boardCoord is the (repo, number) coalescing key: the drain collapses every
// queued event for one coordinate to a single hydrate GET.
type boardCoord struct {
	repo   string
	number uint64
}

// BoardWebhookArm consumes board-relevant forge events from the webhook ingress
// and sinks hydrated issues through the shared Ingester pipeline.
type BoardWebhookArm struct {
	queue    chan forge.ForgeEvent
	hydrator issueHydrator
	ing      *Ingester
	targets  TargetChecker
	log      *slog.Logger
	dropped  atomic.Int64
}

// NewBoardWebhookArm returns an arm that hydrates each accepted event through h,
// gates repos through targets, and sinks through ing.
func NewBoardWebhookArm(h issueHydrator, ing *Ingester, targets TargetChecker, cfg BoardArmConfig) *BoardWebhookArm {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	size := cfg.QueueSize
	if size <= 0 {
		size = defaultBoardQueueSize
	}
	return &BoardWebhookArm{
		queue:    make(chan forge.ForgeEvent, size),
		hydrator: h,
		ing:      ing,
		targets:  targets,
		log:      log,
	}
}

// Dropped reports the number of events this arm dropped on a full queue (the
// same fact published to the boardWebhookDrops expvar). Test-observable.
func (a *BoardWebhookArm) Dropped() int64 { return a.dropped.Load() }

// Enqueue satisfies server.ForgeEventSink's contract (github_webhook.go:44-51):
// it MUST NOT block. It filters to board-relevant issue events (Change ∈
// {OPENED, STATE, UPDATE} ∧ Kind == ISSUE — PR events and COMMENT-change events
// dropped, design.md:231-237) then channel try-sends; a full queue DROPS the
// event with the drop metric + a Warn (the T3 reconciler heals it).
func (a *BoardWebhookArm) Enqueue(_ context.Context, ev forge.ForgeEvent) {
	if !boardRelevant(ev) {
		return
	}
	select {
	case a.queue <- ev:
	default:
		a.dropped.Add(1)
		boardWebhookDrops.Add(1)
		a.log.Warn("board webhook: queue full, dropping event (reconciler heals)",
			"repo", ev.Repo, "number", ev.Number, "change", ev.Change)
	}
}

// boardRelevant reports whether an event is a board-relevant issue change:
// Kind == ISSUE and Change ∈ {OPENED, STATE, UPDATE}. COMMENT-change issue
// events (issue_comment also parses to Kind ISSUE, githubapp_webhook.go:196-210)
// and every PR-kind event are excluded — the board projects issues only, and
// admitting comments would burn one hydrate GET per comment (design.md:231-237).
func boardRelevant(ev forge.ForgeEvent) bool {
	if ev.Kind != compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE {
		return false
	}
	switch ev.Change {
	case compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_OPENED,
		compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_STATE,
		compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_UPDATE:
		return true
	default:
		return false
	}
}

// Run drains the queue until ctx is cancelled — then it returns nil (clean
// shutdown, driver.go:95-99 idiom). Each drain COALESCES per coordinate before
// hydrating, so an N-event burst on one issue costs one GET.
func (a *BoardWebhookArm) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-a.queue:
			a.drainBatch(ctx, ev)
		}
	}
}

// drainBatch coalesces first into a distinct-coordinate set (design.md:271-274):
// it seeds with the event that woke the drain, non-blockingly drains every other
// currently-queued event, and keys each on its NORMALIZED (repo, number) — so an
// edit storm on one issue, and a mixed-case duplicate of one repo, both collapse
// to a single coordinate. It then hydrates + sinks each distinct coordinate once,
// in arrival order.
func (a *BoardWebhookArm) drainBatch(ctx context.Context, first forge.ForgeEvent) {
	seen := map[boardCoord]struct{}{}
	var order []boardCoord

	add := func(ev forge.ForgeEvent) {
		c := boardCoord{repo: normalizeBoardRepo(ev.Repo), number: ev.Number}
		if _, ok := seen[c]; ok {
			return
		}
		seen[c] = struct{}{}
		order = append(order, c)
	}

	add(first)
	for {
		select {
		case ev := <-a.queue:
			add(ev)
		default:
			goto process
		}
	}

process:
	for _, c := range order {
		if err := a.hydrateAndSink(ctx, c); err != nil {
			if errors.Is(err, forge.ErrBudgetExhausted) {
				// Budget exhausted pauses the drain: abandon the rest of this
				// batch (the reconciler heals the un-hydrated coordinates), and
				// the next batch resumes once the client gate reopens
				// (github.go:83-102) — the reconciler's treatment
				// (notify_reconcile.go:139-142).
				a.log.WarnContext(ctx, "board webhook: budget exhausted, pausing drain (reconciler heals)",
					"repo", c.repo, "number", c.number)
				return
			}
			// Per-event errors log-and-continue (driver.go:96-98 idiom).
			a.log.WarnContext(ctx, "board webhook: hydrate/sink failed (isolated)",
				"repo", c.repo, "number", c.number, "err", err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// hydrateAndSink gates the coordinate's repo, hydrates the issue via an
// unconditional conditional GET (the event proves change; no stored per-issue
// ETag in v1, design.md:276-277), and sinks the fresh issue through the shared
// Ingester (ingest.go:82-99 — the one owner-strip/translate/stamp pipeline). A
// non-enabled repo is dropped silently.
func (a *BoardWebhookArm) hydrateAndSink(ctx context.Context, c boardCoord) error {
	enabled, err := a.targets.IsEnabledRepo(ctx, c.repo)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	res, err := a.hydrator.GetIssueConditional(ctx, c.repo, c.number, "")
	if err != nil {
		return err
	}
	if res.NotModified {
		// Unreachable with an empty ETag (a 200-equivalent); defensively skip.
		return nil
	}
	return a.ing.IngestIssues(ctx, c.repo, []forge.Issue{res.V})
}

// normalizeBoardRepo lowercases the event repo at the boundary (Global
// Constraint 8, design.md:213-225): ParseGitHubEvent sets Repo from the
// case-PRESERVED payload full_name, while subscription rows and board
// coordinates are lowercased at the seed/upsert boundary — so a raw event repo
// would silently miss the IsEnabledRepo row and mint a duplicate coordinate.
func normalizeBoardRepo(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}
