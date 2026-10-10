package ingest

// The board webhook arm (RIG-2883 T1): the second consumer behind the one GitHub
// ingress. The handler fans each accepted event to Enqueue (a non-blocking try-
// send), and a drain goroutine hydrates each changed coordinate via a conditional
// GET and sinks it through the shared Ingester — the exact poll-path normalization.

// The webhook payload carries only Number/HTMLURL/State, not the Title/Body/Labels
// TranslateIssue maps, so the arm HYDRATES on event (OQ-4): one GET per distinct
// coordinate. The drain COALESCES per coordinate, so an edit storm of N events on
// one issue costs ONE GET — the event only proves "changed".

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
	// Pulls hydrates PR events onto the board; nil drops every PR event.
	Pulls *PullRequestHydrator
	// PullNumbers maps a CHECKS event's head SHA to its PR; nil drops those events.
	PullNumbers PullNumberResolver
}

// boardCoord is the coalescing key: the drain collapses every queued event for
// one artifact to a single hydrate. A CHECKS event keys on its head SHA until
// the drain resolves it to a PR number.
type boardCoord struct {
	repo    string
	kind    compassv1internal.ForgeArtifactKind
	number  uint64
	headSHA string
}

// BoardWebhookArm consumes board-relevant forge events from the webhook ingress
// and sinks hydrated issues through the shared Ingester pipeline.
type BoardWebhookArm struct {
	queue    chan forge.ForgeEvent
	hydrator issueHydrator
	ing      *Ingester
	pulls    *PullRequestHydrator
	numbers  PullNumberResolver
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
		pulls:    cfg.Pulls,
		numbers:  cfg.PullNumbers,
		targets:  targets,
		log:      log,
	}
}

// Dropped reports the number of events this arm dropped on a full queue (the
// same fact published to the boardWebhookDrops expvar). Test-observable.
func (a *BoardWebhookArm) Dropped() int64 { return a.dropped.Load() }

// Enqueue satisfies server.ForgeEventSink's contract (github_webhook.go:44-51):
// it MUST NOT block. It filters to board-relevant events, then channel
// try-sends; a full queue DROPS the event with the drop metric + a Warn (the
// reconciler heals it).
func (a *BoardWebhookArm) Enqueue(_ context.Context, ev forge.ForgeEvent) {
	if !a.boardRelevant(ev) {
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

// boardRelevant reports whether an event changes what the board shows. Issue
// events count on OPENED, STATE or UPDATE; an issue COMMENT would burn one
// hydrate per comment for nothing shown. Every PR event counts, since reviews
// and checks show on the PR, but only when the arm can hydrate PRs.
func (a *BoardWebhookArm) boardRelevant(ev forge.ForgeEvent) bool {
	switch ev.Kind {
	case compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE:
		switch ev.Change {
		case compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_OPENED,
			compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_STATE,
			compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_UPDATE:
			return true
		default:
			return false
		}
	case compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST:
		if a.pulls == nil {
			return false
		}
		if ev.Number == 0 {
			return ev.HeadSHA != "" && a.numbers != nil
		}
		return true
	default:
		return false
	}
}

// drainBatch coalesces the event that woke the drain and every other queued
// event into distinct keys: normalized repo, kind and number, or head SHA for a
// CHECKS event with no number. An edit storm and a mixed-case duplicate both
// collapse to one key. It then hydrates + sinks each key once, in arrival order.
func (a *BoardWebhookArm) drainBatch(ctx context.Context, first forge.ForgeEvent) {
	seen := map[boardCoord]struct{}{}
	var order []boardCoord

	add := func(ev forge.ForgeEvent) {
		c := boardCoord{repo: normalizeBoardRepo(ev.Repo), kind: ev.Kind, number: ev.Number}
		if ev.Number == 0 {
			c.headSHA = ev.HeadSHA
		}
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
		if err := a.resolveAndSink(ctx, c, seen); err != nil {
			if errors.Is(err, forge.ErrBudgetExhausted) {
				// Budget exhausted pauses the drain: abandon the rest of this
				// batch (the reconciler heals the un-hydrated coordinates) and
				// resume once the client gate reopens.
				a.log.WarnContext(ctx, "board webhook: budget exhausted, pausing drain (reconciler heals)",
					"repo", c.repo, "number", c.number, "head_sha", c.headSHA)
				return
			}
			// Per-event errors log-and-continue (driver.go:96-98 idiom).
			a.log.WarnContext(ctx, "board webhook: hydrate/sink failed (isolated)",
				"repo", c.repo, "number", c.number, "head_sha", c.headSHA, "err", err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// resolveAndSink maps a head-SHA CHECKS key to its PR number, then hydrates.
// A disabled repo spends no lookup. A commit with no PR is skipped, as is a PR
// already in this batch; seen gains the resolved key so a later duplicate is
// skipped too.
func (a *BoardWebhookArm) resolveAndSink(ctx context.Context, c boardCoord, seen map[boardCoord]struct{}) error {
	if c.headSHA != "" {
		enabled, err := a.targets.IsEnabledRepo(ctx, c.repo)
		if err != nil || !enabled {
			return err
		}
		n, err := a.numbers.PullNumberForSHA(ctx, c.repo, c.headSHA)
		if errors.Is(err, forge.ErrNoPullRequestForSHA) {
			return nil
		}
		if err != nil {
			return err
		}
		c = boardCoord{repo: c.repo, kind: c.kind, number: n}
		if _, dup := seen[c]; dup {
			return nil
		}
		seen[c] = struct{}{}
	}
	return a.hydrateAndSink(ctx, c)
}

// hydrateAndSink gates the coordinate's repo, then hydrates the artifact (the
// event proves change, so an issue read sends no stored ETag) and sinks it. A
// non-enabled repo is dropped silently.
func (a *BoardWebhookArm) hydrateAndSink(ctx context.Context, c boardCoord) error {
	enabled, err := a.targets.IsEnabledRepo(ctx, c.repo)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	if c.kind == compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST {
		return a.pulls.Hydrate(ctx, c.repo, c.number)
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
