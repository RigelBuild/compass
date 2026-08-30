//go:build pgtest

package store

// The create_topic gate (peer-DM record R5, amending the zulip-threading model's
// unconditional get-or-create): a post to an unknown topic NAME fails with
// ErrNotFound unless TopicRef.Create is set; with the flag it mints. An id-ref is
// unaffected (Create is ignored). Archived-topic revival is unchanged when the
// flag is set — archive is a tidiness flag, not a lock. These pin the store half
// of the tool-edge sprawl guard; the tool edge threads create_topic into
// TopicRef.Create.

import (
	"context"
	"testing"
)

// TestAppendMessageUnknownTopicWithoutCreateIsNotFound: a name that names no
// existing topic, posted WITHOUT Create, is ErrNotFound and writes nothing —
// never a silent mint. The channel stays empty, proving the gated miss rolls the
// whole append back.
func TestAppendMessageUnknownTopicWithoutCreateIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	_, _, err := s.AppendMessage(ctx,
		Message{AuthorAccountID: author.ID, Blocks: []MessageBlock{textBlock("no topic yet")}},
		string(ch.ID), TopicRef{Name: "brand-new"}, "")
	sentinelIs(t, err, ErrNotFound, "unknown topic name without create flag")

	if n := messageCount(t, ctx, s, ch.ID); n != 0 {
		t.Fatalf("gated miss persisted %d rows, want 0 (refusal must not write)", n)
	}
}

// TestAppendMessageUnknownTopicWithCreateMints: the same post WITH Create mints
// the topic and lands the message under it — the trusted-minter path.
func TestAppendMessageUnknownTopicWithCreateMints(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	posted, inserted, err := s.AppendMessage(ctx,
		Message{AuthorAccountID: author.ID, Blocks: []MessageBlock{textBlock("mint me")}},
		string(ch.ID), TopicRef{Name: "brand-new", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage(create): %v", err)
	}
	if !inserted || posted.TopicID == "" {
		t.Fatalf("create post: inserted=%v topicID=%q, want a real insert under a minted topic", inserted, posted.TopicID)
	}

	// A second post to the SAME name, now WITHOUT the flag, resolves the existing
	// topic — the gate blocks minting, not posting to an already-born topic.
	second, _, err := s.AppendMessage(ctx,
		Message{AuthorAccountID: author.ID, Blocks: []MessageBlock{textBlock("follow-up, no flag")}},
		string(ch.ID), TopicRef{Name: "brand-new"}, "")
	if err != nil {
		t.Fatalf("AppendMessage(follow-up, no flag, existing topic): %v", err)
	}
	if second.TopicID != posted.TopicID {
		t.Fatalf("follow-up landed in topic %q, want the existing %q", second.TopicID, posted.TopicID)
	}
}

// TestAppendMessageArchivedTopicRevivalUnchangedWithCreate: resolving to an
// archived topic clears its archived flag in the same tx (revival), and this is
// unchanged under the new gate — with Create set, a post at a tidied-away name
// revives the existing topic rather than forking a duplicate.
func TestAppendMessageArchivedTopicRevivalUnchangedWithCreate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	// Birth the topic, then archive it.
	first, _, err := s.AppendMessage(ctx,
		Message{AuthorAccountID: author.ID, Blocks: []MessageBlock{textBlock("hello")}},
		string(ch.ID), TopicRef{Name: "tidied", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage(birth): %v", err)
	}
	archived := true
	if _, err := s.UpdateTopic(ctx, string(author.ID), first.TopicID, nil, &archived); err != nil {
		t.Fatalf("UpdateTopic(archive): %v", err)
	}

	// A post at the archived name WITH Create resolves the SAME topic and clears
	// the flag — revival, not a duplicate.
	revived, _, err := s.AppendMessage(ctx,
		Message{AuthorAccountID: author.ID, Blocks: []MessageBlock{textBlock("back on this")}},
		string(ch.ID), TopicRef{Name: "tidied", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage(revive): %v", err)
	}
	if revived.TopicID != first.TopicID {
		t.Fatalf("revive landed in topic %q, want the archived %q (revival, not duplicate)", revived.TopicID, first.TopicID)
	}

	topics, err := s.ListTopics(ctx, string(author.ID), string(ch.ID), false)
	if err != nil {
		t.Fatalf("ListTopics: %v", err)
	}
	if len(topics) != 1 || topics[0].Archived {
		t.Fatalf("after revival topics = %+v, want the one 'tidied' topic un-archived", topics)
	}
}
