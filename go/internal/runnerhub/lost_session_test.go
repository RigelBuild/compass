//go:build unix

package runnerhub

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

type lostReport struct {
	account store.AccountID
	errored bool
}

// recordingLostSink records each report on a channel: the ERRORED drop is
// detached, so a test waits on the report instead of reading a slice.
type recordingLostSink struct{ reported chan lostReport }

func newRecordingLostSink() *recordingLostSink {
	return &recordingLostSink{reported: make(chan lostReport, 8)}
}

func (s *recordingLostSink) OnSessionLost(_ string, account store.AccountID, errored bool) {
	s.reported <- lostReport{account: account, errored: errored}
}

func (s *recordingLostSink) waitOne(t *testing.T) (store.AccountID, bool) {
	t.Helper()
	select {
	case r := <-s.reported:
		return r.account, r.errored
	case <-time.After(10 * time.Second):
		t.Fatal("no loss report within 10s")
		return "", false
	}
}

func (s *recordingLostSink) none(t *testing.T, why string) {
	t.Helper()
	select {
	case r := <-s.reported:
		t.Fatalf("%s: got loss report %+v, want none", why, r)
	default:
	}
}

// A NotFound deliver refusal releases the binding and reports the account, but only
// when the refusing Runner owns the session: a foreign Runner must not unbind it.
func TestDropLostSessionOnlyForOwningRunner(t *testing.T) {
	ctx := context.Background()
	hub := newHubOnly()
	bindings := newFakeBindingStore()
	hub.SetSessionBindingStore(bindings)
	sink := newRecordingLostSink()
	hub.SetSessionLostSink(sink)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("cont-1", testAgentAccount, "runner-1")
	hub.promoteSession(ctx, "cont-1", "sess-1")

	hub.dropLostSession(ctx, "runner-other", "sess-1", false)
	if _, ok := hub.accountForSession(ctx, "sess-1"); !ok {
		t.Fatal("foreign refusal unbound sess-1")
	}
	sink.none(t, "foreign refusal")

	hub.dropLostSession(ctx, "runner-1", "sess-1", false)
	if _, ok := hub.accountForSession(ctx, "sess-1"); ok {
		t.Fatal("owning refusal left sess-1 bound")
	}
	if _, live := hub.SessionForAccount(ctx, testAgentAccount); live {
		t.Fatal("account still resolves a live session after the drop")
	}
	if lost, errored := sink.waitOne(t); lost != testAgentAccount || errored {
		t.Fatalf("lost = %s errored = %v, want %s false", lost, errored, testAgentAccount)
	}
}

func TestErroredSessionDropsBindingAndReportsLoss(t *testing.T) {
	ctx := context.Background()
	hub := newHubOnly()
	bindings := newFakeBindingStore()
	hub.SetSessionBindingStore(bindings)
	sink := newRecordingLostSink()
	hub.SetSessionLostSink(sink)
	ended := make(chanEndSink, 1)
	hub.SetSessionEndSink(ended)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("cont-1", testAgentAccount, "runner-1")
	hub.promoteSession(ctx, "cont-1", "sess-1")

	// Both frames go out before any assertion: the drop is detached, so the owning
	// frame's report is the barrier that proves the foreign one changed nothing.
	frame := sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED)
	if err := hub.Deliver(ctx, RunnerEvent{RunnerID: "runner-other", SessionID: "sess-1", Frame: frame}); err != nil {
		t.Fatalf("Deliver foreign ERRORED frame: %v", err)
	}
	if err := hub.Deliver(ctx, RunnerEvent{RunnerID: "runner-1", SessionID: "sess-1", Frame: frame}); err != nil {
		t.Fatalf("Deliver owning ERRORED frame: %v", err)
	}
	if got := recvEnded(t, ended); got != "sess-1" {
		t.Fatalf("archived %q after owning ERRORED frame, want sess-1", got)
	}
	lost, errored := sink.waitOne(t)
	if lost != testAgentAccount || !errored {
		t.Fatalf("lost = %s errored = %v, want %s true", lost, errored, testAgentAccount)
	}
	if _, ok := hub.accountForSession(ctx, "sess-1"); ok {
		t.Fatal("owning ERRORED frame left sess-1 bound")
	}
	select {
	case got := <-ended:
		t.Fatalf("second archive %q, want exactly one (the foreign frame must not archive)", got)
	case <-sink.reported:
		t.Fatal("second loss report, want exactly one (the foreign frame must not report)")
	default:
	}
}

