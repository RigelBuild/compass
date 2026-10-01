//go:build unix

package runnerhub

import (
	"context"
	"testing"

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
