//go:build linux

package guestd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// maxCapture caps each captured stream (stdout, stderr) of a one-shot Exec at
// 8 MiB (OQ-E). A command that overruns it is an explicit ResourceExhausted
// error, not a silent truncation — the caller must not mistake a clipped tail
// for the whole output. The buffer stops growing at the cap so a runaway child
// cannot exhaust guest memory before the error surfaces.
const maxCapture = 8 << 20

// execState is the supervisor's fail-closed gate: exec is served ONLY in the
// provisioned state, so no exec can run before Provision succeeds (§(b)).
type execState int

const (
	// stateBooting is before net+mount complete; the supervisor is never
	// constructed in this state (run builds it only at the serve step), but it
	// is the zero value so a mis-constructed supervisor fails closed.
	stateBooting execState = iota //nolint:unused // the fail-closed zero value: an unprovisioned supervisor must not sit at stateReady/stateProvisioned; kept as iota anchor even though run never constructs it here
	// stateReady is net+mount succeeded: Health answers, exec is REFUSED.
	stateReady
	// stateProvisioned is post-Provision: exec is accepted.
	stateProvisioned
)

// credentialFunc maps a resolved uid to the process credential a spawned child
// runs under. Production returns a real uid/gid credential (linuxCredential);
// hermetic tests inject one returning nil so a child spawns as the test's own
// uid without needing root to setuid.
type credentialFunc func(uid uint32) *syscall.Credential

// linuxCredential runs a child as the resolved uid with gid == uid, matching the
// baked agent user's (uid,gid) convention. Every child gets an EMPTY capability
// set: guestd sets no ambient and no inheritable caps, so a setuid from guest
// root drops all capabilities (§(b) uid enforcement, egress.go:7-9).
func linuxCredential(uid uint32) *syscall.Credential {
	return &syscall.Credential{Uid: uid, Gid: uid}
}

// armTimeout bounds the in-guest egress arm (§(d), OQ-5), mirroring podman's
// per-command defaultCommandTimeout that bounds the same script on that backend.
const armTimeout = 120 * time.Second

// armStderrTail caps the bytes of the script's combined output carried in a
// failed-arm error so a runaway script cannot balloon the RPC error.
const armStderrTail = 4 << 10

// runNftScript is the production armFunc (§(d)): it spawns the egress script as
// guestd's own root — a spawn path deliberately SEPARATE from exec children,
// with NO syscall.Credential (it never passes through resolveUID/newCredential)
// and never entered in the exec table. The arm is bounded by armTimeout and the
// caller's ctx. On a non-zero exit, timeout, or spawn failure it returns an
// error carrying the exit status and a bounded tail of the combined output.
func runNftScript(ctx context.Context, script string) error {
	ctx, cancel := context.WithTimeout(ctx, armTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script) //nolint:gosec // script is the host-delivered egress ruleset run as guest root by design — this IS the arm surface (§(d))
	// guestd is PID 1 with no PATH, and the arm is a spawn path SEPARATE from
	// exec children (§(d)), so it never inherits mergeEnv's PATH floor. Set the
	// guest rootfs PATH explicitly so the script's bare nft/getent/awk (linked
	// under /bin, guest-image/default.nix) resolve; without it every microVM
	// Start fails "nft: command not found" — the §(e) total-backend outage.
	cmd.Env = []string{"PATH=" + defaultGuestPATH}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if len(out) > armStderrTail {
			out = out[len(out)-armStderrTail:]
		}
		// Only append the output tail when there is one, so a silent failure
		// (e.g. a bare non-zero exit) reads "exit status N", not "exit status N: ".
		if tail := strings.TrimSpace(string(out)); tail != "" {
			return fmt.Errorf("%w: %s", err, tail)
		}
		return err
	}
	return nil
}

