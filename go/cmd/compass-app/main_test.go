//go:build (linux && gtk4) || darwin

package main

import (
	"path/filepath"
	"testing"
	"time"
)

// TestDistDirForExecutable pins the packaging-layout dist resolution: a macOS
// .app stages the binary at Contents/MacOS/compass-app and the UI dist at
// Contents/Resources/dist (the macos-bundle tool, compass-distribution T3), so
// a Finder launch — which passes no --assets flag and sets no
// $COMPASS_ASSETS_DIR — must resolve the sibling Contents/Resources/dist, not a
// non-existent Contents/MacOS/dist. Every other layout keeps dist beside the
// executable (the Linux thin client's bin/compass-app + bin/dist, and dev
// builds).
func TestDistDirForExecutable(t *testing.T) {
	t.Run("macOS .app resolves Contents/Resources/dist", func(t *testing.T) {
		exe := filepath.Join("/Applications", "Compass.app", "Contents", "MacOS", "compass-app")
		want := filepath.Join("/Applications", "Compass.app", "Contents", "Resources", "dist")
		if got := distDirForExecutable(exe); got != want {
			t.Errorf("got %q, want %q (.app dist lives in Contents/Resources)", got, want)
		}
	})

	t.Run("Linux thin client resolves dist beside the executable", func(t *testing.T) {
		exe := filepath.Join("/opt", "compass", "bin", "compass-app")
		want := filepath.Join("/opt", "compass", "bin", "dist")
		if got := distDirForExecutable(exe); got != want {
			t.Errorf("got %q, want %q (non-.app layouts stage dist beside the binary)", got, want)
		}
	})

	t.Run("dev build resolves dist beside the executable", func(t *testing.T) {
		exe := filepath.Join(t.TempDir(), "compass-app")
		want := filepath.Join(filepath.Dir(exe), "dist")
		if got := distDirForExecutable(exe); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("MacOS dir not under Contents stays beside the executable", func(t *testing.T) {
		// A directory literally named MacOS but NOT nested under Contents must
		// not trigger the .app rule — the layout guard checks both segments.
		exe := filepath.Join("/srv", "MacOS", "compass-app")
		want := filepath.Join("/srv", "MacOS", "dist")
		if got := distDirForExecutable(exe); got != want {
			t.Errorf("got %q, want %q (bare MacOS dir is not a .app bundle)", got, want)
		}
	})
}

// TestBringUpTimeoutBudgetsDarwinColdProvisioning pins that darwin gets a
// materially wider bring-up window than linux. The machine ensure step runs
// inside this budget, and a cold `podman machine init` downloads a VM image
// before the stack pulls a single container image, so a linux-sized window
// would deadline every first launch on a fresh Mac and report the timeout
// instead of the download. The assertion is a floor and a relation, not the
// literal figures: re-tuning either budget is fine, collapsing the darwin one
// back onto the linux one is the regression.
func TestBringUpTimeoutBudgetsDarwinColdProvisioning(t *testing.T) {
	linux := bringUpTimeoutFor("linux")
	darwin := bringUpTimeoutFor("darwin")

	if darwin <= linux {
		t.Errorf("bringUpTimeoutFor(darwin) = %v, not greater than linux %v; a cold "+
			"podman machine init cannot fit a linux-sized window", darwin, linux)
	}
	// A cold VM-image download plus three registry pulls does not fit in five
	// minutes on an ordinary connection.
	if darwin < 10*time.Minute {
		t.Errorf("bringUpTimeoutFor(darwin) = %v, too tight for a cold machine init "+
			"plus three image pulls", darwin)
	}
	if linux <= 0 {
		t.Errorf("bringUpTimeoutFor(linux) = %v, want a positive backstop", linux)
	}
}
