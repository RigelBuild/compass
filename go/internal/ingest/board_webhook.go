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
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
)

// boardWebhookDrops exposes queue-full loss; sustained drops degrade updates to the slow reconcile cadence.
// The counter supports an alert beyond the local warning.
var boardWebhookDrops = expvar.NewInt("compass_board_webhook_drops")

// defaultBoardQueueSize absorbs normal bursts; sustained overflow drops events.
// Reconcile heals issue drops; CI-only PR changes wait for another event.
const defaultBoardQueueSize = 1024

// maxHeldTraceparents caps the traces a held PR key collects, so a busy PR
// held through a long budget pause cannot grow its slice without bound.
const maxHeldTraceparents = 32

// defaultBoardRetryWait re-drains held PR keys when the budget error gave no
// reset hint.
const defaultBoardRetryWait = time.Minute

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
	queue    chan queuedEvent
	hydrator issueHydrator
	ing      *Ingester
	pulls    *PullRequestHydrator
	numbers  PullNumberResolver
	targets  TargetChecker
	log      *slog.Logger
	dropped  atomic.Int64

	// pending holds PR keys a budget pause left un-hydrated, each with the
	// traceparents its events carried. The sweep cannot heal a CI-only change,
	// so the drain retries them. Drain goroutine only.
	pending    map[boardCoord][]string
	maxPending int
	after      func(time.Duration) <-chan time.Time
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
		queue:    make(chan queuedEvent, size),
		hydrator: h,
		ing:      ing,
		pulls:    cfg.Pulls,
		numbers:  cfg.PullNumbers,
		targets:  targets,
		log:      log,

		pending:    map[boardCoord][]string{},
		maxPending: size,
		after:      time.After,
	}
}

// Dropped reports the number of events this arm dropped on a full queue (the
// same fact published to the boardWebhookDrops expvar). Test-observable.
func (a *BoardWebhookArm) Dropped() int64 { return a.dropped.Load() }

// Enqueue satisfies server.ForgeEventSink's contract (github_webhook.go:44-51):
// it MUST NOT block. It filters to board-relevant events, then channel
// try-sends; a full queue DROPS the event with the drop metric + a Warn (the
// reconciler heals it).
func (a *BoardWebhookArm) Enqueue(ctx context.Context, ev forge.ForgeEvent) {
	if !a.boardRelevant(ev) {
		return
	}
	select {
	case a.queue <- queuedEvent{ev: ev, traceparent: otelx.Traceparent(ctx)}:
	default:
		a.dropped.Add(1)
		boardWebhookDrops.Add(1)
		a.log.WarnContext(ctx, "board webhook: queue full, dropping event (reconciler heals)",
			"repo", ev.Repo, "number", ev.Number, "change", ev.Change)
	}
}

// Run drains the queue until ctx is cancelled — then it returns nil (clean
// shutdown, driver.go:95-99 idiom). Each drain COALESCES per coordinate before
// hydrating, so an N-event burst on one issue costs one GET. After a budget
// pause it re-drains the held PR keys once the gate's reset hint passes.
func (a *BoardWebhookArm) Run(ctx context.Context) error {
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case q := <-a.queue:
			if wait, paused := a.drainBatch(ctx, q); paused && retry == nil {
				retry = a.retryTimer(wait)
			}
		case <-retry:
			retry = nil
			if wait, paused := a.retryPending(ctx); paused {
				retry = a.retryTimer(wait)
			}
		}
	}
}

