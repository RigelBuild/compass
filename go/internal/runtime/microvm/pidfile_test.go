//go:build unix

// pidfile_test.go is the hermetic tier for §(a)'s pidfile primitives. It needs
// no KVM and no guest: it reads /proc/self/stat and the boot id, exactly as the
// production path does — the same Linux-hermetic posture readPSS already has in
// this unix-tagged package.
//
// Every assertion here is a contract the reaper (§(b)) will act on: it kills on
// a starttime match, refuses to kill on a mismatch or a stale boot id, and
// keeps a dir on errPidUnknown. A wrong verdict from any of these is a killed
// innocent process or a leaked orphan, so each is pinned rather than assumed.

package microvm

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// deadPID returns a pid that provably names no process: pid_max itself, which
// the kernel never allocates (pids are drawn from [1, pid_max)). That makes the
// ENOENT arm deterministic — spawning and reaping a real process would leave a
// pid the kernel is free to recycle before the assertion runs.
func deadPID(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile("/proc/sys/kernel/pid_max")
	if err != nil {
		t.Fatalf("reading pid_max: %v", err)
	}
	pidMax, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parsing pid_max %q: %v", raw, err)
	}
	return pidMax
}

// liveBootID is the boot id the production path writes and compares against.
func liveBootID(t *testing.T) string {
	t.Helper()
	id, err := procBootID()
	if err != nil {
		t.Fatalf("procBootID: %v", err)
	}
	return id
}

// dirEntries lists dir's entry names, sorted — the surface the "no temp file
// left behind" assertions compare against.
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

// TestWritePidfileRoundTrip is the settled record's contract: what
// writePidfile puts on disk must read back as the LIVE identity of the pid it
// names — this process's own starttime from /proc and this boot's id. If the
// starttime written were anything other than the one a later readProcStartTime
// yields, alive() would report a false mismatch and the reaper would refuse to
// kill every real orphan.
func TestWritePidfileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vmm.pid")
	pid := os.Getpid()

	if err := writePidfile(path, pid); err != nil {
		t.Fatalf("writePidfile: %v", err)
	}

	rec, err := readPidfile(path)
	if err != nil {
		t.Fatalf("readPidfile: %v", err)
	}
	if rec.Intent {
		t.Errorf("settled record read back with Intent=true")
	}
	if rec.PID != pid {
		t.Errorf("record PID = %d, want %d", rec.PID, pid)
	}
	wantStart, err := readProcStartTime(pid)
	if err != nil {
		t.Fatalf("readProcStartTime(self): %v", err)
	}
	if rec.StartTime != wantStart {
		t.Errorf("record StartTime = %d, want the live proc value %d", rec.StartTime, wantStart)
	}
	if rec.BootID != liveBootID(t) {
		t.Errorf("record BootID = %q, want the live boot id %q", rec.BootID, liveBootID(t))
	}

	// The whole point of the round-trip: this record must read as ALIVE, since
	// it names a running process on this boot.
	alive, err := rec.alive()
	if err != nil {
		t.Fatalf("alive() on a record naming this live process: %v", err)
	}
	if !alive {
		t.Error("alive() = false for a record naming this live process, want true")
	}

	// Mode 0600: the record is host-private.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("pidfile mode = %04o, want 0600", perm)
	}
}

