//go:build pgtest

package store

// Topic contracts (compass-zulip-threading-model T2, §Red-first): get-or-create
// is settled by the unique index (concurrent posts on one name converge on one
// row, never a surfaced unique-violation); a get-or-create resolving an archived
// name revives it; ListTopics is channel-membership-gated (the D9 not-found
// merge for a non-member); UpdateTopic renames, archives, and MERGES on a
// rename-to-existing (source messages carry the target topic_id, source row
// gone). These are properties only a real Postgres proves, so the file is
// pgtest-tagged.

import (
	"context"
	"sync"
	"testing"
)

// TestAppendMessageConcurrentGetOrCreateOneTopic pins §Red-first (b): two posts
// naming the SAME topic name in one channel, raced, converge on exactly one
// topic row — the ON CONFLICT DO NOTHING + re-SELECT get-or-create never
// surfaces a unique-violation to either racer. A start barrier maximizes overlap
// of the two insert txns.
func TestAppendMessageConcurrentGetOrCreateOneTopic(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, errs[i] = s.AppendMessage(ctx, Message{
				AuthorAccountID: author.ID, Blocks: []MessageBlock{textBlock("racing post")},
			}, string(ch.ID), TopicRef{Name: "retry policy"}, "")
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d get-or-create surfaced an error: %v", i, err)
		}
	}

	// Exactly one topic row survives the race.
	topics, err := s.ListTopics(ctx, string(author.ID), string(ch.ID), false)
	if err != nil {
		t.Fatalf("ListTopics: %v", err)
	}
	if len(topics) != 1 {
		t.Fatalf("channel holds %d topics after %d concurrent posts to one name, want 1", len(topics), n)
	}
	if got := messageCount(t, ctx, s, ch.ID); got != n {
		t.Fatalf("channel holds %d messages, want all %d", got, n)
	}
}

// TestAppendMessageGetOrCreateRevivesArchivedTopic pins §Red-first (c): a
// get-or-create resolving to an archived topic clears its archived flag in the
// same tx — archive is a tidiness flag, not a lock, so a post at a tidied-away
// name revives the conversation rather than erroring or forking a case-variant
// duplicate.
func TestAppendMessageGetOrCreateRevivesArchivedTopic(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	// Birth the topic, then archive it.
	first, _, err := s.AppendMessage(ctx, Message{
		AuthorAccountID: author.ID, Blocks: []MessageBlock{textBlock("hello")},
	}, string(ch.ID), TopicRef{Name: "Retry Policy"}, "")
	if err != nil {
		t.Fatalf("AppendMessage(first): %v", err)
	}
	archived := true
	if _, err := s.UpdateTopic(ctx, string(author.ID), first.TopicID, nil, &archived); err != nil {
		t.Fatalf("UpdateTopic(archive): %v", err)
	}

	// A post at a case-variant of the archived name resolves INTO the archived
	// topic (case-insensitive unique index) and clears the flag.
	revived, _, err := s.AppendMessage(ctx, Message{
		AuthorAccountID: author.ID, Blocks: []MessageBlock{textBlock("back on this")},
	}, string(ch.ID), TopicRef{Name: "retry policy"}, "")
	if err != nil {
		t.Fatalf("AppendMessage(revive): %v", err)
	}
	if revived.TopicID != first.TopicID {
		t.Fatalf("revive landed in topic %q, want the archived %q (case-insensitive get-or-create)", revived.TopicID, first.TopicID)
	}

	// The topic is live again: it appears in the default (non-archived) list.
	topics, err := s.ListTopics(ctx, string(author.ID), string(ch.ID), false)
	if err != nil {
		t.Fatalf("ListTopics: %v", err)
	}
	if len(topics) != 1 || topics[0].Archived {
		t.Fatalf("topics = %+v, want exactly one non-archived topic (archive cleared on revive)", topics)
	}
}

