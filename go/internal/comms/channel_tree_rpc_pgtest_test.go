//go:build pgtest

package comms

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

func textPost(channelID, text string) *compassv1.PostMessageRequest {
	return &compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: channelID},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
		CreateTopic: true,
		Blocks:      []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: text}}},
	}
}

func TestCreateChannelUnderAgentEmitsAttachedChannel(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()
	owner := mustUser(t, h.store, "tree-owner")
	anchor := mustAgent(t, h.store, owner.ID, "anchor")
	leaf := mustChildAgent(t, h.store, owner.ID, "leaf", anchor.ID)
	outsider := mustUser(t, h.store, "tree-outsider")

	events := firstEventAfterBoundary(t, h, owner.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})
	created, err := h.svc.CreateChannel(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name: "tree-room", Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
		ParentAgentHandle: "anchor", MembershipMode: compassv1.ChannelMembershipMode_CHANNEL_MEMBERSHIP_MODE_TREE,
	}))
	if err != nil {
		t.Fatalf("CreateChannel(TREE under anchor): %v", err)
	}
	wire := created.Msg.GetChannel()
	if wire.GetParentAgentId() != string(anchor.ID) || wire.GetMembershipMode() != compassv1.ChannelMembershipMode_CHANNEL_MEMBERSHIP_MODE_TREE {
		t.Fatalf("CreateChannel response parent=%q mode=%v; want %q TREE", wire.GetParentAgentId(), wire.GetMembershipMode(), anchor.ID)
	}
	changed := awaitFirst(t, events).GetChannelChanged()
	if changed.GetChannel().GetId() != wire.GetId() || changed.GetChannel().GetParentAgentId() != string(anchor.ID) {
		t.Fatalf("ChannelChanged = %v; want the attached channel %q under %q", changed, wire.GetId(), anchor.ID)
	}

	if _, err := h.svc.PostMessage(WithActor(ctx, leaf.ID), connect.NewRequest(textPost(wire.GetId(), "from the subtree"))); err != nil {
		t.Fatalf("PostMessage by subtree agent: %v", err)
	}
	_, err = h.svc.PostMessage(WithActor(ctx, outsider.ID), connect.NewRequest(textPost(wire.GetId(), "from outside")))
	connectCodeIs(t, err, connect.CodeNotFound, "PostMessage by non-participant")
}

func TestCreateChannelRejectsUnknownMembershipModeAndForeignAnchor(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "mode-owner")
	mustAgent(t, st, owner.ID, "anchor")
	other := mustUser(t, st, "mode-other")
	mustAgent(t, st, other.ID, "foreign")

	_, err := svc.CreateChannel(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name: "bad-mode", Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
		ParentAgentHandle: "anchor", MembershipMode: compassv1.ChannelMembershipMode(7),
	}))
	connectCodeIs(t, err, connect.CodeInvalidArgument, "unknown membership mode")

	_, err = svc.CreateChannel(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name: "foreign-anchor", Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
		ParentAgentHandle: "mode-other/foreign", MembershipMode: compassv1.ChannelMembershipMode_CHANNEL_MEMBERSHIP_MODE_TREE,
	}))
	connectNotFoundFor(t, err, "mode-other/foreign", "foreign anchor")
}

func TestReparentChannelEmitsOneChannelChangedToAdmittedAccounts(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()
	owner := mustUser(t, h.store, "move-owner")
	from := mustAgent(t, h.store, owner.ID, "from")
	to := mustAgent(t, h.store, owner.ID, "to")
	outsider := mustUser(t, h.store, "move-outsider")
	channel, err := h.store.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name: "moving", Kind: store.ChannelKindChannel,
		ParentAgentID: from.ID, MembershipMode: store.ChannelMembershipModeTree,
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	ownerEvents := firstEventAfterBoundary(t, h, owner.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})
	outsiderEvents := firstEventAfterBoundary(t, h, outsider.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})
	sub, err := h.bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("subscribe event bus: %v", err)
	}
	defer sub.Cancel()

	resp, err := h.svc.ReparentChannel(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.ReparentChannelRequest{
		ChannelId: string(channel.ID), NewParentAgentHandle: "to",
	}))
	if err != nil {
		t.Fatalf("ReparentChannel: %v", err)
	}
	if got := resp.Msg.GetChannel().GetParentAgentId(); got != string(to.ID) {
		t.Fatalf("ReparentChannel response parent = %q; want %q", got, to.ID)
	}
	changed := awaitFirst(t, ownerEvents).GetChannelChanged()
	if changed.GetChannel().GetId() != string(channel.ID) || changed.GetChannel().GetParentAgentId() != string(to.ID) {
		t.Fatalf("owner ChannelChanged = %v; want %q under %q", changed, channel.ID, to.ID)
	}

	// The outsider's own create is a marker: arriving first proves the move
	// event was filtered rather than merely slow.
	marker, err := h.svc.CreateChannel(WithActor(ctx, outsider.ID), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name: "outsider-marker", Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
	}))
	if err != nil {
		t.Fatalf("CreateChannel(marker): %v", err)
	}
	if got := awaitFirst(t, outsiderEvents).GetChannelChanged().GetChannel().GetId(); got != marker.Msg.GetChannel().GetId() {
		t.Fatalf("outsider first event channel = %q; want its marker %q, not the moved channel", got, marker.Msg.GetChannel().GetId())
	}

	var moves int
	for range 2 {
		event := <-sub.Live
		if event.Payload.GetChannelChanged().GetChannel().GetId() == string(channel.ID) {
			moves++
		}
	}
	if moves != 1 {
		t.Fatalf("ChannelChanged events for the moved channel = %d; want exactly one", moves)
	}
}

func TestReparentChannelNonParticipantIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "np-owner")
	anchor := mustAgent(t, st, owner.ID, "anchor")
	mustAgent(t, st, owner.ID, "dest")
	outsider := mustUser(t, st, "np-outsider")
	channel, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name: "private", Kind: store.ChannelKindChannel,
		ParentAgentID: anchor.ID, MembershipMode: store.ChannelMembershipModeTree,
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	_, err = svc.ReparentChannel(WithActor(ctx, outsider.ID), connect.NewRequest(&compassv1.ReparentChannelRequest{
		ChannelId: string(channel.ID),
	}))
	connectCodeIs(t, err, connect.CodeNotFound, "non-participant ReparentChannel")
}
