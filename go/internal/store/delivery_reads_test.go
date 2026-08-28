//go:build pgtest

package store

// The delivery consumer's read side (RIG-1569 T3, design record D1): subscriber
// resolution with the home-channel disjunct, the author agent/human split, and
// the message-id -> channel / message reads the ack arm and settle gate use.
// These are properties only a real Postgres proves (the SQL disjunct, the JOIN
// scoping to agent members), so the file is pgtest-tagged, mirroring
// delivery_cursors_test.go.

import (
	"context"
	"errors"
	"testing"
)

// mustNamedChannelWith creates a plain channel owned by actor with the given
// extra members, for the subscriber-resolution cases.
func mustNamedChannelWith(t *testing.T, s *Store, actor AccountID, name string, members ...AccountID) ChannelID {
	t.Helper()
	ch, err := s.CreateChannel(context.Background(), actor, NewChannel{
		Name: name, Kind: ChannelKindChannel, MemberAccountIDs: members,
	})
	if err != nil {
		t.Fatalf("CreateChannel(%q): %v", name, err)
	}
	return ch.ID
}

// unsubscribeMember flips a member's channel_members row to subscribed=false
// directly, modeling the addOrUpdateMember DO UPDATE that can flip a home or
// mandatory-subscription row to subscribed=false — the exact state the D1/T4
// disjuncts must still deliver through.
func unsubscribeMember(t *testing.T, s *Store, ch ChannelID, acct AccountID) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		"UPDATE channel_members SET subscribed = false WHERE channel_id = $1 AND account_id = $2",
		string(ch), string(acct),
	); err != nil {
		t.Fatalf("unsubscribe member (%s,%s): %v", ch, acct, err)
	}
}

// Case: a MessagePosted on a subscribed channel resolves exactly the subscribed
// AGENT members, author excluded, and never a human member.
func TestSubscribedAgentsResolvesAgentsAuthorExcluded(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	author := mustAgent(t, s, owner.ID, "author")
	recip := mustAgent(t, s, owner.ID, "recip")
	// A channel owned by the human, with both agents as members (owner is pulled
	// in as a member on create), then both agents subscribed.
	ch := mustNamedChannelWith(t, s, owner.ID, "shared", author.ID, recip.ID)
	subscribeAgent(t, s, owner.ID, ch, author.ID)
	subscribeAgent(t, s, owner.ID, ch, recip.ID)

	agents, err := s.SubscribedAgents(ctx, ch, author.ID)
	if err != nil {
		t.Fatalf("SubscribedAgents: %v", err)
	}
	if len(agents) != 1 || agents[0] != recip.ID {
		t.Fatalf("SubscribedAgents = %v, want [%s] (recip only: author excluded, human excluded)", agents, recip.ID)
	}
}

// Case: an unsubscribed non-home member gets nothing (the D1 query returns only
// subscribed-or-home members).
func TestSubscribedAgentsExcludesUnsubscribed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	author := mustAgent(t, s, owner.ID, "author")
	unsub := mustAgent(t, s, owner.ID, "unsub")
	// unsub is a member (added on create) but never subscribed; its home channel
	// is a different channel, so this channel is not its home.
	ch := mustNamedChannelWith(t, s, owner.ID, "shared", author.ID, unsub.ID)
	subscribeAgent(t, s, owner.ID, ch, author.ID)

	agents, err := s.SubscribedAgents(ctx, ch, author.ID)
	if err != nil {
		t.Fatalf("SubscribedAgents: %v", err)
	}
	for _, a := range agents {
		if a == unsub.ID {
			t.Fatalf("SubscribedAgents included the unsubscribed non-home member %s", unsub.ID)
		}
	}
}

// Case: a home-channel row flipped subscribed=false STILL resolves as a
// recipient — the D1 disjunct (design.md:116-137, RT-2 repair). This is the
// load-bearing property a plain subscribed check would miss.
func TestSubscribedAgentsHomeChannelDisjunct(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	home := agent.Agent.HomeChannelID
	// The owner is a member of the agent's home channel (added at CreateAgent), so
	// it can post there. Flip the AGENT's own home row to subscribed=false and
	// assert it still delivers.
	unsubscribeMember(t, s, home, agent.ID)

	agents, err := s.SubscribedAgents(ctx, home, owner.ID)
	if err != nil {
		t.Fatalf("SubscribedAgents: %v", err)
	}
	found := false
	for _, a := range agents {
		if a == agent.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("SubscribedAgents(%s home) = %v, want it to include %s despite subscribed=false (D1 disjunct)", agent.ID, agents, agent.ID)
	}
}