// TestListTopicsChannelMembershipGated pins the D9 read gate: a non-member —
// or a caller naming a channel it cannot see — gets ErrNotFound (the
// not-found/forbidden merge), never a hint the channel exists or an empty list
// it could mistake for "no topics". A member reads the topics.
func TestListTopicsChannelMembershipGated(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	member := mustUser(t, s, "member")
	outsider := mustUser(t, s, "outsider")
	ch := mustChannel(t, s, member.ID)

	if _, _, err := s.AppendMessage(ctx, Message{
		AuthorAccountID: member.ID, Blocks: []MessageBlock{textBlock("hi")},
	}, string(ch.ID), TopicRef{Name: "general"}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// The member sees the topic.
	topics, err := s.ListTopics(ctx, string(member.ID), string(ch.ID), false)
	if err != nil {
		t.Fatalf("ListTopics(member): %v", err)
	}
	if len(topics) != 1 || topics[0].Name != "general" {
		t.Fatalf("member ListTopics = %+v, want the one 'general' topic", topics)
	}

	// The outsider gets ErrNotFound — never an empty list, never a leak.
	_, err = s.ListTopics(ctx, string(outsider.ID), string(ch.ID), false)
	sentinelIs(t, err, ErrNotFound, "non-member ListTopics")

	// An unknown channel is the same ErrNotFound, indistinguishable from a
	// private one the caller cannot see.
	_, err = s.ListTopics(ctx, string(member.ID), "ghost-channel", false)
	sentinelIs(t, err, ErrNotFound, "unknown channel ListTopics")
}

// TestUpdateTopicRenameInPlace pins the non-colliding rename: a rename to a
// fresh name changes the name and leaves the messages under the same topic id.
func TestUpdateTopicRenameInPlace(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	post, _, err := s.AppendMessage(ctx, Message{
		AuthorAccountID: author.ID, Blocks: []MessageBlock{textBlock("hello")},
	}, string(ch.ID), TopicRef{Name: "old name"}, "")
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	name := "new name"
	renamed, err := s.UpdateTopic(ctx, string(author.ID), post.TopicID, &name, nil)
	if err != nil {
		t.Fatalf("UpdateTopic(rename): %v", err)
	}
	if renamed.ID != post.TopicID {
		t.Fatalf("rename minted a new topic %q, want the same %q", renamed.ID, post.TopicID)
	}
	if renamed.Name != "new name" {
		t.Fatalf("renamed name = %q, want %q", renamed.Name, "new name")
	}

	// The message still reads back under the renamed topic.
	msgs, err := s.ListMessages(ctx, ListMessagesQuery{Actor: author.ID, ChannelID: ch.ID, Page: Page{}})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].TopicID != post.TopicID {
		t.Fatalf("message topic = %v, want the renamed topic %q", topicIDsOf(msgs), post.TopicID)
	}
}

// TestUpdateTopicRenameToExistingMerges pins §Red-first (d): renaming a topic to
// a name another topic in the channel already holds MERGES them — the source's
// messages carry the target's topic_id, the target's last_seq absorbs the
// source's, and the emptied source row is gone. The surviving target is
// returned.
func TestUpdateTopicRenameToExistingMerges(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	// Two topics, each with a message. src is renamed INTO dst.
	src, _, err := s.AppendMessage(ctx, Message{
		AuthorAccountID: author.ID, Blocks: []MessageBlock{textBlock("in source")},
	}, string(ch.ID), TopicRef{Name: "source topic"}, "")
	if err != nil {
		t.Fatalf("AppendMessage(source): %v", err)
	}
	dst, _, err := s.AppendMessage(ctx, Message{
		AuthorAccountID: author.ID, Blocks: []MessageBlock{textBlock("in target")},
	}, string(ch.ID), TopicRef{Name: "target topic"}, "")
	if err != nil {
		t.Fatalf("AppendMessage(target): %v", err)
	}
	if src.TopicID == dst.TopicID {
		t.Fatal("source and target resolved to the same topic; the merge test needs two distinct topics")
	}

	// Rename source → the target's name: a collision, so the two merge.
	targetName := "target topic"
	survivor, err := s.UpdateTopic(ctx, string(author.ID), src.TopicID, &targetName, nil)
	if err != nil {
		t.Fatalf("UpdateTopic(merge): %v", err)
	}
	if survivor.ID != dst.TopicID {
		t.Fatalf("merge survivor = %q, want the target %q", survivor.ID, dst.TopicID)
	}

	// The source topic row is gone: only the target remains.
	topics, err := s.ListTopics(ctx, string(author.ID), string(ch.ID), true)
	if err != nil {
		t.Fatalf("ListTopics: %v", err)
	}
	if len(topics) != 1 || topics[0].ID != dst.TopicID {
		t.Fatalf("topics after merge = %+v, want only the target %q", topics, dst.TopicID)
	}

	// Both messages now live under the target topic.
	msgs, err := s.ListMessages(ctx, ListMessagesQuery{Actor: author.ID, ChannelID: ch.ID, Page: Page{}})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("channel holds %d messages after merge, want 2", len(msgs))
	}
	for _, m := range msgs {
		if m.TopicID != dst.TopicID {
			t.Fatalf("post-merge message topic = %q, want the target %q (source messages must carry the target id)", m.TopicID, dst.TopicID)
		}
	}
}

// TestUpdateTopicUnknownOrNonMemberNotFound pins the D9 gate on the write path:
// an unknown topic id, and a topic in a channel the caller is not a member of,
// both collapse to ErrNotFound — topic existence cannot leak across a
// membership boundary.
func TestUpdateTopicUnknownOrNonMemberNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	member := mustUser(t, s, "member")
	outsider := mustUser(t, s, "outsider")
	ch := mustChannel(t, s, member.ID)

	post, _, err := s.AppendMessage(ctx, Message{
		AuthorAccountID: member.ID, Blocks: []MessageBlock{textBlock("hi")},
	}, string(ch.ID), TopicRef{Name: "general"}, "")
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	name := "renamed"
	_, err = s.UpdateTopic(ctx, string(member.ID), "no-such-topic", &name, nil)
	sentinelIs(t, err, ErrNotFound, "unknown topic UpdateTopic")

	_, err = s.UpdateTopic(ctx, string(outsider.ID), post.TopicID, &name, nil)
	sentinelIs(t, err, ErrNotFound, "non-member UpdateTopic")
}
