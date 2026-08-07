//go:build unix && gtk3

// The embedded-mode launch pipeline (SEA-1685 T4.1). Embedded mode wires the
// native shell end-to-end before the window opens: host preflight → spawn and
// supervise the private stack (via the compass-stack CLI) → learn the caller
// account id (WhoAmI, DL-111) → hand the resolved socket + account id to the
// bridge/UI. It is the composition root that supplies the real external effects
// (podman/postgres probes, the compass-stack exec, the h2c-UDS WhoAmI dial)
// behind small injected seams, so the orchestration is unit-testable without a
// real stack.
//
// This file supervises the stack through the compass-stack BINARY (frozen
// design §T4: "consumes T2's compass-stack CLI"), not by importing
// go/internal/stack: `compass-stack up` brings the stack to Ready and exits 0
// while the children keep running (fire-and-return), so the pipeline runs it,
// waits for exit 0, and then dials the same socket it passed as --socket.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
	"github.com/sealedsecurity/compass/go/internal/appconfig"
	"github.com/sealedsecurity/compass/go/internal/preflight"
)

// defaultAgentImage is the canonical agent image ref the embedded stack runs
// when no --image/$COMPASS_AGENT_IMAGE is supplied. The ref is locked
// (docs/designs/platform/compass-agent-image-publish.md §Ref); the native app
// does not bundle the image (DL-112) — compass-stack podman-pulls it from GHCR
// at first run.
const defaultAgentImage = "ghcr.io/sealedsecurity/compass-agent:latest"

// errClientNotImplemented is the native-client (T5) mode's placeholder outcome.
// Client mode is a later slice; embedded mode is this one. It is a sentinel so
// the mode branch is assertable in tests without matching on a message string.
var errClientNotImplemented = errors.New("native-client mode is not yet implemented (T5)")

// embeddedPipeline is the embedded-mode launch pipeline over its injected
// external effects. Each field is one genuine effect the real launch supplies
// (preflight run, the compass-stack up exec, the WhoAmI dial); a test supplies
// deterministic stubs, so the orchestration — order, short-circuit, and the
// argv it builds — is verified with no real podman/postgres/stack/exec.
type embeddedPipeline struct {
	// preflight runs the host precondition checks and folds any failures into a
	// single legible error (the real seam wraps preflight.Deps.Run(...).Err()).
	preflight func(ctx context.Context) error
	// stackUp runs `compass-stack up` with the given argv and waits for it to
	// exit 0 (fire-and-return); a non-zero exit is returned as an error carrying
	// the captured stderr.
	stackUp func(ctx context.Context, args []string) error
	// whoAmI dials the stack socket over h2c-UDS and returns the caller account
	// id (WhoAmI, DL-111 — server-derived, never supplied).
	whoAmI func(ctx context.Context, socket string) (string, error)
}

// embeddedParams is the resolved input to one embedded launch: the single
// socket path the stack serves and the pipeline then dials, the stack argv
// inputs, and the DSN the preflight DB probe used (informational; the argv omits
// --database so compass-stack recomputes the identical default from --state-dir).
type embeddedParams struct {
	// socket is resolveSocket()'s result — the SAME value passed to
	// `--socket` and dialed for WhoAmI (and, upstream, the bridge pump).
	socket string
	// stateDir is the app state directory passed to `--state-dir`.
	stateDir string
	// image is the agent image ref passed to `--image`.
	image string
}

// launchByMode dispatches the resolved app mode: embedded mode runs the
// pipeline (returning the caller account id), client mode returns the T5
// not-implemented sentinel WITHOUT touching any pipeline effect. Any other mode
// value is a programming error (appconfig only ever yields the two).
func launchByMode(ctx context.Context, mode appconfig.Mode, pipeline embeddedPipeline, params embeddedParams) (string, error) {
	switch mode {
	case appconfig.ModeEmbedded:
		return pipeline.run(ctx, params)
	case appconfig.ModeClient:
		return "", errClientNotImplemented
	default:
		return "", fmt.Errorf("unknown app mode %v", mode)
	}
}

