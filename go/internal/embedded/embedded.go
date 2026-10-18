// Package embedded is the embedded-mode launch pipeline. Embedded mode wires the native shell
// end-to-end before the window opens: host preflight → spawn and supervise the
// private stack (via the compass-stack CLI) → learn the caller account id
// (WhoAmI, DL-111) → hand the resolved socket + account id to the bridge/UI. It
// is the composition root that supplies the real external effects (podman
// probes, the compass-stack exec, the h2c-UDS WhoAmI dial) behind small injected
// seams, so the orchestration is unit-testable without a real stack.
//
// It supervises the stack through the compass-stack binary, not by using the
// stack package directly. The shared process exit code is the only dependency.
// compass-stack up exits 0 when its children reach Ready, then the pipeline
// dials the socket it passed as --socket to learn the caller account ID.
package embedded

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec" //nolint:depguard // stack seam: runs the resolved compass-stack binary for up/down
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/preflight"
	"github.com/RigelBuild/compass/go/internal/stack"
)

// ErrStackVersionMismatch marks an up failure caused by a live stack version mismatch.
var ErrStackVersionMismatch = errors.New("embedded stack version mismatch")

// DefaultAgentImage is the canonical agent image ref the embedded stack runs
// when no --image/$COMPASS_AGENT_IMAGE is supplied. The ref is locked
// (RigelBuild/docs:docs/designs/compass/infra/ci/compass-agent-image-publish/design.md, "the name/tag
// contract"); the native app does not bundle the image (DL-112) —
// compass-stack podman-pulls it from GHCR at first run.
const DefaultAgentImage = "ghcr.io/rigelbuild/compass-agent:latest"

// The compass-stack CLI flag names the embedded pipeline drives. Shared by
// stackUpArgs and StackDownArgs so the two argv builders cannot drift on a flag
// spelling (and so the strings are named once rather than repeated inline).
const (
	flagStateDir = "--state-dir"
	flagImage    = "--image"
	flagSocket   = "--socket"
)

// Pipeline is the embedded-mode launch pipeline over its injected
// external effects. Each field is one genuine effect the real launch supplies
// (preflight run, the compass-stack up exec, the WhoAmI dial); a test supplies
// deterministic stubs, so the orchestration — order, short-circuit, and the
// argv it builds — is verified with no real podman/stack/exec.
type Pipeline struct {
	// Preflight runs the host precondition checks and folds any failures into a
	// single legible error (the real seam wraps preflight.Deps.Run(...).Err()).
	Preflight func(ctx context.Context) error
	// StackUp runs `compass-stack up` with the given argv and waits for it to
	// exit 0 (fire-and-return); a non-zero exit is returned as an error carrying
	// the captured stderr.
	StackUp func(ctx context.Context, args []string) error
	// WhoAmI dials the stack socket over h2c-UDS and returns the caller account
	// id (WhoAmI, DL-111 — server-derived, never supplied).
	WhoAmI func(ctx context.Context, socket string) (string, error)
}

// Params is the resolved input to one embedded launch: the single socket
// path the stack serves and the pipeline then dials, plus the stack argv inputs.
// The argv omits --database so compass-stack recomputes the default DSN from
// --state-dir (the app carries no second DSN copy — §A2 reconciliation 1).
type Params struct {
	// Socket is resolveSocket()'s result — the SAME value passed to
	// `--socket` and dialed for WhoAmI (and, upstream, the bridge pump).
	Socket string
	// StateDir is the app state directory passed to `--state-dir`.
	StateDir string
	// Image is the agent image ref passed to `--image`.
	Image string
}

// Run executes the embedded launch in order: preflight → stack up → WhoAmI. A
// preflight failure short-circuits (the stack is never spawned) and returns the
// aggregated legible error verbatim. On success it returns the resolved caller
// account id.
func (p Pipeline) Run(ctx context.Context, params Params) (string, error) {
	if err := p.Preflight(ctx); err != nil {
		return "", err
	}

	args := stackUpArgs(params)
	if err := p.StackUp(ctx, args); err != nil {
		return "", err
	}
	slog.InfoContext(ctx, "stack ready", "socket", params.Socket)

	accountID, err := p.WhoAmI(ctx, params.Socket)
	if err != nil {
		return "", fmt.Errorf("resolving caller identity over %s: %w", params.Socket, err)
	}
	slog.InfoContext(ctx, "caller identity resolved", "account", accountID)
	return accountID, nil
}

