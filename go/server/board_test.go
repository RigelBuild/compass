//go:build unix

package server

// Default-lane (no database) tests for the shared transition executor
// (boardService.SetIssueState): the frozen compare-and-transition
// (compass-issue-model/design.md:513-521). The executor is driven against a fake
// issueStore and a real IssueProjection over a real bus, so the
// read->validate->commit->record+publish->mirror path is provable without
// Postgres — the store interface is narrow exactly so this lane can fake it. The
// pgtest-backed store contract (SetIssueState/GetIssue against a real DB) is
// proven in the store package's own pgtest suite; here we pin the executor's
// decision logic, publish behavior, and the nil-safe mirror.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// boardTestTimeout bounds every fan-out wait; a safety net, never a sleep.
const boardTestTimeout = 2 * time.Second

// fakeIssueStore is an in-memory issueStore: GetIssue reads the current row and
// SetIssueState mutates it, so the executor's compare-and-transition runs its
// full read->commit->read-back path without a database. It records how many
// times SetIssueState was called so a no-op case can assert zero writes.
type fakeIssueStore struct {
	mu     sync.Mutex
	issues map[string]store.Issue
	setErr error // if set, SetIssueState returns it verbatim
	// readBackErr, if set, is returned by GetIssue only on the post-commit
	// read-back (setWrites>0), leaving the pre-commit read unaffected.
	readBackErr error
	setWrites   int
}

func newFakeIssueStore(seed ...store.Issue) *fakeIssueStore {
	f := &fakeIssueStore{issues: make(map[string]store.Issue)}
	for _, iss := range seed {
		f.issues[iss.ID] = iss
	}
	return f
}

func (f *fakeIssueStore) GetIssue(_ context.Context, id string) (store.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readBackErr != nil && f.setWrites > 0 {
		return store.Issue{}, f.readBackErr
	}
	iss, ok := f.issues[id]
	if !ok {
		return store.Issue{}, store.ErrNotFound
	}
	return iss, nil
}

func (f *fakeIssueStore) SetIssueState(_ context.Context, id string, state store.IssueState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	iss, ok := f.issues[id]
	if !ok {
		return store.ErrNotFound
	}
	f.setWrites++
	iss.State = state
	f.issues[id] = iss
	return nil
}

// recordingMirror is a trackerMirror that records every committed issue it was
// asked to mirror, so a test can assert the outbound mirror fired (or was elided
// for ARCHIVED).
type recordingMirror struct {
	mu   sync.Mutex
	seen []store.Issue
	err  error
}

func (m *recordingMirror) MirrorIssueState(_ context.Context, committed store.Issue) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen = append(m.seen, committed)
	return m.err
}

func (m *recordingMirror) snapshot() []store.Issue {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.Issue(nil), m.seen...)
}

// newBoardServiceForTest builds a boardService over the fake store and a real
// IssueProjection (nil store — RecordAndPublish does no store I/O), returning the
// service and its bus so a test can subscribe and assert the fan-out.
func newBoardServiceForTest(t *testing.T, st issueStore) (*boardService, *events.Bus[busPayload]) {
	t.Helper()
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	b := &boardService{
		store:    st,
		issueBrd: board.NewIssueProjection(bus, nil),
	}
	return b, bus
}

// agentSource is the agent-initiated transition source used across these cases.
func agentSource() TransitionSource {
	return TransitionSource{Kind: SourceAgent, Actor: store.AccountID("acct-agent")}
}

// TestSetIssueStateCommitsAndPublishesOnRealTransition pins the happy path: a
// real transition (BACKLOG -> IN_PROGRESS) commits the new state, reads it back,
// and records+publishes the committed truth on the projection. A subscriber
// registered before the call receives the committed issue as the issue=16
// variant, and the returned issue carries the new state.
func TestSetIssueStateCommitsAndPublishesOnRealTransition(t *testing.T) {
	st := newFakeIssueStore(store.Issue{ID: "iss-1", State: store.IssueStateBacklog})
	b, bus := newBoardServiceForTest(t, st)

	sub, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	got, err := b.SetIssueState(context.Background(), "acct-agent", "iss-1", store.IssueStateInProgress, agentSource())
	if err != nil {
		t.Fatalf("SetIssueState(real transition) = %v, want success", err)
	}
	if got.State != store.IssueStateInProgress {
		t.Fatalf("returned state = %v, want IN_PROGRESS (committed read-back)", got.State)
	}
	if st.setWrites != 1 {
		t.Fatalf("store SetIssueState called %d times, want 1", st.setWrites)
	}

	select {
	case e, ok := <-sub.Live:
		if !ok {
			t.Fatal("live channel closed before an event arrived")
		}
		iss := e.Payload.GetIssue()
		if iss == nil || iss.GetId() != "iss-1" ||
			iss.GetState() != compassv1.IssueState_ISSUE_STATE_IN_PROGRESS {
			t.Fatalf("fanned payload = %v, want iss-1/IN_PROGRESS", e.Payload)
		}
	case <-time.After(boardTestTimeout):
		t.Fatal("timed out waiting for the fanned transition")
	}
}

