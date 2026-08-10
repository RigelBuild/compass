//go:build unix

package board

// Default-lane (no database) tests for the Bridge board projection. They
// exercise the public Projection surface over an in-process events.Bus and pin
// the board contract (design.md:1585-1604): a per-session aggregate of
// the latest AgentSessionState (last-write-wins), a deterministically sorted
// snapshot that hands out independent copies, a nil/empty-session guard, and a
// live fan-out onto SubscribeEvents.
//
// Every live-channel wait is event-gated with a deadline as a safety net, never
// as a synchronization device: negative ("nothing published") assertions are
// proven by publishing a real sentinel after the ignored inputs and asserting
// the sentinel is the first event delivered, not by racing a timer.

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
)

// testTimeout bounds every live-channel wait so a broken fan-out fails fast
// instead of hanging the suite. It is a deadline, not a sleep — the tests gate
// on channel receives.
const testTimeout = 5 * time.Second

const (
	starting     = compassv1.AgentSessionState_AGENT_SESSION_STATE_STARTING
	ready        = compassv1.AgentSessionState_AGENT_SESSION_STATE_READY
	working      = compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING
	stopped      = compassv1.AgentSessionState_AGENT_SESSION_STATE_STOPPED
	errored      = compassv1.AgentSessionState_AGENT_SESSION_STATE_ERRORED
	disconnected = compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED
)

// newBoard builds a Projection over a fresh bus, closing the bus at test end.
func newBoard(t *testing.T) (*Projection, *events.Bus[busPayload]) {
	t.Helper()
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	return NewProjection(bus), bus
}

// status is a terse AgentSessionStatus constructor for test inputs.
func status(sessionID string, state compassv1.AgentSessionState) *compassv1.AgentSessionStatus {
	return &compassv1.AgentSessionStatus{SessionId: sessionID, State: state}
}

// recvStatus reads one live event and returns its AgentSessionStatus payload.
// It fails the test if the channel closed early or nothing arrived within the
// deadline. The returned status is the wire copy the bus stamped, not the input.
func recvStatus(t *testing.T, ch <-chan events.Stamped[busPayload]) *compassv1.AgentSessionStatus {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("live channel closed before an event arrived")
		}
		got := e.Payload.GetAgentSessionStatus()
		if got == nil {
			t.Fatalf("live event carried a non-AgentSessionStatus payload: %v", e.Payload)
		}
		return got
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for a live event")
		return nil
	}
}

// idsOf projects a snapshot to its session ids, preserving order.
func idsOf(snap []*compassv1.AgentSessionStatus) []string {
	ids := make([]string, len(snap))
	for i, s := range snap {
		ids[i] = s.GetSessionId()
	}
	return ids
}

// TestRecordsLatestStateLastWriteWins pins that a later transition for a
// session overwrites the earlier one: the snapshot reflects the most recent
// state, never a stale or first-seen one.
func TestRecordsLatestStateLastWriteWins(t *testing.T) {
	p, _ := newBoard(t)

	p.PublishSessionStatus(status("s1", starting))
	p.PublishSessionStatus(status("s1", working))

	snap := p.Snapshot("s1")
	if len(snap) != 1 {
		t.Fatalf("Snapshot(s1) len = %d, want 1", len(snap))
	}
	if got := snap[0].GetState(); got != working {
		t.Errorf("state = %v, want WORKING (last write wins)", got)
	}
}

// TestSnapshotByIDAndAllSorted pins the two Snapshot modes: an all-sessions
// query returns every held session sorted ascending by session_id (regardless
// of insertion order), a by-id query returns just that session, and an unknown
// id returns nothing.
func TestSnapshotByIDAndAllSorted(t *testing.T) {
	p, _ := newBoard(t)

	// Insertion order is deliberately unsorted to prove the sort, not luck.
	p.PublishSessionStatus(status("s3", working))
	p.PublishSessionStatus(status("s1", starting))
	p.PublishSessionStatus(status("s2", ready))

	all := p.Snapshot("")
	wantIDs := []string{"s1", "s2", "s3"}
	gotIDs := idsOf(all)
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("Snapshot(\"\") ids = %v, want %v", gotIDs, wantIDs)
	}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("Snapshot(\"\") ids = %v, want ascending %v", gotIDs, wantIDs)
		}
	}
	// The states must travel with their ids after the sort.
	wantStates := map[string]compassv1.AgentSessionState{"s1": starting, "s2": ready, "s3": working}
	for _, s := range all {
		if got := s.GetState(); got != wantStates[s.GetSessionId()] {
			t.Errorf("state for %s = %v, want %v", s.GetSessionId(), got, wantStates[s.GetSessionId()])
		}
	}

	one := p.Snapshot("s2")
	if len(one) != 1 || one[0].GetSessionId() != "s2" || one[0].GetState() != ready {
		t.Errorf("Snapshot(s2) = %v, want single s2/READY", one)
	}

	if miss := p.Snapshot("nope"); len(miss) != 0 {
		t.Errorf("Snapshot(nope) = %v, want empty", miss)
	}
}

