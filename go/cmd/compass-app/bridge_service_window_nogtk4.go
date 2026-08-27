//go:build linux && !gtk4

// The non-gtk4 build's stub for the bridge service's per-window frame routing
// (M3). The untagged module build (`go build ./...`) and the unix-tagged bridge
// tests compile without the GTK stack, where github.com/wailsapp/wails/v3/pkg/
// application is unavailable (main_nogtk4.go documents why). There is no webview
// window to route to in this build, so windowFromContext always returns a nil
// dispatcher and every call falls back to the app-wide eventEmitter — the same
// behavior the bridge had before M3. The real per-window routing lives in
// bridge_service_window_gtk4.go, compiled into the shipped gtk4 shell.
package main

import "context"

// windowFromContext returns a nil windowDispatcher in the non-gtk4 build: there
// is no Wails window API here, so no call carries an originating window and the
// frame sink uses the app-wide eventEmitter fallback (bridge_service.go
// emitFrame). Matching windowFromContext in bridge_service_window_gtk4.go.
func windowFromContext(_ context.Context) windowDispatcher {
	return nil
}
