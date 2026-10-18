//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
)

// newTokenCmd builds the bearer-token operator surface. Children dial the
// server for one token RPC; plaintext comes from stdin, never argv.
func newTokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage bearer tokens",
	}
	cmd.AddCommand(newRevokeCmd())
	return cmd
}

// newRevokeCmd reads the token from stdin to keep it out of argv; the server
// hashes the received plaintext.
func newRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke",
		Short: "Revoke a bearer token (token read from stdin, admin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := dialClient(cmd)
			if err != nil {
				return err
			}
			return runRevoke(cmd.Context(), client, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}

// errEmptyRevokeToken names the empty-stdin rejection: a token is required and it
// is read from stdin, never the command line.
var errEmptyRevokeToken = errors.New("a token is required: pipe it on stdin (it is never taken from the command line)")

// runRevoke reads the token plaintext from in (trimming surrounding whitespace
// and rejecting an empty value before any RPC), then calls RevokeToken with the
// plaintext. The server hashes it; the CLI never does. The token is never taken
// from argv, so it cannot leak into the process table.
func runRevoke(ctx context.Context, client compassv1connect.CompassServiceClient, in io.Reader, out io.Writer) error {
	raw, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("reading token from stdin: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return errEmptyRevokeToken
	}

	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	if _, err := client.RevokeToken(ctx, &compassv1.RevokeTokenRequest{
		Token: token,
	}); err != nil {
		return fmt.Errorf("revoking token: %w", err)
	}
	_, err = fmt.Fprintln(out, "revoked token")
	return err
}