// run executes the embedded launch in order: preflight → stack up → WhoAmI. A
// preflight failure short-circuits (the stack is never spawned) and returns the
// aggregated legible error verbatim. On success it returns the resolved caller
// account id.
func (p embeddedPipeline) run(ctx context.Context, params embeddedParams) (string, error) {
	if err := p.preflight(ctx); err != nil {
		return "", err
	}

	args := stackUpArgs(params)
	if err := p.stackUp(ctx, args); err != nil {
		return "", err
	}
	slog.Info("stack ready", "socket", params.socket)

	accountID, err := p.whoAmI(ctx, params.socket)
	if err != nil {
		return "", fmt.Errorf("resolving caller identity over %s: %w", params.socket, err)
	}
	slog.Info("caller identity resolved", "account", accountID)
	return accountID, nil
}

// stackUpArgs builds the `compass-stack up` argv from the resolved params. It is
// pure (no I/O, no exec) so the exact invocation is unit-testable without
// running anything — mirroring cmd/compass-stack's pure resolveConfig. --database
// is deliberately omitted: compass-stack computes the identical default DSN from
// --state-dir (cmd/compass-stack/main.go defaultDSN), so passing it would
// duplicate that logic.
func stackUpArgs(p embeddedParams) []string {
	args := []string{
		"up",
		"--state-dir", p.stateDir,
		"--image", p.image,
		"--socket", p.socket,
	}
	return args
}

// runStackUp is the real stackUp seam: it execs the compass-stack binary at bin
// with the given argv and waits for it to exit 0 (up is fire-and-return, so
// Run returning nil means the stack reached Ready and its children keep
// running). A non-zero exit is surfaced with the captured stderr so the failure
// copy is legible.
func runStackUp(bin string) func(ctx context.Context, args []string) error {
	return func(ctx context.Context, args []string) error {
		//nolint:gosec // G204: bin is operator/PATH-resolved (resolveStackBin) and
		// the argv is pipeline-assembled (stackUpArgs), not user input.
		cmd := exec.CommandContext(ctx, bin, args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			if ctx.Err() == context.DeadlineExceeded || errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("compass-stack up exceeded the %s bring-up window "+
					"(a cold agent-image pull from GHCR can take longer on first run): %w", bringUpTimeout, err)
			}
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return fmt.Errorf("compass-stack up failed: %w: %s", err, msg)
			}
			return fmt.Errorf("compass-stack up failed: %w", err)
		}
		return nil
	}
}

// whoAmIOverUDS is the real whoAmI seam: it dials the stack socket over
// prior-knowledge cleartext HTTP/2 (the same door compass-server serves) and
// calls WhoAmI, returning the server-derived caller account id. The transport
// shape mirrors internal/stack/adapters/health.go (the established h2c-UDS
// connect dial).
func whoAmIOverUDS(ctx context.Context, socket string) (string, error) {
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

	client := compassv1connect.NewCompassServiceClient(&http.Client{Transport: transport}, "http://unix")
	resp, err := client.WhoAmI(ctx, connect.NewRequest(&compassv1.WhoAmIRequest{}))
	if err != nil {
		return "", err
	}
	id := resp.Msg.GetAccountId()
	if id == "" {
		return "", errors.New("WhoAmI returned an empty account id")
	}
	return id, nil
}

// resolveMode resolves the --mode/$COMPASS_APP_MODE override to feed
// appconfig.Load. An empty flag falls back to the env; both empty is "no
// override" (Load then uses app.toml, else embedded).
func resolveMode(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv("COMPASS_APP_MODE")
}

// resolveStackBin picks the compass-stack binary to supervise the stack with:
// the --compass-stack flag, else $COMPASS_STACK_BIN, else compass-stack on
// $PATH, else a compass-stack sibling of the running executable (where a
// packaged build stages it, mirroring resolveAssetsDir's beside-the-executable
// pattern). A legible error names every place it looked when none resolves.
func resolveStackBin(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if env := os.Getenv("COMPASS_STACK_BIN"); env != "" {
		return env, nil
	}
	if p, err := exec.LookPath("compass-stack"); err == nil {
		return p, nil
	}
	if exe, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(exe), "compass-stack")
		if info, statErr := os.Stat(sibling); statErr == nil && !info.IsDir() {
			return sibling, nil
		}
	}
	return "", errors.New("compass-stack binary not found: pass --compass-stack, set $COMPASS_STACK_BIN, " +
		"put compass-stack on $PATH, or stage it beside the compass-app executable")
}

