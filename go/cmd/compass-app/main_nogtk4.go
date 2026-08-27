//go:build linux && !gtk4

// The compass-app entrypoint for builds WITHOUT the gtk4 desktop stack.
//
// The real shell (main.go) is gated `unix && gtk4`: it imports Wails v3, whose
// Linux stack is GTK4 + WebKitGTK 6.0. Building it needs that stack's
// pkg-config, which the untagged toolchain does not carry. The module-wide gate
// `go build ./...` runs without that tag, so package main needs a valid,
// cgo-free entrypoint under the default tag too. This stub is that entrypoint:
// it builds everywhere, pulls in no GUI/cgo dependency, and — if a default-tag
// binary is ever actually run — reports that the desktop shell must be built
// with `-tags gtk4` rather than silently doing nothing.
//
// The shipped shell is always the `-tags gtk4` binary (the app-bundle/build.sh
// packaging build and the flake.nix compass-app package both pass `-tags gtk4`,
// as does the dev-box link); this file exists solely so the untagged module
// build has an entrypoint. It is not a runnable product, and it is not the
// deliverable.
package main

import (
	"log/slog"
	"os"
)

func main() {
	slog.Error("compass-app was built without the desktop shell; rebuild with -tags gtk4 "+
		"(the shell requires the GTK4 + WebKitGTK 6.0 stack)", "version", version)
	os.Exit(1)
}
