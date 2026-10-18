//go:build unix

package adapters

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec" //nolint:depguard // postgres container adapter: the podman CLI seam (G204 sites justified below)
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/RigelBuild/compass/go/internal/stack"
)

// containerCommandTimeout bounds one-shot podman commands so a wedged CLI cannot block teardown indefinitely.
const containerCommandTimeout = 120 * time.Second

// defaultSocketDir is required by the stock image bootstrap while PGHOST is unset.
// Include it beside the DSN socket in unix_socket_directories; only the DSN socket belongs to compass-server.
const defaultSocketDir = "/var/run/postgresql"

// PostgresContainer implements both container-start and teardown seams using one stable container name.
type PostgresContainer struct {
	cli containerCLI
	// superuser must match the user-less DSN role; keep-id maps it to the host user.
	superuser string
}

// Compile-time proof the adapter satisfies both seams it fills.
var (
	_ stack.PostgresContainer   = (*PostgresContainer)(nil)
	_ stack.ContainerController = (*PostgresContainer)(nil)
)

// containerCLI is the podman lifecycle surface used by this adapter and its tests.
// The focused seam keeps argv assembly and process lifecycle testable without podman.
type containerCLI interface {
	run(ctx context.Context, args []string) error
	wait(ctx context.Context, name string) error
	stop(ctx context.Context, name string, timeout time.Duration) error
	term(ctx context.Context, name string) error
	remove(ctx context.Context, name string) error
	removeExited(ctx context.Context, name string) error
	exists(ctx context.Context, name string) (bool, error)
}

// NewPostgresContainer builds a PostgresContainer over the real podman CLI,
// resolving the current OS user as the container superuser (see the superuser
// field). A failure to resolve the OS user is returned rather than defaulting,
// because a wrong superuser silently breaks the DSN authentication S4 depends
// on — a loud failure at wiring time beats an opaque "role does not exist" at
// first probe.
func NewPostgresContainer() (*PostgresContainer, error) {
	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("resolving OS user for the postgres container superuser: %w", err)
	}
	return &PostgresContainer{cli: newPodmanExec(), superuser: u.Username}, nil
}

// Start runs the S4 container-backed postgres detached and returns a Process
// handle for the in-process lifecycle. The host socket dir is created before the
// run so the podman bind-mount source exists; the data dir is created by the
// image entrypoint's initdb on the bind-mount. Start returns at launch, not at
// readiness — the core's waitPostgres poll is the accept gate.
func (c *PostgresContainer) Start(ctx context.Context, spec stack.PostgresContainerSpec) (stack.Process, error) {
	// The bind-mount SOURCE must pre-exist or `podman run` fails with a statfs
	// error. The data dir the image's initdb creates/populates; the socket dir
	// must be here first (0700: app-private, reachable only by the owning user).
	if err := os.MkdirAll(spec.SocketDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating postgres socket dir %q: %w", spec.SocketDir, err)
	}
	if err := os.MkdirAll(spec.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating postgres data dir %q: %w", spec.DataDir, err)
	}
	if err := c.cli.run(ctx, runArgs(spec, c.superuser)); err != nil {
		return nil, fmt.Errorf("podman run postgres %q: %w", spec.Name, err)
	}
	return &containerProcess{cli: c.cli, name: spec.Name, stopTimeout: spec.StopTimeout}, nil
}

// Exists reports whether the named container is present (stack.ContainerController).
// A genuine podman engine error (neither the exit-0 "present" nor the exit-1
// "absent" verdict — a wedged daemon, a restart mid-inspect) is treated as
// PRESENT, not absent: entryAlive gates whether a teardown target is built at
// all, so a false "absent" here drops the target entirely — no Stop, no Remove,
// and the socket-quiescence confirm never runs — while the record has already
// been consumed, stranding a live container the down then reports as gone.
// Assuming-present is safe: Stop/Remove are idempotent (an already-gone
// container normalizes to success), so signaling a container that turns out gone
// is harmless, while signaling a still-live one is the whole point.
func (c *PostgresContainer) Exists(ctx context.Context, name string) bool {
	present, err := c.cli.exists(ctx, name)
	if err != nil {
		return true // cannot confirm absence → assume present and drive teardown
	}
	return present
}

// Stop sends the container its stop signal without waiting (stack.ContainerController).
func (c *PostgresContainer) Stop(ctx context.Context, name string) error {
	return c.cli.term(ctx, name)
}

// RemoveExited removes the container once it has exited (stack.ContainerController).
func (c *PostgresContainer) RemoveExited(ctx context.Context, name string) error {
	return c.cli.removeExited(ctx, name)
}

