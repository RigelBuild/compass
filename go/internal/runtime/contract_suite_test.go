package runtime

// The untagged shared suite proves WorkloadRuntime behavior across backend implementations.
// Entrypoints supply backend factories and capability flags without tagged symbols here.
// Each divergence flag controls a row assertion, so silently widening behavior fails.

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// backendCaps describes the session factory and backend-specific contract expectations.
// Each flag controls an assertion that detects a widened divergence.
type backendCaps struct {
	// name identifies the backend under test, for subtest / failure messages.
	name string

	// makeSpec creates backend-specific workloads; the shared body only consumes the handle.
	// Each test supplies a fresh temp workspace.
	makeSpec func(t *testing.T, name string) WorkloadSpec

	// refusesRootExec: a uid-0 exec is refused with a host/guest error (microVM
	// §(b) uid enforcement, record 576). When false (podman) the equivalent
	// posture is asserted instead — a directed unprivileged exec runs as the
	// requested uid, never silently escalated.
	refusesRootExec bool

	// numericUIDOnly: a NON-numeric ExecSpec.User is a host-side error (microVM,
	// divergence 2, record 585-587). When false (podman) the engine resolves
	// image user names, so the row is skipped.
	numericUIDOnly bool

	// emptyMountLabel: MountLabel always returns "" (microVM has no MCS label,
	// divergence 3 / row 12, record 587). When false (podman) MountLabel is the
	// engine's real label (possibly "" on a non-SELinux host), so only a
	// no-error read is asserted.
	emptyMountLabel bool

	// ignoresCommandAndCapAdd: spec.Command and spec.CapAdd have NO effect
	// (microVM: no keep-alive process, no capability grant to the workload;
	// divergence 4, record 588-589). When false (podman) Command is the
	// keep-alive entrypoint, so the row is skipped.
	ignoresCommandAndCapAdd bool

	// capsOutput: a one-shot exec whose output exceeds the 8 MiB cap returns the
	// truncation error rather than a clipped tail (microVM OQ-E, divergence 1,
	// record 583-585). When false (podman) capture is unbounded, so the row is
	// skipped.
	capsOutput bool

	// This flag means a guest powers off on SIGTERM before kill escalation.
	// Podman's sleep PID 1 ignores SIGTERM, so that row is skipped there.
	gracefulStopPowersOff bool

	// This flag permits backend-specific signal errors while preserving one kill contract.
	// MicroVM returns ExitStatusError; podman returns exec.ExitError, and both are recognized.
	portableKillError bool

	// This closure checks backend-specific duplicate-name errors without unix-only types.
	assertDuplicateName func(t *testing.T, err error)

	// resizeErr overrides the shared reserved sentinel; the host uses its permanent unsupported sentinel.
	resizeErr error

	// euidOnly selects the host uid-enforcement posture in rowUIDEnforcement: a
	// host child cannot switch user, so the ONLY accepted uid is the Runner's
	// own euid (execUID) and any other uid (rejectedUID) is refused. Off for the
	// engine legs, which switch the workload to a directed uid.
	euidOnly bool

	// execUID is the uid the directed exec rows run as, as a decimal string.
	// Empty means "1000" (the baked agent uid the engine legs use). The host leg
	// sets the Runner's own euid (os.Geteuid()), the only uid its execs may run
	// as.
	execUID string

	// inheritsRunnerCaps: the workload inherits the Runner's own capability set
	// rather than starting from an empty container set (host). It changes what
	// "CapAdd granted nothing" means in rowCommandCapAddIgnored; the engine legs
	// leave it false and keep the all-zero assertion.
	inheritsRunnerCaps bool

	// rejectedUID is a uid the host backend must REFUSE (any uid other than its
	// euid); used only when euidOnly is set. The engine legs leave it empty.
	rejectedUID string

	// armedDefaultDenyAtStart: every Start arms default-deny egress, even for a
	// spec with no Egress set (microVM, divergence 7). When false (podman, host)
	// arming is AgentRuntime's job, so the row is skipped.
	armedDefaultDenyAtStart bool
}

