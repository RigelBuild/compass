//go:build unix && gtk4

// The Compass multi-window smoke gate (design record §M4). This is the ONE test
// that drives the REAL GTK4/WebKit Wails shell — the window factory, real
// application.Window handles, and a real WindowClosing event — rather than the
// windowDispatcher seam's fakes. It covers exactly the gap the unix (non-gtk4)
// unit suite cannot reach and the M3/M3b reviews flagged as architecturally
// forced: windowFromContext reads application.WindowKey (nil in the nogtk4
// build, so the unit test injects call.window by hand), and the newAppWindow
// close handler fires cancelWindow only on a real WindowClosing.
//
// It runs only under a display: TestMain hosts app.Run() (the blocking GTK loop
// MUST own the main goroutine / OS thread 0) and the Test* bodies run on a
// driver goroutine gated on ApplicationStarted. With no display the gtk4 tests
// self-skip, so a container-less sandbox skips rather than fails — the posture
// of the podman e2e legs. The ci.yml "Multi-window gtk4 e2e gate" step wraps the
// run in xvfb-run so CI and the dev box both have a framebuffer.
//
// No sleeps gate an assertion: window creation and the count after a close are
// observed on the main thread via GetByName (the Window methods' own InvokeSync
// makes the mutate-then-read ordering deterministic), and the bridge-service
// inflight map is read under its mutex (assertInflight/assertNotInflight, the
// unix-tagged unit helpers, reused here).
package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/bridge"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// e2eApp is the single real Wails application the gate drives; it is created and
// run by TestMain and consumed by the Test* bodies once e2eReady closes.
var (
	e2eApp      *application.App
	e2eReady    = make(chan struct{})
	e2eExitCode int
)

// hasDisplay reports whether a display (real or Xvfb) is available. Without one
// the GTK loop cannot start, so the gate self-skips.
func hasDisplay() bool {
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

func TestMain(m *testing.M) {
	if !hasDisplay() {
		// No framebuffer: run the (self-skipping) tests without a GTK loop.
		os.Exit(m.Run())
	}

	e2eApp = application.New(application.Options{Name: "compass-multiwindow-e2e"})
	e2eApp.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		close(e2eReady)
	})

	go func() {
		<-e2eReady
		e2eExitCode = m.Run()
		e2eApp.Quit() // unblocks e2eApp.Run() on the main thread
	}()

	if err := e2eApp.Run(); err != nil {
		// A failed GTK bring-up is a hard gate failure, not a skip.
		os.Stderr.WriteString("compass-app multi-window e2e: app.Run: " + err.Error() + "\n")
		os.Exit(1)
	}
	// WebKitGTK fork+execs its GPU/network helpers in C as our direct children;
	// upstream teardown never joins them. Left alive they hold go test's captured
	// pipe until its WaitDelay fires (a PASS turns into "Test I/O incomplete"), or
	// orphan to init on a reused runner. Reap them here, bounded and loud.
	os.Exit(reapChildren(e2eExitCode))
}

// reapWait hard-bounds the join; past it a child is wedged, so the gate reds
// loudly instead of tripping go test's opaque WaitDelay. reapEscalate is the
// SIGTERM grace before SIGKILL — the helpers honor SIGTERM slowly.
const (
	reapWait     = 10 * time.Second
	reapEscalate = 2 * time.Second
)

// procInfo names one surviving child for the timeout diagnostic.
type procInfo struct {
	pid  int
	comm string
}

// reapChildren joins the WebKit helpers WebKitGTK fork+exec'd as our children.
// They block until signaled (the app has quit), so a passive wait never ends:
// SIGTERM, then SIGKILL if they dawdle, until none remain or reapWait elapses.
// A survivor past the bound reds loudly and non-zero, never masking a failure.
func reapChildren(exitCode int) int {
	start := time.Now()
	signalChildren(liveChildren(), syscall.SIGTERM)

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	escalated := false
	for {
		remaining := liveChildren()
		if len(remaining) == 0 {
			return exitCode
		}
		if time.Since(start) >= reapWait {
			var b strings.Builder
			fmt.Fprintf(&b, "compass-app multi-window e2e: %d child process(es) survived the %s reap deadline:\n", len(remaining), reapWait)
			for _, p := range remaining {
				fmt.Fprintf(&b, "  pid=%d comm=%q\n", p.pid, p.comm)
			}
			os.Stderr.WriteString(b.String())
			if exitCode == 0 {
				return 1
			}
			return exitCode
		}
		if !escalated && time.Since(start) >= reapEscalate {
			signalChildren(remaining, syscall.SIGKILL)
			escalated = true
		}
		<-ticker.C
	}
}