// TestWritePidIntentRoundTrip pins the pre-spawn record's THREE-way verdict,
// which is the reason errPidUnknown exists at all. A same-boot intent record
// names a child that may or may not be running with no pid to check, so the
// reaper must neither kill nor remove — it needs a verdict distinct from both
// alive and dead. A record from a PREVIOUS boot has no such ambiguity: nothing
// it names can exist, so it is plainly dead by the boot-id short-circuit.
func TestWritePidIntentRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vmm.pid")

	if err := writePidIntent(path); err != nil {
		t.Fatalf("writePidIntent: %v", err)
	}

	rec, err := readPidfile(path)
	if err != nil {
		t.Fatalf("readPidfile: %v", err)
	}
	if !rec.Intent {
		t.Errorf("intent record read back with Intent=false (%+v)", rec)
	}
	if rec.BootID != liveBootID(t) {
		t.Errorf("intent record BootID = %q, want the live boot id %q", rec.BootID, liveBootID(t))
	}
	if rec.PID != 0 || rec.StartTime != 0 {
		t.Errorf("intent record carries a pid/starttime (%+v); it names no spawned process", rec)
	}

	// Same boot: unknowable, and that must be a distinct sentinel rather than a
	// bool the reaper would read as a kill authorization or a removal.
	alive, err := rec.alive()
	if !errors.Is(err, errPidUnknown) {
		t.Errorf("alive() on a same-boot intent record: err = %v, want errPidUnknown", err)
	}
	if alive {
		t.Error("alive() = true alongside errPidUnknown; the bool must not claim liveness")
	}

	// A previous boot: dead, with no error — the boot-id short-circuit runs
	// BEFORE the intent check, so a stale intent record is reapable.
	stale := rec
	stale.BootID = perturb(rec.BootID)
	alive, err = stale.alive()
	if err != nil {
		t.Errorf("alive() on a previous-boot intent record: err = %v, want nil", err)
	}
	if alive {
		t.Error("alive() = true for an intent record from a previous boot, want false")
	}
}

// TestWritePidfileSupersedesIntent is the atomic-supersede contract: step 2's
// rename must REPLACE step 1's record in one operation, leaving the dir holding
// exactly one file with exactly the settled record. A scheme that wrote a
// second file, or left the intent record beside the settled one, would hand the
// reaper two contradictory records for one child.
func TestWritePidfileSupersedesIntent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vmm.pid")

	if err := writePidIntent(path); err != nil {
		t.Fatalf("writePidIntent: %v", err)
	}
	if err := writePidfile(path, os.Getpid()); err != nil {
		t.Fatalf("writePidfile over an intent record: %v", err)
	}

	if got := dirEntries(t, dir); !slices.Equal(got, []string{"vmm.pid"}) {
		t.Errorf("dir entries after both writes = %v, want exactly [vmm.pid]", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if strings.Contains(string(raw), pidIntentToken) {
		t.Errorf("pidfile still carries the intent token after the settled write: %q", raw)
	}
	rec, err := readPidfile(path)
	if err != nil {
		t.Fatalf("readPidfile: %v", err)
	}
	if rec.Intent || rec.PID != os.Getpid() {
		t.Errorf("record after the settled write = %+v, want the settled record for pid %d", rec, os.Getpid())
	}
}

// TestPidRecordAliveRefusesUnverifiedIdentities is the kill-an-innocent guard,
// stated as the three ways a record can fail to name its process. Each must be
// (false, nil) — a plain "gone", so the reaper skips it — and NOT a signal.
func TestPidRecordAliveRefusesUnverifiedIdentities(t *testing.T) {
	bootID := liveBootID(t)
	selfStart, err := readProcStartTime(os.Getpid())
	if err != nil {
		t.Fatalf("readProcStartTime(self): %v", err)
	}

	cases := map[string]pidRecord{
		// ENOENT: /proc/<pid> does not exist, so there is nothing to compare.
		"enoent": {PID: deadPID(t), StartTime: selfStart, BootID: bootID},
		// Starttime mismatch on a LIVE pid — the reuse case. This pid is very
		// much alive (it is the test process), so a bare-pid reaper would kill
		// it; the starttime says it is not the recorded process.
		"starttime mismatch": {PID: os.Getpid(), StartTime: selfStart + 1, BootID: bootID},
		// Stale boot id: the record predates a reboot. Load-bearing on its own,
		// because after a reboot a (pid, starttime) pair from the previous boot
		// can legitimately MATCH an unrelated process — and a match authorizes
		// a kill.
		"stale boot id": {PID: os.Getpid(), StartTime: selfStart, BootID: perturb(bootID)},
	}
	for name, rec := range cases {
		t.Run(name, func(t *testing.T) {
			alive, aliveErr := rec.alive()
			if aliveErr != nil {
				t.Fatalf("alive() = _, %v; want a nil error (an unverified identity is plainly gone)", aliveErr)
			}
			if alive {
				t.Errorf("alive() = true for %+v; the reaper would signal a process this record does not name", rec)
			}
		})
	}
}

// TestPidfileWritesLeaveNoTempFile pins the atomic write's hygiene. The reaper
// scans the runtime dir per file, so a leftover `*.tmp*` would be an extra
// unparseable entry it has to reason about — and a temp file that outlived its
// rename would mean the rename was not the only thing that landed.
func TestPidfileWritesLeaveNoTempFile(t *testing.T) {
	dir := t.TempDir()
	names := []string{"vmm.pid", "virtiofsd.pid", "passt.pid"}
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := writePidIntent(path); err != nil {
			t.Fatalf("writePidIntent(%s): %v", name, err)
		}
		if err := writePidfile(path, os.Getpid()); err != nil {
			t.Fatalf("writePidfile(%s): %v", name, err)
		}
	}
	want := slices.Clone(names)
	slices.Sort(want)
	if got := dirEntries(t, dir); !slices.Equal(got, want) {
		t.Errorf("dir entries = %v, want exactly %v (a temp file survived a write)", got, want)
	}
}