// TestPublishFansOutOntoBus pins the live fan-out: a subscriber registered
// before the publish receives the transition as the AgentSessionStatus variant,
// carrying the published session_id and state.
func TestPublishFansOutOntoBus(t *testing.T) {
	p, bus := newBoard(t)

	sub, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	p.PublishSessionStatus(status("s1", working))

	got := recvStatus(t, sub.Live)
	if got.GetSessionId() != "s1" || got.GetState() != working {
		t.Errorf("fan-out payload = %s/%v, want s1/WORKING", got.GetSessionId(), got.GetState())
	}
}

// TestSnapshotReturnsIndependentCopies pins that Snapshot hands out entries the
// caller owns: mutating a returned entry must not corrupt the board's own
// state. Guards against handing out a shared pointer into the aggregate.
func TestSnapshotReturnsIndependentCopies(t *testing.T) {
	p, _ := newBoard(t)
	p.PublishSessionStatus(status("s1", starting))

	first := p.Snapshot("s1")
	first[0].SessionId = "hijacked"
	first[0].State = working

	again := p.Snapshot("s1")
	if len(again) != 1 {
		t.Fatalf("Snapshot(s1) len = %d, want 1 after mutation", len(again))
	}
	if again[0].GetSessionId() != "s1" || again[0].GetState() != starting {
		t.Errorf("board copy = %s/%v after caller mutation, want s1/STARTING (independent copy)",
			again[0].GetSessionId(), again[0].GetState())
	}
}

// TestNilAndEmptySessionIgnored pins the input guard: a nil status and one with
// an empty session_id are dropped — neither recorded nor fanned out. The
// no-publish half is proven deterministically by fanning a real sentinel after
// the ignored inputs and asserting it is the FIRST live event, so an errant
// publish of an ignored input would surface as a wrong first event, not a race.
func TestNilAndEmptySessionIgnored(t *testing.T) {
	p, bus := newBoard(t)

	sub, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	p.PublishSessionStatus(nil)
	p.PublishSessionStatus(status("", ready)) // empty session_id

	if snap := p.Snapshot(""); len(snap) != 0 {
		t.Errorf("Snapshot(\"\") = %v after nil/empty publishes, want empty (nothing recorded)", idsOf(snap))
	}

	// The sentinel is the only valid publish; if either ignored input had been
	// fanned out, it would be the first event instead.
	p.PublishSessionStatus(status("sentinel", working))
	got := recvStatus(t, sub.Live)
	if got.GetSessionId() != "sentinel" {
		t.Errorf("first live event = %s, want sentinel (nil/empty must not publish)", got.GetSessionId())
	}
}

// TestConcurrentPublishAndSnapshotRaceClean is the concurrency guard: N
// goroutines publish distinct sessions while another reads Snapshot("") in a
// loop. Under -race it proves the RWMutex serializes the map correctly; the
// final snapshot must hold exactly N entries. The reader loop is joined before
// the final assertion so there is no wall-clock dependence.
func TestConcurrentPublishAndSnapshotRaceClean(t *testing.T) {
	p, _ := newBoard(t)

	const n = 64
	var writers sync.WaitGroup
	for i := range n {
		writers.Go(func() {
			p.PublishSessionStatus(status(fmt.Sprintf("s%03d", i), working))
		})
	}

	// A concurrent reader that runs until the writers finish, exercising the
	// RLock path against the writers' Lock path.
	done := make(chan struct{})
	var reader sync.WaitGroup
	reader.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
				_ = p.Snapshot("")
			}
		}
	})

	writers.Wait()
	close(done)
	reader.Wait()

	if snap := p.Snapshot(""); len(snap) != n {
		t.Errorf("final Snapshot len = %d, want %d", len(snap), n)
	}
}

