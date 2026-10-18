//go:build (linux && gtk4) || darwin

// The embedded-mode app QUIT lifecycle. Two behaviors implement the DL-108
// contract:
//
//   - Linger (the DEFAULT, and PASSIVE): on a plain app quit (window close, OS
//     quit) the app process exits while the already-detached stack children
//     (postgres + server + runner, spawned by `compass-stack up` in their OWN
//     processes) KEEP running. The app does nothing to the stack on a plain
//     quit — relaunch re-attaches (compass-stack up attaches when a live server
//     answers). So linger needs NO code here; it is the absence of a teardown on
//     plain quit.
//   - Explicit "Quit and stop stack": a distinct user action that runs
//     `compass-stack down` (attach → SIGTERM the child tree → wait the server
//     drain → release the lock) and THEN quits the app. That action is this
//     file's quitController. The container-aware teardown (DL-262 pgid v2
//     DownDetached) is the compass-stack CLI's; the app only execs the binary.
//
// The orchestration is decoupled from Wails and from exec behind small injected
// seams (stackDown, quit), so the quit sequence — build the argv, run down under
// a bounded context, quit regardless of the down outcome — is unit-testable
// without a real stack or a display.
package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/RigelBuild/compass/go/internal/embedded"
)

// quitController holds teardown effects and parameters. Tests inject stubs to
// verify sequencing without executing a stack command or opening a display.
type quitController struct {
	stackDown func(ctx context.Context, args []string) error
	params    embedded.Params
	quit      func()
	timeout   time.Duration
	logger    *slog.Logger
}

// stopStackAndQuit uses a fresh deadline because bring-up's context is canceled.
// Explicit quit wins even if teardown fails; the safe failure mode is a
// lingering stack.
func (c quitController) stopStackAndQuit(ctx context.Context) {
	logger := c.logger
	if logger == nil {
		logger = slog.Default()
	}
	downCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.stackDown(downCtx, embedded.StackDownArgs(c.params)); err != nil {
		// Quit-anyway (OQ-6): log and fall through to quit.
		logger.ErrorContext(ctx, "stopping the embedded stack failed; quitting anyway "+
			"(the stack lingers, which is the safe plain-quit default)", "error", err)
	}
	c.quit()
}
