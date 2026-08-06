//go:build unix && !gtk3

// The compass-app entrypoint for builds WITHOUT the gtk3 desktop stack.
//
// The real shell (main.go) is gated `unix && gtk3`: it imports Wails v3, whose
// only Linux stack available in this repo's frozen toolchain is GTK3 +
// WebKit2GTK 4.1 (the GTK4/WebKit6.0 default path has no pkg-config here). The
// module-wide gate `go build ./...` runs without that tag, so package main needs
// a valid, cgo-free entrypoint under the default tag too. This stub is that
// entrypoint: it builds everywhere, pulls in no GUI/cgo dependency, and — if a
// default-tag binary is ever actually run — reports that the desktop shell must
// be built with `-tags gtk3` rather than silently doing nothing.
//
// The shipped shell is always the `-tags gtk3` binary (go/moon.yml build lane +
// the dev-box link); this file exists solely so the untagged module build has an
// entrypoint. It is not a runnable product, and it is not the deliverable.
package main

import (
	"log/slog"
	"os"
)

func main() {
	slog.Error("compass-app was built without the desktop shell; rebuild with -tags gtk3 " +
		"(the shell requires the GTK3 + WebKit2GTK 4.1 stack)")
	os.Exit(1)
}
