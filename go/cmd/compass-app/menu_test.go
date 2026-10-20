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

// accelerators returns every accelerator bound anywhere in the menu tree.
func accelerators(menu *application.Menu) []string {
	var out []string
	for i := 0; menu.ItemAt(i) != nil; i++ {
		item := menu.ItemAt(i)
		if a := item.GetAccelerator(); a != "" {
			out = append(out, a)
		}
		if item.IsSubmenu() {
			out = append(out, accelerators(item.GetSubmenu())...)
		}
	}
	return out
}

func TestBuildAppMenu(t *testing.T) {
	// Role constructors read the app name from the Wails global. New returns the
	// existing app when the GTK e2e harness already created one.
	name := application.New(application.Options{Name: "Compass"}).Config().Name
	darwin := runtime.GOOS == "darwin"
	// Wails names the app in the Quit label, and titles the app menu, only on darwin.
	quitMenu, quitLabel := "File", "Quit"
	if darwin {
		quitMenu, quitLabel = name, "Quit "+name
	}
	for _, embedded := range []bool{false, true} {
		actions := appMenuActions{newWindow: func() {}}
		if embedded {
			actions.stopStackAndQuit = func() {}
		}
		menu := buildAppMenu(actions)
		menus := submenuLabels(menu)

		if got := slices.Contains(menus["File"], "Quit and stop stack"); got != embedded {
			t.Errorf("embedded=%v: File has stop-stack item = %v", embedded, got)
		}
		if !slices.Contains(menus["File"], "New Window") || slices.Contains(menus["Window"], "New Window") {
			t.Errorf("embedded=%v: New Window must be in File only: File %v, Window %v",
				embedded, menus["File"], menus["Window"])
		}
		// Plain Quit must never be the stack teardown: it stays the standard role.
		if !slices.Contains(menus[quitMenu], quitLabel) {
			t.Errorf("embedded=%v: %s menu lacks %q: %v", embedded, quitMenu, quitLabel, menus[quitMenu])
		}
		// darwin delivers Cmd+C/V/X/A/Z and Backspace to the webview only through
		// Edit; Linux must bind none of them over WebKitGTK's own handling.
		bound := accelerators(menu)
		for _, key := range editingAccelerators(t) {
			if got := slices.Contains(bound, key); got != darwin {
				t.Errorf("embedded=%v: accelerator %q bound = %v, want %v", embedded, key, got, darwin)
			}
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

// editingAccelerators renders the Wails Edit-role keys in GetAccelerator's form.
func editingAccelerators(t *testing.T) []string {
	t.Helper()
	specs := []string{
		"CmdOrCtrl+z", "CmdOrCtrl+Shift+z", "CmdOrCtrl+x", "CmdOrCtrl+c",
		"CmdOrCtrl+v", "CmdOrCtrl+a", "backspace",
	}
	keys := make([]string, 0, len(specs))
	for _, spec := range specs {
		keys = append(keys, application.NewMenuItem("").SetAccelerator(spec).GetAccelerator())
	}
	return keys
}
