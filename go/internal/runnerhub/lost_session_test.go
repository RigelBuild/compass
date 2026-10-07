//go:build unix

package runnerhub

import (
	"context"
	"errors"
	"runtime"
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

	hub.dropLostSession(ctx, "runner-other", "sess-1", nil, false)
	if _, ok := hub.accountForSession(ctx, "sess-1"); !ok {
		t.Fatal("foreign refusal unbound sess-1")
	}
	sink.none(t, "foreign refusal")

	hub.dropLostSession(ctx, "runner-1", "sess-1", nil, false)
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
	if len(frames) != 1 || frames[0].frame.GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED {
		t.Fatalf("relayed session frames = %+v, want only ERRORED; a stale state must not reach the tail", frames)
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

func TestErroredOlderThanNewLifetimeIsIgnored(t *testing.T) {
	ctx := t.Context()
	hub, lifecycle, _ := newHub()
	lost := newRecordingLostSink()
	hub.SetSessionLostSink(lost)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("cont-1", testAgentAccount, "runner-1")
	hub.promoteSession(ctx, "cont-1", "sess-1")

	// Separate streams can deliver out of seq order: the resumed READY overtakes ERRORED.
	deliverState(t, hub, 6, compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	deliverState(t, hub, 5, compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED)

	assertPublished(t, lifecycle, compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	lost.none(t, "an older ERRORED must not retire the resumed lifetime")
}

func TestErroredCleanupKeepsSameEnrollmentRebind(t *testing.T) {
	ctx := t.Context()
	hub, _, _ := newHub()
	bindings := &pausingTenantBindingStore{fakeBindingStore: newFakeBindingStore(), entered: make(chan struct{}), release: make(chan struct{})}
	hub.SetSessionBindingStore(bindings)
	lost := newRecordingLostSink()
	hub.SetSessionLostSink(lost)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(bindings.release) }) }
	t.Cleanup(release)
	hub.bindContainer("cont-1", testAgentAccount, "runner-1")
	hub.promoteSession(ctx, "cont-1", "sess-1")
	gen := hub.EnrollGeneration()
	hub.mu.Lock()
	old, ok := hub.sessionAccounts["sess-1"]
	hub.mu.Unlock()
	if !ok {
		t.Fatal("sess-1 not bound after promote")
	}

	// The old lifetime's cleanup pauses resolving the tenant; a resume re-binds
	// the same session in the same enrollment before it continues.
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.dropLostSessionIfCurrent(ctx, gen, "runner-1", "sess-1", &old, true)
	}()
	select {
	case <-bindings.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("ERRORED cleanup did not start")
	}
	hub.promoteSession(ctx, "cont-1", "sess-1")
	release()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ERRORED cleanup did not finish")
	}

	lost.none(t, "the old lifetime's cleanup must not report the resumed session lost")
	if account, ok := hub.accountForSession(ctx, "sess-1"); !ok || account != testAgentAccount {
		t.Fatalf("binding after old cleanup = (%s, %v), want (%s, true)", account, ok, testAgentAccount)
	}
	if account, _, _, err := bindings.ResolveSessionBinding(ctx, "sess-1"); err != nil || account != testAgentAccount {
		t.Fatalf("durable binding after old cleanup = (%s, %v), want (%s, nil)", account, err, testAgentAccount)
	}
}