// resolveStateDir picks the app state directory: the --state-dir flag, else
// $COMPASS_STATE_DIR, else $XDG_STATE_HOME/compass, else $HOME/.compass. A
// relative XDG_STATE_HOME is treated as unset (matching resolveSocket's handling
// of a relative XDG_RUNTIME_DIR), so the fallback is deterministic.
func resolveStateDir(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("COMPASS_STATE_DIR"); env != "" {
		return env
	}
	if stateHome := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(stateHome) {
		return filepath.Join(stateHome, "compass")
	}
	return filepath.Join(os.Getenv("HOME"), ".compass")
}

// resolveImage picks the agent image ref: the --image flag, else
// $COMPASS_AGENT_IMAGE (the same env compass-runner honors), else the locked
// GHCR default.
func resolveImage(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("COMPASS_AGENT_IMAGE"); env != "" {
		return env
	}
	return defaultAgentImage
}

// embeddedDatabaseDSN is a DELIBERATE second copy of cmd/compass-stack's
// defaultDSN (go/cmd/compass-stack/main.go defaultDSN — the source of truth):
// the keyword/value DSN for the private postgres reachable over a unix socket
// under the state dir. It is duplicated here because the app's preflight DB
// probe needs the DSN BEFORE compass-stack runs; the compass-stack up argv
// omits --database so the CLI recomputes this identical default from
// --state-dir. The two formulas must stay in lockstep
// (TestEmbeddedDatabaseDSNMatchesCompassStackDefault guards this side).
// Consolidating both into one importable helper is a tracked follow-up
// (SEA-1856).
func embeddedDatabaseDSN(stateDir string) string {
	sockDir := filepath.Join(stateDir, "postgres", "sock")
	return fmt.Sprintf("host=%s port=5432 dbname=compass sslmode=disable", sockDir)
}

// realPreflight builds the preflight seam over the real host-probe adapters,
// classified at this T4 wiring boundary (see classifyPreflight).
func realPreflight(image, dsn string) func(ctx context.Context) error {
	deps := preflight.Deps{
		GOOS:             runtime.GOOS,
		CurrentUID:       os.Getuid(),
		ExpectedAgentUID: preflight.DefaultAgentUID,
		PodmanRootless:   podmanRootless,
		ImagePresent:     imagePresent,
		DBReachable:      dbReachable,
	}
	params := preflight.Params{AgentImage: image, DatabaseDSN: dsn}
	return func(ctx context.Context) error {
		return classifyPreflight(deps.Run(ctx, params))
	}
}

// classifyPreflight splits the preflight results by severity at the T4 wiring
// boundary and returns only the FATAL failures folded into one legible error
// (nil when none are fatal).
//
// The split is load-bearing for embedded mode's zero-config cold start. `up`
// (compass-stack) is what STARTS postgres (design.md:176) and PULLS the agent
// image (internal/stack/adapters/image.go EnsureImage), so on a fresh state dir
// the DB and image checks NECESSARILY fail before `up` has run — gating on them
// would make the app unable to cold-start, breaking the zero-config charter. And
// they need no post-up re-check: `up`-Ready is GetServerInfo answering, which
// requires migrations against a live postgres AND the runner booting on the
// pulled image (design.md:189-191), so reaching Ready transitively verifies
// both.
//
//   - FATAL (host capabilities `up` cannot create): OS, UID, rootless podman.
//   - ADVISORY (`up` ensures them, `up`-Ready verifies them): image, database —
//     logged at Warn, never fatal.
//
// PARKED as an SEA-1685 Open Question: whether this severity split belongs in
// the preflight core's Err() rather than here at the boundary. Kept at the
// boundary for now so the core's Run/Err control flow (every failure surfaced)
// is unchanged and still serves the operator-facing "show every unmet
// precondition at once" use.
func classifyPreflight(results preflight.Results) error {
	var fatal preflight.Results
	for _, r := range results {
		if r.OK {
			continue
		}
		switch r.Name {
		case preflight.CheckImage, preflight.CheckDatabase:
			slog.Warn("preflight: precondition unmet; compass-stack up will ensure it",
				"check", r.Name, "detail", r.Detail)
		default:
			fatal = append(fatal, r)
		}
	}
	return fatal.Err()
}
