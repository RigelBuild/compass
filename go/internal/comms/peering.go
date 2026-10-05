package comms

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

var (
	errPeerUserRequired         = errors.New("peer management requires a user account")
	errPeerSelf                 = errors.New("a user cannot peer with itself")
	errPeerRevokedDuringApprove = errors.New("approval was revoked while it was being recorded; retry")
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
	if _, err := c.store.ApprovePeer(ctx, actor, peer.ID); err != nil {
		return nil, edgeError(err)
	}
	state, err := c.store.PeeringWith(ctx, actor, peer.ID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && state == store.PeeringPendingIncoming) {
		// Only the caller can delete its own row, so it revoked during this call.
		return nil, connect.NewError(connect.CodeAborted, errPeerRevokedDuringApprove)
	}
	if err != nil {
		return nil, edgeError(err)
	}
	p := store.Peering{PeerID: peer.ID, Handle: peer.Handle, State: state}
	return connect.NewResponse(&compassv1.ApprovePeerResponse{Peering: peeringToWire(p)}), nil
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
	deleted, err := c.store.RevokePeer(ctx, actor, peer.ID)
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
func (c *Comms) resolvePeerPair(ctx context.Context, handle string) (store.AccountID, store.Account, error) {
	actor, err := c.requireUserActor(ctx)
	if err != nil {
		return "", store.Account{}, err
	}
	peer, err := c.store.UserByHandle(ctx, handle)
	if err != nil {
		return "", store.Account{}, edgeError(notFoundHandle(err, handle))
	}
	if peer.System != nil || peer.User == nil {
		return "", store.Account{}, edgeError(notFoundHandle(store.ErrNotFound, handle))
	}
	if peer.ID == actor {
		return "", store.Account{}, connect.NewError(connect.CodeInvalidArgument, errPeerSelf)
	}
	return actor, peer, nil
}
