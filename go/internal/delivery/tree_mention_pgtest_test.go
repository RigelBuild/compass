//go:build pgtest && unix

package delivery

import (
	"context"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// A TREE channel has no member rows, so mention routing only works if
// ChannelAgentMembers returns the anchor's subtree. The owed row is the durable
// effect of a mention routed to an offline agent.
func TestTreeChannelMentionsRouteToDerivedAgents(t *testing.T) {
	ctx := context.Background()
	s := openDeliveryStore(t)
	owner := mustOwner(t, ctx, s)
	root := mustAgentAcct(t, ctx, s, owner.ID, "root")
	mid, err := s.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "mid", DisplayName: "mid", ParentAgentID: root.ID})
	if err != nil {
		t.Fatalf("CreateAgent(mid): %v", err)
	}
	leaf, err := s.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "leaf", DisplayName: "leaf", ParentAgentID: mid.ID})
	if err != nil {
		t.Fatalf("CreateAgent(leaf): %v", err)
	}
	outsider := mustAgentAcct(t, ctx, s, owner.ID, "outsider")
	ch, err := s.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name: "tree-room", Kind: store.ChannelKindChannel,
		ParentAgentID: root.ID, MembershipMode: store.ChannelMembershipModeTree,
	})
	if err != nil {
		t.Fatalf("CreateChannel(TREE): %v", err)
	}

	handleMention := postThroughStore(t, ctx, s, ch.ID, root.ID, "@leaf please look")
	agentsMention := postThroughStore(t, ctx, s, ch.ID, root.ID, "@agents standup")

	c, _, _ := newPgConsumer(t, s)
	startConsumer(t, c)
	waitMarked(t, ctx, s, string(handleMention.ID))
	waitMarked(t, ctx, s, string(agentsMention.ID))

	// leaf: its @handle plus @agents; mid: @agents only; the author and an
	// agent outside the subtree get nothing.
	waitOwed(t, ctx, s, leaf.ID, 2)
	waitOwed(t, ctx, s, mid.ID, 1)
	for _, a := range []store.Account{root, outsider} {
		if n := owedTotal(t, ctx, s, a.ID); n != 0 {
			t.Fatalf("owed mentions for %s = %d; want 0", a.Handle, n)
		}
	}
}
