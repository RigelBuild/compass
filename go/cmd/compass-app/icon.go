//go:build (linux && gtk4) || darwin

package main

import _ "embed"

// appID is the reverse-DNS app id shared with the macOS bundle identifier and
// the Linux desktop entry.
const appID = "build.rigel.compass"

// appIcon sets the Dock/taskbar icon at runtime, so an unbundled binary (the dev
// loop) shows Compass rather than the generic executable icon.
//
//go:embed icon/appicon.png
var appIcon []byte
