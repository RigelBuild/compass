//go:build pgtest

package comms

// The SubscribeComms per-event D9 visibility filter (subscribe.go forwardComms +
// visibleToActor), driven end to end through the real connect server-stream. The
// shared bus fans every event to every subscriber, so the ONLY thing keeping a
// non-member from seeing a private channel's traffic is this filter — these are
// the leak regressions it defends. Each case is per-variant, because the filter
// is per-variant: MessagePosted/MessageUpdated gate on bare membership,
// ChannelChanged on full channel visibility (member OR SHARED-grouped) plus the
// removed-member final event, AccountChanged/ChannelGroupChanged on their
// directory read predicate. A uniform membership filter would pass the leak
// tests but fail the SHARED-visibility ones, and vice-versa.
//
// Determinism without sleeps: every event is published BEFORE any subscriber
// opens, then a globally-visible "canary" user is created LAST. A subscriber
// opened at since_seq=0 replays the whole ring under the same per-event filter
// the live tail uses (forwardComms filters both loops identically), delivering —
// in seq order, exactly once — precisely the subset that subscriber is entitled
// to, terminated by the canary. So "collect until the canary" yields that
// subscriber's complete entitled set with no wall-clock wait: a leaked event
// would arrive before the canary, and its absence is a real proof, not a race.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// mkCanary creates a fresh human account THROUGH the handler (so it emits an
// AccountChanged) and returns its id. A human account is visible to every
// subscriber (AccountVisibleTo: all users are listable), so its AccountChanged
// is a universal high-water mark for the replay drain below.
func mkCanary(t *testing.T, h streamHarness, handle string) string {
	t.Helper()
	resp, err := h.svc.CreateUser(context.Background(), connect.NewRequest(&compassv1.CreateUserRequest{
		Handle: handle, DisplayName: handle,
	}))
	if err != nil {
		t.Fatalf("create canary %q: %v", handle, err)
	}
	return resp.Msg.GetAccount().GetId()
}

// drainReplayAsActor opens a SubscribeComms stream AS actor (over the
// withActorHeader middleware) at since_seq=0 and returns every event delivered
// up to — and excluding — the sentinel AccountChanged for canaryID. Because the
// canary is published last and is globally visible, in-order per-subscriber
// delivery guarantees every event the actor was entitled to (and that was
// published before the canary) has arrived by the time the canary does. A
// deadline guards a wedged handler; it is never a synchronization device.
func drainReplayAsActor(t *testing.T, h streamHarness, actor store.AccountID, canaryID string) []*compassv1.SubscribeCommsResponse {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req := connect.NewRequest(&compassv1.SubscribeCommsRequest{SinceSeq: 0})
	req.Header().Set(commsTestActorHeader, string(actor))

	type result struct {
		evts []*compassv1.SubscribeCommsResponse
		err  error
	}
	out := make(chan result, 1)
	go func() {
		stream, err := h.client.SubscribeComms(ctx, req)
		if err != nil {
			out <- result{err: err}
			return
		}
		var got []*compassv1.SubscribeCommsResponse
		for stream.Receive() {
			m := stream.Msg()
			if ac := m.GetAccountChanged(); ac != nil && ac.GetAccount().GetId() == canaryID {
				out <- result{evts: got}
				return
			}
			got = append(got, m)
		}
		out <- result{err: fmt.Errorf("stream ended before the canary: %w", stream.Err())}
	}()

	select {
	case r := <-out:
		if r.err != nil {
			t.Fatalf("drain as %s: %v", actor, r.err)
		}
		return r.evts
	case <-time.After(5 * time.Second):
		t.Fatalf("drain as %s: timed out before the canary arrived", actor)
		return nil
	}
}

// messagePostedTopics / messageUpdatedTopics / channelChanges /
// channelGroupChanges / accountChanges pull the topic/channel/group/account ids
// out of one variant of a collected event slice, so a case asserts on
// presence/absence of exactly the variant it means to. A message carries only
// its topic now, so the message variants key on topic id (the channel is
// resolved through the topic server-side).

func messagePostedTopics(evts []*compassv1.SubscribeCommsResponse) []string {
	var out []string
	for _, e := range evts {
		if mp := e.GetMessagePosted(); mp != nil {
			out = append(out, mp.GetMessage().GetTopicId())
		}
	}
	return out
}

