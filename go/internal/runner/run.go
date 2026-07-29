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

	"github.com/sealedsecurity/compass/go/internal/runtime"
)

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
