//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
)

// newAgentConfigCmd builds the agent-config noun: the fleet config-bundle
// operator surface (push/show/delete). It carries no logic of its own; each verb
// is a child that dials the Server and drives one RPC.
func newAgentConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent-config",
		Short: "Manage the fleet agent-config bundle (skills / extensions / mcp)",
	}
	cmd.AddCommand(newPushCmd(), newShowCmd(), newDeleteCmd())
	return cmd
}

// newPushCmd builds `agent-config push --dir <path>`: tar+gzip the dir into a
// bundle the store door accepts and PutAgentConfig it (admin-gated), printing
// the returned canonical version.
func newPushCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Push a config bundle from a local directory (admin)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dir == "" {
				return errMissingDir
			}
			client, err := dialClient(cmd)
			if err != nil {
				return err
			}
			return runPush(cmd.Context(), client, dir, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "",
		"Local bundle root directory whose children are skills/, extensions/, mcp/.")
	return cmd
}

// newShowCmd builds `agent-config show`: GetAgentConfigInfo and render the
// current version plus the declared member names bucketed by top dir. An
// unconfigured fleet renders as a clear "no config" message, not an error.
func newShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show the current config bundle version and member names",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := dialClient(cmd)
			if err != nil {
				return err
			}
			return runShow(cmd.Context(), client, cmd.OutOrStdout())
		},
	}
}

// newDeleteCmd builds `agent-config delete`: DeleteAgentConfig (admin-gated,
// idempotent) and print a confirmation.
func newDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete",
		Short: "Clear the fleet config bundle back to unconfigured (admin)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := dialClient(cmd)
			if err != nil {
				return err
			}
			return runDelete(cmd.Context(), client, cmd.OutOrStdout())
		},
	}
}

// errMissingDir names the required --dir input and how to supply it.
var errMissingDir = errors.New("a bundle directory is required: pass --dir <path>")

// runPush builds the bundle and calls PutAgentConfig, printing the version the
// Server assigns. The bundle build validates the dir client-side, so a malformed
// dir fails with a clear message before any RPC.
func runPush(ctx context.Context, client compassv1connect.CompassServiceClient, dir string, out io.Writer) error {
	bundle, err := buildBundle(dir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := client.PutAgentConfig(ctx, connect.NewRequest(&compassv1.PutAgentConfigRequest{
		Bundle: bundle,
	}))
	if err != nil {
		return fmt.Errorf("pushing config bundle: %w", err)
	}
	_, err = fmt.Fprintf(out, "pushed config bundle, version %s\n", resp.Msg.GetVersion())
	return err
}

// runShow calls GetAgentConfigInfo and renders the result. An empty version with
// no members is the unconfigured fleet — rendered as a clear message, not an
// error (the RPC returns an empty-but-valid response, never an error, for that).
func runShow(ctx context.Context, client compassv1connect.CompassServiceClient, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := client.GetAgentConfigInfo(ctx, connect.NewRequest(&compassv1.GetAgentConfigInfoRequest{}))
	if err != nil {
		return fmt.Errorf("fetching config info: %w", err)
	}
	msg := resp.Msg
	if msg.GetVersion() == "" && len(msg.GetSkills()) == 0 &&
		len(msg.GetExtensions()) == 0 && len(msg.GetMcpServers()) == 0 {
		_, err = fmt.Fprintln(out, "no config declared for this fleet")
		return err
	}
	return renderConfigInfo(out, msg)
}

// renderConfigInfo prints the version and each populated bucket: the multi-member
// name buckets (skills/extensions/mcp + rules/subagents) as count + indented
// names, and the singleton presence flags (settings, AGENTS.md, models.yml) as a
// clear present/absent line each.
func renderConfigInfo(out io.Writer, msg *compassv1.GetAgentConfigInfoResponse) error {
	if _, err := fmt.Fprintf(out, "version: %s\n", msg.GetVersion()); err != nil {
		return err
	}
	buckets := []struct {
		label string
		names []string
	}{
		{"skills", msg.GetSkills()},
		{"extensions", msg.GetExtensions()},
		{"mcp", msg.GetMcpServers()},
		{"rules", msg.GetRules()},
		{"subagents", msg.GetSubagents()},
	}
	for _, b := range buckets {
		if _, err := fmt.Fprintf(out, "%s (%d):\n", b.label, len(b.names)); err != nil {
			return err
		}
		for _, n := range b.names {
			if _, err := fmt.Fprintf(out, "  - %s\n", n); err != nil {
				return err
			}
		}
	}
	presence := []struct {
		label   string
		present bool
	}{
		{topDirSettings, msg.GetHasSettings()},
		{memberAgentsMD, msg.GetHasAgentsMd()},
		{memberModels, msg.GetHasModels()},
	}
	for _, p := range presence {
		state := "absent"
		if p.present {
			state = "present"
		}
		if _, err := fmt.Fprintf(out, "%s: %s\n", p.label, state); err != nil {
			return err
		}
	}
	return nil
}

// runDelete calls DeleteAgentConfig (idempotent) and confirms.
func runDelete(ctx context.Context, client compassv1connect.CompassServiceClient, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	if _, err := client.DeleteAgentConfig(ctx, connect.NewRequest(&compassv1.DeleteAgentConfigRequest{})); err != nil {
		return fmt.Errorf("deleting config bundle: %w", err)
	}
	_, err := fmt.Fprintln(out, "cleared fleet config bundle")
	return err
}
