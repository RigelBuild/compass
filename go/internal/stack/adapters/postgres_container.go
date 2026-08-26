//go:build unix

package adapters

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/RigelBuild/compass/go/internal/stack"
)

// containerCommandTimeout bounds each one-shot podman command (run/stop/rm/
// exists). A cold `podman run` of the postgres image (pull already done by
// then, but initdb-on-first-boot still runs) plus stop/rm are all far under
// this; it exists so a wedged podman surfaces as an error rather than blocking
// teardown forever, mirroring runtime.defaultCommandTimeout.
const containerCommandTimeout = 120 * time.Second

// defaultSocketDir is the compiled-in unix_socket_directories the stock postgres
// image's entrypoint bootstrap phase (docker_temp_server_start) connects its
// setup psql over, with PGHOST unset. It MUST stay in the server's
// unix_socket_directories alongside our bind-mounted DSN socket dir, or the
// entrypoint's createdb/initdb bootstrap fails to connect and the container
// exits before ever binding the DSN socket. The DSN socket dir is the one
// compass-server opens; this one exists only for the image's own bootstrap.
const defaultSocketDir = "/var/run/postgresql"

// PostgresContainer is the real stack.PostgresContainer AND stack.ContainerController:
// it starts the S4 container-backed postgres via `podman run` and tears it down
// by name. The two seams share one podman surface and one container-naming
// contract, so they live in one adapter — the start side records the name the
// teardown side later signals.
type PostgresContainer struct {
	cli containerCLI
	// superuser is the postgres role POSTGRES_USER creates. It MUST equal the
	// role a user-less DSN connects as so the frozen S4 DSN (host=<dir> port=<p>
	// dbname=compass sslmode=disable — no user=) authenticates: pgx resolves a
	// user-less DSN to the OS user, and under --userns=keep-id the container
	// runs as that same host user, so the createdb superuser must be it too.
	// The stock image otherwise defaults POSTGRES_USER=postgres, which a
	// user-less DSN would fail against (role "<osuser>" does not exist). See the
	// T8 DESIGN FORK note: S4 enumerated the env without POSTGRES_USER, but the
	// byte-identical-DSN invariant it froze forces this addition.
	superuser string
}

// Compile-time proof the adapter satisfies both seams it fills.
var (
	_ stack.PostgresContainer   = (*PostgresContainer)(nil)
	_ stack.ContainerController = (*PostgresContainer)(nil)
)

