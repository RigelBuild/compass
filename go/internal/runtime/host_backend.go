// host_backend.go is the host-process WorkloadRuntime backend: the lowest
// substrate tier, running each agent as a direct child process of the Runner
// under the Runner's own uid, with no container, VM, or kernel boundary. It is
// a single-trust-domain tier — the operator is the only principal on the box —
// so several of the nine WorkloadRuntime methods are degenerate by
// construction, and each such method documents that at its definition.
//
// The model is a set of per-agent HANDLES keyed by workload id. A handle owns a
// private 0700 state dir (workspace root, home overlay dir, socket dir) and,
// once ExecStreaming launches the agent, the spawned child's process group. The
// handle map is mutex-guarded because the Runner calls these methods
// concurrently.
//
// WorkloadSpec field map on this backend:
//   - Name        honored: the handle's stable name (Exists lookup key).
//   - Env         ignored here: Exec/ExecStreaming take their environment
//                 from the ExecSpec, and the launch leg that would apply this
//                 field does not exist yet.
//   - UID         interpreted: the process runs as the Runner's own euid; the
//                 AsUser rule (below) enforces that, so this field is not a
//                 second uid source here.
//   - Image       ignored: there is no image to run.
//   - CapAdd       ignored: a host child carries the Runner's own capabilities;
//                 none are added or dropped here.
//   - Mounts      ignored: a host process has no bind mounts; the agent reads
//                 and writes the real host filesystem directly.
//   - Command     ignored: the long-lived agent is launched by ExecStreaming
//                 with its own command, not a container entrypoint.
//   - Egress      ignored: egress is UNENFORCED on this tier. There is no
//                 per-workload firewall and nothing here arms one, so this
//                 backend never claims otherwise.

package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec" //nolint:depguard // host backend seam: *exec.Cmd/*exec.ExitError drive the direct host subprocess this backend exists to run
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrResizeUnsupportedOnHost is returned by HostRuntime.Resize: the host
// backend owns no cgroup, so there is no live resource limit to change. It is a
// permanent unsupported (distinct from ErrResizeNotImplemented's "reserved
// until C3"), returned rather than a silent success so a caller that believes
// it resized is never lied to.
var ErrResizeUnsupportedOnHost = errors.New("runtime: WorkloadRuntime.Resize is unsupported on the host backend (no cgroup ownership)")

// defaultHostStateRoot is where HostRuntime handles keep their per-agent state
// dirs when SelectBackend builds the backend with no explicit root. This is a
// shared tmpdir: MkdirAll will not tighten an existing dir, so on a multi-user
// box a local user could pre-create it with looser permissions. Deployments
// pass their own private root instead.
func defaultHostStateRoot() string {
	return filepath.Join(os.TempDir(), "compass-host")
}

// UnsupportedUserError is an ExecSpec/StreamingExecSpec AsUser naming a uid the
// host backend cannot honor. A host child runs as the Runner's own effective
// uid, so the only accepted value is that euid; any other uid means the caller
// believes a user switch happened, which the host tier cannot provide, so it is
// rejected rather than run wrong.
type UnsupportedUserError struct {
	Requested string
	Euid      string
}

func (e *UnsupportedUserError) Error() string {
	return fmt.Sprintf("runtime: host backend cannot run as user %q: it runs as the Runner's own euid %q", e.Requested, e.Euid)
}

// hostState is a handle's lifecycle: created by Create, advanced to started by
// Start. It is bookkeeping, not a running boundary — the agent process is a
// separate ExecStreaming child.
type hostState int

const (
	hostCreated hostState = iota
	hostStarted
)

// hostProcess tracks the process group of a live streaming exec. pgid is the
// group leader's pid (the child is its own group leader via Setpgid); done
// closes when the reaper has waited the leader, after which waitErr is safe to
// read.
type hostProcess struct {
	pgid    int
	done    chan struct{}
	waitErr error
}

// hostHandle is one per-agent workload: its synthetic id and name, its private
// state dir, its lifecycle state, and the live streaming process (nil until
// ExecStreaming spawns it).
type hostHandle struct {
	id       WorkloadID
	name     string
	stateDir string
	state    hostState
	proc     *hostProcess
}

// HostRuntime runs each agent as a direct host child process under the Runner's
// own uid. It owns a mutex-guarded map of per-agent handles; euid is the
// Runner's effective uid captured at construction, the only uid AsUser accepts.
type HostRuntime struct {
	stateRoot string
	euid      int
	timeout   time.Duration
	mu        sync.Mutex
	handles   map[WorkloadID]*hostHandle
	// afterSpawn, when set by a test, runs between the spawn and the handle
	// record in ExecStreaming. Nil in production.
	afterSpawn func()
}

var _ WorkloadRuntime = (*HostRuntime)(nil)

