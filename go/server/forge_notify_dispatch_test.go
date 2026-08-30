//go:build unix

package server

// forgeNotifyDispatcher unit tests (RIG-2732 T7): the production dispatch adapter
// that resolves a subscriber's account to its live session and wraps the
// notification as an AgentControl before dispatch. Driven through a fake
// notifySessionDispatcher so the resolve/miss branches and the AgentControl
// wrapping are exercised without a live hub or Postgres — the pgtests fake the
// whole ingest.NotifyDispatcher, so this is the only coverage of the real glue.

import (
	"context"
	"errors"
	"testing"

	"github.com/RigelBuild/compass/go/internal/forge"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// fakeSessionDispatcher is a notifySessionDispatcher double: a fixed account->
// session binding plus a recording DispatchControl that captures what was sent.
type fakeSessionDispatcher struct {
	binding map[store.AccountID]string // account -> live session, ok=false when absent

	dispatched   bool
	gotSessionID string
	gotOp        *compassv1internal.AgentControl
	dispatchErr  error // returned by DispatchControl (nil = success)
}

func (f *fakeSessionDispatcher) SessionForAccount(account store.AccountID) (string, bool) {
	s, ok := f.binding[account]
	return s, ok
}

func (f *fakeSessionDispatcher) DispatchControl(_ context.Context, sessionID string, op *compassv1internal.AgentControl) error {
	f.dispatched = true
	f.gotSessionID = sessionID
	f.gotOp = op
	return f.dispatchErr
}

// TestForgeNotifyDispatcherNoLiveSession: an account with no live session returns
// the errNoLiveSession sentinel and dispatches nothing — the router depends on
// this to log-and-move-on (W3), and it must never advance a cursor.
func TestForgeNotifyDispatcherNoLiveSession(t *testing.T) {
	fake := &fakeSessionDispatcher{binding: map[store.AccountID]string{}}
	d := &forgeNotifyDispatcher{hub: fake}

	err := d.Notify(context.Background(), "acct-none", &compassv1internal.ForgeNotification{SubscriptionId: "sub-1"})
	if !errors.Is(err, errNoLiveSession) {
		t.Fatalf("Notify (no session) = %v, want errNoLiveSession", err)
	}
	if fake.dispatched {
		t.Fatal("Notify dispatched a control frame for an account with no live session, want none")
	}
}

// TestForgeNotifyDispatcherLiveSessionDispatches: an account with a live session
// dispatches to THAT session, wrapping the notification in the ForgeNotification
// AgentControl oneof (the exact variant an agent runtime reads) and carrying the
// same notification pointer.
func TestForgeNotifyDispatcherLiveSessionDispatches(t *testing.T) {
	const account = "acct-live"
	const sessionID = "sess-42"
	fake := &fakeSessionDispatcher{binding: map[store.AccountID]string{account: sessionID}}
	d := &forgeNotifyDispatcher{hub: fake}

	n := &compassv1internal.ForgeNotification{
		SubscriptionId: "sub-1",
		Repo:           "owner/repo",
		Number:         7,
		Revision:       "rev-9",
	}
	if err := d.Notify(context.Background(), account, n); err != nil {
		t.Fatalf("Notify (live session) = %v, want nil", err)
	}
	if !fake.dispatched {
		t.Fatal("Notify dispatched nothing to the live session")
	}
	if fake.gotSessionID != sessionID {
		t.Fatalf("dispatched to session %q, want %q", fake.gotSessionID, sessionID)
	}
	fn := fake.gotOp.GetForgeNotification()
	if fn == nil {
		t.Fatalf("dispatched op = %+v, want an AgentControl_ForgeNotification oneof", fake.gotOp)
	}
	if fn != n {
		t.Fatalf("dispatched notification = %+v, want the same pointer passed to Notify (%+v)", fn, n)
	}
}

// TestForgeNotifyDispatcherPropagatesDispatchError: a DispatchControl failure
// (no live stream on the resolved session) propagates to the caller so the
// router treats it as a failed dispatch — it is NOT swallowed.
func TestForgeNotifyDispatcherPropagatesDispatchError(t *testing.T) {
	const account = "acct-live"
	dispatchErr := errors.New("no live stream")
	fake := &fakeSessionDispatcher{
		binding:     map[store.AccountID]string{account: "sess-1"},
		dispatchErr: dispatchErr,
	}
	d := &forgeNotifyDispatcher{hub: fake}

	err := d.Notify(context.Background(), account, &compassv1internal.ForgeNotification{SubscriptionId: "sub-1"})
	if !errors.Is(err, dispatchErr) {
		t.Fatalf("Notify (dispatch error) = %v, want the dispatch error propagated", err)
	}
	// Not the no-session sentinel — the session resolved, the dispatch failed.
	if errors.Is(err, errNoLiveSession) {
		t.Fatal("Notify returned errNoLiveSession on a resolved-session dispatch failure, want the dispatch error")
	}
}

// compile-time proof the fake satisfies the interface the dispatcher depends on.
var _ notifySessionDispatcher = (*fakeSessionDispatcher)(nil)

// recordingEventSink is a ForgeEventSink double that records every event.
type recordingEventSink struct{ got []forge.ForgeEvent }

func (r *recordingEventSink) Enqueue(_ context.Context, ev forge.ForgeEvent) {
	r.got = append(r.got, ev)
}

// TestFanoutSinkDeliversToEverySink: an event enqueued on a multi-sink fanout
// reaches EVERY registered sink (the board arm + the notify arm compose here so
// one accepted /webhooks/github delivery fans out to both). Pins the
// both-sinks-receive invariant the wiring depends on.
func TestFanoutSinkDeliversToEverySink(t *testing.T) {
	a := &recordingEventSink{}
	b := &recordingEventSink{}
	f := &fanoutSink{sinks: []ForgeEventSink{a, b}}

	ev := forge.ForgeEvent{Repo: "owner/repo", Number: 7}
	f.Enqueue(context.Background(), ev)

	if len(a.got) != 1 || len(b.got) != 1 {
		t.Fatalf("fanout delivery: a=%d b=%d, want 1 each", len(a.got), len(b.got))
	}
	if a.got[0].Repo != "owner/repo" || a.got[0].Number != 7 {
		t.Fatalf("sink a received %+v, want the enqueued event", a.got[0])
	}
}
