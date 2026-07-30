//go:build unix

// The Runner's top-level orchestration: Run dials the Server, enrolls, and
// drives the Sessions command loop until the context is cancelled or the link
// drops. It is the entry point cmd/compass-runner wraps — the binary is a thin
// flag/env shell over Run, mirroring how cmd/compass-server wraps server.Serve.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sealedsecurity/compass/go/internal/runtime"
)

// sunPathMax is the longest NUL-terminated path an AF_UNIX address can hold on
// this platform, derived from the kernel's own sockaddr_un rather than a
// literal: the sun_path array is 108 bytes on Linux but 104 on darwin and the
// BSDs, and one byte is spent on the terminator. A hard-coded 107 would silently
// over-admit on every BSD.
//
// This startup check is currently the only sun_path guard in the tree: it runs
// once at boot and validates a MODEL of the worst-case agent socket path (the
// widest container name the Runner can mint under this runtime dir), not each
// real path at its bind site. A per-request check at the bind site is arriving
// separately in #975 and becomes the backstop once merged; until then a path
// the model does not anticipate still reaches the kernel unchecked. The
// constant is derived locally rather than shared across the package boundary
// because it is a property of the OS, not of either package.
const sunPathMax = len(syscall.RawSockaddrUnix{}.Path) - 1

// agentAccountIDWidth is the character width of a server-minted agent account
// id: 16 random bytes hex-encoded, fixed at the minting site (store/ids.go
// newID), hence exactly 32 chars. The Runner never shortens or truncates it, so
// it is a constant contributor to every agent socket path.
const agentAccountIDWidth = 32

// AgentContainerNamePrefix is the container-name prefix prepended to the agent
// account id to form the container name (spec.go BuildSpec). The Runner binary
// wires it into SpecDefaults.NamePrefix, and the startup budget check below
// models the same production wiring — exported so those two sites cannot drift
// apart into a budget that silently mis-measures the real path.
const AgentContainerNamePrefix = "compass-agent-"

// validateRuntimeDir rejects a runtime dir that cannot fit the longest agent
// socket path the Runner will build under it. Every such path is
// dir/containers/<prefix><32-char account id>/agent.sock (host.go serveSocket),
// so the only variable is dir itself — a misconfigured deployment is knowable at
// startup, and refusing to boot beats a bare EINVAL at the first provision.
//
// The budget is measured by building the worst-case path rather than hand-summing
// byte counts, so it tracks agentSocketDir/agentSocketFile automatically if
// either constant changes.
func validateRuntimeDir(dir string) error {
	// A relative dir (notably --runtime-dir="") would pass the budget check and
	// then MkdirAll agent sockets under whatever CWD the Runner happened to
	// start in. This function is the startup gate for that config value, so the
	// absoluteness check belongs here, ahead of the budget computation.
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("runtime dir %q must be an absolute path", dir)
	}

	widest := filepath.Join(dir, agentSocketDir,
		AgentContainerNamePrefix+strings.Repeat("0", agentAccountIDWidth), agentSocketFile)
	if len(widest) > sunPathMax {
		// Name both contributors and the cap, so the operator knows which knob
		// to turn: shorten the runtime dir by at least the overshoot.
		return fmt.Errorf(
			"runtime dir %q (%d bytes) is too long: the longest agent socket path under it is %d bytes, over this platform's AF_UNIX limit of %d; shorten the runtime dir by at least %d bytes",
			dir, len(dir), len(widest), sunPathMax, len(widest)-sunPathMax)
	}
	return nil
}

// Run attaches the Runner to the Server and hosts agent sessions until ctx is
// cancelled. It Dials (constructs the RunnerService client + enrolls), builds
// the production SessionHost over the container engine, and runs the Sessions
// command loop. A returned error is a fatal attach/stream failure; a cancelled
// ctx is a clean shutdown (nil).
func Run(ctx context.Context, cfg RunnerConfig, specs SpecBuilder, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Engine == nil {
		return errors.New("runner config requires a container engine")
	}
	if err := validateRuntimeDir(cfg.RuntimeDir); err != nil {
		return err
	}

	link, err := Dial(ctx, cfg)
	if err != nil {
		return err
	}
	log.Info("runner enrolled", slog.String("runner_id", cfg.RunnerID), slog.Bool("reattached", link.Reattached()))

	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(cfg.Engine, registry)
	host := NewSessionHost(link, rt, registry, cfg.Engine, specs, AgentHostConfig{
		RuntimeDir: cfg.RuntimeDir,
		AgentModel: cfg.AgentModel,
	}, log, nil)
	// The per-container agent sockets the host serves live until the Runner
	// process ends (no per-container Deprovision RPC in the single-Runner MVP);
	// close them all on shutdown, draining any in-flight call.
	if closer, ok := host.(interface{ Close(ctx context.Context) }); ok {
		defer closer.Close(context.WithoutCancel(ctx))
	}

	// The Sessions loop blocks until the stream ends (ctx cancel = clean
	// shutdown; any other end is the link dropping). The relay streams
	// (PublishEvents) are driven per-session inside StartAgent, bound to ctx.
	if err := link.RunSessions(ctx, host); err != nil {
		return fmt.Errorf("runner sessions loop: %w", err)
	}
	return nil
}
