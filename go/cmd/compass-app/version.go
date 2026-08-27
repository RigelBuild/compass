package main

import (
	"fmt"
	"io"
)

const (
	flagVersionLong  = "--version"
	flagVersionShort = "-version"
)

// version is the shell build version reported by the desktop app (the workspace
// version 0.1.0); override at build time with -ldflags
// "-X main.version=<v>". It lives in this untagged file so both the gtk4 shell
// (main.go) and the nogtk4 stub (main_nogtk4.go) link the same symbol and one
// -ldflags -X main.version stamps a single target.
var version = "0.1.0"

// printVersionIfRequested writes version to w and reports handled=true when args
// (os.Args[1:]) is exactly a --version/-version request; otherwise it is a no-op
// returning handled=false. Keeping the decision and the write in one pure
// function lets the shell's --version path be tested without a gtk4/Wails
// display (main.go calls it before flag registration).
func printVersionIfRequested(args []string, w io.Writer) (handled bool, err error) {
	if len(args) != 1 || (args[0] != flagVersionLong && args[0] != flagVersionShort) {
		return false, nil
	}
	_, err = fmt.Fprintln(w, version)
	return true, err
}
