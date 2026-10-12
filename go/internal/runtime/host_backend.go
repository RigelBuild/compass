// The host-process WorkloadRuntime backend: the lowest substrate tier, running
// each agent as a direct child of the Runner under the Runner's own uid, no
// container/VM/kernel boundary. Single-trust-domain (the operator is the only
// principal), so several of the nine methods are degenerate — documented each.

// The model is per-agent HANDLES keyed by workload id, each owning a private
// 0700 state dir and, once ExecStreaming launches, the child's process group.
// The handle map is mutex-guarded (the Runner calls these concurrently).

// WorkloadSpec fields: Name is honored (stable-name lookup key) and UID is
// interpreted as the Runner's own euid (AsUser enforces it). Env, Image, CapAdd,
// Mounts, Command, and Egress are all ignored — a host child carries the
// Runner's own capabilities, has no image/mounts, and egress is UNENFORCED here.

package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

// WorkspaceUID resolves the uid host-backend agents run as: the Runner's own
// effective uid captured at construction — the same h.euid checkUser enforces —
// so the uid handed to each workspace is exactly the uid its execs will be
// accepted under. os.Geteuid returns -1 where the syscall is unavailable; a
// blind uint32 conversion would wrap it to a huge uid, so the negative case is
// refused before any narrowing. The error wording is stable so the startup
// failure text does not regress.
func (h *HostRuntime) WorkspaceUID() (uint32, error) {
	if h.euid < 0 {
		return 0, fmt.Errorf("host backend: geteuid returned %d, no usable effective uid to run agents as", h.euid)
	}
	return uint32(h.euid), nil //nolint:gosec // G115: euid is non-negative here (the < 0 case returned above), so the narrowing cannot wrap.
}

// EgressUnenforced marks this tier as unable to constrain egress. A host child
// shares the host's network namespace, so there is no boundary to firewall —
// the nftables arm the container tiers run has nothing to attach to here. This
// reports the absence of enforcement, never that enforcement happened
// elsewhere.
func (h *HostRuntime) EgressUnenforced() bool {
	return true
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

// Exec runs a command as a direct host child under the Runner's uid and
// timeout. Missing programs return 127; programs that cannot execute return
// 126. A missing or non-directory workdir or missing interpreter is an error.
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

	if err := ctx.Err(); err != nil {
		return ExecOutput{}, err
	}
	childEnv := envSlice(spec.Env)
	if code, stderr, ok := hostExecPrecheck(spec.Command[0], spec.Workdir, childEnv); ok {
		return ExecOutput{Stderr: stderr, ExitCode: code}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	cmd := hostExecCommand(cctx, spec.Command, spec.Workdir, childEnv)
	cmd.WaitDelay = 10 * time.Second
	cmd.Env = childEnv
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
			executablePath := cmd.Path
			if code, stderr, ok := hostExecStartFailure(spec.Command[0], spec.Workdir, executablePath, runErr); ok {
				return ExecOutput{Stderr: stderr, ExitCode: code}, nil
			}
			if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
				return ExecOutput{Stdout: out.String(), Stderr: errBuf.String(), ExitCode: exitErr.ExitCode()}, nil
			}
			if errors.Is(runErr, exec.ErrWaitDelay) {
				return ExecOutput{Stdout: out.String(), Stderr: errBuf.String(), ExitCode: cmd.ProcessState.ExitCode()}, nil
			}
			return ExecOutput{}, &SpawnError{Program: spec.Command[0], Err: runErr}
		}
	}
	return ExecOutput{Stdout: out.String(), Stderr: errBuf.String(), ExitCode: 0}, nil
}

// ExecStreaming starts a host child in its own process group and returns its live
// pipes. Missing programs return 127; programs that cannot execute return 126.
// A missing or non-directory workdir or missing interpreter is an error.
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

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	childEnv := envSlice(spec.Env)
	if code, stderr, ok := hostExecPrecheck(spec.Command[0], spec.Workdir, childEnv); ok {
		return hostStartFailureStream(code, stderr), nil
	}
	execCtx, cancel := context.WithCancel(ctx)
	cmd := hostExecCommand(execCtx, spec.Command, spec.Workdir, childEnv)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	cmd.Env = childEnv
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
		cleanupErr := closePipes(stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW)
		if code, stderr, ok := hostExecStartFailure(spec.Command[0], spec.Workdir, cmd.Path, startErr); ok {
			stream := hostStartFailureStream(code, stderr)
			if cleanupErr != nil {
				_ = errors.Join(stream.IO.Stdin.Close(), stream.IO.Stdout.Close(), stream.IO.Stderr.Close())
				return nil, &SpawnError{Program: spec.Command[0], Err: cleanupErr}
			}
			return stream, nil
		}
		return nil, errors.Join(&SpawnError{Program: spec.Command[0], Err: startErr}, cleanupErr)
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

func hostExecCommand(ctx context.Context, command []string, workdir *string, env []string) *exec.Cmd {
	path := command[0]
	if !strings.ContainsRune(path, '/') {
		pathEnv := ""
		for _, kv := range env {
			if value, ok := strings.CutPrefix(kv, "PATH="); ok {
				pathEnv = value
				break
			}
		}
		for _, dir := range filepath.SplitList(pathEnv) {
			if dir == "" {
				dir = "."
			}
			candidate := filepath.Join(dir, path)
			if candidate == path {
				candidate = "." + string(filepath.Separator) + candidate
			}
			resolved := pathAtWorkdir(candidate, workdir)
			if info, err := os.Stat(resolved); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
				path, err = filepath.Abs(resolved)
				if err != nil {
					path = candidate
				}
				break
			}
		}
	}
	cmd := exec.CommandContext(ctx, path, command[1:]...) //nolint:gosec // path is resolved from the exec environment PATH; argv remains an argument vector
	cmd.Path = path
	cmd.Err = nil
	return cmd
}

