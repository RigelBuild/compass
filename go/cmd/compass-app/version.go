package main

import (
	"fmt"
	"io"
)

const (
	flagVersionLong  = "--version"
	flagVersionShort = "-version"
)

// version is the ldflags-overridable build version shared by GTK and nogtk shells.
var version = "0.1.0"

// printVersionIfRequested handles an exact --version/-version request and is
// testable without a GTK display.
func printVersionIfRequested(args []string, w io.Writer) (handled bool, err error) {
	if len(args) != 1 || (args[0] != flagVersionLong && args[0] != flagVersionShort) {
		return false, nil
	}
	_, err = fmt.Fprintln(w, version)
	return true, err
}
