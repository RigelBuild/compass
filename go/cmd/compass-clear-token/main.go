//go:build unix

// Command compass-clear-token removes the native client's stored bearer token.
// It is intentionally silent on success and never reads or prints the credential;
// tokenstore owns the keyring/file backend selection.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/RigelBuild/compass/go/internal/tokenstore"
)

// version is the build version; override at build time with -ldflags
// "-X main.version=<v>".
var version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "compass-clear-token:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("compass-clear-token", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	showVersion := fs.Bool("version", false, "Print the version and exit.")
	serverURL := fs.String("server-url", "", "HTTPS server URL whose stored bearer should be removed (required)")
	stateDir := fs.String("state-dir", "", "App state directory used by the token store (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		_, err := fmt.Fprintln(os.Stdout, version)
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *serverURL == "" {
		return errors.New("a server URL is required: pass --server-url")
	}
	if *stateDir == "" {
		return errors.New("a state directory is required: pass --state-dir")
	}
	store := tokenstore.New(*stateDir)
	if _, err := store.Read(*serverURL); errors.Is(err, tokenstore.ErrNotFound) {
		return nil
	} else if err != nil {
		return fmt.Errorf("read stored token: %w", err)
	}
	if err := store.Delete(*serverURL); err != nil {
		return fmt.Errorf("delete stored token: %w", err)
	}
	return nil
}
