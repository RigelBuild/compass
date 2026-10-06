//go:build pgtest

package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

func appendAsParticipant(t *testing.T, s *Store, author AccountID, channel ChannelID, text string) (Message, error) {
	t.Helper()
	m, _, err := s.AppendMessage(t.Context(), Message{
		AuthorAccountID: author, Blocks: []MessageBlock{textBlock(text)},
	}, string(channel), TopicRef{Name: "general", Create: true}, "")
	return m, err
}

func countChannelMembers(t *testing.T, s *Store, id ChannelID) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM channel_members WHERE channel_id = $1`, string(id)).Scan(&n); err != nil {
		t.Fatalf("count channel members: %v", err)
	}
	return n
}

// treeFixture: owner → root → mid → leaf, plus a sibling outside root's subtree
// and a foreign user; the TREE channel is anchored at root.
type treeFixture struct {
	owner, foreign           Account
	root, mid, leaf, sibling Account
	channel                  Channel
}

func newTreeFixture(t *testing.T, s *Store) treeFixture {
	t.Helper()
	owner := mustUser(t, s, "probe-owner")
	root := mustAgent(t, s, owner.ID, "probe-root")
	mid := mustAgentWithParent(t, s, owner.ID, root.ID, "probe-mid")
	return treeFixture{
		owner:   owner,
		foreign: mustUser(t, s, "probe-foreign"),
		root:    root,
		mid:     mid,
		leaf:    mustAgentWithParent(t, s, owner.ID, mid.ID, "probe-leaf"),
		sibling: mustAgent(t, s, owner.ID, "probe-sibling"),
		channel: mustAttachedChannel(t, s, owner.ID, root.ID, "probe-tree", ChannelMembershipModeTree),
	}
}

func TestChannelParticipantTreeArm(t *testing.T) {
	s := newTestStore(t)
	f := newTreeFixture(t, s)

	for _, author := range []Account{f.owner, f.root, f.mid, f.leaf} {
		if _, err := appendAsParticipant(t, s, author.ID, f.channel.ID, "from "+author.Handle); err != nil {
			t.Fatalf("AppendMessage by participant %s: %v", author.Handle, err)
		}
	}
	for _, outsider := range []Account{f.sibling, f.foreign} {
		_, err := appendAsParticipant(t, s, outsider.ID, f.channel.ID, "intrusion")
		sentinelIs(t, err, ErrNotFound, "post by non-participant "+outsider.Handle)
	}

	topics, err := s.ListTopics(t.Context(), string(f.leaf.ID), string(f.channel.ID), false)
	if err != nil {
		t.Fatalf("ListTopics by a subtree agent: %v", err)
	}
	if len(topics) != 1 || topics[0].Name != "general" {
		t.Fatalf("ListTopics by a subtree agent = %+v, want the general topic", topics)
	}
	_, err = s.ListTopics(t.Context(), string(f.sibling.ID), string(f.channel.ID), false)
	sentinelIs(t, err, ErrNotFound, "ListTopics by an agent outside the subtree")

	for _, tc := range []struct {
		who  Account
		want bool
	}{{f.leaf, true}, {f.owner, true}, {f.sibling, false}, {f.foreign, false}} {
		got, err := s.IsChannelMember(t.Context(), tc.who.ID, f.channel.ID)
		if err != nil || got != tc.want {
			t.Fatalf("IsChannelMember(%s) = %v, %v; want %v", tc.who.Handle, got, err, tc.want)
		}
		got, err = s.IsTopicChannelMember(t.Context(), tc.who.ID, topics[0].ID)
		if err != nil || got != tc.want {
			t.Fatalf("IsTopicChannelMember(%s) = %v, %v; want %v", tc.who.Handle, got, err, tc.want)
		}
	}
	if got, err := s.IsTopicChannelMember(t.Context(), f.owner.ID, "missing-topic"); err != nil || got {
		t.Fatalf("IsTopicChannelMember(unknown topic) = %v, %v; want false", got, err)
	}
	if n := countChannelMembers(t, s, f.channel.ID); n != 0 {
		t.Fatalf("tree channel member rows = %d, want 0 (participation is derived)", n)
	}
}

func TestChannelVisibilityOwnerSetParity(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "visibility-owner")
	anchor := mustAgent(t, s, owner.ID, "visibility-anchor")
	sibling := mustAgent(t, s, owner.ID, "visibility-sibling")
	foreign := mustUser(t, s, "visibility-foreign")
	channel := mustAttachedChannel(t, s, owner.ID, anchor.ID, "visibility-tree", ChannelMembershipModeTree)

	for _, tc := range []struct {
		name string
		who  AccountID
		want bool
	}{
		{name: "owner", who: owner.ID, want: true},
		{name: "same-owner sibling", who: sibling.ID, want: true},
		{name: "foreign user", who: foreign.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channels, err := s.ListChannels(t.Context(), tc.who)
			if err != nil {
				t.Fatalf("ListChannels: %v", err)
			}
			listed := false
			for _, got := range channels {
				if got.ID == channel.ID {
					listed = true
				}
			}
			visible, err := s.ChannelVisibleTo(t.Context(), tc.who, channel.ID)
			if err != nil {
				t.Fatalf("ChannelVisibleTo: %v", err)
			}
			if listed != tc.want || visible != tc.want || visible != listed {
				t.Fatalf("owner-set visibility: ListChannels=%v ChannelVisibleTo=%v, want %v", listed, visible, tc.want)
			}
		})
	}
}

func TestChannelTreeReadPathsUseDerivedParticipants(t *testing.T) {
	s := newTestStore(t)
	f := newTreeFixture(t, s)

	message, err := appendAsParticipant(t, s, f.leaf.ID, f.channel.ID, "subtree searchable history")
	if err != nil {
		t.Fatalf("AppendMessage by subtree agent: %v", err)
	}

	listed, err := s.ListMessages(t.Context(), ListMessagesQuery{Actor: f.leaf.ID, ChannelID: f.channel.ID})
	if err != nil || len(listed) != 1 || listed[0].ID != message.ID {
		t.Fatalf("ListMessages for subtree agent = %v, %v; want message %q", listed, err, message.ID)
	}

	searched, err := s.SearchMessages(t.Context(), f.leaf.ID, SearchScope{ChannelID: f.channel.ID}, "subtree searchable", Page{})
	if err != nil || len(searched) != 1 || searched[0].ID != message.ID {
		t.Fatalf("SearchMessages for subtree agent = %v, %v; want message %q", searched, err, message.ID)
	}

	cursorSeq, err := s.q.GetPageCursorSeq(t.Context(), db.GetPageCursorSeqParams{
		AccountID: string(f.leaf.ID), ID: string(message.ID), ChannelID: string(f.channel.ID),
	})
	if err != nil || cursorSeq == 0 {
		t.Fatalf("GetPageCursorSeq for subtree agent = %d, %v; want message sequence", cursorSeq, err)
	}
	page, err := s.ListMessages(t.Context(), ListMessagesQuery{Actor: f.leaf.ID, ChannelID: f.channel.ID, Page: Page{BeforeMessageID: message.ID}})
	if err != nil || len(page) != 0 {
		t.Fatalf("ListMessages cursor for subtree agent = %v, %v; want empty page", page, err)
	}

	ownerOnly, err := s.ListMessages(t.Context(), ListMessagesQuery{Actor: f.sibling.ID, ChannelID: f.channel.ID})
	if err != nil || len(ownerOnly) != 0 {
		t.Fatalf("owner-set sibling history = %v, %v; want no messages", ownerOnly, err)
	}
	if _, err := s.UpdateMessageBlocksAsAuthor(t.Context(), f.leaf.ID, message.ID, []MessageBlock{textBlock("edited by subtree author")}); err != nil {
		t.Fatalf("UpdateMessageBlocksAsAuthor by subtree author: %v", err)
	}
	if _, err := s.UpdateTopic(t.Context(), string(f.leaf.ID), listed[0].TopicID, nil, nil); err != nil {
		t.Fatalf("UpdateTopic by subtree agent: %v", err)
	}
	_, _, err = s.AppendMessage(t.Context(), Message{AuthorAccountID: f.leaf.ID, Blocks: []MessageBlock{pendingAsk("tree-ask", false)}}, string(f.channel.ID), TopicRef{Name: "asks", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage(ask): %v", err)
	}
	askFilter, err := askIDContainmentFilter("tree-ask")
	if err != nil {
		t.Fatalf("askIDContainmentFilter: %v", err)
	}
	asks, err := s.q.FindAskMessage(t.Context(), db.FindAskMessageParams{AccountID: string(f.leaf.ID), Column2: askFilter})
	if err != nil || len(asks) != 1 {
		t.Fatalf("FindAskMessage for subtree agent = %d rows, %v; want one ask", len(asks), err)
	}
	if _, _, err := s.AnswerAsk(t.Context(), f.sibling.ID, "tree-ask", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}}}); err == nil {
		t.Fatal("owner-set sibling answered TREE history, want membership refusal")
	}
	if _, _, err := s.AnswerAsk(t.Context(), f.leaf.ID, "tree-ask", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}}}); err != nil {
		t.Fatalf("AnswerAsk by subtree agent: %v", err)
	}
}

func TestChannelTreeMembersAreAttributedAndSubscriptionsIntersect(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "materialize-owner")
	rootA := mustAgent(t, s, owner.ID, "materialize-a")
	childA := mustAgentWithParent(t, s, owner.ID, rootA.ID, "materialize-a-child")
	rootB := mustAgent(t, s, owner.ID, "materialize-b")
	childB := mustAgentWithParent(t, s, owner.ID, rootB.ID, "materialize-b-child")
	otherOwner := mustUser(t, s, "materialize-other")
	outside := mustAgent(t, s, otherOwner.ID, "materialize-outside")
	channelA := mustAttachedChannel(t, s, owner.ID, rootA.ID, "materialize-tree-a", ChannelMembershipModeTree)
	channelB := mustAttachedChannel(t, s, owner.ID, rootB.ID, "materialize-tree-b", ChannelMembershipModeTree)

	if _, err := s.ReparentAgent(t.Context(), owner.ID, childA.ID, rootB.ID); err != nil {
		t.Fatalf("ReparentAgent(child A out): %v", err)
	}
	if _, err := s.scopedPool().Exec(t.Context(),
		`INSERT INTO channel_subscriptions (channel_id, account_id, subscribed) VALUES ($1, $2, TRUE)`,
		string(channelA.ID), string(childA.ID)); err != nil {
		t.Fatalf("insert stale subscription: %v", err)
	}
	if _, err := s.scopedPool().Exec(t.Context(),
		`INSERT INTO channel_subscriptions (channel_id, account_id, subscribed) VALUES ($1, $2, TRUE)`,
		string(channelA.ID), string(rootA.ID)); err != nil {
		t.Fatalf("insert live subscription: %v", err)
	}
	if _, err := s.scopedPool().Exec(t.Context(),
		`INSERT INTO channel_subscriptions (channel_id, account_id, subscribed) VALUES ($1, $2, TRUE)`,
		string(channelA.ID), string(outside.ID)); err != nil {
		t.Fatalf("insert outside stale subscription: %v", err)
	}

	channels, err := s.ListChannels(t.Context(), owner.ID)
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	got := make(map[ChannelID]map[AccountID]bool)
	for _, channel := range channels {
		if channel.ID == channelA.ID || channel.ID == channelB.ID {
			got[channel.ID] = memberSet(channel)
			if channel.ID == channelA.ID && !reflect.DeepEqual(channel.SubscriberAccountIDs, []AccountID{rootA.ID}) {
				t.Fatalf("channel A subscribers = %v, want only %s", channel.SubscriberAccountIDs, rootA.ID)
			}
			if channel.ID == channelB.ID && len(channel.SubscriberAccountIDs) != 0 {
				t.Fatalf("channel B subscribers = %v, want none", channel.SubscriberAccountIDs)
			}
		}
	}
	wantA := map[AccountID]bool{owner.ID: true, rootA.ID: true}
	wantB := map[AccountID]bool{owner.ID: true, rootB.ID: true, childA.ID: true, childB.ID: true}
	if !reflect.DeepEqual(got[channelA.ID], wantA) || !reflect.DeepEqual(got[channelB.ID], wantB) {
		t.Fatalf("derived participants are misattributed: A=%v want %v; B=%v want %v", got[channelA.ID], wantA, got[channelB.ID], wantB)
	}
	if n := countChannelMembers(t, s, channelA.ID); n != 0 {
		t.Fatalf("TREE channel has %d stored member rows, want 0", n)
	}
}

func TestChannelParticipantFollowsReparentAgent(t *testing.T) {
	s := newTestStore(t)
	f := newTreeFixture(t, s)
	if _, err := appendAsParticipant(t, s, f.leaf.ID, f.channel.ID, "before move"); err != nil {
		t.Fatalf("post before move: %v", err)
	}
	before := countChannelMembers(t, s, f.channel.ID)

	if _, err := s.ReparentAgent(t.Context(), f.owner.ID, f.leaf.ID, f.sibling.ID); err != nil {
		t.Fatalf("ReparentAgent(leaf → sibling): %v", err)
	}
	_, err := appendAsParticipant(t, s, f.leaf.ID, f.channel.ID, "after move")
	sentinelIs(t, err, ErrNotFound, "post after the agent left the subtree")
	if after := countChannelMembers(t, s, f.channel.ID); after != before {
		t.Fatalf("member rows changed on agent reparent: %d → %d, want no reconcile", before, after)
	}
}

func TestChannelParticipantExplicitChannelIgnoresTree(t *testing.T) {
	s := newTestStore(t)
	f := newTreeFixture(t, s)
	explicit := mustAttachedChannel(t, s, f.owner.ID, f.root.ID, "probe-explicit", ChannelMembershipModeExplicit, f.root.ID)

	_, err := appendAsParticipant(t, s, f.mid.ID, explicit.ID, "not a member")
	sentinelIs(t, err, ErrNotFound, "post by a subtree agent into an EXPLICIT channel")
	posted, err := appendAsParticipant(t, s, f.root.ID, explicit.ID, "member row")
	if err != nil {
		t.Fatalf("post by the explicit member anchor: %v", err)
	}
	if got, err := s.IsTopicChannelMember(t.Context(), f.mid.ID, posted.TopicID); err != nil || got {
		t.Fatalf("IsTopicChannelMember(subtree agent, explicit topic) = %v, %v; want false", got, err)
	}
}

// hasGenuineAdd needs stored-row semantics: a derived participant with no
// member row is still a genuine add.
func TestHasGenuineAddIgnoresDerivedParticipation(t *testing.T) {
	s := newTestStore(t)
	f := newTreeFixture(t, s)
	tx, err := s.beginTenantTx(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()

	add, err := hasGenuineAdd(t.Context(), tx, f.channel.ID, []MemberUpdate{{AccountID: f.leaf.ID, Subscribed: true}})
	if err != nil {
		t.Fatalf("hasGenuineAdd: %v", err)
	}
	if !add {
		t.Fatal("hasGenuineAdd(derived participant without a row) = false, want true")
	}
}

// The schema does not forbid parent cycles (the store refuses them on write), so
// a raw-SQL cycle proves the UNION walk in both probes terminates.
func TestChannelParticipantTerminatesOnParentCycle(t *testing.T) {
	s := newTestStore(t)
	f := newTreeFixture(t, s)
	if _, err := appendAsParticipant(t, s, f.owner.ID, f.channel.ID, "seed"); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	topics, err := s.ListTopics(t.Context(), string(f.owner.ID), string(f.channel.ID), false)
	if err != nil || len(topics) != 1 {
		t.Fatalf("ListTopics = %v, %v; want one topic", topics, err)
	}
	a := mustAgent(t, s, f.owner.ID, "cycle-a")
	b := mustAgentWithParent(t, s, f.owner.ID, a.ID, "cycle-b")
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE agent_accounts SET parent_agent_id = $2 WHERE account_id = $1`, string(a.ID), string(b.ID)); err != nil {
		t.Fatalf("close the parent cycle: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if got, err := s.IsChannelMember(ctx, b.ID, f.channel.ID); err != nil || got {
		t.Fatalf("IsChannelMember(cyclic agent) = %v, %v; want false", got, err)
	}
	if got, err := s.IsTopicChannelMember(ctx, b.ID, topics[0].ID); err != nil || got {
		t.Fatalf("IsTopicChannelMember(cyclic agent) = %v, %v; want false", got, err)
	}
}
