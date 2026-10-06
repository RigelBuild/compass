//go:build pgtest

package store

import (
	"slices"
	"testing"
)

// subscribeTree toggles account's subscription on a TREE channel, acting as
// the account itself (any participant may toggle its own override).
func subscribeTree(t *testing.T, s *Store, account AccountID, channel ChannelID, subscribed bool) {
	t.Helper()
	u := MemberUpdate{AccountID: account, Subscribed: subscribed, Unsubscribe: !subscribed}
	if _, _, err := s.UpdateChannelMembers(t.Context(), account, channel, []MemberUpdate{u}, MemberUpdatesOptions{}); err != nil {
		t.Fatalf("UpdateChannelMembers(subscribe %v, %s): %v", subscribed, account, err)
	}
}

// deliverySites reports, for one agent and channel, the answer of each of
// the four sites that gate delivery.
type deliverySites struct {
	subscribed, sweep, undelivered, inSweep bool
}

func readDeliverySites(t *testing.T, s *Store, agent AccountID, channel ChannelID, author AccountID) deliverySites {
	t.Helper()
	subscribers, err := s.SubscribedAgents(t.Context(), channel, author)
	if err != nil {
		t.Fatalf("SubscribedAgents: %v", err)
	}
	sweep, err := s.SweepChannels(t.Context(), agent)
	if err != nil {
		t.Fatalf("SweepChannels: %v", err)
	}
	undelivered, err := s.UndeliveredMessages(t.Context(), agent)
	if err != nil {
		t.Fatalf("UndeliveredMessages: %v", err)
	}
	inSweep, err := s.InSweepSet(t.Context(), agent, channel)
	if err != nil {
		t.Fatalf("InSweepSet: %v", err)
	}
	return deliverySites{
		subscribed:  slices.Contains(subscribers, agent),
		sweep:       slices.Contains(sweep, channel),
		undelivered: len(undelivered[channel]) > 0,
		inSweep:     inSweep,
	}
}

func TestChannelTreeSubscriptionDrivesEveryDeliverySite(t *testing.T) {
	s := newTestStore(t)
	f := newTreeFixture(t, s)

	if got := readDeliverySites(t, s, f.leaf.ID, f.channel.ID, f.root.ID); got != (deliverySites{}) {
		t.Fatalf("unsubscribed derived participant sites = %+v; want none (COALESCE default)", got)
	}

	subscribeTree(t, s, f.leaf.ID, f.channel.ID, true)
	if _, err := appendAsParticipant(t, s, f.root.ID, f.channel.ID, "after subscribe"); err != nil {
		t.Fatalf("AppendMessage by anchor: %v", err)
	}
	want := deliverySites{subscribed: true, sweep: true, undelivered: true, inSweep: true}
	if got := readDeliverySites(t, s, f.leaf.ID, f.channel.ID, f.root.ID); got != want {
		t.Fatalf("subscribed derived participant sites = %+v; want %+v", got, want)
	}
	// The anchor and the middle agent never toggled, so the leaf's override
	// must not leak to them.
	if got := readDeliverySites(t, s, f.mid.ID, f.channel.ID, f.root.ID); got != (deliverySites{}) {
		t.Fatalf("untoggled mid agent sites = %+v; want none", got)
	}

	subscribeTree(t, s, f.leaf.ID, f.channel.ID, false)
	if got := readDeliverySites(t, s, f.leaf.ID, f.channel.ID, f.root.ID); got != (deliverySites{}) {
		t.Fatalf("unsubscribed-again participant sites = %+v; want none", got)
	}
}

func TestChannelTreeReparentOutMakesOverrideInert(t *testing.T) {
	s := newTestStore(t)
	f := newTreeFixture(t, s)
	subscribeTree(t, s, f.leaf.ID, f.channel.ID, true)

	if _, err := s.ReparentAgent(t.Context(), f.owner.ID, f.leaf.ID, f.sibling.ID); err != nil {
		t.Fatalf("ReparentAgent(leaf → sibling): %v", err)
	}
	if _, err := appendAsParticipant(t, s, f.root.ID, f.channel.ID, "after move"); err != nil {
		t.Fatalf("AppendMessage by anchor: %v", err)
	}
	if got := readDeliverySites(t, s, f.leaf.ID, f.channel.ID, f.root.ID); got != (deliverySites{}) {
		t.Fatalf("reparented-out agent sites = %+v; want none", got)
	}
	members, err := s.ChannelAgentMembers(t.Context(), f.channel.ID, f.root.ID)
	if err != nil {
		t.Fatalf("ChannelAgentMembers: %v", err)
	}
	if slices.Contains(members, f.leaf.ID) {
		t.Fatalf("ChannelAgentMembers = %v; want reparented leaf absent", members)
	}
	var overrides int
	if err := s.scopedPool().QueryRow(t.Context(),
		`SELECT count(*) FROM channel_subscriptions WHERE channel_id = $1 AND account_id = $2`,
		string(f.channel.ID), string(f.leaf.ID)).Scan(&overrides); err != nil {
		t.Fatalf("count overrides: %v", err)
	}
	if overrides != 1 {
		t.Fatalf("override rows after reparent = %d; want the row left in place", overrides)
	}
}

// ChannelAgentMembers backs both @handle resolution and @agents expansion;
// only the anchor-to-subtree descent can produce these rows.
func TestChannelTreeAgentMembersAreTheSubtree(t *testing.T) {
	s := newTestStore(t)
	f := newTreeFixture(t, s)

	members, err := s.ChannelAgentMembers(t.Context(), f.channel.ID, f.root.ID)
	if err != nil {
		t.Fatalf("ChannelAgentMembers: %v", err)
	}
	want := []AccountID{f.mid.ID, f.leaf.ID}
	slices.Sort(want)
	if !slices.Equal(members, want) {
		t.Fatalf("ChannelAgentMembers(author=root) = %v; want subtree %v without author, owner, or sibling", members, want)
	}
}

func TestChannelTreeMembershipWritesRefused(t *testing.T) {
	s := newTestStore(t)
	f := newTreeFixture(t, s)

	for name, u := range map[string]MemberUpdate{
		"add":    {AccountID: f.sibling.ID},
		"remove": {AccountID: f.leaf.ID, Remove: true},
	} {
		_, _, err := s.UpdateChannelMembers(t.Context(), f.owner.ID, f.channel.ID, []MemberUpdate{u}, MemberUpdatesOptions{})
		sentinelIs(t, err, ErrInvalidArgument, "TREE "+name)
	}
	_, _, err := s.UpdateChannelMembers(t.Context(), f.owner.ID, f.channel.ID,
		[]MemberUpdate{{AccountID: f.sibling.ID, Subscribed: true}}, MemberUpdatesOptions{})
	sentinelIs(t, err, ErrNotFound, "TREE subscribe of a non-participant")

	_, err = s.SetChannelPolicy(t.Context(), f.owner.ID, f.channel.ID, ChannelPolicy{MandatorySubscription: true})
	sentinelIs(t, err, ErrInvalidArgument, "TREE mandatory flip")
	_, err = s.SetChannelPolicy(t.Context(), f.owner.ID, f.channel.ID, ChannelPolicy{
		PostPolicy: ChannelPostPolicyOwnerOnly, OwnerAccountID: f.owner.ID,
	})
	sentinelIs(t, err, ErrInvalidArgument, "TREE OWNER_ONLY")
}
