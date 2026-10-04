//go:build unix

package runnerhub

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
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
