//go:build unix && gtk3

// Command compass-app is the Compass native desktop shell: a Wails v3
// application that opens one window loading the prebuilt SolidJS UI (apps/ui
// dist) and exposes the compass_rpc / compass_rpc_cancel IPC bridge to it,
// backed by the bridge pump over the daemon's Unix socket.
//
// This is T3 slice 2 — the shell + IPC bridge only. Stack spawn/supervision,
// host preflight, and mode selection are T4 (SEA-1685): the window points at a
// daemon a developer starts by hand (compass-stack up), and the dial target is a
// single flag/env-supplied socket with a sensible default. There is no stack
// supervisor here.
//
// Assets: the dist lives at repo apps/ui/dist, OUTSIDE this Go package's
// directory subtree, so //go:embed cannot reach it (embed forbids ".." patterns
// — "invalid pattern syntax"). Instead the dist directory is resolved at runtime
// (flag/env, default relative to the executable) and served via
// application.BundledAssetFileServer over os.DirFS, which still serves the Wails
// runtime.js at /wails/runtime.js. See the T3 brief DE-RISK #1.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/RigelBuild/compass/go/internal/appconfig"
	"github.com/RigelBuild/compass/go/internal/bridge"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

func main() {
	if err := run(); err != nil {
		slog.Error("compass-app exited with an error", "error", err)
		os.Exit(1)
	}
}

// bringUpTimeout bounds the whole embedded bring-up (preflight + compass-stack
// up + WhoAmI) as a backstop against a wedged launch. app.Run() itself is not
// context-bound. Per the T4.1 brief the bring-up window is ~60s; lifecycle
// polish (a longer window covering a cold agent-image pull) is T4.2.
const bringUpTimeout = 60 * time.Second

