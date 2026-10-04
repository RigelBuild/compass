//go:build pgtest

package comms

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

func TestApprovePeerHandlerAuthorizationAndTargetTypes(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "peering-owner")
	agent := mustAgent(t, st, owner.ID, "peering-agent")
	peer := mustUser(t, st, "peering-target")
	if _, err := st.EnsureSystemAccount(ctx); err != nil {
		t.Fatalf("EnsureSystemAccount: %v", err)
	}

	_, err := svc.ApprovePeer(WithActor(ctx, agent.ID), connect.NewRequest(&compassv1.ApprovePeerRequest{PeerHandle: peer.Handle}))
	connectCodeIs(t, err, connect.CodePermissionDenied, "agent approval")

	_, err = svc.ApprovePeer(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.ApprovePeerRequest{PeerHandle: owner.Handle}))
	connectCodeIs(t, err, connect.CodeInvalidArgument, "self approval")
	for _, handle := range []string{"peering-owner/peering-agent", store.SystemAccountHandle, "peering-missing"} {
		_, err := svc.ApprovePeer(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.ApprovePeerRequest{PeerHandle: handle}))
		connectCodeIs(t, err, connect.CodeNotFound, "approval target "+handle)
	}
}

func TestListPeersHandlerShowsThreeStates(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	a := mustUser(t, st, "peering-state-a")
	b := mustUser(t, st, "peering-state-b")
	c := mustUser(t, st, "peering-state-c")
	d := mustUser(t, st, "peering-state-d")

	for _, edge := range [][2]store.AccountID{{a.ID, b.ID}, {c.ID, a.ID}, {a.ID, d.ID}, {d.ID, a.ID}} {
		if _, err := st.ApprovePeer(ctx, edge[0], edge[1]); err != nil {
			t.Fatalf("ApprovePeer(%q, %q): %v", edge[0], edge[1], err)
		}
	}

	resp, err := svc.ListPeers(WithActor(ctx, a.ID), connect.NewRequest(&compassv1.ListPeersRequest{}))
	if err != nil {
		t.Fatalf("ListPeers: %v", err)
	}
	got := make(map[string]compassv1.PeeringState)
	for _, peer := range resp.Msg.GetPeerings() {
		got[peer.GetHandle()] = peer.GetState()
	}
	want := map[string]compassv1.PeeringState{
		b.Handle: compassv1.PeeringState_PEERING_STATE_PENDING_OUTGOING,
		c.Handle: compassv1.PeeringState_PEERING_STATE_PENDING_INCOMING,
		d.Handle: compassv1.PeeringState_PEERING_STATE_APPROVED,
	}
	if len(got) != len(want) {
		t.Fatalf("ListPeers returned %v, want exactly %v", got, want)
	}
	for handle, state := range want {
		if got[handle] != state {
			t.Errorf("state for %q = %v, want %v", handle, got[handle], state)
		}
	}
}

func TestRevokePeerHandlerReclaimedHandleDoesNotRevokeOldAccount(t *testing.T) {
	ctx := context.Background()
	s, dsn := newTestStoreDSN(t)
	a := mustUser(t, s, "peering-reclaim-a")
	b := mustUser(t, s, "peering-reclaim-b")
	for _, edge := range [][2]store.AccountID{{a.ID, b.ID}, {b.ID, a.ID}} {
		if _, err := s.ApprovePeer(ctx, edge[0], edge[1]); err != nil {
			t.Fatalf("ApprovePeer(%q, %q): %v", edge[0], edge[1], err)
		}
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test schema: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, "UPDATE account_handles SET handle = $2 WHERE account_id = $1", string(b.ID), "peering-reclaim-b-old"); err != nil {
		t.Fatalf("rename peer handle: %v", err)
	}
	if _, err := s.CreateUser(ctx, store.NewUser{Handle: "peering-reclaim-b", DisplayName: "Reclaimed"}); err != nil {
		t.Fatalf("reclaim peer handle: %v", err)
	}

	svc := NewComms(s, newBus(t), nil, a.ID)
	resp, err := svc.RevokePeer(WithActor(ctx, a.ID), connect.NewRequest(&compassv1.RevokePeerRequest{PeerHandle: "peering-reclaim-b"}))
	if err != nil {
		t.Fatalf("RevokePeer(reclaimed handle): %v", err)
	}
	if resp.Msg.GetDeleted() {
		t.Fatal("RevokePeer(reclaimed handle) reported deleted=true, want false")
	}
	peers, err := svc.ListPeers(WithActor(ctx, a.ID), connect.NewRequest(&compassv1.ListPeersRequest{}))
	if err != nil {
		t.Fatalf("ListPeers after reclaimed revoke: %v", err)
	}
	if len(peers.Msg.GetPeerings()) != 1 || peers.Msg.GetPeerings()[0].GetUserAccountId() != string(b.ID) || peers.Msg.GetPeerings()[0].GetState() != compassv1.PeeringState_PEERING_STATE_APPROVED {
		t.Fatalf("old account peering after reclaimed revoke = %+v, want original approved peer", peers.Msg.GetPeerings())
	}
}
