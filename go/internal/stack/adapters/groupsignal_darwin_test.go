//go:build darwin

package adapters

import (
	"testing"

	"golang.org/x/sys/unix"
)

// TestPackGroupLeaderTimevalMatchesSpawnSide is the down half of the mirror-test
// pair that pins the darwin identity encoding, exactly as
// TestParseGroupLeaderStatParsesParenthesizedComm mirrors the Linux parser. It
// feeds the SAME synthetic timeval the spawn-side test
// (stack.TestPackStartTimevalMatchesDownSide) uses and asserts the same literal
// uint64.
//
// The duplication it guards is load-bearing: Alive compares this packing's
// output against a token stack.packStartTimeval produced at spawn, for uint64
// equality, so a one-sided change to either expression would report every live
// child as not-alive and silently skip it at teardown — the worst teardown
// failure available, because it is silent.
func TestPackGroupLeaderTimevalMatchesSpawnSide(t *testing.T) {
	tv := unix.Timeval{Sec: 1_700_000_123, Usec: 456_789}
	const want = uint64(1_700_000_123)*1_000_000 + 456_789
	if got := packGroupLeaderTimeval(tv); got != want {
		t.Fatalf("packGroupLeaderTimeval(%d.%06d) = %d, want %d", tv.Sec, tv.Usec, got, want)
	}
}

// TestReadGroupLeaderStartTimeSelfIsStable drives the real darwin sysctl reader
// against a live process (this one, its own group leader candidate): the token
// must be non-zero and identical across two reads, or the identity gate would
// stop matching a group moments after it was recorded.
func TestReadGroupLeaderStartTimeSelfIsStable(t *testing.T) {
	pid := unix.Getpid()
	first, err := readGroupLeaderStartTime(pid)
	if err != nil {
		t.Fatalf("readGroupLeaderStartTime(%d) = %v", pid, err)
	}
	if first == 0 {
		t.Fatalf("readGroupLeaderStartTime(%d) = 0, want a non-zero identity token", pid)
	}
	second, err := readGroupLeaderStartTime(pid)
	if err != nil {
		t.Fatalf("readGroupLeaderStartTime(%d) second read = %v", pid, err)
	}
	if first != second {
		t.Fatalf("start time not stable across reads: %d then %d", first, second)
	}
}

// TestReadGroupLeaderStartTimeDeadPGIDErrors proves the reader fails closed for
// a pgid that names no process, so Alive reports not-alive rather than matching
// on a zero token.
func TestReadGroupLeaderStartTimeDeadPGIDErrors(t *testing.T) {
	dead := deadPGID(t)
	if got, err := readGroupLeaderStartTime(dead); err == nil {
		t.Fatalf("readGroupLeaderStartTime(%d) = %d, nil for a dead pgid; want an error", dead, got)
	}
}