func messageUpdatedTopics(evts []*compassv1.SubscribeCommsResponse) []string {
	var out []string
	for _, e := range evts {
		if mu := e.GetMessageUpdated(); mu != nil {
			out = append(out, mu.GetMessage().GetTopicId())
		}
	}
	return out
}

func topicUpsertedIDs(evts []*compassv1.SubscribeCommsResponse) []string {
	var out []string
	for _, e := range evts {
		if tu := e.GetTopicUpserted(); tu != nil {
			out = append(out, tu.GetTopic().GetId())
		}
	}
	return out
}

// channelChanges returns every ChannelChanged delivered for channel chID, so a
// case can assert the exact count and inspect removed_account_ids.
func channelChanges(evts []*compassv1.SubscribeCommsResponse, chID string) []*compassv1.ChannelChanged {
	var out []*compassv1.ChannelChanged
	for _, e := range evts {
		if cc := e.GetChannelChanged(); cc != nil && cc.GetChannel().GetId() == chID {
			out = append(out, cc)
		}
	}
	return out
}

func channelGroupChanges(evts []*compassv1.SubscribeCommsResponse) []string {
	var out []string
	for _, e := range evts {
		if cg := e.GetChannelGroupChanged(); cg != nil {
			out = append(out, cg.GetGroup().GetId())
		}
	}
	return out
}

func accountChanges(evts []*compassv1.SubscribeCommsResponse) []string {
	var out []string
	for _, e := range evts {
		if ac := e.GetAccountChanged(); ac != nil {
			out = append(out, ac.GetAccount().GetId())
		}
	}
	return out
}

// TestSubscribeCommsPrivateMessageLeakBlocked is the load-bearing leak
// regression: a non-member of a private channel MUST NOT receive its
// MessagePosted or MessageUpdated over SubscribeComms, while a member MUST. The
// bus fans both events to both subscribers; only visibleToActor's IsChannelMember
// gate stops the leak. Against the pre-fix unfiltered forwardComms both events
// reach the non-member — this reddens on B's collected set.
func TestSubscribeCommsPrivateMessageLeakBlocked(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()

	memberA := mustUser(t, h.store, "member-a")
	outsiderB := mustUser(t, h.store, "outsider-b")

	// A private, ungrouped channel: A is the sole founding member, B is not.
	priv, err := h.store.CreateChannel(ctx, memberA.ID, store.NewChannel{Name: "private", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel(private): %v", err)
	}

	// A posts a plain message (MessagePosted) into a named topic ...
	posted, err := h.svc.PostMessage(WithActor(ctx, memberA.ID), connect.NewRequest(&compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(priv.ID)}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, CreateTopic: true, Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "secret"}}}}))
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	// The private topic both the post and the ask land in: a message carries only
	// its topic now, so the leak assertion correlates on this topic id (the
	// channel is resolved through it server-side).
	privTopic := posted.Msg.GetMessage().GetTopicId()
	// ... and answers a pending ask so the same channel also emits a
	// MessageUpdated (the second gated variant). The ask message is seeded
	// directly in the store (no event needed for it); RespondToAsk through the
	// handler publishes the MessageUpdated.
	if _, _, err := h.store.AppendMessage(ctx, store.Message{AuthorAccountID: memberA.ID, Blocks: []store.MessageBlock{pendingAskStore("ask-leak")}}, string(priv.ID), store.TopicRef{ID: privTopic}, ""); err != nil {
		t.Fatalf("AppendMessage(ask): %v", err)
	}
	if _, err := h.svc.RespondToAsk(WithActor(ctx, memberA.ID), connect.NewRequest(&compassv1.RespondToAskRequest{
		AskId:   "ask-leak",
		Answers: []*compassv1.AskQuestionAnswer{{QuestionId: "q1", ChosenOptionIds: []string{"opt-a"}}},
	})); err != nil {
		t.Fatalf("RespondToAsk: %v", err)
	}

	canary := mkCanary(t, h, "canary")

	// The member sees both the post and the update for the private channel.
	memberEvts := drainReplayAsActor(t, h, memberA.ID, canary)
	if got := messagePostedTopics(memberEvts); !containsString(got, privTopic) {
		t.Fatalf("member did not receive MessagePosted for its own topic; posted-topics = %v", got)
	}
	if got := messageUpdatedTopics(memberEvts); !containsString(got, privTopic) {
		t.Fatalf("member did not receive MessageUpdated for its own topic; updated-topics = %v", got)
	}

	// The non-member sees NEITHER — the leak the filter defends.
	outsiderEvts := drainReplayAsActor(t, h, outsiderB.ID, canary)
	if got := messagePostedTopics(outsiderEvts); containsString(got, privTopic) {
		t.Fatalf("LEAK: non-member received MessagePosted for a private topic; posted-topics = %v", got)
	}
	if got := messageUpdatedTopics(outsiderEvts); containsString(got, privTopic) {
		t.Fatalf("LEAK: non-member received MessageUpdated for a private topic; updated-topics = %v", got)
	}
}

