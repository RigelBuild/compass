//go:build unix

package adapters

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"syscall"

	"github.com/RigelBuild/compass/go/internal/stack"
)

// GroupSignaller is the real stack.GroupSignaller: it signals and classifies a
// persisted child process group by pgid for the cross-process teardown. It
// targets the whole group (negative pgid), the same primitive the in-process
// escalation uses (process.go: syscall.Kill(-pid, SIGKILL)).
//
// It is the only teardown seam that touches groups this process did not spawn,
// so every operation is scoped to a caller-supplied pgid read from the stack's
// own state-dir record — never a scan, never a pattern.
type GroupSignaller struct{}

// Compile-time proof the adapter satisfies the core seam.
var _ stack.GroupSignaller = (*GroupSignaller)(nil)

// NewGroupSignaller builds a GroupSignaller.
func NewGroupSignaller() *GroupSignaller {
	return &GroupSignaller{}
}

// Signal delivers sig to the whole process group named by pgid (negative pgid
// per kill(2)'s group-signal convention). Only SignalTerm and SignalKill are
// valid dispositions; any other is an error rather than a silent no-op.
func (g *GroupSignaller) Signal(pgid int, sig stack.ProcessSignal) error {
	// Defense in depth: the parser already rejects pgid <= 1, but this is the one
	// sink that reaches syscall.Kill(-pgid, ...), where -1 is the "every process
	// the caller may signal" wildcard and 0 is the caller's own group. Never let
	// a degenerate value reach it, whatever the caller passed.
	if pgid <= 1 {
		return fmt.Errorf("refusing to signal degenerate pgid %d", pgid)
	}
	var sysSig syscall.Signal
	switch sig {
	case stack.SignalTerm:
		sysSig = syscall.SIGTERM
	case stack.SignalKill:
		sysSig = syscall.SIGKILL
	default:
		return fmt.Errorf("unknown process signal %d", int(sig))
	}
	if err := syscall.Kill(-pgid, sysSig); err != nil {
		return fmt.Errorf("signal %v to group %d: %w", sysSig, pgid, err)
	}
	return nil
}

// Liveness classifies a group as gone, owned, orphaned, or recycled. Only ESRCH
// means gone: any other kill(0) error falls through to the leader read, so a
// probe failure can never report a live group torn down.
func (g *GroupSignaller) Liveness(pgid int, startTime uint64) stack.GroupLiveness {
	// kill(-1, 0) and kill(0, 0) probe far beyond one child group.
	if pgid <= 1 {
		return stack.GroupGone
	}
	switch err := syscall.Kill(-pgid, 0); {
	case errors.Is(err, syscall.ESRCH):
		return stack.GroupGone
	case errors.Is(err, syscall.EPERM):
		// Another uid's group: our children share our uid, so this pgid was reused.
		return stack.GroupRecycled
	}
	got, err := readGroupLeaderStartTime(pgid)
	if err != nil {
		// Members outlive a reaped leader, and Linux never reuses a live pgid.
		return stack.GroupOrphaned
	}
	if got != startTime {
		return stack.GroupRecycled
	}
	return stack.GroupOwned
}

// parseGroupLeaderStat extracts field 22 (starttime) from a /proc/<pid>/stat
// line. Split out from the Linux reader (groupsignal_linux.go) so the
// parenthesized-comm parse is unit-tested against synthesized lines without a
// live process — the same split (and the same gotcha) as
// stack.parseStatStartTime. It stays in the unix-built file so that test
// compiles on every unix: the parse rule is pure text handling.
func parseGroupLeaderStat(line string) (uint64, error) {
	// comm (field 2) is parenthesized and may contain spaces AND parens, so
	// count fields from the LAST ')'; field[0] after it is state (field 3), so
	// starttime (field 22) is index 22-3.
	rparen := strings.LastIndexByte(line, ')')
	if rparen < 0 {
		return 0, errors.New("no comm terminator ')'")
	}
	rest := strings.Fields(line[rparen+1:])
	const startTimeIndexAfterComm = 22 - 3
	if len(rest) <= startTimeIndexAfterComm {
		return 0, fmt.Errorf("only %d fields after comm, need field 22", len(rest))
	}
	startTime, err := strconv.ParseUint(rest[startTimeIndexAfterComm], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unparseable starttime %q: %w", rest[startTimeIndexAfterComm], err)
	}
	return startTime, nil
}
