//go:build pgtest

package store

// D9 write-authorization refusals, enforced server-side in SQL on every write
// path (design.md:1101-1102, :1197): a caller may only mutate what it can see.
// Each test drives the refusal the gate now provides and pairs it with the
// happy path the gate must still admit — so a regression that drops the gate
// reddens the refusal, and one that over-refuses reddens the companion. These
// would all PASS against the pre-gate code on the refusal (the write went
// through), which is exactly why they are the regression the fix defends.

import (
	"context"
	"testing"
)

// TestAppendMessageNonMemberRefusedNoRow pins the PostMessage membership gate:
// an author who is not a member of the target channel is refused with
// ErrNotFound (the not-found/forbidden merge) and — critically — persists no
// row and fans out no event. Pre-gate the insert ran, leaking a private
// channel's history to a non-member and publishing a MessagePosted they should
// never have caused; the assertion on the row count is what catches a gate that
// refuses the return value but still writes.
func TestAppendMessageNonMemberRefusedNoRow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	member := mustUser(t, s, "member")
	outsider := mustUser(t, s, "outsider")
	ch := mustChannel(t, s, member.ID) // member is the sole founding member

	// A non-member author is refused, and nothing is written.
	_, _, err := s.AppendMessage(ctx, Message{AuthorAccountID: outsider.ID, Blocks: []MessageBlock{textBlock("intrusion")}}, string(ch.ID), TopicRef{Name: "general", Create: true}, "")
	sentinelIs(t, err, ErrNotFound, "non-member append")
	if n := messageCount(t, ctx, s, ch.ID); n != 0 {
		t.Fatalf("non-member append persisted %d rows, want 0 (refusal must not write)", n)
	}

	// The member author still succeeds — the gate admits the visible set.
	posted, _, err := s.AppendMessage(ctx, Message{AuthorAccountID: member.ID, Blocks: []MessageBlock{textBlock("legitimate")}}, string(ch.ID), TopicRef{Name: "general", Create: true}, "")
	if err != nil {
		t.Fatalf("member append: %v", err)
	}
	// The member's post is the only row, so a member's read sees exactly it and
	// nothing the outsider tried to write.
	msgs, err := s.ListMessages(ctx, ListMessagesQuery{Actor: member.ID, ChannelID: ch.ID, Page: Page{}})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != posted.ID {
		t.Fatalf("member read = %+v, want exactly the member's post %q", msgs, posted.ID)
	}
}

// TestUpdateChannelMembersNonMemberRefused pins the UpdateChannelMembers gate:
// a non-member actor cannot mutate a channel's membership — including the
// self-add escalation (adding itself to gain read access). It is refused with
// ErrNotFound and the membership is unchanged. Pre-gate the bare existence
// check let any actor mutate an existing channel, so a non-member could add
// itself and then read a private channel's history — the escalation this
// defends. The member actor still mutates successfully.
func TestUpdateChannelMembersNonMemberRefused(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	outsider := mustUser(t, s, "outsider")
	newcomer := mustUser(t, s, "newcomer")
	ch := mustChannel(t, s, owner.ID)

	// Self-add escalation by a non-member is refused.
	_, _, err := s.UpdateChannelMembers(ctx, outsider.ID, ch.ID, []MemberUpdate{
		{AccountID: outsider.ID},
	})
	sentinelIs(t, err, ErrNotFound, "non-member self-add")

	// The outsider gained no membership: the refusal did not write.
	after, err := s.getChannel(ctx, ch.ID)
	if err != nil {
		t.Fatalf("getChannel: %v", err)
	}
	if containsAccount(after.MemberAccountIDs, outsider.ID) {
		t.Fatalf("non-member added itself despite refusal: members = %v", after.MemberAccountIDs)
	}

	// The member (owner) still mutates: adding a newcomer succeeds.
	updated, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: newcomer.ID},
	})
	if err != nil {
		t.Fatalf("member UpdateChannelMembers: %v", err)
	}
	if !containsAccount(updated.MemberAccountIDs, newcomer.ID) {
		t.Fatalf("member add did not take: members = %v", updated.MemberAccountIDs)
	}
}

