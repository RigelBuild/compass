// Package runtime is the per-agent isolation substrate: each agent runs in its
// own rootless-podman container with its own full git clone, a scoped $HOME for
// that agent's credentials, and a default-deny egress firewall — the container
// is the unit of isolation (compass.md §5.3). The Runner owns the container
// lifecycle: create/start the container from the configured agent base image,
// arm the firewall, exec the agent as a non-root user, and tear it all down.
// The agent clones its own repos and activates their devenv from inside the
// container, so neither the image build nor the clone is the Runner's job.
//
// The layering, bottom to top:
//   - podman.go — a thin ContainerRuntime over the podman CLI: the only place a
//     subprocess is spawned. Everything above depends on the interface, so a
//     libpod-REST backend can replace it without touching a caller.
//   - egress.go — the default-deny + allowlist firewall applied inside the
//     container before the agent runs.
//   - workspace.go — clone-per-container plus the scoped $HOME and its git
//     credential helper.
//   - agent.go — AgentRuntime, the lifecycle façade the Runner drives.
//   - registry.go — AgentRegistry, the Runner's live-container handle cache the
//     session RPCs resolve a launched container by name through.
//
// This file is the container-runtime seam: a ContainerRuntime interface plus
// PodmanCLI, its rootless-podman-CLI implementation. Rootless is a hard
// requirement (compass.md §5.3, §7.1): no daemon, no root, no rootful fallback.
// Containers run with --userns=keep-id so files the agent writes in a
// bind-mount map back to the invoking user on the host.
//
// Kill-on-abandon: the Rust original relied on tokio's kill_on_drop to reap a
// subprocess whose future was dropped. Go has no Drop, so cancellation is
// explicit: every method threads the caller's context.Context, one-shot execs
// run under a per-command timeout via exec.CommandContext (a hung podman is
// killed, never blocks the caller forever), and a streaming exec ties the
// process to a cancellable context with a Cancel/WaitDelay pair so abandoning
// its handle — or cancelling the parent context — SIGKILLs and reaps the exec
// rather than leaking a live in-container agent.
package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ContainerID is a running (or created) container, identified by the full id
// podman prints.
type ContainerID string

// String returns the raw container id.
func (c ContainerID) String() string { return string(c) }

// Mount is a host→container bind mount. ReadOnly maps to :ro and every mount
// gets SELinux relabelling (:Z) so the substrate works on enforcing hosts.
type Mount struct {
	HostPath      string
	ContainerPath string
	ReadOnly      bool
}

// ContainerSpec is everything needed to create one agent container. Kept
// engine-agnostic: the podman-specific argv is assembled in PodmanCLI.Create,
// not here.
type ContainerSpec struct {
	// Image reference in local container storage (e.g. compass-agent:latest —
	// one shared base image, not a per-repo tag).
	Image string
	// Name is the container name — the Runner's stable handle for this agent
	// workstream.
	Name string
	// CapAdd is the Linux capabilities to add. The substrate adds only
	// NET_ADMIN, and only so the entrypoint can arm the egress firewall; the
	// agent itself runs as a non-root user with an empty capability set (see
	// egress.go).
	CapAdd []string
	// Mounts is the read-only host bind mounts (e.g. a host cache mounted
	// read-only).
	Mounts []Mount
	// Env is the environment variables set on the container.
	Env map[string]string
	// Command is the long-lived entrypoint. The container stays up (the Runner
	// execs into it); a sleep loop when empty.
	Command []string
}

// ExecSpec is how to run a command inside a container.
type ExecSpec struct {
	Command []string
	// User is the --user value. Nil runs as the image's default user (root);
	// agent work always sets a uid so it runs unprivileged.
	User *string
	// Workdir is the --workdir inside the container.
	Workdir *string
	Env     map[string]string
	// Stdin is bytes to write to the command's stdin. Used to feed a script to
	// `sh -s` — the script (and any secret it embeds) then never appears in the
	// exec argv, which is visible in the container's process list.
	Stdin *string
}

