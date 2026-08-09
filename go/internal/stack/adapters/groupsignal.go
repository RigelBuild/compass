//go:build unix

package adapters

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/sealedsecurity/compass/go/internal/stack"
)

// GroupSignaller is the real stack.GroupSignaller: it signals and
// identity-checks a persisted child process group by pgid for the cross-process
// teardown. It targets the whole group (negative pgid), the same primitive the
// in-process escalation uses (process.go: syscall.Kill(-pid, SIGKILL)).
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

// Alive reports whether the process group named by pgid exists AND its leader's
// current start time equals startTime — the identity gate. A group that no
// longer exists (kill(-pgid, 0) == ESRCH) or whose leader's start time no longer
// matches (a recycled pid) is reported not-alive, so the caller never signals a
// gone-or-recycled group as if it were the original child.
//
// The two checks are ordered existence-then-identity: the kill(0) probe cheaply
// rules out the ESRCH case, then the /proc start-time read confirms the leader
// is the same process. A start-time read failure (the leader vanished between
// the two syscalls, or /proc is unavailable) is treated as not-alive — the safe
// verdict is never to signal.
func (g *GroupSignaller) Alive(pgid int, startTime uint64) bool {
	// Existence: signal 0 to the group. ESRCH means gone; EPERM means it exists
	// but is not ours (still "exists"); nil means exists.
	if err := syscall.Kill(-pgid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	// Identity: the group leader's pid is the pgid; its start time must match the
	// recorded token, closing the pid-recycling window.
	got, err := readGroupLeaderStartTime(pgid)
	if err != nil {
		return false
	}
	return got == startTime
}

// readGroupLeaderStartTime reads field 22 (starttime) of /proc/<pgid>/stat — the
// group leader, since pid == pgid for a Setpgid child. It duplicates the core's
// parser (rather than exporting it across the package boundary) because the
// parenthesized-comm gotcha is the same on both sides and the two are read-only
// leaf helpers; see stack.parseStatStartTime for the full explanation.
func readGroupLeaderStartTime(pgid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pgid))
	if err != nil {
		return 0, fmt.Errorf("read /proc/%d/stat: %w", pgid, err)
	}
	line := string(data)
	// comm (field 2) is parenthesized and may contain spaces AND parens, so
	// count fields from the LAST ')'; field[0] after it is state (field 3), so
	// starttime (field 22) is index 22-3.
	rparen := strings.LastIndexByte(line, ')')
	if rparen < 0 {
		return 0, fmt.Errorf("/proc/%d/stat: no comm terminator ')'", pgid)
	}
	rest := strings.Fields(line[rparen+1:])
	const startTimeIndexAfterComm = 22 - 3
	if len(rest) <= startTimeIndexAfterComm {
		return 0, fmt.Errorf("/proc/%d/stat: only %d fields after comm, need field 22", pgid, len(rest))
	}
	startTime, err := strconv.ParseUint(rest[startTimeIndexAfterComm], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("/proc/%d/stat: unparseable starttime %q: %w", pgid, rest[startTimeIndexAfterComm], err)
	}
	return startTime, nil
}
