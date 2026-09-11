package runtime

// applecontainer.go is the Apple `container` CLI WorkloadRuntime backend: the
// macOS arm of the substrate, driving Apple's container tool the same way
// podman.go drives podman — argv builders split from the shared cliEngine
// subprocess seam (clispawn.go), so every serialized command shape is
// unit-testable without a binary.
//
// Deliberately untagged (no //go:build darwin): the driver is plain Go over a
// subprocess, so its argv builders and version parser compile and test on any
// host. Only the CLI it invokes is macOS-only.
//
// Two shape differences from podman, both measured on real hardware:
//   - No userns remap. virtiofs performs identity translation at the host↔guest
//     boundary, so podman's --userns=keep-id:uid=,gid= has no analogue and needs
//     none; guest writes already land host-side as the invoking macOS user.
//   - No SELinux. Mounts carry no :Z relabel and MountLabel reports no label.

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AppleContainerConfig is the operator-supplied wiring for the apple-container
// backend. Both fields are optional: a zero value selects the default below.
type AppleContainerConfig struct {
	// Program is the engine binary. Empty invokes `container` on PATH.
	Program string
	// Timeout is the per-command wall-clock cap. Zero uses
	// defaultCommandTimeout.
	Timeout time.Duration
}

// AppleContainerCLI is a WorkloadRuntime over Apple's `container` CLI,
// mirroring PodmanCLI's shape, over the same shared subprocess seam.
type AppleContainerCLI struct {
	cliEngine
}

var _ WorkloadRuntime = (*AppleContainerCLI)(nil)

// appleProgram is the engine binary name NewAppleContainerCLI defaults to.
const appleProgram = "container"

// NewAppleContainerCLI builds an AppleContainerCLI from cfg, defaulting to
// `container` on PATH under the shared per-command timeout.
func NewAppleContainerCLI(cfg AppleContainerConfig) *AppleContainerCLI {
	program := cfg.Program
	if program == "" {
		program = appleProgram
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = defaultCommandTimeout
	}
	return &AppleContainerCLI{cliEngine{program: program, timeout: timeout}}
}

// Create assembles and runs `container create`, returning the new container id.
// The CLI renders its `[N/6] <phase>` progress on stderr, so stdout carries the
// id alone.
func (a *AppleContainerCLI) Create(ctx context.Context, spec WorkloadSpec) (WorkloadID, error) {
	stdout, err := a.run(ctx, "container create", appleCreateArgs(spec))
	if err != nil {
		return "", err
	}
	return WorkloadID(strings.TrimSpace(string(stdout))), nil
}

// appleCreateArgs assembles the argv for `container create`. Split out so the
// argv assembly is unit-testable without spawning the CLI, the createArgs
// discipline.
//
// No userns/uid-remap token is emitted, and that is load-bearing: virtiofs
// translates identity at the boundary, so the podman remap has no analogue here
// and a hand-rolled substitute would break the ownership round-trip that
// already works.
func appleCreateArgs(spec WorkloadSpec) []string {
	// Preallocate: 3 fixed tokens (create, --name+value) + 2 per
	// cap/mount/env pair + image + command tokens, so the appends below don't
	// reallocate.
	args := make([]string, 0, 3+2*(len(spec.CapAdd)+len(spec.Mounts)+len(spec.Env))+1+len(spec.Command))
	args = append(args, "create", "--name", spec.Name)
	for _, capability := range spec.CapAdd {
		args = append(args, "--cap-add", capability)
	}
	for _, mount := range spec.Mounts {
		args = append(args, "--volume", appleMountArg(mount))
	}
	for _, kv := range sortedEnv(spec.Env) {
		args = append(args, "--env", kv.key+"="+kv.value)
	}
	args = append(args, spec.Image)
	args = append(args, spec.Command...)
	return args
}

// appleMountArg assembles a `host:container[:ro]` volume argument. No :Z
// relabel: SELinux does not exist on macOS, so podman's mountArg suffix has
// nothing to label here.
func appleMountArg(mount Mount) string {
	arg := mount.HostPath + ":" + mount.ContainerPath
	if mount.ReadOnly {
		arg += ":ro"
	}
	return arg
}

// minAppleContainerMajor / minAppleContainerMinor are the `container` version
// floor: 1.0.0 removed the v0 XPC APIs and froze the config surface, so it is
// the first release whose CLI contract this backend can rely on. The floor is
// hard — there is no compatibility path below it.
const (
	minAppleContainerMajor = 1
	minAppleContainerMinor = 0
)

// VerifyAppleContainerSupport checks the engine meets the 1.0.0 floor, probing
// `container --version` and erroring below it. It names both the required floor
// and the found version so an operator on too-old a CLI learns the cause at
// startup rather than deep inside the first container create.
func (a *AppleContainerCLI) VerifyAppleContainerSupport(ctx context.Context) error {
	stdout, err := a.run(ctx, "container --version", []string{"--version"})
	if err != nil {
		return err
	}
	return appleVersionFloorVerdict(strings.TrimSpace(string(stdout)))
}