// childExec is one running exec: a direct child of guestd (guest PID 1) in its
// own process group. Signal targets the group; the reap that feeds the exit
// frame is the owning ExecStream handler's cmd.Wait.
type childExec struct {
	cmd *exec.Cmd
}

// signalGroup delivers sig to the child's whole process group (negative pid).
// It is best-effort: a group that has already exited yields ESRCH, which is the
// "signal on an exited exec is a no-op success" case — not actionable, so the
// error is intentionally dropped.
func (c *childExec) signalGroup(sig syscall.Signal) {
	if c.cmd.Process == nil {
		return
	}
	// ESRCH (already-reaped group) is the documented no-op-success case; any
	// other failure is equally non-actionable here (the caller returns success
	// per the Signal contract).
	_ = syscall.Kill(-c.cmd.Process.Pid, sig)
}

// supervisor is the full GuestControl handler (§(b)): the booting -> ready ->
// provisioned gate, the session's default exec uid + base env recorded by
// Provision, and the exec table (exec_id -> running child). It replaces V2a's
// Health-only healthService; a successful Health handshake still proves bringup,
// and exec is fail-closed behind Provision.
type supervisor struct {
	// Immutable boot state, set at construction — safe to read lock-free.
	version          string
	netProvisioned   bool
	workspaceMounted bool
	bootNonce        []byte

	// newCredential builds the per-child process credential; a seam so tests
	// spawn as their own uid.
	newCredential credentialFunc

	// armFunc arms egress from a non-empty nft_script (§(d)); a seam so
	// hermetic tests inject a fake arm. Production is runNftScript, which
	// spawns the script as guestd's own root, a spawn path deliberately
	// separate from exec children (never through resolveUID/newCredential).
	armFunc func(ctx context.Context, script string) error

	// stopServing cancels the serving context on an RPC-driven Stop
	// (Signal("", ...)); run wires it and observes rpcStop to drive poweroff.
	stopServing context.CancelFunc
	stopOnce    sync.Once

	mu             sync.Mutex
	state          execState
	defaultExecUID uint32
	baseEnv        map[string]string
	execs          map[string]*childExec
	nextID         uint64
	// captureLimit overrides the one-shot Exec per-stream capture cap; zero
	// means maxCapture. It exists so a hermetic test can drive the overflow
	// branch (OQ-E) with a tiny cap instead of emitting 8 MiB. Read under mu.
	captureLimit int
	// rpcStop records that an RPC Stop (not a Unix signal) cancelled serving,
	// so run ends in reboot(RB_POWER_OFF) rather than a bare PID-1 exit (§(d)).
	rpcStop bool
}

var _ compassv1internalconnect.GuestControlHandler = (*supervisor)(nil)

// Health answers the host handshake with the boot state and echoes the boot
// nonce (§(e)) — a liveness/identity binding, not a secret. It reads only
// immutable fields, so it is safe concurrently with exec and needs no lock.
func (s *supervisor) Health(
	_ context.Context,
	_ *connect.Request[compassv1internal.HealthRequest],
) (*connect.Response[compassv1internal.HealthResponse], error) {
	return connect.NewResponse(&compassv1internal.HealthResponse{
		GuestdVersion:    s.version,
		NetProvisioned:   s.netProvisioned,
		WorkspaceMounted: s.workspaceMounted,
		BootNonce:        s.bootNonce,
	}), nil
}