// TestSetIssueStateRejectsUnspecifiedTarget pins that an UNSPECIFIED target is
// refused CodeInvalidArgument, with NO store write — the proto zero is not a
// real lifecycle. The rejection happens inside the serialized transition, so it
// is checked against current truth, not as a separable pre-lock step.
func TestSetIssueStateRejectsUnspecifiedTarget(t *testing.T) {
	st := newFakeIssueStore(store.Issue{ID: "iss-1", State: store.IssueStateTodo})
	b, _ := newBoardServiceForTest(t, st)

	_, err := b.SetIssueState(context.Background(), "acct-agent", "iss-1", store.IssueStateUnspecified, agentSource())
	if err == nil {
		t.Fatal("SetIssueState(UNSPECIFIED) = nil error, want CodeInvalidArgument")
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("UNSPECIFIED target error code = %v, want InvalidArgument", got)
	}
	if st.setWrites != 0 {
		t.Fatalf("store SetIssueState called %d times for a rejected target, want 0", st.setWrites)
	}
}

// TestSetIssueStateUnknownIssueIsNotFound pins that a transition on an issue the
// store does not know is CodeNotFound (mapped from store.ErrNotFound), so the
// hub renders it in-band as a not_found BoardCallError. No write is attempted.
func TestSetIssueStateUnknownIssueIsNotFound(t *testing.T) {
	st := newFakeIssueStore() // empty
	b, _ := newBoardServiceForTest(t, st)

	_, err := b.SetIssueState(context.Background(), "acct-agent", "ghost", store.IssueStateDone, agentSource())
	if err == nil {
		t.Fatal("SetIssueState(unknown issue) = nil error, want CodeNotFound")
	}
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("unknown-issue error code = %v, want NotFound", got)
	}
	if st.setWrites != 0 {
		t.Fatalf("store SetIssueState called %d times for an unknown issue, want 0", st.setWrites)
	}
}

// TestSetIssueStateNoOpOnSameStateDoesNotPublish pins the idempotent no-op: a
// target equal to current state returns current truth with NO store write and NO
// publish (nothing changed). ARCHIVED is included in the any-to-any idempotency
// (a re-archive is a no-op), pinned by the ARCHIVED sub-case.
func TestSetIssueStateNoOpOnSameStateDoesNotPublish(t *testing.T) {
	cases := []struct {
		name  string
		state store.IssueState
	}{
		{"in_progress", store.IssueStateInProgress},
		{"archived", store.IssueStateArchived},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeIssueStore(store.Issue{ID: "iss-1", State: tc.state})
			b, bus := newBoardServiceForTest(t, st)

			sub, err := bus.Subscribe(0, bus.InstanceEpoch())
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			t.Cleanup(sub.Cancel)

			got, err := b.SetIssueState(context.Background(), "acct-agent", "iss-1", tc.state, agentSource())
			if err != nil {
				t.Fatalf("SetIssueState(same state) = %v, want success no-op", err)
			}
			if got.State != tc.state {
				t.Fatalf("no-op returned state = %v, want the unchanged %v", got.State, tc.state)
			}
			if st.setWrites != 0 {
				t.Fatalf("store SetIssueState called %d times on a no-op, want 0 (no commit)", st.setWrites)
			}
			// No publish on a no-op: the live channel must carry nothing.
			select {
			case e := <-sub.Live:
				t.Fatalf("a no-op published a fan-out event %v, want none", e.Payload)
			case <-time.After(50 * time.Millisecond):
				// No event, as required.
			}
		})
	}
}