// TestReadPidfileRejectsUnparseableContent is the "I cannot tell what this
// says" contract: every malformed form must ERROR rather than read back as a
// zero record, because the reaper must never mistake an unreadable pidfile for
// one naming nothing alive — that path removes the dir and erases the record.
func TestReadPidfileRejectsUnparseableContent(t *testing.T) {
	bootID := liveBootID(t)
	cases := map[string]string{
		"empty":                   "",
		"garbage":                 "not a pid record at all\n",
		"intent missing boot id":  pidIntentToken + "\n",
		"intent with extra field": pidIntentToken + " " + bootID + " extra\n",
		"settled missing boot id": "1234 5678\n",
		"settled extra field":     "1234 5678 " + bootID + " extra\n",
		"non-numeric pid":         "abc 5678 " + bootID + "\n",
		"non-numeric starttime":   "1234 abc " + bootID + "\n",
		"non-positive pid":        "0 5678 " + bootID + "\n",
		"negative pid":            "-1 5678 " + bootID + "\n",
		"second record line":      "1234 5678 " + bootID + "\n9999 1111 " + bootID + "\n",
		"starttime out of uint64": "1234 99999999999999999999999 " + bootID + "\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "vmm.pid")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			rec, err := readPidfile(path)
			if err == nil {
				t.Fatalf("readPidfile(%q) = %+v, nil; want an error", content, rec)
			}
		})
	}
}

// TestReadPidfileErrorsOnAbsentFile keeps the absent-file case distinct from the
// malformed one: it must surface as an os.ErrNotExist-wrapping error, since the
// reaper's "no readable pidfiles" arm (the age gate) is selected by exactly that
// distinction.
func TestReadPidfileErrorsOnAbsentFile(t *testing.T) {
	_, err := readPidfile(filepath.Join(t.TempDir(), "vmm.pid"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("readPidfile on an absent path: err = %v, want it to wrap os.ErrNotExist", err)
	}
}

// TestParseProcStartTimeFindsField22 is the field-22 parsing hazard, checked
// against lines whose field 22 is KNOWN rather than through the live /proc.
// That independence is the whole point: writePidfile and alive() both go
// through this parse, so a wrong offset makes them misparse IDENTICALLY and
// still agree — a write→read→alive round-trip passes just as happily on a
// naive whole-line strings.Fields split. Verified by mutation: swapping this
// parse for that split leaves every round-trip test in this file green.
//
// Field 2 is the comm, parenthesized, carrying the first 15 bytes of the
// executable's basename VERBATIM — spaces and parens included. A whole-line
// split therefore misaligns every later field and yields a wrong-but-plausible
// starttime, not an error: alive() reports a false mismatch and the reaper
// silently refuses to kill a real orphan.
func TestParseProcStartTimeFindsField22(t *testing.T) {
	// Each line's field 22 is 987654321, and every other numeric field is a
	// distinguishable decoy, so an off-by-N offset reads a different value
	// rather than coincidentally matching.
	const want = 987654321
	// Fields 3..21 (state + 18 numbers), then field 22, then a tail.
	tail := " 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 " +
		strconv.Itoa(want) + " 111 222 333 444\n"
	cases := map[string]string{
		"plain comm":              "4242 (sleep) S" + tail,
		"comm with a space":       "4242 (a b c) S" + tail,
		"comm with parens":        "4242 (a (b) c) S" + tail,
		"comm with both":          "4242 (a b) c (d) 0 0) S" + tail,
		"comm that looks numeric": "4242 (0 0 0 0 0) S" + tail,
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parseProcStartTime(line)
			if err != nil {
				t.Fatalf("parseProcStartTime: %v", err)
			}
			if got != want {
				t.Errorf("field 22 = %d, want %d — the parse is offset by the comm's own spaces/parens", got, want)
			}
		})
	}
}