// Provision transitions ready -> provisioned (§(b)): it records the session's
// default exec uid (validated non-zero) and base env, opening the exec gate.
// When nft_script is non-empty it arms egress in-guest (§(d)): the script runs
// as guestd's own root via armFunc BEFORE the state transition, under s.mu, so
// no exec is served until the arm succeeds. A failed arm returns CodeInternal
// and leaves the gate closed at stateReady (the host tears the VM down). An
// empty nft_script skips the arm (the §(e) hermetic test seam) and opens the
// gate as before.
func (s *supervisor) Provision(
	ctx context.Context,
	req *connect.Request[compassv1internal.ProvisionRequest],
) (*connect.Response[compassv1internal.ProvisionResponse], error) {
	m := req.Msg
	if m.GetDefaultExecUid() == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("default_exec_uid must be non-zero: the guest supervisor never runs an exec as root"))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == stateProvisioned {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("already provisioned"))
	}
	if s.state != stateReady {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("not ready: net/mount bringup incomplete"))
	}
	// Arm egress before opening the gate (§(d), OQ-4: under s.mu). A non-empty
	// script that fails to arm leaves the state at stateReady so requireProvisioned
	// keeps refusing every exec; a retried Provision may run.
	if script := m.GetNftScript(); script != "" {
		if err := s.armFunc(ctx, script); err != nil {
			return nil, connect.NewError(connect.CodeInternal,
				fmt.Errorf("arming nft egress: %w", err))
		}
	}
	s.state = stateProvisioned
	s.defaultExecUID = m.GetDefaultExecUid()
	s.baseEnv = m.GetBaseEnv()
	return connect.NewResponse(&compassv1internal.ProvisionResponse{}), nil
}

// Exec runs one command to completion and returns its captured output (§(b)).
// A non-zero child exit is a SUCCESSFUL response with a non-zero ExitCode, never
// a handler error; a handler error means the exec could not be attempted or
// completed (gate closed, uid 0, spawn failure, timeout, or capture overflow).
// stdin bytes are fed to the child's stdin pipe, never argv, so a script's body
// never appears in the guest process list.
func (s *supervisor) Exec(
	ctx context.Context,
	req *connect.Request[compassv1internal.ExecRequest],
) (*connect.Response[compassv1internal.ExecResponse], error) {
	m := req.Msg
	if err := s.requireProvisioned(); err != nil {
		return nil, err
	}
	uid, err := s.resolveUID(m.Uid)
	if err != nil {
		return nil, err
	}

	// timeout_seconds is enforced guest-side too, so a wedged child cannot
	// outlive its caller's interest even if the host's RPC deadline slips.
	if t := m.GetTimeoutSeconds(); t > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(t)*time.Second)
		defer cancel()
	}

	cmd, err := s.buildChild(m.GetCommand(), uid, m.Workdir, m.GetEnv())
	if err != nil {
		return nil, err
	}
	cmd.Stdin = bytes.NewReader(m.GetStdin())
	limit := s.effectiveCaptureLimit()
	stdout := &cappedBuffer{limit: limit}
	stderr := &cappedBuffer{limit: limit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("spawning exec: %w", err))
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	select {
	case werr := <-waitErr:
		if stdout.over || stderr.over {
			return nil, connect.NewError(connect.CodeResourceExhausted,
				fmt.Errorf("exec output exceeded the %d-byte capture cap", limit))
		}
		code, ok := exitStatus(werr)
		if !ok {
			// A non-ExitError wait failure is a spawn/IO fault, not a command
			// that ran and failed — surface it as a handler error.
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("waiting for exec: %w", werr))
		}
		return connect.NewResponse(&compassv1internal.ExecResponse{
			Stdout:   stdout.Bytes(),
			Stderr:   stderr.Bytes(),
			ExitCode: int32(code), //nolint:gosec // G115: code is a bounded exit status (0-255 or 128+signal), never overflows int32
		}), nil
	case <-ctx.Done():
		// The caller's interest ended before the child exited: SIGKILL the
		// group and reap it so no guest process outlives the RPC (§(b)).
		signalGroupKill(cmd)
		<-waitErr
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, connect.NewError(connect.CodeDeadlineExceeded,
				errors.New("exec exceeded its timeout"))
		}
		return nil, connect.NewError(connect.CodeCanceled, errors.New("exec cancelled"))
	}
}

