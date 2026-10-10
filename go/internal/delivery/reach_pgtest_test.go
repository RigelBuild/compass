//go:build pgtest && unix

package delivery

import (
	"context"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

func TestRevokedPeerPostDoesNotDispatchWakeOrRecordOwedMention(t *testing.T) {
	ctx := context.Background()
	s := openDeliveryStore(t)
	ownerA, err := s.CreateUser(ctx, store.NewUser{Handle: "reach-a", DisplayName: "reach-a"})
	if err != nil {
		t.Fatalf("CreateUser(A): %v", err)
	}
	ownerB, err := s.CreateUser(ctx, store.NewUser{Handle: "b", DisplayName: "b"})
	if err != nil {
		t.Fatalf("CreateUser(B): %v", err)
	}
	author, err := s.CreateAgent(ctx, ownerA.ID, store.NewAgent{Handle: "a", DisplayName: "a"})
	if err != nil {
		t.Fatalf("CreateAgent(A): %v", err)
	}
	recipient, err := s.CreateAgent(ctx, ownerB.ID, store.NewAgent{Handle: "x", DisplayName: "x"})
	if err != nil {
		t.Fatalf("CreateAgent(B): %v", err)
	}
	channel, err := s.CreateChannel(ctx, ownerA.ID, store.NewChannel{
		Name: "reach-room", Kind: store.ChannelKindChannel,
		MemberAccountIDs: []store.AccountID{author.ID, recipient.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	for _, agent := range []store.AccountID{author.ID, recipient.ID} {
		if _, _, err := s.UpdateChannelMembers(ctx, ownerA.ID, channel.ID,
			[]store.MemberUpdate{{AccountID: agent, Subscribed: true}}, store.MemberUpdatesOptions{}); err != nil {
			t.Fatalf("subscribe agent %s: %v", agent, err)
		}
	}
	for _, edge := range [][2]store.AccountID{{ownerA.ID, ownerB.ID}, {ownerB.ID, ownerA.ID}} {
		if _, err := s.ApprovePeer(ctx, edge[0], edge[1]); err != nil {
			t.Fatalf("ApprovePeer(%s, %s): %v", edge[0], edge[1], err)
		}
	}
	if _, err := s.RevokePeer(ctx, ownerA.ID, ownerB.ID); err != nil {
		t.Fatalf("RevokePeer(A, B): %v", err)
	}

	c, disp, _ := newPgConsumer(t, s)
	waker := newFakeWaker()
	c.SetAgentWaker(waker)
	startConsumer(t, c)
	fab := fakeFabricOf(c)
	fab.waitSubscribed(t)

	msg := postThroughStore(t, ctx, s, channel.ID, author.ID, "@b/x after revoke")
	publishRef(t, ctx, c, s.EffectiveTenant(ctx), string(msg.ID))
	fab.waitAcked(t, string(msg.ID))

	if got := disp.snapshot(); len(got) != 0 {
		t.Fatalf("dispatches after revoked-peer post = %+v, want none", got)
	}
	if got := waker.count(recipient.ID); got != 0 {
		t.Fatalf("wakes for revoked peer agent = %d, want 0", got)
	}
	if got := owedTotal(t, ctx, s, recipient.ID); got != 0 {
		t.Fatalf("owed mentions for revoked peer agent = %d, want 0", got)
	}
}