// pausingTenantBindingStore holds the first SessionBindingTenant until released.
type pausingTenantBindingStore struct {
	*fakeBindingStore
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (b *pausingTenantBindingStore) SessionBindingTenant(ctx context.Context, sessionID, runnerID string) (store.TenantID, error) {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	return b.fakeBindingStore.SessionBindingTenant(ctx, sessionID, runnerID)
}

func TestErroredCleanupKeepsRebindDuringDurableDelete(t *testing.T) {
	ctx := t.Context()
	hub, _, _ := newHub()
	bindings := &pausingDeleteBindingStore{fakeBindingStore: newFakeBindingStore(), entered: make(chan struct{}), release: make(chan struct{})}
	hub.SetSessionBindingStore(bindings)
	lost := newRecordingLostSink()
	hub.SetSessionLostSink(lost)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(bindings.release) }) }
	t.Cleanup(release)
	hub.bindContainer("cont-1", testAgentAccount, "runner-1")
	hub.promoteSession(ctx, "cont-1", "sess-1")
	gen := hub.EnrollGeneration()
	hub.mu.Lock()
	old := hub.sessionAccounts["sess-1"]
	hub.mu.Unlock()

	// The cleanup has matched the old binding and pauses in its durable delete;
	// a resume re-binds the session before the delete runs.
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.dropLostSessionIfCurrent(ctx, gen, "runner-1", "sess-1", &old, true)
	}()
	select {
	case <-bindings.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("cleanup did not reach the durable delete")
	}
	hub.promoteSession(ctx, "cont-1", "sess-1")
	release()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cleanup did not finish")
	}

	lost.none(t, "a cleanup whose row was re-bound must not report the session lost")
	if account, ok := hub.accountForSession(ctx, "sess-1"); !ok || account != testAgentAccount {
		t.Fatalf("binding after old cleanup = (%s, %v), want (%s, true)", account, ok, testAgentAccount)
	}
	if account, _, _, err := bindings.ResolveSessionBinding(ctx, "sess-1"); err != nil || account != testAgentAccount {
		t.Fatalf("durable binding after old cleanup = (%s, %v), want (%s, nil)", account, err, testAgentAccount)
	}
}

func TestErroredCleanupKeepsPeerRebind(t *testing.T) {
	ctx := t.Context()
	hub, _, _ := newHub()
	bindings := &pausingDeleteBindingStore{fakeBindingStore: newFakeBindingStore(), entered: make(chan struct{}), release: make(chan struct{})}
	hub.SetSessionBindingStore(bindings)
	lost := newRecordingLostSink()
	hub.SetSessionLostSink(lost)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(bindings.release) }) }
	t.Cleanup(release)
	hub.bindContainer("cont-1", testAgentAccount, "runner-1")
	hub.promoteSession(ctx, "cont-1", "sess-1")
	gen := hub.EnrollGeneration()
	hub.mu.Lock()
	old := hub.sessionAccounts["sess-1"]
	hub.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.dropLostSessionIfCurrent(ctx, gen, "runner-1", "sess-1", &old, true)
	}()
	select {
	case <-bindings.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("cleanup did not reach the durable delete")
	}
	// Another Server re-binds the row; this hub's cache still holds the old binding.
	if _, _, err := bindings.fakeBindingStore.RecordSessionBinding(ctx, "sess-1", testAgentAccount, "runner-1"); err != nil {
		t.Fatalf("peer RecordSessionBinding: %v", err)
	}
	release()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cleanup did not finish")
	}

	lost.none(t, "a cleanup whose row a peer re-bound must not report the session lost")
	if account, ok := hub.accountForSession(ctx, "sess-1"); !ok || account != testAgentAccount {
		t.Fatalf("binding after old cleanup = (%s, %v), want (%s, true)", account, ok, testAgentAccount)
	}
}

