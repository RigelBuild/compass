//go:build unix

// pidfile.go is the host-written per-session process record (V7 §(a)): the
// three pidfiles under a session's runtime dir that make the dir the durable
// record of the session's process set, so an orphan left by a Runner crash can
// be identified and killed WITHOUT the risk of killing an innocent process that
// inherited a recycled pid.
//
// The record is (pid, starttime, bootid), not a bare pid, and it is written in
// TWO atomic steps around each spawn. Both choices are load-bearing and each is
// argued at its own declaration below: writePidIntent for the pre-spawn step,
// pidRecord.alive for the identity comparison.

package microvm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// bootIDPath is the kernel's per-boot UUID. It changes on every boot and is
// stable for the boot's life, which is exactly the property the cross-reboot
// arm of the identity check needs.
const bootIDPath = "/proc/sys/kernel/random/boot_id"

// pidIntentToken is the literal standing in place of a pid in the pre-spawn
// INTENT record. It cannot collide with a settled record: a settled record's
// first field always parses as a positive integer.
const pidIntentToken = "intent"

// procStartTimeField is the 1-based field number of starttime in
// /proc/<pid>/stat (proc(5)): the process start time in clock ticks since boot.
const procStartTimeField = 22

// errPidUnknown is returned by pidRecord.alive for a SAME-BOOT intent record: a
// child that was about to be spawned when the writer died, so it may or may not
// be running and there is NO pid with which to find out. It is deliberately
// neither "alive" nor "dead" — the reaper (V7 §(b)) must route it to its
// possibly-live arm (warn, keep the dir) rather than to a kill or a removal,
// and a bool return cannot express that third verdict.
var errPidUnknown = errors.New("microvm: pidfile records a pre-spawn intent from this boot: liveness unknowable")

// procBootID reads the host boot id ONCE per process and caches it: it cannot
// change while this process lives (a new boot id means a new kernel, which
// means this process is gone), so re-reading it per liveness check would be a
// syscall per pidfile for a value that is constant by construction.
var procBootID = sync.OnceValues(func() (string, error) {
	raw, err := os.ReadFile(bootIDPath)
	if err != nil {
		return "", fmt.Errorf("reading boot id from %s: %w", bootIDPath, err)
	}
	id := strings.TrimSpace(string(raw))
	if id == "" {
		return "", fmt.Errorf("empty boot id in %s", bootIDPath)
	}
	// The boot id is one field of a space-separated record, so whitespace in it
	// would make a written record unparseable. A kernel UUID never contains
	// any; fail loudly rather than emit a file that cannot be read back.
	if strings.ContainsAny(id, " \t") {
		return "", fmt.Errorf("boot id %q from %s contains whitespace", id, bootIDPath)
	}
	return id, nil
})

// pidRecord is one parsed pidfile: either the pre-spawn intent record (Intent
// true, PID/StartTime zero) or the settled record naming a spawned child.
type pidRecord struct {
	Intent    bool
	PID       int
	StartTime uint64
	BootID    string
}

// writePidIntent writes the pre-spawn INTENT record `intent <bootid>` to path
// at 0600. It is called BEFORE the child's startChild so the runtime dir names
// every child that MAY become live before it can be live (§(a)).
//
// Atomicity alone does not close the spawn→write window: the pid does not exist
// until the spawn returns, so a writer dying between a successful spawn and the
// settled write would leave a dir naming only the children it already recorded
// and NO record at all of the live child it just started — whereupon the reaper
// reads "everything recorded is dead", removes the dir, and erases the only
// evidence of the leak. The intent record makes the on-disk set CONSERVATIVE by
// construction: it may over-name a child that never spawned (which the reaper
// tolerates — there is no pid to signal), but it can never under-name a live
// one, which is the failure the reaper cannot recover from.
func writePidIntent(path string) error {
	bootID, err := procBootID()
	if err != nil {
		return err
	}
	return writePidRecordLine(path, pidIntentToken+" "+bootID+"\n")
}

// writePidfile writes the settled record `<pid> <starttime> <bootid>` to path
// at 0600, superseding any intent record in ONE atomic rename — so a concurrent
// reader sees the intent record or the settled record, never a torn prefix and
// never nothing. It is called after startChild succeeds, with the pid the spawn
// returned.
//
// starttime is read here, from the live process, rather than trusted from
// anywhere else: it is the second half of the identity pair pidRecord.alive
// compares, and a record carrying a starttime that was not actually read off
// this pid would defeat the whole reuse defense.
func writePidfile(path string, pid int) error {
	bootID, err := procBootID()
	if err != nil {
		return err
	}
	startTime, err := readProcStartTime(pid)
	if err != nil {
		return err
	}
	line := strconv.Itoa(pid) + " " + strconv.FormatUint(startTime, 10) + " " + bootID + "\n"
	return writePidRecordLine(path, line)
}

// writePidRecordLine writes line to path atomically: a temp file in the SAME
// directory, then os.Rename. A same-directory rename is atomic, so a reader
// sees the complete record or the previous one, never a torn prefix — and the
// torn-write window a plain os.WriteFile leaves open is EXACTLY the Runner-crash
// window the reaper exists for, where a half-written pidfile would demote a
// recorded live child to the no-pidfile arm and leak it.
//
// The temp file is removed on every failure path: the runtime dir is scanned
// per-file by the reaper, so a stray temp file left behind would be an
// unparseable extra entry it has to reason about.
func writePidRecordLine(path, line string) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp")
	if err != nil {
		return fmt.Errorf("creating temp pidfile in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	closed := false
	defer func() {
		if err == nil {
			return
		}
		if !closed {
			_ = tmp.Close() // the write already failed; a close error on the doomed temp adds nothing actionable
		}
		_ = os.Remove(tmpName) // best-effort cleanup of our own temp file on an already-failing write
	}()

	// os.CreateTemp already opens at 0600, but that mode is umask-masked;
	// Chmod makes the record's 0600 a guarantee rather than an environment
	// dependency.
	if err = tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("setting mode on temp pidfile %s: %w", tmpName, err)
	}
	if _, err = tmp.WriteString(line); err != nil {
		return fmt.Errorf("writing temp pidfile %s: %w", tmpName, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing temp pidfile %s: %w", tmpName, err)
	}
	closed = true
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming temp pidfile into %s: %w", path, err)
	}
	return nil
}

