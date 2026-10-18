//go:build unix && gtk4

// gtk4-only e2e helpers: the real-window plumbing the multi-window smoke gate
// needs and the unix (non-gtk4) unit suite has no equivalent for, because they
// touch application.Window and application.WindowKey — the very symbols the
// windowDispatcher seam exists to keep out of the unix build.
package main

import (
	"context"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// getWindow looks the named window up on the real app, on the main thread (the
// window manager is mutated from the GTK loop, so a read races it otherwise).
func getWindow(name string) (application.Window, bool) {
	return application.InvokeSyncWithResultAndOther(func() (application.Window, bool) {
		return e2eApp.Window.GetByName(name)
	})
}

// mustGetWindow returns the named window or fails: after newAppWindow created
// it, the shell must have registered it under that name.
func mustGetWindow(t *testing.T, name string) application.Window {
	t.Helper()
	win, ok := getWindow(name)
	if !ok {
		t.Fatalf("window %q not found after newAppWindow", name)
	}
	return win
}

// launchWindowedCall uses production context capture so its windowDispatcher
// matches the close handler. The channel closes when the call ends.
func launchWindowedCall(svc *bridgeService, win application.Window, requestID string) <-chan struct{} {
	ctx := context.WithValue(context.Background(), application.WindowKey, win)
	callCtx, call := svc.register(ctx, requestID)
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.run(callCtx, call, rpcRequest{RequestID: requestID, Path: "/e2e/" + requestID})
	}()
	return done
}