// TestCreateChannelGroupAuthz pins requireGroupCreateAuthz across its whole
// predicate: a channel created inside a group the actor may not create in is
// refused with ErrNotFound, while every authorized path succeeds — actor owns
// the group, the group is SHARED (visible to all), the actor is an agent whose
// owning user owns the group (Matt's ruling), and the ungrouped path has no
// parent to authorize against. Pre-gate CreateChannel took any GroupID, so the
// OWNER-not-owned case succeeded — the leak this refusal defends.
func TestCreateChannelGroupAuthz(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	stranger := mustUser(t, s, "stranger")
	agent := mustAgent(t, s, owner.ID, "agent") // owned by owner

	ownerGroup, err := s.CreateChannelGroup(ctx, owner.ID, NewChannelGroup{Name: "private", Visibility: VisibilityOwner})
	if err != nil {
		t.Fatalf("CreateChannelGroup(owner): %v", err)
	}
	sharedGroup, err := s.CreateChannelGroup(ctx, owner.ID, NewChannelGroup{Name: "shared", Visibility: VisibilityShared})
	if err != nil {
		t.Fatalf("CreateChannelGroup(shared): %v", err)
	}

	// A stranger cannot create in the owner-visibility group they neither own
	// nor may see — ErrNotFound (the not-found/forbidden merge).
	_, err = s.CreateChannel(ctx, stranger.ID, NewChannel{
		Name: "intrusion", Kind: ChannelKindChannel, GroupID: ownerGroup.ID,
	})
	sentinelIs(t, err, ErrNotFound, "stranger in owner-visibility group")

	// The owner creates in their own owner-visibility group — authorized.
	if _, err := s.CreateChannel(ctx, owner.ID, NewChannel{
		Name: "mine", Kind: ChannelKindChannel, GroupID: ownerGroup.ID,
	}); err != nil {
		t.Fatalf("owner in own group: %v", err)
	}

	// The owner's agent creates in the owner's group — an agent acts within its
	// owning user's space (Matt's ruling), so it is authorized transitively.
	if _, err := s.CreateChannel(ctx, agent.ID, NewChannel{
		Name: "agent-work", Kind: ChannelKindChannel, GroupID: ownerGroup.ID,
	}); err != nil {
		t.Fatalf("agent of owning user in owner's group: %v", err)
	}

	// A stranger creates in a SHARED group — visible to everyone, so authorized.
	if _, err := s.CreateChannel(ctx, stranger.ID, NewChannel{
		Name: "collab", Kind: ChannelKindChannel, GroupID: sharedGroup.ID,
	}); err != nil {
		t.Fatalf("stranger in shared group: %v", err)
	}

	// An ungrouped channel (empty GroupID) has no parent group to authorize
	// against — any actor may create one (they are its founding member).
	if _, err := s.CreateChannel(ctx, stranger.ID, NewChannel{
		Name: "solo", Kind: ChannelKindChannel,
	}); err != nil {
		t.Fatalf("ungrouped create: %v", err)
	}
}

// TestOpenAgentWorkspaceNonMemberRefused pins the OpenAgentWorkspace home-channel
// gate: the actor must be a member of the agent's home channel (fork f —
// workspace access is a projection of home-channel membership). A non-member is
// refused with ErrNotFound; the owner, seeded into the home channel at
// CreateAgent, succeeds. Pre-gate the open took an ungated insert path, so any
// actor could open any agent's workspace — the leak this refusal defends. (The
// unknown-agent case is TestOpenAgentWorkspaceUnknownAgentNotFound.)
func TestOpenAgentWorkspaceNonMemberRefused(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	outsider := mustUser(t, s, "outsider")
	agent := mustAgent(t, s, owner.ID, "agent")

	// A non-member of the agent's home channel is refused.
	_, err := s.OpenAgentWorkspace(ctx, outsider.ID, agent.ID)
	sentinelIs(t, err, ErrNotFound, "non-member workspace open")

	// The owner is a home-channel member (seeded at CreateAgent) — authorized.
	ws, err := s.OpenAgentWorkspace(ctx, owner.ID, agent.ID)
	if err != nil {
		t.Fatalf("home-channel member workspace open: %v", err)
	}
	if ws.AgentAccountID != agent.ID {
		t.Fatalf("opened workspace agent = %q, want %q", ws.AgentAccountID, agent.ID)
	}
}

// TestIsTopicChannelMember pins the topic-scoped stream-edge gate: a member of
// the topic's channel resolves true and a non-member resolves false, with the
// channel resolved THROUGH the topic (topics.channel_id) — the read-parity
// resolver the SubscribeComms fan-out uses to gate MessagePosted/MessageUpdated
// now that a wire message carries only its topic. An unknown topic resolves
// false (the not-found/forbidden merge extended to the stream). A mutation that
// dropped the channel_members join (matching any topic) would redden the
// non-member and unknown-topic cases.
func TestIsTopicChannelMember(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	member := mustUser(t, s, "member")
	outsider := mustUser(t, s, "outsider")
	ch := mustChannel(t, s, member.ID) // member is the sole founding member

	// Seed a message so a topic exists under the channel; its TopicID is the
	// topic the gate resolves through.
	posted, _, err := s.AppendMessage(ctx, Message{AuthorAccountID: member.ID, Blocks: []MessageBlock{textBlock("seed")}}, string(ch.ID), TopicRef{Name: "general", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage(seed): %v", err)
	}
	topicID := posted.TopicID
	if topicID == "" {
		t.Fatal("seeded message has no TopicID, want the get-or-created topic")
	}

	// The channel member resolves true.
	ok, err := s.IsTopicChannelMember(ctx, member.ID, topicID)
	if err != nil {
		t.Fatalf("IsTopicChannelMember(member): %v", err)
	}
	if !ok {
		t.Fatal("IsTopicChannelMember(member) = false, want true (a member of the topic's channel)")
	}

	// A non-member of the topic's channel resolves false.
	ok, err = s.IsTopicChannelMember(ctx, outsider.ID, topicID)
	if err != nil {
		t.Fatalf("IsTopicChannelMember(outsider): %v", err)
	}
	if ok {
		t.Fatal("IsTopicChannelMember(outsider) = true, want false (not a member of the topic's channel)")
	}

	// An unknown topic resolves false, not an error.
	ok, err = s.IsTopicChannelMember(ctx, member.ID, "topic-does-not-exist")
	if err != nil {
		t.Fatalf("IsTopicChannelMember(unknown topic): %v", err)
	}
	if ok {
		t.Fatal("IsTopicChannelMember(unknown topic) = true, want false")
	}
}