func run() error {
	if handled, err := printVersionIfRequested(os.Args[1:], os.Stdout); handled {
		return err
	}

	socketFlag := flag.String("socket", "",
		"Unix socket the Compass daemon serves compass.v1 on. Defaults to "+
			"$COMPASS_SOCKET, then $XDG_RUNTIME_DIR/compass/server.sock. In "+
			"embedded mode the supervised stack serves it; in client mode it is "+
			"dialed as-is.")
	assetsFlag := flag.String("assets", "",
		"Directory of the prebuilt apps/ui dist to serve. Defaults to "+
			"$COMPASS_ASSETS_DIR, then a 'dist' directory beside the executable.")
	modeFlag := flag.String("mode", "",
		"Operating mode override (embedded|client). Defaults to $COMPASS_APP_MODE, "+
			"then app.toml, then embedded.")
	stackBinFlag := flag.String("compass-stack", "",
		"Path to the compass-stack binary the embedded stack is supervised with. "+
			"Defaults to $COMPASS_STACK_BIN, then compass-stack on $PATH, then a "+
			"compass-stack sibling of this executable.")
	stateDirFlag := flag.String("state-dir", "",
		"App state directory for the embedded stack. Defaults to "+
			"$COMPASS_STATE_DIR, then $XDG_STATE_HOME/compass, then $HOME/.compass.")
	imageFlag := flag.String("image", "",
		"Agent container image ref for the embedded stack. Defaults to "+
			"$COMPASS_AGENT_IMAGE, then "+defaultAgentImage+".")
	flag.Parse()

	socket := resolveSocket(*socketFlag)
	assetsDir := resolveAssetsDir(*assetsFlag)

	// run() is the process root (called directly by main), so the root context
	// originates here; the bring-up window is derived from it, not re-rooted.
	cfg, err := appconfig.Load(os.Getenv("XDG_CONFIG_HOME"), os.Getenv("HOME"), resolveMode(*modeFlag))
	if err != nil {
		return err
	}

	// run() is the process root (called directly by main), so context.Background()
	// here is the sanctioned process-root context, not a mid-tree re-root.
	stateDir := resolveStateDir(*stateDirFlag)
	svc, quitter, err := launch(cfg, socket, stateDir,
		resolveImage(*imageFlag), stackBinFlag)
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

	startupJS, err := shellStartupJS(cfg.Mode.String(), cfg.ServerURL)
	if err != nil {
		return err
	}

	// The menu is installed in both modes. The "Window"/"New Window" item opens
	// an additional Bridge window and is always available. The embedded-only
	// "File"/"Quit and stop stack" item (DL-108, T4.2) is gated on quitter: client
	// mode has no stack to stop. Plain quit (window close, OS quit) LINGERS by
	// default — the stack children stay running and the app does nothing to them
	// (relaunch re-attaches), so there is deliberately no OnShutdown *stack*
	// teardown here. (The window-set persist hook above is unrelated: it touches
	// only the state-dir window list, never the stack.)
	menu := application.NewMenu()
	if quitter != nil {
		quitter.quit = app.Quit
		fileMenu := menu.AddSubmenu("File")
		fileMenu.Add("Quit and stop stack").OnClick(func(_ *application.Context) {
			// A UI-event callback has no inherited context.Context, so this is
			// the legitimate main-entrypoint root; stopStackAndQuit derives its
			// bounded teardown deadline from it.
			quitter.stopStackAndQuit(context.Background())
		})
	}
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

	slog.Info("compass-app starting", "mode", cfg.Mode, "socket", socket, "assets", assetsDir, "version", version)
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
		// OQ-8: the shell injects the launch mode (both modes) and, in client
		// mode, the server URL as synchronous startup globals the UI reads at
		// entry with no IPC to pick its boot path. JS runs before the app bundle.
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

// launch dispatches the resolved mode into the embedded or client launch arm and
// returns the wired bridge service plus (embedded only) the quit controller.
// Only the embedded arm resolves the compass-stack binary and builds the quit
// controller; a client-only install has neither a compass-stack binary nor a
// stack to stop, so those effects must not gate a client launch (design §T5.6).
func launch(
	cfg appconfig.Config, socket, stateDir, image string, stackBinFlag *string,
) (*bridgeService, *quitController, error) {
	switch cfg.Mode {
	case appconfig.ModeEmbedded:
		stackBin, err := resolveStackBin(*stackBinFlag)
		if err != nil {
			return nil, nil, err
		}
		pipeline := embeddedPipeline{
			preflight: realPreflight(image, embeddedDatabaseDSN(stateDir)),
			stackUp:   runStackUp(stackBin),
			whoAmI:    whoAmIOverUDS,
		}
		params := embeddedParams{socket: socket, stateDir: stateDir, image: image}

		// The embedded bring-up (preflight → stack up → WhoAmI) runs BEFORE the
		// window opens, under a bounded bring-up context rooted at the process
		// root (run() is called directly by main).
		bringUpCtx, cancel := context.WithTimeout(context.Background(), bringUpTimeout)
		accountID, quitter, err := runEmbedded(bringUpCtx, pipeline, params, runStackDown(stackBin))
		cancel()
		if err != nil {
			return nil, nil, err
		}

		svc := newBridgeService(bridge.NewPump(bridge.NewUnixTarget(socket)), nil, nil, nil)
		svc.accountID = accountID
		return svc, quitter, nil
	case appconfig.ModeClient:
		// Client mode opens the window immediately: no pre-window probe (the
		// single auto-connect is the UI's boot-time shellConnect(""), T5.5).
		svc, err := runClient(cfg, stateDir)
		if err != nil {
			return nil, nil, err
		}
		return svc, nil, nil
	default:
		return nil, nil, fmt.Errorf("unknown app mode %v", cfg.Mode)
	}
}

// shellStartupJS builds the OQ-8 startup script the webview loads before the app
// bundle. It assigns window.__COMPASS_MODE__ in both modes and, in client mode,
// window.__COMPASS_SERVER_URL__. Each value is JSON-encoded (encoding/json) so a
// hostile server URL containing quotes/backslashes/</script> cannot break out of
// the script or inject — the encoded form is always a valid JS string literal.
func shellStartupJS(mode, serverURL string) (string, error) {
	modeJSON, err := json.Marshal(mode)
	if err != nil {
		return "", fmt.Errorf("encoding startup mode global: %w", err)
	}
	js := "window.__COMPASS_MODE__=" + string(modeJSON) + ";"
	if mode == appconfig.ModeClient.String() {
		urlJSON, err := json.Marshal(serverURL)
		if err != nil {
			return "", fmt.Errorf("encoding startup server-url global: %w", err)
		}
		js += "window.__COMPASS_SERVER_URL__=" + string(urlJSON) + ";"
	}
	return js, nil
}

// resolveSocket picks the daemon socket to dial: the --socket flag, else
// $COMPASS_SOCKET, else $XDG_RUNTIME_DIR/compass/server.sock (the server default,
// go/server/socket.go DefaultSocketPath). A relative XDG_RUNTIME_DIR is treated
// as unset, matching the server, so the fallback is deterministic.
func resolveSocket(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("COMPASS_SOCKET"); env != "" {
		return env
	}
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(runtimeDir) {
		return filepath.Join(runtimeDir, "compass", "server.sock")
	}
	return filepath.Join(os.Getenv("HOME"), ".compass", "server.sock")
}

// resolveAssetsDir picks the dist directory to serve: the --assets flag, else
// $COMPASS_ASSETS_DIR, else a 'dist' directory beside the executable (where a
// packaged build stages apps/ui dist). See DE-RISK #1: the dist is outside this
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
		return filepath.Join(filepath.Dir(exe), "dist")
	}
	return "dist"
}