// TestParseProcStartTimeRejectsMalformedLines keeps a truncated or garbled stat
// line an ERROR rather than a zero starttime: zero would compare unequal to
// every real record and quietly demote a live orphan to "gone".
func TestParseProcStartTimeRejectsMalformedLines(t *testing.T) {
	cases := map[string]string{
		"empty":                "",
		"no comm terminator":   "4242 (sleep S 1 2 3\n",
		"too few fields":       "4242 (sleep) S 1 2 3\n",
		"non-numeric field 22": "4242 (sleep) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 nope 111\n",
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parseProcStartTime(line)
			if err == nil {
				t.Errorf("parseProcStartTime(%q) = %d, nil; want an error", line, got)
			}
		})
	}
}

// TestParseProcStartTimeAgreesWithProcSelf ties the parse above back to the
// real kernel format: the synthetic lines pin the OFFSET, and this pins that
// /proc/self/stat actually has that shape. Field 22 is ticks since boot, so the
// only bound available without re-deriving the clock is that it is non-zero and
// below the uptime in ticks — enough to catch a parse that landed on a pointer
// field (those are vastly larger) or on a zero.
func TestParseProcStartTimeAgreesWithProcSelf(t *testing.T) {
	raw, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatalf("reading /proc/self/stat: %v", err)
	}
	startTime, err := parseProcStartTime(string(raw))
	if err != nil {
		t.Fatalf("parseProcStartTime(/proc/self/stat): %v", err)
	}
	if startTime == 0 {
		t.Fatal("this process's starttime parsed as 0; field 22 is non-zero for any process after boot")
	}
	uptimeRaw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		t.Fatalf("reading /proc/uptime: %v", err)
	}
	uptimeSeconds, err := strconv.ParseFloat(strings.Fields(string(uptimeRaw))[0], 64)
	if err != nil {
		t.Fatalf("parsing uptime %q: %v", uptimeRaw, err)
	}
	// USER_HZ is conventionally 100 and is what /proc/<pid>/stat reports in;
	// an upper bound only needs it to be no smaller than that.
	const userHZ = 100
	if maxTicks := uint64(uptimeSeconds * userHZ); startTime > maxTicks {
		t.Errorf("starttime %d ticks exceeds the host uptime %d ticks — field 22 was read from the wrong offset",
			startTime, maxTicks)
	}
}