// NewExecSpec builds an ExecSpec running command with an empty environment.
func NewExecSpec(command ...string) ExecSpec {
	return ExecSpec{Command: command, Env: map[string]string{}}
}

// AsUser sets the --user the command runs as.
func (s ExecSpec) AsUser(user string) ExecSpec {
	s.User = &user
	return s
}

// InDir sets the --workdir the command runs in.
func (s ExecSpec) InDir(dir string) ExecSpec {
	s.Workdir = &dir
	return s
}

// WithStdin feeds input to the command's stdin (e.g. a script for `sh -s`).
func (s ExecSpec) WithStdin(input string) ExecSpec {
	s.Stdin = &input
	return s
}

// ExecOutput is the captured result of an exec: streams plus the exit code.
type ExecOutput struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Success reports whether the command exited zero.
func (o ExecOutput) Success() bool { return o.ExitCode == 0 }

// StreamingExecSpec is how to run a long-lived, streaming command inside a
// container. Unlike ExecSpec it carries no stdin script: a streaming exec's
// whole purpose is a live stdin/stdout pipe pair for a long-running process
// (the agent), for which a one-shot stdin feed is meaningless — so it is
// omitted here rather than silently ignored.
type StreamingExecSpec struct {
	Command []string
	// User is the --user value. Nil runs as the image's default user (root);
	// agent work always sets a uid so it runs unprivileged.
	User *string
	// Workdir is the --workdir inside the container.
	Workdir *string
	Env     map[string]string
}

// NewStreamingExecSpec builds a StreamingExecSpec running command with an empty
// environment.
func NewStreamingExecSpec(command ...string) StreamingExecSpec {
	return StreamingExecSpec{Command: command, Env: map[string]string{}}
}

// AsUser sets the --user the command runs as.
func (s StreamingExecSpec) AsUser(user string) StreamingExecSpec {
	s.User = &user
	return s
}

// InDir sets the --workdir the command runs in.
func (s StreamingExecSpec) InDir(dir string) StreamingExecSpec {
	s.Workdir = &dir
	return s
}

// StreamingIO is the live IO of a streaming exec: the pipes the caller reads and
// writes for the exec's whole lifetime. Held separately from ChildHandle so
// ownership of the streams is explicit — they are the process's stdio pipes,
// leaving the handle with kill/wait only.
type StreamingIO struct {
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Stderr io.ReadCloser
}

// ChildHandle is a kill/wait handle over a streaming exec's child process. It
// carries no live IO: the child's stdio lives in StreamingIO, so this handle can
// only signal or await the process.
//
// The process was started under a cancellable context whose Cancel SIGKILLs it,
// so cancelling the parent context — or calling Kill — terminates the exec even
// though Go has no Drop. WaitDelay bounds the reap so Wait returns even if a
// reader is still holding a stdio pipe open.
type ChildHandle struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

// Kill signals the exec (SIGKILL) via its context, without reaping — the reap
// happens in Wait. The session manager kills a container's exec on a deliberate
// stop/teardown; killing an already-exited process is not an error.
func (h *ChildHandle) Kill() error {
	h.cancel()
	return nil
}

// Wait awaits the exec's exit and reaps it, returning the process result (a
// non-nil error for a non-zero exit or a kill signal). The session manager
// watches this to tell an unexpected agent exit (crash) from a deliberate
// stop/teardown kill.
func (h *ChildHandle) Wait() error {
	return h.cmd.Wait()
}

// Terminate SIGKILLs the exec and reaps it in one call — Kill then Wait. A
// consumer that abandons a streaming exec must call this (or Kill+Wait) so the
// watch goroutine and the child are both cleaned up; dropping the handle without
// it leaks a goroutine and a zombie until the Runner exits (Go has no Drop to
// reap it automatically). Use Kill+Wait separately only when the caller needs to
// observe the exit between the two — e.g. distinguishing a crash from a
// deliberate teardown; Terminate is the safe default for the abandon path.
func (h *ChildHandle) Terminate() error {
	h.cancel()
	return h.cmd.Wait()
}

