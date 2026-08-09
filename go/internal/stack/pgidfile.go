//go:build unix

package stack

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// pgidFileName is the state-dir record of the child process groups a successful
// up spawned, beside stack.lock / stack.lock.guard. A fresh down (which holds no
// in-memory Process handle for a stack a prior up spawned) reads it to learn
// which groups to signal. It is removed on a fully successful teardown.
const pgidFileName = "stack.pgids"

// pgidFileVersion is the format/provenance version written in the record header.
// It is a format guard only — never a child-liveness signal (see pgidRecord).
const pgidFileVersion = "1"

// pgidEntry is one supervised child's teardown identity: its component, the
// process-group id (== the child's pid, set via Setpgid at spawn), and the
// group leader's start time as read at spawn. StartTime is the identity token —
// it turns the down-side check from "does a group with this pgid exist" (which a
// recycled pid passes falsely) into "does a group with this pgid AND this leader
// start time exist", closing the pid-recycling window.
type pgidEntry struct {
	Component Component
	Pgid      int
	StartTime uint64
}

// pgidRecord is the parsed pgid file: a provenance header plus the per-child
// entries in start order (postgres, compass-server, compass-runner).
//
// WriterPid and Version are format/provenance and version guard ONLY, NEVER a
// child-liveness signal: up always exits after a successful spawn
// (main.go:235-238), so the writer pid is dead in every linger teardown and
// discriminates nothing about whether the children are alive. Child liveness is
// decided per entry by pgid identity (Pgid + StartTime), not by the header.
type pgidRecord struct {
	WriterPid int
	Version   string
	Entries   []pgidEntry
}

// matches reports whether the record holds an entry whose recorded pgid and
// start-time identity token both equal the arguments. It is the read-side
// identity predicate: a recorded pgid whose leader start time no longer matches
// is a different (recycled) process and must never be signaled as if it were the
// original child.
func (r pgidRecord) matches(pgid int, startTime uint64) bool {
	for _, e := range r.Entries {
		if e.Pgid == pgid && e.StartTime == startTime {
			return true
		}
	}
	return false
}

// writePgidFile publishes rec to <stateDir>/stack.pgids atomically (temp +
// rename in the same dir), mode 0600, plain text with a trailing newline. The
// rename is the torn-write guard: a reader either sees the whole previous file
// or the whole new one, never a partial record — the same publish-complete
// discipline the lockfile uses (lockfile.go:111-117). Rewritten after each
// successful child spawn, so the on-disk file is always a complete earlier
// prefix of the start sequence.
//
// Format:
//
//	<version> <writerPid>
//	<component> <pgid> <starttime>
//	...          (one line per entry, in start order)
func writePgidFile(stateDir string, rec pgidRecord) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %d\n", rec.Version, rec.WriterPid)
	for _, e := range rec.Entries {
		fmt.Fprintf(&b, "%s %d %d\n", e.Component, e.Pgid, e.StartTime)
	}

	path := filepath.Join(stateDir, pgidFileName)
	tmp, err := os.CreateTemp(stateDir, pgidFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create pgid temp in %q: %w", stateDir, err)
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()        // best-effort cleanup; the chmod error is the actionable failure
		_ = os.Remove(tmpName) // best-effort cleanup of the abandoned temp
		return fmt.Errorf("chmod pgid temp %q: %w", tmpName, err)
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		_ = tmp.Close()        // best-effort cleanup; the write error is the actionable failure
		_ = os.Remove(tmpName) // best-effort cleanup of the abandoned temp
		return fmt.Errorf("write pgid temp %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName) // best-effort cleanup of the abandoned temp
		return fmt.Errorf("close pgid temp %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName) // best-effort cleanup: the rename failed, so the temp still exists
		return fmt.Errorf("publish pgid file %q: %w", path, err)
	}
	return nil
}

// readPgidFile parses <stateDir>/stack.pgids. A missing file is reported as
// os.ErrNotExist (wrapped) so the caller can distinguish "no teardown record"
// from a genuine read/parse failure. The rename in writePgidFile guarantees no
// torn record is ever observed, but parsing is defensive regardless: a malformed
// header or entry line is a hard error rather than a silent partial parse, since
// signaling off a half-understood record is exactly the blast radius the design
// forbids.
func readPgidFile(stateDir string) (pgidRecord, error) {
	path := filepath.Join(stateDir, pgidFileName)
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the stack-owned pgid file in the state dir, not user input
	if err != nil {
		return pgidRecord{}, err
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return pgidRecord{}, fmt.Errorf("pgid file %q: empty or missing header", path)
	}

	header := strings.Fields(lines[0])
	if len(header) != 2 {
		return pgidRecord{}, fmt.Errorf("pgid file %q: malformed header %q", path, lines[0])
	}
	writerPid, err := strconv.Atoi(header[1])
	if err != nil {
		return pgidRecord{}, fmt.Errorf("pgid file %q: unparseable writer pid %q: %w", path, header[1], err)
	}
	rec := pgidRecord{WriterPid: writerPid, Version: header[0]}

	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		entry, err := parsePgidLine(line)
		if err != nil {
			return pgidRecord{}, fmt.Errorf("pgid file %q: %w", path, err)
		}
		rec.Entries = append(rec.Entries, entry)
	}
	return rec, nil
}