// TestSetIssueStateCallsNilSafeMirrorOnRealTransition pins the outbound tracker
// mirror seam: a wired mirror fires on a real transition (carrying the committed
// issue), and — the nil-safe guarantee — a nil mirror is a clean no-op (no
// panic). ARCHIVED is elided from the mirror (it has no tracker status), pinned
// by the archived sub-case.
func TestSetIssueStateCallsNilSafeMirrorOnRealTransition(t *testing.T) {
	t.Run("wired mirror fires with committed issue", func(t *testing.T) {
		st := newFakeIssueStore(store.Issue{ID: "iss-1", State: store.IssueStateBacklog})
		b, _ := newBoardServiceForTest(t, st)
		mirror := &recordingMirror{}
		b.mirror = mirror

		if _, err := b.SetIssueState(context.Background(), "acct-agent", "iss-1", store.IssueStateInProgress, agentSource()); err != nil {
			t.Fatalf("SetIssueState = %v, want success", err)
		}
		seen := mirror.snapshot()
		if len(seen) != 1 || seen[0].ID != "iss-1" || seen[0].State != store.IssueStateInProgress {
			t.Fatalf("mirror saw %+v, want one committed iss-1/IN_PROGRESS", seen)
		}
	})
	t.Run("nil mirror is a clean no-op", func(t *testing.T) {
		st := newFakeIssueStore(store.Issue{ID: "iss-1", State: store.IssueStateBacklog})
		b, _ := newBoardServiceForTest(t, st) // mirror stays nil
		if _, err := b.SetIssueState(context.Background(), "acct-agent", "iss-1", store.IssueStateTodo, agentSource()); err != nil {
			t.Fatalf("SetIssueState with a nil mirror = %v, want a clean success", err)
		}
	})
	t.Run("ARCHIVED is elided from the mirror", func(t *testing.T) {
		st := newFakeIssueStore(store.Issue{ID: "iss-1", State: store.IssueStateDone})
		b, _ := newBoardServiceForTest(t, st)
		mirror := &recordingMirror{}
		b.mirror = mirror

		if _, err := b.SetIssueState(context.Background(), "acct-agent", "iss-1", store.IssueStateArchived, agentSource()); err != nil {
			t.Fatalf("SetIssueState(->ARCHIVED) = %v, want success", err)
		}
		if seen := mirror.snapshot(); len(seen) != 0 {
			t.Fatalf("mirror fired %d times for an ARCHIVED transition, want 0 (elided)", len(seen))
		}
	})
}

// TestSetIssueStateCommitErrorMapsThroughTransitionStoreError covers the
// commit-failure branch (store.SetIssueState returns a non-sentinel error):
// transitionStoreError's default arm maps it to CodeInternal and the failure is
// before RecordAndPublish, so nothing fans out. Kills a mutant that dropped the
// SetIssueState error check or mis-mapped the code.
func TestSetIssueStateCommitErrorMapsThroughTransitionStoreError(t *testing.T) {
	st := newFakeIssueStore(store.Issue{ID: "iss-1", State: store.IssueStateBacklog})
	st.setErr = errors.New("boom commit") // non-sentinel -> default arm (CodeInternal)
	b, bus := newBoardServiceForTest(t, st)

	sub, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	_, err = b.SetIssueState(context.Background(), "acct-agent", "iss-1", store.IssueStateInProgress, agentSource())
	if err == nil {
		t.Fatal("SetIssueState(commit error) = nil error, want CodeInternal")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("commit error code = %v, want Internal", got)
	}
	select {
	case e := <-sub.Live:
		t.Fatalf("a commit failure published a fan-out event %v, want none", e.Payload)
	case <-time.After(50 * time.Millisecond):
		// No event, as required (failure is before RecordAndPublish).
	}
}

// TestSetIssueStateReadBackErrorMapsThroughTransitionStoreError covers the
// post-commit read-back-failure branch (the SECOND GetIssue returns a
// non-sentinel error): transitionStoreError maps it to CodeInternal. The commit
// DID happen (setWrites==1) but RecordAndPublish is after the read-back, so no
// event fans out — this pins the design's commit-then-read-back ordering. Kills
// a mutant that dropped the read-back error check.
func TestSetIssueStateReadBackErrorMapsThroughTransitionStoreError(t *testing.T) {
	st := newFakeIssueStore(store.Issue{ID: "iss-1", State: store.IssueStateBacklog})
	st.readBackErr = errors.New("boom readback") // non-sentinel -> default arm (CodeInternal)
	b, bus := newBoardServiceForTest(t, st)

	sub, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	_, err = b.SetIssueState(context.Background(), "acct-agent", "iss-1", store.IssueStateInProgress, agentSource())
	if err == nil {
		t.Fatal("SetIssueState(read-back error) = nil error, want CodeInternal")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("read-back error code = %v, want Internal", got)
	}
	if st.setWrites != 1 {
		t.Fatalf("store SetIssueState called %d times, want 1 (the commit did happen)", st.setWrites)
	}
	select {
	case e := <-sub.Live:
		t.Fatalf("a read-back failure published a fan-out event %v, want none", e.Payload)
	case <-time.After(50 * time.Millisecond):
		// No event: RecordAndPublish is after the read-back, so a read-back
		// failure means no publish.
	}
}

