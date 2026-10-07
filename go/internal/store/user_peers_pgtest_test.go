//go:build pgtest

package store

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

func TestApprovePeerIdempotentAndListStates(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := mustUser(t, s, "peer-a")
	b := mustUser(t, s, "peer-b")

	assertPair := func(user, peer AccountID, want PeeringState) {
		t.Helper()
		if got, err := s.PeeringWith(ctx, user, peer); err != nil || got != want {
			t.Fatalf("PeeringWith(%q, %q) = %v, %v; want %v", user, peer, got, err, want)
		}
	}
	if _, err := s.PeeringWith(ctx, a.ID, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PeeringWith before any approval error = %v, want ErrNotFound", err)
	}
	inserted, err := s.ApprovePeer(ctx, a.ID, b.ID)
	if err != nil || !inserted {
		t.Fatalf("ApprovePeer(a, b) = (%v, %v), want (true, nil)", inserted, err)
	}
	inserted, err = s.ApprovePeer(ctx, a.ID, b.ID)
	if err != nil || inserted {
		t.Fatalf("second ApprovePeer(a, b) = (%v, %v), want (false, nil)", inserted, err)
	}
	assertPair(a.ID, b.ID, PeeringPendingOutgoing)
	assertPair(b.ID, a.ID, PeeringPendingIncoming)

	assertPeerings := func(user AccountID, wantID AccountID, wantHandle string, wantState PeeringState) {
		t.Helper()
		peers, err := s.ListPeerings(ctx, user)
		if err != nil {
			t.Fatalf("ListPeerings(%q): %v", user, err)
		}
		if len(peers) != 1 || peers[0].PeerID != wantID || peers[0].Handle != wantHandle || peers[0].State != wantState {
			t.Fatalf("ListPeerings(%q) = %+v, want one %q/%q state %v", user, peers, wantID, wantHandle, wantState)
		}
	}
	assertPeerings(a.ID, b.ID, b.Handle, PeeringPendingOutgoing)
	assertPeerings(b.ID, a.ID, a.Handle, PeeringPendingIncoming)

	inserted, err = s.ApprovePeer(ctx, b.ID, a.ID)
	if err != nil || !inserted {
		t.Fatalf("ApprovePeer(b, a) = (%v, %v), want (true, nil)", inserted, err)
	}
	if inserted, err := s.ApprovePeer(ctx, b.ID, a.ID); err != nil || inserted {
		t.Fatalf("repeat ApprovePeer(b, a) = (%v, %v), want (false, nil)", inserted, err)
	}
	assertPair(a.ID, b.ID, PeeringApproved)
	assertPair(b.ID, a.ID, PeeringApproved)
	assertPeerings(a.ID, b.ID, b.Handle, PeeringApproved)
	assertPeerings(b.ID, a.ID, a.Handle, PeeringApproved)
}

func TestListPeeringsOrdersByHandle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	me := mustUser(t, s, "peer-order-me")
	for _, h := range []string{"peer-order-c", "peer-order-a", "peer-order-b"} {
		u := mustUser(t, s, h)
		if _, err := s.ApprovePeer(ctx, me.ID, u.ID); err != nil {
			t.Fatalf("ApprovePeer(%s): %v", h, err)
		}
	}
	want := []string{"peer-order-a", "peer-order-b", "peer-order-c"}
	peers, err := s.ListPeerings(ctx, me.ID)
	if err != nil {
		t.Fatalf("ListPeerings: %v", err)
	}
	got := make([]string, len(peers))
	for i, p := range peers {
		got[i] = p.Handle
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ListPeerings handles = %v, want %v", got, want)
	}
}

func TestRevokePeerTransitions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := mustUser(t, s, "revoke-a")
	b := mustUser(t, s, "revoke-b")
	for _, edge := range [][2]AccountID{{a.ID, b.ID}, {b.ID, a.ID}} {
		if _, err := s.ApprovePeer(ctx, edge[0], edge[1]); err != nil {
			t.Fatalf("ApprovePeer(%q, %q): %v", edge[0], edge[1], err)
		}
	}

	deleted, err := s.RevokePeer(ctx, a.ID, b.ID)
	if err != nil || !deleted {
		t.Fatalf("RevokePeer(a, b) = (%v, %v), want (true, nil)", deleted, err)
	}
	for user, wantState := range map[AccountID]PeeringState{
		a.ID: PeeringPendingIncoming,
		b.ID: PeeringPendingOutgoing,
	} {
		peers, err := s.ListPeerings(ctx, user)
		if err != nil {
			t.Fatalf("ListPeerings(%q): %v", user, err)
		}
		if len(peers) != 1 || peers[0].State != wantState {
			t.Errorf("ListPeerings(%q) = %+v, want one state %v", user, peers, wantState)
		}
	}
	deleted, err = s.RevokePeer(ctx, a.ID, b.ID)
	if err != nil || deleted {
		t.Fatalf("second RevokePeer(a, b) = (%v, %v), want (false, nil)", deleted, err)
	}
}

