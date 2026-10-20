//go:build unix && gtk4

package main

import (
	"sync"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// TestNewWindowOpenerOpensUniqueWindowsE2E drives File > New Window's production
// opener against the real window manager, with concurrent clicks as Wails delivers them.
func TestNewWindowOpenerOpensUniqueWindowsE2E(t *testing.T) {
	if !hasDisplay() {
		t.Skip("no DISPLAY/WAYLAND_DISPLAY; run under the ci.yml gtk4 e2e gate")
	}
	names := []string{defaultWindowName, defaultWindowName + "-2", defaultWindowName + "-3"}
	t.Cleanup(func() {
		for _, name := range names {
			if win, ok := getWindow(name); ok {
				application.InvokeSync(func() { win.Close() })
			}
		}
	})

	open := newWindowOpener(e2eApp, newBridgeService(nil, e2eApp.Event, nil))
	var wg sync.WaitGroup
	for range names {
		wg.Go(open)
	}
	wg.Wait()

	for _, name := range names {
		mustGetWindow(t, name)
	}
	if _, ok := getWindow(defaultWindowName + "-4"); ok {
		t.Errorf("three opens created a fourth window")
	}
}
