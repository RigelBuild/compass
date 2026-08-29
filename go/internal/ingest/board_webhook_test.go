package ingest

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// fakeHydrator is a scripted issueHydrator that records every GetIssueConditional
// call (repo+number) so a test can assert the coalesce lever at the call count.
type fakeHydrator struct {
	mu      sync.Mutex
	calls   []boardCoord
	result  forge.Issue
	err     error
	errOnce bool // if true, return err only on the first call, then succeed
	served  int
}

func (h *fakeHydrator) GetIssueConditional(_ context.Context, repo string, number uint64, _ string) (forge.ConditionalResult[forge.Issue], error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, boardCoord{repo: repo, number: number})
	h.served++
	if h.err != nil && (!h.errOnce || h.served == 1) {
		return forge.ConditionalResult[forge.Issue]{}, h.err
	}
	res := h.result
	res.Number = number
	return forge.ConditionalResult[forge.Issue]{V: res}, nil
}

func (h *fakeHydrator) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

// fakeTargets is a scripted TargetChecker: a repo is enabled iff it is in the
// lowercased enabled set. It records every checked repo.
type fakeTargets struct {
	mu       sync.Mutex
	enabled  map[string]bool
	checked  []string
	checkErr error
}

func newFakeTargets(repos ...string) *fakeTargets {
	m := map[string]bool{}
	for _, r := range repos {
		m[r] = true
	}
	return &fakeTargets{enabled: m}
}

func (f *fakeTargets) IsEnabledRepo(_ context.Context, repo string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checked = append(f.checked, repo)
	if f.checkErr != nil {
		return false, f.checkErr
	}
	return f.enabled[repo], nil
}

// newArmHarness wires a fake hydrator + targets + a recording sink into a
// BoardWebhookArm with a real Ingester (so the full owner-strip/translate/stamp
// pipeline runs) and a small queue for the full-queue test.
func newArmHarness(t *testing.T, h issueHydrator, targets TargetChecker, queueSize int) (*BoardWebhookArm, *recordingSink) {
	t.Helper()
	sink := &recordingSink{}
	// The Ingester's forgeReader is unused on the webhook path (IngestIssues
	// takes caller-fetched issues), so a bare fake provider suffices.
	ing := NewIngester(forge.NewFakeProvider("gh"), sink, testForgeRef())
	arm := NewBoardWebhookArm(h, ing, targets, BoardArmConfig{QueueSize: queueSize})
	return arm, sink
}

func issueEvent(repo string, number uint64, change compassv1internal.ForgeNotificationKind) forge.ForgeEvent {
	return forge.ForgeEvent{
		Repo:   repo,
		Number: number,
		Kind:   compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_ISSUE,
		Change: change,
	}
}

const (
	changeUpdate  = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_UPDATE
	changeState   = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_STATE
	changeComment = compassv1internal.ForgeNotificationKind_FORGE_NOTIFICATION_KIND_COMMENT
	boardKindPR   = compassv1internal.ForgeArtifactKind_FORGE_ARTIFACT_KIND_PULL_REQUEST
)

// drainAll deterministically processes every currently-queued event by pulling
// one head event and letting drainBatch coalesce+drain the rest — no goroutine,
// no clock. It mirrors exactly one Run iteration per queued head, so the result
// is the same as Run would produce, but synchronously observable.
func drainAll(ctx context.Context, arm *BoardWebhookArm) {
	for {
		select {
		case ev := <-arm.queue:
			arm.drainBatch(ctx, ev)
		default:
			return
		}
	}
}

