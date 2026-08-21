//go:build unix

package adapters

import (
	"context"
	"testing"

	"github.com/RigelBuild/compass/go/internal/stack"
)

// TestGroupSignallerAliveThenSignalTearsDown drives the real syscall adapter
// against a real child process group (the re-exec helper), with no timing
// guesses: every wait is gated on an event (the child's ready file via
// startHelper, then proc.Wait for exit).
//
//  1. A freshly started, identity-matched group is Alive.
//  2. A wrong start-time token (a recycled pid) reports NOT alive — the identity
//     gate, not bare existence.
//  3. A real SIGTERM through Signal delivers to the group; after the child exits
//     (gated on proc.Wait), the group is no longer Alive.
func TestGroupSignallerAliveThenSignalTearsDown(t *testing.T) {
	proc := startHelper(t, "trap", stack.ComponentServer, nil)
	pgid := proc.Pid()

	gs := NewGroupSignaller()

	startTime, err := readGroupLeaderStartTime(pgid)
	if err != nil {
		t.Fatalf("readGroupLeaderStartTime(%d) = %v", pgid, err)
	}

	// 1. Identity-matched → alive.
	if !gs.Alive(pgid, startTime) {
		t.Fatalf("Alive(%d, %d) = false, want true for a live identity-matched group", pgid, startTime)
	}
	// 2. Wrong token (recycled pid) → not alive.
	if gs.Alive(pgid, startTime+1) {
		t.Fatalf("Alive(%d, %d) = true, want false for a mismatched start-time token", pgid, startTime+1)
	}

	// 3. Real SIGTERM to the group; the trap helper converts it to a clean exit.
	if err := gs.Signal(pgid, stack.SignalTerm); err != nil {
		t.Fatalf("Signal(SIGTERM) = %v", err)
	}
	// Gate on the actual exit event, not a sleep.
	if err := proc.Wait(context.Background()); err != nil {
		t.Fatalf("proc.Wait after SIGTERM = %v", err)
	}
	// The group leader has exited; a re-check with the original token is not alive.
	// (kill(-pgid,0) ESRCH, or the /proc read fails — either way not-alive.)
	if gs.Alive(pgid, startTime) {
		t.Fatalf("Alive(%d, %d) = true after the group exited, want false", pgid, startTime)
	}
}

// TestGroupSignallerAliveDeadGroup proves a pgid that names no process reports
// not-alive rather than erroring or signaling.
func TestGroupSignallerAliveDeadGroup(t *testing.T) {
	gs := NewGroupSignaller()
	dead := deadPGID(t)
	if gs.Alive(dead, 12345) {
		t.Fatalf("Alive(%d, ...) = true for a nonexistent group, want false", dead)
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
// (deliberately duplicated, groupsignal.go:93-97) cannot drift on the
// load-bearing identity token — a "simplify to strings.Fields(line)" regression
// here would be caught rather than only by the real-/proc integration test whose
// comm has no embedded spaces.
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
	gs := NewGroupSignaller()
	for pgid := 1 << 20; pgid < (1<<20)+100000; pgid++ {
		if !gs.Alive(pgid, 0) {
			// Alive is false either because ESRCH or because /proc read failed;
			// for a pgid with no process the kill(0) is ESRCH — good enough for a
			// "not a live group" pgid the signal tests want.
			return pgid
		}
	}
	t.Fatal("could not find a dead pgid")
	return 0
}
