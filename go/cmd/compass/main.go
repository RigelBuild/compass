//go:build unix

// Command compass is the operator-facing Compass CLI: a Cobra subcommand tree
// that dials the Compass Server's authenticated door and drives fleet-wide
// operator actions. Today it hosts the agent-config noun (push/show/delete of
// the fleet config bundle, SEA-1671); secrets is a planned future sibling noun,
// so the root is kept extensible rather than agent-config-specific.
//
// Unlike the other cmd/ binaries this one deliberately uses Cobra (the
// subcommand tree) + Viper (server-addr precedence: flag -> env -> config file),
// the gh/kubectl analog for a multi-noun operator tool. The admin bearer token
// is env/file only, NEVER a flag, so it cannot leak into the process table
// (mirroring cmd/compass-runner/main.go:105-107 and internal/runner/runner.go).
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// version is the CLI build version; override at build time with -ldflags
// "-X main.version=<v>".
var version = "0.1.0"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "compass:", err)
		os.Exit(1)
	}
}

// newRootCmd builds the compass root command with the persistent connection
// flags every subcommand shares and mounts the agent-config noun. Secrets is a
// future sibling noun; adding it is one more AddCommand here.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "compass",
		Short:         "Operator CLI for a Compass fleet",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	addConnFlags(root.PersistentFlags())

	root.AddCommand(newAgentConfigCmd())
	return root
}

// addConnFlags registers the shared connection flags on a flag set — the root's
// persistent flags in the binary, and a subcommand's own flags in tests that
// exercise a command in isolation.
func addConnFlags(f *pflag.FlagSet) {
	f.String("server-addr", "",
		"Compass Server base URL (e.g. https://server.example:443). "+
			"Falls back to $COMPASS_SERVER_ADDR, then the config file.")
	f.String("ca", "",
		"PEM CA/certificate to trust for the Server's TLS door instead of the "+
			"system roots (the local dogfood self-signed cert). Falls back to "+
			"$COMPASS_ADMIN_CA, then the config file.")
	f.String("token-file", "",
		"Path to a file holding the admin bearer token, read 0600-style. The "+
			"token is env/file only, never a flag, so it cannot leak into the "+
			"process table. $COMPASS_ADMIN_TOKEN takes precedence when set.")
	f.String("config", "",
		"Path to a config file supplying server-addr/ca (lowest precedence).")
}