func TestErroredCleanupWithoutDurableWriteKeepsPeerRow(t *testing.T) {
	ctx := t.Context()
	hub, _, _ := newHub()
	bindings := newFakeBindingStore()
	bindings.recordErr = errors.New("store down")
	hub.SetSessionBindingStore(bindings)
	lost := newRecordingLostSink()
	hub.SetSessionLostSink(lost)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("cont-1", testAgentAccount, "runner-1")
	// The durable write fails, so this hub caches a binding with no version.
	hub.promoteSession(ctx, "cont-1", "sess-1")
	gen := hub.EnrollGeneration()
	hub.mu.Lock()
	old := hub.sessionAccounts["sess-1"]
	hub.mu.Unlock()
	if old.version != "" {
		t.Fatalf("cached version after failed write = %q, want empty", old.version)
	}
	// A peer Server then writes the session's row.
	bindings.mu.Lock()
	bindings.recordErr = nil
	bindings.mu.Unlock()
	if _, _, err := bindings.RecordSessionBinding(ctx, "sess-1", testAgentAccount, "runner-1"); err != nil {
		t.Fatalf("peer RecordSessionBinding: %v", err)
	}

	hub.dropLostSessionIfCurrent(ctx, gen, "runner-1", "sess-1", &old, true)
	if _, _, _, err := bindings.ResolveSessionBinding(ctx, "sess-1"); err != nil {
		t.Fatalf("peer's durable binding after old cleanup: %v, want it kept", err)
	}
	lost.none(t, "a session the peer re-bound is not lost")
	if account, ok := hub.accountForSession(ctx, "sess-1"); !ok || account != testAgentAccount {
		t.Fatalf("binding after old cleanup = (%s, %v), want the peer's (%s, true)", account, ok, testAgentAccount)
	}
}

func TestErroredCleanupWithoutDurableWriteKeepsLegacyRow(t *testing.T) {
	ctx := t.Context()
	hub, _, _ := newHub()
	bindings := newFakeBindingStore()
	bindings.recordErr = errors.New("store down")
	hub.SetSessionBindingStore(bindings)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("cont-1", testAgentAccount, "runner-1")
	hub.promoteSession(ctx, "cont-1", "sess-1")
	gen := hub.EnrollGeneration()
	hub.mu.Lock()
	old := hub.sessionAccounts["sess-1"]
	hub.mu.Unlock()
	// A row written before binding_version existed carries the column default "".
	bindings.mu.Lock()
	bindings.bindings["sess-1"] = store.SessionBinding{TenantID: bindings.tenant, SessionID: "sess-1", AccountID: testAgentAccount, RunnerID: "runner-1"}
	bindings.versions["sess-1"] = ""
	bindings.mu.Unlock()

	hub.dropLostSessionIfCurrent(ctx, gen, "runner-1", "sess-1", &old, true)
	if _, _, _, err := bindings.ResolveSessionBinding(ctx, "sess-1"); err != nil {
		t.Fatalf("legacy durable binding after version-less cleanup: %v, want it kept", err)
	}
}

// pausingDeleteBindingStore holds the first DeleteSessionBindingVersion until released.
type pausingDeleteBindingStore struct {
	*fakeBindingStore
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (b *pausingDeleteBindingStore) DeleteSessionBindingVersion(ctx context.Context, sessionID, version string) (bool, error) {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	return b.fakeBindingStore.DeleteSessionBindingVersion(ctx, sessionID, version)
}

func TestLostSessionCleanupFromOldEnrollmentKeepsRebinding(t *testing.T) {
	ctx := t.Context()
	hub, _, _ := newHub()
	lost := newRecordingLostSink()
	hub.SetSessionLostSink(lost)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	oldGen := hub.EnrollGeneration()
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("cont-1", testAgentAccount, "runner-1")
	hub.promoteSession(ctx, "cont-1", "sess-1")

	hub.dropLostSessionIfCurrent(ctx, oldGen, "runner-1", "sess-1", nil, true)
	lost.none(t, "cleanup from the old enrollment must not unbind the re-bound session")
	if account, ok := hub.accountForSession(ctx, "sess-1"); !ok || account != testAgentAccount {
		t.Fatalf("binding after stale cleanup = (%s, %v), want (%s, true)", account, ok, testAgentAccount)
	}

	hub.dropLostSessionIfCurrent(ctx, hub.EnrollGeneration(), "runner-1", "sess-1", nil, true)
	if account, errored := lost.waitOne(t); account != testAgentAccount || !errored {
		t.Fatalf("current-enrollment cleanup report = (%s, %v), want (%s, true)", account, errored, testAgentAccount)
	}
}

func TestLowerSeqCannotOvertakeHigherSeqMidDelivery(t *testing.T) {
	ctx := t.Context()
	hub, lifecycle, _ := newHub()
	store := &pausingResolveBindingStore{fakeBindingStore: newFakeBindingStore(), entered: make(chan struct{}), release: make(chan struct{})}
	hub.SetSessionBindingStore(store)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(store.release) }) }
	t.Cleanup(release)

	// READY seq 6 has recorded its seq and pauses resolving the (uncached) binding.
	readyDone := make(chan error, 1)
	go func() {
		readyDone <- hub.Deliver(ctx, RunnerEvent{
			RunnerID: "runner-1", RunnerSeq: 6, SessionID: "sess-1",
			Frame: sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_READY),
		})
	}()
	select {
	case <-store.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("READY did not reach the binding lookup")
	}
	if !sessionLockHeld(hub, "sess-1") {
		t.Fatal("lifecycle lock free while READY seq 6 is mid-delivery; seq 5 could overtake it")
	}
	erroredDone := make(chan error, 1)
	go func() {
		erroredDone <- hub.Deliver(ctx, RunnerEvent{
			RunnerID: "runner-1", RunnerSeq: 5, SessionID: "sess-1",
			Frame: sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED),
		})
	}()
	release()
	for name, done := range map[string]chan error{"READY": readyDone, "ERRORED": erroredDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Deliver(%s) = %v, want nil", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("Deliver(%s) did not complete", name)
		}
	}
	assertPublished(t, lifecycle, compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
}

