//go:build (linux && gtk4) || darwin

// Command compass-app is the Compass native desktop shell: a Wails v3
// application that opens one window loading the prebuilt SolidJS UI (apps/ui
// dist) and exposes the compass_rpc / compass_rpc_cancel IPC bridge to it,
// backed by the bridge pump.
//
// The app runs in two configured modes (appconfig, resolved at launch):
//   - EMBEDDED: it supervises a private stack through compass-stack and dials
//     its Unix socket over h2c.
//   - CLIENT: it dials a headless Compass stack over the authenticated TLS door.
//
// When app.toml and a mode override are both absent, the shell opens its
// first-run chooser before entering either configured mode.
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
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/RigelBuild/compass/go/internal/appconfig"
	"github.com/RigelBuild/compass/go/internal/bridge"
	"github.com/RigelBuild/compass/go/internal/tokenstore"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// bringUpTimeout bounds the whole embedded bring-up (preflight + compass-stack
// up + WhoAmI) as a backstop against a wedged launch; app.Run() itself is not
// context-bound. It is generous because a cold first run pulls THREE images —
// the agent image from GHCR plus the stock postgres and collector images
// (DL-260) — before the stack reaches Ready, so the window covers three
// sequential registry pulls, not one.
//
// On darwin the window is wider still. The machine ensure step runs inside it,
// and a cold `podman machine init` downloads a VM image before any of the
// above starts — minutes on its own, on a link whose speed we do not control.
// A budget that cannot fit the work it wraps is not a backstop; it is a
// deadline the first launch on a fresh Mac loses every time, and the error it
// produces names the timeout rather than the download. So darwin gets a window
// sized for cold provisioning plus the same three pulls. Both remain backstops
// against a wedge, not performance targets.
//
// The configured embedded bring-up runs before a window opens. First-run setup
// only runs preflight inside the visible chooser and saves the choice; the next
// launch takes the configured bring-up path.
var bringUpTimeout = bringUpTimeoutFor(runtime.GOOS)

// bringUpTimeoutFor returns the bring-up budget for the given host OS. It takes
// the OS as a parameter rather than reading runtime.GOOS so the per-OS choice
// is unit-testable from any host.
func bringUpTimeoutFor(goos string) time.Duration {
	if goos == "darwin" {
		return 15 * time.Minute
	}
	return 180 * time.Second
}

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

	socketFlag := flag.String("socket", "",
		"Unix socket the Compass stack serves compass.v1 on. Defaults to "+
			"$COMPASS_SOCKET, then $XDG_RUNTIME_DIR/compass/server.sock. In "+
			"embedded mode the supervised stack serves it; in client mode the "+
			"remote is dialed over TLS instead.")
	assetsFlag := flag.String("assets", "",
		"Directory of the prebuilt apps/ui dist to serve. Defaults to "+
			"$COMPASS_ASSETS_DIR, then the dist for the executable's layout: "+
			"a .app's Contents/Resources/dist, else a 'dist' beside the executable.")
	stateDirFlag := flag.String("state-dir", "",
		"App state directory (the embedded stack + client tokenstore live here). "+
			"Defaults to $COMPASS_STATE_DIR, then $XDG_STATE_HOME/compass, then "+
			"$HOME/.compass.")
	modeFlag := flag.String("mode", "",
		"Operating mode override (embedded|client). Defaults to $COMPASS_APP_MODE, "+
			"then app.toml, else the first-run chooser.")
	stackBinFlag := flag.String("compass-stack", "",
		"Path to the compass-stack binary the embedded stack is supervised with. "+
			"Defaults to $COMPASS_STACK_BIN, then a compass-stack sibling of this "+
			"executable, then compass-stack on $PATH.")
	imageFlag := flag.String("image", "",
		"Agent container image ref for the embedded stack. Defaults to "+
			"$COMPASS_AGENT_IMAGE, then "+defaultAgentImage+".")
	flag.Parse()

	socket := resolveSocket(*socketFlag)
	assetsDir := resolveAssetsDir(*assetsFlag)

	cfg, err := appconfig.Load(os.Getenv("XDG_CONFIG_HOME"), os.Getenv("HOME"), resolveMode(*modeFlag))
	setupMode := errors.Is(err, appconfig.ErrNoConfig)
	if err != nil && !setupMode {
		return err
	}

	stateDir := resolveStateDir(*stateDirFlag)
	var (
		svc     *bridgeService
		quitter *quitController
		setup   *setupService
		dialog  *dialogService
	)
	if setupMode {
		svc, setup, dialog, err = newSetupServices(stateDir, resolveImage(*imageFlag))
		if err != nil {
			return err
		}
	} else {
		svc, quitter, err = launch(cfg, socket, stateDir, resolveImage(*imageFlag), stackBinFlag)
		if err != nil {
			return err
		}
	}

	services := []application.Service{application.NewService(svc)}
	if setup != nil {
		services = append(services, application.NewService(setup), application.NewService(dialog))
	}
	app := application.New(application.Options{
		Name:        "compass-app",
		Description: "Compass native desktop shell",
		Services:    services,
		Assets: application.AssetOptions{
			Handler: application.BundledAssetFileServer(os.DirFS(assetsDir)),
		},
	})
	if dialog != nil {
		dialog.app = app
	}
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

	// The menu is installed in every mode. The embedded-only quit item is gated
	// on quitter; setup and client mode have no stack teardown controller.
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
		newAppWindow(app, svc, name, "Compass")
	})
	app.Menu.Set(menu)

	// Restore the persisted window set (Compass multi-window M1). An absent,
	// empty, or corrupt set opens one default Bridge window. Each window factory
	// reads the current shell mode when creating its startup script.
	names := windowNamesOrDefault(loadWindowSet(stateDir))
	for _, name := range names {
		newAppWindow(app, svc, name, "Compass")
	}

	mode, _ := svc.shellState()
	slog.Info("compass-app starting", "mode", mode, "socket", socket, "assets", assetsDir, "version", version)
	return app.Run()
}