// TestTerminalFilteredFromAllButQueryableByID pins the terminal-session contract
// (review-fix round): STOPPED/ERRORED are excluded from the all-sessions
// Snapshot(""), DISCONNECTED is kept as live, and a terminal session stays
// answerable by single id. It defends three regressions at once:
//   - dropping the terminal filter (a STOPPED/ERRORED session leaking into the
//     "every live session" listing);
//   - a naive "live == not stopped/errored/disconnected" filter that would
//     wrongly drop DISCONNECTED (which awaits reattach, so it is LIVE);
//   - evicting a terminal session from the map (Matt ruled: keep it queryable by
//     id so a caller polling a known id still learns the final state).
func TestTerminalFilteredFromAllButQueryableByID(t *testing.T) {
	p, _ := newBoard(t)

	p.PublishSessionStatus(status("s1", working))      // live
	p.PublishSessionStatus(status("s2", stopped))      // terminal
	p.PublishSessionStatus(status("s3", errored))      // terminal
	p.PublishSessionStatus(status("s4", disconnected)) // live (awaits reattach)

	// All-mode: exactly the two live sessions, sorted; terminals filtered out,
	// DISCONNECTED retained.
	all := p.Snapshot("")
	wantIDs := []string{"s1", "s4"}
	if gotIDs := idsOf(all); len(gotIDs) != len(wantIDs) ||
		gotIDs[0] != wantIDs[0] || gotIDs[1] != wantIDs[1] {
		t.Fatalf("Snapshot(\"\") ids = %v, want %v (terminals excluded, DISCONNECTED kept live, sorted)",
			idsOf(all), wantIDs)
	}
	states := map[string]compassv1.AgentSessionState{}
	for _, s := range all {
		states[s.GetSessionId()] = s.GetState()
	}
	if states["s4"] != disconnected {
		t.Errorf("s4 state in all-mode = %v, want DISCONNECTED (DISCONNECTED is live, not terminal)", states["s4"])
	}

	// By-id: each terminal session is still held with its terminal state.
	if one := p.Snapshot("s2"); len(one) != 1 || one[0].GetState() != stopped {
		t.Errorf("Snapshot(s2) = %v, want single s2/STOPPED (terminal retained, queryable by id)", one)
	}
	if one := p.Snapshot("s3"); len(one) != 1 || one[0].GetState() != errored {
		t.Errorf("Snapshot(s3) = %v, want single s3/ERRORED (terminal retained, queryable by id)", one)
	}
}

// TestLiveToTerminalTransitionLeavesAllModeKeepsByID pins the transition: a
// session that was live and listed becomes terminal and drops out of the
// all-sessions listing, yet is still answerable by id with its terminal state.
// A regression that recorded the terminal state but forgot to filter it from
// all-mode (or one that evicted the row on terminal) reddens exactly one half.
func TestLiveToTerminalTransitionLeavesAllModeKeepsByID(t *testing.T) {
	p, _ := newBoard(t)

	p.PublishSessionStatus(status("s1", working))
	if ids := idsOf(p.Snapshot("")); len(ids) != 1 || ids[0] != "s1" {
		t.Fatalf("Snapshot(\"\") = %v after WORKING, want [s1]", ids)
	}

	p.PublishSessionStatus(status("s1", stopped))
	if ids := idsOf(p.Snapshot("")); len(ids) != 0 {
		t.Errorf("Snapshot(\"\") = %v after STOPPED, want empty (terminal drops from all-mode)", ids)
	}
	if one := p.Snapshot("s1"); len(one) != 1 || one[0].GetState() != stopped {
		t.Errorf("Snapshot(s1) = %v after STOPPED, want single s1/STOPPED (kept queryable by id)", one)
	}
}

