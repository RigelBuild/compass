//go:build (linux && gtk3) || darwin

// The gtk3 build's binding of the bridge service's per-window frame routing
// (M3) to the real Wails window API. It is split out of bridge_service.go
// (//go:build unix) so that file — and its unix-tagged tests — never import
// github.com/wailsapp/wails/v3/pkg/application, which only compiles under
// -tags gtk3 on this toolchain (the default GTK4/WebKit6.0 path has no
// pkg-config here; main_nogtk3.go documents it repo-wide). This mirrors the
// main.go / main_nogtk3.go split: the real shell is the gtk3 binary, and the
// windowDispatcher seam (bridge_service.go) is what keeps the forwarding path
// testable without the GTK stack.
package main

import (
	"context"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// windowFromContext extracts the originating webview window from a bound-method
// call context. Wails puts the calling window under application.WindowKey
// (messageprocessor_call.go:16,136); the comma-ok assertion yields a nil window
// when no key is present (a windowless transport, or a direct test call).
//
// It returns an UNTYPED-nil windowDispatcher when no window is present — never a
// typed nil wrapping a nil *application.WebviewWindow — so the nil check in
// emitFrame (call.window != nil) correctly selects the app-wide fallback.
func windowFromContext(ctx context.Context) windowDispatcher {
	win, ok := ctx.Value(application.WindowKey).(application.Window)
	if !ok || win == nil {
		return nil
	}
	return wailsWindowDispatcher{win: win}
}

// wailsWindowDispatcher adapts an application.Window to the windowDispatcher
// seam: dispatch delivers one response frame to that window's webview via
// DispatchWailsEvent (webview_window.go:1372), the per-window analogue of the
// app-wide eventEmitter.Emit. The struct is comparable (a single interface
// field), so an inflightCall.window value can index M3b's cancel-all-for-window.
type wailsWindowDispatcher struct {
	win application.Window
}

// dispatch routes one frame to this window's webview only. DispatchWailsEvent
// internally no-ops once the window isDestroyed() (webview_window.go:1373), so a
// frame for a call whose window has since closed is dropped, not broadcast (A4).
func (d wailsWindowDispatcher) dispatch(name string, resp responseFrame) {
	d.win.DispatchWailsEvent(&application.CustomEvent{Name: name, Data: resp})
}
