//go:build (linux && gtk4) || darwin

package main

import (
	_ "embed"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// appID is the reverse-DNS app id shared with the macOS bundle identifier and
// the Linux desktop entry's StartupWMClass.
const appID = "build.rigel.compass"

// appIcon sets the Dock/taskbar icon at runtime, so an unbundled binary (the dev
// loop) shows Compass rather than the generic executable icon.
//
//go:embed icon/appicon.png
var appIcon []byte

// appOptions is the shared identity for every Wails app the shell creates.
func appOptions() application.Options {
	return application.Options{
		Name:        "Compass",
		Description: "Compass native desktop shell",
		Icon:        appIcon,
		Linux:       application.LinuxOptions{ApplicationID: appID},
	}
}
