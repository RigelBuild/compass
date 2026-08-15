//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
)

// CLI vocabulary tokens for the --delivery and --kind flags. Each is both an
// accepted flag value (parseDelivery/parseKind) and the operator-facing label
// rendered back (deliveryLabel/kindLabel), so one constant keeps input and
// output in sync.
const (
	deliveryEnv  = "env"
	deliveryFile = "file"

	kindGeneric  = "generic"
	kindProvider = "provider"
	kindGH       = "gh"
)

// newSecretCmd builds the secret noun: the fleet secrets operator surface
// (set/list/delete). It carries no logic of its own; each verb is a child that
// dials the Server and drives one SecretsService RPC. A secret value is read
// from stdin, never argv, so it cannot leak into the process table.
func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage fleet secrets (set / list / delete)",
	}
	cmd.AddCommand(newSecretSetCmd(), newSecretListCmd(), newSecretDeleteCmd())
	return cmd
}

// newSecretSetCmd builds `secret set <NAME> [--delivery] [--kind] [--provider]
// [--host]`: declare a secret's registry row and write its value. The value is
// read from stdin, never a flag or positional, so it cannot leak into the
// process table (the load-bearing convention shared with the bearer token).
func newSecretSetCmd() *cobra.Command {
	var delivery, kind, provider, host string
	cmd := &cobra.Command{
		Use:   "set <NAME>",
		Short: "Declare a secret and write its value (value read from stdin, admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialSecretsClient(cmd)
			if err != nil {
				return err
			}
			return runSecretSet(cmd.Context(), client, secretSetArgs{
				name:     args[0],
				delivery: delivery,
				kind:     kind,
				provider: provider,
				host:     host,
			}, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&delivery, "delivery", "",
		"How the secret is delivered to agents: env or file (required).")
	cmd.Flags().StringVar(&kind, "kind", kindGeneric,
		"Secret kind: generic, provider, or gh.")
	cmd.Flags().StringVar(&provider, kindProvider, "",
		"LLM provider id (required when --kind provider).")
	cmd.Flags().StringVar(&host, "host", "",
		"gh host (required when --kind gh).")
	return cmd
}

// newSecretListCmd builds `secret list`: ListSecrets and render each declared
// secret's name, set/unset state, and routing. It NEVER renders a value (there
// is none on the wire). An empty list renders a clear message, not an error.
func newSecretListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List declared secrets with set/unset state and routing (never values)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := dialSecretsClient(cmd)
			if err != nil {
				return err
			}
			return runSecretList(cmd.Context(), client, cmd.OutOrStdout())
		},
	}
}

// newSecretDeleteCmd builds `secret delete <NAME>`: DeleteSecret and confirm.
func newSecretDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <NAME>",
		Short: "Delete a declared secret (admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialSecretsClient(cmd)
			if err != nil {
				return err
			}
			return runSecretDelete(cmd.Context(), client, args[0], cmd.OutOrStdout())
		},
	}
}

// errEmptySecretValue names the empty-stdin rejection: a secret value is
// required and it is read from stdin.
var errEmptySecretValue = errors.New("a secret value is required: pipe it on stdin (it is never taken from the command line)")

// secretSetArgs is the resolved `secret set` input: the name and the routing
// flags, parsed and validated before any RPC.
type secretSetArgs struct {
	name     string
	delivery string
	kind     string
	provider string
	host     string
}

// parseDelivery maps the --delivery flag to its enum. It is required, so an
// empty or unknown value is a clear error naming the valid choices.
func parseDelivery(s string) (compassv1.SecretDelivery, error) {
	switch s {
	case deliveryEnv:
		return compassv1.SecretDelivery_SECRET_DELIVERY_ENV, nil
	case deliveryFile:
		return compassv1.SecretDelivery_SECRET_DELIVERY_FILE, nil
	default:
		return compassv1.SecretDelivery_SECRET_DELIVERY_UNSPECIFIED,
			fmt.Errorf("a delivery is required: pass --delivery env or --delivery file (got %q)", s)
	}
}

