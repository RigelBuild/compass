//go:build linux && gtk4

package main

import (
	"os"
	"strconv"
	"strings"
)

// liveChildren returns every process still descended from this one, discovered
// by walking /proc for entries whose ppid chains back to os.Getpid(). WebKit's
// GPU/network helpers are our direct children; scanning the whole subtree also
// catches any helper-of-a-helper before it reparents to init.
func liveChildren() []procInfo {
	self := os.Getpid()

	entries, err := os.ReadDir("/proc")
	if err != nil {
		// Without procfs there is nothing to reap and no way to prove otherwise;
		// treat as clean so the gate never wedges on an unreadable /proc.
		return nil
	}

	ppids := make(map[int]int, len(entries))
	comms := make(map[int]string, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a pid directory
		}
		ppid, comm, ok := readStat(pid)
		if !ok {
			continue // vanished between readdir and read; not our concern
		}
		ppids[pid] = ppid
		comms[pid] = comm
	}

	var out []procInfo
	for pid := range ppids {
		if pid != self && descendsFrom(pid, self, ppids) {
			out = append(out, procInfo{pid: pid, comm: comms[pid]})
		}
	}
	return out
}

// readStat parses ppid and comm out of /proc/<pid>/stat. comm can hold spaces
// and parens, so key off the final ')': the two space-separated fields after it
// are state then ppid.
func readStat(pid int) (ppid int, comm string, ok bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, "", false
	}
	s := string(raw)
	open := strings.IndexByte(s, '(')
	shut := strings.LastIndexByte(s, ')')
	if open < 0 || shut < 0 || shut < open {
		return 0, "", false
	}
	comm = s[open+1 : shut]
	fields := strings.Fields(s[shut+1:])
	if len(fields) < 2 {
		return 0, "", false
	}
	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, "", false
	}
	return ppid, comm, true
}

// descendsFrom reports whether pid chains up to root through the ppid map. The
// walk is bounded by the map size so pid reuse mid-scan cannot loop forever.
func descendsFrom(pid, root int, ppids map[int]int) bool {
	for hops := 0; hops <= len(ppids); hops++ {
		ppid, seen := ppids[pid]
		if !seen {
			return false
		}
		if ppid == root {
			return true
		}
		if ppid <= 1 {
			return false // reached init/kernel without hitting root
		}
		pid = ppid
	}
	return false
}