// StreamingExec is a long-lived streaming exec: its live StreamingIO plus a
// ChildHandle for kill/wait. The Runner drains IO for the process's life (the
// agent's pipes carry diagnostics) and uses Process to terminate or await the
// in-container agent.
type StreamingExec struct {
	IO      StreamingIO
	Process *ChildHandle
}

// SpawnError is the engine binary being unusable (missing, permission): the
// subprocess could not be spawned at all.
type SpawnError struct {
	Program string
	Err     error
}

func (e *SpawnError) Error() string {
	return fmt.Sprintf("failed to spawn %q: %v", e.Program, e.Err)
}

// Unwrap exposes the underlying spawn failure for errors.Is/As.
func (e *SpawnError) Unwrap() error { return e.Err }

// CommandError is a container operation the engine ran and rejected, carrying
// the argv summary and captured stderr for diagnosis. The summary names the
// operation without leaking the full argv (which may hold env values or a token
// on stdin).
type CommandError struct {
	Summary  string
	ExitCode int
	Stderr   string
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("%q failed (exit %d): %s", e.Summary, e.ExitCode, e.Stderr)
}

// TimeoutError is a container operation that overran the per-command wall-clock
// cap and was killed rather than left to block the caller forever.
type TimeoutError struct {
	Summary string
	Timeout time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("%q timed out after %ds", e.Summary, int(e.Timeout.Seconds()))
}

// ContainerRuntime is the container engine seam. Every operation is a subprocess
// (or, later, a socket round-trip), so each threads the caller's context for
// cancellation and carries the per-command timeout the implementation applies.
// An interface so the Runner can hold a ContainerRuntime and tests can
// substitute a fake.
type ContainerRuntime interface {
	// Create makes a container from spec without starting it, returning its id.
	Create(ctx context.Context, spec ContainerSpec) (ContainerID, error)

	// Start starts a created container.
	Start(ctx context.Context, id ContainerID) error

	// Exec runs a command in a running container, capturing its output. A
	// non-zero exit is a successful runtime call returning a failed command
	// (ExecOutput.ExitCode), not an error; only a spawn failure or timeout is an
	// error.
	Exec(ctx context.Context, id ContainerID, spec ExecSpec) (ExecOutput, error)

	// ExecStreaming starts a long-lived streaming command in a running
	// container, returning its live stdio pipes plus a kill/wait handle rather
	// than awaiting completion. The transport for the long-running agent
	// process: its stdin/stdout stay open for the process's life (the agent's
	// protocol rides the per-container socket, so the pipes carry diagnostics).
	// Unlike Exec there is no wall-clock timeout — the process is meant to run
	// indefinitely — but the exec is still bound to ctx, so cancelling it
	// terminates the process.
	ExecStreaming(ctx context.Context, id ContainerID, spec StreamingExecSpec) (*StreamingExec, error)

	// Stop stops a running container, allowing timeout for graceful exit before
	// podman kills it.
	Stop(ctx context.Context, id ContainerID, timeout time.Duration) error

	// Remove removes a container (force-kills if still running).
	Remove(ctx context.Context, id ContainerID) error

	// Exists reports whether a container with name currently exists (any state).
	Exists(ctx context.Context, name string) (bool, error)

	// MountLabel reports the container's SELinux mount label (its private MCS
	// category), read from `podman inspect`. The config-update path relabels a
	// freshly materialized version dir into this category so a confined agent
	// can read it (agentHost.RefreshConfig -> ConfigMaterializer relabel).
	MountLabel(ctx context.Context, id ContainerID) (string, error)
}

// defaultCommandTimeout is the default per-command wall-clock cap. A hung podman
// (stalled pull, wedged userns/cgroup setup, hung exec) must surface as an
// error, never block the calling task forever.
const defaultCommandTimeout = 120 * time.Second

// podman exec subcommand + flag tokens, shared across the one-shot and
// streaming argv builders so the two stay in lockstep.
const (
	argExec        = "exec"
	argInteractive = "--interactive"
)

// PodmanCLI is a ContainerRuntime over the podman CLI.
type PodmanCLI struct {
	program string
	timeout time.Duration
}

