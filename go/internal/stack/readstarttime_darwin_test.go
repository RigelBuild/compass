//go:build darwin

package stack

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// TestPackStartTimevalMatchesDownSide is the spawn half of the mirror-test pair
// that pins the darwin identity encoding. The down side
// (adapters.TestPackGroupLeaderTimevalMatchesSpawnSide) feeds the SAME synthetic
// timeval through its own packing and asserts this same literal.
//
// It exists because the two packings are deliberately duplicated across a
// package boundary the packages cannot cross, and GroupSignaller.Alive compares
// their outputs for uint64 equality — a drift would report every live child as
// not-alive and silently skip it at teardown. Both tests must be updated
// together or one reds, which is the point.
func TestPackStartTimevalMatchesDownSide(t *testing.T) {
	tv := unix.Timeval{Sec: 1_700_000_123, Usec: 456_789}
	const want = uint64(1_700_000_123)*1_000_000 + 456_789
	if got := packStartTimeval(tv); got != want {
		t.Fatalf("packStartTimeval(%d.%06d) = %d, want %d", tv.Sec, tv.Usec, got, want)
	}
}

// TestReadProcessStartTimeSelfIsStable drives the real darwin sysctl reader
// against a live process (this one): the token must be non-zero and identical
// across two reads. A start time that moved between reads, or came back zero,
// would break the identity gate — Alive would stop matching a group it spawned
// moments earlier and skip it at teardown.
func TestReadProcessStartTimeSelfIsStable(t *testing.T) {
	pid := os.Getpid()
	first, err := readProcessStartTime(pid)
	if err != nil {
		t.Fatalf("readProcessStartTime(%d) = %v", pid, err)
	}
	if first == 0 {
		t.Fatalf("readProcessStartTime(%d) = 0, want a non-zero identity token", pid)
	}
	second, err := readProcessStartTime(pid)
	if err != nil {
		t.Fatalf("readProcessStartTime(%d) second read = %v", pid, err)
	}
	if first != second {
		t.Fatalf("start time not stable across reads: %d then %d", first, second)
	}
}

// TestReadProcessStartTimeDeadPidErrors proves the reader fails closed for a pid
// that names no process. It must NOT return a zero token: the token feeds an
// equality check, so a silent 0 would match any record carrying 0 and signal a
// group that is not ours. The dead pid comes from the package's existing
// deadPID scan (lockfile_test.go), not a guessed constant a busy host could
// have live.
func TestReadProcessStartTimeDeadPidErrors(t *testing.T) {
	dead := deadPID(t)
	if got, err := readProcessStartTime(dead); err == nil {
		t.Fatalf("readProcessStartTime(%d) = %d, nil for a dead pid; want an error", dead, got)
	}
}