// Case: IsAgentAccount distinguishes an agent account from a human account and
// an unknown id — the settle gate's author split.
func TestIsAgentAccount(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	if ok, err := s.IsAgentAccount(ctx, agent.ID); err != nil || !ok {
		t.Fatalf("IsAgentAccount(agent) = %v,%v, want true,nil", ok, err)
	}
	if ok, err := s.IsAgentAccount(ctx, owner.ID); err != nil || ok {
		t.Fatalf("IsAgentAccount(human) = %v,%v, want false,nil", ok, err)
	}
	if ok, err := s.IsAgentAccount(ctx, "nonexistent"); err != nil || ok {
		t.Fatalf("IsAgentAccount(unknown) = %v,%v, want false,nil", ok, err)
	}
}

// Case: MessageChannel resolves a message id to its channel (the ack arm's
// channel resolution), and an unknown id is ErrNotFound (a fail-closed no-op).
func TestMessageChannelResolution(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID
	id, _ := postAs(t, s, ch, owner.ID, "hi")

	got, err := s.MessageChannel(ctx, id)
	if err != nil || got != ch {
		t.Fatalf("MessageChannel(%s) = %v,%v, want %s,nil", id, got, err, ch)
	}
	if _, err := s.MessageChannel(ctx, "no-such-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MessageChannel(unknown) err = %v, want ErrNotFound", err)
	}
}

// Case: MessageByID reads a message's current blocks (the settle-gate re-read),
// and an unknown id is ErrNotFound.
func TestMessageByID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID
	id, _ := postAs(t, s, ch, owner.ID, "current body")

	m, err := s.MessageByID(ctx, id)
	if err != nil {
		t.Fatalf("MessageByID(%s): %v", id, err)
	}
	if string(m.ID) != id || messageChannel(t, ctx, s, m.ID) != ch {
		t.Fatalf("MessageByID = id=%s channel=%s, want id=%s channel=%s", m.ID, messageChannel(t, ctx, s, m.ID), id, ch)
	}
	if len(m.Blocks) != 1 || m.Blocks[0].Text == nil || *m.Blocks[0].Text != "current body" {
		t.Fatalf("MessageByID blocks = %+v, want one text block 'current body'", m.Blocks)
	}
	if _, err := s.MessageByID(ctx, "no-such-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MessageByID(unknown) err = %v, want ErrNotFound", err)
	}
}

// Case: ChannelAgentMembers returns exactly the non-author AGENT members,
// regardless of subscribe state, and never a human member or the author. This is
// the mention→steer routing set (design.md:526-527): membership, not subscription.
func TestChannelAgentMembersResolvesAllAgentMembersAuthorExcluded(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner") // a human member (pulled in on create)
	author := mustAgent(t, s, owner.ID, "author")
	a1 := mustAgent(t, s, owner.ID, "a1")
	a2 := mustAgent(t, s, owner.ID, "a2")
	// A channel with the author + two other agents as members. The human owner is
	// added as a member on create. Subscribe only ONE agent to prove subscribe
	// state is irrelevant to this set.
	ch := mustNamedChannelWith(t, s, owner.ID, "shared", author.ID, a1.ID, a2.ID)
	subscribeAgent(t, s, owner.ID, ch, a1.ID)

	members, err := s.ChannelAgentMembers(ctx, ch, author.ID)
	if err != nil {
		t.Fatalf("ChannelAgentMembers: %v", err)
	}
	// Expect exactly {a1, a2}: author excluded, human owner excluded (no
	// agent_accounts row), both agents present regardless of subscribed flag.
	want := map[AccountID]bool{a1.ID: true, a2.ID: true}
	if len(members) != len(want) {
		t.Fatalf("ChannelAgentMembers = %v, want the 2 non-author agent members %v", members, want)
	}
	for _, m := range members {
		if !want[m] {
			t.Fatalf("ChannelAgentMembers returned unexpected member %s (want only %v: author + human excluded)", m, want)
		}
		if m == author.ID || m == owner.ID {
			t.Fatalf("ChannelAgentMembers included an excluded account %s", m)
		}
	}
}

