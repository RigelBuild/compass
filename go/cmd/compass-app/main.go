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
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/sealedsecurity/compass/go/internal/appconfig"
	"github.com/sealedsecurity/compass/go/internal/bridge"
	"github.com/wailsapp/wails/v3/pkg/application"
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

	// The embedded launch pipeline (mode-select → preflight → stack up → WhoAmI)
	// runs BEFORE the window opens, under a bounded bring-up context. The
	// resolved account id is handed to the bridge service for the JS/UI.
	stateDir := resolveStateDir(*stateDirFlag)
	image := resolveImage(*imageFlag)
	stackBin, err := resolveStackBin(*stackBinFlag)
	if err != nil {
		return err
	}
	pipeline := embeddedPipeline{
		preflight: realPreflight(image, embeddedDatabaseDSN(stateDir)),
		stackUp:   runStackUp(stackBin),
		whoAmI:    whoAmIOverUDS,
	}
	params := embeddedParams{socket: socket, stateDir: stateDir, image: image}

	bringUpCtx, cancel := context.WithTimeout(context.Background(), bringUpTimeout)
	accountID, err := launchByMode(bringUpCtx, cfg.Mode, pipeline, params)
	cancel()
	if err != nil {
		return err
	}

	svc := newBridgeService(bridge.NewPump(bridge.NewUnixTarget(socket)), nil)
	svc.accountID = accountID

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

	// Explicit "Quit and stop stack" (DL-108, T4.2). Plain quit (window close,
	// OS quit) LINGERS by default — the stack children stay running and the app
	// does nothing to them (relaunch re-attaches), so there is deliberately NO
	// OnShutdown teardown here. Only this menu item tears the stack down.
	quitter := quitController{
		stackDown: runStackDown(stackBin),
		params:    params,
		bin:       stackBin,
		quit:      app.Quit,
		timeout:   stackDownTimeout,
	}
	menu := application.NewMenu()
	fileMenu := menu.AddSubmenu("File")
	fileMenu.Add("Quit and stop stack").OnClick(func(_ *application.Context) {
		// A UI-event callback has no inherited context.Context, so this is the
		// legitimate main-entrypoint root; stopStackAndQuit derives its bounded
		// teardown deadline from it.
		quitter.stopStackAndQuit(context.Background())
	})
	app.Menu.Set(menu)

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "Compass",
		Width:  1280,
		Height: 800,
		URL:    "/",
	})

	slog.Info("compass-app starting", "socket", socket, "assets", assetsDir)
	return app.Run()
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