// TestPidfileIdentifiesAProcessWithADeceptiveComm exercises the hazard
// end-to-end on a REAL process whose comm carries spaces and parens: the
// production write→read→alive path must call a live child alive. The offset
// itself is pinned by the synthetic cases above (this path cannot detect a
// wrong one); what this adds is that nothing in the live path — comm
// truncation, the log capture, the two-step write — breaks on such a name.
func TestPidfileIdentifiesAProcessWithADeceptiveComm(t *testing.T) {
	dir := t.TempDir()
	// A basename whose first 15 bytes contain both spaces and parens, so the
	// comm the kernel reports is itself field-shaped. Copied from /bin/sh: comm
	// comes from the EXECUTED binary, so a shell script under this name would
	// report "sh" and prove nothing.
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh on PATH: %v", err)
	}
	shBytes, err := os.ReadFile(sh)
	if err != nil {
		t.Fatalf("reading %s: %v", sh, err)
	}
	bin := filepath.Join(dir, "a b) c (d) 0 0 0 0")
	if err = os.WriteFile(bin, shBytes, 0o700); err != nil {
		t.Fatalf("writing the deceptively-named binary: %v", err)
	}

	vm := &VM{}
	c := &child{
		name:    "cloud-hypervisor",
		logPath: filepath.Join(dir, "child.log"),
		// Blocks on a stdin read rather than sleeping, so it stays alive for
		// the assertions and exits when the pipe closes — no timing window.
		cmd: exec.CommandContext(t.Context(), bin, "-c", "read -r _ || true"),
	}
	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	if err = vm.startRecordedChild(c, dir, "vmm.pid"); err != nil {
		t.Fatalf("startRecordedChild over a deceptively-named binary: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close() // releases the child's blocking read; a close error on teardown is not actionable
		<-c.exited
	})

	// The comm really is deceptive: assert the fixture before trusting the
	// verdict it produces, so a kernel that truncated the parens away cannot
	// make this test pass vacuously.
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(c.cmd.Process.Pid) + "/stat")
	if err != nil {
		t.Fatalf("reading the child's stat: %v", err)
	}
	comm := string(stat)
	comm = comm[strings.IndexByte(comm, '(')+1 : strings.LastIndexByte(comm, ')')]
	if !strings.ContainsAny(comm, " )") {
		t.Fatalf("comm %q carries no space or paren; the fixture no longer exercises the hazard", comm)
	}

	rec, err := readPidfile(filepath.Join(dir, "vmm.pid"))
	if err != nil {
		t.Fatalf("readPidfile: %v", err)
	}
	if rec.PID != c.cmd.Process.Pid {
		t.Errorf("record PID = %d, want the child's %d", rec.PID, c.cmd.Process.Pid)
	}
	alive, err := rec.alive()
	if err != nil {
		t.Fatalf("alive() on the live deceptively-named child: %v", err)
	}
	if !alive {
		t.Errorf("alive() = false for a LIVE child with comm %q", comm)
	}
}

// TestSpawnFailureLeavesTheIntentRecord is the whole reason step 1 exists,
// exercised through the production wiring rather than the primitives alone: when
// the spawn between the two writes fails, the runtime dir must still NAME the
// child. Before the intent record, that crash window left a dir with no record
// at all of a child that may have become live, and §(b) step 3 would read
// "everything recorded is dead" and remove the evidence.
func TestSpawnFailureLeavesTheIntentRecord(t *testing.T) {
	dir := t.TempDir()
	vm := &VM{}
	c := &child{
		name:    "cloud-hypervisor",
		logPath: filepath.Join(dir, "cloud-hypervisor.log"),
		// An absolute path to a binary that does not exist: Start fails, so the
		// settled write never runs.
		cmd: exec.CommandContext(t.Context(), filepath.Join(dir, "no-such-binary")),
	}

	err := vm.startRecordedChild(c, dir, "vmm.pid")
	if err == nil {
		t.Fatal("startRecordedChild over an unspawnable binary = nil, want an error")
	}
	if c.cmd.Process != nil {
		t.Fatalf("the fake spawned after all (pid %d); the test no longer exercises the spawn-failure window", c.cmd.Process.Pid)
	}

	// The path must be registered for cleanup even though the boot failed —
	// otherwise the intent record outlives the session that wrote it.
	if !slices.Contains(vm.pidfiles, filepath.Join(dir, "vmm.pid")) {
		t.Errorf("vm.pidfiles = %v, want it to carry vmm.pid so Shutdown removes the intent record", vm.pidfiles)
	}

	rec, readErr := readPidfile(filepath.Join(dir, "vmm.pid"))
	if readErr != nil {
		t.Fatalf("readPidfile after a failed spawn: %v — the dir no longer names the child", readErr)
	}
	if !rec.Intent {
		t.Errorf("record after a failed spawn = %+v, want the intent record (no settled record can exist)", rec)
	}
	// And it routes to the possibly-live arm, not to a kill or a removal.
	if _, aliveErr := rec.alive(); !errors.Is(aliveErr, errPidUnknown) {
		t.Errorf("alive() after a failed spawn: err = %v, want errPidUnknown", aliveErr)
	}
}

// perturb returns a boot id that is well-formed but NOT this host's, by
// swapping the first hex digit for a different one. Fabricating a
// plainly-bogus string would leave the boot-id comparison passing for the
// wrong reason (an unparseable id rather than a merely different one).
func perturb(bootID string) string {
	if bootID == "" {
		return "0"
	}
	first := "0"
	if bootID[0] == '0' {
		first = "1"
	}
	return first + bootID[1:]
}