// windowOptions builds the Wails options for a Compass window. Every window is a
// Bridge window (URL "/") at the fixed 1280x800 size. Name keys the persisted set
// and later per-frame routing, so it is always set.
func windowOptions(name, title, startupJS string) application.WebviewWindowOptions {
	return application.WebviewWindowOptions{
		Name:   name,
		Title:  title,
		Width:  1280,
		Height: 800,
		URL:    "/",
		// Startup globals run before the app bundle reads its boot mode.
		JS: startupJS,
	}
}

// newAppWindow creates a Bridge window and attaches its close-time cancellation
// handler so every window tears down its own in-flight bridge calls.
func newAppWindow(app *application.App, svc *bridgeService, name, title string) {
	mode, serverURL := svc.shellState()
	startupJS, err := shellStartupJS(mode, serverURL)
	if err != nil {
		slog.Error("compass-app building window startup script", "error", err)
		return
	}
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

// newSetupServices builds the shared first-run services without resolving or starting a stack.
func newSetupServices(stateDir, image string) (*bridgeService, *setupService, *dialogService, error) {
	configPath, err := appconfig.ConfigPath(os.Getenv("XDG_CONFIG_HOME"), os.Getenv("HOME"))
	if err != nil {
		return nil, nil, nil, err
	}
	gate := &firstRunGate{}
	picks := &caPicks{}
	wiring := &setupWiring{configPath: configPath, gate: gate, picks: picks}
	svc := newSetupBridgeService(nil, tokenstore.New(stateDir), wiring)
	preflight := realPreflight(image)
	setup := &setupService{
		gate: gate,
		svc:  svc,
		preflight: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, bringUpTimeout)
			defer cancel()
			return preflight(ctx)
		},
		save: func() error { return appconfig.SaveEmbedded(configPath) },
	}
	return svc, setup, &dialogService{picks: picks}, nil
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
			preflight: realPreflight(image),
			stackUp:   runStackUp(stackBin),
			whoAmI:    whoAmIOverUDS,
		}
		params := embeddedParams{socket: socket, stateDir: stateDir, image: image}

		// The embedded bring-up (preflight → stack up → WhoAmI) runs BEFORE the
		// window opens, under a bounded context. launch() is invoked once from run()
		// with no context upstream, so this context.Background() is the sanctioned
		// root of the bring-up pipeline, not a mid-tree re-root.
		bringUpCtx, cancel := context.WithTimeout(context.Background(), bringUpTimeout)
		accountID, quitter, err := runEmbedded(bringUpCtx, pipeline, params, runStackDown(stackBin))
		cancel()
		if err != nil {
			return nil, nil, err
		}

		conn := &connection{mode: cfg.Mode.String(), pump: bridge.NewPump(bridge.NewUnixTarget(socket))}
		svc := newBridgeService(conn, nil, nil)
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

// resolveMode resolves the --mode/$COMPASS_APP_MODE override for appconfig.Load.
// An empty flag falls back to the environment; an empty result leaves app.toml
// authoritative, and an absent file then opens the first-run chooser.
func resolveMode(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv("COMPASS_APP_MODE")
}

// resolveSocket picks the stack socket to serve/dial: the --socket flag, else
// $COMPASS_SOCKET, else $XDG_RUNTIME_DIR/compass/server.sock (the server
// default, go/server/socket.go DefaultSocketPath). A relative XDG_RUNTIME_DIR is
// treated as unset, matching the server, so the fallback is deterministic. In
// embedded mode the supervised stack serves this socket (passed as --socket and
// dialed for WhoAmI); the client arm dials the remote over TLS instead and never
// touches it.
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
