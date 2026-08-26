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
//
// v2 (this build) grows the entry line into a kind-tagged discriminated union
// (proc / ctr, see pgidEntry): the container-backed postgres of S4 has no
// process-group teardown identity, so it is recorded and torn down by container
// name instead. v2 is a strict superset — a v1 record (untagged 3-field proc
// lines) still parses, as all-process entries. A shipped v1 binary reading a v2
// record never half-parses it: not by a header check (v1 never compares the
// header version) but by the entry grammar — a v2 `proc` line is 4 fields where
// v1 demands exactly 3, and a `ctr` line's leading token is not a known
// component — so v1's parser hard-errors under the same
// signal-off-a-half-understood-record discipline. The forward guard (this
// reader refusing an unknown/newer version) protects a v2 reader from a future
// v3; it never gates v1↔v2.
const pgidFileVersion = "2"

// pgidFileVersionV1 is the prior format this build still reads for back-compat:
// untagged 3-field proc lines (no kind tag), parsed as all-process entries.
const pgidFileVersionV1 = "1"

// pgidEntryKind tags an entry line's grammar in the v2 discriminated union.
type pgidEntryKind int

const (
	// entryProc is a process-group child, torn down by group signal
	// (identity-checked pgid + leader start time). It is the zero value, so an
	// unqualified pgidEntry literal is a process entry — the v1 shape unchanged.
	entryProc pgidEntryKind = iota
	// entryContainer is a container child (S4's containerized postgres), torn
	// down by name via podman (stop, then rm -f). A container has no process
	// group of its own under rootless podman (it runs beneath conmon), so its
	// teardown identity is its stable per-state-dir name, not a pgid.
	entryContainer
)

// entryKindTag renders a kind as its on-disk line tag.
func (k pgidEntryKind) entryKindTag() string {
	switch k {
	case entryContainer:
		return "ctr"
	default:
		return "proc"
	}
}

// pgidEntry is one supervised child's teardown identity, a kind-tagged
// discriminated union (Kind):
//
//   - entryProc (the v1 shape, unchanged): a process-group child. Component +
//     the process-group id (== the child's pid, set via Setpgid at spawn) +
//     the group leader's start time as read at spawn. StartTime is the identity
//     token — it turns the down-side check from "does a group with this pgid
//     exist" (which a recycled pid passes falsely) into "does a group with this
//     pgid AND this leader start time exist", closing the pid-recycling window.
//     ContainerName is empty.
//   - entryContainer: a container child (S4's containerized postgres).
//     Component + ContainerName (the stable per-state-dir podman name, the
//     authoritative teardown identity). Pgid/StartTime are unused (zero): a
//     rootless container runs beneath conmon, outside the client's process
//     group, so it has no group to signal — it is torn down by name.
type pgidEntry struct {
	Kind          pgidEntryKind
	Component     Component
	Pgid          int
	StartTime     uint64
	ContainerName string
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

// writePgidFile publishes rec to <stateDir>/stack.pgids atomically (temp +
// rename in the same dir), mode 0600, plain text with a trailing newline. The
// rename is the torn-write guard: a reader either sees the whole previous file
// or the whole new one, never a partial record — the same publish-complete
// discipline the lockfile uses (lockfile.go:111-117). Rewritten after each
// successful child spawn, so the on-disk file is always a complete earlier
// prefix of the start sequence.
//
// Format (v2):
//
//	<version> <writerPid>
//	proc <component> <pgid> <starttime>   (a process-group child)
//	ctr <component> <name>                (a container child)
//	...          (one line per entry, in start order)
//
// The leading kind tag makes the entry line a discriminated union; a v1 record
// (untagged 3-field proc lines) is read-only back-compat — this build never
// writes it.
func writePgidFile(stateDir string, rec pgidRecord) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %d\n", pgidFileVersion, rec.WriterPid)
	for _, e := range rec.Entries {
		switch e.Kind {
		case entryContainer:
			fmt.Fprintf(&b, "%s %s %s\n", e.Kind.entryKindTag(), e.Component, e.ContainerName)
		default:
			fmt.Fprintf(&b, "%s %s %d %d\n", e.Kind.entryKindTag(), e.Component, e.Pgid, e.StartTime)
		}
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
//
// Version dispatch (the S4/DL-262 cross-version rule):
//   - "2" — this build's format: kind-tagged entry lines (proc / ctr).
//   - "1" — back-compat: untagged 3-field proc lines, read as all-process
//     entries (v2 is a strict superset of v1).
//   - anything else — refused legibly. This is the FORWARD guard, protecting a
//     v2 reader from a future v3 it cannot understand, the same
//     never-signal-off-a-half-understood-record discipline. (The reverse
//     direction — a v1 binary meeting a v2 record — is guarded by the entry
//     grammar in v1's own parser, not here.)
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
	version := header[0]
	if version != pgidFileVersion && version != pgidFileVersionV1 {
		return pgidRecord{}, fmt.Errorf("pgid file %q: unsupported record version %q (this build reads %q and %q); stop the stack with the build that started it", path, version, pgidFileVersionV1, pgidFileVersion)
	}
	writerPid, err := strconv.Atoi(header[1])
	if err != nil {
		return pgidRecord{}, fmt.Errorf("pgid file %q: unparseable writer pid %q: %w", path, header[1], err)
	}
	rec := pgidRecord{WriterPid: writerPid, Version: version}

	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		entry, err := parsePgidLine(version, line)
		if err != nil {
			return pgidRecord{}, fmt.Errorf("pgid file %q: %w", path, err)
		}
		rec.Entries = append(rec.Entries, entry)
	}
	return rec, nil
}

