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
)

// newAgentCmd builds the agent noun: the agent-session inspection operator
// surface. It carries no logic of its own; each verb is a child that dials the
// Server and drives one CompassService RPC. It is distinct from the agent-config
// noun (fleet config bundles) — a separate Cobra command, no name clash.
func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Inspect agent sessions (status)",
	}
	cmd.AddCommand(newAgentStatusCmd())
	return cmd
}

// newAgentStatusCmd builds `agent status [--session <id>]`: GetAgentStatus and
// render each session's id and state. An empty --session returns every live
// session; a set --session returns just that one (queryable even when terminal,
// per the by-id contract at internal/server/service.go:343-358).
func newAgentStatusCmd() *cobra.Command {
	var session string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show live agent-session state (all sessions, or one with --session)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := dialClient(cmd)
			if err != nil {
				return err
			}
			return runAgentStatus(cmd.Context(), client, session, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&session, "session", "",
		"Restrict to one session id (default: every live session).")
	return cmd
}

// runAgentStatus calls GetAgentStatus with the resolved session filter and
// renders the returned statuses. An empty result renders a clear message, not an
// error — no live sessions (or an unknown id) is a valid answer, not a failure.
func runAgentStatus(ctx context.Context, client compassv1connect.CompassServiceClient, session string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := client.GetAgentStatus(ctx,
		connect.NewRequest(&compassv1.GetAgentStatusRequest{SessionId: session}))
	if err != nil {
		return fmt.Errorf("getting agent status: %w", err)
	}
	statuses := resp.Msg.GetStatuses()
	if len(statuses) == 0 {
		_, err = fmt.Fprintln(out, "no live agent sessions")
		return err
	}
	return renderAgentStatuses(out, statuses)
}

// renderAgentStatuses prints a session-id + state column for each status. The
// state renders as the short operator-facing token (the enum name minus the
// AGENT_SESSION_STATE_ prefix, lowercased), so a WORKING session reads "working".
func renderAgentStatuses(out io.Writer, statuses []*compassv1.AgentSessionStatus) error {
	if _, err := fmt.Fprintf(out, "%-40s %s\n", "SESSION", "STATE"); err != nil {
		return err
	}
	for _, s := range statuses {
		if _, err := fmt.Fprintf(out, "%-40s %s\n", s.GetSessionId(), stateLabel(s.GetState())); err != nil {
			return err
		}
	}
	return nil
}

// stateLabel renders an AgentSessionState as the short operator-facing token:
// the generated enum name with the AGENT_SESSION_STATE_ prefix stripped and
// lowercased (WORKING → "working"). An unknown value falls back to "unspecified".
func stateLabel(state compassv1.AgentSessionState) string {
	name, ok := compassv1.AgentSessionState_name[int32(state)]
	if !ok {
		return unspecifiedLabel
	}
	return strings.ToLower(strings.TrimPrefix(name, "AGENT_SESSION_STATE_"))
}
