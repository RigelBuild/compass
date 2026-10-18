//go:build unix

package runnerhub

// promoteSession notifies SessionStartSink so the delivery consumer can sweep a newly live session.
// Tests drive the lifecycle directly and verify the synchronous sink record.

import (
	"context"
	"sync"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// startRecord is one recorded session-start edge.
type startRecord struct {
	sessionID string
	account   store.AccountID
}

// fakeSessionStartSink records notifications synchronously so tests can verify promotion does not wait on store work.
type fakeSessionStartSink struct {
	mu     sync.Mutex
	starts []startRecord
}

func (f *fakeSessionStartSink) OnSessionStarted(sessionID string, account store.AccountID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, startRecord{sessionID: sessionID, account: account})
}

func (f *fakeSessionStartSink) snapshot() []startRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]startRecord, len(f.starts))
	copy(out, f.starts)
	return out
}

// Case T6/hub: promoteSession fires the session-start sink EXACTLY ONCE per
// promotion, with the bound (session, account), non-blocking. The hub itself must
// NOT run the sweep inline — it only hands the edge to the sink, which records
// synchronously and returns; the recorded fact right after the call proves the
// hub did not block on store work.
func TestPromoteSessionFiresStartSink(t *testing.T) {
	hub := newHubOnly()
	sink := &fakeSessionStartSink{}
	hub.SetSessionStartSink(sink)

	// The Provision->Start promotion path: record the container's account, then
	// promote it onto the minted session id.
	hub.bindContainer("c1", testAgentAccount, "runner-1")
	hub.promoteSession(context.Background(), "c1", "sess-1")

	got := sink.snapshot()
	if len(got) != 1 {
		t.Fatalf("session-start edges = %d, want exactly 1 per promotion", len(got))
	}
	if got[0].sessionID != "sess-1" || got[0].account != testAgentAccount {
		t.Fatalf("start edge = %+v, want {sess-1, %s}", got[0], testAgentAccount)
	}
}

// Case T6/hub (no binding): a promotion for a container with no recorded account
// creates no session binding and fires no start edge.
func TestPromoteSessionNoBindingFiresNothing(t *testing.T) {
	hub := newHubOnly()
	sink := &fakeSessionStartSink{}
	hub.SetSessionStartSink(sink)

	// No bindContainer: the container has no recorded account.
	hub.promoteSession(context.Background(), "c-unknown", "sess-1")

	if got := sink.snapshot(); len(got) != 0 {
		t.Fatalf("start edges = %d, want 0 (a non-binding promotion sweeps nothing)", len(got))
	}
	// And no live binding was created.
	if _, ok := hub.SessionForAccount(context.Background(), testAgentAccount); ok {
		t.Fatal("a non-binding promotion created a live session binding, want none")
	}
}

// Case T6/hub (nil-safe): a hub with NO session-start sink wired still binds the
// session correctly at promoteSession — the sink is nil-safe, so every existing
// hub test (which wires no session-start sink) is unchanged.
func TestPromoteSessionNilStartSinkStillBinds(t *testing.T) {
	hub := newHubOnly() // no SetSessionStartSink

	hub.bindContainer("c1", testAgentAccount, "runner-1")
	hub.promoteSession(context.Background(), "c1", "sess-1")

	// The binding is live in both directions — promoteSession did its job with no
	// sink wired.
	if acct, ok := hub.accountForSession(context.Background(), "sess-1"); !ok || acct != testAgentAccount {
		t.Fatalf("accountForSession(sess-1) = (%q, %v), want (%s, true)", acct, ok, testAgentAccount)
	}
	if sess, ok := hub.SessionForAccount(context.Background(), testAgentAccount); !ok || sess != "sess-1" {
		t.Fatalf("SessionForAccount(%s) = (%q, %v), want (sess-1, true)", testAgentAccount, sess, ok)
	}
}
