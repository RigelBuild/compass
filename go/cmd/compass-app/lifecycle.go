//go:build (linux && gtk4) || darwin

// The embedded-mode app QUIT lifecycle. Two behaviors sit on the frozen DL-108
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
)

// stackDownTimeout bounds the explicit teardown (compass-stack down: attach,
// SIGTERM the child tree, wait the server drain, release the lock). The bring-up
// context is already cancelled by the time the window is open, so
// stopStackAndQuit roots a FRESH bounded context off the caller's rather than
// reusing it.
const stackDownTimeout = 60 * time.Second

// quitController is the explicit "Quit and stop stack" orchestration over its
// injected effects. It holds the teardown seam (stackDown), the argv inputs
// (params, resolved once in run()), the app-quit indirection (quit, wired to
// app.Quit), the teardown deadline (timeout), and a logger. A test supplies a
// recording stackDown stub and a counting quit, so the quit sequence is
// verified with no real exec and no display.
type quitController struct {
	stackDown func(ctx context.Context, args []string) error
	params    embeddedParams
	quit      func()
	timeout   time.Duration
	logger    *slog.Logger
}

// stopStackAndQuit runs the explicit teardown and then quits the app. It derives
// a FRESH bounded context off the caller's (the bring-up context is already
// cancelled by the time the window is open), builds the `compass-stack down`
// argv, and runs the injected stackDown seam under that deadline.
//
// Quit-anyway on a failed down (OQ-6): if `compass-stack down` fails, the app
// STILL quits. Rationale: the user's explicit intent is to quit, trapping them
// in a live window because teardown hiccuped is worse, and a lingering stack is
// the SAFE failure (it is the plain-quit default anyway).
func (c quitController) stopStackAndQuit(ctx context.Context) {
	logger := c.logger
	if logger == nil {
		logger = slog.Default()
	}
	downCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.stackDown(downCtx, stackDownArgs(c.params)); err != nil {
		// Quit-anyway (OQ-6): log and fall through to quit.
		logger.Error("stopping the embedded stack failed; quitting anyway "+
			"(the stack lingers, which is the safe plain-quit default)", "error", err)
	}
	c.quit()
}
