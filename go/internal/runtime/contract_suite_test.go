package runtime

// The shared ContainerRuntime contract suite (record §U5, the V2b acceptance
// gate): one table-driven body proving MicroVMRuntime and PodmanCLI behave
// identically through the runtime.ContainerRuntime interface, run against BOTH
// backends. It is UNTAGGED (package runtime, no build tag) so it compiles on
// every platform: it references ONLY untagged production symbols plus the
// backendCaps descriptor, never a KVM/podman-only symbol (microvmtest,
// podmanUsable, buildImage, *DuplicateNameError all live in tagged files and are
// reached only through the caps closures). The two entrypoints
// (contract_podman_test.go //go:build podman, contract_microvm_test.go
// //go:build microvm && unix) supply the factory + caps and gate on their
// backend's availability.
//
// The suite drives ContainerRuntime DIRECTLY — a different, lower layer than
// TestPerAgentContainerLifecycle (lifecycle_test.go), which drives
// AgentRuntime.Launch. The two do not overlap.
//
// The 6 conceded divergences (record 580-593) are expressed as per-backend
// capability flags on backendCaps, each asserted by a row, so a divergence that
// silently WIDENS is a test failure. Each row is its own helper so the shared
// runner stays a thin dispatcher.

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// backendCaps is the per-backend expectation descriptor the shared runner
// consumes: a container/session factory plus the flags that encode the 6
// conceded divergences (record 580-593). Every flag toggles the EXACT assertion
// of a row, so a backend that silently widens a divergence fails a row rather
// than passing quietly.
type backendCaps struct {
	// name identifies the backend under test, for subtest / failure messages.
	name string

	// makeSpec builds a ContainerSpec for the backend: the podman leg bakes an
	// Image + a `sleep infinity` keep-alive Command, the microVM leg a
	// /workspace virtio-fs mount; both bake UID 1000. Backend-specific container
	// creation is encapsulated HERE, never in the shared body (record 122-123).
	// t supplies t.TempDir() for a per-session workspace.
	makeSpec func(t *testing.T, name string) ContainerSpec

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

	// gracefulStopPowersOff: a SIGTERM-honoring guest powers off
	// (reboot(RB_POWER_OFF)) before the kill escalation, so Stop completes well
	// under its grace (microVM row 9, record 829-831). When false (podman) a
	// `sleep infinity` PID 1 ignores SIGTERM and Stop burns the full grace, so
	// the row is skipped — the row would prove nothing there.
	gracefulStopPowersOff bool

	// portableKillError encodes divergence 6 (record 590-591): the deliberate-
	// kill error is a portable *ExitStatusError (Signal != 0) on microVM, but
	// the byte-identical podman path yields an *exec.ExitError from a SIGKILLed
	// child (ExitCode() == -1). Both prove the SAME behavioral contract — a
	// deliberate Kill+Wait surfaces as a signalled exit isDeliberateKill accepts
	// — in the two shapes the frozen isDeliberateKill (agent_exec.go:239-258)
	// matches. Asserting a single type across both is impossible without a
	// production change to PodmanCLI, which U5 forbids; the gate keeps the podman
	// byte-path unregressed AND proves the microVM portable-error path.
	portableKillError bool

	// assertDuplicateName asserts the backend's typed name-collision error for a
	// duplicate-name Create (row 7): *DuplicateNameError on microVM, the
	// engine's name-in-use *CommandError on podman. Carried as a closure so the
	// shared body never names *DuplicateNameError (a unix-tagged symbol).
	assertDuplicateName func(t *testing.T, err error)
}

// runContractSuite is called only from the two build-tagged entrypoints
// (contract_podman_test.go, contract_microvm_test.go), so the untagged `unused`
// lint pass (the module's `golangci-lint ./...` lane runs without build tags)
// sees no caller for it or the row helpers it reaches. This blank reference is
// the untagged build's root into the suite graph, marking the whole reachable
// set used; under either build tag the real caller supersedes it.
var _ = runContractSuite

