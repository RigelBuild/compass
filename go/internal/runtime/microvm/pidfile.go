//go:build unix

// Session pidfiles record each child as (pid, starttime, boot id), preventing
// crash recovery from signaling an innocent process after pid reuse. An intent
// record is written before spawn and a settled record after it.

package microvm

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// bootIDPath is the kernel UUID that remains stable for a boot and changes on
// reboot, allowing the identity check to reject stale records.
const bootIDPath = "/proc/sys/kernel/random/boot_id"

// pidIntentToken is the literal standing in place of a pid in the pre-spawn
// INTENT record. It cannot collide with a settled record: a settled record's
// first field always parses as a positive integer.
const pidIntentToken = "intent"

// procStartTimeField is the 1-based field number of starttime in
// /proc/<pid>/stat (proc(5)): the process start time in clock ticks since boot.
const procStartTimeField = 22

// errPidUnknown marks a same-boot intent whose child may or may not be running.
// The reaper must keep the directory and never signal or remove it; bool alone
// cannot represent this third liveness verdict.
var errPidUnknown = errors.New("microvm: pidfile records a pre-spawn intent from this boot: liveness unknowable")

// procBootID caches the boot UUID once per process because it cannot change
// while this process runs; rereading it per pidfile would only add syscalls.
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

// writePidIntent writes an atomic pre-spawn record so a crash cannot leave a
// possibly-live child unrecorded. The conservative intent may name a child that
// never spawned, which the reaper can safely tolerate.
func writePidIntent(path string) error {
	bootID, err := procBootID()
	if err != nil {
		return fmt.Errorf("pidfile %s: %w", path, err)
	}
	return writePidRecordLine(path, pidIntentToken+" "+bootID+"\n")
}

// writePidfile atomically replaces the intent with the spawned child's pid,
// starttime, and boot id. Read starttime from the live process to preserve the
// pid-reuse identity check.
func writePidfile(path string, pid int) error {
	bootID, err := procBootID()
	if err != nil {
		return fmt.Errorf("pidfile %s: %w", path, err)
	}
	startTime, err := readProcStartTime(pid)
	if err != nil {
		return fmt.Errorf("pidfile %s: %w", path, err)
	}
	line := strconv.Itoa(pid) + " " + strconv.FormatUint(startTime, 10) + " " + bootID + "\n"
	return writePidRecordLine(path, line)
}

// writePidRecordLine uses a same-directory rename so readers see the old or new
// complete record after a runner crash. It is not fsync-durable across host
// crashes; the boot-id check rejects records from the previous boot. Failures
// remove the temporary file so the reaper sees no stray entry.
func writePidRecordLine(path, line string) (err error) {
	dir := filepath.Dir(path)
	// The pattern's trailing '*' puts os.CreateTemp's random suffix where a
	// reader expects it (".vmm.pid.tmp1234" reads as a temp of vmm.pid), and
	// the leading dot keeps the transient file out of a non-dotfile scan.
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp.*")
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

// maxPidfileRecordBytes bounds readPidfile's read. A record is one line of
// about 60 bytes, so anything longer is not a record at all: bounding the read
// keeps a pidfile that was replaced by something huge from being pulled into
// memory whole, and routes it to the malformed arm below instead.
const maxPidfileRecordBytes = 256

// readPidfile parses either record form from path. Anything else is an error
// rather than a zero record: an unreadable pidfile is a distinct state from a
// dead one, and the reaper must not treat "I cannot tell what this says" as
// "nothing here is alive".
func readPidfile(path string) (pidRecord, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is a host-built pidfile path in the session runtime dir (<RunRoot>/microvm/<id>/*.pid), not user input
	if err != nil {
		return pidRecord{}, fmt.Errorf("reading pidfile %s: %w", path, err)
	}
	defer func() {
		_ = f.Close() // read-only handle: a close error cannot affect the bytes already read, and the record is returned by value
	}()
	// One byte PAST the bound, so an over-long file is detected rather than
	// silently truncated into a prefix that happens to parse as a record.
	raw, err := io.ReadAll(io.LimitReader(f, maxPidfileRecordBytes+1))
	if err != nil {
		return pidRecord{}, fmt.Errorf("reading pidfile %s: %w", path, err)
	}
	if len(raw) > maxPidfileRecordBytes {
		return pidRecord{}, fmt.Errorf("pidfile %s: record exceeds %d bytes", path, maxPidfileRecordBytes)
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

// alive compares boot id before pid/starttime: after reboot, both pid and
// starttime can match an unrelated process, so stale records must be rejected
// before any signal. A same-boot intent returns errPidUnknown; callers must
// keep the directory and never signal it. Other errors are real read failures.
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
		// The process is gone: its /proc entry was already absent, or the task
		// was reaped between the open and the read. Any OTHER read failure is a
		// genuine fault and propagates, because the reaper must not read "I
		// could not look" as "dead".
		if procReadMeansGone(err) {
			return false, nil
		}
		return false, err
	}
	return startTime == r.StartTime, nil
}

// readProcStartTime reads immutable /proc starttime for a pid, the identity
// component used to detect pid reuse within a boot.
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

// procReadMeansGone recognizes ENOENT and ESRCH from /proc reads; ESRCH does not
// match os.ErrNotExist, so both must be checked explicitly.
func procReadMeansGone(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ESRCH)
}

// parseProcStartTime is separate from reading so tests can use known field 22
// values. It starts after the final ')' because /proc field 2 can contain spaces
// and parentheses, which would shift a whole-line split.
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