// TestSubscribeCommsPrivateTopicUpsertedLeakBlocked is the third gated-variant
// leak regression (after the MessagePosted/MessageUpdated and ChannelChanged
// cases): a non-member of a private channel MUST NOT receive its TopicUpserted
// over SubscribeComms, while a member MUST. A topic rename fans a TopicUpserted
// carrying the topic's name to the shared bus; only visibleToActor's
// IsChannelMember gate (resolving the topic's channel) stops the private topic's
// name/rename leaking to a non-member. Against a regressed forwardComms that
// drops the TopicUpserted branch the event reaches the non-member — this reddens
// on B's collected set, at read-parity with ListTopics.
func TestSubscribeCommsPrivateTopicUpsertedLeakBlocked(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()

	memberA := mustUser(t, h.store, "member-a")
	outsiderB := mustUser(t, h.store, "outsider-b")

	// A private, ungrouped channel: A is the sole founding member, B is not.
	priv, err := h.store.CreateChannel(ctx, memberA.ID, store.NewChannel{Name: "private", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel(private): %v", err)
	}

	// A posts a message into a named topic, creating the topic (no TopicUpserted
	// yet — get-or-create does not emit one).
	posted, err := h.svc.PostMessage(WithActor(ctx, memberA.ID), connect.NewRequest(&compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(priv.ID)}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, CreateTopic: true, Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "secret"}}}}))
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	privTopic := posted.Msg.GetMessage().GetTopicId()

	// A renames the topic (A is the founding member, so store-authorized) — this
	// is the event under test: UpdateTopic publishes the TopicUpserted.
	newName := "renamed"
	if _, err := h.svc.UpdateTopic(WithActor(ctx, memberA.ID), connect.NewRequest(&compassv1.UpdateTopicRequest{
		TopicId: privTopic,
		Name:    &newName,
	})); err != nil {
		t.Fatalf("UpdateTopic(rename): %v", err)
	}

	canary := mkCanary(t, h, "canary")

	// The member sees the TopicUpserted for its own topic.
	memberEvts := drainReplayAsActor(t, h, memberA.ID, canary)
	if got := topicUpsertedIDs(memberEvts); !containsString(got, privTopic) {
		t.Fatalf("member did not receive TopicUpserted for its own topic; topic-upserted-ids = %v", got)
	}

	// The non-member sees NONE — the leak the filter defends.
	outsiderEvts := drainReplayAsActor(t, h, outsiderB.ID, canary)
	if got := topicUpsertedIDs(outsiderEvts); containsString(got, privTopic) {
		t.Fatalf("LEAK: non-member received TopicUpserted for a private topic; topic-upserted-ids = %v", got)
	}
}

