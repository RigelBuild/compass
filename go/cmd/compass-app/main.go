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
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/RigelBuild/compass/go/internal/appconfig"
	"github.com/RigelBuild/compass/go/internal/bridge"
	"github.com/RigelBuild/compass/go/internal/embedded"
	"github.com/RigelBuild/compass/go/internal/tokenstore"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

func main() {
	ctx := context.Background()
	if err := run(ctx); err != nil {
		slog.ErrorContext(ctx, "compass-app exited with an error", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
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
			"$COMPASS_AGENT_IMAGE, then "+embedded.DefaultAgentImage+".")
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
		svc, setup, dialog, err = newSetupServices(stateDir, embedded.ResolveImage(*imageFlag))
		if err != nil {
			return err
		}
	} else {
		svc, quitter, err = launch(ctx, cfg, socket, stateDir, embedded.ResolveImage(*imageFlag), stackBinFlag)
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
			Handler:    application.BundledAssetFileServer(os.DirFS(assetsDir)),
			Middleware: shellGlobalsMiddleware(svc),
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
			slog.ErrorContext(ctx, "compass-app persisting window set", "error", err)
		}
	})

	// The menu is installed in every mode. The embedded-only quit item is gated
	// on quitter; setup and client mode have no stack teardown controller.
	menu := application.NewMenu()
	if quitter != nil {
		quitter.quit = app.Quit
		fileMenu := menu.AddSubmenu("File")
		fileMenu.Add("Quit and stop stack").OnClick(func(_ *application.Context) {
			// stopStackAndQuit derives its bounded teardown deadline from the run root.
			quitter.stopStackAndQuit(ctx)
		})
	}
	windowMenu := menu.AddSubmenu("Window")
	windowMenu.Add("New Window").OnClick(func(_ *application.Context) {
		name := nextWindowName(app)
		newAppWindow(app, svc, name, "Compass")
	})
	app.Menu.Set(menu)

	// Restore saved windows; each page reads shell state when its script loads.
	names := windowNamesOrDefault(loadWindowSet(stateDir))
	for _, name := range names {
		newAppWindow(app, svc, name, "Compass")
	}

	mode, _ := svc.shellState()
	slog.InfoContext(ctx, "compass-app starting", "mode", mode, "socket", socket, "assets", assetsDir, "version", version)
	return app.Run()
}

// windowOptions keeps every persisted window on the shared asset route.
func windowOptions(name, title string) application.WebviewWindowOptions {
	return application.WebviewWindowOptions{
		Name:   name,
		Title:  title,
		Width:  1280,
		Height: 800,
		URL:    "/",
	}
}

// shellGlobalsMiddleware serves startup globals from the service's current state.
func shellGlobalsMiddleware(svc *bridgeService) application.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/compass-shell-globals.js" || r.Method != http.MethodGet {
				next.ServeHTTP(w, r)
				return
			}

			mode, serverURL := svc.shellState()
			script, err := shellStartupJS(mode, serverURL)
			if err != nil {
				http.Error(w, "unable to encode shell globals", http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			if _, err := io.WriteString(w, script); err != nil {
				slog.ErrorContext(r.Context(), "compass-app writing shell globals", "error", err)
			}
		})
	}
}

// newAppWindow creates a Bridge window and attaches its close-time cancellation
// handler so every window tears down its own in-flight bridge calls.
func newAppWindow(app *application.App, svc *bridgeService, name, title string) {
	opts := windowOptions(name, title)
	win := app.Window.NewWithOptions(opts)
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
	preflight := embedded.RealPreflight(image)
	setup := &setupService{
		gate: gate,
		svc:  svc,
		preflight: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, embedded.BringUpTimeout)
			defer cancel()
			return preflight(ctx)
		},
		save: func(ctx context.Context) error { return appconfig.SaveEmbedded(ctx, configPath) },
	}
	return svc, setup, &dialogService{picks: picks}, nil
}

// launch dispatches the resolved mode and returns its bridge service. Only the
// embedded arm resolves compass-stack and builds a quit controller; client-only
// installs have no local stack to stop.
func launch(
	ctx context.Context, cfg appconfig.Config, socket, stateDir, image string, stackBinFlag *string,
) (*bridgeService, *quitController, error) {
	switch cfg.Mode {
	case appconfig.ModeEmbedded:
		stackBin, err := embedded.ResolveStackBin(*stackBinFlag)
		if err != nil {
			return nil, nil, err
		}
		pipeline := embedded.Pipeline{
			Preflight: embedded.RealPreflight(image),
			StackUp:   embedded.RunStackUp(stackBin),
			WhoAmI:    embedded.WhoAmIOverUDS,
		}
		params := embedded.Params{Socket: socket, StateDir: stateDir, Image: image}

		// Embedded bring-up completes under a bounded context before the window opens.
		bringUpCtx, cancel := context.WithTimeout(ctx, embedded.BringUpTimeout)
		accountID, quitter, err := runEmbedded(bringUpCtx, pipeline, params, embedded.RunStackDown(stackBin))
		cancel()
		if err != nil {
			if errors.Is(err, embedded.ErrStackVersionMismatch) {
				return nil, nil, restartOlderStack(ctx, params, embedded.RunStackDown(stackBin))
			}
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

// shellStartupJS JSON-encodes values before assigning webview globals, so URL
// text cannot break out of a JavaScript string literal.
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

// resolveAssetsDir uses the flag, environment, then executable layout because
// the dist directory is outside this package.
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

// resolveMode uses the flag, then environment; empty leaves app.toml authoritative
// and may open the first-run chooser.
func resolveMode(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv("COMPASS_APP_MODE")
}

// resolveSocket uses the flag, COMPASS_SOCKET, then the server default. A
// relative XDG_RUNTIME_DIR is ignored; client mode dials remotely over TLS.
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

// distDirForExecutable handles macOS app bundles and sibling dist layouts.
func distDirForExecutable(exe string) string {
	dir := filepath.Dir(exe)
	if filepath.Base(dir) == "MacOS" && filepath.Base(filepath.Dir(dir)) == "Contents" {
		return filepath.Join(filepath.Dir(dir), "Resources", "dist")
	}
	return filepath.Join(dir, "dist")
}

// runEmbedded runs preflight, stack up, and WhoAmI, then returns the embedded
// quit controller. Client installs have no local stack to stop.
func runEmbedded(
	ctx context.Context,
	pipeline embedded.Pipeline,
	params embedded.Params,
	stackDown func(ctx context.Context, args []string) error,
) (string, *quitController, error) {
	accountID, err := pipeline.Run(ctx, params)
	if err != nil {
		return "", nil, err
	}
	quitter := &quitController{
		stackDown: stackDown,
		params:    params,
		timeout:   embedded.StackDownTimeout,
	}
	return accountID, quitter, nil
}
