//go:build pgtest

package store

// Message contracts: AppendMessage assigns id + timestamp and validates its
// input; ListMessages pages newest-first with a working BeforeMessageID cursor
// and a clamped limit; idempotency dedups on (author, non-empty request id) and
// not on an empty key; UpdateMessageBlocks replaces the block set; blocks (text
// and ask, with all ask fields) round-trip through JSONB unchanged; and
// SearchMessages finds matches, scopes to the actor's visible channels, narrows
// by scope, and tolerates punctuated queries.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// textBlock and askBlock build the two block variants for the tests.
func textBlock(s string) MessageBlock { return MessageBlock{Text: &s} }

func askBlock() MessageBlock { return askBlockID("ask-1") }

// askBlockID builds the ask-block variant with a caller-chosen AskID, so a test
// can control (or empty) the incoming id.
func askBlockID(id string) MessageBlock {
	return MessageBlock{Ask: &Ask{
		AskID: id,
		Questions: []AskQuestion{{
			QuestionID: "q1",
			Question:   "Which environment?",
			Options: []AskOption{
				{ID: "opt-a", Label: "staging", Description: "the staging cluster"},
				{ID: "opt-b", Label: "prod"},
			},
			AllowMultiple:   true,
			ChosenOptionIDs: []string{"opt-a", "opt-b"},
		}},
	}}
}

// sampleBlocks is a mixed text + ask block set exercised by the durability and
// round-trip tests.
func sampleBlocks() []MessageBlock {
	return []MessageBlock{textBlock("deploying now"), askBlock()}
}

func TestAppendMessageAssignsIDAndTimestamp(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	msg, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("hello")},
	}, "")
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if msg.ID == "" {
		t.Fatal("AppendMessage did not assign an id")
	}
	if msg.At.IsZero() {
		t.Fatal("AppendMessage did not assign a timestamp")
	}
}

func TestAppendMessageNoBlocksInvalid(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)
	_, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
	}, "")
	sentinelIs(t, err, ErrInvalidArgument, "message with no blocks")
}

func TestAppendMessageUnknownChannelNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	// The D9 membership gate precedes the insert: an author who is not a member
	// of the target channel is refused, and an unknown channel has no members —
	// so an unknown channel and a private one the author cannot see both
	// collapse to ErrNotFound (the not-found/forbidden merge), never a hint the
	// channel exists. Pre-gate this reached the insert and the FK surfaced
	// ErrInvalidArgument.
	_, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: "ghost"}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("hi")},
	}, "")
	sentinelIs(t, err, ErrNotFound, "unknown channel")
}

func TestAppendMessageIdempotency(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	// Same non-empty request id + author → the same stored message, no duplicate.
	first, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("only once")},
	}, "req-idem")
	if err != nil {
		t.Fatalf("AppendMessage(first): %v", err)
	}
	second, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("different body, same key")},
	}, "req-idem")
	if err != nil {
		t.Fatalf("AppendMessage(retry): %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("retry minted a new message %q, want the stored %q", second.ID, first.ID)
	}
	// The retry returns the ORIGINAL stored content, not the retry's body.
	if len(second.Blocks) != 1 || second.Blocks[0].Text == nil || *second.Blocks[0].Text != "only once" {
		t.Fatalf("dedup returned %+v, want the first stored block", second.Blocks)
	}
	if got := messageCount(t, ctx, s, ch.ID); got != 1 {
		t.Fatalf("channel has %d messages after a deduped retry, want 1", got)
	}

	// An empty request id never dedups: two empty-key appends are two messages.
	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("a")},
	}, ""); err != nil {
		t.Fatalf("AppendMessage(empty key a): %v", err)
	}
	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("b")},
	}, ""); err != nil {
		t.Fatalf("AppendMessage(empty key b): %v", err)
	}
	if got := messageCount(t, ctx, s, ch.ID); got != 3 {
		t.Fatalf("channel has %d messages, want 3 (1 deduped + 2 empty-key)", got)
	}
}

// TestAppendMessageInsertedFlag pins the M3 store contract that the handler's
// duplicate-publish suppression rests on: AppendMessage returns inserted=true on
// the first write of an idempotency key and inserted=false on a retry with the
// same key. The handler publishes MessagePosted only when inserted, so a false
// here is what stops a second fan-out on an idempotent retry.
func TestAppendMessageInsertedFlag(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	_, inserted, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("first")},
	}, "req-inserted")
	if err != nil {
		t.Fatalf("AppendMessage(first): %v", err)
	}
	if !inserted {
		t.Fatal("first write returned inserted=false, want true (a row was genuinely inserted)")
	}

	// A retry with the SAME key writes no row: inserted=false, so the handler
	// suppresses a duplicate MessagePosted.
	_, inserted, err = s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("retry, same key")},
	}, "req-inserted")
	if err != nil {
		t.Fatalf("AppendMessage(retry): %v", err)
	}
	if inserted {
		t.Fatal("dedup retry returned inserted=true, want false (no row was written)")
	}

	// An empty key never dedups: it is always a genuine insert.
	_, inserted, err = s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("empty key")},
	}, "")
	if err != nil {
		t.Fatalf("AppendMessage(empty key): %v", err)
	}
	if !inserted {
		t.Fatal("empty-key write returned inserted=false, want true (never deduped)")
	}
}