// NewPodmanCLI builds a PodmanCLI invoking `podman` on PATH with the default
// per-command timeout.
func NewPodmanCLI() *PodmanCLI {
	return &PodmanCLI{program: "podman", timeout: defaultCommandTimeout}
}

// WithProgram uses an explicit engine binary (e.g. an absolute path, or
// `docker` in a pinch). Defaults to `podman` on PATH.
func (p *PodmanCLI) WithProgram(program string) *PodmanCLI {
	p.program = program
	return p
}

// WithTimeout overrides the per-command timeout.
func (p *PodmanCLI) WithTimeout(timeout time.Duration) *PodmanCLI {
	p.timeout = timeout
	return p
}

// Create assembles and runs `podman create`, returning the new container id.
func (p *PodmanCLI) Create(ctx context.Context, spec ContainerSpec) (ContainerID, error) {
	// Preallocate: 4 fixed tokens (create, --name+value, --userns) + 2 per
	// cap/mount/env pair + image + command tokens, so the appends below don't
	// reallocate.
	args := make([]string, 0, 4+2*(len(spec.CapAdd)+len(spec.Mounts)+len(spec.Env))+1+len(spec.Command))
	args = append(args,
		"create",
		"--name", spec.Name,
		// Rootless uid mapping: files the agent writes in a bind-mount map back
		// to the invoking user, not to a high subuid (compass.md §5.3).
		"--userns=keep-id",
	)
	for _, cap := range spec.CapAdd {
		args = append(args, "--cap-add", cap)
	}
	for _, mount := range spec.Mounts {
		args = append(args, "-v", mountArg(mount))
	}
	for _, kv := range sortedEnv(spec.Env) {
		args = append(args, "-e", kv.key+"="+kv.value)
	}
	args = append(args, spec.Image)
	args = append(args, spec.Command...)

	stdout, err := p.run(ctx, "podman create", args)
	if err != nil {
		return "", err
	}
	return ContainerID(strings.TrimSpace(string(stdout))), nil
}

// Start starts a created container.
func (p *PodmanCLI) Start(ctx context.Context, id ContainerID) error {
	_, err := p.run(ctx, "podman start", []string{"start", id.String()})
	return err
}

// Exec runs a command in a running container, capturing its output. A non-zero
// exit is captured in ExecOutput, not folded into an error (a denied firewall
// probe is an expected non-zero); a spawn failure or timeout is an error.
func (p *PodmanCLI) Exec(ctx context.Context, id ContainerID, spec ExecSpec) (ExecOutput, error) {
	args := []string{argExec}
	// Forward stdin only when there's input to feed, so `sh -s` reads the script
	// from the pipe rather than the argv.
	if spec.Stdin != nil {
		args = append(args, argInteractive)
	}
	if spec.User != nil {
		args = append(args, "--user", *spec.User)
	}
	if spec.Workdir != nil {
		args = append(args, "--workdir", *spec.Workdir)
	}
	for _, kv := range sortedEnv(spec.Env) {
		args = append(args, "-e", kv.key+"="+kv.value)
	}
	args = append(args, id.String())
	args = append(args, spec.Command...)

	stdout, stderr, exitCode, err := p.spawnCapture(ctx, "podman exec", args, spec.Stdin)
	if err != nil {
		return ExecOutput{}, err
	}
	return ExecOutput{
		Stdout:   string(stdout),
		Stderr:   string(stderr),
		ExitCode: exitCode,
	}, nil
}