// ExecStream runs a long-lived command over one bidi stream (§(b)): the first
// frame MUST be StartExec; the response stream is ExecStarted first, interleaved
// stdout/stderr, then exactly one terminal ExecExit emitted from guestd's own
// reap. stdin frames feed the child's stdin pipe; StdinClose half-closes it.
// The child is bound to the stream context: if the stream breaks (host
// disconnect / cancel), guestd SIGKILLs and reaps it so no orphan survives.
func (s *supervisor) ExecStream(
	ctx context.Context,
	stream *connect.BidiStream[compassv1internal.ExecStreamRequest, compassv1internal.ExecStreamResponse],
) error {
	if err := s.requireProvisioned(); err != nil {
		return err
	}

	first, err := stream.Receive()
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("reading start frame: %w", err))
	}
	start := first.GetStart()
	if start == nil {
		return connect.NewError(connect.CodeFailedPrecondition,
			errors.New("first ExecStream frame must be StartExec"))
	}

	uid, err := s.resolveUID(start.Uid)
	if err != nil {
		return err
	}
	cmd, err := s.buildChild(start.GetCommand(), uid, start.Workdir, start.GetEnv())
	if err != nil {
		return err
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("opening stdin pipe: %w", err))
	}
	var sendMu sync.Mutex
	cmd.Stdout = &streamWriter{stream: stream, mu: &sendMu, stdout: true}
	cmd.Stderr = &streamWriter{stream: stream, mu: &sendMu, stdout: false}

	// Acquire the shared send mutex BEFORE cmd.Start so the child's stdout/stderr
	// copier goroutines (spawned by cmd.Start, Sending through this same mutex)
	// cannot emit an output frame ahead of the mandatory first ExecStarted frame
	// (§(b): the response stream is ExecStarted first). A fast child (e.g. `echo`)
	// can otherwise produce output and win the mutex before this goroutine sends
	// Started; streamWriter.Write blocks on this mutex until Started has gone out.
	sendMu.Lock()
	if err := cmd.Start(); err != nil {
		sendMu.Unlock()
		return connect.NewError(connect.CodeInternal, fmt.Errorf("spawning exec: %w", err))
	}

	id := s.register(cmd)
	defer s.unregister(id)

	startErr := stream.Send(&compassv1internal.ExecStreamResponse{
		Frame: &compassv1internal.ExecStreamResponse_Started{
			Started: &compassv1internal.ExecStarted{ExecId: id},
		},
	})
	sendMu.Unlock()
	if startErr != nil {
		signalGroupKill(cmd)
		_ = cmd.Wait() // reap the child we could not report; the stream is already broken
		return connect.NewError(connect.CodeInternal, fmt.Errorf("sending started frame: %w", startErr))
	}

	// Receive loop: stdin frames feed the child's stdin, StdinClose half-closes
	// it. A clean half-close (io.EOF after the client's CloseRequest) ends the
	// loop without killing the child — a one-shot-style stream that sent all its
	// input still runs to completion. Any OTHER receive error is a broken stream
	// (host disconnect / ctx cancel): close disconnected so the wait select
	// kills and reaps the bound child, since connect does not reliably cancel
	// the server ctx on a client-side cancel over this transport.
	disconnected := make(chan struct{})
	go func() {
		defer close(disconnected)
		for {
			msg, rerr := stream.Receive()
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					// Clean end of the request stream (client CloseRequest):
					// no more stdin, so close the child's stdin pipe (a
					// stdin-reading child now sees EOF). This is NOT a broken
					// stream, so do not trigger the disconnect kill — wait until
					// the stream actually breaks or the child exits.
					_ = stdin.Close()
					<-ctx.Done()
				}
				return
			}
			switch {
			case msg.GetStdin() != nil:
				// A short write is a dead child pipe; the reap will report the
				// exit, so this write failure is not independently actionable.
				_, _ = stdin.Write(msg.GetStdin())
			case msg.GetStdinClose() != nil:
				_ = stdin.Close() // half-close; the child sees stdin EOF
			}
		}
	}()

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	var werr error
	select {
	case werr = <-waitErr:
	case <-disconnected:
		// Broken stream: kill the group and reap so no orphan survives to VM
		// teardown; the terminal frame still comes from our reap.
		signalGroupKill(cmd)
		werr = <-waitErr
	case <-ctx.Done():
		signalGroupKill(cmd)
		werr = <-waitErr
	}

	code, sig := exitStatusSignal(werr)
	sendMu.Lock()
	// The exit frame ALWAYS comes from our reap so the host's Wait never hangs;
	// on a broken stream the Send fails harmlessly (the host already knows).
	_ = stream.Send(&compassv1internal.ExecStreamResponse{
		Frame: &compassv1internal.ExecStreamResponse_Exit{
			Exit: &compassv1internal.ExecExit{ExitCode: int32(code), Signal: int32(sig)}, //nolint:gosec // G115: code is a bounded exit status (0-255 or 128+signal) and sig is a small signal number — neither overflows int32
		},
	})
	sendMu.Unlock()
	return nil
}