// TestAppendMessageParentThreading pins the L2 threading contract: a message
// posted with ParentMessageID naming an existing message in the SAME channel
// persists it and round-trips through ListMessages; a root message (empty
// ParentMessageID) round-trips as ""; a ParentMessageID naming no message is
// ErrInvalidArgument; and a ParentMessageID naming a real message in a
// DIFFERENT channel (even one the author can see) is the SAME indistinguishable
// ErrInvalidArgument — the parent must be same-channel and the existence oracle
// stays closed (a caller cannot tell "in another channel" from "does not
// exist").
func TestAppendMessageParentThreading(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	// A root message threads under nothing and reads back with ParentMessageID "".
	root, inserted, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("root")},
	}, "")
	if err != nil {
		t.Fatalf("AppendMessage(root): %v", err)
	}
	if !inserted {
		t.Fatal("root insert returned inserted=false, want true")
	}
	if root.ParentMessageID != "" {
		t.Fatalf("root ParentMessageID = %q, want \"\"", root.ParentMessageID)
	}

	// A reply threads under the root, persisting the parent id.
	reply, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("reply")}, ParentMessageID: root.ID,
	}, "")
	if err != nil {
		t.Fatalf("AppendMessage(reply): %v", err)
	}
	if reply.ParentMessageID != root.ID {
		t.Fatalf("reply ParentMessageID = %q, want the root %q", reply.ParentMessageID, root.ID)
	}

	// It round-trips through the read path: ListMessages returns the reply with
	// its parent id and the root with an empty one.
	msgs, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	parents := map[MessageID]MessageID{}
	for _, m := range msgs {
		parents[m.ID] = m.ParentMessageID
	}
	if got := parents[reply.ID]; got != root.ID {
		t.Fatalf("listed reply ParentMessageID = %q, want the root %q", got, root.ID)
	}
	if got := parents[root.ID]; got != "" {
		t.Fatalf("listed root ParentMessageID = %q, want \"\"", got)
	}

	// A ParentMessageID naming no message is rejected with ErrInvalidArgument.
	_, _, nonexistentErr := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("orphan reply")}, ParentMessageID: MessageID("no-such-message"),
	}, "")
	sentinelIs(t, nonexistentErr, ErrInvalidArgument, "reply under a nonexistent parent")

	// Cross-channel parent: the author is a member of a second channel and posts
	// a root there, then tries to reply in this channel naming that other
	// channel's root as the parent. Same-channel is required, so this is
	// rejected even though the FK would pass (the parent row exists).
	other := mustNamedChannel(t, s, author.ID, "other-room")
	otherRoot, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: other.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("root in the other channel")},
	}, "")
	if err != nil {
		t.Fatalf("AppendMessage(other-channel root): %v", err)
	}
	_, _, crossChannelErr := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("cross-channel reply")}, ParentMessageID: otherRoot.ID,
	}, "")
	sentinelIs(t, crossChannelErr, ErrInvalidArgument, "reply under a cross-channel parent")

	// Existence oracle closed: the cross-channel rejection and the
	// nonexistent-parent rejection are the SAME sentinel, so a caller cannot
	// distinguish "the parent lives in another channel" from "the parent does
	// not exist at all".
	if !errors.Is(crossChannelErr, ErrInvalidArgument) || !errors.Is(nonexistentErr, ErrInvalidArgument) {
		t.Fatalf("cross-channel err %v and nonexistent err %v must both be ErrInvalidArgument", crossChannelErr, nonexistentErr)
	}
	// Neither message may disclose the other channel's id — both name only the
	// parent message id, keeping the oracle shut.
	if strings.Contains(crossChannelErr.Error(), string(other.ID)) {
		t.Fatalf("cross-channel error %q leaks the other channel id %q", crossChannelErr, other.ID)
	}
}

func TestAppendMessageMintsAskID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	// Case 1: an ask submitted with an empty AskID is server-minted. The store
	// owns the ask_id (comms.proto:278-280); a caller-supplied empty id must be
	// filled so the ask is addressable by RespondToAsk.
	empty := askBlockID("")
	appended, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("choose one"), empty},
	}, "req-mint")
	if err != nil {
		t.Fatalf("AppendMessage(empty ask id): %v", err)
	}
	if appended.Blocks[1].Ask == nil {
		t.Fatal("returned message lost its ask block")
	}
	minted := appended.Blocks[1].Ask.AskID
	if minted == "" {
		t.Fatal("AppendMessage did not mint an ask_id for an empty-id ask")
	}
	if len(minted) != 32 {
		t.Fatalf("minted ask_id = %q (len %d), want 32 hex chars to match newID()", minted, len(minted))
	}

	// Case 2: round-trip — the minted id must be in the persisted JSONB, not
	// only the in-memory return. Read it back via the dedup path (same author +
	// request id) so we see exactly what was stored.
	stored, err := s.getMessageByRequestID(ctx, author.ID, "req-mint")
	if err != nil {
		t.Fatalf("getMessageByRequestID: %v", err)
	}
	if stored.Blocks[1].Ask == nil {
		t.Fatal("stored message lost its ask block")
	}
	if got := stored.Blocks[1].Ask.AskID; got != minted {
		t.Fatalf("persisted ask_id = %q, want the minted %q (id must land in the JSONB)", got, minted)
	}

	// Case 3: a caller-supplied ask_id is preserved verbatim, never overwritten.
	supplied, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{askBlockID("ask-fixed-123")},
	}, "req-fixed")
	if err != nil {
		t.Fatalf("AppendMessage(fixed ask id): %v", err)
	}
	if got := supplied.Blocks[0].Ask.AskID; got != "ask-fixed-123" {
		t.Fatalf("returned ask_id = %q, want the caller-supplied ask-fixed-123 (must not overwrite)", got)
	}
	rt, err := s.getMessageByRequestID(ctx, author.ID, "req-fixed")
	if err != nil {
		t.Fatalf("getMessageByRequestID(fixed): %v", err)
	}
	if got := rt.Blocks[0].Ask.AskID; got != "ask-fixed-123" {
		t.Fatalf("persisted ask_id = %q, want the caller-supplied ask-fixed-123", got)
	}
}

func TestListMessagesNewestFirstAndPaging(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	// Append in a known order; ListMessages returns newest-first.
	bodies := []string{"m0", "m1", "m2", "m3"}
	ids := make([]MessageID, 0, len(bodies))
	for _, body := range bodies {
		m, _, err := s.AppendMessage(ctx, Message{
			Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
			Blocks: []MessageBlock{textBlock(body)},
		}, "")
		if err != nil {
			t.Fatalf("AppendMessage(%s): %v", body, err)
		}
		ids = append(ids, m.ID)
	}
	newest, oldest := ids[3], ids[0]

	page, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{Limit: 2})
	if err != nil {
		t.Fatalf("ListMessages(first page): %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("first page len = %d, want 2", len(page))
	}
	if page[0].ID != newest {
		t.Fatalf("first result = %q, want newest %q (newest-first order)", page[0].ID, newest)
	}
	if page[1].ID != ids[2] {
		t.Fatalf("second result = %q, want %q", page[1].ID, ids[2])
	}

	// Page strictly before the last item of page one — returns the older half.
	next, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{Limit: 2, BeforeMessageID: page[1].ID})
	if err != nil {
		t.Fatalf("ListMessages(next page): %v", err)
	}
	if len(next) != 2 {
		t.Fatalf("next page len = %d, want 2", len(next))
	}
	if next[0].ID != ids[1] || next[1].ID != oldest {
		t.Fatalf("next page = [%q %q], want [%q %q]", next[0].ID, next[1].ID, ids[1], oldest)
	}

	// A zero limit falls back to the default page size (not zero results).
	all, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{Limit: 0})
	if err != nil {
		t.Fatalf("ListMessages(default limit): %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("default-limit page len = %d, want all 4 (0 must mean default, not 0)", len(all))
	}
}

func TestListMessagesUnknownCursorInvalid(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)
	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("m")},
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	_, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{BeforeMessageID: "not-a-real-id"})
	sentinelIs(t, err, ErrInvalidArgument, "unknown before-cursor")
}

func TestClampLimitBounds(t *testing.T) {
	// The page-size clamp is the guard against an unbounded read: zero becomes
	// the default, and anything over the max is capped exactly at the max.
	tests := []struct {
		name      string
		requested uint32
		want      uint32
	}{
		{"zero becomes default", 0, defaultPageLimit},
		{"one is preserved", 1, 1},
		{"under max is preserved", maxPageLimit - 1, maxPageLimit - 1},
		{"at max is preserved", maxPageLimit, maxPageLimit},
		{"just over max is capped", maxPageLimit + 1, maxPageLimit},
		{"far over max is capped", 100000, maxPageLimit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampLimit(tc.requested); got != tc.want {
				t.Fatalf("clampLimit(%d) = %d, want %d", tc.requested, got, tc.want)
			}
		})
	}
}