// ExecStreaming starts a streaming `podman exec -i`, returning the live pipes
// plus a kill/wait handle. The exec is bound to a cancellable child of ctx: its
// Cancel SIGKILLs the process and WaitDelay bounds the reap, so cancelling the
// parent context or calling ChildHandle.Kill terminates the in-container agent
// even without a Go Drop.
func (p *PodmanCLI) ExecStreaming(ctx context.Context, id ContainerID, spec StreamingExecSpec) (*StreamingExec, error) {
	execCtx, cancel := context.WithCancel(ctx)
	//nolint:gosec // G204: the container-engine seam — see spawnCapture. The
	// engine binary is operator-set and the exec argv is Runner-assembled.
	cmd := exec.CommandContext(execCtx, p.program, execStreamingArgs(id, spec)...)
	// A dropped session must kill the exec, or the in-container agent keeps
	// running after the Runner lets go of the handle. No command timeout: a
	// streaming session is long-lived by design.
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = 10 * time.Second

	spawnErr := func(err error) (*StreamingExec, error) {
		cancel()
		return nil, &SpawnError{Program: p.program, Err: err}
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return spawnErr(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return spawnErr(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return spawnErr(err)
	}
	if err := cmd.Start(); err != nil {
		return spawnErr(err)
	}

	return &StreamingExec{
		IO:      StreamingIO{Stdin: stdin, Stdout: stdout, Stderr: stderr},
		Process: &ChildHandle{cmd: cmd, cancel: cancel},
	}, nil
}

// stopGraceSeconds converts a graceful-stop timeout to podman's whole-second
// --time. It rounds a positive Duration up so any positive grace is at least
// one second — truncating toward zero would turn a sub-second grace into
// --time 0, an immediate SIGKILL with no grace — and clamps a negative Duration
// to 0, since podman reads a negative --time as an infinite (unbounded) wait.
func stopGraceSeconds(timeout time.Duration) int64 {
	return max(int64(math.Ceil(timeout.Seconds())), 0)
}

// Stop stops a running container, allowing timeout for graceful exit.
func (p *PodmanCLI) Stop(ctx context.Context, id ContainerID, timeout time.Duration) error {
	// podman's --time is whole seconds; the interface takes a Duration for idiom
	// and callsite clarity, converted at this CLI boundary.
	_, err := p.run(ctx, "podman stop", []string{
		"stop",
		"--time", strconv.FormatInt(stopGraceSeconds(timeout), 10),
		id.String(),
	})
	return err
}

// Remove removes a container (force-kills if still running).
func (p *PodmanCLI) Remove(ctx context.Context, id ContainerID) error {
	_, err := p.run(ctx, "podman rm", []string{"rm", "--force", id.String()})
	return err
}

// Exists reports whether a container with name exists in any state. `container
// exists` encodes the answer in its exit code (0 present, 1 absent), so it can't
// go through run (which treats non-zero as an error); anything other than 0/1 is
// a real engine failure.
func (p *PodmanCLI) Exists(ctx context.Context, name string) (bool, error) {
	_, stderr, exitCode, err := p.spawnCapture(ctx, "podman container exists", []string{"container", "exists", name}, nil)
	if err != nil {
		return false, err
	}
	switch exitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, &CommandError{
			Summary:  "podman container exists",
			ExitCode: exitCode,
			Stderr:   strings.TrimSpace(string(stderr)),
		}
	}
}

// MountLabel reads the container's SELinux mount label via `podman inspect`,
// trimming the trailing newline the CLI prints. A one-shot fire-and-check like
// Start/Remove: a non-zero exit becomes a CommandError through run.
func (p *PodmanCLI) MountLabel(ctx context.Context, id ContainerID) (string, error) {
	out, err := p.run(ctx, "podman inspect", inspectMountLabelArgs(id))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// spawnCapture spawns `podman <args>`, optionally writing stdin, and captures
// output under the command timeout. The single subprocess seam: a spawn
// failure, a timeout, and a captured non-zero exit are all mapped here. summary
// names the operation for error context without leaking the full argv (which may
// hold env values or a token on stdin).
//
// A non-zero exit is returned as (stdout, stderr, code, nil) — the caller
// decides whether that is an error (run) or an expected result (Exec, Exists).
// Only a spawn failure, this call's own timeout, or parent-context cancellation
// is a non-nil error.
func (p *PodmanCLI) spawnCapture(ctx context.Context, summary string, args []string, stdin *string) (stdout, stderr []byte, exitCode int, err error) {
	cctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	//nolint:gosec // G204: this is the container-engine seam — spawning the
	// configured engine binary (p.program) with caller-supplied argv is the
	// module's entire purpose. Host/allowlist inputs are validated upstream
	// (isValidHost) before reaching an argv, and the program is operator-set,
	// not attacker-controlled.
	cmd := exec.CommandContext(cctx, p.program, args...)
	// A killed process that leaked a child still holding the output pipe would
	// keep Run blocked on that pipe indefinitely; WaitDelay bounds that wait so a
	// leaked-pipe hang can't outlive this call's timeout by more than WaitDelay.
	cmd.WaitDelay = 10 * time.Second
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if stdin != nil {
		// A strings.Reader hits EOF when the script is exhausted, so the child
		// never blocks waiting for more input.
		cmd.Stdin = strings.NewReader(*stdin)
	}

	if runErr := cmd.Run(); runErr != nil {
		switch {
		case cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil:
			// This call's own timeout fired (not the parent): the process was
			// killed, so surface a timeout rather than a bogus exit code.
			return nil, nil, 0, &TimeoutError{Summary: summary, Timeout: p.timeout}
		case ctx.Err() != nil:
			// The caller cancelled: propagate the context error.
			return nil, nil, 0, ctx.Err()
		default:
			var exitErr *exec.ExitError
			if errors.As(runErr, &exitErr) {
				// Ran to completion but exited non-zero: not an error here.
				return out.Bytes(), errBuf.Bytes(), exitErr.ExitCode(), nil
			}
			return nil, nil, 0, &SpawnError{Program: p.program, Err: runErr}
		}
	}
	return out.Bytes(), errBuf.Bytes(), 0, nil
}

// run runs `podman <args>`, requiring a zero exit (a non-zero becomes a
// CommandError). For fire-and-check operations like create/start/stop/remove.
func (p *PodmanCLI) run(ctx context.Context, summary string, args []string) ([]byte, error) {
	stdout, stderr, exitCode, err := p.spawnCapture(ctx, summary, args, nil)
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		return nil, &CommandError{
			Summary:  summary,
			ExitCode: exitCode,
			Stderr:   strings.TrimSpace(string(stderr)),
		}
	}
	return stdout, nil
}

// execStreamingArgs assembles the argv for a streaming `podman exec -i`. Split
// out so the argv assembly is unit-testable without spawning podman.
// --interactive keeps stdin open for the live JSON-RPC channel; there is
// deliberately no --tty (the stream is newline-delimited JSON-RPC, not a
// terminal).
func execStreamingArgs(id ContainerID, spec StreamingExecSpec) []string {
	args := []string{argExec, argInteractive}
	if spec.User != nil {
		args = append(args, "--user", *spec.User)
	}
	if spec.Workdir != nil {
		args = append(args, "--workdir", *spec.Workdir)
	}
	for _, kv := range sortedEnv(spec.Env) {
		args = append(args, "-e", kv.key+"="+kv.value)
	}
	args = append(args, id.String())
	args = append(args, spec.Command...)
	return args
}

// inspectMountLabelArgs assembles the argv for reading a container's SELinux
// mount label. Split out so the argv assembly is unit-testable without spawning
// podman, mirroring execStreamingArgs.
func inspectMountLabelArgs(id ContainerID) []string {
	return []string{"inspect", "--format", "{{.MountLabel}}", id.String()}
}

// mountArg assembles a `-v host:container[:ro],Z` argument. SELinux relabelling
// (Z) is always applied so a bind mount works on an enforcing host; ro is added
// for read-only mounts (e.g. the shared bare-repo cache).
func mountArg(mount Mount) string {
	suffix := ":Z"
	if mount.ReadOnly {
		suffix = ":ro,Z"
	}
	return mount.HostPath + ":" + mount.ContainerPath + suffix
}

// envEntry is one sorted environment key/value pair for deterministic argv.
type envEntry struct {
	key   string
	value string
}

// sortedEnv returns env's entries ordered by key, so the assembled argv is
// deterministic (matching the Rust BTreeMap ordering the argv tests assert).
func sortedEnv(env map[string]string) []envEntry {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]envEntry, len(keys))
	for i, key := range keys {
		entries[i] = envEntry{key: key, value: env[key]}
	}
	return entries
}
