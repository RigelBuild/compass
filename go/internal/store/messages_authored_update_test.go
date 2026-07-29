//go:build pgtest

package store

// UpdateMessageBlocksAsAuthor: the AUTHORIZING block-update path, the store leg
// of the relayed-agent-input write-through (SEA-1364 T3). Its whole reason to
// exist is that updateMessageBlocksExec takes a bare MessageID and
// performs NO membership or authorship check — safe for AnswerAsk, which has
// already gated on the actor's visible set, but a privilege hole on a path whose
// message id arrives from a relayed agent frame. Every test here defends one
// half of the predicate or one validation:
//
//   - membership AND authorship both required, and both collapse to ErrNotFound
//     (the D9 not-found/forbidden merge — an actor must not learn that a message
//     it cannot touch exists);
//   - the updated row comes back via RETURNING, so the caller can fan out the
//     post-update state without a second read;
//   - the validations updateMessageBlocksExec already enforces (empty block set,
//     an ask block with an empty AskID) are enforced here too, plus an empty
//     message id;
//   - and, critically, AnswerAsk is untouched — its locked read-modify-write
//     still runs through the un-authorized shared core.

import (
	"context"
	"reflect"
	"testing"
)

// authoredMessage appends one text message by author into ch and returns it —
// the row the authorizing-update cases then try to edit under various actors.
func authoredMessage(t *testing.T, ctx context.Context, s *Store, author AccountID, ch ChannelID, text string) Message {
	t.Helper()
	msg, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch}, AuthorAccountID: author,
		Blocks: []MessageBlock{textBlock(text)},
	}, "")
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	return msg
}

// The happy path: the author, who is a member of the message's channel, edits
// its blocks in place and gets the UPDATED row back from RETURNING — same id,
// same channel, same author, new blocks — and the edit is durable on re-read.
//
// Mutation that reddens it: returning the pre-update row (RETURNING on the old
// tuple), or dropping the text_content refresh (the re-read block assertion
// stays green but search would silently drift — covered by the read-back here
// only for blocks; the text index is asserted by the search suite).
func TestUpdateMessageBlocksAsAuthorEditsInPlaceAndReturnsRow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)
	msg := authoredMessage(t, ctx, s, author.ID, ch.ID, "first draft")

	replacement := []MessageBlock{textBlock("settled turn"), askBlockID("ask-existing-7")}
	updated, err := s.UpdateMessageBlocksAsAuthor(ctx, author.ID, msg.ID, replacement)
	if err != nil {
		t.Fatalf("UpdateMessageBlocksAsAuthor: %v", err)
	}
	if updated.ID != msg.ID {
		t.Fatalf("returned message id = %q, want the edited row %q", updated.ID, msg.ID)
	}
	if updated.Container.ChannelID != ch.ID {
		t.Fatalf("returned channel = %q, want %q", updated.Container.ChannelID, ch.ID)
	}
	if updated.AuthorAccountID != author.ID {
		t.Fatalf("returned author = %q, want %q", updated.AuthorAccountID, author.ID)
	}
	assertBlocksEqual(t, updated.Blocks, replacement)

	// Durable: a re-read through the normal visibility path sees the new set.
	got, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("channel holds %d messages, want 1 (an update must edit in place, never insert)", len(got))
	}
	assertBlocksEqual(t, got[0].Blocks, replacement)
}