// appleVersionFloorVerdict parses raw `container --version` output and returns
// nil at or above the floor, an error below it. Pure so the refusal — the copy
// an operator actually reads — is unit-testable without spawning the CLI.
func appleVersionFloorVerdict(raw string) error {
	major, minor, err := parseAppleContainerVersion(raw)
	if err != nil {
		return err
	}
	if major < minAppleContainerMajor || (major == minAppleContainerMajor && minor < minAppleContainerMinor) {
		return fmt.Errorf(
			"apple container %d.%d or newer is required (%d.%d froze the CLI and XPC "+
				"contract this backend drives), but this host has %q",
			minAppleContainerMajor, minAppleContainerMinor,
			minAppleContainerMajor, minAppleContainerMinor, raw)
	}
	return nil
}

// appleVersionPattern matches the first major.minor in a `container --version`
// line. The CLI prints prose, not a bare version —
// `container CLI version 1.1.0 (build: release, commit: 5973b9c)` — so the
// number is extracted rather than split off the front.
var appleVersionPattern = regexp.MustCompile(`(\d+)\.(\d+)`)

// parseAppleContainerVersion parses the major.minor out of a `container
// --version` line. Split out so the floor comparison is unit-testable without
// spawning the CLI. An input carrying no major.minor is an error.
func parseAppleContainerVersion(raw string) (major, minor int, err error) {
	match := appleVersionPattern.FindStringSubmatch(raw)
	if match == nil {
		return 0, 0, fmt.Errorf("unparseable apple container version %q: want a major.minor[.patch] in the output", raw)
	}
	major, err = strconv.Atoi(match[1])
	if err != nil {
		return 0, 0, fmt.Errorf("unparseable apple container major version in %q: %w", raw, err)
	}
	minor, err = strconv.Atoi(match[2])
	if err != nil {
		return 0, 0, fmt.Errorf("unparseable apple container minor version in %q: %w", raw, err)
	}
	return major, minor, nil
}

// Start starts a created container.
func (a *AppleContainerCLI) Start(ctx context.Context, id WorkloadID) error {
	_, err := a.run(ctx, "container start", []string{"start", id.String()})
	return err
}

// Exec runs a command in a running container, capturing its output. A non-zero
// exit is captured in ExecOutput, not folded into an error (a denied firewall
// probe is an expected non-zero); a spawn failure or timeout is an error.
func (a *AppleContainerCLI) Exec(ctx context.Context, id WorkloadID, spec ExecSpec) (ExecOutput, error) {
	stdout, stderr, exitCode, err := a.spawnCapture(ctx, "container exec", appleExecArgs(id, spec), spec.Stdin)
	if err != nil {
		return ExecOutput{}, err
	}
	return ExecOutput{
		Stdout:   string(stdout),
		Stderr:   string(stderr),
		ExitCode: exitCode,
	}, nil
}

// appleExecArgs assembles the argv for a one-shot `container exec`. Split out so
// the argv assembly is unit-testable without spawning the CLI.
func appleExecArgs(id WorkloadID, spec ExecSpec) []string {
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
		args = append(args, "--env", kv.key+"="+kv.value)
	}
	args = append(args, id.String())
	args = append(args, spec.Command...)
	return args
}

// ExecStreaming starts a streaming `container exec -i` through the shared
// subprocess seam, returning the live pipes plus a kill/wait handle.
func (a *AppleContainerCLI) ExecStreaming(ctx context.Context, id WorkloadID, spec StreamingExecSpec) (*StreamingExec, error) {
	return a.spawnStreaming(ctx, appleExecStreamingArgs(id, spec))
}

// appleExecStreamingArgs assembles the argv for a streaming `container exec -i`.
// --interactive keeps stdin open for the process's life; there is deliberately
// no --tty (the agent is a headless process draining diagnostic pipes, not a
// terminal session).
func appleExecStreamingArgs(id WorkloadID, spec StreamingExecSpec) []string {
	args := []string{argExec, argInteractive}
	if spec.User != nil {
		args = append(args, "--user", *spec.User)
	}
	if spec.Workdir != nil {
		args = append(args, "--workdir", *spec.Workdir)
	}
	for _, kv := range sortedEnv(spec.Env) {
		args = append(args, "--env", kv.key+"="+kv.value)
	}
	args = append(args, id.String())
	args = append(args, spec.Command...)
	return args
}

// Stop stops a running container, allowing timeout for graceful exit. A
// container that is already gone is success: teardown runs more than once (a
// retry, or after an operator removed the container by hand) and must be a
// no-op the second time, not a hard failure. An already-stopped but existing
// container already exits 0, so only absence needs tolerating.
func (a *AppleContainerCLI) Stop(ctx context.Context, id WorkloadID, timeout time.Duration) error {
	return a.runTolerateMissing(ctx, "container stop", appleStopArgs(id, timeout))
}

