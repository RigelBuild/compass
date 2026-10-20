//go:build (linux && gtk4) || darwin

package main

import (
	_ "embed"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// appIcon is the macOS Dock icon for an unbundled binary (the dev loop). GTK4
// has no runtime icon API; Linux takes the icon from the desktop entry.
//
//go:embed icon/appicon.png
var appIcon []byte

// appOptions is the shared identity for every Wails app the shell creates.
// Linux.ApplicationID stays unset so GLib keeps prgname compass-app: WebKitGTK's
// data dir and the X11 WM_CLASS (StartupWMClass in compass.desktop) key on it.
func appOptions() application.Options {
	return application.Options{
		Name:        "Compass",
		Description: "Compass native desktop shell",
		Icon:        appIcon,
	}
}