// THE cross-account security case. A different account — a full member of the
// same channel, so ONLY authorship separates it from the author — cannot edit
// the message, and gets ErrNotFound rather than a distinct forbidden. The
// positive control (the author succeeding on the same row) is what stops this
// passing vacuously if the update were broken for everyone.
//
// Mutation: drop `AND author_account_id = $actor` from the predicate → the
// member's edit succeeds and both the error assertion and the untouched-content
// assertion redden.
func TestUpdateMessageBlocksAsAuthorCrossAccountIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	other := mustUser(t, s, "other")
	// `other` is a founding member of the channel: it can READ the message, so
	// the only thing refusing its edit is the authorship half of the predicate.
	ch, err := s.CreateChannel(ctx, author.ID, NewChannel{
		Name: "room", Kind: ChannelKindChannel, MemberAccountIDs: []AccountID{other.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	msg := authoredMessage(t, ctx, s, author.ID, ch.ID, "the author's words")

	_, err = s.UpdateMessageBlocksAsAuthor(ctx, other.ID, msg.ID, []MessageBlock{textBlock("put into their mouth")})
	sentinelIs(t, err, ErrNotFound, "a channel member editing another account's message")

	// Positive control: the author CAN edit the same row, so the refusal above
	// is authorship, not a blanket failure.
	if _, err := s.UpdateMessageBlocksAsAuthor(ctx, author.ID, msg.ID, []MessageBlock{textBlock("the author's own edit")}); err != nil {
		t.Fatalf("the author cannot edit its own message: %v", err)
	}

	// The rejected edit left no trace: the row carries the author's own text.
	got, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	assertBlocksEqual(t, got[0].Blocks, []MessageBlock{textBlock("the author's own edit")})
}

// The membership half: an account that AUTHORED the message but has since been
// removed from its channel cannot edit it — a revoked member is refused with the
// same ErrNotFound, so losing read access loses write access atomically. This is
// the case a bare authorship check would miss.
//
// Mutation: drop the channel_members EXISTS clause → the revoked author's edit
// succeeds and this reddens twice (error and content).
func TestUpdateMessageBlocksAsAuthorRevokedMemberIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	author := mustUser(t, s, "author")
	ch, err := s.CreateChannel(ctx, owner.ID, NewChannel{
		Name: "room", Kind: ChannelKindChannel, MemberAccountIDs: []AccountID{author.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	msg := authoredMessage(t, ctx, s, author.ID, ch.ID, "posted while a member")

	// Positive control BEFORE the revoke: while still a member the author can
	// edit, so the post-revoke refusal is the membership change and nothing else.
	if _, err := s.UpdateMessageBlocksAsAuthor(ctx, author.ID, msg.ID, []MessageBlock{textBlock("edited while a member")}); err != nil {
		t.Fatalf("the author cannot edit while still a member: %v", err)
	}

	if _, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{{AccountID: author.ID, Remove: true}}); err != nil {
		t.Fatalf("UpdateChannelMembers(remove): %v", err)
	}

	_, err = s.UpdateMessageBlocksAsAuthor(ctx, author.ID, msg.ID, []MessageBlock{textBlock("edited after the revoke")})
	sentinelIs(t, err, ErrNotFound, "a revoked member editing its own past message")

	// The revoked edit left no trace: the owner still reads the pre-revoke text.
	got, err := s.ListMessages(ctx, owner.ID, ContainerRef{ChannelID: ch.ID}, Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	assertBlocksEqual(t, got[0].Blocks, []MessageBlock{textBlock("edited while a member")})
}

// An unknown message id is the same ErrNotFound as a message the actor may not
// touch — the two are indistinguishable, which is what makes the refusals above
// non-enumerating.
func TestUpdateMessageBlocksAsAuthorUnknownMessageIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	_, err := s.UpdateMessageBlocksAsAuthor(ctx, author.ID, MessageID("ghost"), []MessageBlock{textBlock("x")})
	sentinelIs(t, err, ErrNotFound, "unknown message id")
}

// The three input validations, each ErrInvalidArgument and each leaving the row
// untouched. Empty message id is new here (updateMessageBlocksExec lets it fall
// through to a zero-rows ErrNotFound); the empty block set and the empty AskID
// mirror the checks the shared core already makes, restated because this path
// deliberately does not call it.
func TestUpdateMessageBlocksAsAuthorRejectsMalformedInput(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)
	msg := authoredMessage(t, ctx, s, author.ID, ch.ID, "untouched")

	t.Run("empty message id", func(t *testing.T) {
		// An empty id is malformed input, NOT a missing row: a relayed
		// MessageUpdated whose message.id never got stamped is an agent/wire
		// defect the caller should see as such, distinct from an edit aimed at a
		// row the actor may not touch.
		_, err := s.UpdateMessageBlocksAsAuthor(ctx, author.ID, MessageID(""), []MessageBlock{textBlock("x")})
		sentinelIs(t, err, ErrInvalidArgument, "empty message id")
	})
	t.Run("empty block set", func(t *testing.T) {
		_, err := s.UpdateMessageBlocksAsAuthor(ctx, author.ID, msg.ID, nil)
		sentinelIs(t, err, ErrInvalidArgument, "empty block set")
	})
	t.Run("ask block with no ask id", func(t *testing.T) {
		// ask_id is minted once at append and immutable; an update carrying an
		// id-less ask would orphan any pending RespondToAsk against the original.
		_, err := s.UpdateMessageBlocksAsAuthor(ctx, author.ID, msg.ID, []MessageBlock{textBlock("x"), askBlockID("")})
		sentinelIs(t, err, ErrInvalidArgument, "ask block with an empty ask_id")
	})

	// None of the three wrote anything.
	got, err := s.ListMessages(ctx, author.ID, ContainerRef{ChannelID: ch.ID}, Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	assertBlocksEqual(t, got[0].Blocks, []MessageBlock{textBlock("untouched")})
}

// The regression a shared-helper change would cause: AnswerAsk must keep working
// through the UN-authorized updateMessageBlocksExec core. AnswerAsk gates on
// membership itself (its FOR UPDATE find-and-lock JOINs channel_members) and
// deliberately allows a MEMBER who is not the author to answer — so folding it
// onto the authorship-requiring path would silently break every ask answered by
// anyone but the asker. This test answers as a non-author member precisely to
// pin that.
func TestAnswerAskStillWorksForANonAuthorMember(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	asker := mustUser(t, s, "asker")
	answerer := mustUser(t, s, "answerer")
	ch, err := s.CreateChannel(ctx, asker.ID, NewChannel{
		Name: "room", Kind: ChannelKindChannel, MemberAccountIDs: []AccountID{answerer.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, _, err := s.AppendMessage(ctx, Message{
		Container: ContainerRef{ChannelID: ch.ID}, AuthorAccountID: asker.ID,
		Blocks: []MessageBlock{textBlock("choose one"), pendingAsk("ask-1", false)},
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	updated, err := s.AnswerAsk(ctx, answerer.ID, "ask-1", []AskAnswer{{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}}})
	if err != nil {
		t.Fatalf("AnswerAsk by a non-author member: %v", err)
	}
	if got := updated.Blocks[1].Ask.Questions[0].ChosenOptionIDs; !reflect.DeepEqual(got, []string{"opt-a"}) {
		t.Fatalf("returned chosen = %v, want [opt-a]", got)
	}
	if got := answeredAsk(t, ctx, s, asker.ID, ch.ID, "ask-1"); !reflect.DeepEqual(got, []string{"opt-a"}) {
		t.Fatalf("persisted chosen = %v, want [opt-a]", got)
	}
}
