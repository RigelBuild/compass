//go:build (linux && gtk4) || darwin

package main

import (
	"runtime"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// appMenuActions: stopStackAndQuit is nil outside embedded mode, which has no
// stack to tear down.
type appMenuActions struct {
	newWindow        func()
	stopStackAndQuit func()
}

// buildAppMenu: macOS routes Cmd+C/V/X/A to the webview only through Edit-menu
// selectors. Off darwin the Wails Edit role steals those keys (and bare
// Backspace) from WebKitGTK as app accelerators, so Linux keeps native editing.
func buildAppMenu(actions appMenuActions) *application.Menu {
	darwin := runtime.GOOS == "darwin"
	menu := application.NewMenu()
	if darwin {
		menu.AddRole(application.AppMenu)
	}

	fileMenu := menu.AddSubmenu("File")
	fileMenu.Add("New Window").SetAccelerator("CmdOrCtrl+n").OnClick(func(*application.Context) {
		actions.newWindow()
	})
	// Off darwin the Window role already carries Close.
	if darwin {
		fileMenu.AddSeparator()
		fileMenu.AddRole(application.CloseWindow)
	}
	if actions.stopStackAndQuit != nil {
		fileMenu.AddSeparator()
		fileMenu.Add("Quit and stop stack").OnClick(func(*application.Context) {
			actions.stopStackAndQuit()
		})
	}
	if !darwin {
		fileMenu.AddSeparator()
		fileMenu.AddRole(application.Quit)
	}

	if darwin {
		menu.AddRole(application.EditMenu)
	}
	// Hand-built: the View role adds Reload, which drops UI state, and Developer
	// Tools in any build without the production tag.
	viewMenu := menu.AddSubmenu("View")
	viewMenu.AddRole(application.ResetZoom)
	viewMenu.AddRole(application.ZoomIn)
	viewMenu.AddRole(application.ZoomOut)
	viewMenu.AddSeparator()
	viewMenu.AddRole(application.ToggleFullscreen)
	menu.AddRole(application.WindowMenu)
	return menu
}
