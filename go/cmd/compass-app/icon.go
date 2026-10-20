//go:build (linux && gtk4) || darwin

package main

import (
	_ "embed"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// appIcon is the macOS Dock icon on every run; Wails sets it at startup, so it
// also overrides a bundle's AppIcon.icns (built from this same PNG). GTK4 has no
// runtime icon API; Linux takes the icon from the desktop entry.
//
//go:embed icon/appicon.png
var appIcon []byte

// appOptions is the shared identity for every Wails app the shell creates.
// Linux.ApplicationID stays unset: Wails would also hand it to g_set_prgname,
// and WebKitGTK keys its data directory on prgname, so stored state would reset.
func appOptions() application.Options {
	return application.Options{
		Name:        "Compass",
		Description: "Compass native desktop shell",
		Icon:        appIcon,
	}
}
