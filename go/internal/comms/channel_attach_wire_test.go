package comms

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// The channel-attach fields are additive: their numbers are the wire contract
// the server columns and older clients rely on, so a renumber must fail here.
func TestChannelAttachFieldNumbers(t *testing.T) {
	cases := []struct {
		msg   proto.Message
		field protoreflect.Name
		num   protoreflect.FieldNumber
	}{
		{&compassv1.Channel{}, "pinned_entries", 10},
		{&compassv1.Channel{}, "parent_agent_id", 11},
		{&compassv1.Channel{}, "membership_mode", 12},
		{&compassv1.CreateChannelRequest{}, "member_handles", 4},
		{&compassv1.CreateChannelRequest{}, "parent_agent_handle", 5},
		{&compassv1.CreateChannelRequest{}, "membership_mode", 6},
		{&compassv1.ReparentChannelRequest{}, "channel_id", 1},
		{&compassv1.ReparentChannelRequest{}, "new_parent_agent_handle", 2},
	}
	for _, c := range cases {
		fd := c.msg.ProtoReflect().Descriptor().Fields().ByName(c.field)
		if fd == nil {
			t.Fatalf("%s.%s: field missing", c.msg.ProtoReflect().Descriptor().Name(), c.field)
		}
		if fd.Number() != c.num {
			t.Errorf("%s.%s = %d, want %d", c.msg.ProtoReflect().Descriptor().Name(), c.field, fd.Number(), c.num)
		}
	}
}

// A channel written before the attach fields existed decodes as an EXPLICIT
// root channel; an attached TREE channel survives a marshal round trip.
func TestChannelAttachRoundTrip(t *testing.T) {
	legacy, err := proto.Marshal(&compassv1.Channel{Id: "ch-1", Name: "general"})
	if err != nil {
		t.Fatal(err)
	}
	var old compassv1.Channel
	if err := proto.Unmarshal(legacy, &old); err != nil {
		t.Fatal(err)
	}
	if old.GetParentAgentId() != "" || old.GetMembershipMode() != compassv1.ChannelMembershipMode_CHANNEL_MEMBERSHIP_MODE_EXPLICIT {
		t.Fatalf("legacy channel decoded as parent=%q mode=%v", old.GetParentAgentId(), old.GetMembershipMode())
	}

	in := &compassv1.Channel{
		Id:             "ch-2",
		ParentAgentId:  "acct-agent",
		MembershipMode: compassv1.ChannelMembershipMode_CHANNEL_MEMBERSHIP_MODE_TREE,
	}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out compassv1.Channel
	if err := proto.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(in, &out) {
		t.Fatalf("round trip: got %v, want %v", &out, in)
	}
}

func TestChannelToWireCarriesTreeAttachment(t *testing.T) {
	got := channelToWire(store.Channel{
		ParentAgentID:  "agent-anchor",
		MembershipMode: store.ChannelMembershipModeTree,
	})
	if got.GetParentAgentId() != "agent-anchor" || got.GetMembershipMode() != compassv1.ChannelMembershipMode_CHANNEL_MEMBERSHIP_MODE_TREE {
		t.Fatalf("channelToWire attachment = parent %q mode %v, want agent-anchor and TREE", got.GetParentAgentId(), got.GetMembershipMode())
	}
}
