//go:build unix

package server

import (
	"os"
	"testing"
)

func TestSuppliedAdminTokenModeAllowed(t *testing.T) {
	const heldGID = 2000
	const unheldGID = 3000
	const effectiveGID = 1000

	groups := []int{heldGID}
	cases := []struct {
		name string
		mode os.FileMode
		gid  uint32
		want bool
	}{
		{name: "owner private", mode: 0o600, gid: effectiveGID, want: true},
		{name: "group readable with effective gid", mode: 0o640, gid: effectiveGID, want: true},
		{name: "group readable with held gid", mode: 0o640, gid: heldGID, want: true},
		{name: "group readable with held supplementary gid", mode: 0o440, gid: heldGID, want: true},
		{name: "group readable with unheld gid", mode: 0o640, gid: unheldGID},
		{name: "group writable", mode: 0o660, gid: heldGID},
		{name: "other readable", mode: 0o604, gid: effectiveGID},
		{name: "directory", mode: os.ModeDir | 0o600, gid: effectiveGID},
		{name: "fifo", mode: os.ModeNamedPipe | 0o600, gid: effectiveGID},
		{name: "group executable", mode: 0o610, gid: heldGID},
		{name: "setuid", mode: 0o600 | os.ModeSetuid, gid: effectiveGID},
		{name: "sticky", mode: 0o600 | os.ModeSticky, gid: effectiveGID},
		{name: "setgid", mode: 0o600 | os.ModeSetgid, gid: effectiveGID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := suppliedAdminTokenModeAllowed(tc.mode, tc.gid, effectiveGID, groups); got != tc.want {
				t.Fatalf("suppliedAdminTokenModeAllowed(%#o, gid=%d) = %v, want %v", tc.mode, tc.gid, got, tc.want)
			}
		})
	}
}