func TestUpdateMessageBlocksReplaces(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	msg, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("original")},
	}, "")
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	replacement := []MessageBlock{textBlock("rewritten"), askBlock()}
	if err := s.UpdateMessageBlocks(ctx, msg.ID, replacement); err != nil {
		t.Fatalf("UpdateMessageBlocks: %v", err)
	}
	got, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1", len(got))
	}
	assertBlocksEqual(t, got[0].Blocks, replacement)
}

func TestUpdateMessageBlocksRejectsEmptyAskID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	msg, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("original")},
	}, "")
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// ask_id is assigned once at append and immutable thereafter. An update
	// carrying an ask block with an empty AskID must be rejected, NOT re-minted:
	// a fresh id would orphan any pending RespondToAsk against the original.
	err = s.UpdateMessageBlocks(ctx, msg.ID, []MessageBlock{textBlock("rewritten"), askBlockID("")})
	sentinelIs(t, err, ErrInvalidArgument, "update ask block with empty ask_id")

	// The rejected update must not have persisted: the message still reads back
	// its original single text block, unchanged.
	got, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	assertBlocksEqual(t, got[0].Blocks, []MessageBlock{textBlock("original")})

	// Companion: an update carrying a NON-empty AskID succeeds and round-trips
	// the caller's existing id verbatim (the id the append minted is what an
	// update must carry back).
	replacement := []MessageBlock{textBlock("rewritten"), askBlockID("ask-existing-42")}
	if err := s.UpdateMessageBlocks(ctx, msg.ID, replacement); err != nil {
		t.Fatalf("UpdateMessageBlocks(non-empty ask id): %v", err)
	}
	after, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{})
	if err != nil {
		t.Fatalf("ListMessages(after update): %v", err)
	}
	assertBlocksEqual(t, after[0].Blocks, replacement)
	if got := after[0].Blocks[1].Ask.AskID; got != "ask-existing-42" {
		t.Fatalf("updated ask_id = %q, want the preserved ask-existing-42", got)
	}
}

func TestUpdateMessageBlocksUnknownNotFound(t *testing.T) {
	err := newTestStore(t).UpdateMessageBlocks(context.Background(), MessageID("ghost"), []MessageBlock{textBlock("x")})
	sentinelIs(t, err, ErrNotFound, "unknown message")
}

func TestMessageBlocksRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	want := sampleBlocks()
	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID, Blocks: want,
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	got, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1", len(got))
	}
	// Every ask field (id, question, options with descriptions, allow_multiple,
	// chosen ids) survives the JSONB round-trip identically.
	assertBlocksEqual(t, got[0].Blocks, want)
}

func TestSearchMessages(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	alice := mustUser(t, s, "alice")
	bob := mustUser(t, s, "bob")

	// Two channels. alice is in chA; bob is in chB. Neither shares the other's.
	chA := mustNamedChannel(t, s, alice.ID, "room-a")
	chB := mustNamedChannel(t, s, bob.ID, "room-b")

	// alice posts a matching and a non-matching message in chA.
	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: chA.ID}, AuthorAccountID: alice.ID,
		Blocks: []MessageBlock{textBlock("the peregrine falcon dives fast")},
	}, ""); err != nil {
		t.Fatalf("append match: %v", err)
	}
	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: chA.ID}, AuthorAccountID: alice.ID,
		Blocks: []MessageBlock{textBlock("an unrelated grocery list")},
	}, ""); err != nil {
		t.Fatalf("append nonmatch: %v", err)
	}
	// bob posts a message that also contains "falcon" but in chB, which alice
	// is not a member of.
	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: chB.ID}, AuthorAccountID: bob.ID,
		Blocks: []MessageBlock{textBlock("falcon heavy launch scheduled")},
	}, ""); err != nil {
		t.Fatalf("append bob: %v", err)
	}

	// alice finds her matching message.
	hits, err := s.SearchMessages(ctx, alice.ID, SearchScope{}, "falcon", Page{})
	if err != nil {
		t.Fatalf("SearchMessages(alice, falcon): %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("alice found %d messages for 'falcon', want 1 (her own; bob's is not visible)", len(hits))
	}
	if hits[0].Container.ChannelID != chA.ID {
		t.Fatalf("alice's hit is in %q, want her channel %q", hits[0].Container.ChannelID, chA.ID)
	}

	// A word present in no visible message returns nothing.
	none, err := s.SearchMessages(ctx, alice.ID, SearchScope{}, "aardvark", Page{})
	if err != nil {
		t.Fatalf("SearchMessages(alice, aardvark): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("found %d messages for a word present nowhere, want 0", len(none))
	}

	// Visibility scoping: bob searching "falcon" finds only his own message,
	// never alice's in a channel he is not a member of.
	bobHits, err := s.SearchMessages(ctx, bob.ID, SearchScope{}, "falcon", Page{})
	if err != nil {
		t.Fatalf("SearchMessages(bob, falcon): %v", err)
	}
	if len(bobHits) != 1 || bobHits[0].Container.ChannelID != chB.ID {
		t.Fatalf("bob found %d hits (%v), want exactly his chB message", len(bobHits), channelIDsOf(bobHits))
	}

	// Scope narrows to one channel: alice scoping to chB (which she cannot see)
	// yields nothing rather than leaking bob's message.
	scoped, err := s.SearchMessages(ctx, alice.ID, SearchScope{ChannelID: chB.ID}, "falcon", Page{})
	if err != nil {
		t.Fatalf("SearchMessages(alice, scope chB): %v", err)
	}
	if len(scoped) != 0 {
		t.Fatalf("alice scoped to chB found %d, want 0 (not a member)", len(scoped))
	}

	// A punctuated / operator-laden query does not error (websearch_to_tsquery
	// parses it safely) and still matches.
	safe, err := s.SearchMessages(ctx, alice.ID, SearchScope{}, `"peregrine falcon" -grocery`, Page{})
	if err != nil {
		t.Fatalf("SearchMessages with punctuation errored: %v", err)
	}
	if len(safe) != 1 {
		t.Fatalf("punctuated phrase query found %d, want 1", len(safe))
	}
}

// pendingAsk builds a pending (unanswered) single-question ask block with a
// caller-chosen id and arity, its one question "q1" offering exactly opt-a and
// opt-b. Distinct from askBlockID, which pre-fills ChosenOptionIDs — an ask
// being answered must start empty.
func pendingAsk(id string, allowMultiple bool) MessageBlock {
	return MessageBlock{Ask: &Ask{
		AskID: id,
		Questions: []AskQuestion{{
			QuestionID: "q1",
			Question:   "Which environment?",
			Options: []AskOption{
				{ID: "opt-a", Label: "staging"},
				{ID: "opt-b", Label: "prod"},
			},
			AllowMultiple: allowMultiple,
		}},
	}}
}

// pendingMultiQ builds a pending TWO-question ask: q1 (single-select, opt-a/
// opt-b) and q2 (single-select, opt-c/opt-d). The disjoint option sets let a
// test prove per-question validation (an option offered by one question is not
// offered by the other) and atomic coverage across more than one question.
func pendingMultiQ(id string) MessageBlock {
	return MessageBlock{Ask: &Ask{
		AskID: id,
		Questions: []AskQuestion{
			{
				QuestionID: "q1",
				Question:   "Which environment?",
				Options: []AskOption{
					{ID: "opt-a", Label: "staging"},
					{ID: "opt-b", Label: "prod"},
				},
			},
			{
				QuestionID: "q2",
				Question:   "Which region?",
				Options: []AskOption{
					{ID: "opt-c", Label: "us-east"},
					{ID: "opt-d", Label: "us-west"},
				},
			},
		},
	}}
}

// askByID reads back the ask block carrying askID from the channel, so a test
// asserts what was durably persisted per-question, not just the return value.
func askByID(t *testing.T, ctx context.Context, s *Store, actor AccountID, ch ChannelID, askID string) *Ask {
	t.Helper()
	msgs, err := s.ListMessages(ctx, actor, ContainerRef{ChannelID: ch}, Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			if b.Ask != nil && b.Ask.AskID == askID {
				return b.Ask
			}
		}
	}
	t.Fatalf("ask %q not found in channel %s", askID, ch)
	return nil
}

// answeredAsk returns the recorded chosen option ids on the SOLE question of a
// single-question ask (the common case). It fails loud if the ask carries other
// than one question, so a multi-question ask is never silently read as if flat.
func answeredAsk(t *testing.T, ctx context.Context, s *Store, actor AccountID, ch ChannelID, askID string) []string {
	t.Helper()
	ask := askByID(t, ctx, s, actor, ch, askID)
	if len(ask.Questions) != 1 {
		t.Fatalf("answeredAsk: ask %q has %d questions, want exactly 1", askID, len(ask.Questions))
	}
	return ask.Questions[0].ChosenOptionIDs
}

func TestAnswerAskHappyPath(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("choose one"), pendingAsk("ask-1", false)},
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	updated, err := s.AnswerAsk(ctx, author.ID, "ask-1", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}}})
	if err != nil {
		t.Fatalf("AnswerAsk: %v", err)
	}
	// The returned message records the chosen option on the ask's question.
	if got := updated.Blocks[1].Ask.Questions[0].ChosenOptionIDs; !reflect.DeepEqual(got, []string{"opt-a"}) {
		t.Fatalf("returned chosen = %v, want [opt-a]", got)
	}
	// And it is durable: a re-read sees the same answer persisted.
	if got := answeredAsk(t, ctx, s, author.ID, ch.ID, "ask-1"); !reflect.DeepEqual(got, []string{"opt-a"}) {
		t.Fatalf("persisted chosen = %v, want [opt-a]", got)
	}
}