func TestStaleStateAfterErroredIsIgnored(t *testing.T) {
	ctx := t.Context()
	hub, lifecycle, tail := newHub()
	bindings := newFakeBindingStore()
	hub.SetSessionBindingStore(bindings)
	lost := newRecordingLostSink()
	hub.SetSessionLostSink(lost)
	settle := &fakeSettleSink{}
	hub.SetSettleSink(settle)
	presence := &fakePresenceSink{}
	hub.SetPresenceSink(presence)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("cont-1", testAgentAccount, "runner-1")
	hub.promoteSession(ctx, "cont-1", "sess-1")

	deliverState(t, hub, 5, compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED)
	if account, errored := lost.waitOne(t); account != testAgentAccount || !errored {
		t.Fatalf("lost report = (%s, %v), want (%s, true)", account, errored, testAgentAccount)
	}
	// The fallback ERRORED overtook a frame buffered on the cancelled shared stream.
	deliverState(t, hub, 4, compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING)

	assertPublished(t, lifecycle, compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED)
	frames := tail.snapshot()
	if len(frames) != 2 || frames[0].frame.GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED || frames[1].frame.GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING {
		t.Fatalf("relayed session frames = %+v, want ERRORED then WORKING", frames)
	}
	if got := settle.snapshot(); len(got) != 1 || got[0].state != compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED {
		t.Fatalf("settle edges = %+v, want only ERRORED", got)
	}
	// The ERRORED loss path adds its own DISCONNECTED edge; only the stale WORKING is wrong.
	for _, rec := range presence.lifecycleSnapshot() {
		if rec.state == compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING {
			t.Fatalf("presence edges = %+v, want no edge for the stale frame", presence.lifecycleSnapshot())
		}
	}
}

func TestNewLifetimeStateAfterErroredPublishes(t *testing.T) {
	ctx := t.Context()
	hub, lifecycle, _ := newHub()
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)

	deliverState(t, hub, 5, compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED)
	deliverState(t, hub, 6, compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	// A dead-lifetime frame that arrives after recovery began must still not publish.
	deliverState(t, hub, 4, compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING)
	deliverState(t, hub, 7, compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING)

	assertPublished(t, lifecycle,
		compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED,
		compassv1.AgentSessionState_AGENT_SESSION_STATE_READY,
		compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING)
}

