package store

import (
	"context"
	"fmt"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// PeeringState is the caller's view of one peering, derived from which
// directed approval rows exist.
type PeeringState int

// Peering states: a single directed approval is pending; approvals in both
// directions are approved.
const (
	PeeringPendingOutgoing PeeringState = iota + 1
	PeeringPendingIncoming
	PeeringApproved
)

// Peering is one user the caller has an approval with, in either direction.
type Peering struct {
	PeerID AccountID
	Handle string
	State  PeeringState
}

// ApprovePeer records user's approval of peer and returns the resulting state;
// inserted is false when the approval already existed. A non-user, unknown, or
// other-tenant peer fails its composite foreign key and is ErrInvalidArgument.
func (s *Store) ApprovePeer(ctx context.Context, user, peer AccountID) (state PeeringState, inserted bool, err error) {
	if user == peer {
		return 0, false, fmt.Errorf("%w: a user cannot peer with itself", ErrInvalidArgument)
	}
	rows, err := s.q.InsertUserPeer(ctx, db.InsertUserPeerParams{UserID: string(user), PeerUserID: string(peer)})
	if err != nil {
		if pgErrIs(err, pgForeignKeyViolation) {
			return 0, false, fmt.Errorf("%w: peer %q is not a user in this tenant", ErrInvalidArgument, peer)
		}
		return 0, false, fmt.Errorf("store: approve peer: %w", err)
	}
	// The caller's own row was just written, so only the reverse row decides the state.
	reverse, err := s.q.UserPeerExists(ctx, db.UserPeerExistsParams{UserID: string(peer), PeerUserID: string(user)})
	if err != nil {
		return 0, false, fmt.Errorf("store: approve peer: %w", err)
	}
	state = PeeringPendingOutgoing
	if reverse {
		state = PeeringApproved
	}
	return state, rows != 0, nil
}

// RevokePeer deletes user's approval of peer; deleted reports whether one existed.
func (s *Store) RevokePeer(ctx context.Context, user, peer AccountID) (bool, error) {
	if user == peer {
		return false, fmt.Errorf("%w: a user cannot peer with itself", ErrInvalidArgument)
	}
	rows, err := s.q.DeleteUserPeer(ctx, db.DeleteUserPeerParams{UserID: string(user), PeerUserID: string(peer)})
	if err != nil {
		return false, fmt.Errorf("store: revoke peer: %w", err)
	}
	return rows != 0, nil
}

// ListPeerings returns every user with an approval to or from user, by handle.
func (s *Store) ListPeerings(ctx context.Context, user AccountID) ([]Peering, error) {
	rows, err := s.q.ListUserPeerings(ctx, string(user))
	if err != nil {
		return nil, fmt.Errorf("store: list peerings: %w", err)
	}
	peers := make([]Peering, 0, len(rows))
	for _, row := range rows {
		state := PeeringPendingIncoming
		switch {
		case row.Outgoing && row.Incoming:
			state = PeeringApproved
		case row.Outgoing:
			state = PeeringPendingOutgoing
		}
		peers = append(peers, Peering{PeerID: AccountID(row.PeerID), Handle: row.Handle, State: state})
	}
	return peers, nil
}
