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

// newPeerCmd manages the caller's own peering approvals; the token picks the user.
func newPeerCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "peer", Short: "Manage user peering approvals"}
	cmd.AddCommand(&cobra.Command{
		Use:   "approve <handle>",
		Short: "Approve a user as a peer",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialCommsClient(cmd)
			if err != nil {
				return err
			}
			return runPeerApprove(cmd.Context(), client, args[0], cmd.OutOrStdout())
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "revoke <handle>",
		Short: "Revoke a user's peer approval",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := dialCommsClient(cmd)
			if err != nil {
				return err
			}
			return runPeerRevoke(cmd.Context(), client, args[0], cmd.OutOrStdout())
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   listVerb,
		Short: "List peer requests and approvals",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := dialCommsClient(cmd)
			if err != nil {
				return err
			}
			return runPeerList(cmd.Context(), client, cmd.OutOrStdout())
		},
	})
	return cmd
}

func runPeerApprove(ctx context.Context, client compassv1connect.CommsServiceClient, handle string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := client.ApprovePeer(ctx, connect.NewRequest(&compassv1.ApprovePeerRequest{PeerHandle: handle}))
	if err != nil {
		return fmt.Errorf("approving peer %q: %w", handle, err)
	}
	return writePeerLine(out, resp.Msg.GetPeering())
}

func runPeerRevoke(ctx context.Context, client compassv1connect.CommsServiceClient, handle string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := client.RevokePeer(ctx, connect.NewRequest(&compassv1.RevokePeerRequest{PeerHandle: handle}))
	if err != nil {
		return fmt.Errorf("revoking peer %q: %w", handle, err)
	}
	message := "no approval for " + handle
	if resp.Msg.GetDeleted() {
		message = "revoked " + handle
	}
	_, err = fmt.Fprintln(out, message)
	return err
}

func runPeerList(ctx context.Context, client compassv1connect.CommsServiceClient, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := client.ListPeers(ctx, connect.NewRequest(&compassv1.ListPeersRequest{}))
	if err != nil {
		return fmt.Errorf("listing peers: %w", err)
	}
	for _, peer := range resp.Msg.GetPeerings() {
		if err := writePeerLine(out, peer); err != nil {
			return err
		}
	}
	return nil
}

func writePeerLine(out io.Writer, peer *compassv1.Peering) error {
	_, err := fmt.Fprintf(out, "%s\t%s\n", peer.GetHandle(), peerStateLabel(peer.GetState()))
	return err
}

func peerStateLabel(state compassv1.PeeringState) string {
	name, ok := compassv1.PeeringState_name[int32(state)]
	if !ok {
		return unspecifiedLabel
	}
	return strings.ToLower(strings.TrimPrefix(name, "PEERING_STATE_"))
}
