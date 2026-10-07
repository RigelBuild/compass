//go:build unix

package adapters

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/stack"
)

// TestGroupSignallerLivenessThenSignalTearsDown drives the real adapter against
// a real child group: owned with the right token, recycled with a wrong one, and
// gone once a delivered SIGTERM ends it. Every wait is event-gated.
func TestGroupSignallerLivenessThenSignalTearsDown(t *testing.T) {
	proc := startHelper(t, "trap", stack.ComponentServer, nil)
	pgid := proc.Pid()
	gs := NewGroupSignaller()

	startTime, err := readGroupLeaderStartTime(pgid)
	if err != nil {
		t.Fatalf("readGroupLeaderStartTime(%d) = %v", pgid, err)
	}
	if got := gs.Liveness(pgid, startTime); got != stack.GroupOwned {
		t.Fatalf("Liveness(%d, matching token) = %v, want GroupOwned", pgid, got)
	}
	if got := gs.Liveness(pgid, startTime+1); got != stack.GroupRecycled {
		t.Fatalf("Liveness(%d, wrong token) = %v, want GroupRecycled", pgid, got)
	}

	if err := gs.Signal(pgid, stack.SignalTerm); err != nil {
		t.Fatalf("Signal(SIGTERM) = %v", err)
	}
	if err := proc.Wait(context.Background()); err != nil {
		t.Fatalf("proc.Wait after SIGTERM = %v", err)
	}
	// The reaped single-member group is ESRCH.
	if got := gs.Liveness(pgid, startTime); got != stack.GroupGone {
		t.Fatalf("Liveness(%d) after exit = %v, want GroupGone", pgid, got)
	}
}

// TestGroupSignallerLivenessOrphanedGroup proves a group whose leader was reaped
// while a member lives is orphaned, never gone, and stays signalable.
func TestGroupSignallerLivenessOrphanedGroup(t *testing.T) {
	memberReady := filepath.Join(t.TempDir(), "member-ready")
	proc := startHelper(t, "forkmember", stack.ComponentServer, []string{helperMemberReadyKey + "=" + memberReady})
	pgid := proc.Pid()
	gs := NewGroupSignaller()
	// Until the group is seen gone, only this test's members can hold the pgid.
	released := false
	t.Cleanup(func() {
		if released {
			return
		}
		if err := gs.Signal(pgid, stack.SignalKill); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Logf("cleanup SIGKILL of group %d: %v", pgid, err)
		}
	})
	waitReady(t, memberReady, proc)

	startTime, err := readGroupLeaderStartTime(pgid)
	if err != nil {
		t.Fatalf("readGroupLeaderStartTime(%d) = %v", pgid, err)
	}
	// Kill and reap only the leader, by its own pid; the member keeps the pgid.
	if err := syscall.Kill(pgid, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL leader %d = %v", pgid, err)
	}
	if err := proc.Wait(context.Background()); err == nil {
		t.Fatal("proc.Wait after leader SIGKILL = nil, want the kill exit error")
	}
	if got := gs.Liveness(pgid, startTime); got != stack.GroupOrphaned {
		t.Fatalf("Liveness(%d) with a reaped leader and a live member = %v, want GroupOrphaned", pgid, got)
	}

	if err := gs.Signal(pgid, stack.SignalKill); err != nil {
		t.Fatalf("Signal(SIGKILL) to orphaned group = %v", err)
	}
	// init reaps the reparented member; poll that event with a deadline.
	deadline := time.Now().Add(5 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for gs.Liveness(pgid, startTime) != stack.GroupGone {
		if !time.Now().Before(deadline) {
			t.Fatalf("orphaned group %d still present 5s after SIGKILL", pgid)
		}
		<-ticker.C
	}
	released = true
}

// TestGroupSignallerLivenessDeadGroup proves a pgid that names no process, and a
// degenerate pgid, report gone rather than erroring or signaling.
func TestGroupSignallerLivenessDeadGroup(t *testing.T) {
	gs := NewGroupSignaller()
	for _, pgid := range []int{deadPGID(t), 1, 0, -1} {
		if got := gs.Liveness(pgid, 12345); got != stack.GroupGone {
			t.Fatalf("Liveness(%d, ...) = %v, want GroupGone", pgid, got)
		}
	}
}

// TestGroupSignallerUnknownSignal proves an unsupported disposition is a legible
// error, never a silent no-op that would leave a group unsignaled.
func TestGroupSignallerUnknownSignal(t *testing.T) {
	gs := NewGroupSignaller()
	// A disposition past the defined SignalTerm/SignalKill.
	if err := gs.Signal(deadPGID(t), stack.ProcessSignal(99)); err == nil {
		t.Fatal("Signal(unknown) = nil, want an error")
	}
}

// TestParseGroupLeaderStatParsesParenthesizedComm guards the adapter's own copy
// of the /proc/<pid>/stat field-22 parse against the parenthesized-comm gotcha:
// a comm with embedded spaces AND parens must not throw off the field count. It
// mirrors stack.TestReadStartTimeProcParsesParenthesizedComm so the two parsers
// (deliberately duplicated, see readGroupLeaderStartTime) cannot drift on the
// load-bearing identity token — a "simplify to strings.Fields(line)" regression
// here would be caught rather than only by the real-/proc integration test whose
// comm has no embedded spaces. The darwin encoding has its own mirrored pair,
// TestPackGroupLeaderTimevalMatchesSpawnSide, for the same reason.
func TestParseGroupLeaderStatParsesParenthesizedComm(t *testing.T) {
	// comm is "(weird )(name)" — embedded spaces and parens; starttime (field 22)
	// is 987654.
	line := "1234 (weird )(name) S 1 1234 1234 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 ...\n"
	got, err := parseGroupLeaderStat(line)
	if err != nil {
		t.Fatalf("parseGroupLeaderStat = %v", err)
	}
	if got != 987654 {
		t.Fatalf("starttime = %d, want 987654", got)
	}
}

// deadPGID returns a pgid that names no live process group: it scans upward from
// a high number until kill(-pgid, 0) reports ESRCH.
func deadPGID(t *testing.T) int {
	t.Helper()
	for pgid := 1 << 20; pgid < (1<<20)+100000; pgid++ {
		if errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
			return pgid
		}
	}
	t.Fatal("could not find a dead pgid")
	return 0
}