// appleStopArgs assembles the `container stop` argv. --time is whole seconds;
// the interface takes a Duration for idiom and callsite clarity, converted at
// this CLI boundary by the shared stopGraceSeconds rounding.
func appleStopArgs(id WorkloadID, timeout time.Duration) []string {
	return []string{"stop", "--time", strconv.FormatInt(stopGraceSeconds(timeout), 10), id.String()}
}

// Remove removes a container (force-kills if still running). Removing an
// already-removed or never-created id is success, for the same idempotent
// teardown reason as Stop; unlike podman's `rm --force`, this CLI exits 1 on a
// missing id.
func (a *AppleContainerCLI) Remove(ctx context.Context, id WorkloadID) error {
	return a.runTolerateMissing(ctx, "container rm", appleRemoveArgs(id))
}

// appleRemoveArgs assembles the `container rm` argv. No --volumes counterpart to
// podman's removeArgs: this CLI has no anonymous-volume lifecycle to leak.
func appleRemoveArgs(id WorkloadID) []string {
	return []string{"rm", "--force", id.String()}
}

// Exists reports whether a container with name exists in any state. This CLI has
// no `container exists` verb, so absence is read off `container inspect`: exit 1
// with a "not found" stderr. A generic non-zero is a real engine failure, never
// absence.
func (a *AppleContainerCLI) Exists(ctx context.Context, name string) (bool, error) {
	_, stderr, exitCode, err := a.spawnCapture(ctx, "container inspect", appleInspectArgs(name), nil)
	if err != nil {
		return false, err
	}
	return classifyInspectErr(exitCode, string(stderr))
}

// appleInspectArgs assembles the `container inspect` argv used as the existence
// probe.
func appleInspectArgs(name string) []string {
	return []string{"inspect", name}
}

// appleNotFoundStderr is the CLI's own "that container does not exist" refusal.
// The two wordings differ by verb — inspect says `container not found: <id>`,
// rm/stop say `notFound: "container with ID <id> not found"` — and both carry
// this substring.
const appleNotFoundStderr = "not found"

// appleMissingContainer reports whether a non-zero result is the CLI saying the
// container does not exist.
//
// Exit code AND wording, never the code alone: an unreachable apiserver also
// exits 1 (an "XPC connection error" stderr), so a code-only guard would report
// every container as absent while the engine is down.
func appleMissingContainer(exitCode int, stderr string) bool {
	return exitCode == 1 && strings.Contains(stderr, appleNotFoundStderr)
}

// classifyInspectErr turns an inspect exit code plus stderr into an existence
// verdict. Pure so the classification is unit-testable against real CLI output
// without spawning anything.
//
// Only the CLI's own "not found" wording means absence; any other non-zero is a
// failed probe, and reporting it as absence would let a caller recreate a
// container that already exists.
func classifyInspectErr(exitCode int, stderr string) (bool, error) {
	trimmed := strings.TrimSpace(stderr)
	switch {
	case exitCode == 0:
		return true, nil
	case appleMissingContainer(exitCode, trimmed):
		return false, nil
	default:
		return false, &CommandError{Summary: "container inspect", ExitCode: exitCode, Stderr: trimmed}
	}
}

// MountLabel reports no SELinux mount label: SELinux does not exist on macOS,
// so there is no MCS category to relabel a config dir into. The microVM
// precedent, and the reason the config-update relabel is a no-op here.
func (a *AppleContainerCLI) MountLabel(_ context.Context, _ WorkloadID) (string, error) {
	return "", nil
}

// Resize is the S1-frozen resize-in-place verb, unimplemented here for the same
// reason as PodmanCLI.Resize: a silent no-op would report a limit change that
// never happened. This CLI exposes no live resource-update verb at the version
// floor.
func (a *AppleContainerCLI) Resize(_ context.Context, _ WorkloadID, _ ResourceLimits) error {
	return ErrResizeNotImplemented
}

// runTolerateMissing runs `container <args>` like run, but reports the CLI's
// own "container does not exist" refusal as success. The teardown verbs
// (stop/rm) use it so a repeated teardown is a no-op rather than a hard
// failure; every other non-zero exit still becomes a CommandError.
func (a *AppleContainerCLI) runTolerateMissing(ctx context.Context, summary string, args []string) error {
	_, stderr, exitCode, err := a.spawnCapture(ctx, summary, args, nil)
	if err != nil {
		return err
	}
	trimmed := strings.TrimSpace(string(stderr))
	if exitCode == 0 || appleMissingContainer(exitCode, trimmed) {
		return nil
	}
	return &CommandError{Summary: summary, ExitCode: exitCode, Stderr: trimmed}
}