// Remove force-removes the container, the SIGKILL-tier escalation
// (stack.ContainerController): `podman rm -f`.
func (c *PostgresContainer) Remove(ctx context.Context, name string) error {
	return c.cli.remove(ctx, name)
}

// runArgs builds the detached podman command; tests cover its flags without spawning podman.
// Graceful stop removes the container; replacement clears stale names; keep-id maps the host data owner.
// Stop timeout exceeds server drain grace, while the socket mounts and POSTGRES_USER match the user-less DSN.
// The bootstrap and DSN sockets are listed; the server listens only on sockets.
func runArgs(spec stack.PostgresContainerSpec, superuser string) []string {
	pgdata := "/pgdata"
	return []string{
		cmdPodmanRun, flagPodmanDetach,
		flagPodmanRM,
		flagPodmanReplace,
		flagPodmanName, spec.Name,
		"--userns=keep-id",
		flagPodmanStopTimeout, strconv.FormatInt(stopSeconds(spec.StopTimeout), 10),
		"-e", "POSTGRES_DB=" + postgresDB,
		"-e", "POSTGRES_HOST_AUTH_METHOD=trust",
		"-e", "POSTGRES_USER=" + superuser,
		"-e", "PGDATA=" + pgdata,
		"-v", spec.DataDir + ":" + pgdata + ":Z",
		"-v", spec.SocketDir + ":" + spec.SocketDir + ":Z",
		spec.Image,
		"-c", "unix_socket_directories=" + defaultSocketDir + "," + spec.SocketDir,
		"-c", "listen_addresses=",
		"-p", spec.Port,
	}
}

// These podman flags are shared by all container argv builders and live beside their common CLI surface.
const (
	// cmdPodmanRun is the podman subcommand every argv builder leads with.
	cmdPodmanRun = "run"
	// flagPodmanDetach runs the container in the background, returning at launch.
	flagPodmanDetach = "--detach"
	// flagPodmanRM removes the container after exit, including graceful stop.
	flagPodmanRM = "--rm"
	// flagPodmanReplace clears stale containers with the stable name before a fresh up.
	flagPodmanReplace = "--replace"
	// flagPodmanName sets the component's persisted teardown identity.
	flagPodmanName = "--name"
	// flagPodmanStopTimeout sets the default grace when stop has no explicit timeout.
	flagPodmanStopTimeout = "--stop-timeout"
	flagPodmanEnv         = "-e"
	flagPodmanVolume      = "-v"
	flagPodmanRestart     = "--restart"
)

// postgresDB is created by POSTGRES_DB and opened by compass-server's DSN.
// Kept local so the adapter's argv contract is self-contained.
const postgresDB = "compass"

// stopSeconds rounds positive durations up so sub-second grace never becomes an immediate SIGKILL.
func stopSeconds(timeout time.Duration) int64 {
	return int64(math.Max(math.Ceil(timeout.Seconds()), 0))
}

// containerProcess maps Process signals and waits to podman; detached teardown uses the persisted name, not a host pid.
type containerProcess struct {
	cli         containerCLI
	name        string
	stopTimeout time.Duration
}

// Compile-time proof the handle satisfies the core seam.
var _ stack.Process = (*containerProcess)(nil)

// Signal requests a graceful stop; hard removal belongs to detached teardown, not the in-process Process contract.
func (p *containerProcess) Signal(ctx context.Context, sig stack.ProcessSignal) error {
	switch sig {
	case stack.SignalTerm:
		if err := p.cli.stop(ctx, p.name, p.stopTimeout); err != nil {
			return fmt.Errorf("podman stop postgres %q: %w", p.name, err)
		}
		return nil
	default:
		return fmt.Errorf("unknown process signal %d", int(sig))
	}
}

// Wait blocks on podman wait; a removed or stopped container has completed its drain.
func (p *containerProcess) Wait(ctx context.Context) error {
	return p.cli.wait(ctx, p.name)
}

// Pid returns a sentinel: containers have no host pid, so teardown persists and uses the container name.
func (p *containerProcess) Pid() int { return 0 }

// podmanExec implements this stack's narrower container lifecycle, distinct from runtime.PodmanCLI for agent containers.
type podmanExec struct {
	program string
	timeout time.Duration
}

// newPodmanExec builds a podmanExec invoking `podman` on PATH.
func newPodmanExec() *podmanExec {
	return &podmanExec{program: "podman", timeout: containerCommandTimeout}
}

// run requires a successful podman exit; cancellation is withheld because interrupted netns setup can leave pasta without resolv.conf.
func (e *podmanExec) run(ctx context.Context, args []string) error {
	return e.fireAndCheckWithCancellation(ctx, args, true)
}