// signalChildren sends sig to each named process. ESRCH is expected and ignored
// — the child exited between discovery and the signal; any other error is a real
// fault worth surfacing, never swallowed.
func signalChildren(procs []procInfo, sig syscall.Signal) {
	for _, p := range procs {
		if err := syscall.Kill(p.pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			fmt.Fprintf(os.Stderr, "compass-app multi-window e2e: signal %d pid=%d: %v\n", sig, p.pid, err)
		}
	}
}

// TestMultiWindowCloseCancelsOnlyClosingWindowE2E is the leak-gate proof through
// the REAL shell: two real Bridge windows each own an in-flight bridge call
// (registered through the real windowFromContext, which reads the window off
// application.WindowKey — the path the nogtk4 unit test cannot exercise);
// closing one window fires its real WindowClosing handler, which calls
// cancelWindow and sweeps ONLY that window's call, while the other window's call
// stays in-flight. This is the daemon-observable half of the §M4 checklist
// (a closed window's bridge subscription terminates) driven end to end.
func TestMultiWindowCloseCancelsOnlyClosingWindowE2E(t *testing.T) {
	if !hasDisplay() {
		t.Skip("no DISPLAY/WAYLAND_DISPLAY; run under xvfb-run (the ci.yml gtk4 e2e gate, or `xvfb-run go test -tags 'unix gtk4'` locally)")
	}

	// A stub bridge target whose handler blocks until released, so each
	// registered call stays in-flight until the test drains it. stubServer is
	// the unix-tagged unit helper (real h2c UDS), reused here.
	release := make(chan struct{})
	socket := stubServer(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/grpc-web+proto")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	t.Cleanup(func() { close(release) })

	svc := newBridgeService(bridge.NewPump(bridge.NewUnixTarget(socket)), e2eApp.Event, nil, nil)

	// Create two REAL Bridge windows through the production factory, which
	// attaches the real WindowClosing → cancelWindow handler to each.
	const nameA = "e2e-winA"
	const nameB = "e2e-winB"
	// Distinct titles per window: the two windows are meant to be
	// distinguishable, and it keeps the factory's title parameter genuinely
	// varied rather than a package-wide constant.
	application.InvokeSync(func() {
		newAppWindow(e2eApp, svc, nameA, "Compass — "+nameA, "")
		newAppWindow(e2eApp, svc, nameB, "Compass — "+nameB, "")
	})

	winA := mustGetWindow(t, nameA)
	winB := mustGetWindow(t, nameB)

	// Register an in-flight call for each window through the REAL capture path:
	const idA = "e2e-req-A"
	const idB = "e2e-req-B"
	doneA := launchWindowedCall(svc, winA, idA)
	launchWindowedCall(svc, winB, idB)

	assertInflight(t, svc, idA)
	assertInflight(t, svc, idB)

	// Close winA for real: its WindowClosing handler runs cancelWindow(winA).
	application.InvokeSync(func() { winA.Close() })

	// The closing window's call is swept: cancelWindow cancels its context, the
	// pump returns, run's finish drops the entry and closes doneA. waitDone gates
	// on that (no sleep); winB's call is untouched and stays in-flight.
	waitDone(t, doneA)
	assertNotInflight(t, svc, idA)
	assertInflight(t, svc, idB)

	// The other window is untouched — still live in the shell (the "close one
	// leaves the other live" checklist row). winA's removal from the window
	// manager is GTK-async, so it is NOT asserted here — a GetByName race would
	// need a sleep the no-sleep rule forbids; that winA closed is already proven
	// above by its call being swept (assertNotInflight), the contract §M4 gates.
	if _, ok := getWindow(nameB); !ok {
		t.Errorf("winB gone after closing winA")
	}
}
