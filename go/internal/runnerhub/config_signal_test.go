//go:build unix

package runnerhub

// The ConfigVersion emit seam (config_signal.go). Every case pins a behavior a
// plausible bug would break:
//   - SignalConfigVersion pushes a ConfigVersion to every live session carrying
//     the store's version verbatim (NOT minted) — a Put emits the content hash.
//   - An empty version (the Delete/cleared marker) rides through unchanged.
//   - No live sessions, and no Runner enrolled, are clean no-op successes.

import (
	"testing"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// TestSignalConfigVersionPushesStoreVersion pins the emit seam: after binding two
// live sessions and attaching a live send, SignalConfigVersion pushes a
// ConfigVersion to EACH, and the version is exactly the caller-supplied string
// (the store's content version), never a minted token.
func TestSignalConfigVersionPushesStoreVersion(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	bindSession(hub, "sess-a")
	bindSession(hub, "sess-b")
	router, _, err := hub.routerFor("any")
	if err != nil {
		t.Fatalf("routerFor after enroll = %v, want a router", err)
	}
	rec := newRecordingSend()
	router.attach(rec.send)

	const version = "deadbeefcafe"
	if err := hub.SignalConfigVersion(version); err != nil {
		t.Fatalf("SignalConfigVersion = %v, want nil", err)
	}
	pushed := configVersionsPushed(t, rec)
	// One fleet-wide frame per stream, regardless of how many sessions are live —
	// ConfigVersion carries no session_id (record §527-528, §563).
	if len(pushed) != 1 {
		t.Fatalf("pushed %d ConfigVersion frames, want 1 (one fleet-wide frame per stream regardless of live session count)", len(pushed))
	}
	for _, cv := range pushed {
		if cv.GetVersion() != version {
			t.Fatalf("ConfigVersion version = %q, want the store version %q (not minted)", cv.GetVersion(), version)
		}
	}
}

// TestSignalConfigVersionEmptyVersionIsTheClearedMarker pins the Delete path: an
// empty version rides through to every live session unchanged — the
// fleet-cleared marker the Runner reads as "materialize an empty dir".
func TestSignalConfigVersionEmptyVersionIsTheClearedMarker(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	bindSession(hub, "sess-a")
	router, _, err := hub.routerFor("any")
	if err != nil {
		t.Fatalf("routerFor after enroll = %v, want a router", err)
	}
	rec := newRecordingSend()
	router.attach(rec.send)

	if err := hub.SignalConfigVersion(""); err != nil {
		t.Fatalf("SignalConfigVersion(empty) = %v, want nil", err)
	}
	pushed := configVersionsPushed(t, rec)
	if len(pushed) != 1 {
		t.Fatalf("pushed %d ConfigVersion frames, want 1", len(pushed))
	}
	if got := pushed[0].GetVersion(); got != "" {
		t.Fatalf("cleared-marker version = %q, want empty", got)
	}
}

// TestSignalConfigVersionNoLiveSessionsIsNoop: a signal with no live sessions
// (nothing bound) pushes nothing and is a clean success.
func TestSignalConfigVersionNoLiveSessionsIsNoop(t *testing.T) {
	hub := newHubOnly()
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	router, _, _ := hub.routerFor("any")
	rec := newRecordingSend()
	router.attach(rec.send)

	if err := hub.SignalConfigVersion("v1"); err != nil {
		t.Fatalf("SignalConfigVersion with no live sessions = %v, want nil", err)
	}
	if got := len(configVersionsPushed(t, rec)); got != 0 {
		t.Fatalf("pushed %d frames with no live sessions, want 0", got)
	}
}

// TestSignalConfigVersionNoRunnerIsNoop: with no Runner enrolled, a signal is a
// clean no-op (best-effort — the Runner re-fetches on reconnect).
func TestSignalConfigVersionNoRunnerIsNoop(t *testing.T) {
	hub := newHubOnly() // no enroll
	if err := hub.SignalConfigVersion("v1"); err != nil {
		t.Fatalf("SignalConfigVersion with no runner = %v, want nil", err)
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