func TestApprovePeerRejectsSelfAndInvalidPeer(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	user := mustUser(t, s, "invalid-peer-user")
	agent := mustAgent(t, s, user.ID, "invalid-peer-agent")

	if _, err := s.ApprovePeer(ctx, user.ID, user.ID); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("ApprovePeer(self) error = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.ApprovePeer(ctx, user.ID, agent.ID); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("ApprovePeer(agent id) error = %v, want ErrInvalidArgument", err)
	}
}

func TestUserPeerRenameAndReclaim(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := mustUser(t, s, "peer-rename-a")
	b := mustUser(t, s, "peer-rename-b")
	if _, err := s.ApprovePeer(ctx, a.ID, b.ID); err != nil {
		t.Fatalf("ApprovePeer: %v", err)
	}
	if _, err := s.ApprovePeer(ctx, b.ID, a.ID); err != nil {
		t.Fatalf("ApprovePeer reverse: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		"UPDATE account_handles SET handle = $2 WHERE account_id = $1", string(b.ID), "peer-rename-b-new"); err != nil {
		t.Fatalf("rename peer: %v", err)
	}
	peers, err := s.ListPeerings(ctx, a.ID)
	if err != nil || len(peers) != 1 || peers[0].PeerID != b.ID || peers[0].Handle != "peer-rename-b-new" || peers[0].State != PeeringApproved {
		t.Fatalf("peer after rename = %+v, %v; want original account, renamed handle, approved", peers, err)
	}

	if _, err := s.pool.Exec(ctx,
		"UPDATE account_handles SET handle = $2 WHERE account_id = $1", string(b.ID), "peer-rename-b-old"); err != nil {
		t.Fatalf("free peer handle: %v", err)
	}
	reclaimed, err := s.CreateUser(ctx, NewUser{Handle: "peer-rename-b", DisplayName: "Reclaimed"})
	if err != nil {
		t.Fatalf("reclaim peer handle: %v", err)
	}
	peers, err = s.ListPeerings(ctx, a.ID)
	if err != nil || len(peers) != 1 || peers[0].PeerID != b.ID || peers[0].Handle != "peer-rename-b-old" || peers[0].State != PeeringApproved {
		t.Fatalf("peer after reclaim = %+v, %v; want original id approved", peers, err)
	}
	if inserted, err := s.ApprovePeer(ctx, a.ID, reclaimed.ID); err != nil || !inserted {
		t.Fatalf("ApprovePeer(reclaimed account) = (%v, %v), want fresh row", inserted, err)
	}
	peers, err = s.ListPeerings(ctx, a.ID)
	if err != nil || len(peers) != 2 {
		t.Fatalf("ListPeerings after fresh approval = %+v, %v; want two distinct accounts", peers, err)
	}
}

func TestUserPeerCrossTenantIsolation(t *testing.T) {
	ctxA := context.Background()
	s := newTestStore(t)
	tenantB := seedTenant(t, s, "peer-tenant-b")
	ctxB := WithTenant(context.Background(), tenantB)
	a := mustUser(t, s, "peer-tenant-a-user")
	b, err := s.CreateUser(ctxB, NewUser{Handle: "peer-tenant-b-user", DisplayName: "Tenant B"})
	if err != nil {
		t.Fatalf("CreateUser(B): %v", err)
	}

	if _, err := s.ApprovePeer(ctxA, a.ID, b.ID); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("cross-tenant ApprovePeer error = %v, want ErrInvalidArgument", err)
	}
	peers, err := s.ListPeerings(ctxA, a.ID)
	if err != nil || len(peers) != 0 {
		t.Fatalf("tenant A peerings = %+v, %v; want none", peers, err)
	}

	other := mustUser(t, s, "peer-tenant-a-peer")
	if _, err := s.ApprovePeer(ctxA, a.ID, other.ID); err != nil {
		t.Fatalf("same-tenant ApprovePeer: %v", err)
	}
	if peers, err := s.ListPeerings(ctxA, a.ID); err != nil || len(peers) != 1 {
		t.Fatalf("tenant A peerings = %+v, %v; want the same-tenant row", peers, err)
	}
	// Probe user_peers alone: ListPeerings also joins RLS-scoped handles, which
	// would hide the row even if user_peers itself leaked.
	pair := db.UserPeerPairParams{UserID: string(a.ID), PeerUserID: string(other.ID)}
	if row, err := s.q.UserPeerPair(ctxA, pair); err != nil || !row.Outgoing {
		t.Fatalf("tenant A row under tenant A = %+v, %v; want visible", row, err)
	}
	if row, err := s.q.UserPeerPair(ctxB, pair); err != nil || row.Outgoing {
		t.Fatalf("tenant A row under tenant B = %+v, %v; want hidden", row, err)
	}
}
