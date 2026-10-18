//go:build unix

package runnerhub

// SignalConfigChanged sends one payload-free frame per live stream.
// No live sessions or enrolled Runner is a successful no-op.

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// TestSignalConfigChangedNudgesAttachedRouter pins the emit seam: after binding
// two live sessions and attaching a live send, SignalConfigChanged pushes exactly
// ONE payload-free ConfigVersion frame for the fleet.
func TestSignalConfigChangedNudgesAttachedRouter(t *testing.T) {
	hub := newHubOnly()
	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	bindSession(hub, "sess-a")
	bindSession(hub, "sess-b")
	router, _, err := hub.routerFor("any")
	if err != nil {
		t.Fatalf("routerFor after enroll = %v, want a router", err)
	}
	rec := newRecordingSend()
	router.attach(t.Context(), rec.send)
	defer router.detach(errStreamClosed)

	if err := hub.SignalConfigChanged(context.Background()); err != nil {
		t.Fatalf("SignalConfigChanged = %v, want nil", err)
	}
	// The fleet-wide frame is queued-not-pushed; gate on the sender draining it.
	waitRecorded(t, rec, 1)
	pushed := configVersionsPushed(t, rec)
	if len(pushed) != 1 {
		t.Fatalf("pushed %d ConfigVersion frames, want 1 (one fleet-wide frame per stream)", len(pushed))
	}
	if !proto.Equal(pushed[0], &compassv1internal.ConfigVersion{}) {
		t.Fatalf("ConfigVersion frame = %v, want an empty nudge", pushed[0])
	}
}

// TestSignalConfigChangedNoLiveSessionsIsNoop: a signal with no live sessions
// (nothing bound) pushes nothing and is a clean success.
func TestSignalConfigChangedNoLiveSessionsIsNoop(t *testing.T) {
	hub := newHubOnly()
	hub.enroll(context.Background(), "runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	router, _, _ := hub.routerFor("any")
	rec := newRecordingSend()
	router.attach(t.Context(), rec.send)
	defer router.detach(errStreamClosed)

	if err := hub.SignalConfigChanged(context.Background()); err != nil {
		t.Fatalf("SignalConfigChanged with no live sessions = %v, want nil", err)
	}
	if got := len(configVersionsPushed(t, rec)); got != 0 {
		t.Fatalf("pushed %d frames with no live sessions, want 0", got)
	}
}

// TestSignalConfigChangedNoRunnerIsNoop: with no Runner enrolled, a signal is a
// clean no-op (best-effort — the Runner re-fetches on reconnect).
func TestSignalConfigChangedNoRunnerIsNoop(t *testing.T) {
	hub := newHubOnly() // no enroll
	if err := hub.SignalConfigChanged(context.Background()); err != nil {
		t.Fatalf("SignalConfigChanged with no runner = %v, want nil", err)
	}
}

// configVersionsPushed extracts every ConfigVersion command the recorder saw.
func configVersionsPushed(t *testing.T, rec *recordingSend) []*compassv1internal.ConfigVersion {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []*compassv1internal.ConfigVersion
	for _, cmd := range rec.sent {
		if cv := cmd.GetConfigVersion(); cv != nil {
			out = append(out, cv)
		}
	}
	return out
}