// stackUpArgs returns the pure `compass-stack up` argv. It passes only
// state-dir, image, and socket; stack-owned defaults remain with the CLI.
func stackUpArgs(p Params) []string {
	args := []string{
		"up",
		flagStateDir, p.StateDir,
		flagImage, p.Image,
		flagSocket, p.Socket,
	}
	return args
}

// captureStderr uses a file, not a pipe: long-lived stack children inherit
// stderr and would keep os/exec's pipe-copy goroutine, and Wait, blocked.
func captureStderr(cmd *exec.Cmd) (read func() string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "compass-stack-stderr-*")
	if err != nil {
		return nil, nil, fmt.Errorf("creating stderr capture file: %w", err)
	}
	cmd.Stderr = f
	read = func() string {
		// Best-effort: an unreadable capture degrades to the generic
		// "compass-stack ... failed" error, and never blocks surfacing.
		b, _ := os.ReadFile(f.Name())
		return strings.TrimSpace(string(b))
	}
	cleanup = func() {
		// Deferred cleanup of a temp capture file: a Close/Remove error here is
		// not actionable (the process is past the point the stderr mattered), so
		// it is discarded.
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	return read, cleanup, nil
}

// RunStackUp is the real stackUp seam: it execs the compass-stack binary at bin
// with the given argv and waits for it to exit 0 (up is fire-and-return, so
// Run returning nil means the stack reached Ready and its children keep
// running). A non-zero exit is surfaced with the captured stderr so the failure
// copy is legible.
func RunStackUp(bin string) func(ctx context.Context, args []string) error {
	return func(ctx context.Context, args []string) error {
		//nolint:gosec // G204: bin is operator/PATH-resolved (ResolveStackBin) and
		// the argv is pipeline-assembled (stackUpArgs), not user input.
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = prependExecDirToPath(os.Environ(), filepath.Dir(bin))
		stderr, cleanup, capErr := captureStderr(cmd)
		if capErr != nil {
			return capErr
		}
		defer cleanup()
		if err := cmd.Run(); err != nil {
			if ctx.Err() == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("compass-stack up exceeded the %s bring-up window "+
					"(a cold first run pulls three images from their registries — the agent "+
					"image from GHCR, the postgres and collector images from their stock "+
					"registries — which can take longer): %w", BringUpTimeout, err)
			}
			if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == stack.VersionMismatchExitCode {
				if msg := stderr(); msg != "" {
					return fmt.Errorf("compass-stack up failed: %w: %s", ErrStackVersionMismatch, msg)
				}
				return fmt.Errorf("compass-stack up failed: %w", ErrStackVersionMismatch)
			}
			if msg := stderr(); msg != "" {
				return fmt.Errorf("compass-stack up failed: %w: %s", err, msg)
			}
			return fmt.Errorf("compass-stack up failed: %w", err)
		}
		return nil
	}
}

// StackDownArgs builds the `compass-stack down` argv from the resolved params.
// It mirrors stackUpArgs (pure, no I/O, no exec) so the exact teardown
// invocation is unit-testable without running anything. down parses the SAME
// config flags as up, and its resolveConfig REQUIRES a non-empty --state-dir AND
// --image (both rejected if empty), so --image is carried even though teardown
// does not pull an image — it is the config key compass-stack keys the stack's
// identity off. --database is omitted for the same reason as up (compass-stack
// recomputes the identical default DSN from --state-dir), and --linger is
// omitted because down is not lingerable (down's whole job is to tear the stack
// down, so a linger flag would be nonsense — compass-stack rejects it).
func StackDownArgs(p Params) []string {
	args := []string{
		"down",
		flagStateDir, p.StateDir,
		flagImage, p.Image,
		flagSocket, p.Socket,
	}
	return args
}