// NewHostRuntime builds a HostRuntime keeping per-agent state dirs under
// stateRoot, capturing the Runner's effective uid as the only uid its execs may
// run as.
func NewHostRuntime(stateRoot string) *HostRuntime {
	return &HostRuntime{
		stateRoot: stateRoot,
		euid:      os.Geteuid(),
		timeout:   defaultCommandTimeout,
		handles:   make(map[WorkloadID]*hostHandle),
	}
}

// WithTimeout overrides the per-command wall-clock cap Exec applies.
func (h *HostRuntime) WithTimeout(timeout time.Duration) *HostRuntime {
	h.timeout = timeout
	return h
}

// Create allocates a per-agent handle: it mints a synthetic WorkloadID and
// creates the handle's private 0700 state-dir tree (workspace root, home
// overlay dir, socket dir). No process is spawned. A duplicate name is refused
// under the same lock that inserts, so two concurrent Creates of one name
// cannot both pass. See the file header for the WorkloadSpec field map.
func (h *HostRuntime) Create(_ context.Context, spec WorkloadSpec) (WorkloadID, error) {
	id, err := mintSessionID()
	if err != nil {
		return "", err
	}
	stateDir := filepath.Join(h.stateRoot, "host", string(id))
	for _, sub := range []string{"", "workspace", "home", "socket"} {
		if mkErr := os.MkdirAll(filepath.Join(stateDir, sub), 0o700); mkErr != nil {
			if rmErr := os.RemoveAll(stateDir); rmErr != nil {
				return "", errors.Join(fmt.Errorf("runtime: host creating state dir: %w", mkErr), fmt.Errorf("runtime: host cleaning up refused state dir %s: %w", stateDir, rmErr))
			}
			return "", fmt.Errorf("runtime: host creating state dir: %w", mkErr)
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, existing := range h.handles {
		if existing.name == spec.Name {
			rmErr := os.RemoveAll(stateDir)
			dupErr := fmt.Errorf("runtime: host workload named %q already exists", spec.Name)
			if rmErr != nil {
				return "", errors.Join(dupErr, fmt.Errorf("runtime: host cleaning up refused state dir %s: %w", stateDir, rmErr))
			}
			return "", dupErr
		}
	}
	h.handles[id] = &hostHandle{
		id:       id,
		name:     spec.Name,
		stateDir: stateDir,
		state:    hostCreated,
	}
	return id, nil
}

// Start transitions the handle created → started and validates its state dir.
// Bookkeeping only: there is no init process to launch (the agent is a later
// ExecStreaming child), so a "started" host handle is not a running boundary.
func (h *HostRuntime) Start(_ context.Context, id WorkloadID) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	handle, ok := h.handles[id]
	if !ok {
		return fmt.Errorf("runtime: host workload %q does not exist", id)
	}
	info, err := os.Stat(handle.stateDir)
	if err != nil {
		return fmt.Errorf("runtime: host validating state dir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("runtime: host state path %q is not a directory", handle.stateDir)
	}
	handle.state = hostStarted
	return nil
}

// Exec runs the command as a direct host subprocess of the Runner, under the
// Runner's own uid, honoring ExecSpec's command/env/workdir/stdin under the
// per-command timeout. A non-zero exit is a successful call returning
// ExecOutput.ExitCode; only a spawn failure or a timeout is a Go error. The
// child inherits the Runner's environment with ExecSpec.Env overriding per key
// — there is no image here to supply a baseline. See checkUser for the AsUser
// rule.
func (h *HostRuntime) Exec(ctx context.Context, id WorkloadID, spec ExecSpec) (ExecOutput, error) {
	if err := h.checkUser(spec.User); err != nil {
		return ExecOutput{}, err
	}
	if _, err := h.startedHandle(id); err != nil {
		return ExecOutput{}, err
	}
	if len(spec.Command) == 0 {
		return ExecOutput{}, errors.New("runtime: host exec requires a command")
	}

	cctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	//nolint:gosec // G204: the host backend seam spawns the Runner-assembled agent command; the argv is not attacker-controlled.
	cmd := exec.CommandContext(cctx, spec.Command[0], spec.Command[1:]...)
	cmd.WaitDelay = 10 * time.Second
	cmd.Env = envSlice(spec.Env)
	if spec.Workdir != nil {
		cmd.Dir = *spec.Workdir
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if spec.Stdin != nil {
		cmd.Stdin = strings.NewReader(*spec.Stdin)
	}

	if runErr := cmd.Run(); runErr != nil {
		switch {
		case cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil:
			// This call's own timeout fired: the process was killed, so surface
			// a timeout rather than a bogus exit code.
			return ExecOutput{}, &TimeoutError{Summary: "host exec", Timeout: h.timeout}
		case ctx.Err() != nil:
			return ExecOutput{}, ctx.Err()
		default:
			if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
				// Ran to completion but exited non-zero: a successful call.
				return ExecOutput{Stdout: out.String(), Stderr: errBuf.String(), ExitCode: exitErr.ExitCode()}, nil
			}
			if errors.Is(runErr, exec.ErrWaitDelay) {
				// The command itself exited; a leaked grandchild held the output
				// pipe past that exit. The exit status is real, so report it with
				// what was captured rather than losing it to a spawn error.
				return ExecOutput{Stdout: out.String(), Stderr: errBuf.String(), ExitCode: cmd.ProcessState.ExitCode()}, nil
			}
			return ExecOutput{}, &SpawnError{Program: spec.Command[0], Err: runErr}
		}
	}
	return ExecOutput{Stdout: out.String(), Stderr: errBuf.String(), ExitCode: 0}, nil
}

// ExecStreaming spawns the agent as a host child in its own process group,
// stdio piped, bound to ctx so cancelling it terminates the process. This is
// the one clean mapping — where the host-tier agent comes to life. No
// wall-clock timeout: the process is meant to run indefinitely. Same AsUser
// rule as Exec.
func (h *HostRuntime) ExecStreaming(ctx context.Context, id WorkloadID, spec StreamingExecSpec) (*StreamingExec, error) {
	if err := h.checkUser(spec.User); err != nil {
		return nil, err
	}
	handle, err := h.startedHandle(id)
	if err != nil {
		return nil, err
	}
	if len(spec.Command) == 0 {
		return nil, errors.New("runtime: host streaming exec requires a command")
	}

	execCtx, cancel := context.WithCancel(ctx)
	//nolint:gosec // G204: the host backend seam spawns the Runner-assembled agent command; the argv is not attacker-controlled.
	cmd := exec.CommandContext(execCtx, spec.Command[0], spec.Command[1:]...)
	// Own process group so Stop/Remove can signal the whole tree, and ctx
	// cancellation SIGKILLs that group rather than the leader alone.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	cmd.Env = envSlice(spec.Env)
	if spec.Workdir != nil {
		cmd.Dir = *spec.Workdir
	}

	// os.Pipe (not StdoutPipe) so the reaper goroutine can Wait without racing
	// the caller's reads: exec closes only pipes it created, so these stay under
	// the caller's ownership.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, &SpawnError{Program: spec.Command[0], Err: err}
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, errors.Join(&SpawnError{Program: spec.Command[0], Err: err}, closePipes(stdinR, stdinW))
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, errors.Join(&SpawnError{Program: spec.Command[0], Err: err}, closePipes(stdinR, stdinW, stdoutR, stdoutW))
	}
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	if startErr := cmd.Start(); startErr != nil {
		cancel()
		return nil, errors.Join(&SpawnError{Program: spec.Command[0], Err: startErr}, closePipes(stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW))
	}
	// Close the child-side ends in the parent so the caller sees EOF when the
	// child exits and closes its own copies.
	if closeErr := closePipes(stdinR, stdoutW, stderrW); closeErr != nil {
		termErr := cmd.Cancel()
		waitErr := cmd.Wait()
		cancel()
		return nil, errors.Join(&SpawnError{Program: spec.Command[0], Err: closeErr}, termErr, waitErr)
	}

	proc := &hostProcess{pgid: cmd.Process.Pid, done: make(chan struct{})}
	go func() {
		proc.waitErr = cmd.Wait()
		close(proc.done)
	}()
	// Test seam: lets a test occupy the gap between the spawn and the record
	// below, which is otherwise a lock-free window no caller can time.
	if h.afterSpawn != nil {
		h.afterSpawn()
	}

	// The child is already running, so a Remove that landed during the spawn
	// would have found no process to kill. Detect that and reap our own child
	// rather than hand back a live workload the backend can no longer reach.
	h.mu.Lock()
	if _, live := h.handles[id]; !live {
		h.mu.Unlock()
		cancel()
		<-proc.done
		return nil, errors.Join(
			fmt.Errorf("runtime: host workload %q was removed during spawn", id),
			closePipes(stdinW, stdoutR, stderrR),
		)
	}
	handle.proc = proc
	h.mu.Unlock()

	kill := func() error {
		cancel()
		return killGroup(proc.pgid, syscall.SIGKILL)
	}
	wait := func() error {
		<-proc.done
		return proc.waitErr
	}
	return &StreamingExec{
		IO:      StreamingIO{Stdin: stdinW, Stdout: stdoutR, Stderr: stderrR},
		Process: newChildHandleFuncs(kill, wait),
	}, nil
}

// Stop signals the handle's process group: SIGTERM, wait up to timeout, then
// SIGKILL. Scope is the group the backend spawned — a process the agent
// double-forked out of that group is NOT reliably stopped (no cgroup freezer in
// v1). Stopping a handle with no live process is a no-op.
func (h *HostRuntime) Stop(ctx context.Context, id WorkloadID, timeout time.Duration) error {
	proc := h.liveProcess(id)
	if proc == nil {
		return nil
	}
	if termErr := killGroup(proc.pgid, syscall.SIGTERM); termErr != nil {
		return termErr
	}
	select {
	case <-proc.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(timeout):
		if killErr := killGroup(proc.pgid, syscall.SIGKILL); killErr != nil {
			return killErr
		}
		<-proc.done
		return nil
	}
}

// Remove force-kills the process group if still live, then deletes the handle's
// state dir. It does NOT touch anything outside that dir: the agent's writes to
// the real host filesystem are permanent, the tier's declared posture, not a
// cleanup bug. Removing an unknown id is a no-op, for idempotent teardown.
func (h *HostRuntime) Remove(_ context.Context, id WorkloadID) error {
	h.mu.Lock()
	handle, ok := h.handles[id]
	// Capture the process in the same critical section that drops the handle:
	// ExecStreaming writes handle.proc under this lock, so reading it after
	// unlocking both races and can miss a child that is about to be spawned.
	var proc *hostProcess
	if ok {
		proc = handle.proc
		delete(h.handles, id)
	}
	h.mu.Unlock()
	if !ok {
		return nil
	}
	if proc != nil {
		select {
		case <-proc.done:
		default:
			if killErr := killGroup(proc.pgid, syscall.SIGKILL); killErr != nil {
				return killErr
			}
			<-proc.done
		}
	}
	return os.RemoveAll(handle.stateDir)
}

// Exists reports whether the backend holds a handle under this name.
// Degenerate: there is no container registry, so existence is handle-existence,
// not container-existence — a crashed agent whose state dir remains still
// Exists, mirroring a stopped-but-not-removed container.
func (h *HostRuntime) Exists(_ context.Context, name string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, handle := range h.handles {
		if handle.name == name {
			return true, nil
		}
	}
	return false, nil
}

// MountLabel returns "", nil. Degenerate: there is no container and no
// per-container SELinux MCS category, so there is no label to relabel a config
// dir into.
func (h *HostRuntime) MountLabel(_ context.Context, _ WorkloadID) (string, error) {
	return "", nil
}

// Resize returns ErrResizeUnsupportedOnHost. Degenerate: the host backend owns
// no cgroup, so it never fakes a limit change that did not happen.
func (h *HostRuntime) Resize(_ context.Context, _ WorkloadID, _ ResourceLimits) error {
	return ErrResizeUnsupportedOnHost
}

// checkUser enforces the host AsUser rule: nil runs as the Runner's own euid,
// the euid as a numeric string is accepted, and any other uid is rejected — a
// host child cannot switch user, so accepting a different uid would run the
// caller's command wrong under a uid it did not ask for.
func (h *HostRuntime) checkUser(user *string) error {
	if user == nil {
		return nil
	}
	euid := strconv.Itoa(h.euid)
	if *user == euid {
		return nil
	}
	return &UnsupportedUserError{Requested: *user, Euid: euid}
}

// startedHandle returns the handle for id, erroring if it is unknown or not yet
// started.
func (h *HostRuntime) startedHandle(id WorkloadID) (*hostHandle, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	handle, ok := h.handles[id]
	if !ok {
		return nil, fmt.Errorf("runtime: host workload %q does not exist", id)
	}
	if handle.state != hostStarted {
		return nil, fmt.Errorf("runtime: host workload %q is not started", id)
	}
	return handle, nil
}

// liveProcess returns id's process if the handle exists and has one, else nil.
func (h *HostRuntime) liveProcess(id WorkloadID) *hostProcess {
	h.mu.Lock()
	defer h.mu.Unlock()
	handle, ok := h.handles[id]
	if !ok {
		return nil
	}
	return handle.proc
}

// killGroup signals the process group led by pgid. An already-gone group
// (ESRCH) is success — the process the signal targets has already exited.
func killGroup(pgid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("runtime: host signalling process group %d: %w", pgid, err)
	}
	return nil
}

// closePipes closes every file, joining any close errors.
func closePipes(files ...*os.File) error {
	var err error
	for _, f := range files {
		err = errors.Join(err, f.Close())
	}
	return err
}

// envSlice renders the child's environment: the Runner's own environment as the
// baseline, with env overriding it per key. A host child has no image to supply
// a baseline, so without inheritance even PATH is unset and an unqualified
// command cannot resolve. The agent already runs in the operator's own trust
// domain, so an isolated environment here would be a posture the tier does not
// actually have.
func envSlice(env map[string]string) []string {
	merged := make(map[string]string, len(env))
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			merged[k] = v
		}
	}
	maps.Copy(merged, env)
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}