// parsePgidLine parses one entry line, dispatched on the record version.
//
//   - v1 ("1"): an untagged "<component> <pgid> <starttime>" line, parsed as a
//     process entry — the format DL-183 froze, kept for back-compat.
//   - v2 ("2"): a kind-tagged line, "proc <component> <pgid> <starttime>" or
//     "ctr <component> <name>", a discriminated union.
//
// An unknown component name, an unparseable number, or an unknown/mismatched
// kind tag is a hard error — the record must be understood exactly or not
// signaled off at all. This is also the guard a shipped v1 binary relies on when
// it meets a v2 record: its v1 parser sees a 4-field "proc …" line (where it
// demands exactly 3) or a "ctr" leading token that is not a known component, and
// hard-errors — refusing to half-parse, by the entry grammar, not a header
// check.
func parsePgidLine(version, line string) (pgidEntry, error) {
	if version == pgidFileVersionV1 {
		// v1: the whole line is an untagged proc body.
		return parseProcEntry(line, strings.Fields(line))
	}

	// v2: the leading token is the kind tag.
	f := strings.Fields(line)
	if len(f) == 0 {
		return pgidEntry{}, fmt.Errorf("malformed entry line %q", line)
	}
	switch f[0] {
	case entryProc.entryKindTag():
		return parseProcEntry(line, f[1:])
	case entryContainer.entryKindTag():
		return parseContainerEntry(line, f[1:])
	default:
		return pgidEntry{}, fmt.Errorf("unknown entry kind %q in entry line %q", f[0], line)
	}
}

// parseProcEntry parses a process entry's "<component> <pgid> <starttime>" body
// (the fields after the kind tag, or the whole v1 line). It is the identity-and-
// safety-checked path: an unknown component, an unparseable or degenerate pgid,
// or an unparseable start time is a hard error.
func parseProcEntry(line string, f []string) (pgidEntry, error) {
	if len(f) != 3 {
		return pgidEntry{}, fmt.Errorf("malformed proc entry line %q", line)
	}
	comp, ok := componentFromString(f[0])
	if !ok {
		return pgidEntry{}, fmt.Errorf("unknown component %q in entry line %q", f[0], line)
	}
	pgid, err := strconv.Atoi(f[1])
	if err != nil {
		return pgidEntry{}, fmt.Errorf("unparseable pgid %q in entry line %q: %w", f[1], line, err)
	}
	// A real process-group leader pid is always > 1; pid 1 is init and is never a
	// compass child. Refuse a degenerate pgid at parse time so a corrupt or
	// tampered record is rejected wholesale rather than reaching the signal sink,
	// where kill(-1, ...) is the "every process the caller may signal" wildcard
	// and kill(0, ...) targets the down process's own group — exactly the
	// pattern-kill blast radius the design forbids.
	if pgid <= 1 {
		return pgidEntry{}, fmt.Errorf("invalid pgid %d in entry line %q: a process-group leader pid is always > 1", pgid, line)
	}
	startTime, err := strconv.ParseUint(f[2], 10, 64)
	if err != nil {
		return pgidEntry{}, fmt.Errorf("unparseable start time %q in entry line %q: %w", f[2], line, err)
	}
	return pgidEntry{Kind: entryProc, Component: comp, Pgid: pgid, StartTime: startTime}, nil
}

// parseContainerEntry parses a container entry's "<component> <name>" body (the
// fields after the ctr kind tag). An unknown component or a missing/empty name
// is a hard error — the name is the container's whole teardown identity, so an
// unusable one must not reach the podman sink.
func parseContainerEntry(line string, f []string) (pgidEntry, error) {
	if len(f) != 2 {
		return pgidEntry{}, fmt.Errorf("malformed ctr entry line %q", line)
	}
	comp, ok := componentFromString(f[0])
	if !ok {
		return pgidEntry{}, fmt.Errorf("unknown component %q in entry line %q", f[0], line)
	}
	name := f[1]
	if name == "" {
		return pgidEntry{}, fmt.Errorf("empty container name in entry line %q", line)
	}
	return pgidEntry{Kind: entryContainer, Component: comp, ContainerName: name}, nil
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
// (the identity token). It is a var, not a func, so tests can stub it without a
// live process. The wired implementation reads /proc/<pid>/stat, which exists
// only on Linux — and the embedded stack is Linux/podman-only at runtime anyway
// (the runner loop, compass-native-app design.md:247,346-348), so the seam is a
// test seam, not a cross-OS portability claim: on a non-Linux unix this reader
// fails and up refuses, which is the correct outcome on an unsupported host.
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
