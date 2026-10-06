//go:build pgtest

package store

import (
	"context"
	"testing"
	"time"
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