// parsePgidLine parses one "<component> <pgid> <starttime>" entry line. An
// unknown component name or an unparseable number is a hard error — the record
// must be understood exactly or not signaled off at all.
func parsePgidLine(line string) (pgidEntry, error) {
	f := strings.Fields(line)
	if len(f) != 3 {
		return pgidEntry{}, fmt.Errorf("malformed entry line %q", line)
	}
	comp, ok := componentFromString(f[0])
	if !ok {
		return pgidEntry{}, fmt.Errorf("unknown component %q in entry line %q", f[0], line)
	}
	pgid, err := strconv.Atoi(f[1])
	if err != nil {
		return pgidEntry{}, fmt.Errorf("unparseable pgid %q in entry line %q: %w", f[1], line, err)
	}
	startTime, err := strconv.ParseUint(f[2], 10, 64)
	if err != nil {
		return pgidEntry{}, fmt.Errorf("unparseable start time %q in entry line %q: %w", f[2], line, err)
	}
	return pgidEntry{Component: comp, Pgid: pgid, StartTime: startTime}, nil
}

// componentFromString is the inverse of Component.String for the three
// supervised children. It is defined here beside the parser (its only consumer)
// rather than on Component, keeping the pgid file format self-contained.
func componentFromString(s string) (Component, bool) {
	switch s {
	case ComponentPostgres.String():
		return ComponentPostgres, true
	case ComponentServer.String():
		return ComponentServer, true
	case ComponentRunner.String():
		return ComponentRunner, true
	default:
		return 0, false
	}
}

// removePgidFile deletes the pgid file. It is idempotent — an absent file is not
// an error — so the fully-successful teardown path and a retried down can both
// call it unconditionally.
func removePgidFile(stateDir string) error {
	path := filepath.Join(stateDir, pgidFileName)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove pgid file %q: %w", path, err)
	}
	return nil
}

// readStartTime is the package-internal seam that reads a process's start time
// (the identity token). It is a var, not a func, so tests can stub it and the
// package is not hard-Linux in principle — the real implementation reads
// /proc/<pid>/stat, which only exists on Linux, but the seam keeps that a
// swappable detail rather than a compile-time dependency.
var readStartTime = readStartTimeProc

// readStartTimeProc reads field 22 (starttime, in clock ticks since boot) of
// /proc/<pid>/stat.
//
// The parse gotcha: field 2 (comm) is the executable name wrapped in
// parentheses and MAY itself contain spaces AND parentheses (e.g. a process
// named "(ec) foo"), so splitting the whole line on whitespace miscounts. The
// robust parse the kernel documents (proc(5)) is to find the LAST ')' — comm is
// the only parenthesized field and everything after it is space-separated
// fixed-position fields — then count fields from there. After the last ')':
// field[0] is state (field 3), so starttime (field 22) is field[22-3] = index
// 19 of the post-comm split.
func readStartTimeProc(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, fmt.Errorf("read /proc/%d/stat: %w", pid, err)
	}
	startTime, err := parseStatStartTime(string(data))
	if err != nil {
		return 0, fmt.Errorf("/proc/%d/stat: %w", pid, err)
	}
	return startTime, nil
}

// parseStatStartTime extracts field 22 (starttime) from a /proc/<pid>/stat line.
// Split out from readStartTimeProc so the parenthesized-comm parse is unit-tested
// against synthesized lines without a live process.
func parseStatStartTime(line string) (uint64, error) {
	rparen := strings.LastIndexByte(line, ')')
	if rparen < 0 {
		return 0, fmt.Errorf("no comm terminator ')' in %q", line)
	}
	// Fields after comm, space-separated. field[0] == field 3 (state).
	rest := strings.Fields(line[rparen+1:])
	const startTimeIndexAfterComm = 22 - 3 // field 22 (starttime); field 3 is the first post-comm field
	if len(rest) <= startTimeIndexAfterComm {
		return 0, fmt.Errorf("only %d fields after comm, need field 22", len(rest))
	}
	startTime, err := strconv.ParseUint(rest[startTimeIndexAfterComm], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unparseable starttime %q: %w", rest[startTimeIndexAfterComm], err)
	}
	return startTime, nil
}