func TestReenrollClearsErroredBoundary(t *testing.T) {
	ctx := t.Context()
	hub, lifecycle, _ := newHub()
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	deliverState(t, hub, 5, compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED)

	// A restarted Runner's counter starts over, so its low seqs are a new lifetime.
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	deliverState(t, hub, 1, compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)

	assertPublished(t, lifecycle,
		compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED,
		compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
}

func TestErroredFromBeforeReenrollDoesNotSurviveIt(t *testing.T) {
	ctx := t.Context()
	lifecycle := &fakeLifecycleSink{}
	tail := &pausingTailSink{entered: make(chan struct{}), release: make(chan struct{})}
	hub := NewHub(lifecycle, tail, nil, discardLogger())
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(tail.release) }) }
	t.Cleanup(release)

	oldDone := make(chan error, 1)
	go func() {
		oldDone <- hub.Deliver(ctx, RunnerEvent{
			RunnerID: "runner-1", RunnerSeq: 5, SessionID: "sess-1",
			Frame: sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED),
		})
	}()
	select {
	case <-tail.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("old ERRORED did not reach the tail sink")
	}
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	release()
	select {
	case err := <-oldDone:
		if err != nil {
			t.Fatalf("Deliver(old ERRORED) = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("old ERRORED did not complete after release")
	}

	deliverState(t, hub, 1, compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	statuses := lifecycle.snapshot()
	if n := len(statuses); n == 0 || statuses[n-1].GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_READY {
		t.Fatalf("published statuses = %+v, want the new enrollment's READY last", statuses)
	}
}

// pausingTailSink holds the first ERRORED frame in RelaySessionFrame until released.
type pausingTailSink struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (s *pausingTailSink) RelaySessionFrame(_ string, frame *compassv1internal.SessionFrame) {
	if frame.GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED {
		return
	}
	s.once.Do(func() {
		close(s.entered)
		<-s.release
	})
}

func deliverState(t *testing.T, hub *Hub, seq uint64, state compassv1.AgentSessionState) {
	t.Helper()
	if err := hub.Deliver(t.Context(), RunnerEvent{
		RunnerID: "runner-1", RunnerSeq: seq, SessionID: "sess-1", Frame: sessionStateFrame(state),
	}); err != nil {
		t.Fatalf("Deliver(%s, seq %d) = %v, want nil", state, seq, err)
	}
}

func assertPublished(t *testing.T, lifecycle *fakeLifecycleSink, want ...compassv1.AgentSessionState) {
	t.Helper()
	statuses := lifecycle.snapshot()
	got := make([]compassv1.AgentSessionState, 0, len(statuses))
	for _, s := range statuses {
		got = append(got, s.GetState())
	}
	if !slices.Equal(got, want) {
		t.Fatalf("published states = %v, want %v", got, want)
	}
}

func TestConcurrentErroredPublishesAfterInFlightLifecycle(t *testing.T) {
	ctx := t.Context()
	lifecycle := &blockingLifecycleSink{workingEntered: make(chan struct{}), releaseWorking: make(chan struct{})}
	tail := &blockingTailSink{errored: make(chan struct{})}
	var releaseOnce sync.Once
	releaseWorking := func() { releaseOnce.Do(func() { close(lifecycle.releaseWorking) }) }
	t.Cleanup(releaseWorking)

	hub := NewHub(lifecycle, tail, nil, discardLogger())
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("cont-1", testAgentAccount, "runner-1")
	hub.promoteSession(ctx, "cont-1", "sess-1")

	workingDone := make(chan error, 1)
	go func() {
		workingDone <- hub.Deliver(ctx, RunnerEvent{
			RunnerID: "runner-1", RunnerSeq: 1, SessionID: "sess-1",
			Frame: sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING),
		})
	}()
	select {
	case <-lifecycle.workingEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("WORKING lifecycle publish did not block")
	}
	if hub.lifecycleMu.TryLock() {
		hub.lifecycleMu.Unlock()
		t.Fatal("lifecycle lock released while WORKING is still being published")
	}

	erroredDone := make(chan error, 1)
	go func() {
		erroredDone <- hub.Deliver(ctx, RunnerEvent{
			RunnerID: "runner-1", RunnerSeq: 2, SessionID: "sess-1",
			Frame: sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED),
		})
	}()
	select {
	case <-tail.errored:
	case <-time.After(10 * time.Second):
		t.Fatal("ERRORED frame did not reach the tail sink")
	}
	if hub.lifecycleMu.TryLock() {
		hub.lifecycleMu.Unlock()
		t.Fatal("lifecycle lock released before ERRORED delivery completed")
	}
	releaseWorking()
	select {
	case err := <-workingDone:
		if err != nil {
			t.Fatalf("Deliver(WORKING) = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Deliver(WORKING) did not complete after release")
	}
	select {
	case err := <-erroredDone:
		if err != nil {
			t.Fatalf("Deliver(ERRORED) = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Deliver(ERRORED) did not complete after WORKING release")
	}

	statuses := lifecycle.snapshot()
	if len(statuses) != 2 || statuses[0].GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING || statuses[1].GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED {
		t.Fatalf("published lifecycle order = %+v, want WORKING then ERRORED", statuses)
	}
}

type blockingLifecycleSink struct {
	mu             sync.Mutex
	statuses       []*compassv1.AgentSessionStatus
	workingEntered chan struct{}
	releaseWorking chan struct{}
}

func (s *blockingLifecycleSink) PublishSessionStatus(status *compassv1.AgentSessionStatus) {
	if status.GetState() == compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING {
		close(s.workingEntered)
		<-s.releaseWorking
	}
	s.mu.Lock()
	s.statuses = append(s.statuses, status)
	s.mu.Unlock()
}

func (s *blockingLifecycleSink) snapshot() []*compassv1.AgentSessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*compassv1.AgentSessionStatus(nil), s.statuses...)
}

type blockingTailSink struct {
	errored chan struct{}
}

func (s *blockingTailSink) RelaySessionFrame(_ string, frame *compassv1internal.SessionFrame) {
	if frame.GetState() == compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED {
		close(s.errored)
	}
}

type chanEndSink chan string

func (c chanEndSink) OnSessionEnded(_ context.Context, sessionID string) { c <- sessionID }

