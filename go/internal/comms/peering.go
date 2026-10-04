package comms

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

var (
	errPeerUserRequired           = errors.New("peer management requires a user account")
	errPeerSelf                   = errors.New("a user cannot peer with itself")
	errPeeringMissingAfterApprove = errors.New("approved peering missing from list")
)

// ApprovePeer records the calling user's approval of another user.
func (c *Comms) ApprovePeer(
	ctx context.Context,
	req *connect.Request[compassv1.ApprovePeerRequest],
) (*connect.Response[compassv1.ApprovePeerResponse], error) {
	actor, peer, err := c.resolvePeerPair(ctx, req.Msg.GetPeerHandle())
	if err != nil {
		return nil, err
	}
	if _, err := c.store.ApprovePeer(ctx, actor, peer); err != nil {
		return nil, edgeError(err)
	}
	peers, err := c.store.ListPeerings(ctx, actor)
	if err != nil {
		return nil, edgeError(err)
	}
	for _, p := range peers {
		if p.PeerID == peer {
			return connect.NewResponse(&compassv1.ApprovePeerResponse{Peering: peeringToWire(p)}), nil
		}
	}
	return nil, connect.NewError(connect.CodeInternal, errPeeringMissingAfterApprove)
}

// RevokePeer withdraws the calling user's approval; deleted is false when none
// existed, so a revoke by a reclaimed handle is visible rather than silent.
func (c *Comms) RevokePeer(
	ctx context.Context,
	req *connect.Request[compassv1.RevokePeerRequest],
) (*connect.Response[compassv1.RevokePeerResponse], error) {
	actor, peer, err := c.resolvePeerPair(ctx, req.Msg.GetPeerHandle())
	if err != nil {
		return nil, err
	}
	deleted, err := c.store.RevokePeer(ctx, actor, peer)
	if err != nil {
		return nil, edgeError(err)
	}
	return connect.NewResponse(&compassv1.RevokePeerResponse{Deleted: deleted}), nil
}

// ListPeers lists the calling user's peerings in every state.
func (c *Comms) ListPeers(
	ctx context.Context,
	_ *connect.Request[compassv1.ListPeersRequest],
) (*connect.Response[compassv1.ListPeersResponse], error) {
	actor, err := c.requireUserActor(ctx)
	if err != nil {
		return nil, err
	}
	peers, err := c.store.ListPeerings(ctx, actor)
	if err != nil {
		return nil, edgeError(err)
	}
	out := make([]*compassv1.Peering, len(peers))
	for i, peer := range peers {
		out[i] = peeringToWire(peer)
	}
	return connect.NewResponse(&compassv1.ListPeersResponse{Peerings: out}), nil
}

// requireUserActor returns the caller when it is a user; agents do not manage peering.
func (c *Comms) requireUserActor(ctx context.Context) (store.AccountID, error) {
	actor := c.actorFromContext(ctx)
	acc, err := c.store.GetAccount(ctx, actor)
	if err != nil {
		return "", edgeError(err)
	}
	if acc.User == nil {
		return "", connect.NewError(connect.CodePermissionDenied, errPeerUserRequired)
	}
	return actor, nil
}

// resolvePeerPair resolves the user caller and the named peer user. Agent,
// system, and unknown handles share one not-found so none is distinguishable.
func (c *Comms) resolvePeerPair(ctx context.Context, handle string) (store.AccountID, store.AccountID, error) {
	actor, err := c.requireUserActor(ctx)
	if err != nil {
		return "", "", err
	}
	peer, err := c.store.UserByHandle(ctx, handle)
	if err != nil {
		return "", "", edgeError(notFoundHandle(err, handle))
	}
	if peer.System != nil || peer.User == nil {
		return "", "", edgeError(notFoundHandle(store.ErrNotFound, handle))
	}
	if peer.ID == actor {
		return "", "", connect.NewError(connect.CodeInvalidArgument, errPeerSelf)
	}
	return actor, peer.ID, nil
}
