//go:build unix

package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/store"
)

// newServerSecretCmd builds the server-secret noun: the DEPLOYMENT-owned secret
// surface, disjoint from the fleet `secret` noun by reserved name prefix. It
// carries no logic of its own; its verb is a child that dials the Server and
// drives one SecretsService RPC.
func newServerSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server-secret",
		Short: "Manage deployment-owned server secrets (list)",
	}
	cmd.AddCommand(newServerSecretListCmd())
	return cmd
}

// newServerSecretListCmd builds `server-secret list`: ListServerSecrets and
// render each declared server secret's name and set/unset state. It NEVER
// renders a value (there is none on the wire). An empty list renders a clear
// message, not an error.
func newServerSecretListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List declared server secrets with set/unset state (never values)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := dialSecretsClient(cmd)
			if err != nil {
				return err
			}
			return runServerSecretList(cmd.Context(), client, cmd.OutOrStdout())
		},
	}
}

// runServerSecretList calls ListServerSecrets and renders each declared server
// secret. An empty list renders a clear message, not an error.
//
// Unlike the user path, declared does NOT imply set: the Server self-declares
// every server-secret NAME at boot while the operator populates the VALUES
// separately, so the set/unset column is the whole point of the verb.
func runServerSecretList(ctx context.Context, client compassv1connect.SecretsServiceClient, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := client.ListServerSecrets(ctx, connect.NewRequest(&compassv1.ListServerSecretsRequest{}))
	if err != nil {
		return fmt.Errorf("listing server secrets: %w", err)
	}
	secrets := resp.Msg.GetServerSecrets()
	if len(secrets) == 0 {
		_, err = fmt.Fprintln(out, "no server secrets declared for this deployment")
		return err
	}
	for _, s := range secrets {
		if err := renderServerSecretStatus(out, s); err != nil {
			return err
		}
	}
	return nil
}

// renderServerSecretStatus prints one server secret as "<BARE_NAME>: set|unset".
// It NEVER prints a value — none is carried on the wire.
//
// The reserved prefix is STRIPPED so the bare name — the spelling the operator
// configures — starts the line. That is load-bearing beyond readability: the
// deployment's seed script matches each secret with a line-anchored
// "\n<NAME>: " glob, which the stored prefixed form would never hit.
//
// ANY reserved prefix is stripped, not just SERVER_: the master key carries
// COMPASS_ (store.MasterKeyName) and is a real server_secrets row, so it lists
// here too. Stripping only one would print that row with its prefix intact
// while every other row appeared bare — the same output column meaning two
// different spellings.
func renderServerSecretStatus(out io.Writer, s *compassv1.ServerSecretStatus) error {
	state := "unset"
	if s.GetIsSet() {
		state = "set"
	}
	_, err := fmt.Fprintf(out, "%s: %s\n", bareServerSecretName(s.GetName()), state)
	return err
}

// bareServerSecretName strips whichever reserved server-secret prefix a stored
// name carries, returning the spelling the operator configured.
func bareServerSecretName(name string) string {
	for _, p := range []string{store.ServerSecretPrefix, store.GatewayCredentialsPrefix, store.CompassPrefix} {
		if bare, ok := strings.CutPrefix(name, p); ok {
			return bare
		}
	}
	return name
}