// TestSetIssueStateMirrorErrorSurfacesAsInternal covers the mirror-failure
// branch: a wired mirror returning an error is wrapped as CodeInternal. The
// mirror WAS called with the committed issue (snapshot len==1), and — the
// low-severity ordering wart PR-C will resolve — RecordAndPublish runs BEFORE
// the mirror, so the publish already fired even though the call reports failure.
// Kills a mutant that swallowed the mirror error.
func TestSetIssueStateMirrorErrorSurfacesAsInternal(t *testing.T) {
	st := newFakeIssueStore(store.Issue{ID: "iss-1", State: store.IssueStateBacklog})
	b, bus := newBoardServiceForTest(t, st)
	mirror := &recordingMirror{err: errors.New("boom mirror")}
	b.mirror = mirror

	sub, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	_, err = b.SetIssueState(context.Background(), "acct-agent", "iss-1", store.IssueStateInProgress, agentSource())
	if err == nil {
		t.Fatal("SetIssueState(mirror error) = nil error, want CodeInternal")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("mirror error code = %v, want Internal", got)
	}
	if seen := mirror.snapshot(); len(seen) != 1 || seen[0].ID != "iss-1" || seen[0].State != store.IssueStateInProgress {
		t.Fatalf("mirror saw %+v, want one committed iss-1/IN_PROGRESS", seen)
	}
	// The publish fires BEFORE the mirror (the error-after-commit ordering wart
	// PR-C resolves): a mirror failure does not un-publish the transition.
	select {
	case e, ok := <-sub.Live:
		if !ok {
			t.Fatal("live channel closed before an event arrived")
		}
		if iss := e.Payload.GetIssue(); iss == nil || iss.GetId() != "iss-1" ||
			iss.GetState() != compassv1.IssueState_ISSUE_STATE_IN_PROGRESS {
			t.Fatalf("fanned payload = %v, want iss-1/IN_PROGRESS", e.Payload)
		}
	case <-time.After(boardTestTimeout):
		t.Fatal("timed out waiting for the fanned transition (publish precedes the mirror)")
	}
}

// TestSetIssueStateAsAccountMapsRequestAndAttributesActor pins the BoardCaller
// entry point: it maps the request's issue_id + target onto the executor as an
// agent-sourced transition, and returns the post-transition issue on the wire.
// The successful commit proves the request fields threaded through; the returned
// wire Issue proves the store<->wire mapping ran.
func TestSetIssueStateAsAccountMapsRequestAndReturnsWireIssue(t *testing.T) {
	st := newFakeIssueStore(store.Issue{ID: "iss-1", State: store.IssueStateBacklog})
	b, _ := newBoardServiceForTest(t, st)

	resp, err := b.SetIssueStateAsAccount(context.Background(), "acct-agent", &compassv1internal.SetIssueStateRequest{
		IssueId: "iss-1",
		State:   compassv1.IssueState_ISSUE_STATE_QUEUED,
	})
	if err != nil {
		t.Fatalf("SetIssueStateAsAccount = %v, want success", err)
	}
	if resp.GetIssue().GetId() != "iss-1" ||
		resp.GetIssue().GetState() != compassv1.IssueState_ISSUE_STATE_QUEUED {
		t.Fatalf("response issue = %v, want iss-1/QUEUED", resp.GetIssue())
	}
}

// unusedTransitionSourceKinds keeps the non-agent SourceKind values referenced
// so the fully-defined enum (agent | tracker | auto) is not flagged as unused
// while only SourceAgent is exercised in this PR — the tracker/auto producers
// (PR-B/PR-C) supply them without changing the type.
var _ = []SourceKind{SourceTracker, SourceAuto}