func TestAnswerAskVisibilityCollapse(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	actorA := mustUser(t, s, "alice")
	actorB := mustUser(t, s, "bob")

	// alice posts an ask in a channel bob is not a member of.
	chA := mustNamedChannel(t, s, actorA.ID, "alice-room")
	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: chA.ID}, AuthorAccountID: actorA.ID,
		Blocks: []MessageBlock{pendingAsk("ask-secret", false)},
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// Positive control (teeth): the member CAN answer the same ask. Without
	// this, the collapse assertions below pass vacuously whenever AnswerAsk is
	// broken for everyone — a not-found that means "nobody can answer" is not
	// the D9 collapse. alice, a member, must succeed.
	if _, err := s.AnswerAsk(ctx, actorA.ID, "ask-secret", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}}}); err != nil {
		t.Fatalf("member alice cannot answer her own ask: %v", err)
	}

	// THE load-bearing security test (brief pin 2): bob answering alice's
	// non-visible ask gets ErrNotFound — identical to a nonexistent ask — so the
	// ask's existence cannot leak across the membership boundary. It must NOT be
	// a distinct not-authorized error.
	_, err := s.AnswerAsk(ctx, actorB.ID, "ask-secret", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-b"}}})
	sentinelIs(t, err, ErrNotFound, "answer a non-visible ask")

	// The collapse must be indistinguishable from a truly nonexistent ask id.
	_, ghostErr := s.AnswerAsk(ctx, actorB.ID, "ask-does-not-exist", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}}})
	sentinelIs(t, ghostErr, ErrNotFound, "answer a nonexistent ask")

	// bob's rejected answer had no effect: the ask still carries only the
	// member's answer, never bob's opt-b.
	if got := answeredAsk(t, ctx, s, actorA.ID, chA.ID, "ask-secret"); !reflect.DeepEqual(got, []string{"opt-a"}) {
		t.Fatalf("ask recorded %v after bob's rejected answer, want the member's [opt-a] only", got)
	}
}

func TestAnswerAskNonexistentNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)
	// A real member with a real message, but no ask carries this id.
	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{textBlock("no ask here")},
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	_, err := s.AnswerAsk(ctx, author.ID, "ask-ghost", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}}})
	sentinelIs(t, err, ErrNotFound, "answer a nonexistent ask id")
}

// TestAnswerAskValidation pins the reject arm of the atomic-answer contract the
// SEA-1243 reshape ratifies (record §"Server-side answer validation"): coverage
// must be EXACT (contract 3 — no unknown qid, no repeated qid, no gap) and each
// covered answer must respect its question's option set and arity (contract 5 —
// single-select arity, option-not-offered, duplicate option). Every listed
// input is ErrInvalidArgument; the trailing checks prove none of the rejects
// left a trace on any ask.
func TestAnswerAskValidation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	// A single-select ask, a multi-select ask, and a two-question ask, one
	// message each.
	for _, b := range []MessageBlock{pendingAsk("ask-single", false), pendingAsk("ask-multi", true), pendingMultiQ("ask-mq")} {
		if _, _, err := s.AppendMessage(ctx, Message{
			Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
			Blocks: []MessageBlock{b},
		}, ""); err != nil {
			t.Fatalf("AppendMessage(%s): %v", b.Ask.AskID, err)
		}
	}

	cases := []struct {
		name    string
		askID   string
		answers []AskAnswer
	}{
		// (a) unknown question_id: names a qid the ask does not carry.
		{"unknown question_id", "ask-single", []AskAnswer{{QuestionID: "q-nope", ChosenOptionIDs: []string{"opt-a"}}}},
		// (b) repeated question_id: q1 answered twice, q2 never — one entry per
		// question is the rule even though the count would otherwise match.
		{"repeated question_id", "ask-mq", []AskAnswer{
			{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}},
			{QuestionID: "q1", ChosenOptionIDs: []string{"opt-b"}},
		}},
		// (c) coverage gap: a two-question ask with only one question answered.
		{"coverage gap", "ask-mq", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}}}},
		// (d) single-select arity: >1 option on a single-select question.
		{"multi on single-select", "ask-single", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a", "opt-b"}}}},
		// (e) option not offered by THAT question: opt-z is offered by nobody.
		{"option not offered", "ask-single", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-z"}}}},
		// (e') option offered by a DIFFERENT question: opt-c belongs to q2, not
		// q1 — cross-question option leakage must still be rejected.
		{"option offered by other question", "ask-mq", []AskAnswer{
			{QuestionID: "q1", ChosenOptionIDs: []string{"opt-c"}},
			{QuestionID: "q2", ChosenOptionIDs: []string{"opt-c"}},
		}},
		// (f) duplicate option id within one answer.
		{"duplicate option in one answer", "ask-multi", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a", "opt-a"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.AnswerAsk(ctx, author.ID, tc.askID, tc.answers)
			sentinelIs(t, err, ErrInvalidArgument, tc.name)
		})
	}

	// None of the rejected answers persisted: every ask is still unanswered on
	// every question.
	for _, id := range []string{"ask-single", "ask-multi", "ask-mq"} {
		for _, q := range askByID(t, ctx, s, author.ID, ch.ID, id).Questions {
			if len(q.ChosenOptionIDs) != 0 || q.CustomText != "" {
				t.Fatalf("ask %q question %q recorded chosen=%v custom=%q after rejected answers, want none", id, q.QuestionID, q.ChosenOptionIDs, q.CustomText)
			}
		}
	}
}

