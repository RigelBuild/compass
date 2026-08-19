//go:build unix && gtk3

// The Compass multi-window smoke gate (design record §M4). This is the ONE test
// that drives the REAL GTK3/WebKit Wails shell — the window factory, real
// application.Window handles, and a real WindowClosing event — rather than the
// windowDispatcher seam's fakes. It covers exactly the gap the unix (non-gtk3)
// unit suite cannot reach and the M3/M3b reviews flagged as architecturally
// forced: windowFromContext reads application.WindowKey (nil in the nogtk3
// build, so the unit test injects call.window by hand), and the newAppWindow
// close handler fires cancelWindow only on a real WindowClosing.
//
// It runs only under a display: TestMain hosts app.Run() (the blocking GTK loop
// MUST own the main goroutine / OS thread 0) and the Test* bodies run on a
// driver goroutine gated on ApplicationStarted. With no display the gtk3 tests
// self-skip, so a container-less sandbox skips rather than fails — the posture
// of the podman e2e legs. The ci.yml "Multi-window gtk3 e2e gate" step wraps the
// run in xvfb-run so CI and the dev box both have a framebuffer.
//
// No sleeps gate an assertion: window creation and the count after a close are
// observed on the main thread via GetByName (the Window methods' own InvokeSync
// makes the mutate-then-read ordering deterministic), and the bridge-service
// inflight map is read under its mutex (assertInflight/assertNotInflight, the
// unix-tagged unit helpers, reused here).
package main

import (
	"net/http"
	"os"
	"testing"

	"github.com/sealedsecurity/compass/go/internal/bridge"
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
	os.Exit(e2eExitCode)
}

// TestMultiWindowCloseCancelsOnlyClosingWindowE2E is the leak-gate proof through
// the REAL shell: two real Bridge windows each own an in-flight bridge call
// (registered through the real windowFromContext, which reads the window off
// application.WindowKey — the path the nogtk3 unit test cannot exercise);
// closing one window fires its real WindowClosing handler, which calls
// cancelWindow and sweeps ONLY that window's call, while the other window's call
// stays in-flight. This is the daemon-observable half of the §M4 checklist
// (a closed window's bridge subscription terminates) driven end to end.
func TestMultiWindowCloseCancelsOnlyClosingWindowE2E(t *testing.T) {
	if !hasDisplay() {
		t.Skip("no DISPLAY/WAYLAND_DISPLAY; run under xvfb-run (the ci.yml gtk3 e2e gate, or `xvfb-run go test -tags 'unix gtk3'` locally)")
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