// RunStackDown is the real stackDown seam: it execs the compass-stack binary at
// bin with the given argv and waits for it to exit 0 (down attaches to the live
// stack, SIGTERMs the child tree, waits the server drain, and releases the
// lock). A non-zero exit is surfaced with the captured stderr so the failure
// copy is legible — mirroring RunStackUp's shape exactly.
func RunStackDown(bin string) func(ctx context.Context, args []string) error {
	return func(ctx context.Context, args []string) error {
		//nolint:gosec // G204: bin is operator/PATH-resolved (ResolveStackBin) and
		// the argv is pipeline-assembled (StackDownArgs), not user input.
		cmd := exec.CommandContext(ctx, bin, args...)
		// down has already consumed its teardown record, so a timeout must SIGTERM
		// it: SIGKILL would skip the survivor rewrite and leak the stack.
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = stackDownCancelGrace
		cmd.Env = prependExecDirToPath(os.Environ(), filepath.Dir(bin))
		stderr, cleanup, capErr := captureStderr(cmd)
		if capErr != nil {
			return capErr
		}
		defer cleanup()
		if err := cmd.Run(); err != nil {
			if ctx.Err() == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("compass-stack down exceeded the %s teardown window "+
					"(attach, SIGTERM the child tree, wait the server drain): %w", StackDownTimeout, err)
			}
			if msg := stderr(); msg != "" {
				return fmt.Errorf("compass-stack down failed: %w: %s", err, msg)
			}
			return fmt.Errorf("compass-stack down failed: %w", err)
		}
		return nil
	}
}

// WhoAmIOverUDS is the real whoAmI seam: it dials the stack socket over
// prior-knowledge cleartext HTTP/2 (the same door compass-server serves) and
// calls WhoAmI, returning the server-derived caller account id. The transport
// shape mirrors internal/stack/adapters/health.go (the established h2c-UDS
// connect dial).
func WhoAmIOverUDS(ctx context.Context, socket string) (string, error) {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{
		Protocols: protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	defer transport.CloseIdleConnections()

	client := compassv1connect.NewCompassServiceClient(
		connect.NewClient(
			connecthttp.NewTransport(
				&http.Client{Transport: transport},
				"http://unix",
				connecthttp.WithReadMaxBytes(0),
			),
		),
	)
	resp, err := client.WhoAmI(ctx, &compassv1.WhoAmIRequest{})
	if err != nil {
		return "", err
	}
	id := resp.GetAccountId()
	if id == "" {
		return "", errors.New("WhoAmI returned an empty account id")
	}
	return id, nil
}

// ResolveStackBin picks the compass-stack binary to supervise the stack with:
// the --compass-stack flag, else $COMPASS_STACK_BIN, else a compass-stack
// sibling of the running executable (where a packaged build stages it, mirroring
// resolveAssetsDir's beside-the-executable pattern), else compass-stack on
// $PATH. The sibling is preferred over $PATH so a packaged build's staged
// sidecar wins over any ambient compass-stack, while a dev-box build (no
// sibling) still falls through to $PATH. A legible error names every place it
// looked when none resolves.
func ResolveStackBin(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if env := os.Getenv("COMPASS_STACK_BIN"); env != "" {
		return env, nil
	}
	if exe, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(exe), "compass-stack")
		if info, statErr := os.Stat(sibling); statErr == nil && !info.IsDir() {
			return sibling, nil
		}
	}
	if p, err := exec.LookPath("compass-stack"); err == nil {
		return p, nil
	}
	return "", errors.New("compass-stack binary not found: pass --compass-stack, set $COMPASS_STACK_BIN, " +
		"put compass-stack on $PATH, or stage it beside the compass-app executable")
}

// prependExecDirToPath prefers bundled sidecars without replacing the ambient
// PATH. Only an absolute execDir is prepended; a bare binary name is a no-op.
func prependExecDirToPath(env []string, execDir string) []string {
	if !filepath.IsAbs(execDir) {
		return env
	}
	out := make([]string, len(env), len(env)+1)
	copy(out, env)
	for i, e := range out {
		if value, ok := strings.CutPrefix(e, "PATH="); ok {
			// A present-but-empty PATH takes execDir alone: appending the
			// separator would leave a trailing empty element, which exec
			// loaders read as the current directory.
			if value == "" {
				out[i] = "PATH=" + execDir
			} else {
				out[i] = "PATH=" + execDir + string(os.PathListSeparator) + value
			}
			return out
		}
	}
	return append(out, "PATH="+execDir)
}

// ResolveImage picks the agent image ref: the --image flag, else
// $COMPASS_AGENT_IMAGE (the same env compass-runner honors), else the locked
// GHCR default.
func ResolveImage(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("COMPASS_AGENT_IMAGE"); env != "" {
		return env
	}
	return DefaultAgentImage
}