// TestAnswerAskEmptySkipSatisfiesCoverage pins Matt's OQ2 (record §validation):
// an answer entry with NO chosen ids AND empty custom_text is an ACCEPTED
// deliberate skip that still satisfies coverage — the native forward-skip
// parity. A two-question ask answered with one real answer and one empty-skip
// entry SUCCEEDS and persists exactly that: the answered question carries its
// choice, the skipped one stays empty (not rejected, not defaulted).
func TestAnswerAskEmptySkipSatisfiesCoverage(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{pendingMultiQ("ask-mq")},
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// q1 gets a real answer; q2 is an explicit empty-skip. Coverage is
	// satisfied (both questions named) and the whole answer is accepted.
	if _, err := s.AnswerAsk(ctx, author.ID, "ask-mq", []AskAnswer{
		{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}},
		{QuestionID: "q2"},
	}); err != nil {
		t.Fatalf("AnswerAsk(one real + one empty-skip): %v", err)
	}

	// Per-question persistence: q1 carries opt-a, q2 stays empty on both counts.
	ask := askByID(t, ctx, s, author.ID, ch.ID, "ask-mq")
	byID := map[string]AskQuestion{}
	for _, q := range ask.Questions {
		byID[q.QuestionID] = q
	}
	if got := byID["q1"].ChosenOptionIDs; !reflect.DeepEqual(got, []string{"opt-a"}) {
		t.Fatalf("q1 chosen = %v, want [opt-a]", got)
	}
	if got := byID["q2"].ChosenOptionIDs; len(got) != 0 {
		t.Fatalf("skipped q2 chosen = %v, want none", got)
	}
	if got := byID["q2"].CustomText; got != "" {
		t.Fatalf("skipped q2 custom_text = %q, want empty", got)
	}
}

// TestAnswerAskRejectsReAnswer pins Fork 2 (SEA-1243): an ask is answered
// EXACTLY ONCE. The first answer persists and flips the Ask.Answered flag; a
// second answer — even a different one — is rejected with ErrConflict rather
// than silently overwriting the recorded answer. The trailing read proves the
// reject is atomic: the original answer survives untouched, so the guard fires
// before any per-question write.
func TestAnswerAskRejectsReAnswer(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{pendingAsk("ask-1", false)},
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// First answer succeeds and records opt-a.
	if _, err := s.AnswerAsk(ctx, author.ID, "ask-1", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}}}); err != nil {
		t.Fatalf("first AnswerAsk: %v", err)
	}
	if got := answeredAsk(t, ctx, s, author.ID, ch.ID, "ask-1"); !reflect.DeepEqual(got, []string{"opt-a"}) {
		t.Fatalf("after first answer chosen = %v, want [opt-a]", got)
	}

	// Second answer — a DIFFERENT valid choice — is rejected as a conflict. The
	// containment SELECT re-reads the persisted JSONB, so this also proves the
	// Answered flag survived the round-trip.
	_, err := s.AnswerAsk(ctx, author.ID, "ask-1", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-b"}}})
	sentinelIs(t, err, ErrConflict, "re-answer an answered ask")

	// The reject is atomic: the original opt-a is intact, never overwritten by
	// the rejected opt-b.
	if got := answeredAsk(t, ctx, s, author.ID, ch.ID, "ask-1"); !reflect.DeepEqual(got, []string{"opt-a"}) {
		t.Fatalf("after rejected re-answer chosen = %v, want the original [opt-a]", got)
	}
}

// TestAnswerAskRejectsReAnswerAfterSkip is the load-bearing Fork 2 case: a
// FULLY-SKIPPED ask (its sole question answered with an empty-skip entry)
// leaves NO per-question trace, so only the Answered flag distinguishes it from
// a still-pending ask. A second answer must still be rejected with ErrConflict
// — proving the guard keys on Answered, not on the presence of a recorded
// choice.
func TestAnswerAskRejectsReAnswerAfterSkip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{pendingAsk("ask-skip", false)},
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// An empty-skip entry is an accepted answer that satisfies coverage; it
	// records no chosen option but still flips Answered.
	if _, err := s.AnswerAsk(ctx, author.ID, "ask-skip", []AskAnswer{{QuestionID: "q1"}}); err != nil {
		t.Fatalf("first AnswerAsk(empty-skip): %v", err)
	}
	if got := answeredAsk(t, ctx, s, author.ID, ch.ID, "ask-skip"); len(got) != 0 {
		t.Fatalf("after skip chosen = %v, want none (skip records no choice)", got)
	}

	// Despite the ask carrying no per-question trace, the re-answer is rejected
	// as a conflict — the Answered flag alone gates it.
	_, err := s.AnswerAsk(ctx, author.ID, "ask-skip", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}}})
	sentinelIs(t, err, ErrConflict, "re-answer a skipped ask")

	// The reject took no effect: the ask stays skipped, never gaining opt-a.
	if got := answeredAsk(t, ctx, s, author.ID, ch.ID, "ask-skip"); len(got) != 0 {
		t.Fatalf("after rejected re-answer chosen = %v, want none (still skipped)", got)
	}
}

func TestAnswerAskMultiSelect(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{pendingAsk("ask-multi", true)},
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// A multi-select ask accepts and records more than one offered option.
	if _, err := s.AnswerAsk(ctx, author.ID, "ask-multi", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a", "opt-b"}}}); err != nil {
		t.Fatalf("AnswerAsk(multi): %v", err)
	}
	if got := answeredAsk(t, ctx, s, author.ID, ch.ID, "ask-multi"); !reflect.DeepEqual(got, []string{"opt-a", "opt-b"}) {
		t.Fatalf("persisted chosen = %v, want [opt-a opt-b]", got)
	}
}