// Case: an agent member with subscribed=false (and not its home channel) is STILL
// returned by ChannelAgentMembers — the property that distinguishes it from
// SubscribedAgents (which filters subscribed-or-home). Uses unsubscribeMember to
// set subscribed=false and asserts the member is still present.
func TestChannelAgentMembersIncludesUnsubscribed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	author := mustAgent(t, s, owner.ID, "author")
	unsub := mustAgent(t, s, owner.ID, "unsub")
	// unsub is a member of this (non-home) channel; flip it explicitly to
	// subscribed=false so SubscribedAgents would exclude it.
	ch := mustNamedChannelWith(t, s, owner.ID, "shared", author.ID, unsub.ID)
	unsubscribeMember(t, s, ch, unsub.ID)

	// SubscribedAgents excludes it (the contrast that makes the property load-bearing).
	subs, err := s.SubscribedAgents(ctx, ch, author.ID)
	if err != nil {
		t.Fatalf("SubscribedAgents: %v", err)
	}
	for _, a := range subs {
		if a == unsub.ID {
			t.Fatalf("precondition failed: SubscribedAgents included the unsubscribed %s", unsub.ID)
		}
	}

	// ChannelAgentMembers still returns it: membership, not subscription.
	members, err := s.ChannelAgentMembers(ctx, ch, author.ID)
	if err != nil {
		t.Fatalf("ChannelAgentMembers: %v", err)
	}
	found := false
	for _, m := range members {
		if m == unsub.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("ChannelAgentMembers(%s) = %v, want it to include the unsubscribed member %s (membership, not subscription)", ch, members, unsub.ID)
	}
}

// TestSweepChannelsResolvesDisjunctSet pins the pin sweep's channel enumeration
// (design.md T7): SweepChannels returns exactly the D1 disjunct set — every
// channel the agent is subscribed to, PLUS its home channel, PLUS any
// mandatory_subscription channel it is a member of — mirroring the
// UndeliveredMessages/SubscribedAgents disjunct. The home channel is included
// EVEN WHEN its subscribed flag is flipped false (the RT-2 guarantee); a
// mandatory channel is included EVEN WHEN the member's subscribed flag is false
// (the T4 policy disjunct); and a channel the agent is not a member of is
// excluded.
func TestSweepChannelsResolvesDisjunctSet(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	home := agent.Agent.HomeChannelID

	// A second channel the agent is subscribed to.
	subbed := mustNamedChannelWith(t, s, owner.ID, "subbed", agent.ID)
	subscribeAgent(t, s, owner.ID, subbed, agent.ID)
	// A mandatory-subscription channel the agent is a MEMBER of but whose row is
	// NOT subscribed — the T4 policy disjunct must still sweep it. mustPolicyChannel
	// seeds it with MandatorySubscription; flip the agent's row subscribed=false so
	// only the mandatory disjunct (not cm.subscribed) can admit it.
	mand := mustPolicyChannel(t, s, owner.ID, "mandatory", ChannelPolicy{
		MandatorySubscription: true,
	}, agent.ID)
	unsubscribeMember(t, s, mand.ID, agent.ID)
	// A channel the agent is NOT a member of — must be excluded.
	mustNamedChannelWith(t, s, owner.ID, "foreign")
	// Flip the agent's home row subscribed=false: the home channel must still be
	// in the sweep set (RT-2 disjunct), independent of the flag.
	unsubscribeMember(t, s, home, agent.ID)

	channels, err := s.SweepChannels(ctx, agent.ID)
	if err != nil {
		t.Fatalf("SweepChannels: %v", err)
	}
	got := map[ChannelID]bool{}
	for _, ch := range channels {
		got[ch] = true
	}
	if !got[home] {
		t.Fatalf("SweepChannels = %v, want it to include the home channel %s despite subscribed=false (RT-2 disjunct)", channels, home)
	}
	if !got[subbed] {
		t.Fatalf("SweepChannels = %v, want it to include the subscribed channel %s", channels, subbed)
	}
	if !got[mand.ID] {
		t.Fatalf("SweepChannels = %v, want it to include the mandatory-subscription channel %s despite subscribed=false (T4 policy disjunct)", channels, mand.ID)
	}
	if len(channels) != 3 {
		t.Fatalf("SweepChannels = %v, want exactly [home, subbed, mandatory] (a non-member channel is excluded)", channels)
	}
}