// retryTimer arms the re-drain for held PR keys; nil when none are held.
func (a *BoardWebhookArm) retryTimer(wait time.Duration) <-chan time.Time {
	if len(a.pending) == 0 {
		return nil
	}
	if wait <= 0 {
		wait = defaultBoardRetryWait
	}
	return a.after(wait)
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

// drainBatch coalesces queued events by repo, kind, and number (or head SHA for checks).
// Held keys wait for retry timers so one exhausted PR cannot pause fresh work.
func (a *BoardWebhookArm) drainBatch(ctx context.Context, first queuedEvent) (time.Duration, bool) {
	seen := map[boardCoord]struct{}{}
	carried := map[boardCoord][]string{}
	var order []boardCoord
	add := func(q queuedEvent) {
		ev := q.ev
		c := boardCoord{repo: normalizeBoardRepo(ev.Repo), kind: ev.Kind, number: ev.Number}
		if ev.Number == 0 {
			c.headSHA = ev.HeadSHA
		}
		if _, held := a.pending[c]; held {
			if q.traceparent != "" && len(a.pending[c]) < maxHeldTraceparents {
				a.pending[c] = append(a.pending[c], q.traceparent)
			}
			return
		}
		if _, ok := seen[c]; !ok {
			seen[c] = struct{}{}
			order = append(order, c)
		}
		if q.traceparent != "" {
			carried[c] = append(carried[c], q.traceparent)
		}
	}

	add(first)
	for {
		select {
		case q := <-a.queue:
			add(q)
		default:
			return a.process(ctx, order, seen, carried)
		}
	}
}

// retryPending re-drains the PR keys held by an earlier budget pause.
func (a *BoardWebhookArm) retryPending(ctx context.Context) (time.Duration, bool) {
	order, seen, carried := a.takePending()
	return a.process(ctx, order, seen, carried)
}

// takePending empties the held PR keys into a fresh batch.
func (a *BoardWebhookArm) takePending() ([]boardCoord, map[boardCoord]struct{}, map[boardCoord][]string) {
	seen := make(map[boardCoord]struct{}, len(a.pending))
	order := make([]boardCoord, 0, len(a.pending))
	carried := make(map[boardCoord][]string, len(a.pending))
	for c, tps := range a.pending {
		seen[c] = struct{}{}
		order = append(order, c)
		carried[c] = tps
	}
	clear(a.pending)
	return order, seen, carried
}

// hold keeps the batch's un-hydrated PR keys for a retry; issue keys are left to
// the sweep, which sees their updated_at move. Past maxPending a key is dropped.
func (a *BoardWebhookArm) hold(rest []boardCoord, carried map[boardCoord][]string) {
	for _, c := range rest {
		if c.kind != compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST {
			continue
		}
		if _, ok := a.pending[c]; !ok && len(a.pending) >= a.maxPending {
			a.dropped.Add(1)
			boardWebhookDrops.Add(1)
			continue
		}
		tps := carried[c]
		a.pending[c] = tps[:min(len(tps), maxHeldTraceparents)]
	}
}

// process hydrates each key once. A budget error holds the remaining PR keys
// and returns the gate's reset hint.
func (a *BoardWebhookArm) process(ctx context.Context, order []boardCoord, seen map[boardCoord]struct{}, carried map[boardCoord][]string) (time.Duration, bool) {
	// The pass is a root on the loop ctx; it merges many deliveries, so it links
	// to each carried trace instead of picking one parent.
	ctx, span := otel.Tracer(instrumentationScope).Start(otelx.WithoutSpan(ctx), "ingest.board.drain",
		trace.WithLinks(carriedLinks(ctx, order, carried)...))
	defer span.End()
	for i, c := range order {
		if err := a.resolveAndSink(ctx, c, seen); err != nil {
			if errors.Is(err, forge.ErrBudgetExhausted) {
				a.hold(order[i:], carried)
				var wait time.Duration
				if rle, ok := errors.AsType[*forge.RateLimitError](err); ok {
					wait = rle.RetryAfter
				}
				a.log.WarnContext(ctx, "board webhook: budget exhausted, pausing drain",
					"repo", c.repo, "number", c.number, "head_sha", c.headSHA, "held", len(a.pending))
				return wait, true
			}
			// Per-event errors log-and-continue (driver.go:96-98 idiom).
			a.log.WarnContext(ctx, "board webhook: hydrate/sink failed (isolated)",
				"repo", c.repo, "number", c.number, "head_sha", c.headSHA, "err", err)
		}
		if ctx.Err() != nil {
			return 0, false
		}
	}
	return 0, false
}

// carriedLinks turns the traceparents carried by the pass's keys into links.
func carriedLinks(ctx context.Context, order []boardCoord, carried map[boardCoord][]string) []trace.Link {
	var links []trace.Link
	for _, c := range order {
		for _, tp := range carried[c] {
			if sc := trace.SpanContextFromContext(otelx.ContextWithTraceparent(ctx, tp)); sc.IsValid() {
				links = append(links, trace.Link{SpanContext: sc})
			}
		}
	}
	return links
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

// normalizeBoardRepo matches the lowercase subscription coordinates used by lookups.
// Event payloads preserve case, so using them directly would miss the row and duplicate the coordinate.
func normalizeBoardRepo(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}