// TestAnswerAskRejectsDuplicateOption pins the L4 fix in applyAskAnswer: a
// multi-select ask answered with the SAME offered option id twice is
// ErrInvalidArgument ("chosen more than once"), while distinct multi-selections
// still succeed. Pre-fix the duplicate silently persisted a doubled option;
// the positive companion keeps the negative from passing vacuously.
func TestAnswerAskRejectsDuplicateOption(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{pendingAsk("ask-dup", true)},
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// The same offered option chosen twice is rejected.
	_, err := s.AnswerAsk(ctx, author.ID, "ask-dup", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a", "opt-a"}}})
	sentinelIs(t, err, ErrInvalidArgument, "duplicate chosen option")

	// The rejected answer left no trace: the ask is still unanswered.
	if got := answeredAsk(t, ctx, s, author.ID, ch.ID, "ask-dup"); len(got) != 0 {
		t.Fatalf("ask recorded %v after a rejected duplicate answer, want none", got)
	}

	// Distinct multi-selections still succeed on the same ask.
	if _, err := s.AnswerAsk(ctx, author.ID, "ask-dup", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a", "opt-b"}}}); err != nil {
		t.Fatalf("distinct multi-select rejected: %v", err)
	}
	if got := answeredAsk(t, ctx, s, author.ID, ch.ID, "ask-dup"); !reflect.DeepEqual(got, []string{"opt-a", "opt-b"}) {
		t.Fatalf("persisted chosen = %v, want [opt-a opt-b]", got)
	}
}

// TestAnswerAskConcurrentDistinctAsksSerialize is the SEA-1226 red-first
// regression: two distinct asks on ONE message answered concurrently. AnswerAsk
// reads the whole block set, records its answer on its own ask in that snapshot,
// and writes ALL blocks back (UpdateMessageBlocks) — so an unserialized
// read-modify-write lets the second writer's stale snapshot clobber the first
// writer's answer, silently losing it (last-writer-wins). The contract is that
// both answers survive; this test SHOULD stay RED until the store serializes the
// answer path, then flip GREEN with no test change.
func TestAnswerAskConcurrentDistinctAsksSerialize(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	// One message carrying two independent pending asks.
	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{pendingAsk("ask-x", false), pendingAsk("ask-y", false)},
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// Answer both asks concurrently. A start barrier maximizes overlap of the
	// read-modify-write windows; no sleeps — the WaitGroup gates completion.
	start := make(chan struct{})
	var wg sync.WaitGroup
	answer := func(askID, opt string) {
		defer wg.Done()
		<-start
		_, _ = s.AnswerAsk(ctx, author.ID, askID, []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{opt}}})
	}
	wg.Add(2)
	go answer("ask-x", "opt-a")
	go answer("ask-y", "opt-b")
	close(start)
	wg.Wait()

	// Both answers must be durable. Under the SEA-1226 lost-update the later
	// write's stale block snapshot overwrites the earlier answer, so one of
	// these reads back empty.
	if got := answeredAsk(t, ctx, s, author.ID, ch.ID, "ask-x"); !reflect.DeepEqual(got, []string{"opt-a"}) {
		t.Fatalf("ask-x chosen = %v, want [opt-a] (lost update: SEA-1226)", got)
	}
	if got := answeredAsk(t, ctx, s, author.ID, ch.ID, "ask-y"); !reflect.DeepEqual(got, []string{"opt-b"}) {
		t.Fatalf("ask-y chosen = %v, want [opt-b] (lost update: SEA-1226)", got)
	}
}

// TestAnswerAskConcurrentSameAskOneConflict pins the load-bearing scenario the
// answer-once guard EXISTS for: two RespondToAsk RPCs racing on the SAME ask
// (the real-world double-click). TestAnswerAskRejectsReAnswer proves the guard
// sequentially, so today the concurrent correctness is only transitive — the
// FOR UPDATE lock + Answered guard are assumed to compose but never exercised
// together under a genuine race. This test does it directly: two goroutines,
// gated on a start barrier, answer one ask with DIFFERENT options. Exactly one
// must win (nil err, its option persisted) and exactly one must lose
// (ErrConflict, no effect). Under READ COMMITTED the loser blocks on the row
// lock, EvalPlanQual-re-reads the committed Answered=true row, and the guard
// rejects it — so the outcome is deterministic with no sleeps. Two nils would
// be a lost-update regression (both wrote); two conflicts a both-lost
// regression; and a clobbered read-back would mean the loser's write leaked.
func TestAnswerAskConcurrentSameAskOneConflict(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	// One message carrying one pending single-select ask (q1: opt-a / opt-b).
	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{pendingAsk("ask-race", false)},
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// Two racers on the SAME ask, each choosing a different option. The start
	// barrier maximizes overlap of the two read-modify-write windows; no sleeps
	// — the WaitGroup gates completion and the row lock makes the winner
	// deterministic-in-count (exactly one). Each goroutine records its own error
	// in a private slot so the classification below sees both outcomes.
	opts := [2]string{"opt-a", "opt-b"}
	var errs [2]error
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	for i := range opts {
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = s.AnswerAsk(ctx, author.ID, "ask-race", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{opts[i]}}})
		}(i)
	}
	close(start)
	wg.Wait()

	// Exactly one winner, exactly one ErrConflict loser.
	nils, conflicts, winner := 0, 0, -1
	for i, err := range errs {
		switch {
		case err == nil:
			nils++
			winner = i
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("racer %d (opt %s) returned unexpected error: %v", i, opts[i], err)
		}
	}
	switch {
	case nils == 2:
		t.Fatalf("both racers succeeded (errs=%v): lost-update regression — the answer-once guard did not serialize the race", errs)
	case conflicts == 2:
		t.Fatalf("both racers got ErrConflict (errs=%v): both-lost regression — no answer was ever persisted", errs)
	case nils != 1 || conflicts != 1:
		t.Fatalf("want exactly one nil + one ErrConflict, got %d nil / %d conflict (errs=%v)", nils, conflicts, errs)
	}

	// The persisted choice is the WINNER's option — length 1, one of the two,
	// never empty (nothing written) and never the loser's write leaking in.
	got := answeredAsk(t, ctx, s, author.ID, ch.ID, "ask-race")
	if len(got) != 1 || (got[0] != "opt-a" && got[0] != "opt-b") {
		t.Fatalf("persisted choice = %v, want exactly one of [opt-a] / [opt-b]", got)
	}
	if got[0] != opts[winner] {
		t.Fatalf("persisted choice = %v, but the nil-returning racer chose %s — winner's write did not persist", got, opts[winner])
	}
}

