//go:build linux

package adapters

import (
	"os"
	"strconv"
	"syscall"
	"testing"

	"github.com/RigelBuild/compass/go/internal/stack"
)

// TestGroupSignallerLivenessForeignUIDIsRecycled pins that another uid's group
// is never ours: kill(0) EPERM must not fall through to an orphaned verdict.
func TestGroupSignallerLivenessForeignUIDIsRecycled(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can signal every group, so no EPERM is observable")
	}
	pgid := foreignGroupLeader(t)
	// The real token would otherwise read as owned, so only EPERM can say recycled.
	startTime, err := readGroupLeaderStartTime(pgid)
	if err != nil {
		t.Skipf("leader %d start time unreadable: %v", pgid, err)
	}
	if got := NewGroupSignaller().Liveness(pgid, startTime); got != stack.GroupRecycled {
		t.Fatalf("Liveness(%d) for another uid's group = %v, want GroupRecycled", pgid, got)
	}
}

// foreignGroupLeader returns a live pgid > 1 that kill(0) reports as EPERM.
func foreignGroupLeader(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc: %v", err)
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid && syscall.Kill(-pid, 0) == syscall.EPERM {
			return pid
		}
	}
	t.Skip("no other uid's process group leader is visible on this host")
	return 0
}
