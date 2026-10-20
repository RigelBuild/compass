//go:build (linux && gtk4) || darwin

package main

import (
	"runtime"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// appMenuActions are the Compass-specific menu callbacks. stopStackAndQuit is
// nil outside embedded mode, which has no stack to tear down.
type appMenuActions struct {
	newWindow        func()
	stopStackAndQuit func()
}

// buildAppMenu builds the platform-standard menu bar with Compass items added.
// macOS routes Cmd+C/V/X/A to the webview only through Edit-menu selectors, so
// the Edit role is load-bearing, not cosmetic. AppMenu is nil off darwin.
func buildAppMenu(actions appMenuActions) *application.Menu {
	menu := application.NewMenu()
	menu.AddRole(application.AppMenu)

	fileMenu := menu.AddSubmenu("File")
	fileMenu.Add("New Window").SetAccelerator("CmdOrCtrl+n").OnClick(func(*application.Context) {
		actions.newWindow()
	})
	fileMenu.AddSeparator()
	fileMenu.AddRole(application.CloseWindow)
	if actions.stopStackAndQuit != nil {
		fileMenu.AddSeparator()
		fileMenu.Add("Quit and stop stack").OnClick(func(*application.Context) {
			actions.stopStackAndQuit()
		})
	}
	// darwin's Quit lives in the app menu; other platforms put it under File.
	if runtime.GOOS != "darwin" {
		fileMenu.AddRole(application.Quit)
	}

	menu.AddRole(application.EditMenu)
	menu.AddRole(application.ViewMenu)
	menu.AddRole(application.WindowMenu)
	return menu
}