// Signal delivers a signal to a running exec's process group by exec_id; an
// empty exec_id targets the guest itself (graceful Stop, §(d)). Signalling an
// unknown or already-exited exec is a no-op success.
//
// For an empty exec_id the signal value forwards to the running children, but
// the STOP DECISION is unconditional: any empty-exec_id Signal sets rpcStop and
// tears the guest down (initiateStop → serve drain → power-off), regardless of
// which signal was sent. The host only ever sends SIGTERM here (§(d) "SIGTERM
// for Stop"); the field is not a per-signal switch on the guest side, so a
// future caller must not expect Signal("", SIGUSR1) to mean anything narrower
// than "stop the guest".
func (s *supervisor) Signal(
	_ context.Context,
	req *connect.Request[compassv1internal.SignalRequest],
) (*connect.Response[compassv1internal.SignalResponse], error) {
	m := req.Msg
	sig := syscall.Signal(m.GetSignal())

	if m.GetExecId() == "" {
		// signal value forwarded to children; the stop decision is unconditional.
		s.initiateStop(sig)
		return connect.NewResponse(&compassv1internal.SignalResponse{}), nil
	}

	s.mu.Lock()
	child := s.execs[m.GetExecId()]
	s.mu.Unlock()
	if child == nil {
		// Unknown or already-reaped exec: no-op success (§(b)).
		return connect.NewResponse(&compassv1internal.SignalResponse{}), nil
	}
	child.signalGroup(sig)
	return connect.NewResponse(&compassv1internal.SignalResponse{}), nil
}

// requireProvisioned is the exec gate: Exec/ExecStream are refused with a typed
// failed-precondition error until Provision succeeds (§(b)).
func (s *supervisor) requireProvisioned() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != stateProvisioned {
		return connect.NewError(connect.CodeFailedPrecondition,
			errors.New("exec refused: session is not provisioned"))
	}
	return nil
}

// resolveUID resolves the exec's effective uid: an absent uid falls back to the
// session default recorded by Provision; a uid of 0 is REFUSED before any spawn
// (§(b) uid enforcement — the supervisor never runs an exec as root).
func (s *supervisor) resolveUID(uid *uint32) (uint32, error) {
	var u uint32
	if uid == nil {
		s.mu.Lock()
		u = s.defaultExecUID
		s.mu.Unlock()
	} else {
		u = *uid
	}
	if u == 0 {
		return 0, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("exec uid 0 is refused: the guest supervisor never runs an exec as root"))
	}
	return u, nil
}

// effectiveCaptureLimit is the one-shot Exec per-stream capture cap: the
// injected captureLimit when a test set one, else maxCapture. Read under mu
// because captureLimit shares the mutable-field lock domain.
func (s *supervisor) effectiveCaptureLimit() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.captureLimit > 0 {
		return s.captureLimit
	}
	return maxCapture
}