// TestSubscribeCommsChannelChangedSharedVisibleToNonMember pins that
// ChannelChanged is gated on full channel visibility, NOT bare membership: a
// SHARED-grouped channel's ChannelChanged reaches a non-member viewer (at
// read-parity with ListChannels), while a private ungrouped channel's does not.
// A uniform membership filter would wrongly drop the SHARED channel's event —
// this case is what catches a regression back to that uniform check.
func TestSubscribeCommsChannelChangedSharedVisibleToNonMember(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()

	owner := mustUser(t, h.store, "owner")
	viewer := mustUser(t, h.store, "viewer") // member of neither channel

	sharedGroup, err := h.store.CreateChannelGroup(ctx, owner.ID, store.NewChannelGroup{Name: "shared", Visibility: store.VisibilityShared})
	if err != nil {
		t.Fatalf("CreateChannelGroup(shared): %v", err)
	}

	// A SHARED-grouped channel (kind=CHANNEL): visible to everyone via the group
	// lattice even without membership. Its ChannelChanged is emitted through the
	// handler.
	sharedCh, err := h.svc.CreateChannel(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name: "announce", Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL, GroupId: string(sharedGroup.ID),
	}))
	if err != nil {
		t.Fatalf("CreateChannel(shared): %v", err)
	}
	sharedID := sharedCh.Msg.GetChannel().GetId()

	// A private ungrouped channel: owner-only, so the viewer cannot see it.
	privCh, err := h.svc.CreateChannel(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name: "private", Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
	}))
	if err != nil {
		t.Fatalf("CreateChannel(private): %v", err)
	}
	privID := privCh.Msg.GetChannel().GetId()

	canary := mkCanary(t, h, "canary")

	viewerEvts := drainReplayAsActor(t, h, viewer.ID, canary)
	if len(channelChanges(viewerEvts, sharedID)) == 0 {
		t.Fatalf("non-member viewer did not receive ChannelChanged for a SHARED-grouped channel (bare-membership over-drop); events = %+v", viewerEvts)
	}
	if n := len(channelChanges(viewerEvts, privID)); n != 0 {
		t.Fatalf("LEAK: non-member viewer received %d ChannelChanged for a private channel", n)
	}

	// Sanity: the owner sees both (member of each).
	ownerEvts := drainReplayAsActor(t, h, owner.ID, canary)
	if len(channelChanges(ownerEvts, sharedID)) == 0 || len(channelChanges(ownerEvts, privID)) == 0 {
		t.Fatalf("owner missing a ChannelChanged it created; shared=%d private=%d",
			len(channelChanges(ownerEvts, sharedID)), len(channelChanges(ownerEvts, privID)))
	}
}

// TestSubscribeCommsAccountChangedDirectoryScoping pins AccountChanged at
// ListAccounts read-parity: a new HUMAN account's AccountChanged reaches every
// subscriber (all users are listable), but a new AGENT's AccountChanged reaches
// only its owner (and co-members), NOT an unrelated account. This is the
// pass-through-vs-scope contract: the filter must not over-block the human
// directory event, and must not leak an owner-scoped agent to a stranger. The
// pre-rework filter passed ALL account events through unfiltered — the agent
// leak this reddens on the stranger's set.
func TestSubscribeCommsAccountChangedDirectoryScoping(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()

	agentOwner := mustUser(t, h.store, "agent-owner")
	stranger := mustUser(t, h.store, "stranger") // owns nothing, shares no channel

	// A new human account: globally visible, so both subscribers must receive it.
	extra, err := h.svc.CreateUser(ctx, connect.NewRequest(&compassv1.CreateUserRequest{Handle: "extra", DisplayName: "Extra"}))
	if err != nil {
		t.Fatalf("CreateUser(extra): %v", err)
	}
	extraID := extra.Msg.GetAccount().GetId()

	// A new agent owned by agentOwner: owner-scoped, so only the owner may see
	// its AccountChanged.
	agent, err := h.svc.CreateAgent(WithActor(ctx, agentOwner.ID), connect.NewRequest(&compassv1.CreateAgentRequest{Handle: "secret-agent", DisplayName: "Secret"}))
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	agentID := agent.Msg.GetAccount().GetId()

	canary := mkCanary(t, h, "canary")

	ownerEvts := drainReplayAsActor(t, h, agentOwner.ID, canary)
	if got := accountChanges(ownerEvts); !containsString(got, extraID) {
		t.Fatalf("owner did not receive the human AccountChanged; account-changes = %v", got)
	}
	if got := accountChanges(ownerEvts); !containsString(got, agentID) {
		t.Fatalf("owner did not receive its own agent's AccountChanged; account-changes = %v", got)
	}

	strangerEvts := drainReplayAsActor(t, h, stranger.ID, canary)
	if got := accountChanges(strangerEvts); !containsString(got, extraID) {
		t.Fatalf("stranger did not receive the globally-visible human AccountChanged (over-block); account-changes = %v", got)
	}
	if got := accountChanges(strangerEvts); containsString(got, agentID) {
		t.Fatalf("LEAK: stranger received an owner-scoped agent's AccountChanged; account-changes = %v", got)
	}
}