// parseKind maps the --kind flag to its enum and enforces the routing field each
// kind requires: provider carries a provider id, gh carries a host.
func parseKind(kind, provider, host string) (compassv1.SecretKind, error) {
	switch kind {
	case kindGeneric:
		return compassv1.SecretKind_SECRET_KIND_GENERIC, nil
	case kindProvider:
		if provider == "" {
			return compassv1.SecretKind_SECRET_KIND_UNSPECIFIED,
				errors.New("--kind provider requires --provider <id>")
		}
		return compassv1.SecretKind_SECRET_KIND_PROVIDER, nil
	case kindGH:
		if host == "" {
			return compassv1.SecretKind_SECRET_KIND_UNSPECIFIED,
				errors.New("--kind gh requires --host <h>")
		}
		return compassv1.SecretKind_SECRET_KIND_GH, nil
	default:
		return compassv1.SecretKind_SECRET_KIND_UNSPECIFIED,
			fmt.Errorf("unknown kind %q: pass --kind generic, provider, or gh", kind)
	}
}

// runSecretSet validates the routing flags, reads the value from in (trimming a
// single trailing newline and rejecting an empty value), and calls SetSecret.
// The value is never taken from argv, so it cannot leak into the process table.
func runSecretSet(ctx context.Context, client compassv1connect.SecretsServiceClient, args secretSetArgs, in io.Reader, out io.Writer) error {
	delivery, err := parseDelivery(args.delivery)
	if err != nil {
		return err
	}
	kind, err := parseKind(args.kind, args.provider, args.host)
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("reading secret value from stdin: %w", err)
	}
	value := strings.TrimRight(string(raw), "\n")
	if value == "" {
		return errEmptySecretValue
	}

	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	if _, err := client.SetSecret(ctx, connect.NewRequest(&compassv1.SetSecretRequest{
		Name:     args.name,
		Value:    value,
		Delivery: delivery,
		Kind:     kind,
		Provider: args.provider,
		Host:     args.host,
	})); err != nil {
		return fmt.Errorf("setting secret %s: %w", args.name, err)
	}
	_, err = fmt.Fprintf(out, "set secret %s\n", args.name)
	return err
}

// runSecretList calls ListSecrets and renders each declared secret. An empty
// list renders a clear message, not an error.
func runSecretList(ctx context.Context, client compassv1connect.SecretsServiceClient, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := client.ListSecrets(ctx, connect.NewRequest(&compassv1.ListSecretsRequest{}))
	if err != nil {
		return fmt.Errorf("listing secrets: %w", err)
	}
	secrets := resp.Msg.GetSecrets()
	if len(secrets) == 0 {
		_, err = fmt.Fprintln(out, "no secrets declared for this fleet")
		return err
	}
	for _, s := range secrets {
		if err := renderSecretStatus(out, s); err != nil {
			return err
		}
	}
	return nil
}

// renderSecretStatus prints one secret's name, set/unset state, delivery, kind,
// and its routing field (provider or host) when present. It NEVER prints a value
// — none is carried on the wire.
func renderSecretStatus(out io.Writer, s *compassv1.SecretStatus) error {
	state := "unset"
	if s.GetIsSet() {
		state = "set"
	}
	line := fmt.Sprintf("%s: %s  delivery=%s kind=%s",
		s.GetName(), state, deliveryLabel(s.GetDelivery()), kindLabel(s.GetKind()))
	if p := s.GetProvider(); p != "" {
		line += " provider=" + p
	}
	if h := s.GetHost(); h != "" {
		line += " host=" + h
	}
	_, err := fmt.Fprintln(out, line)
	return err
}

// deliveryLabel renders a SecretDelivery as the short operator-facing token
// (env/file), falling back to "unspecified" for the zero value.
func deliveryLabel(d compassv1.SecretDelivery) string {
	switch d {
	case compassv1.SecretDelivery_SECRET_DELIVERY_ENV:
		return deliveryEnv
	case compassv1.SecretDelivery_SECRET_DELIVERY_FILE:
		return deliveryFile
	default:
		return "unspecified"
	}
}

// kindLabel renders a SecretKind as the short operator-facing token, falling
// back to "unspecified" for the zero value.
func kindLabel(k compassv1.SecretKind) string {
	switch k {
	case compassv1.SecretKind_SECRET_KIND_GENERIC:
		return kindGeneric
	case compassv1.SecretKind_SECRET_KIND_PROVIDER:
		return kindProvider
	case compassv1.SecretKind_SECRET_KIND_GH:
		return kindGH
	default:
		return "unspecified"
	}
}

// runSecretDelete calls DeleteSecret and confirms.
func runSecretDelete(ctx context.Context, client compassv1connect.SecretsServiceClient, name string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	if _, err := client.DeleteSecret(ctx, connect.NewRequest(&compassv1.DeleteSecretRequest{Name: name})); err != nil {
		return fmt.Errorf("deleting secret %s: %w", name, err)
	}
	_, err := fmt.Fprintf(out, "deleted secret %s\n", name)
	return err
}
