//go:build (linux && gtk4) || darwin

// This gtk4-only binding connects per-window frame routing to Wails. Keeping it
// separate lets unix builds and tests avoid importing the GTK-only application
// package; the windowDispatcher seam keeps forwarding testable without GTK.
package main

import (
	"context"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// windowFromContext extracts Wails' originating window. Missing or nil values
// return an untyped nil so emitFrame uses the app-wide fallback.
func windowFromContext(ctx context.Context) windowDispatcher {
	win, ok := ctx.Value(application.WindowKey).(application.Window)
	if !ok || win == nil {
		return nil
	}
	return wailsWindowDispatcher{win: win}
}

// wailsWindowDispatcher adapts application.Window to the comparable routing
// seam used for close-time lookup.
type wailsWindowDispatcher struct {
	win application.Window
}

// dispatch targets only this window; Wails drops frames after it closes.
func (d wailsWindowDispatcher) dispatch(name string, resp responseFrame) {
	d.win.DispatchWailsEvent(&application.CustomEvent{Name: name, Data: resp})
}