// execUser selects the uid for exec rows: engine legs use baked uid 1000,
// while the host leg must set its own euid or its user check fails.
func (c backendCaps) execUser() string {
	if c.execUID != "" {
		return c.execUID
	}
	return "1000"
}

// resize is the sentinel rowResize expects: the caps override when set, else the
// shared C3-reserved ErrResizeNotImplemented the engine legs use.
func (c backendCaps) resize() error {
	if c.resizeErr != nil {
		return c.resizeErr
	}
	return ErrResizeNotImplemented
}

// wantCapEff returns the expected effective capabilities after CapAdd.
// Host children inherit the Runner's set; engine workloads start empty.
func (c backendCaps) wantCapEff(t *testing.T) string {
	t.Helper()
	if !c.inheritsRunnerCaps {
		return "0000000000000000"
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skipf("reading own /proc/self/status: %v", err)
	}
	for line := range strings.SplitSeq(string(status), "\n") {
		if rest, ok := strings.CutPrefix(line, "CapEff:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatal("no CapEff line in own /proc/self/status")
	return ""
}

// runContractSuite dispatches shared rows; stateless exec rows reuse one session,
// while lifecycle rows own separate sessions so they cannot perturb one another.
func runContractSuite(t *testing.T, newRuntime func(t *testing.T) WorkloadRuntime, caps backendCaps) {
	t.Helper()
	rt := newRuntime(t)
	primary := startRunning(t, rt, caps, "contract-primary")

	t.Run("exec_exit_codes", func(t *testing.T) { rowExecExitCodes(t, rt, caps, primary) })
	t.Run("exec_stdin", func(t *testing.T) { rowExecStdin(t, rt, caps, primary) })
	t.Run("exec_unstartable_programs", func(t *testing.T) { rowExecUnstartablePrograms(t, rt, caps, primary) })
	t.Run("exec_stream_unstartable_programs", func(t *testing.T) { rowExecStreamingUnstartablePrograms(t, rt, caps, primary) })
	t.Run("streaming_stdio", func(t *testing.T) { rowStreamingStdio(t, rt, caps, primary) })
	t.Run("kill_wait", func(t *testing.T) { rowKillWait(t, rt, caps, primary) })
	t.Run("ctx_cancel_reaps", func(t *testing.T) { rowCtxCancelReaps(t, rt, caps, primary) })
	t.Run("uid_enforcement", func(t *testing.T) { rowUIDEnforcement(t, rt, caps, primary) })
	t.Run("resize_not_implemented", func(t *testing.T) { rowResize(t, rt, caps, primary) })
	t.Run("mount_label", func(t *testing.T) { rowMountLabel(t, rt, caps, primary) })
	if caps.numericUIDOnly {
		t.Run("non_numeric_user_refused", func(t *testing.T) { rowNonNumericUser(t, rt, primary) })
	}
	if caps.capsOutput {
		t.Run("output_cap_truncation", func(t *testing.T) { rowOutputCap(t, rt, primary) })
	}
	if caps.ignoresCommandAndCapAdd {
		t.Run("command_capadd_ignored", func(t *testing.T) { rowCommandCapAddIgnored(t, rt, caps) })
	}
	t.Run("duplicate_name_refused", func(t *testing.T) { rowDuplicateName(t, rt, caps) })
	t.Run("stop_remove_idempotence", func(t *testing.T) { rowStopRemoveIdempotence(t, rt, caps) })
	t.Run("exists_before_after_remove", func(t *testing.T) { rowExistsBeforeAfterRemove(t, rt, caps) })
	t.Run("running_lifecycle", func(t *testing.T) { rowRunningLifecycle(t, rt, caps) })
	if caps.gracefulStopPowersOff {
		t.Run("stop_grace_powers_off", func(t *testing.T) { rowStopGrace(t, rt, caps) })
	}
	if caps.armedDefaultDenyAtStart {
		t.Run("default_deny_at_start", func(t *testing.T) { rowDefaultDenyAtStart(t, rt, caps, primary) })
	}
}

// rowExecExitCodes — row 1 (record 568-569): exit 0 is a success carrying the
// echoed body; a non-zero exit is a SUCCESSFUL call returning the code, NEVER an
// error. A regression that folded a non-zero exit into err would turn every
// expected-failure probe (a denied firewall check) into a fatal.
func rowExecExitCodes(t *testing.T, rt WorkloadRuntime, caps backendCaps, primary WorkloadID) {
	t.Helper()
	out, err := rt.Exec(t.Context(), primary, NewExecSpec("sh", "-c", "echo hello-body").AsUser(caps.execUser()))
	if err != nil {
		t.Fatalf("Exec(echo): %v", err)
	}
	if !out.Success() {
		t.Fatalf("Exec(echo) exit = %d, stderr = %q, want exit 0", out.ExitCode, out.Stderr)
	}
	if !strings.Contains(out.Stdout, "hello-body") {
		t.Fatalf("Exec(echo) stdout = %q, want it to carry the echoed body", out.Stdout)
	}
	out, err = rt.Exec(t.Context(), primary, NewExecSpec("sh", "-c", "exit 7").AsUser(caps.execUser()))
	if err != nil {
		t.Fatalf("a non-zero exit must be a successful call, got err %v", err)
	}
	if out.ExitCode != 7 {
		t.Fatalf("Exec(exit 7) ExitCode = %d, want 7", out.ExitCode)
	}
	if out.Success() {
		t.Fatal("Exec(exit 7).Success() = true, want false")
	}
}

// hasCannotInvokeReason accepts each backend's 126 wording, podman's included.
func hasCannotInvokeReason(stderr string) bool {
	lower := strings.ToLower(stderr)
	for _, reason := range []string{"not executable", "permission denied", "not a regular file"} {
		if strings.Contains(lower, reason) {
			return true
		}
	}
	return false
}

func rowExecUnstartablePrograms(t *testing.T, rt WorkloadRuntime, caps backendCaps, primary WorkloadID) {
	t.Helper()
	program := "/etc/passwd"
	for _, tc := range []struct {
		name    string
		command string
		code    int
	}{
		{name: "missing program", command: "/compass-missing-contract-probe", code: 127},
		{name: "present but non-executable program", command: program, code: 126},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := rt.Exec(t.Context(), primary, NewExecSpec(tc.command).AsUser(caps.execUser()))
			if err != nil {
				t.Fatalf("Exec(%q) = %v, want exit %d and nil error", tc.command, err, tc.code)
			}
			if out.ExitCode != tc.code || out.Stderr == "" {
				t.Fatalf("Exec(%q) = %+v, want exit %d and non-empty stderr", tc.command, out, tc.code)
			}
			if tc.code == 126 && (!strings.Contains(out.Stderr, tc.command) || !hasCannotInvokeReason(out.Stderr)) {
				t.Fatalf("Exec(%q) stderr = %q, want program path and a cannot-invoke reason", tc.command, out.Stderr)
			}
		})
	}
}

