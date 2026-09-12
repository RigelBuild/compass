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
// surface (set/list), disjoint from the fleet `secret` noun by reserved name
// prefix. It carries no logic of its own; each verb is a child that dials the
// Server and drives one SecretsService RPC. A server secret value is read from
// stdin, never argv, so it cannot leak into the process table.
func newServerSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server-secret",
		Short: "Manage deployment-owned server secrets (set / list)",
	}
	cmd.AddCommand(newServerSecretSetCmd(), newServerSecretListCmd())
	return cmd
}

// newServerSecretSetCmd builds `server-secret set <NAME>`: write a server
// secret's value. The value is read from stdin, never a flag or positional, so
// it cannot leak into the process table (the load-bearing convention shared
// with the fleet `secret set` verb and the bearer token).
//
// The name is accepted with OR without the reserved prefix, because the two
// sides spell it differently: the deployment's config carries the BARE name
// (the operator writes `forge.appId`-style config, not a registry key) while
// the server-secret registry carries the PREFIXED one (serve.go's
// serverSecretName wraps every declared name). Accepting both and sending the
// prefixed form means the operator can paste either spelling and still write
// the row the Server reads.
func newServerSecretSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <NAME>",
		Short: "Write a server secret's value (value read from stdin, admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialSecretsClient(cmd)
			if err != nil {
				return err
			}
			return runServerSecretSet(cmd.Context(), client, args[0], cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
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

// runServerSecretSet reads the value from in (trimming a single trailing
// newline and rejecting an empty value) and calls SetServerSecret under the
// prefixed name. The value is never taken from argv, so it cannot leak into the
// process table.
func runServerSecretSet(ctx context.Context, client compassv1connect.SecretsServiceClient, name string, in io.Reader, out io.Writer) error {
	value, err := readSecretValue(in)
	if err != nil {
		return err
	}
	wire, err := serverSecretWireName(name)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	if _, err := client.SetServerSecret(ctx, connect.NewRequest(&compassv1.SetServerSecretRequest{
		Name:  wire,
		Value: value,
	})); err != nil {
		return fmt.Errorf("setting server secret %s: %w", wire, err)
	}
	_, err = fmt.Fprintf(out, "set server secret %s\n", wire)
	return err
}

// serverSecretWireName maps the operator's spelling to the registry's. A name
// that already carries a reserved prefix is sent as-is (never double-prefixed);
// a bare one is wrapped, matching serve.go's serverSecretName. The store's
// HasServerSecretPrefix is the authority on what counts as prefixed, so the two
// doors cannot drift.
//
// A bare name that would SHADOW the master-key row is refused rather than
// wrapped. `list` strips any reserved prefix, so the master key prints as the
// bare `MASTER_KEY`; feeding that spelling back here would wrap it to
// `SERVER_MASTER_KEY`, which is a DIFFERENT secret. That name clears the
// server's master-key guard (it compares the exact COMPASS_MASTER_KEY name),
// so the write would silently mint a shadow row, leave the real key untouched,
// and make `list` print the same bare name twice. Refusing is the only safe
// answer: wrapping writes a different secret than the operator named, with no
// error at any layer.
func serverSecretWireName(name string) (string, error) {
	if store.HasServerSecretPrefix(name) {
		return name, nil
	}
	if store.CompassPrefix+name == store.MasterKeyName {
		return "", fmt.Errorf(
			"%s is the bare spelling of %s, which is provisioned and rotated by the server; pass the full name if you meant a different secret",
			name, store.MasterKeyName)
	}
	return store.ServerSecretPrefix + name, nil
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
