//go:build unix

package runnerhub

// RIG-1569 T3 — enroll (the Runner-reconnect teardown) fires the hub's
// SessionReapSink with the session ids whose bindings it just cleared, so the
// delivery consumer can reap held-deliver registry entries a no-frame author
// death left behind (design.md:172-175). White-box (package runnerhub) so the
// test drives the unexported bind/enroll lifecycle directly and asserts the edge
// through a fake sink. Sleep-free: enroll fires the sink inline after releasing
// h.mu (the sink only records and returns), so every assertion reads a
// synchronously-recorded fact.

import (
	"slices"
	"sync"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// fakeSessionReapSink records OnSessionsReaped calls — the hub's session-reap
// edge sink. Records synchronously and returns immediately (as the real
// consumer's hook does: a pure in-memory delete), so a test asserting enroll
// does not block reads the recorded fact right after the call.
type fakeSessionReapSink struct {
	mu    sync.Mutex
	calls [][]string
}

func (f *fakeSessionReapSink) OnSessionsReaped(sessionIDs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]string, len(sessionIDs))
	copy(cp, sessionIDs)
	f.calls = append(f.calls, cp)
}

func (f *fakeSessionReapSink) snapshot() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// Case T3/hub: a re-enroll fires the reap sink exactly once with the cleared
// session ids (the keys of sessionAccounts, which Consumer.held is keyed by).
func TestEnrollFiresReapSinkWithClearedSessionIDs(t *testing.T) {
	hub := newHubOnly()
	fake := &fakeSessionReapSink{}
	hub.SetSessionReapSink(fake)

	// A first enroll binds the Runner, then two live sessions promote onto it.
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	hub.bindContainer("c1", "acct-a")
	hub.promoteSession("c1", "sess-a")
	hub.bindContainer("c2", "acct-b")
	hub.promoteSession("c2", "sess-b")

	// The first enroll fired the reap edge once with no ids (nothing was bound);
	// drop it so the assertion below covers only the re-enroll's reap.
	if got := fake.snapshot(); len(got) != 1 || len(got[0]) != 0 {
		t.Fatalf("first enroll reap calls = %+v, want one empty-id call (nothing bound yet)", got)
	}

	// The Runner reconnects: enroll clears both bindings and reaps both ids.
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})

	calls := fake.snapshot()
	if len(calls) != 2 {
		t.Fatalf("reap calls = %d, want 2 (one per enroll)", len(calls))
	}
	reaped := slices.Clone(calls[1])
	slices.Sort(reaped)
	if want := []string{"sess-a", "sess-b"}; !slices.Equal(reaped, want) {
		t.Fatalf("re-enroll reaped %+v, want %+v (the cleared session ids)", reaped, want)
	}
}

// Case T3/hub (nil-safe): a hub with NO reap sink wired still clears every
// binding at re-enroll and does not panic — the sink is nil-safe, so every
// existing hub test (which wires no reap sink) is unchanged.
func TestEnrollNilReapSinkStillClears(t *testing.T) {
	hub := newHubOnly() // no SetSessionReapSink

	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})
	hub.bindContainer("c1", "acct-a")
	hub.promoteSession("c1", "sess-a")

	// A re-enroll with no reap sink clears the binding without panicking.
	hub.enroll("runner-1", store.Subject{Kind: store.SubjectRunner, ID: "runner-1"})

	if sess, ok := hub.SessionForAccount("acct-a"); ok {
		t.Fatalf("SessionForAccount(acct-a) = %q ok=true after re-enroll, want ok=false (binding cleared)", sess)
	}
}
