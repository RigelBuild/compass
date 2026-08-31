//go:build (linux && gtk4) || darwin

// Command compass-app is the Compass native desktop shell: a Wails v3
// application that opens one window loading the prebuilt SolidJS UI (apps/ui
// dist) and exposes the compass_rpc / compass_rpc_cancel IPC bridge to it,
// backed by the bridge pump over the client's TLS connection to a headless
// Compass stack.
//
// The app is a native CLIENT only (RIG-2554): it dials a headless Compass stack
// over the authenticated TLS door (client.go, runClient). Embedded mode — an
// in-process stack supervisor — was retired; the app no longer spawns, monitors,
// or tears down a stack. The headless stack is brought up out of band on a
// dedicated machine, and app.toml's server_url points the app at it.
//
// Assets: the dist lives at repo apps/ui/dist, OUTSIDE this Go package's
// directory subtree, so //go:embed cannot reach it (embed forbids ".." patterns
// — "invalid pattern syntax"). Instead the dist directory is resolved at runtime
// (flag/env, default relative to the executable) and served via
// application.BundledAssetFileServer over os.DirFS, which still serves the Wails
// runtime.js at /wails/runtime.js. See the T3 brief DE-RISK #1.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/RigelBuild/compass/go/internal/appconfig"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

func main() {
	if err := run(); err != nil {
		slog.Error("compass-app exited with an error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	if handled, err := printVersionIfRequested(os.Args[1:], os.Stdout); handled {
		return err
	}

	assetsFlag := flag.String("assets", "",
		"Directory of the prebuilt apps/ui dist to serve. Defaults to "+
			"$COMPASS_ASSETS_DIR, then the dist for the executable's layout: "+
			"a .app's Contents/Resources/dist, else a 'dist' beside the executable.")
	stateDirFlag := flag.String("state-dir", "",
		"App state directory (the client tokenstore lives here). Defaults to "+
			"$COMPASS_STATE_DIR, then $XDG_STATE_HOME/compass, then $HOME/.compass.")
	flag.Parse()

	assetsDir := resolveAssetsDir(*assetsFlag)

	cfg, err := appconfig.Load(os.Getenv("XDG_CONFIG_HOME"), os.Getenv("HOME"))
	if err != nil {
		return err
	}

	stateDir := resolveStateDir(*stateDirFlag)
	svc, err := launch(cfg, stateDir)
	if err != nil {
		return err
	}

	app := application.New(application.Options{
		Name:        "compass-app",
		Description: "Compass native desktop shell",
		Services: []application.Service{
			application.NewService(svc),
		},
		Assets: application.AssetOptions{
			Handler: application.BundledAssetFileServer(os.DirFS(assetsDir)),
		},
	})
	// The bridge service emits response frames through the app's event manager;
	// wire it now that the app (and its EventManager) exists.
	svc.events = app.Event

	// Persist the live window set on shutdown so a relaunch reopens the same set
	// (Compass multi-window M1). Best-effort: a failed persist must not block
	// shutdown, so the error is logged, never propagated.
	app.OnShutdown(func() {
		wins := app.Window.GetAll()
		names := make([]string, 0, len(wins))
		for _, w := range wins {
			names = append(names, w.Name())
		}
		if err := saveWindowSet(stateDir, names); err != nil {
			slog.Error("compass-app persisting window set", "error", err)
		}
	})

	startupJS, err := shellStartupJS(cfg.ServerURL)
	if err != nil {
		return err
	}

	// The "Window"/"New Window" item opens an additional Bridge window and is
	// always available. Plain quit (window close, OS quit) has no stack teardown:
	// the app is a client and owns no stack. (The window-set persist hook above
	// touches only the state-dir window list.)
	menu := application.NewMenu()
	windowMenu := menu.AddSubmenu("Window")
	windowMenu.Add("New Window").OnClick(func(_ *application.Context) {
		name := nextWindowName(app)
		newAppWindow(app, svc, name, "Compass", startupJS)
	})
	app.Menu.Set(menu)

	// Restore the persisted window set (Compass multi-window M1). First-ever run
	// (empty/absent/corrupt set) opens exactly one default "bridge" window. Every
	// window is a Bridge window (URL "/") carrying the SAME startupJS injection
	// (record §A1/§A3), so the factory forwards the identical script to each.
	names := windowNamesOrDefault(loadWindowSet(stateDir))
	for _, name := range names {
		newAppWindow(app, svc, name, "Compass", startupJS)
	}

	slog.Info("compass-app starting", "mode", cfg.Mode, "assets", assetsDir, "version", version)
	return app.Run()
}

// windowOptions builds the Wails options for a Compass window. Every window is a
// Bridge window (URL "/", record §A1) at the fixed 1280x800 size, carrying the
// caller-resolved startupJS unchanged so the mode/serverURL injection is
// identical across windows (§A3). Name keys the persisted set and later per-frame
// routing (M3), so it is always set. Kept a pure forwarder so it is unit-testable
// without a display.
func windowOptions(name, title, startupJS string) application.WebviewWindowOptions {
	return application.WebviewWindowOptions{
		Name:   name,
		Title:  title,
		Width:  1280,
		Height: 800,
		URL:    "/",
		// OQ-8: the shell injects the client mode marker and the server URL as
		// synchronous startup globals the UI reads at entry with no IPC to pick
		// its boot path. JS runs before the app bundle.
		JS: startupJS,
	}
}

// newAppWindow creates a Compass Bridge window on app from windowOptions and
// attaches the per-window WindowClosing close-cancel handler (design §M3b):
// when the window closes, every in-flight bridge call registered to it is
// canceled and dropped, driving the same teardown as compass_rpc_cancel so no
// call's pump goroutine or server-side subscription leaks for the app's
// lifetime. Attaching here (not per call site) means every window — New Window
// menu and restore loop alike — gets the leak gate; it cannot be forgotten at a
// call site. The unsubscribe func the registration returns is ignored: the
// window and its handler die together.
func newAppWindow(app *application.App, svc *bridgeService, name, title, startupJS string) {
	win := app.Window.NewWithOptions(windowOptions(name, title, startupJS))
	win.OnWindowEvent(events.Common.WindowClosing, func(_ *application.WindowEvent) {
		svc.cancelWindow(wailsWindowDispatcher{win: win})
	})
}

// nextWindowName returns the first window name that has no live window on app.
// The default window is defaultWindowName ("bridge"); additional windows are
// suffixed bridge-2, bridge-3, … The live-window check goes through the app, so
// the pure name selection is factored into firstFreeName for unit testing.
func nextWindowName(app *application.App) string {
	return firstFreeName(defaultWindowName, func(n string) bool {
		_, ok := app.Window.GetByName(n)
		return ok
	})
}

// firstFreeName returns base if exists(base) is false, else the first
// base-2, base-3, … for which exists reports false. exists reports whether a
// name is already taken.
func firstFreeName(base string, exists func(string) bool) string {
	if !exists(base) {
		return base
	}
	for n := 2; ; n++ {
		name := fmt.Sprintf("%s-%d", base, n)
		if !exists(name) {
			return name
		}
	}
}

// launch wires the native-client bridge service (design §T5.6). Embedded mode —
// the in-process stack supervisor — was retired in RIG-2554, so launch is a thin
// wrapper over runClient: there is no stack to spawn, monitor, or tear down, and
// no quit controller. The Mode was validated to client by appconfig.Load; the
// default arm rejects any other resolved Mode legibly rather than dialing a
// half-configured target.
func launch(cfg appconfig.Config, stateDir string) (*bridgeService, error) {
	switch cfg.Mode {
	case appconfig.ModeClient:
		// Client mode opens the window immediately: no pre-window probe (the
		// single auto-connect is the UI's boot-time shellConnect(""), T5.5).
		return runClient(cfg, stateDir)
	default:
		return nil, fmt.Errorf("unknown app mode %v", cfg.Mode)
	}
}

// shellStartupJS builds the OQ-8 startup script the webview loads before the app
// bundle. It assigns window.__COMPASS_MODE__ ("client" — the app is a native
// client only, RIG-2554) and window.__COMPASS_SERVER_URL__. The server URL is
// JSON-encoded (encoding/json) so a hostile URL containing quotes/backslashes/
// </script> cannot break out of the script or inject — the encoded form is
// always a valid JS string literal.
func shellStartupJS(serverURL string) (string, error) {
	urlJSON, err := json.Marshal(serverURL)
	if err != nil {
		return "", fmt.Errorf("encoding startup server-url global: %w", err)
	}
	js := `window.__COMPASS_MODE__="client";` +
		"window.__COMPASS_SERVER_URL__=" + string(urlJSON) + ";"
	return js, nil
}

// resolveAssetsDir picks the dist directory to serve: the --assets flag, else
// $COMPASS_ASSETS_DIR, else the dist directory for the running executable's
// layout (distDirForExecutable). See DE-RISK #1: the dist is outside this
// package's tree, so it is served from a runtime-resolved directory rather than
// embedded.
func resolveAssetsDir(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("COMPASS_ASSETS_DIR"); env != "" {
		return env
	}
	if exe, err := os.Executable(); err == nil {
		return distDirForExecutable(exe)
	}
	return "dist"
}

// distDirForExecutable resolves the dist directory for a given executable path.
// A macOS .app stages the binary at Contents/MacOS/compass-app and the UI dist
// at Contents/Resources/dist (the macos-bundle tool, compass-distribution T3),
// so when the executable sits in a Contents/MacOS directory the dist is one
// level up at Contents/Resources/dist. Every other packaging — the Linux thin
// client's bin/compass-app + bin/dist, or a dev build beside the module — stages
// dist beside the executable.
func distDirForExecutable(exe string) string {
	dir := filepath.Dir(exe)
	if filepath.Base(dir) == "MacOS" && filepath.Base(filepath.Dir(dir)) == "Contents" {
		return filepath.Join(filepath.Dir(dir), "Resources", "dist")
	}
	return filepath.Join(dir, "dist")
}