// RealPreflight builds the preflight seam over the real host-probe adapters,
// classified at this wiring boundary (see classifyPreflight). The DB probe and
// the app-side DSN duplicate are gone (§A2 reconciliation 1): under DL-260
// postgres is a container the stack itself starts, so a pre-`up` reachability
// probe has no signal on the cold-start path — `up`-Ready is the DB
// verification. MachineReady comes from the per-OS machineReadyAdapter: on
// darwin it is the podman-machine ensure step (provision or start the Linux VM,
// then re-probe — design §A5), and on linux it is nil because there is no
// machine. The preflight core keys the check off GOOS and FAILS on darwin when
// the adapter is nil, so this wiring cannot regress into a silently-skipped
// check.
func RealPreflight(image string) func(ctx context.Context) error {
	deps := realPreflightDeps(runtime.GOOS)
	params := preflight.Params{AgentImage: image}
	return func(ctx context.Context) error {
		return classifyPreflight(ctx, deps.Run(ctx, params))
	}
}

// realPreflightDeps takes GOOS so tests can verify darwin wiring on any host.
// The same value selects core checks and the machine adapter.
func realPreflightDeps(goos string) preflight.Deps {
	return preflight.Deps{
		GOOS:           goos,
		PodmanRootless: podmanRootless,
		PodmanVersion:  podmanVersionAtLeastFloor,
		MachineReady:   machineReadyAdapter(goos),
		ImagePresent:   imagePresent,
	}
}

// classifyPreflight makes host-capability failures fatal and the image advisory.
// A fresh stack pulls the image; reaching Ready verifies migrations and runner
// startup, so an image precheck would block cold starts without adding coverage.
func classifyPreflight(ctx context.Context, results preflight.Results) error {
	var fatal preflight.Results
	for _, r := range results {
		if r.OK {
			continue
		}
		switch r.Name {
		case preflight.CheckImage:
			slog.WarnContext(ctx, "preflight: precondition unmet; compass-stack up will ensure it",
				"check", r.Name, "detail", r.Detail)
		default:
			fatal = append(fatal, r)
		}
	}
	return fatal.Err()
}

// StackDownTimeout bounds the explicit teardown (compass-stack down: attach,
// SIGTERM the child tree, wait the server drain, release the lock). The bring-up
// context is already cancelled by the time the window is open, so
// stopStackAndQuit roots a FRESH bounded context off the caller's rather than
// reusing it.
const StackDownTimeout = 60 * time.Second

// stackDownCancelGrace is how long a timed-out down gets after SIGTERM to
// rewrite its survivor record before os/exec escalates to SIGKILL.
const stackDownCancelGrace = 10 * time.Second

// BringUpTimeout bounds the whole embedded bring-up (preflight + compass-stack
// up + WhoAmI) as a backstop against a wedged launch; app.Run() itself is not
// context-bound. It is generous because a cold first run pulls THREE images —
// the agent image from GHCR plus the stock postgres and collector images
// (DL-260) — before the stack reaches Ready, so the window covers three
// sequential registry pulls, not one.
//
// On darwin the window is wider still. The machine ensure step runs inside it,
// and a cold `podman machine init` downloads a VM image before any of the
// above starts — minutes on its own, on a link whose speed we do not control.
// A budget that cannot fit the work it wraps is not a backstop; it is a
// deadline the first launch on a fresh Mac loses every time, and the error it
// produces names the timeout rather than the download. So darwin gets a window
// sized for cold provisioning plus the same three pulls. Both remain backstops
// against a wedge, not performance targets.
//
// The configured embedded bring-up runs before a window opens. First-run setup
// only runs preflight inside the visible chooser and saves the choice; the next
// launch takes the configured bring-up path.
var BringUpTimeout = BringUpTimeoutFor(runtime.GOOS)

// BringUpTimeoutFor returns the bring-up budget for the given host OS. It takes
// the OS as a parameter rather than reading runtime.GOOS so the per-OS choice
// is unit-testable from any host.
func BringUpTimeoutFor(goos string) time.Duration {
	if goos == "darwin" {
		return 15 * time.Minute
	}
	return 180 * time.Second
}