// wait blocks for container exit; an absent container is already complete.
func (e *podmanExec) wait(ctx context.Context, name string) error {
	if err := e.fireAndCheck(ctx, []string{"wait", name}); err != nil {
		if isNoSuchContainer(err) {
			return nil
		}
		return err
	}
	return nil
}

// stop requests a graceful stop within timeout; an absent container is already stopped.
func (e *podmanExec) stop(ctx context.Context, name string, timeout time.Duration) error {
	if err := e.fireAndCheck(ctx, []string{"stop", "--time", strconv.FormatInt(stopSeconds(timeout), 10), name}); err != nil {
		if isNoSuchContainer(err) {
			return nil
		}
		return err
	}
	return nil
}

// term sends the configured stop signal without waiting so the caller owns the drain budget.
// The signal is inspected because podman kill cannot use the configured signal directly.
func (e *podmanExec) term(ctx context.Context, name string) error {
	sig, err := e.output(ctx, []string{"container", "inspect", "--format", "{{.Config.StopSignal}}", name})
	if err == nil {
		if sig == "" {
			sig = "SIGTERM"
		}
		err = e.fireAndCheck(ctx, []string{"kill", "--signal", sig, name})
	}
	if err != nil && !isNoSuchContainer(err) {
		return err
	}
	return nil
}

// remove force-removes without the stop grace; an absent container is already removed.
func (e *podmanExec) remove(ctx context.Context, name string) error {
	if err := e.fireAndCheck(ctx, []string{"rm", "--force", "--time", "0", "--volumes", name}); err != nil {
		if isNoSuchContainer(err) {
			return nil
		}
		return err
	}
	return nil
}

// removeExited uses non-forced rm; running and absent containers are left for the caller's next poll.
func (e *podmanExec) removeExited(ctx context.Context, name string) error {
	if err := e.fireAndCheck(ctx, []string{"rm", "--volumes", name}); err != nil {
		if isNoSuchContainer(err) || isContainerRunning(err) {
			return nil
		}
		return err
	}
	return nil
}

// exists reads podman's exit code: 0 means present and 1 means absent.
func (e *podmanExec) exists(ctx context.Context, name string) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, e.program, "container", "exists", name) //nolint:gosec // G204: the container seam — program is the operator-set engine and name is a state-dir-derived container name, neither attacker-controlled
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("podman container exists %q: %w", name, err)
}

// fireAndCheck runs `podman <args>` under the command timeout, folding a
// non-zero exit into an error carrying the captured stderr for diagnosis.
func (e *podmanExec) fireAndCheck(ctx context.Context, args []string) error {
	return e.fireAndCheckWithCancellation(ctx, args, false)
}

func (e *podmanExec) fireAndCheckWithCancellation(ctx context.Context, args []string, detached bool) error {
	commandCtx := ctx
	if detached {
		commandCtx = context.WithoutCancel(ctx)
	}
	cctx, cancel := context.WithTimeout(commandCtx, e.timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, e.program, args...) //nolint:gosec // G204: the container seam — program is the operator-set engine and args are Stack-built from a state-dir-derived spec, neither attacker-controlled
	cmd.WaitDelay = podmanWaitDelay
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("podman %s: %w", args[0], runErr)
		}
		return fmt.Errorf("podman %s: %w: %s", args[0], runErr, msg)
	}
	return nil
}

// output runs `podman <args>` under the command timeout and returns its trimmed
// stdout, folding a non-zero exit into an error carrying the captured stderr.
func (e *podmanExec) output(ctx context.Context, args []string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, e.program, args...) //nolint:gosec // G204: same seam as fireAndCheck; args are Stack-built from a state-dir-derived name
	cmd.WaitDelay = podmanWaitDelay
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("podman %s: %w: %s", args[0], err, msg)
		}
		return "", fmt.Errorf("podman %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

// podmanWaitDelay bounds how long a podman call waits on stdio a child still
// holds open after podman exits or is killed, so a leaked pipe cannot hang down.
const podmanWaitDelay = 2 * time.Second

// isNoSuchContainer reports whether err is podman's "no such container" (the
// container vanished in the verify→signal gap, or was already gone). The
// teardown treats it as success — the container is gone, which is the goal.
func isNoSuchContainer(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such container")
}

// isContainerRunning reports whether err is podman refusing a non-forced rm of a
// running or paused container ("container state improper").
func isContainerRunning(err error) bool {
	return err != nil && strings.Contains(err.Error(), "container state improper")
}
