//go:build unix

package runnerhub

import (
	"context"
	"errors"
	"testing"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

type recordingLostSink struct{ lost []store.AccountID }

func (s *recordingLostSink) OnSessionLost(_ string, account store.AccountID) {
	s.lost = append(s.lost, account)
}

// A NotFound deliver refusal releases the binding and reports the account, but only
// when the refusing Runner owns the session: a foreign Runner must not unbind it.
func TestDropLostSessionOnlyForOwningRunner(t *testing.T) {
	ctx := context.Background()
	hub := newHubOnly()
	bindings := newFakeBindingStore()
	hub.SetSessionBindingStore(bindings)
	sink := &recordingLostSink{}
	hub.SetSessionLostSink(sink)
	hub.enroll(ctx, "runner-1", runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	hub.bindContainer("cont-1", testAgentAccount, "runner-1")
	hub.promoteSession(ctx, "cont-1", "sess-1")

	hub.dropLostSession(ctx, "runner-other", "sess-1")
	if _, ok := hub.accountForSession(ctx, "sess-1"); !ok || len(sink.lost) != 0 {
		t.Fatalf("foreign refusal: bound=%v lost=%v, want bound and no wake", ok, sink.lost)
	}

	hub.dropLostSession(ctx, "runner-1", "sess-1")
	if _, ok := hub.accountForSession(ctx, "sess-1"); ok {
		t.Fatal("owning refusal left sess-1 bound")
	}
	if _, live := hub.SessionForAccount(ctx, testAgentAccount); live {
		t.Fatal("account still resolves a live session after the drop")
	}
	if len(sink.lost) != 1 || sink.lost[0] != testAgentAccount {
		t.Fatalf("lost = %v, want [%s]", sink.lost, testAgentAccount)
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
	hub.dropLostSession(ctx, "runner-other", "sess-lost")
	hub.dropLostSession(ctx, "runner-1", "sess-lost")
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
