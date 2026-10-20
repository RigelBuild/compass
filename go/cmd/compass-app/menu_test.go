//go:build (linux && gtk4) || darwin

package main

import (
	"runtime"
	"slices"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"
)

func submenuLabels(menu *application.Menu) map[string][]string {
	out := map[string][]string{}
	for i := 0; menu.ItemAt(i) != nil; i++ {
		top := menu.ItemAt(i)
		if !top.IsSubmenu() {
			continue
		}
		sub := top.GetSubmenu()
		for j := 0; sub.ItemAt(j) != nil; j++ {
			if item := sub.ItemAt(j); !item.IsSeparator() {
				out[top.Label()] = append(out[top.Label()], item.Label())
			}
		}
	}
	return out
}

func TestBuildAppMenu(t *testing.T) {
	// Role constructors read the app name from the Wails global. New returns the
	// existing app when the GTK e2e harness already created one.
	quitLabel := "Quit " + application.New(application.Options{Name: "Compass"}).Config().Name
	darwin := runtime.GOOS == "darwin"
	for _, embedded := range []bool{false, true} {
		actions := appMenuActions{newWindow: func() {}}
		if embedded {
			actions.stopStackAndQuit = func() {}
		}
		menus := submenuLabels(buildAppMenu(actions))

		if got := slices.Contains(menus["File"], "Quit and stop stack"); got != embedded {
			t.Errorf("embedded=%v: File has stop-stack item = %v", embedded, got)
		}
		// Plain Quit must never be the stack teardown: it stays the standard role.
		quitMenu := "File"
		if darwin {
			quitMenu = "Compass"
		}
		if !slices.Contains(menus[quitMenu], quitLabel) {
			t.Errorf("embedded=%v: %s menu lacks the standard Quit: %v", embedded, quitMenu, menus[quitMenu])
		}
		// darwin delivers Cmd+V to the webview only through Edit; Linux must not
		// register Edit accelerators over WebKitGTK's own handling.
		if _, hasEdit := menus["Edit"]; hasEdit != darwin {
			t.Errorf("embedded=%v: Edit menu present = %v, want %v", embedded, hasEdit, darwin)
		}
		closeCount := 0
		for _, items := range menus {
			for _, label := range items {
				if label == "Close" || label == "Close Window" {
					closeCount++
				}
			}
		}
		if closeCount != 1 {
			t.Errorf("embedded=%v: %d Close items, want 1: %v", embedded, closeCount, menus)
		}
		for _, label := range menus["View"] {
			if label == "Reload" || label == "Force Reload" || label == "Open Developer Tools" {
				t.Errorf("embedded=%v: View menu has %q", embedded, label)
			}
		}
	}
}