// TestSubscribeCommsRemovedMemberGetsFinalChannelChanged pins the removed-member
// exception: a member removed from a private channel receives exactly ONE final
// ChannelChanged — the removal event, carrying itself in removed_account_ids —
// and nothing more for that channel (it is a non-member thereafter, and the
// channel is private, so full visibility is false). A never-member still
// receives nothing. Without the removed_account_ids carve-out the removed member
// would never learn of its own removal (bare visibility is already false); with
// it, it must get exactly the one event, not the earlier create event too.
func TestSubscribeCommsRemovedMemberGetsFinalChannelChanged(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()

	owner := mustUser(t, h.store, "owner")
	removed := mustUser(t, h.store, "removed")
	neverMember := mustUser(t, h.store, "never")

	// Private channel with owner + `removed` as founding members (emits a create
	// ChannelChanged with removed_account_ids empty).
	ch, err := h.svc.CreateChannel(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name: "room", Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
		MemberHandles: []string{removed.Handle},
	}))
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	chID := ch.Msg.GetChannel().GetId()

	// Owner removes `removed` (emits a ChannelChanged with removed in
	// removed_account_ids).
	if _, err := h.svc.UpdateChannelMembers(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.UpdateChannelMembersRequest{
		ChannelId:           chID,
		RemoveMemberHandles: []string{removed.Handle},
	})); err != nil {
		t.Fatalf("UpdateChannelMembers(remove): %v", err)
	}

	canary := mkCanary(t, h, "canary")

	// The removed member gets EXACTLY ONE ChannelChanged for this channel — the
	// removal event — with itself in removed_account_ids. The earlier create
	// event is NOT delivered: at replay time it is a non-member and not in that
	// event's removed set, so full visibility is false.
	removedEvts := drainReplayAsActor(t, h, removed.ID, canary)
	rccs := channelChanges(removedEvts, chID)
	if len(rccs) != 1 {
		t.Fatalf("removed member received %d ChannelChanged for the channel, want exactly 1 (the final removal event)", len(rccs))
	}
	if !containsString(rccs[0].GetRemovedAccountIds(), string(removed.ID)) {
		t.Fatalf("removed member's final ChannelChanged does not carry it in removed_account_ids: %v", rccs[0].GetRemovedAccountIds())
	}

	// A never-member of this private channel receives nothing for it.
	neverEvts := drainReplayAsActor(t, h, neverMember.ID, canary)
	if n := len(channelChanges(neverEvts, chID)); n != 0 {
		t.Fatalf("LEAK: a never-member received %d ChannelChanged for a private channel", n)
	}
}

// TestSubscribeCommsChannelGroupChangedScoping pins ChannelGroupChanged at
// ListChannelGroups read-parity: a SHARED group's change reaches a non-owner,
// an OWNER group's does not. Pre-rework this passed through unfiltered, leaking
// the owner group's existence to any subscriber.
func TestSubscribeCommsChannelGroupChangedScoping(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()

	owner := mustUser(t, h.store, "owner")
	stranger := mustUser(t, h.store, "stranger")

	sharedGroup, err := h.svc.CreateChannelGroup(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateChannelGroupRequest{
		Name: "shared", Visibility: compassv1.ChannelGroupVisibility_CHANNEL_GROUP_VISIBILITY_SHARED,
	}))
	if err != nil {
		t.Fatalf("CreateChannelGroup(shared): %v", err)
	}
	sharedID := sharedGroup.Msg.GetGroup().GetId()

	ownerGroup, err := h.svc.CreateChannelGroup(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateChannelGroupRequest{
		Name: "owner-only", Visibility: compassv1.ChannelGroupVisibility_CHANNEL_GROUP_VISIBILITY_OWNER,
	}))
	if err != nil {
		t.Fatalf("CreateChannelGroup(owner): %v", err)
	}
	ownerID := ownerGroup.Msg.GetGroup().GetId()

	canary := mkCanary(t, h, "canary")

	strangerEvts := drainReplayAsActor(t, h, stranger.ID, canary)
	if got := channelGroupChanges(strangerEvts); !containsString(got, sharedID) {
		t.Fatalf("non-owner did not receive the SHARED group's ChannelGroupChanged (over-block); group-changes = %v", got)
	}
	if got := channelGroupChanges(strangerEvts); containsString(got, ownerID) {
		t.Fatalf("LEAK: non-owner received an OWNER group's ChannelGroupChanged; group-changes = %v", got)
	}
}