// TestUnspecifiedStateIgnored pins the new UNSPECIFIED guard (defense-in-depth):
// a status carrying the zero state is neither recorded nor fanned out. The
// no-record half is proven by Snapshot("s9") being empty; the no-publish half is
// proven deterministically — a real sentinel published after must be the FIRST
// live event, so an errant UNSPECIFIED fan-out would surface as a wrong first
// event, not a timer race. A regression that dropped the UNSPECIFIED clause from
// the input guard would record a phantom zero-state row and fan a bogus event.
func TestUnspecifiedStateIgnored(t *testing.T) {
	p, bus := newBoard(t)

	sub, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	p.PublishSessionStatus(status("s9", compassv1.AgentSessionState_AGENT_SESSION_STATE_UNSPECIFIED))

	if snap := p.Snapshot("s9"); len(snap) != 0 {
		t.Errorf("Snapshot(s9) = %v after UNSPECIFIED publish, want empty (nothing recorded)", idsOf(snap))
	}

	p.PublishSessionStatus(status("sentinel", working))
	if got := recvStatus(t, sub.Live); got.GetSessionId() != "sentinel" {
		t.Errorf("first live event = %s, want sentinel (UNSPECIFIED must not publish)", got.GetSessionId())
	}
}

// TestConcurrentSameSessionRecordPublishAgree pins the record+publish atomicity
// invariant for the review-fix round: two goroutines race distinct live states
// onto ONE session id. Because PublishSessionStatus records and fans under one
// write lock, the state Snapshot(id) returns after both writers join must equal
// the state of the LAST AgentSessionStatus a subscriber saw — the map and the
// bus can never diverge. A regression that split record and publish out of the
// lock (or reordered them) could let the snapshot hold one writer's state while
// the last bus event carried the other's; this reddens on that disagreement.
// Joined before asserting (no wall-clock dependence); -race clean.
func TestConcurrentSameSessionRecordPublishAgree(t *testing.T) {
	p, bus := newBoard(t)

	sub, err := bus.Subscribe(0, bus.InstanceEpoch())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	// Two live (non-terminal) states so the session stays in all-mode and the
	// last-write-wins outcome is one of exactly these two.
	raced := []compassv1.AgentSessionState{ready, working}
	var writers sync.WaitGroup
	for _, st := range raced {
		writers.Go(func() {
			p.PublishSessionStatus(status("s1", st))
		})
	}
	writers.Wait()

	// Drain every fanned event; the last one is what a subscriber ends on. Both
	// publishes are non-blocking sends already committed under the lock, so a
	// bounded 2-event drain (gated on receives) captures them without a timer.
	var lastBus compassv1.AgentSessionState
	for range raced {
		lastBus = recvStatus(t, sub.Live).GetState()
	}

	snap := p.Snapshot("s1")
	if len(snap) != 1 {
		t.Fatalf("Snapshot(s1) len = %d, want 1", len(snap))
	}
	if snap[0].GetState() != lastBus {
		t.Errorf("Snapshot(s1) state = %v, last bus event = %v — must agree (record+publish atomic under one lock)",
			snap[0].GetState(), lastBus)
	}
}

// TestSnapshotEchoesAgentAccount pins the DL-167 account echo through the board:
// a recorded status carries its agent_account_id into the snapshot (both the
// by-id and all-sessions arms), and a status with no account round-trips an
// empty account — the stated residual gap (an unbound session carries none),
// not a wrong or dropped value.
//
// Mutation: dropping entry.account from statusOf (projection.go:156) or from the
// PublishSessionStatus record (projection.go:98) reddens the account assertion —
// the snapshot would carry an empty account for a bound session.
func TestSnapshotEchoesAgentAccount(t *testing.T) {
	p, _ := newBoard(t)

	const account = "0123456789abcdef0123456789abcdef"
	p.PublishSessionStatus(&compassv1.AgentSessionStatus{SessionId: "s-bound", State: working, AgentAccountId: account})
	p.PublishSessionStatus(&compassv1.AgentSessionStatus{SessionId: "s-unbound", State: working})

	byID := p.Snapshot("s-bound")
	if len(byID) != 1 || byID[0].GetAgentAccountId() != account {
		t.Fatalf("Snapshot(s-bound) account = %v, want %q echoed through the board", byID, account)
	}

	unbound := p.Snapshot("s-unbound")
	if len(unbound) != 1 || unbound[0].GetAgentAccountId() != "" {
		t.Fatalf("Snapshot(s-unbound) account = %v, want empty (the unbound residual gap)", unbound)
	}

	// The all-sessions arm carries the account too.
	for _, s := range p.Snapshot("") {
		if s.GetSessionId() == "s-bound" && s.GetAgentAccountId() != account {
			t.Fatalf("Snapshot(\"\") account for s-bound = %q, want %q (the all-arm must echo it)", s.GetAgentAccountId(), account)
		}
	}
}
