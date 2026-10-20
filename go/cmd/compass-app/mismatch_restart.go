//go:build (linux && gtk4) || darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/RigelBuild/compass/go/internal/embedded"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

const (
	stackMismatchTitle   = "Older Compass stack running"
	stackMismatchMessage = "A Compass stack from an older version is still running. Stop it and start this version? Running agent sessions will be stopped."
)

// restartOlderStack asks before stopping a version-mismatched lingering stack.
// The re-exec waits until the dialog app's loop has exited, so the new process
// never starts while the old GTK/AppKit loop still owns the display.
func restartOlderStack(ctx context.Context, params embedded.Params, down func(context.Context, []string) error) error {
	app := application.New(appOptions())
	result := make(chan error, 1)
	restart := false
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		err := handleStackVersionMismatch(
			ctx,
			params,
			func() (bool, error) { return confirmOlderStack(app) },
			down,
			func() error { restart = true; return nil },
			func(message string) { showStackDownError(app, message) },
		)
		result <- err
		app.Quit()
	})

	if err := app.Run(); err != nil {
		return fmt.Errorf("running older-stack confirmation dialog: %w", err)
	}
	select {
	case err := <-result:
		if err != nil || !restart {
			return err
		}
		return reexecApp()
	default:
		return errors.New("user declined stopping the older Compass stack")
	}
}

// confirmOlderStack blocks on a button callback. Every user exit maps to a
// button: a parentless NSAlert has no close box and Escape triggers the cancel
// button; on GTK, closing the dialog reports the cancel button.
func confirmOlderStack(app *application.App) (bool, error) {
	choice := make(chan bool, 1)
	dialog := app.Dialog.Question().
		SetTitle(stackMismatchTitle).
		SetMessage(stackMismatchMessage)
	dialog.AddButton("Stop and restart").SetAsDefault().OnClick(func() { choice <- true })
	dialog.AddButton("Quit").SetAsCancel().OnClick(func() { choice <- false })
	dialog.Show()
	return <-choice, nil
}

func showStackDownError(app *application.App, message string) {
	done := make(chan struct{})
	dialog := app.Dialog.Error().
		SetTitle("Unable to stop older Compass stack").
		SetMessage("The older Compass stack could not be stopped. " + message)
	dialog.AddButton("Quit").SetAsDefault().SetAsCancel().OnClick(func() { close(done) })
	dialog.Show()
	<-done
}

func reexecApp() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding compass-app executable: %w", err)
	}
	//nolint:gosec // G204: re-execs this same binary with its own argv; no outside input.
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		return fmt.Errorf("replacing compass-app process: %w", err)
	}
	return nil
}

func handleStackVersionMismatch(
	ctx context.Context,
	params embedded.Params,
	confirm func() (bool, error),
	down func(context.Context, []string) error,
	reexec func() error,
	showError func(string),
) error {
	confirmed, err := confirm()
	if err != nil {
		return fmt.Errorf("confirming older-stack restart: %w", err)
	}
	if !confirmed {
		return errors.New("user declined stopping the older Compass stack")
	}

	downCtx, cancel := context.WithTimeout(ctx, embedded.StackDownTimeout)
	defer cancel()
	if err := down(downCtx, embedded.StackDownArgs(params)); err != nil {
		showError(err.Error())
		return fmt.Errorf("stopping older Compass stack: %w", err)
	}
	if err := reexec(); err != nil {
		return fmt.Errorf("restarting compass-app: %w", err)
	}
	return nil
}