// mergeEnv assembles the child's environment: the session base env overlaid by
// the exec's own env (exec keys win, §(b) env base). The result is sorted so it
// is deterministic for logging/tests — ordering is otherwise irrelevant.
func (s *supervisor) mergeEnv(execEnv map[string]string) []string {
	s.mu.Lock()
	merged := make(map[string]string, len(s.baseEnv)+len(execEnv)+1)
	maps.Copy(merged, s.baseEnv)
	s.mu.Unlock()
	maps.Copy(merged, execEnv)
	// Floor a default search PATH when neither the base env nor the exec env
	// carries one, so a bare argv[0] resolves (the container-baked-PATH analog,
	// §(b)). A caller-supplied PATH — even empty — is kept verbatim.
	if _, ok := merged["PATH"]; !ok {
		merged["PATH"] = defaultGuestPATH
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

// buildChild constructs the exec's *exec.Cmd: a direct child in its own process
// group (Setpgid), run under the resolved uid's credential with an empty cap
// set. stdin/stdout/stderr are the caller's to wire — this only fixes the
// spawn-shape invariants common to one-shot and streaming exec.
func (s *supervisor) buildChild(argv []string, uid uint32, workdir *string, env map[string]string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("exec command is empty: command[0] is the program"))
	}
	merged := s.mergeEnv(env)
	prog, resolveErr := resolveProgram(argv[0], merged)
	if resolveErr != nil {
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("resolving exec program %q: %w", argv[0], resolveErr))
	}
	// exec.Command resolves a bare argv[0] against guestd's OWN process $PATH,
	// but guestd is PID 1 with no PATH; override Path with the session-env
	// resolution above and clear the (now-irrelevant) process-PATH lookup error.
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // argv is the caller's command by design — this IS the exec surface
	cmd.Path = prog
	cmd.Err = nil
	if workdir != nil {
		cmd.Dir = *workdir
	}
	cmd.Env = merged
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:    true,
		Credential: s.newCredential(uid),
	}
	return cmd, nil
}

// defaultGuestPATH is floored into an exec's environment when neither the
// session base env nor the exec's own env supplies a PATH. It mirrors the PATH
// a container image bakes in (podman resolves a bare command against the image
// PATH): the guest rootfs userland lives under these directories
// (guest-image/default.nix), so a bare `sh`/`echo`/`compass-agent` resolves
// without every caller having to spell out a PATH.
const defaultGuestPATH = "/bin:/usr/bin:/sbin:/usr/sbin"

// resolveProgram finds the executable for a bare argv[0] using the search PATH
// carried in the merged session env, NOT guestd's own process env. guestd runs
// as PID 1 with no PATH, so Go's exec.LookPath (which reads os.Environ) can
// never resolve a bare command — resolution must honor the session env, the
// same PATH the child runs with (§(b), the container-image-PATH analog). A name
// containing a slash is used as given. Returns the resolved path, or an error
// if no executable is found on the session PATH.
func resolveProgram(file string, env []string) (string, error) {
	if strings.ContainsRune(file, '/') {
		return file, nil
	}
	var pathEnv string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			pathEnv = v
			break
		}
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		if candidate := filepath.Join(dir, file); isExecutable(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable file %q not found in $PATH", file)
}

// isExecutable reports whether path is a regular file with an execute bit set.
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

// initiateStop is the RPC-driven shutdown trigger (§(d)): it SIGTERMs every
// running exec, then cancels the serving context so serveHandshake drains. run
// observes rpcStop after serve returns and ends in reboot(RB_POWER_OFF) — a
// PID-1 exit would panic the kernel, which the VMM never sees as a guest exit.
func (s *supervisor) initiateStop(sig syscall.Signal) {
	s.mu.Lock()
	s.rpcStop = true
	children := make([]*childExec, 0, len(s.execs))
	for _, c := range s.execs {
		children = append(children, c)
	}
	s.mu.Unlock()

	for _, c := range children {
		c.signalGroup(sig)
	}
	s.stopOnce.Do(func() {
		if s.stopServing != nil {
			s.stopServing()
		}
	})
}

