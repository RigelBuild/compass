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
	"flag"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/sealedsecurity/compass/go/internal/bridge"
	"github.com/wailsapp/wails/v3/pkg/application"
)

func main() {
	if err := run(); err != nil {
		slog.Error("compass-app exited with an error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	socketFlag := flag.String("socket", "",
		"Unix socket the Compass daemon serves compass.v1 on. Defaults to "+
			"$COMPASS_SOCKET, then $XDG_RUNTIME_DIR/compass/server.sock. The "+
			"developer starts the daemon (compass-stack up); the shell only "+
			"dials it. Stack supervision is T4.")
	assetsFlag := flag.String("assets", "",
		"Directory of the prebuilt apps/ui dist to serve. Defaults to "+
			"$COMPASS_ASSETS_DIR, then a 'dist' directory beside the executable.")
	flag.Parse()

	socket := resolveSocket(*socketFlag)
	assetsDir := resolveAssetsDir(*assetsFlag)

	svc := newBridgeService(bridge.NewPump(bridge.NewUnixTarget(socket)), nil)

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
