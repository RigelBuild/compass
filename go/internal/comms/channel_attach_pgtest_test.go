//go:build pgtest

package comms

import (
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

func TestPublishChannelChangedCarriesTreeAttachment(t *testing.T) {
	h := newStreamHarness(t)
	owner := mustUser(t, h.store, "tree-event-owner")
	anchor := mustAgent(t, h.store, owner.ID, "tree-event-anchor")
	channel, err := h.store.CreateChannel(t.Context(), owner.ID, store.NewChannel{
		Name: "tree-event", Kind: store.ChannelKindChannel,
		ParentAgentID: anchor.ID, MembershipMode: store.ChannelMembershipModeTree,
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	sub, err := h.bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("subscribe event bus: %v", err)
	}
	defer sub.Cancel()

	h.svc.publishChannelChanged(channel, nil)
	select {
	case event := <-sub.Live:
		changed := event.Payload.GetChannelChanged()
		if changed == nil {
			t.Fatalf("published payload = %T, want ChannelChanged", event.Payload.GetPayload())
		}
		got := changed.GetChannel()
		if got.GetParentAgentId() != string(anchor.ID) || got.GetMembershipMode() != compassv1.ChannelMembershipMode_CHANNEL_MEMBERSHIP_MODE_TREE {
			t.Fatalf("ChannelChanged channel parent=%q mode=%v, want parent=%q TREE", got.GetParentAgentId(), got.GetMembershipMode(), anchor.ID)
		}
	default:
		t.Fatal("publishChannelChanged emitted no event")
	}
	select {
	case event := <-sub.Live:
		t.Fatalf("publishChannelChanged emitted duplicate event %T", event.Payload.GetPayload())
	default:
	}
}