// TestArmHydratesAndSinks: a board-relevant issue event hydrates the coordinate
// and the fresh issue reaches PublishIssueUpdate with a stripped body and a
// stamped ForgeRef (the full pipeline ran).
func TestArmHydratesAndSinks(t *testing.T) {
	body := stamped(t, "real body", forge.Author{AgentHandle: "atlas", OwnerHandle: "matt", SessionID: "s1"})
	h := &fakeHydrator{result: forge.Issue{Title: "a bug", Body: body, State: "open"}}
	arm, sink := newArmHarness(t, h, newFakeTargets("owner/repo"), 16)

	arm.Enqueue(context.Background(), issueEvent("owner/repo", 7, changeUpdate))
	drainAll(context.Background(), arm)

	if len(sink.got) != 1 {
		t.Fatalf("sink got %d issues, want 1", len(sink.got))
	}
	got := sink.got[0]
	if got.GetNumber() != 7 {
		t.Errorf("Number = %d, want 7", got.GetNumber())
	}
	if got.GetBody() != "real body" {
		t.Errorf("Body = %q, want stripped %q", got.GetBody(), "real body")
	}
	if got.GetAgent().GetAgentHandle() != "atlas" {
		t.Errorf("Agent.AgentHandle = %q, want atlas", got.GetAgent().GetAgentHandle())
	}
	if got.GetForge().GetProvider() != testForgeRef().GetProvider() || got.GetRepo() != "owner/repo" {
		t.Errorf("ForgeRef/Repo not stamped: forge=%v repo=%q", got.GetForge(), got.GetRepo())
	}
	if h.callCount() != 1 {
		t.Errorf("hydrate calls = %d, want 1", h.callCount())
	}
}

// TestArmDropsNonEnabledRepo: an event for a repo with no enabled subscription
// row is gated out before the hydrate — no sink, no hydrate GET spent.
func TestArmDropsNonEnabledRepo(t *testing.T) {
	h := &fakeHydrator{result: forge.Issue{Title: "x", State: "open"}}
	arm, sink := newArmHarness(t, h, newFakeTargets(), 16)

	arm.Enqueue(context.Background(), issueEvent("owner/repo", 7, changeUpdate))
	drainAll(context.Background(), arm)

	if len(sink.got) != 0 {
		t.Fatalf("sink got %d, want 0 for a non-enabled repo", len(sink.got))
	}
	if h.callCount() != 0 {
		t.Fatalf("hydrate calls = %d, want 0 (gate before hydrate)", h.callCount())
	}
}

// TestArmDropsPRKind: a pull_request-kind event is filtered at Enqueue — never
// queued, never hydrated.
func TestArmDropsPRKind(t *testing.T) {
	h := &fakeHydrator{result: forge.Issue{State: "open"}}
	arm, sink := newArmHarness(t, h, newFakeTargets("owner/repo"), 16)

	ev := issueEvent("owner/repo", 7, changeUpdate)
	ev.Kind = boardKindPR
	arm.Enqueue(context.Background(), ev)

	if l := len(arm.queue); l != 0 {
		t.Fatalf("queue len = %d, want 0 (PR-kind filtered at Enqueue)", l)
	}
	drainAll(context.Background(), arm)
	if len(sink.got) != 0 || h.callCount() != 0 {
		t.Fatalf("PR-kind leaked: sink=%d hydrate=%d", len(sink.got), h.callCount())
	}
}

// TestArmDropsCommentChange: a COMMENT-change issue event is filtered at Enqueue
// — no hydrate GET is spent (the cost bound, design.md:233-237).
func TestArmDropsCommentChange(t *testing.T) {
	h := &fakeHydrator{result: forge.Issue{State: "open"}}
	arm, sink := newArmHarness(t, h, newFakeTargets("owner/repo"), 16)

	arm.Enqueue(context.Background(), issueEvent("owner/repo", 7, changeComment))
	if l := len(arm.queue); l != 0 {
		t.Fatalf("queue len = %d, want 0 (COMMENT-change filtered)", l)
	}
	drainAll(context.Background(), arm)
	if len(sink.got) != 0 || h.callCount() != 0 {
		t.Fatalf("COMMENT-change leaked: sink=%d hydrate=%d", len(sink.got), h.callCount())
	}
}

// TestArmNormalizesMixedCaseRepo: a mixed-case full_name is lowercased before
// the gate and sink, so the lowercased subscription row matches; a same-issue
// mixed-case duplicate coalesces to one coordinate (one hydrate GET, one sink).
func TestArmNormalizesMixedCaseRepo(t *testing.T) {
	h := &fakeHydrator{result: forge.Issue{State: "open"}}
	targets := newFakeTargets("owner/repo")
	arm, sink := newArmHarness(t, h, targets, 16)

	// Two events on the same coordinate, differing only by repo case.
	arm.Enqueue(context.Background(), issueEvent("Owner/Repo", 7, changeUpdate))
	arm.Enqueue(context.Background(), issueEvent("owner/repo", 7, changeState))
	drainAll(context.Background(), arm)

	if len(sink.got) != 1 {
		t.Fatalf("sink got %d, want 1 (mixed-case dup coalesced)", len(sink.got))
	}
	if h.callCount() != 1 {
		t.Fatalf("hydrate calls = %d, want 1 (one coordinate)", h.callCount())
	}
	if got := sink.got[0].GetRepo(); got != "owner/repo" {
		t.Errorf("sinked Repo = %q, want lowercased owner/repo", got)
	}
	for _, r := range targets.checked {
		if r != strings.ToLower(r) {
			t.Errorf("IsEnabledRepo checked non-normalized repo %q", r)
		}
	}
}