// runContractSuite runs the shared rows against one backend, created via
// newRuntime and described by caps. The stateless exec/stream rows share one
// running container (booted once, amortized on the KVM-gated microVM leg); the
// lifecycle rows (duplicate-name, idempotence, exists, stop-grace) each manage
// their own so an identity/teardown row never perturbs another. A thin
// dispatcher: each row is its own helper.
func runContractSuite(t *testing.T, newRuntime func(t *testing.T) ContainerRuntime, caps backendCaps) {
	t.Helper()
	rt := newRuntime(t)
	primary := startRunning(t, rt, caps, "contract-primary")

	t.Run("exec_exit_codes", func(t *testing.T) { rowExecExitCodes(t, rt, primary) })
	t.Run("exec_stdin", func(t *testing.T) { rowExecStdin(t, rt, primary) })
	t.Run("streaming_stdio", func(t *testing.T) { rowStreamingStdio(t, rt, caps, primary) })
	t.Run("kill_wait_deliberate", func(t *testing.T) { rowKillWait(t, rt, caps, primary) })
	t.Run("ctx_cancel_reaps", func(t *testing.T) { rowCtxCancelReaps(t, rt, primary) })
	t.Run("uid_enforcement", func(t *testing.T) { rowUIDEnforcement(t, rt, caps, primary) })
	t.Run("resize_not_implemented", func(t *testing.T) { rowResize(t, rt, primary) })
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
	if caps.gracefulStopPowersOff {
		t.Run("stop_grace_powers_off", func(t *testing.T) { rowStopGrace(t, rt, caps) })
	}
}

