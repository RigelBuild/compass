//go:build unix

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
)

// spawnTimeout bounds SpawnAgent, which has no server-side deadline and may pull
// the agent image before it starts the container.
const spawnTimeout = 5 * time.Minute

// agentSpawnArgs is the parsed `agent spawn` flag set.
type agentSpawnArgs struct {
	handle      string
	displayName string
	parent      string
	requestID   string
}

// agentSpawnClients holds the two services `agent spawn` drives: CommsService
// creates the account, CompassService resolves the caller and spawns.
type agentSpawnClients struct {
	comms   compassv1connect.CommsServiceClient
	compass compassv1connect.CompassServiceClient
}

// newAgentSpawnCmd builds `agent spawn`: create an agent account owned by the
// caller, then bring it online with SpawnAgent (Provision + Start).
func newAgentSpawnCmd() *cobra.Command {
	var args agentSpawnArgs
	cmd := &cobra.Command{
		Use:   "spawn --handle <handle>",
		Short: "Create an agent account and bring it online (CreateAgent, then SpawnAgent)",
		Long: "Create an agent owned by the caller and start its session. If the handle " +
			"already exists for the caller, the existing agent is spawned instead, so a " +
			"failed spawn can be rerun. Pass the --request-id a failed run printed to " +
			"rejoin it instead of provisioning a second container.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConn(cmd)
			if err != nil {
				return err
			}
			comms, err := newCommsClient(cfg)
			if err != nil {
				return err
			}
			compass, err := newClient(cfg)
			if err != nil {
				return err
			}
			return runAgentSpawn(cmd.Context(), agentSpawnClients{comms: comms, compass: compass}, args, cmd.OutOrStdout())
		},
	}
	f := cmd.Flags()
	f.StringVar(&args.handle, "handle", "", "Agent handle, unique within the caller's agents (required).")
	f.StringVar(&args.displayName, "display-name", "", "Display name (default: the handle).")
	f.StringVar(&args.parent, "parent", "", "Parent agent handle in the agent tree (default: a root agent).")
	f.StringVar(&args.requestID, "request-id", "",
		"Idempotency key for the spawn. A retry with the same key rejoins the first spawn "+
			"(default: a fresh random key, printed on failure).")
	return cmd
}

// runAgentSpawn runs CreateAgent then SpawnAgent. An AlreadyExists create falls
// through to spawning the caller's existing agent of that handle.
func runAgentSpawn(ctx context.Context, c agentSpawnClients, args agentSpawnArgs, out io.Writer) error {
	if args.handle == "" {
		return errors.New("--handle is required")
	}
	if args.displayName == "" {
		args.displayName = args.handle
	}
	if args.requestID == "" {
		args.requestID = newSpawnRequestID()
	}

	existed, err := createAgent(ctx, c.comms, args)
	if err != nil {
		return err
	}
	qualified, err := qualifiedAgentHandle(ctx, c, args.handle)
	if err != nil {
		return err
	}

	sctx, cancel := context.WithTimeout(ctx, spawnTimeout)
	defer cancel()
	resp, err := c.compass.SpawnAgent(sctx, connect.NewRequest(&compassv1.SpawnAgentRequest{
		AgentHandle:     qualified,
		ClientRequestId: args.requestID,
	}))
	if err != nil {
		return fmt.Errorf("spawning agent %s (retry with --request-id %s to rejoin this spawn): %w",
			qualified, args.requestID, err)
	}
	if existed {
		if _, err := fmt.Fprintf(out, "agent %s already exists; spawned it\n", qualified); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(out, "agent:     %s\nsession:   %s\ncontainer: %s\n",
		qualified, resp.Msg.GetSessionId(), resp.Msg.GetContainerName())
	return err
}

// createAgent creates the account and reports whether the handle already existed.
func createAgent(ctx context.Context, comms compassv1connect.CommsServiceClient, args agentSpawnArgs) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	_, err := comms.CreateAgent(ctx, connect.NewRequest(&compassv1.CreateAgentRequest{
		Handle:       args.handle,
		DisplayName:  args.displayName,
		ParentHandle: args.parent,
	}))
	if connect.CodeOf(err) == connect.CodeAlreadyExists {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("creating agent %q: %w", args.handle, err)
	}
	return false, nil
}

// qualifiedAgentHandle maps the caller's agent handle to the `owner/agent` form
// SpawnAgent takes. The owner is the caller's own account: CreateAgent creates
// the agent under the caller, so another owner's agent of the same handle is
// never chosen.
func qualifiedAgentHandle(ctx context.Context, c agentSpawnClients, handle string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	who, err := c.compass.WhoAmI(ctx, connect.NewRequest(&compassv1.WhoAmIRequest{}))
	if err != nil {
		return "", fmt.Errorf("resolving the caller: %w", err)
	}
	owner := who.Msg.GetAccountId()
	resp, err := c.comms.ListAccounts(ctx, connect.NewRequest(&compassv1.ListAccountsRequest{}))
	if err != nil {
		return "", fmt.Errorf("listing accounts: %w", err)
	}
	var ownerHandle string
	found := false
	for _, acc := range resp.Msg.GetAccounts() {
		if acc.GetId() == owner {
			ownerHandle = acc.GetHandle()
		}
		if acc.GetHandle() == handle && acc.GetAgent().GetOwnerUserId() == owner {
			found = true
		}
	}
	if !found || ownerHandle == "" {
		return "", fmt.Errorf("no agent %q owned by the caller is visible", handle)
	}
	return ownerHandle + "/" + handle, nil
}

// newSpawnRequestID returns a random idempotency key for one spawn run.
func newSpawnRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error (Go 1.24+).
	return "compass-cli-spawn-" + hex.EncodeToString(b[:])
}