// TestAppendMessageRejectsMalformedAsk pins the marshal-totality half of the
// SEA-1243 ask invariant (contract 1; blocks.go validateAskQuestions, fired at
// AppendMessage via marshalBlocks): an ask block is ErrInvalidArgument if it
// carries zero questions, any empty question_id, or a duplicate question_id
// within the ask — a duplicate or empty key would make an AskQuestionAnswer
// unaddressable, so the bad row never reaches the database. Each case pairs the
// reject with a no-persist check so a regression that both accepts AND writes is
// caught, not just one of the two.
func TestAppendMessageRejectsMalformedAsk(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	cases := []struct {
		name string
		ask  *Ask
	}{
		{"zero questions", &Ask{AskID: "ask-empty", Questions: nil}},
		{"empty question_id", &Ask{AskID: "ask-blank-qid", Questions: []AskQuestion{
			{QuestionID: "", Question: "unaddressable?", Options: []AskOption{{ID: "opt-a", Label: "x"}}},
		}}},
		{"duplicate question_id", &Ask{AskID: "ask-dup-qid", Questions: []AskQuestion{
			{QuestionID: "q1", Question: "first?", Options: []AskOption{{ID: "opt-a", Label: "x"}}},
			{QuestionID: "q1", Question: "second?", Options: []AskOption{{ID: "opt-b", Label: "y"}}},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := s.AppendMessage(ctx, Message{
				Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
				Blocks: []MessageBlock{{Ask: tc.ask}},
			}, "")
			sentinelIs(t, err, ErrInvalidArgument, tc.name)
		})
	}

	// Nothing was written: every malformed append was rejected before the row.
	if n := messageCount(t, ctx, s, ch.ID); n != 0 {
		t.Fatalf("channel holds %d messages after rejected malformed appends, want 0", n)
	}
}

// TestListMessagesFailsLoudOnZeroQuestionAsk pins the unmarshal-totality half
// (contract 2; blocks.go unmarshalBlocks): a stored ask row with zero questions
// — the stale pre-reshape JSONB shape {"ask_id":…,"question":…,"options":…},
// whose old flat keys Go ignores so Questions decodes to nil — must fail LOUD on
// read, not decode to an unanswerable ghost ask. The corrupt row is injected
// directly into the messages table (bypassing the marshal guard, which would
// never write it), then read back through the public ListMessages path; the read
// must surface an error naming the no-questions corruption, not a silent decode.
func TestListMessagesFailsLoudOnZeroQuestionAsk(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	// The pre-reshape stored shape: kind=ask with an ask carrying the OLD flat
	// question/options keys and no "questions" array. marshalBlocks would reject
	// this, so it is inserted straight into the table to simulate a legacy row.
	const staleBlocks = `[{"kind":"ask","ask":{"ask_id":"stale-1","question":"Which environment?","options":[{"id":"opt-a","label":"staging"}]}}]`
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO messages (id, channel_id, author_account_id, at_unix_ms, blocks) VALUES ($1, $2, $3, $4, $5)`,
		newID(), string(ch.ID), string(author.ID), int64(1), staleBlocks,
	); err != nil {
		t.Fatalf("inject stale row: %v", err)
	}

	_, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{})
	if err == nil {
		t.Fatal("ListMessages decoded a zero-question ask silently, want a fail-loud error")
	}
	if !strings.Contains(err.Error(), "no questions") {
		t.Fatalf("ListMessages error = %v, want it to name the no-questions corruption", err)
	}
}

// TestListMessagesFailsLoudOnEmptyQuestionID pins the read-back half of the
// SEA-1243 question_id totality invariant (Greptile P1): the write path
// (validateAskQuestions, blocks.go:95) rejects an empty question_id, but the
// read path (unmarshalBlocks, blocks.go:170) today only rejects ZERO-question
// asks — a stored ask whose single question has an empty question_id survives
// read-back and decodes to an unaddressable question. The row is injected
// straight into the messages table (the marshal guard would never write it),
// then read back through ListMessages; the read must fail loud naming the empty
// question_id, not decode silently. The question array is non-empty and the one
// question is otherwise valid (real question text + one offered option), so the
// ONLY defect is the empty id — isolating the new guard from the existing
// zero-question check.
func TestListMessagesFailsLoudOnEmptyQuestionID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	// A stored ask with one question whose question_id is "". Everything else is
	// valid, so the empty id is the sole corruption.
	const blankQIDBlocks = `[{"kind":"ask","ask":{"ask_id":"blank-qid-1","questions":[{"question_id":"","question":"Which environment?","options":[{"id":"opt-a","label":"staging"}]}]}}]`
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO messages (id, channel_id, author_account_id, at_unix_ms, blocks) VALUES ($1, $2, $3, $4, $5)`,
		newID(), string(ch.ID), string(author.ID), int64(1), blankQIDBlocks,
	); err != nil {
		t.Fatalf("inject empty-question_id row: %v", err)
	}

	_, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{})
	if err == nil {
		t.Fatal("ListMessages decoded an ask with an empty question_id silently, want a fail-loud error")
	}
	if !strings.Contains(err.Error(), "empty question_id") {
		t.Fatalf("ListMessages error = %v, want it to name the empty question_id corruption", err)
	}
}