// register adds a running child to the exec table under a fresh exec_id.
func (s *supervisor) register(cmd *exec.Cmd) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("exec-%d", s.nextID)
	s.execs[id] = &childExec{cmd: cmd}
	return id
}

// unregister drops a reaped child from the exec table, so a later Signal on its
// exec_id is a no-op success.
func (s *supervisor) unregister(id string) {
	s.mu.Lock()
	delete(s.execs, id)
	s.mu.Unlock()
}

// signalGroupKill SIGKILLs a child's whole process group (negative pid),
// best-effort — an already-exited group yields ESRCH, which is fine here.
func signalGroupKill(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// The child is being force-reaped; a kill failure (ESRCH on an already-dead
	// group) is not actionable — the following Wait reaps it regardless.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// exitStatus extracts a one-shot exit code from a cmd.Wait error. ok is false
// when the error is not a process exit (a spawn/IO fault the caller must surface
// as a handler error rather than an exit code). A nil error is exit 0.
func exitStatus(werr error) (code int, ok bool) {
	if werr == nil {
		return 0, true
	}
	var ee *exec.ExitError
	if !errors.As(werr, &ee) {
		return 0, false
	}
	if ws, wok := ee.Sys().(syscall.WaitStatus); wok {
		if ws.Signaled() {
			return 128 + int(ws.Signal()), true
		}
		return ws.ExitStatus(), true
	}
	return ee.ExitCode(), true
}

// exitStatusSignal extracts the (exit_code, signal) pair for a stream's terminal
// ExecExit frame. A signalled child reports both the signal and the 128+signal
// exit code (shell convention); a normal exit reports code with signal 0.
func exitStatusSignal(werr error) (code int, sig syscall.Signal) {
	if werr == nil {
		return 0, 0
	}
	var ee *exec.ExitError
	if !errors.As(werr, &ee) {
		// A non-exit wait fault (e.g. a stream write error killed the copy):
		// report a generic failure code, no signal.
		return -1, 0
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return 128 + int(ws.Signal()), ws.Signal()
		}
		return ws.ExitStatus(), 0
	}
	return ee.ExitCode(), 0
}

// cappedBuffer accumulates output up to limit bytes, then drops the rest and
// flags over. Bounding growth keeps a runaway child from exhausting guest
// memory; the over flag lets the caller return an explicit overflow error
// instead of silently truncating (OQ-E).
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.over {
		return len(p), nil
	}
	room := c.limit - c.buf.Len()
	if len(p) <= room {
		return c.buf.Write(p)
	}
	if room > 0 {
		// Partial buffer write of a []byte never errors (bytes.Buffer.Write
		// always returns a nil error), so the byte count is authoritative.
		_, _ = c.buf.Write(p[:room])
	}
	c.over = true
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }

// streamWriter turns a child's stdout/stderr into interleaved ExecStream frames,
// serialized under a shared mutex (connect's Send is not concurrency-safe). It
// copies each chunk because os/exec reuses its read buffer across Writes.
type streamWriter struct {
	stream *connect.BidiStream[compassv1internal.ExecStreamRequest, compassv1internal.ExecStreamResponse]
	mu     *sync.Mutex
	stdout bool
}

func (w *streamWriter) Write(p []byte) (int, error) {
	b := make([]byte, len(p))
	copy(b, p)
	var frame compassv1internal.ExecStreamResponse
	if w.stdout {
		frame.Frame = &compassv1internal.ExecStreamResponse_Stdout{Stdout: b}
	} else {
		frame.Frame = &compassv1internal.ExecStreamResponse_Stderr{Stderr: b}
	}
	w.mu.Lock()
	err := w.stream.Send(&frame)
	w.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return len(p), nil
}