// rowExecExitCodes — row 1 (record 568-569): exit 0 is a success carrying the
// echoed body; a non-zero exit is a SUCCESSFUL call returning the code, NEVER an
// error. A regression that folded a non-zero exit into err would turn every
// expected-failure probe (a denied firewall check) into a fatal.
func rowExecExitCodes(t *testing.T, rt ContainerRuntime, primary ContainerID) {
	t.Helper()
	out, err := rt.Exec(t.Context(), primary, NewExecSpec("sh", "-c", "echo hello-body").AsUser("1000"))
	if err != nil {
		t.Fatalf("Exec(echo): %v", err)
	}
	if !out.Success() {
		t.Fatalf("Exec(echo) exit = %d, stderr = %q, want exit 0", out.ExitCode, out.Stderr)
	}
	if !strings.Contains(out.Stdout, "hello-body") {
		t.Fatalf("Exec(echo) stdout = %q, want it to carry the echoed body", out.Stdout)
	}
	out, err = rt.Exec(t.Context(), primary, NewExecSpec("sh", "-c", "exit 7").AsUser("1000"))
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

// rowExecStdin — row 2 (record 567-569, agent.go:238-246): the script-over-stdin
// shape end to end (the secret-safe channel). `sh -s` reads the script from
// stdin, so the body never appears in the argv / process list.
func rowExecStdin(t *testing.T, rt ContainerRuntime, primary ContainerID) {
	t.Helper()
	out, err := rt.Exec(t.Context(), primary, NewExecSpec("sh", "-s").WithStdin("echo from-stdin").AsUser("1000"))
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
func rowStreamingStdio(t *testing.T, rt ContainerRuntime, caps backendCaps, primary ContainerID) {
	t.Helper()
	stream, err := rt.ExecStreaming(t.Context(), primary, NewStreamingExecSpec("cat").AsUser("1000"))
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

// rowKillWait — row 4 (record 570-575, 631-634): Kill a long sleeper mid-stream,
// drain both pipes in goroutines so the kill/wait never blocks on a full pipe,
// and assert Wait surfaces a deliberate-kill signal error. The error SHAPE is
// capability-gated (portableKillError, divergence 6): microVM yields the
// portable *ExitStatusError, podman the byte-identical *exec.ExitError — both
// prove a signalled exit isDeliberateKill accepts, so the podman byte-path stays
// unregressed AND the microVM portable path works.
func rowKillWait(t *testing.T, rt ContainerRuntime, caps backendCaps, primary ContainerID) {
	t.Helper()
	stream, err := rt.ExecStreaming(t.Context(), primary, NewStreamingExecSpec("sleep", "300").AsUser("1000"))
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

// rowCtxCancelReaps — row 5 (record 823-825, the U2 reap-on-broken-stream rule
// end to end): cancel the ctx passed to ExecStreaming and assert Wait returns —
// no orphan survives a host-side cancel. Wait returning IS the reap signal (Wait
// reaps). A bounded select fails loudly rather than hanging the suite if the
// child is never reaped.
func rowCtxCancelReaps(t *testing.T, rt ContainerRuntime, primary ContainerID) {
	t.Helper()
	cctx, cancel := context.WithCancel(t.Context())
	stream, err := rt.ExecStreaming(cctx, primary, NewStreamingExecSpec("sleep", "300").AsUser("1000"))
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

// rowUIDEnforcement — row 6 (record 576, microvm-runner.md:358-360): a uid-0
// exec is refused on the microVM backend; the podman row asserts its equivalent
// posture — a directed unprivileged exec runs as the requested uid, never
// silently escalated to root.
func rowUIDEnforcement(t *testing.T, rt ContainerRuntime, caps backendCaps, primary ContainerID) {
	t.Helper()
	if caps.refusesRootExec {
		if _, err := rt.Exec(t.Context(), primary, NewExecSpec("id", "-u").AsUser("0")); err == nil {
			t.Fatal("a uid-0 exec must be refused on this backend; got no error")
		}
		return
	}
	out, err := rt.Exec(t.Context(), primary, NewExecSpec("id", "-u").AsUser("1000"))
	if err != nil {
		t.Fatalf("Exec(id -u): %v", err)
	}
	if got := strings.TrimSpace(out.Stdout); got != "1000" {
		t.Fatalf("directed unprivileged exec ran as uid %q, want 1000", got)
	}
}

// rowResize — row 11 (record 577-578): Resize returns ErrResizeNotImplemented on
// both backends until C3. The S1-frozen verb must refuse legibly, never fake a
// limit change that never happened.
func rowResize(t *testing.T, rt ContainerRuntime, primary ContainerID) {
	t.Helper()
	if err := rt.Resize(t.Context(), primary, ResourceLimits{CPUShares: 512}); !errors.Is(err, ErrResizeNotImplemented) {
		t.Fatalf("Resize err = %v, want ErrResizeNotImplemented", err)
	}
}

// rowMountLabel — row 12: "" on microVM (record 587, capability-gated); podman
// returns its real label, which may legitimately be "" on a non-SELinux host, so
// there the row asserts only a no-error read.
func rowMountLabel(t *testing.T, rt ContainerRuntime, caps backendCaps, primary ContainerID) {
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
func rowNonNumericUser(t *testing.T, rt ContainerRuntime, primary ContainerID) {
	t.Helper()
	if _, err := rt.Exec(t.Context(), primary, NewExecSpec("id", "-u").AsUser("not-a-number")); err == nil {
		t.Fatal("a non-numeric ExecSpec.User must be a host-side error on this backend; got no error")
	}
}

// rowOutputCap — divergence 1 (microVM only, OQ-E, record 583-585): an over-cap
// one-shot exec returns the truncation error, not a clipped tail. A backend that
// started truncating silently (widening) would pass a caller a partial output as
// if whole — this row fails that.
func rowOutputCap(t *testing.T, rt ContainerRuntime, primary ContainerID) {
	t.Helper()
	// 9 MiB > the 8 MiB cap; content is irrelevant, only the byte count.
	if _, err := rt.Exec(t.Context(), primary, NewExecSpec("sh", "-c", "head -c 9437184 /dev/zero").AsUser("1000")); err == nil {
		t.Fatal("an over-cap exec must return the truncation error on this backend; got no error")
	}
}

// rowCommandCapAddIgnored — divergence 4 (microVM only, record 588-589):
// spec.Command and spec.CapAdd have NO effect. A session created with a bogus
// Command and an added capability still boots (Command is not the keep-alive),
// still execs, and the workload has an EMPTY capability set (CapAdd granted
// nothing). A backend that started honoring either would widen the divergence.
func rowCommandCapAddIgnored(t *testing.T, rt ContainerRuntime, caps backendCaps) {
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
	out, err := rt.Exec(t.Context(), id, NewExecSpec("echo", "alive").AsUser("1000"))
	if err != nil || !out.Success() || !strings.Contains(out.Stdout, "alive") {
		t.Fatalf("exec must still work on the ignored-Command session: out=%+v err=%v", out, err)
	}
	status, err := rt.Exec(t.Context(), id, NewExecSpec("cat", "/proc/self/status").AsUser("1000"))
	if err != nil || !status.Success() {
		t.Fatalf("reading /proc/self/status: out=%+v err=%v", status, err)
	}
	capEff := ""
	for line := range strings.SplitSeq(status.Stdout, "\n") {
		if rest, ok := strings.CutPrefix(line, "CapEff:"); ok {
			capEff = strings.TrimSpace(rest)
		}
	}
	if capEff != "0000000000000000" {
		t.Fatalf("workload CapEff = %q, want the empty set (spec.CapAdd must grant the workload nothing)", capEff)
	}
}

// rowDuplicateName — row 7 (record 580-581 lineage): a duplicate-name Create is
// refused with the backend's typed collision error keyed on spec.Name. The
// second Create of a live name must fail, and with the expected type (gated via
// caps).
func rowDuplicateName(t *testing.T, rt ContainerRuntime, caps backendCaps) {
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
func rowStopRemoveIdempotence(t *testing.T, rt ContainerRuntime, caps backendCaps) {
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
	if err := rt.Remove(t.Context(), ContainerID("contract-never-created")); err != nil {
		t.Fatalf("Remove of a never-created id must be nil: %v", err)
	}
}

// rowExistsBeforeAfterRemove — row 10 (record 587-590 lineage): Exists is true
// after Create, false after Remove, keyed on spec.Name.
func rowExistsBeforeAfterRemove(t *testing.T, rt ContainerRuntime, caps backendCaps) {
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

// rowStopGrace — row 9 (microVM only, record 829-831): a SIGTERM-honoring guest
// powers off (reboot(RB_POWER_OFF), observed as a VMM exit) WELL BEFORE the kill
// escalation, proving the graceful preamble is not dead weight that always burns
// the full timeout. Observable through the interface as Stop completing far under
// its grace.
func rowStopGrace(t *testing.T, rt ContainerRuntime, caps backendCaps) {
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

// startRunning Creates + Starts a container from caps.makeSpec and registers a
// Remove backstop, the shared happy-path setup for the exec/stream rows. A Create
// or Start failure is fatal (the row cannot run).
func startRunning(t *testing.T, rt ContainerRuntime, caps backendCaps, name string) ContainerID {
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

// registerRemove registers a t.Cleanup Remove backstop for id. The cleanup
// detaches cancellation with context.WithoutCancel so teardown still runs after
// the test's own ctx is cancelled (t.Context() is cancelled before cleanups run)
// — a leaked container would collide with the next run's name (the existing e2e
// pattern, brief §Go house rules).
func registerRemove(t *testing.T, rt ContainerRuntime, id ContainerID, label string) {
	t.Helper()
	t.Cleanup(func() {
		if err := rt.Remove(context.WithoutCancel(t.Context()), id); err != nil {
			t.Errorf("Remove(%s) cleanup: %v", label, err)
		}
	})
}

// deliberateKill reports whether err is the deliberate-kill signal error the
// backend produces after a Kill+Wait (divergence 6): the portable
// *ExitStatusError with a non-zero Signal on microVM, or the byte-identical
// *exec.ExitError from a SIGKILLed child (ExitCode() == -1, i.e. death by
// signal) on podman. ExitCode() is portable across platforms, unlike the
// unix-only syscall.WaitStatus methods, so this untagged file stays
// cross-platform-compilable.
func deliberateKill(err error, caps backendCaps) bool {
	if err == nil {
		return false
	}
	if caps.portableKillError {
		var e *ExitStatusError
		return errors.As(err, &e) && e.Signal != 0
	}
	var e *exec.ExitError
	return errors.As(err, &e) && e.ExitCode() == -1
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
