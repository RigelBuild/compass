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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// sunPathMax derives the platform's AF_UNIX path limit from sockaddr_un and
// excludes the NUL terminator. It is local because the limit is an OS property.
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

// validateRuntimeDir builds the widest agent socket path and rejects a runtime
// dir that exceeds the platform limit. Constructing the path tracks its component
// constants without hand-summing byte counts.
func validateRuntimeDir(dir string) error {
	// Reject relative paths before budgeting; otherwise sockets would be created
	// under whichever working directory the Runner started in.
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

// staleContainerSweepTimeout gives best-effort startup cleanup a single bounded
// budget. It must exceed podman rm's 10s default stop grace: a stale agent runs
// sleep infinity, which ignores SIGTERM, so every removal waits the full grace.
// A var only so a test can shorten it; never reassigned in production.
var staleContainerSweepTimeout = 30 * time.Second

// sweepStaleAgentContainers removes this Runner's stale owned containers and
// socket dirs after Dial verifies the Runner identity and before sessions start.
func sweepStaleAgentContainers(ctx context.Context, engine runtime.WorkloadRuntime, runtimeDir, runnerID string, log *slog.Logger) {
	if lister, ok := engine.(ownedWorkloadLister); ok {
		sweepCtx, cancel := context.WithTimeout(ctx, staleContainerSweepTimeout)
		defer cancel()
		names, err := lister.ListByOwner(sweepCtx, AgentContainerNamePrefix, runnerID)
		if err != nil {
			log.WarnContext(ctx, "listing stale agent containers", slog.Any("error", err))
		} else {
			var wg sync.WaitGroup
			var removed atomic.Int64
			for _, name := range names {
				wg.Go(func() {
					if err := engine.Remove(sweepCtx, name); err != nil {
						log.WarnContext(ctx, "removing stale agent container", slog.String("name", name.String()), slog.Any("error", err))
						return
					}
					removed.Add(1)
				})
			}
			wg.Wait()
			if count := removed.Load(); count > 0 {
				log.InfoContext(ctx, "removed stale agent containers", slog.Int64("count", count))
			}
		}
	}
	if runtimeDir == "" {
		return
	}
	containerDir := filepath.Join(runtimeDir, agentSocketDir)
	entries, err := os.ReadDir(containerDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.WarnContext(ctx, "listing stale agent socket directories", slog.Any("error", err))
		}
		return
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), AgentContainerNamePrefix) && entry.IsDir() {
			if err := os.RemoveAll(filepath.Join(containerDir, entry.Name())); err != nil {
				log.WarnContext(ctx, "removing stale agent socket directory", slog.String("name", entry.Name()), slog.Any("error", err))
			}
		}
	}
}

// Five attempts with 1s, 2s, 4s, and 8s backoffs cover brief restarts, then fail loud;
// the per-attempt timeout stops a stalled server from blocking an attempt forever.
const (
	dialMaxAttempts      = 5
	dialInitialBackoff   = time.Second
	enrollAttemptTimeout = 10 * time.Second
)

type dialFunc func(context.Context, RunnerConfig) (*ServerLink, error)
type waitFunc func(context.Context, time.Duration) bool

// runDialWithRetry returns ctx.Err() once ctx is cancelled, so Run can treat it as shutdown.
func runDialWithRetry(ctx context.Context, cfg RunnerConfig, log *slog.Logger, dial dialFunc, wait waitFunc) (*ServerLink, error) {
	var lastErr error
	for attempt := 1; attempt <= dialMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, enrollAttemptTimeout)
		link, err := dial(attemptCtx, cfg)
		attemptTimedOut := attemptCtx.Err() == context.DeadlineExceeded
		cancel()
		if err == nil {
			return link, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
		// The attempt's own deadline is a stalled server, which is retryable.
		timedOut := attemptTimedOut && errors.Is(err, context.DeadlineExceeded)
		log.WarnContext(ctx, "runner enrollment attempt failed", slog.Int("attempt", attempt),
			slog.Int("max_attempts", dialMaxAttempts), slog.Bool("timed_out", timedOut), slog.Any("error", err))
		if !timedOut && !retryableDialError(err) {
			return nil, fmt.Errorf("runner enrollment failed (not retryable, attempt %d): %w", attempt, err)
		}
		if attempt < dialMaxAttempts && !wait(ctx, dialInitialBackoff<<uint(attempt-1)) {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("runner enrollment failed after %d attempts: %w", dialMaxAttempts, lastErr)
}

func retryableDialError(err error) bool {
	if _, ok := errors.AsType[*dialConfigurationError](err); ok {
		return false
	}
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated, connect.CodePermissionDenied, connect.CodeInvalidArgument, connect.CodeFailedPrecondition:
		return false
	default:
		return true
	}
}

func waitDialBackoff(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// agentHostConfig copies Runner-wide agent settings into the session host.
func agentHostConfig(cfg RunnerConfig, runnerID string) AgentHostConfig {
	return AgentHostConfig{
		RuntimeDir:    cfg.RuntimeDir,
		AgentModel:    cfg.AgentModel,
		AgentBatching: cfg.AgentBatching,
		RunnerID:      runnerID,
	}
}

// Run attaches to the Server with bounded enrollment retries, then hosts sessions until ctx is cancelled.
// A cancelled ctx is a clean shutdown (nil); exhausted retries or a dropped session stream return an error.
func Run(ctx context.Context, cfg RunnerConfig, specs SpecBuilder, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Engine == nil {
		return errors.New("runner config requires a container engine")
	}
	if cfg.RunnerID == "" {
		if _, ok := cfg.Token.(*FileToken); !ok {
			return errors.New("runner config requires a runner id")
		}
	}
	if err := validateRuntimeDir(cfg.RuntimeDir); err != nil {
		return err
	}
	link, err := runDialWithRetry(ctx, cfg, log, Dial, waitDialBackoff)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	runnerID := link.RunnerID()
	if runnerID == "" {
		return errors.New("runner enrollment returned an empty runner id")
	}
	sweepStaleAgentContainers(ctx, cfg.Engine, cfg.RuntimeDir, runnerID, log)
	if ctx.Err() != nil {
		return nil
	}
	log.InfoContext(ctx, "runner enrolled", slog.String("runner_id", runnerID), slog.Bool("reattached", link.Reattached()))
	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(cfg.Engine, registry)
	host := NewSessionHost(link, rt, registry, cfg.Engine, specs, agentHostConfig(cfg, runnerID), log)
	// The per-container agent sockets the host serves live until the Runner
	// process ends (no per-container Deprovision RPC in the single-Runner MVP);
	// close them all on shutdown, draining any in-flight call.
	if closer, ok := host.(interface{ Close(ctx context.Context) }); ok {
		defer closer.Close(context.WithoutCancel(ctx))
	}
	// The Sessions loop blocks until the stream ends (ctx cancel = clean
	// shutdown; any other end is the link dropping). The relay streams
	// (PublishEvents) are driven per-session inside StartAgent, bound to ctx.
	if err := link.RunSessions(ctx, host, log); err != nil {
		return fmt.Errorf("runner sessions loop: %w", err)
	}
	return nil
}
