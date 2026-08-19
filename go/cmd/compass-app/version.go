package main

// version is the shell build version reported by the desktop app (the workspace
// version 0.1.0); override at build time with -ldflags
// "-X main.version=<v>". It lives in this untagged file so both the gtk3 shell
// (main.go) and the nogtk3 stub (main_nogtk3.go) link the same symbol and one
// -ldflags -X main.version stamps a single target.
var version = "0.1.0"

// versionRequested reports whether args (os.Args[1:]) is exactly a
// --version/-version request.
func versionRequested(args []string) bool {
	return len(args) == 1 && (args[0] == flagVersionLong || args[0] == flagVersionShort)
}

const (
	flagVersionLong  = "--version"
	flagVersionShort = "-version"
)