// pausingResolveBindingStore holds the first ResolveSessionBinding until released.
type pausingResolveBindingStore struct {
	*fakeBindingStore
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (b *pausingResolveBindingStore) ResolveSessionBinding(ctx context.Context, sessionID string) (store.AccountID, string, string, error) {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	return b.fakeBindingStore.ResolveSessionBinding(ctx, sessionID)
}

// sessionLockHeld reports whether sessionID's lifecycle lock is held right now.
func sessionLockHeld(hub *Hub, sessionID string) bool {
	hub.lifecycleMu.Lock()
	l := hub.sessionLocks[sessionID]
	hub.lifecycleMu.Unlock()
	if l == nil {
		return false
	}
	if l.mu.TryLock() {
		l.mu.Unlock()
		return false
	}
	return true
}

func TestSlowBindingLookupStallsOnlyItsSession(t *testing.T) {
	ctx := t.Context()
	hub, lifecycle, _ := newHub()
	store := &pausingResolveBindingStore{fakeBindingStore: newFakeBindingStore(), entered: make(chan struct{}), release: make(chan struct{})}
	hub.SetSessionBindingStore(store)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(store.release) }) }
	t.Cleanup(release)
	hub.bindContainer("cont-2", testAgentAccount, "runner-1")
	hub.promoteSession(ctx, "cont-2", "sess-2")

	slowDone := make(chan error, 1)
	go func() {
		slowDone <- hub.Deliver(ctx, RunnerEvent{
			RunnerID: "runner-1", RunnerSeq: 1, SessionID: "sess-1",
			Frame: sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_READY),
		})
	}()
	select {
	case <-store.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("sess-1 did not reach the binding lookup")
	}
	fastDone := make(chan error, 1)
	go func() {
		fastDone <- hub.Deliver(ctx, RunnerEvent{
			RunnerID: "runner-1", RunnerSeq: 2, SessionID: "sess-2",
			Frame: sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING),
		})
	}()
	select {
	case err := <-fastDone:
		if err != nil {
			t.Fatalf("Deliver(sess-2) = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sess-2 lifecycle delivery stalled behind sess-1's binding lookup")
	}
	assertPublished(t, lifecycle, compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING)
	release()
	select {
	case err := <-slowDone:
		if err != nil {
			t.Fatalf("Deliver(sess-1) = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sess-1 did not complete")
	}
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

func TestReenrollWaitsForInFlightErrored(t *testing.T) {
	ctx := t.Context()
	lifecycle := &fakeLifecycleSink{}
	tail := &pausingTailSink{entered: make(chan struct{}), release: make(chan struct{})}
	hub := NewHub(lifecycle, tail, nil, discardLogger())
	lost := newRecordingLostSink()
	hub.SetSessionLostSink(lost)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("cont-1", testAgentAccount, "runner-1")
	hub.promoteSession(ctx, "cont-1", "sess-1")
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
	if hub.enrollMu.TryLock() {
		hub.enrollMu.Unlock()
		t.Fatal("enroll lock free while a session frame is mid-delivery")
	}
	enrolled := make(chan struct{})
	go func() {
		hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
		hub.bindContainer("cont-1", testAgentAccount, "runner-1")
		hub.promoteSession(ctx, "cont-1", "sess-1")
		close(enrolled)
	}()
	// A pending writer refuses new readers, so the ERRORED's cleanup queues behind enroll.
	for deadline := time.Now().Add(10 * time.Second); hub.enrollMu.TryRLock(); {
		hub.enrollMu.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("re-enroll never queued behind the in-flight delivery")
		}
		runtime.Gosched()
	}
	release()
	select {
	case err := <-oldDone:
		if err != nil {
			t.Fatalf("Deliver(old ERRORED) = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("old ERRORED did not complete after release")
	}
	select {
	case <-enrolled:
	case <-time.After(10 * time.Second):
		t.Fatal("re-enroll did not complete")
	}
	if _, _, err := hub.routerFor("sess-1"); err != nil {
		t.Fatalf("routerFor(sess-1) after re-bind = %v, want the binding kept", err)
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

func TestFramesFromStreamBeforeReenrollAreDropped(t *testing.T) {
	ctx := t.Context()
	hub, lifecycle, tail := newHub()
	settle := &fakeSettleSink{}
	hub.SetSettleSink(settle)
	lost := newRecordingLostSink()
	hub.SetSessionLostSink(lost)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	oldGen := hub.EnrollGeneration()

	// The Runner restarts and resumes the same session id before the old stream drains.
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("cont-1", testAgentAccount, "runner-1")
	hub.promoteSession(ctx, "cont-1", "sess-1")
	if err := hub.Deliver(ctx, RunnerEvent{
		RunnerID: "runner-1", RunnerSeq: 9, SessionID: "sess-1", EnrollGen: oldGen,
		Frame: sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED),
	}); err != nil {
		t.Fatalf("Deliver(old-stream ERRORED) = %v, want nil", err)
	}

	if got := lifecycle.snapshot(); len(got) != 0 {
		t.Fatalf("published statuses = %+v, want none from the old stream", got)
	}
	if got := tail.snapshot(); len(got) != 0 {
		t.Fatalf("relayed frames = %+v, want none from the old stream", got)
	}
	if got := settle.snapshot(); len(got) != 0 {
		t.Fatalf("settle edges = %+v, want none from the old stream", got)
	}
	lost.none(t, "an old-stream ERRORED must not unbind the resumed session")
	deliverState(t, hub, 1, compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
	assertPublished(t, lifecycle, compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
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
	if !sessionLockHeld(hub, "sess-1") {
		t.Fatal("lifecycle lock released while WORKING is still being published")
	}

	erroredDone := make(chan error, 1)
	go func() {
		erroredDone <- hub.Deliver(ctx, RunnerEvent{
			RunnerID: "runner-1", RunnerSeq: 2, SessionID: "sess-1",
			Frame: sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED),
		})
	}()
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
	select {
	case <-tail.errored:
	default:
		t.Fatal("ERRORED never reached the tail sink")
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
	hub.dropLostSession(ctx, "runner-other", "sess-lost", nil, false)
	hub.dropLostSession(ctx, "runner-1", "sess-lost", nil, false)
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