// TestArmFullQueueDropsWithoutBlocking: with the drain not running, a queue of
// capacity 1 accepts one event and DROPS the second without blocking Enqueue.
func TestArmFullQueueDropsWithoutBlocking(t *testing.T) {
	h := &fakeHydrator{result: forge.Issue{State: "open"}}
	arm, _ := newArmHarness(t, h, newFakeTargets("owner/repo"), 1)

	arm.Enqueue(context.Background(), issueEvent("owner/repo", 1, changeUpdate))
	// Second enqueue must not block (drain is not running) and must drop.
	done := make(chan struct{})
	go func() {
		arm.Enqueue(context.Background(), issueEvent("owner/repo", 2, changeUpdate))
		close(done)
	}()
	<-done // gates on Enqueue returning; it hangs the test (no close) if it blocks
	if arm.Dropped() != 1 {
		t.Fatalf("Dropped() = %d, want 1", arm.Dropped())
	}
}

// TestArmHydrateErrorRetriesNextEvent: a hydrate error on one coordinate is
// isolated (log-and-continue) and never crashes the drain — a subsequent event
// on a different coordinate is hydrated and sinked.
func TestArmHydrateErrorRetriesNextEvent(t *testing.T) {
	h := &fakeHydrator{result: forge.Issue{State: "open"}, err: errBoom, errOnce: true}
	arm, sink := newArmHarness(t, h, newFakeTargets("owner/repo"), 16)

	arm.Enqueue(context.Background(), issueEvent("owner/repo", 1, changeUpdate))
	drainAll(context.Background(), arm)
	// First coordinate errored: nothing sinked, drain did not crash.
	if len(sink.got) != 0 {
		t.Fatalf("sink got %d after the erroring event, want 0", len(sink.got))
	}
	// A second event succeeds.
	arm.Enqueue(context.Background(), issueEvent("owner/repo", 2, changeUpdate))
	drainAll(context.Background(), arm)
	if len(sink.got) != 1 {
		t.Fatalf("sink got %d, want 1 (recovered after error)", len(sink.got))
	}
	if sink.got[0].GetNumber() != 2 {
		t.Errorf("sinked Number = %d, want 2", sink.got[0].GetNumber())
	}
}

// TestArmCoalescesBurst: a burst of N events on ONE coordinate, all queued
// before the drain wakes, coalesces to ONE hydrate GET (the coalesce lever,
// observable at the fake hydrator call count).
func TestArmCoalescesBurst(t *testing.T) {
	h := &fakeHydrator{result: forge.Issue{State: "open"}}
	arm, sink := newArmHarness(t, h, newFakeTargets("owner/repo"), 64)

	for range 20 {
		arm.Enqueue(context.Background(), issueEvent("owner/repo", 7, changeUpdate))
	}
	drainAll(context.Background(), arm)

	if h.callCount() != 1 {
		t.Fatalf("hydrate calls = %d, want 1 (20-event burst coalesced to one GET)", h.callCount())
	}
	if len(sink.got) != 1 {
		t.Fatalf("sink got %d, want 1", len(sink.got))
	}
}

// TestArmRunReturnsNilOnCancel: Run drains until ctx cancel and returns nil
// (driver.go:95-99 idiom). Gated on the result channel, not a clock.
func TestArmRunReturnsNilOnCancel(t *testing.T) {
	h := &fakeHydrator{result: forge.Issue{State: "open"}}
	arm, _ := newArmHarness(t, h, newFakeTargets("owner/repo"), 16)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- arm.Run(ctx) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v, want nil on cancel", err)
	}
}