func rowExecStreamingUnstartablePrograms(t *testing.T, rt WorkloadRuntime, caps backendCaps, primary WorkloadID) {
	t.Helper()
	program := "/etc/passwd"
	for _, tc := range []struct {
		name    string
		command string
		code    int
	}{
		{name: "missing program", command: "/compass-missing-contract-probe", code: 127},
		{name: "present but non-executable program", command: program, code: 126},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream, err := rt.ExecStreaming(t.Context(), primary, NewStreamingExecSpec(tc.command).AsUser(caps.execUser()))
			if err != nil {
				t.Fatalf("ExecStreaming(%q) = %v, want an exited stream", tc.command, err)
			}
			defer stream.IO.Stdin.Close()
			// Drain both concurrently: microVM stdio is unbuffered io.Pipe, so
			// reading stdout to EOF first blocks the pump on the stderr frame.
			stderrCh := make(chan []byte, 1)
			go func() {
				b, _ := io.ReadAll(stream.IO.Stderr)
				stderrCh <- b
			}()
			stdout, err := io.ReadAll(stream.IO.Stdout)
			if err != nil || len(stdout) != 0 {
				t.Fatalf("stdout = %q, err = %v; want empty stdout", stdout, err)
			}
			stderr := <-stderrCh
			if len(stderr) == 0 {
				t.Fatalf("stderr is empty; want a diagnostic")
			}
			if tc.code == 126 && (!strings.Contains(string(stderr), tc.command) || !hasCannotInvokeReason(string(stderr))) {
				t.Fatalf("stderr = %q, want program path %q and a cannot-invoke reason", stderr, tc.command)
			}
			if waitErr := stream.Process.Wait(); exitCodeOf(waitErr, caps) != tc.code {
				t.Fatalf("Process.Wait() = %v; want exit %d without signal", waitErr, tc.code)
			}
		})
	}
}