// containerCLI is the narrow podman surface this adapter needs: run a detached
// container, block on its exit, stop/remove/exists it by name. *podmanExec
// satisfies it; a fake satisfies it in tests, so the argv assembly and the
// Process lifecycle are unit-testable without a real podman.
type containerCLI interface {
	run(ctx context.Context, args []string) error
	wait(ctx context.Context, name string) error
	stop(ctx context.Context, name string, timeout time.Duration) error
	remove(ctx context.Context, name string) error
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
// A podman error is treated as not-present: the teardown's socket-quiescence
// confirm is the verdict, so a transient inspect failure must not strand the
// down by reporting a phantom-live container.
func (c *PostgresContainer) Exists(name string) bool {
	present, err := c.cli.exists(context.Background(), name)
	if err != nil {
		return false
	}
	return present
}

// Stop requests a graceful `podman stop -t <timeout>` (stack.ContainerController).
func (c *PostgresContainer) Stop(name string, timeout time.Duration) error {
	return c.cli.stop(context.Background(), name, timeout)
}

// Remove force-removes the container, the SIGKILL-tier escalation
// (stack.ContainerController): `podman rm -f`.
func (c *PostgresContainer) Remove(name string) error {
	return c.cli.remove(context.Background(), name)
}

// runArgs assembles the S4 `podman run` argv (detached). Split out as a pure
// function so the full flag/env set is unit-tested without spawning podman,
// mirroring runtime.createArgs. The contract (S4, verified against
// docker.io/library/postgres:18):
//
//   - --rm: the container auto-removes when it exits, so a graceful `podman stop`
//     (the SIGTERM teardown tier, whether in-process Down or a fresh
//     cross-process down) both stops AND removes it — the acceptance's
//     "container gone" is a plain stop, not the SIGKILL escalation. The
//     ContainerController.Remove (`podman rm -f`) escalation tier stays for the
//     stop-ignored case and is idempotent on an already-removed container.
//   - --replace: idempotent name reuse. Should a container of this name survive
//     (a crash before --rm fired, an escalation race), --replace clears it so a
//     fresh up never collides on the stable per-state-dir name.
//   - --userns=keep-id: the S4 rootless uid map. postgres refuses uid 0 and the
//     bind-mounted data dir is host-user-owned, so the container user must map
//     to the host user.
//   - --stop-timeout: the safe default for any `podman stop` that names no -t;
//     >= the wrapper's 30s smart-shutdown grace so podman never SIGKILLs a still-
//     draining cluster (the detached-down path passes its own explicit -t).
//   - env POSTGRES_DB / POSTGRES_HOST_AUTH_METHOD / POSTGRES_USER / PGDATA (see
//     the superuser field for why POSTGRES_USER is required beyond S4's list).
//   - -v data dir -> PGDATA and the socket dir -> itself (same path both sides,
//     so the host DSN host=<dir> resolves the byte-identical socket).
//   - server args: unix_socket_directories lists BOTH the image's compiled
//     default (for the entrypoint bootstrap) and our bind-mounted DSN dir;
//     listen_addresses=” is socket-only (no TCP); -p is the DSN port.
func runArgs(spec stack.PostgresContainerSpec, superuser string) []string {
	pgdata := "/pgdata"
	return []string{
		"run", "--detach",
		"--rm",
		"--replace",
		"--name", spec.Name,
		"--userns=keep-id",
		"--stop-timeout", strconv.FormatInt(stopSeconds(spec.StopTimeout), 10),
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

// postgresDB is the single database the private store holds, created by the
// image entrypoint from POSTGRES_DB. It matches the dbname compass-server's DSN
// opens; kept here beside the argv builder rather than imported from the core so
// the adapter's argv contract is self-contained (the core's containerDBName is
// the same literal — a two-line contract, not worth a cross-package export).
const postgresDB = "compass"

// stopSeconds converts the stop-timeout Duration to podman's whole-second flag,
// rounding a positive duration up so a sub-second grace never truncates to 0 (an
// immediate SIGKILL), and clamping negatives to 0. Mirrors
// runtime.stopGraceSeconds.
func stopSeconds(timeout time.Duration) int64 {
	return int64(math.Max(math.Ceil(timeout.Seconds()), 0))
}

// containerProcess is the in-process Process handle over a running container. It
// maps the stack.Process contract onto podman: Signal(SignalTerm) -> podman
// stop, Wait -> podman wait (block until exit), Pid -> a sentinel (a container
// has no host pid the stack uses; its teardown identity is its name, persisted
// as a v2 container entry, never a pgid).
type containerProcess struct {
	cli         containerCLI
	name        string
	stopTimeout time.Duration
}

// Compile-time proof the handle satisfies the core seam.
var _ stack.Process = (*containerProcess)(nil)

// Signal requests a graceful stop: `podman stop -t <stopTimeout>`. SignalKill is
// not a valid in-process disposition (the cross-process teardown escalates via
// ContainerController.Remove instead), matching the process handle which also
// rejects anything but SignalTerm.
func (p *containerProcess) Signal(sig stack.ProcessSignal) error {
	switch sig {
	case stack.SignalTerm:
		if err := p.cli.stop(context.Background(), p.name, p.stopTimeout); err != nil {
			return fmt.Errorf("podman stop postgres %q: %w", p.name, err)
		}
		return nil
	default:
		return fmt.Errorf("unknown process signal %d", int(sig))
	}
}

// Wait blocks until the container exits or ctx is done, via `podman wait`. A
// container this process stopped via Signal exits cleanly, so the wait returns;
// a wait error after our stop is not actionable during drain (the container is
// gone or going), so it is normalized to nil, mirroring the process handle's
// stop-then-any-exit-is-clean drain semantics.
func (p *containerProcess) Wait(ctx context.Context) error {
	return p.cli.wait(ctx, p.name)
}

// Pid reports a sentinel: a rootless container runs beneath conmon, outside this
// process's group, so it has no host pid the stack persists as a pgid. The core
// never records a container child's Pid (startPostgresContainer records a v2
// container entry keyed by name instead), so this is never read as an identity.
func (p *containerProcess) Pid() int { return 0 }

// podmanExec is the real containerCLI: a thin shell over the podman binary on
// PATH. It is deliberately independent of internal/runtime's PodmanCLI (which is
// the agent-container substrate with a different lifecycle surface): the stack's
// postgres container needs run-detached + wait + stop/rm/exists, a smaller and
// differently-shaped set, so a focused seam here beats bending the agent runtime
// to a second caller.
type podmanExec struct {
	program string
	timeout time.Duration
}

// newPodmanExec builds a podmanExec invoking `podman` on PATH.
func newPodmanExec() *podmanExec {
	return &podmanExec{program: "podman", timeout: containerCommandTimeout}
}

// run runs `podman <args>` to completion, requiring a zero exit.
func (e *podmanExec) run(ctx context.Context, args []string) error {
	return e.fireAndCheck(ctx, args)
}

// wait blocks until the named container exits (`podman wait <name>`). A
// non-existent container is a completed wait, not an error: the caller Waits
// after a Signal(stop), and a container removed under it (or already gone) is
// the exit we were waiting for.
func (e *podmanExec) wait(ctx context.Context, name string) error {
	if err := e.fireAndCheck(ctx, []string{"wait", name}); err != nil {
		if isNoSuchContainer(err) {
			return nil
		}
		return err
	}
	return nil
}

// stop requests a graceful stop bounded by timeout (`podman stop -t <secs>`). A
// non-existent container is already stopped — not an error.
func (e *podmanExec) stop(ctx context.Context, name string, timeout time.Duration) error {
	if err := e.fireAndCheck(ctx, []string{"stop", "--time", strconv.FormatInt(stopSeconds(timeout), 10), name}); err != nil {
		if isNoSuchContainer(err) {
			return nil
		}
		return err
	}
	return nil
}

// remove force-removes the container (`podman rm -f`). An absent container is
// already removed — not an error.
func (e *podmanExec) remove(ctx context.Context, name string) error {
	if err := e.fireAndCheck(ctx, []string{"rm", "--force", "--volumes", name}); err != nil {
		if isNoSuchContainer(err) {
			return nil
		}
		return err
	}
	return nil
}

// exists reports whether a container with name is present in any state. `podman
// container exists` encodes the answer in its exit code (0 present, 1 absent),
// so it is read from the exit code, not treated as an error.
func (e *podmanExec) exists(ctx context.Context, name string) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, e.program, "container", "exists", name) //nolint:gosec // G204: the container seam — program is the operator-set engine and name is a state-dir-derived container name, neither attacker-controlled
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("podman container exists %q: %w", name, err)
}

// fireAndCheck runs `podman <args>` under the command timeout, folding a
// non-zero exit into an error carrying the captured stderr for diagnosis.
func (e *podmanExec) fireAndCheck(ctx context.Context, args []string) error {
	cctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, e.program, args...) //nolint:gosec // G204: the container seam — program is the operator-set engine and args are Stack-built from a state-dir-derived spec, neither attacker-controlled
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return fmt.Errorf("podman %s: %w", args[0], err)
		}
		return fmt.Errorf("podman %s: %w: %s", args[0], err, msg)
	}
	return nil
}

// isNoSuchContainer reports whether err is podman's "no such container" (the
// container vanished in the verify→signal gap, or was already gone). The
// teardown treats it as success — the container is gone, which is the goal.
func isNoSuchContainer(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such container")
}
