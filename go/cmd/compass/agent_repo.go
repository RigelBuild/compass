//go:build unix

package main

import (
	"context"
	"fmt"
	"io"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
)

func newAgentRepoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Add, remove and list agent workstream repositories",
	}
	cmd.AddCommand(newAgentRepoAddCmd(), newAgentRepoRemoveCmd(), newAgentRepoListCmd())
	return cmd
}

func newAgentRepoAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <agent> <org/name>",
		Short: "Grant an agent access to an exact repository",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialAgentRepositoryClient(cmd)
			if err != nil {
				return err
			}
			return runAgentRepoAdd(cmd.Context(), client, args[0], args[1], cmd.OutOrStdout())
		},
	}
}

func newAgentRepoRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <agent> <org/name>",
		Short: "Revoke an agent repository grant",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialAgentRepositoryClient(cmd)
			if err != nil {
				return err
			}
			return runAgentRepoRemove(cmd.Context(), client, args[0], args[1], cmd.OutOrStdout())
		},
	}
}

func newAgentRepoListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <agent>",
		Short: "List an agent's workstream repositories",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialAgentRepositoryClient(cmd)
			if err != nil {
				return err
			}
			return runAgentRepoList(cmd.Context(), client, args[0], cmd.OutOrStdout())
		},
	}
}

func runAgentRepoAdd(ctx context.Context, client compassv1connect.AgentRepositoryServiceClient, agent, repo string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := client.GrantAgentRepository(ctx, connect.NewRequest(&compassv1.GrantAgentRepositoryRequest{
		AgentHandle: agent, Repository: repo,
	}))
	if err != nil {
		return fmt.Errorf("adding agent repository: %w", err)
	}
	result := "already present"
	if resp.Msg.GetAdded() {
		result = "added"
	}
	_, err = fmt.Fprintf(out, "%s %s\n", result, repo)
	return err
}

func runAgentRepoRemove(ctx context.Context, client compassv1connect.AgentRepositoryServiceClient, agent, repo string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := client.RevokeAgentRepository(ctx, connect.NewRequest(&compassv1.RevokeAgentRepositoryRequest{
		AgentHandle: agent, Repository: repo,
	}))
	if err != nil {
		return fmt.Errorf("removing agent repository: %w", err)
	}
	result := "not present"
	if resp.Msg.GetRemoved() {
		result = "removed"
	}
	_, err = fmt.Fprintf(out, "%s %s\n", result, repo)
	return err
}

func runAgentRepoList(ctx context.Context, client compassv1connect.AgentRepositoryServiceClient, agent string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := client.ListAgentRepositories(ctx, connect.NewRequest(&compassv1.ListAgentRepositoriesRequest{
		AgentHandle: agent,
	}))
	if err != nil {
		return fmt.Errorf("listing agent repositories: %w", err)
	}
	for _, repo := range resp.Msg.GetRepositories() {
		if _, err := fmt.Fprintln(out, repo); err != nil {
			return err
		}
	}
	return nil
}