// rowExecStdin — row 2 (record 567-569, agent.go:238-246): the script-over-stdin
// shape end to end (the secret-safe channel). `sh -s` reads the script from
// stdin, so the body never appears in the argv / process list.
func rowExecStdin(t *testing.T, rt WorkloadRuntime, caps backendCaps, primary WorkloadID) {
	t.Helper()
	out, err := rt.Exec(t.Context(), primary, NewExecSpec("sh", "-s").WithStdin("echo from-stdin").AsUser(caps.execUser()))
	if err != nil {
		t.Fatalf("Exec(sh -s): %v", err)
	}
	if !out.Success() {
		t.Fatalf("Exec(sh -s) exit = %d, stderr = %q, want exit 0", out.ExitCode, out.Stderr)
	}
	if out.Stdout != "from-stdin\n" {
		t.Fatalf("Exec(sh -s) stdout = %q, want %q", out.Stdout, "from-stdin\n")
	}
}

// rowStreamingStdio — row 3: write to Stdin, read the echoed bytes back from
// Stdout, proving live bidirectional interleaving over the pipes; then
// Terminate. stderr is drained in a goroutine so a full pipe never deadlocks the
// terminate.
func rowStreamingStdio(t *testing.T, rt WorkloadRuntime, caps backendCaps, primary WorkloadID) {
	t.Helper()
	stream, err := rt.ExecStreaming(t.Context(), primary, NewStreamingExecSpec("cat").AsUser(caps.execUser()))
	if err != nil {
		t.Fatalf("ExecStreaming(cat): %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, stream.IO.Stderr) }()
	for _, msg := range []string{"ping-1\n", "ping-2\n"} {
		if _, err := io.WriteString(stream.IO.Stdin, msg); err != nil {
			t.Fatalf("write %q to stdin: %v", msg, err)
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(stream.IO.Stdout, buf); err != nil {
			t.Fatalf("read echoed %q from stdout: %v", msg, err)
		}
		if string(buf) != msg {
			t.Fatalf("cat echoed %q, want %q", string(buf), msg)
		}
	}
	if err := stream.Process.Terminate(); err != nil && !deliberateKill(err, caps) {
		t.Fatalf("Terminate: unexpected error %v", err)
	}
}

// rowKillWait drains both pipes before asserting that Kill+Wait yields a recognized signal error.
// Backends may expose different error types, but both must preserve the kill contract.
func rowKillWait(t *testing.T, rt WorkloadRuntime, caps backendCaps, primary WorkloadID) {
	t.Helper()
	stream, err := rt.ExecStreaming(t.Context(), primary, NewStreamingExecSpec("sleep", "300").AsUser(caps.execUser()))
	if err != nil {
		t.Fatalf("ExecStreaming(sleep): %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, stream.IO.Stdout) }()
	go func() { _, _ = io.Copy(io.Discard, stream.IO.Stderr) }()
	if err := stream.Process.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	assertDeliberateKill(t, stream.Process.Wait(), caps)
}

// rowCtxCancelReaps verifies that canceling a streaming exec reaps its child.
// The bounded wait fails instead of hanging if reaping does not finish.
func rowCtxCancelReaps(t *testing.T, rt WorkloadRuntime, caps backendCaps, primary WorkloadID) {
	t.Helper()
	cctx, cancel := context.WithCancel(t.Context())
	stream, err := rt.ExecStreaming(cctx, primary, NewStreamingExecSpec("sleep", "300").AsUser(caps.execUser()))
	if err != nil {
		cancel()
		t.Fatalf("ExecStreaming(sleep): %v", err)
	}
	go func() { _, _ = io.Copy(io.Discard, stream.IO.Stdout) }()
	go func() { _, _ = io.Copy(io.Discard, stream.IO.Stderr) }()
	cancel()
	done := make(chan struct{})
	go func() { _ = stream.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Wait did not return within 30s after ctx cancel; the streaming child was not reaped")
	}
}

// rowUIDEnforcement checks each backend's user boundary: guest refusal, podman requested uid,
// or host euid-only execution.
func rowUIDEnforcement(t *testing.T, rt WorkloadRuntime, caps backendCaps, primary WorkloadID) {
	t.Helper()
	if caps.euidOnly {
		// A host child cannot switch user, so an exec as the Runner's own euid
		// runs and an exec naming any other uid is refused — the tier never runs
		// a command under a uid the caller did not actually get.
		out, err := rt.Exec(t.Context(), primary, NewExecSpec("id", "-u").AsUser(caps.execUser()))
		if err != nil {
			t.Fatalf("Exec(id -u) as the Runner's euid: %v", err)
		}
		if got := strings.TrimSpace(out.Stdout); got != caps.execUser() {
			t.Fatalf("exec ran as uid %q, want the Runner's euid %q", got, caps.execUser())
		}
		if _, err := rt.Exec(t.Context(), primary, NewExecSpec("id", "-u").AsUser(caps.rejectedUID)); err == nil {
			t.Fatalf("an exec naming uid %q (not the Runner's euid) must be refused; got no error", caps.rejectedUID)
		}
		return
	}
	if caps.refusesRootExec {
		if _, err := rt.Exec(t.Context(), primary, NewExecSpec("id", "-u").AsUser("0")); err == nil {
			t.Fatal("a uid-0 exec must be refused on this backend; got no error")
		}
		return
	}
	out, err := rt.Exec(t.Context(), primary, NewExecSpec("id", "-u").AsUser(caps.execUser()))
	if err != nil {
		t.Fatalf("Exec(id -u): %v", err)
	}
	if got := strings.TrimSpace(out.Stdout); got != caps.execUser() {
		t.Fatalf("directed unprivileged exec ran as uid %q, want %q", got, caps.execUser())
	}
}

// rowResize — row 11 (record 577-578): Resize refuses legibly, never faking a
// limit change that never happened. The expected sentinel is caps.resize():
// ErrResizeNotImplemented (the engine legs' C3-reserved sentinel) or, for the
// host leg, the distinct permanent ErrResizeUnsupportedOnHost.
func rowResize(t *testing.T, rt WorkloadRuntime, caps backendCaps, primary WorkloadID) {
	t.Helper()
	want := caps.resize()
	if err := rt.Resize(t.Context(), primary, ResourceLimits{CPUShares: 512}); !errors.Is(err, want) {
		t.Fatalf("Resize err = %v, want %v", err, want)
	}
}

// rowMountLabel — row 12: "" on microVM (record 587, capability-gated); podman
// returns its real label, which may legitimately be "" on a non-SELinux host, so
// there the row asserts only a no-error read.
func rowMountLabel(t *testing.T, rt WorkloadRuntime, caps backendCaps, primary WorkloadID) {
	t.Helper()
	label, err := rt.MountLabel(t.Context(), primary)
	if err != nil {
		t.Fatalf("MountLabel: %v", err)
	}
	if caps.emptyMountLabel && label != "" {
		t.Fatalf("MountLabel = %q, want empty on this backend (no MCS label)", label)
	}
}

// rowNonNumericUser — divergence 2 (microVM only, record 585-587): a non-numeric
// ExecSpec.User is a host-side error. Asserting the refusal fails a backend that
// started resolving names (silently widening).
func rowNonNumericUser(t *testing.T, rt WorkloadRuntime, primary WorkloadID) {
	t.Helper()
	if _, err := rt.Exec(t.Context(), primary, NewExecSpec("id", "-u").AsUser("not-a-number")); err == nil {
		t.Fatal("a non-numeric ExecSpec.User must be a host-side error on this backend; got no error")
	}
}

// rowOutputCap — divergence 1 (microVM only, OQ-E, record 583-585): an over-cap
// one-shot exec returns the truncation error, not a clipped tail. A backend that
// started truncating silently (widening) would pass a caller a partial output as
// if whole — this row fails that.
func rowOutputCap(t *testing.T, rt WorkloadRuntime, primary WorkloadID) {
	t.Helper()
	// 9 MiB > the 8 MiB cap; content is irrelevant, only the byte count.
	if _, err := rt.Exec(t.Context(), primary, NewExecSpec("sh", "-c", "head -c 9437184 /dev/zero").AsUser("1000")); err == nil {
		t.Fatal("an over-cap exec must return the truncation error on this backend; got no error")
	}
}

// rowCommandCapAddIgnored pins the microVM divergence: Command and CapAdd do not affect
// startup or the workload's empty capability set.
func rowCommandCapAddIgnored(t *testing.T, rt WorkloadRuntime, caps backendCaps) {
	t.Helper()
	spec := caps.makeSpec(t, "contract-cmd-capadd")
	spec.Command = []string{"/nonexistent-entrypoint-must-be-ignored"}
	spec.CapAdd = []string{"NET_ADMIN"}
	id, err := rt.Create(t.Context(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	registerRemove(t, rt, id, "cmd-capadd")
	if err := rt.Start(t.Context(), id); err != nil {
		t.Fatalf("Start must succeed though spec.Command is bogus (Command ignored): %v", err)
	}
	out, err := rt.Exec(t.Context(), id, NewExecSpec("echo", "alive").AsUser(caps.execUser()))
	if err != nil || !out.Success() || !strings.Contains(out.Stdout, "alive") {
		t.Fatalf("exec must still work on the ignored-Command session: out=%+v err=%v", out, err)
	}
	status, err := rt.Exec(t.Context(), id, NewExecSpec("cat", "/proc/self/status").AsUser(caps.execUser()))
	if err != nil || !status.Success() {
		t.Fatalf("reading /proc/self/status: out=%+v err=%v", status, err)
	}
	capEff := ""
	for line := range strings.SplitSeq(status.Stdout, "\n") {
		if rest, ok := strings.CutPrefix(line, "CapEff:"); ok {
			capEff = strings.TrimSpace(rest)
		}
	}
	want := caps.wantCapEff(t)
	if capEff != want {
		t.Fatalf("workload CapEff = %q, want %q (spec.CapAdd must grant the workload nothing)", capEff, want)
	}
}

// rowDuplicateName — row 7 (record 580-581 lineage): a duplicate-name Create is
// refused with the backend's typed collision error keyed on spec.Name. The
// second Create of a live name must fail, and with the expected type (gated via
// caps).
func rowDuplicateName(t *testing.T, rt WorkloadRuntime, caps backendCaps) {
	t.Helper()
	const name = "contract-dup"
	id, err := rt.Create(t.Context(), caps.makeSpec(t, name))
	if err != nil {
		t.Fatalf("first Create(%s): %v", name, err)
	}
	registerRemove(t, rt, id, "dup")
	_, err = rt.Create(t.Context(), caps.makeSpec(t, name))
	if err == nil {
		t.Fatal("a duplicate-name Create must be refused; got no error")
	}
	caps.assertDuplicateName(t, err)
}

// rowStopRemoveIdempotence — row 8 (record 577): a double Stop is not an error, a
// Remove of an already-removed id is nil, and a Remove of a never-created id is
// nil.
func rowStopRemoveIdempotence(t *testing.T, rt WorkloadRuntime, caps backendCaps) {
	t.Helper()
	id := startRunning(t, rt, caps, "contract-idem")
	if err := rt.Stop(t.Context(), id, 5*time.Second); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := rt.Stop(t.Context(), id, 5*time.Second); err != nil {
		t.Fatalf("double Stop must not error: %v", err)
	}
	if err := rt.Remove(t.Context(), id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := rt.Remove(t.Context(), id); err != nil {
		t.Fatalf("Remove of an already-removed id must be nil: %v", err)
	}
	if err := rt.Remove(t.Context(), WorkloadID("contract-never-created")); err != nil {
		t.Fatalf("Remove of a never-created id must be nil: %v", err)
	}
}

// rowExistsBeforeAfterRemove — row 10 (record 587-590 lineage): Exists is true
// after Create, false after Remove, keyed on spec.Name.
func rowExistsBeforeAfterRemove(t *testing.T, rt WorkloadRuntime, caps backendCaps) {
	t.Helper()
	const name = "contract-exists"
	id, err := rt.Create(t.Context(), caps.makeSpec(t, name))
	if err != nil {
		t.Fatalf("Create(%s): %v", name, err)
	}
	registerRemove(t, rt, id, "exists")
	ok, err := rt.Exists(t.Context(), name)
	if err != nil {
		t.Fatalf("Exists after Create: %v", err)
	}
	if !ok {
		t.Fatal("Exists after Create = false, want true")
	}
	if err := rt.Remove(t.Context(), id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	ok, err = rt.Exists(t.Context(), name)
	if err != nil {
		t.Fatalf("Exists after Remove: %v", err)
	}
	if ok {
		t.Fatal("Exists after Remove = true, want false")
	}
}

func rowRunningLifecycle(t *testing.T, rt WorkloadRuntime, caps backendCaps) {
	t.Helper()
	const name = "contract-running"
	id, err := rt.Create(t.Context(), caps.makeSpec(t, name))
	if err != nil {
		t.Fatalf("Create(%s): %v", name, err)
	}
	registerRemove(t, rt, id, "running")
	if running, err := rt.Running(t.Context(), "contract-unknown"); err != nil || running {
		t.Fatalf("Running unknown name = %v, %v; want false, nil", running, err)
	}
	if err := rt.Start(t.Context(), id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if running, err := rt.Running(t.Context(), name); err != nil || !running {
		t.Fatalf("Running after Start = %v, %v; want true, nil", running, err)
	}
	if err := rt.Stop(t.Context(), id, 5*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if running, err := rt.Running(t.Context(), name); err != nil || running {
		t.Fatalf("Running after Stop = %v, %v; want false, nil", running, err)
	}
}

// rowStopGrace verifies that a SIGTERM-honoring guest powers off before kill escalation.
// Stop must complete well before its grace expires.
func rowStopGrace(t *testing.T, rt WorkloadRuntime, caps backendCaps) {
	t.Helper()
	id := startRunning(t, rt, caps, "contract-stopgrace")
	const grace = 30 * time.Second
	start := time.Now()
	if err := rt.Stop(t.Context(), id, grace); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("graceful Stop completed in %s (grace %s)", elapsed, grace)
	if elapsed >= grace-5*time.Second {
		t.Fatalf("Stop took %s of a %s grace: the guest did not power off gracefully — it fell through to the kill escalation", elapsed, grace)
	}
}

// rowDefaultDenyAtStart — divergence 7. It requires exit 124 (a dropped SYN hits
// the guest timeout) because a guest with no route fails fast and must not pass.
func rowDefaultDenyAtStart(t *testing.T, rt WorkloadRuntime, caps backendCaps, primary WorkloadID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	script := "timeout 10 bash -c 'exec 3<>/dev/tcp/8.8.8.8/443 && echo connected'"
	out, err := rt.Exec(ctx, primary, NewExecSpec("sh", "-c", script).AsUser(caps.execUser()))
	if err != nil {
		t.Fatalf("connect probe errored (harness fault, not a firewall verdict): %v", err)
	}
	if out.ExitCode != 124 {
		t.Fatalf("connect to 8.8.8.8:443 from a spec with no Egress: exit=%d stdout=%q stderr=%q; want 124 (SYN dropped by the default-deny ruleset)",
			out.ExitCode, out.Stdout, out.Stderr)
	}
}

// startRunning Creates + Starts a container from caps.makeSpec and registers a
// Remove backstop, the shared happy-path setup for the exec/stream rows. A Create
// or Start failure is fatal (the row cannot run).
func startRunning(t *testing.T, rt WorkloadRuntime, caps backendCaps, name string) WorkloadID {
	t.Helper()
	id, err := rt.Create(t.Context(), caps.makeSpec(t, name))
	if err != nil {
		t.Fatalf("Create(%s): %v", name, err)
	}
	registerRemove(t, rt, id, name)
	if err := rt.Start(t.Context(), id); err != nil {
		t.Fatalf("Start(%s): %v", name, err)
	}
	return id
}

// registerRemove adds a cleanup backstop that detaches test cancellation.
// Teardown must still run after t.Context() is canceled.
func registerRemove(t *testing.T, rt WorkloadRuntime, id WorkloadID, label string) {
	t.Helper()
	t.Cleanup(func() {
		if err := rt.Remove(context.WithoutCancel(t.Context()), id); err != nil {
			t.Errorf("Remove(%s) cleanup: %v", label, err)
		}
	})
}

// exitCodeOf reads a plain exit code from a Wait error: the portable
// *ExitStatusError, or *exec.ExitError only for backends without one; -1 otherwise.
func exitCodeOf(err error, caps backendCaps) int {
	if e, ok := errors.AsType[*ExitStatusError](err); ok && e.Signal == 0 {
		return e.Code
	}
	if caps.portableKillError {
		return -1
	}
	if e, ok := errors.AsType[*exec.ExitError](err); ok {
		return e.ExitCode()
	}
	return -1
}

func TestExitCodeOf(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "exit 3")
	runErr := cmd.Run()
	if _, ok := errors.AsType[*exec.ExitError](runErr); !ok {
		t.Fatalf("sh exit 3 = %v, want *exec.ExitError", runErr)
	}
	for _, tc := range []struct {
		name     string
		err      error
		portable bool
		want     int
	}{
		{name: "exec.ExitError without portable exit error", err: runErr, want: 3},
		{name: "exec.ExitError with portable exit error", err: runErr, portable: true, want: -1},
		{name: "portable exit", err: &ExitStatusError{Code: 126}, portable: true, want: 126},
		{name: "signalled portable exit", err: &ExitStatusError{Signal: 9}, portable: true, want: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCodeOf(tc.err, backendCaps{portableKillError: tc.portable}); got != tc.want {
				t.Fatalf("exitCodeOf = %d, want %d", got, tc.want)
			}
		})
	}
}

// deliberateKill recognizes the backend's signaled-exit error after Kill+Wait.
// ExitCode is portable here, so this untagged file avoids syscall.WaitStatus.
func deliberateKill(err error, caps backendCaps) bool {
	if err == nil {
		return false
	}
	if caps.portableKillError {
		e, ok := errors.AsType[*ExitStatusError](err)
		return ok && e.Signal != 0
	}
	e, ok := errors.AsType[*exec.ExitError](err)
	return ok && killedBySIGKILL(e)
}

// assertDeliberateKill fails unless err is the expected deliberate-kill error
// for the backend. The gated shape is what keeps the podman byte-path
// unregressed while proving the microVM portable-error path (divergence 6).
func assertDeliberateKill(t *testing.T, err error, caps backendCaps) {
	t.Helper()
	if err == nil {
		t.Fatal("Kill+Wait returned nil; want a deliberate-kill signal error")
	}
	if !deliberateKill(err, caps) {
		t.Fatalf("Kill+Wait error = %v (%T); want a deliberate-kill signal error for the %s backend", err, err, caps.name)
	}
}
