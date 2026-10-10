package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

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

// ApprovePeer records user's approval of peer; inserted is false when it
// already existed. A non-user, unknown, or other-tenant peer fails its
// composite foreign key and is ErrInvalidArgument.
func (s *Store) ApprovePeer(ctx context.Context, user, peer AccountID) (bool, error) {
	if user == peer {
		return false, fmt.Errorf("%w: a user cannot peer with itself", ErrInvalidArgument)
	}
	rows, err := s.q.InsertUserPeer(ctx, db.InsertUserPeerParams{UserID: string(user), PeerUserID: string(peer)})
	if err != nil {
		if pgErrIs(err, pgForeignKeyViolation) {
			return false, fmt.Errorf("%w: peer %q is not a user in this tenant", ErrInvalidArgument, peer)
		}
		return false, fmt.Errorf("store: approve peer: %w", err)
	}
	return rows != 0, nil
}

// PeeringWith returns user's peering state with peer, read from both directed
// rows in one statement; ErrNotFound when neither row exists.
func (s *Store) PeeringWith(ctx context.Context, user, peer AccountID) (PeeringState, error) {
	row, err := s.q.UserPeerPair(ctx, db.UserPeerPairParams{UserID: string(user), PeerUserID: string(peer)})
	if err != nil {
		return 0, fmt.Errorf("store: peering with: %w", err)
	}
	return peeringState(row.Outgoing, row.Incoming)
}

func peeringState(outgoing, incoming bool) (PeeringState, error) {
	switch {
	case outgoing && incoming:
		return PeeringApproved, nil
	case outgoing:
		return PeeringPendingOutgoing, nil
	case incoming:
		return PeeringPendingIncoming, nil
	default:
		return 0, ErrNotFound
	}
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
		state, err := peeringState(row.Outgoing, row.Incoming)
		if err != nil {
			return nil, fmt.Errorf("store: list peerings: %w", err)
		}
		peers = append(peers, Peering{PeerID: AccountID(row.PeerID), Handle: row.Handle, State: state})
	}
	return peers, nil
}

// OwnersPeered reports whether both users have approved each other.
func (s *Store) OwnersPeered(ctx context.Context, a, b AccountID) (bool, error) {
	peered, err := s.q.OwnersPeered(ctx, db.OwnersPeeredParams{UserID: string(a), PeerUserID: string(b)})
	if err != nil {
		return false, fmt.Errorf("store: check peering: %w", err)
	}
	return peered, nil
}

// OwnersPeeredTx locks both approvals against revoke until tx ends.
func (s *Store) OwnersPeeredTx(ctx context.Context, tx pgx.Tx, a, b AccountID) (bool, error) {
	rows, err := db.New(tx).OwnersPeeredRowsForShare(ctx, db.OwnersPeeredRowsForShareParams{
		UserID:     string(a),
		PeerUserID: string(b),
	})
	if err != nil {
		return false, fmt.Errorf("store: lock peering: %w", err)
	}
	return len(rows) == 2, nil
}