func hostExecPrecheck(program string, workdir *string, env []string) (int, string, bool) {
	explicit := strings.ContainsRune(program, '/')
	if workdir != nil {
		if info, err := os.Stat(*workdir); err != nil || !info.IsDir() {
			return 0, "", false
		}
	}
	if explicit {
		info, err := os.Stat(pathAtWorkdir(program, workdir))
		if errors.Is(err, syscall.ENOENT) {
			return hostMissingProgram(program, true)
		}
		if err != nil {
			return 0, "", false
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return hostNonExecutableProgram(program)
		}
		return 0, "", false
	}
	pathEnv := ""
	for _, kv := range env {
		if value, ok := strings.CutPrefix(kv, "PATH="); ok {
			pathEnv = value
			break
		}
	}
	notExecutable := false
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, program)
		info, err := os.Stat(pathAtWorkdir(candidate, workdir))
		// Like a shell PATH search: unreadable entries and directories are skipped.
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.Mode().Perm()&0o111 == 0 {
			notExecutable = true
			continue
		}
		return 0, "", false
	}
	if notExecutable {
		return hostNonExecutableProgram(program)
	}
	return hostMissingProgram(program, false)
}
func hostMissingProgram(program string, explicit bool) (int, string, bool) {
	if explicit {
		return 127, fmt.Sprintf("%q: no such file or directory\n", program), true
	}
	return 127, fmt.Sprintf("executable file %q not found in $PATH\n", program), true
}

func pathAtWorkdir(path string, workdir *string) string {
	if workdir != nil && !filepath.IsAbs(path) {
		return filepath.Join(*workdir, path)
	}
	return path
}

func hostExecStartFailure(program string, workdir *string, executablePath string, err error) (int, string, bool) {
	path, ok := errors.AsType[*os.PathError](err)
	if !ok || path.Op != "fork/exec" || path.Path != executablePath {
		return 0, "", false
	}
	switch {
	case errors.Is(path.Err, syscall.ENOENT):
		if workdir != nil {
			if _, err := os.Stat(*workdir); err != nil {
				return 0, "", false
			}
		}
		if _, err := os.Stat(pathAtWorkdir(executablePath, workdir)); errors.Is(err, syscall.ENOENT) {
			return hostMissingProgram(program, strings.ContainsRune(program, '/'))
		}
		return 0, "", false
	case errors.Is(path.Err, syscall.ENOEXEC), errors.Is(path.Err, syscall.EACCES), errors.Is(path.Err, syscall.EPERM):
		return hostNonExecutableProgram(program)
	default:
		return 0, "", false
	}
}

func hostNonExecutableProgram(program string) (int, string, bool) {
	return 126, fmt.Sprintf("%q: permission denied / not executable\n", program), true
}

func hostStartFailureStream(code int, stderr string) *StreamingExec {
	stdinR, stdinW := io.Pipe()
	_ = stdinR.Close()
	return &StreamingExec{
		IO: StreamingIO{
			Stdin:  stdinW,
			Stdout: io.NopCloser(strings.NewReader("")),
			Stderr: io.NopCloser(strings.NewReader(stderr)),
		},
		Process: newChildHandleFuncs(func() error { return nil }, func() error {
			return &ExitStatusError{Code: code}
		}),
	}
}

// Stop SIGTERMs the spawned process group, then SIGKILLs after timeout. A process
// double-forked out of that group is not reliably stopped (no cgroup freezer).
func (h *HostRuntime) Stop(ctx context.Context, id WorkloadID, timeout time.Duration) error {
	stopped := h.liveProcess(id)
	if stopped != nil {
		if err := signalAndWait(ctx, stopped, timeout); err != nil {
			return err
		}
	}
	// Only a confirmed stop reads as not running, like a stopped podman container;
	// a failed signal or expired ctx leaves a possibly live process marked started.
	// A process launched during the stop keeps the handle started.
	h.mu.Lock()
	if handle, ok := h.handles[id]; ok && (handle.proc == nil || handle.proc == stopped) {
		handle.state = hostCreated
	}
	h.mu.Unlock()
	return nil
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

// Running reports whether the named handle is started and its agent process, if
// spawned yet, has not exited. Start is bookkeeping and the process arrives at
// ExecStreaming, so a started handle with no process counts as running.
func (h *HostRuntime) Running(_ context.Context, name string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, handle := range h.handles {
		if handle.name != name {
			continue
		}
		if handle.state != hostStarted {
			return false, nil
		}
		if handle.proc == nil {
			return true, nil
		}
		select {
		case <-handle.proc.done:
			return false, nil
		default:
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

// AgentStateDir returns the private 0700 state dir Create minted for the handle,
// where the Runner's host Provision leg serves the agent's gateway socket and
// materializes its config — the host tier has no bind mounts, so both live here
// and are threaded to the agent by path. An unknown id returns ok=false. This is
// the host backend's Provision-leg selector, the analogue of the microVM
// backend's AgentGatewayEndpoint: only HostRuntime implements it, so podman and
// the microVM backend leave the Runner's default leg byte-identical.
func (h *HostRuntime) AgentStateDir(id WorkloadID) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	handle, ok := h.handles[id]
	if !ok {
		return "", false
	}
	return handle.stateDir, true
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

func signalAndWait(ctx context.Context, proc *hostProcess, timeout time.Duration) error {
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