// TestSubscribeCommsPrivateMessageLeakBlockedLiveTail is the M4 live-tail
// companion to the replay-path leak test above. The existing leak coverage
// publishes BEFORE subscribing, so it exercises only forwardComms's replay loop;
// a regression that filtered only the replay loop and left the live tail
// unfiltered would ship green against it. Here both subscribers open and go LIVE
// on the bus FIRST (member + non-member of a private channel), THEN a
// MessagePosted is published — so the event travels the live path, not replay.
// The member's live stream must receive it; the non-member's must not.
//
// The non-member's negative is proven without a sleep: after the private post, a
// globally-visible canary MessagePosted is fired into a channel the non-member
// IS a member of. In-order per-subscriber delivery means the non-member's FIRST
// delivered event is that canary, never the private post — its absence ahead of
// the canary is a real proof, not a race. (For the member, the private post is
// its first event, so it is asserted directly.)
func TestSubscribeCommsPrivateMessageLeakBlockedLiveTail(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()

	memberA := mustUser(t, h.store, "member-a")
	outsiderB := mustUser(t, h.store, "outsider-b")

	// A private channel A founds and B is not in, plus a shared canary channel
	// B IS a member of (so the canary post is visible to B, our high-water mark).
	priv, err := h.store.CreateChannel(ctx, memberA.ID, store.NewChannel{Name: "private", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel(private): %v", err)
	}
	canaryCh, err := h.store.CreateChannel(ctx, outsiderB.ID, store.NewChannel{Name: "canary-room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel(canary): %v", err)
	}

	// Both subscribers go LIVE first (half-duplex: subscribe concurrently with
	// the mutations that follow). No event is in the ring yet — CreateChannel
	// runs on the store directly, off the bus — so each stream's first event is
	// the first bus post it is entitled to.
	memberEvents := firstEventAfterBoundary(t, h, memberA.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})
	outsiderEvents := firstEventAfterBoundary(t, h, outsiderB.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})

	// The private post: member entitled, non-member not.
	privPosted, err := h.svc.PostMessage(WithActor(ctx, memberA.ID), connect.NewRequest(&compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(priv.ID)}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, CreateTopic: true, Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "secret"}}}}))
	if err != nil {
		t.Fatalf("PostMessage(private): %v", err)
	}
	privID := privPosted.Msg.GetMessage().GetId()

	// The canary post into a channel the non-member CAN see: its high-water mark.
	canaryPosted, err := h.svc.PostMessage(WithActor(ctx, outsiderB.ID), connect.NewRequest(&compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(canaryCh.ID)}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, CreateTopic: true, Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "visible canary"}}}}))
	if err != nil {
		t.Fatalf("PostMessage(canary): %v", err)
	}
	canaryID := canaryPosted.Msg.GetMessage().GetId()

	// The member's live tail delivers the private post as its first event.
	memberFirst := awaitFirst(t, memberEvents)
	mp := memberFirst.GetMessagePosted()
	if mp == nil {
		t.Fatalf("member first live event = %T, want MessagePosted", memberFirst.GetPayload())
	}
	if mp.GetMessage().GetId() != privID {
		t.Fatalf("member received message id %q, want the private post %q", mp.GetMessage().GetId(), privID)
	}

	// The non-member's FIRST live event is the canary, never the private post —
	// the live tail filtered the private MessagePosted out. This is the M4 leak
	// the replay-only tests cannot catch.
	outsiderFirst := awaitFirst(t, outsiderEvents)
	omp := outsiderFirst.GetMessagePosted()
	if omp == nil {
		t.Fatalf("non-member first live event = %T, want the canary MessagePosted", outsiderFirst.GetPayload())
	}
	if got := omp.GetMessage().GetId(); got == privID {
		t.Fatalf("LEAK: non-member's live tail received the private MessagePosted %q", privID)
	}
	if got := omp.GetMessage().GetId(); got != canaryID {
		t.Fatalf("non-member first live event id = %q, want the canary %q", got, canaryID)
	}
}