func recvEnded(t *testing.T, c chanEndSink) string {
	t.Helper()
	select {
	case s := <-c:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("no session-end archive within 10s")
		return ""
	}
}

type endedArchive struct {
	tenant    store.TenantID
	sessionID string
}

type tenantRecordingEndSink struct{ ended chan endedArchive }

func (s tenantRecordingEndSink) OnSessionEnded(ctx context.Context, sessionID string) {
	tenant, _ := store.TenantFromContext(ctx)
	s.ended <- endedArchive{tenant: tenant, sessionID: sessionID}
}

// When the durable sweep faults, the RAM-snapshot fallback still archives each
// session under its own tenant, read from the binding rows the fault left behind.
func TestEnrollReapFaultArchivesUnderSessionTenant(t *testing.T) {
	ctx := t.Context()
	hub := newHubOnly()
	bindings := newFakeBindingStore()
	hub.SetSessionBindingStore(bindings)
	ended := tenantRecordingEndSink{ended: make(chan endedArchive, 1)}
	hub.SetSessionEndSink(ended)
	subj := runnerSubject()
	tier, egress := compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED
	hub.enroll(ctx, "runner-1", subj, tier, egress)
	hub.bindContainer("cont-1", testAgentAccount, "runner-1")
	hub.promoteSession(ctx, "cont-1", "sess-1")

	bindings.mu.Lock()
	bindings.deleteForRunnerErr = errors.New("sweep fault")
	bindings.mu.Unlock()
	hub.enroll(ctx, "runner-1", subj, tier, egress)
	select {
	case got := <-ended.ended:
		if got != (endedArchive{tenant: bindings.tenant, sessionID: "sess-1"}) {
			t.Fatalf("archived %+v, want sess-1 under %q", got, bindings.tenant)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no session-end archive within 10s")
	}
}

// A session id bound in several tenants resolves to no single tenant, so the
// fallback skips its archive rather than reading an arbitrary tenant's copy.
func TestEnrollReapFaultSkipsAmbiguousSessionArchive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		hub := newHubOnly()
		bindings := newFakeBindingStore()
		hub.SetSessionBindingStore(bindings)
		ended := tenantRecordingEndSink{ended: make(chan endedArchive, 1)}
		hub.SetSessionEndSink(ended)
		subj := runnerSubject()
		tier, egress := compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED
		hub.enroll(ctx, "runner-1", subj, tier, egress)
		hub.bindContainer("cont-1", testAgentAccount, "runner-1")
		hub.promoteSession(ctx, "cont-1", "sess-1")

		bindings.mu.Lock()
		bindings.deleteForRunnerErr = errors.New("sweep fault")
		bindings.tenantErr = store.ErrConflict
		bindings.mu.Unlock()
		hub.enroll(ctx, "runner-1", subj, tier, egress)
		synctest.Wait()
		select {
		case got := <-ended.ended:
			t.Fatalf("archived %+v for an ambiguous session id, want no archive", got)
		default:
		}
	})
}

// A session that ends without Stop is archived: on a lost-session drop by its owning
// Runner, and on each binding the re-enroll reap removes. A foreign refusal is not.
func TestSessionEndedWithoutStopIsArchived(t *testing.T) {
	ctx := t.Context()
	hub := newHubOnly()
	bindings := newFakeBindingStore()
	hub.SetSessionBindingStore(bindings)
	ended := make(chanEndSink, 4)
	hub.SetSessionEndSink(ended)
	subj := runnerSubject()
	tier, egress := compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED
	hub.enroll(ctx, "runner-1", subj, tier, egress)

	hub.bindContainer("cont-1", testAgentAccount, "runner-1")
	hub.promoteSession(ctx, "cont-1", "sess-lost")
	hub.dropLostSession(ctx, "runner-other", "sess-lost", false)
	hub.dropLostSession(ctx, "runner-1", "sess-lost", false)
	if got := recvEnded(t, ended); got != "sess-lost" {
		t.Fatalf("archived %q after the lost drop, want sess-lost", got)
	}

	hub.bindContainer("cont-2", testAgentAccount, "runner-1")
	hub.promoteSession(ctx, "cont-2", "sess-reaped")
	hub.enroll(ctx, "runner-1", subj, tier, egress)
	if got := recvEnded(t, ended); got != "sess-reaped" {
		t.Fatalf("archived %q after the re-enroll reap, want sess-reaped (the foreign refusal must not archive)", got)
	}
}