// readPidfile parses either record form from path. Anything else is an error
// rather than a zero record: an unreadable pidfile is a distinct state from a
// dead one, and the reaper must not treat "I cannot tell what this says" as
// "nothing here is alive".
func readPidfile(path string) (pidRecord, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is a host-built pidfile path in the session runtime dir (<RunRoot>/microvm/<id>/*.pid), not user input
	if err != nil {
		return pidRecord{}, fmt.Errorf("reading pidfile %s: %w", path, err)
	}
	text, rest, _ := strings.Cut(string(raw), "\n")
	if strings.TrimSpace(rest) != "" {
		return pidRecord{}, fmt.Errorf("pidfile %s: unexpected content after the record line", path)
	}
	fields := strings.Fields(text)
	if len(fields) > 0 && fields[0] == pidIntentToken {
		if len(fields) != 2 {
			return pidRecord{}, fmt.Errorf("pidfile %s: malformed intent record %q", path, text)
		}
		return pidRecord{Intent: true, BootID: fields[1]}, nil
	}
	if len(fields) != 3 {
		return pidRecord{}, fmt.Errorf("pidfile %s: malformed record %q", path, text)
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return pidRecord{}, fmt.Errorf("pidfile %s: parsing pid %q: %w", path, fields[0], err)
	}
	if pid < 1 {
		return pidRecord{}, fmt.Errorf("pidfile %s: non-positive pid %d", path, pid)
	}
	startTime, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return pidRecord{}, fmt.Errorf("pidfile %s: parsing starttime %q: %w", path, fields[1], err)
	}
	return pidRecord{PID: pid, StartTime: startTime, BootID: fields[2]}, nil
}

// alive reports whether the process this record names is still running.
//
// The boot id is compared FIRST, and the short-circuit is not belt-and-braces:
// starttime is measured in clock ticks SINCE BOOT, so after a reboot a
// long-uptime host re-issues low pids and a stale record's (pid, starttime) pair
// can legitimately MATCH an unrelated process on the new boot. Since a match is
// what authorizes the reaper to signal, that would be a plausible wrong kill; a
// boot-id mismatch means the record predates a reboot and nothing it names can
// exist, so it is "gone" before any proc read or signal.
//
// Within the same boot it re-reads starttime and compares: a mismatch or ENOENT
// means the recorded process is gone and the pid, if live at all, now belongs to
// an unrelated process that must NOT be killed. A bare-pid file can make
// neither distinction.
//
// A same-boot intent record is the third verdict: false with errPidUnknown (see
// errPidUnknown). The bool is meaningless when that error is returned.
func (r pidRecord) alive() (bool, error) {
	bootID, err := procBootID()
	if err != nil {
		return false, err
	}
	if r.BootID != bootID {
		return false, nil
	}
	if r.Intent {
		return false, errPidUnknown
	}
	startTime, err := readProcStartTime(r.PID)
	if err != nil {
		// The process is gone: its /proc entry (or the whole pid) no longer
		// exists. Any other read failure is a genuine fault and propagates,
		// because the reaper must not read "I could not look" as "dead".
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return startTime == r.StartTime, nil
}

// readProcStartTime reads field 22 (starttime) of /proc/<pid>/stat: the
// kernel's process start time in clock ticks since boot, immutable for the
// process's life. Together with the pid it is unique across pid reuse within a
// boot for any wrap slower than one clock tick — the same identity pair systemd
// and every pidfd-less supervisor relies on.
func readProcStartTime(pid int) (uint64, error) {
	statPath := "/proc/" + strconv.Itoa(pid) + "/stat"
	raw, err := os.ReadFile(statPath) //nolint:gosec // G304: statPath is a /proc path built from an integer pid, not user input
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", statPath, err)
	}
	startTime, err := parseProcStartTime(string(raw))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", statPath, err)
	}
	return startTime, nil
}

// parseProcStartTime extracts field 22 from one /proc/<pid>/stat line. It is
// separate from the read above so it can be tested against lines whose field 22
// is KNOWN: a round-trip through the live /proc cannot detect a wrong offset,
// because the writer and the liveness check would misparse identically and
// still agree.
//
// The parse starts from the LAST ')' in the line, not from a whole-line field
// split: field 2 is the executable name, parenthesized, and it may itself
// contain both spaces and parentheses (it is the first 15 bytes of a
// caller-chosen comm), so a naive split misaligns every field after it — and
// yields a wrong-but-plausible number rather than an error.
func parseProcStartTime(line string) (uint64, error) {
	commEnd := strings.LastIndexByte(line, ')')
	if commEnd < 0 {
		return 0, errors.New("malformed stat: no comm terminator")
	}
	// The first field after comm is field 3 (state), so field N sits at index
	// N-3 of this slice.
	fields := strings.Fields(line[commEnd+1:])
	const startTimeIndex = procStartTimeField - 3
	if len(fields) <= startTimeIndex {
		return 0, fmt.Errorf("malformed stat: %d fields after comm, need %d",
			len(fields), startTimeIndex+1)
	}
	startTime, err := strconv.ParseUint(fields[startTimeIndex], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing starttime %q: %w", fields[startTimeIndex], err)
	}
	return startTime, nil
}