// TestListMessagesFailsLoudOnDuplicateQuestionID is the duplicate arm of the
// same Greptile P1: the write path rejects a duplicate question_id within an ask
// (validateAskQuestions, blocks.go:104), but read-back (unmarshalBlocks) does
// not — a stored ask with two questions sharing a question_id survives read-back
// today, making one of the two questions unaddressable by an AskQuestionAnswer.
// The corrupt row is injected directly (the marshal guard would reject it), then
// read back through ListMessages; the read must fail loud naming the duplicate
// question_id. Both questions carry distinct text and their own option, so the
// only defect is the shared id.
func TestListMessagesFailsLoudOnDuplicateQuestionID(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	// A stored ask with two questions sharing question_id "dup-1"; each is
	// otherwise valid, so the shared id is the sole corruption.
	const dupQIDBlocks = `[{"kind":"ask","ask":{"ask_id":"dup-qid-1","questions":[{"question_id":"dup-1","question":"Which environment?","options":[{"id":"opt-a","label":"staging"}]},{"question_id":"dup-1","question":"Which region?","options":[{"id":"opt-b","label":"us-east"}]}]}}]`
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO messages (id, channel_id, author_account_id, at_unix_ms, blocks) VALUES ($1, $2, $3, $4, $5)`,
		newID(), string(ch.ID), string(author.ID), int64(1), dupQIDBlocks,
	); err != nil {
		t.Fatalf("inject duplicate-question_id row: %v", err)
	}

	_, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{})
	if err == nil {
		t.Fatal("ListMessages decoded an ask with a duplicate question_id silently, want a fail-loud error")
	}
	if !strings.Contains(err.Error(), "duplicate question_id") {
		t.Fatalf("ListMessages error = %v, want it to name the duplicate question_id corruption", err)
	}
}

// TestAnswerAskRecordsCustomText pins the custom_text answer channel (review M3):
// applyAskAnswer records q.CustomText = a.CustomText (messages.go:380), a real
// free-text answer path with no existing positive coverage — the only sibling
// (TestAnswerAskEmptySkipSatisfiesCoverage) asserts custom_text stays EMPTY on a
// skip. A free-text question carries NO options, so it is answered by custom_text
// alone. This documents existing behavior, so it PASSES on HEAD. The ask pairs
// the free-text question with a single-select one answered validly, so coverage
// is exact; the assertion is specifically on the free-text question's persisted
// custom_text and its empty chosen set.
func TestAnswerAskRecordsCustomText(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	// q-region is free-text (zero options): answerable by custom_text alone.
	// q1 is single-select, answered with a real option so coverage is exact.
	freeTextAsk := MessageBlock{Ask: &Ask{
		AskID: "ask-free",
		Questions: []AskQuestion{
			{
				QuestionID: "q-region",
				Question:   "Which region?",
			},
			{
				QuestionID: "q1",
				Question:   "Which environment?",
				Options: []AskOption{
					{ID: "opt-a", Label: "staging"},
					{ID: "opt-b", Label: "prod"},
				},
			},
		},
	}}
	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{freeTextAsk},
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// q-region answered by custom_text with no chosen options; q1 by an option.
	if _, err := s.AnswerAsk(ctx, author.ID, "ask-free", []AskAnswer{
		{QuestionID: "q-region", CustomText: "us-east-2, please"},
		{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}},
	}); err != nil {
		t.Fatalf("AnswerAsk(custom_text + option): %v", err)
	}

	// Durable per-question state: q-region carries the custom_text and no chosen
	// options.
	ask := askByID(t, ctx, s, author.ID, ch.ID, "ask-free")
	byID := map[string]AskQuestion{}
	for _, q := range ask.Questions {
		byID[q.QuestionID] = q
	}
	if got := byID["q-region"].CustomText; got != "us-east-2, please" {
		t.Fatalf("q-region custom_text = %q, want %q", got, "us-east-2, please")
	}
	if got := byID["q-region"].ChosenOptionIDs; len(got) != 0 {
		t.Fatalf("q-region chosen = %v, want none", got)
	}
}

// TestAnswerAskRejectsOptionPlusCustomTextOnSingleSelect pins guard M1
// (validateQuestionAnswer, messages.go:400): a NON-AllowMultiple question
// answered with BOTH exactly one chosen option AND non-empty custom_text is two
// answers to a single-select question, so it is ErrInvalidArgument. A focused
// test rather than a row in TestAnswerAskValidation, to leave that table's
// shared no-persist sweep undisturbed. The negative is paired with its own
// no-persist check so a regression that both accepts AND records the doubled
// answer is caught, not just an over-eager accept.
func TestAnswerAskRejectsOptionPlusCustomTextOnSingleSelect(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	// A single-select ask: q1 offers opt-a/opt-b, AllowMultiple=false.
	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{pendingAsk("ask-single", false)},
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// One chosen option AND a custom_text on the single-select question: reject.
	_, err := s.AnswerAsk(ctx, author.ID, "ask-single", []AskAnswer{
		{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}, CustomText: "foo"},
	})
	sentinelIs(t, err, ErrInvalidArgument, "option plus custom text on single-select")
	if err == nil || !strings.Contains(err.Error(), "both an option and custom text") {
		t.Fatalf("AnswerAsk error = %v, want it to name the option+custom-text conflict", err)
	}

	// The rejected answer left no trace: the ask is still unanswered on q1.
	ask := askByID(t, ctx, s, author.ID, ch.ID, "ask-single")
	for _, q := range ask.Questions {
		if len(q.ChosenOptionIDs) != 0 || q.CustomText != "" {
			t.Fatalf("ask question %q recorded chosen=%v custom=%q after a rejected answer, want none", q.QuestionID, q.ChosenOptionIDs, q.CustomText)
		}
	}
}

// TestAppendMessageRejectsRecommendedOutOfRange pins guard M2
// (validateAskQuestions, blocks.go:106, the marshal/append write path): an ask
// question whose Recommended *int32 is non-nil and outside [0, len(Options)) is
// ErrInvalidArgument. Mirrors TestAppendMessageRejectsMalformedAsk: each reject
// is paired with a post-hoc no-persist sweep so a regression that both accepts
// AND writes is caught. The happy-path case (in-range Recommended accepted)
// keeps the negatives from passing vacuously — an over-eager guard rejecting a
// valid index would redden it.
func TestAppendMessageRejectsRecommendedOutOfRange(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)

	ptr := func(v int32) *int32 { return &v }
	twoOpts := []AskOption{{ID: "opt-a", Label: "staging"}, {ID: "opt-b", Label: "prod"}}

	cases := []struct {
		name        string
		recommended *int32
	}{
		// off-by-one high: index == len(Options), the first out-of-range value.
		{"index equals option count", ptr(int32(2))},
		// negative: below the valid floor of 0.
		{"negative index", ptr(int32(-1))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := s.AppendMessage(ctx, Message{
				Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
				Blocks: []MessageBlock{{Ask: &Ask{AskID: "ask-oob", Questions: []AskQuestion{{
					QuestionID: "q1", Question: "Which environment?", Options: twoOpts, Recommended: tc.recommended,
				}}}}},
			}, "")
			sentinelIs(t, err, ErrInvalidArgument, tc.name)
			if err == nil || !strings.Contains(err.Error(), "recommended index") || !strings.Contains(err.Error(), "out of range") {
				t.Fatalf("AppendMessage error = %v, want it to name the out-of-range recommended index", err)
			}
		})
	}

	// Nothing was written: every out-of-range append was rejected before the row.
	if n := messageCount(t, ctx, s, ch.ID); n != 0 {
		t.Fatalf("channel holds %d messages after rejected out-of-range appends, want 0", n)
	}

	// Happy path: an in-range Recommended (index 1 of a 2-option question) is
	// accepted and persists a row.
	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []MessageBlock{{Ask: &Ask{AskID: "ask-in-range", Questions: []AskQuestion{{
			QuestionID: "q1", Question: "Which environment?", Options: twoOpts, Recommended: ptr(int32(1)),
		}}}}},
	}, ""); err != nil {
		t.Fatalf("AppendMessage(in-range recommended): %v", err)
	}
	if n := messageCount(t, ctx, s, ch.ID); n != 1 {
		t.Fatalf("channel holds %d messages after one accepted append, want 1", n)
	}
}

// mustChannel / mustNamedChannel create a plain owner-scoped channel.
func mustChannel(t *testing.T, s *Store, actor AccountID) Channel {
	t.Helper()
	return mustNamedChannel(t, s, actor, "room")
}

func mustNamedChannel(t *testing.T, s *Store, actor AccountID, name string) Channel {
	t.Helper()
	ch, err := s.CreateChannel(context.Background(), actor, NewChannel{Name: name, Kind: ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel(%q): %v", name, err)
	}
	return ch
}

// messageCount reads how many message rows a channel holds.
func messageCount(t *testing.T, ctx context.Context, s *Store, ch ChannelID) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM messages WHERE channel_id = $1", string(ch),
	).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return n
}

func channelIDsOf(msgs []Message) []ChannelID {
	ids := make([]ChannelID, len(msgs))
	for i, m := range msgs {
		ids[i] = m.Container.ChannelID
	}
	return ids
}

// assertBlocksEqual compares two block sets by value (text pointers dereferenced,
// ask fields deep-compared), so a round-trip that drops or reorders a field is
// caught.
func assertBlocksEqual(t *testing.T, got, want []MessageBlock) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("block count = %d, want %d (got %+v)", len(got), len(want), got)
	}
	for i := range want {
		w, g := want[i], got[i]
		switch {
		case w.Text != nil:
			if g.Text == nil || *g.Text != *w.Text {
				t.Fatalf("block %d text = %v, want %q", i, g.Text, *w.Text)
			}
			if g.Ask != nil {
				t.Fatalf("block %d has an unexpected ask payload alongside text", i)
			}
		case w.Ask != nil:
			if g.Ask == nil {
				t.Fatalf("block %d lost its ask payload", i)
			}
			if !reflect.DeepEqual(*g.Ask, *w.Ask) {
				t.Fatalf("block %d ask = %+v, want %+v", i, *g.Ask, *w.Ask)
			}
			if g.Text != nil {
				t.Fatalf("block %d has an unexpected text payload alongside ask", i)
			}
		}
	}
}
