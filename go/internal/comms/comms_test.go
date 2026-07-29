//go:build pgtest

package comms

// Unary CommsService handler contracts, driven in-process via connect.NewRequest
// + WithActor against a real store and a real bus (no mocks). These cover the
// authorization edge (a non-member's channel op collapses to CodeNotFound), the
// membership tiers (join grants read via MemberAccountIDs, a subscribe toggle
// flips SubscriberAccountIDs), CreateChannel's ChannelChanged fan-out, the
// RespondToAsk happy path + visibility collapse, and the store-error -> connect-
// code edge mapping.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// mustUser creates a human account through the store or fails the test.
func mustUser(t *testing.T, s *store.Store, handle string) store.Account {
	t.Helper()
	acc, err := s.CreateUser(context.Background(), store.NewUser{Handle: handle, DisplayName: handle})
	if err != nil {
		t.Fatalf("CreateUser(%q): %v", handle, err)
	}
	return acc
}

// mustAgent creates an owned agent account through the store or fails the test.
func mustAgent(t *testing.T, s *store.Store, owner store.AccountID, handle string) store.Account {
	t.Helper()
	acc, err := s.CreateAgent(context.Background(), owner, store.NewAgent{Handle: handle, DisplayName: handle})
	if err != nil {
		t.Fatalf("CreateAgent(%q): %v", handle, err)
	}
	return acc
}

// connectCodeIs asserts err is a connect error carrying want.
func connectCodeIs(t *testing.T, err error, want connect.Code, ctx string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: got nil error, want connect code %v", ctx, want)
	}
	if got := connect.CodeOf(err); got != want {
		t.Fatalf("%s: connect code = %v, want %v (err: %v)", ctx, got, want, err)
	}
}

// newHandler builds a handler over a real store + bus with a bootstrapped admin
// fallback, returning both so tests drive it directly with WithActor.
func newHandler(t *testing.T) (*Comms, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	bus := newBus(t)
	admin, err := st.BootstrapAdmin(context.Background(), store.NewUser{Handle: "root", DisplayName: "Root"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	return NewComms(st, bus, admin.ID), st
}

func TestCreateChannelEmitsChannelChanged(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()
	creator := mustUser(t, h.store, "creator")

	events := firstEventAfterBoundary(t, h, creator.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})

	created, err := h.svc.CreateChannel(WithActor(ctx, creator.ID), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name: "new-room", Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
	}))
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	wantID := created.Msg.GetChannel().GetId()

	got := awaitFirst(t, events)
	cc := got.GetChannelChanged()
	if cc == nil {
		t.Fatalf("event payload = %T, want ChannelChanged", got.GetPayload())
	}
	if cc.GetChannel().GetId() != wantID {
		t.Fatalf("ChannelChanged id = %q, want the created %q", cc.GetChannel().GetId(), wantID)
	}
}

func TestMembershipTiersJoinVersusSubscribe(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	owner := mustUser(t, st, "owner")
	newcomer := mustUser(t, st, "newcomer")

	created, err := svc.CreateChannel(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateChannelRequest{
		Name: "room", Kind: compassv1.ChannelKind_CHANNEL_KIND_CHANNEL,
	}))
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	chID := created.Msg.GetChannel().GetId()

	// Join tier: add as a member WITHOUT subscribing -> read access
	// (MemberAccountIDs) but no push (absent from SubscriberAccountIDs).
	joined, err := svc.UpdateChannelMembers(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.UpdateChannelMembersRequest{
		ChannelId:           chID,
		AddMemberAccountIds: []string{string(newcomer.ID)},
	}))
	if err != nil {
		t.Fatalf("UpdateChannelMembers(join): %v", err)
	}
	if !containsString(joined.Msg.GetChannel().GetMemberAccountIds(), string(newcomer.ID)) {
		t.Fatalf("after join, members %v missing %s", joined.Msg.GetChannel().GetMemberAccountIds(), newcomer.ID)
	}
	if containsString(joined.Msg.GetChannel().GetSubscriberAccountIds(), string(newcomer.ID)) {
		t.Fatalf("join alone put %s in subscribers %v; join must not grant push", newcomer.ID, joined.Msg.GetChannel().GetSubscriberAccountIds())
	}

	// Subscribe tier: a subscribe toggle flips the per-member boolean, so the
	// member now appears in SubscriberAccountIDs too.
	subbed, err := svc.UpdateChannelMembers(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.UpdateChannelMembersRequest{
		ChannelId:           chID,
		SubscribeAccountIds: []string{string(newcomer.ID)},
	}))
	if err != nil {
		t.Fatalf("UpdateChannelMembers(subscribe): %v", err)
	}
	if !containsString(subbed.Msg.GetChannel().GetSubscriberAccountIds(), string(newcomer.ID)) {
		t.Fatalf("after subscribe, subscribers %v missing %s", subbed.Msg.GetChannel().GetSubscriberAccountIds(), newcomer.ID)
	}

	// Unsubscribe flips it back off while keeping membership.
	unsubbed, err := svc.UpdateChannelMembers(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.UpdateChannelMembersRequest{
		ChannelId:             chID,
		UnsubscribeAccountIds: []string{string(newcomer.ID)},
	}))
	if err != nil {
		t.Fatalf("UpdateChannelMembers(unsubscribe): %v", err)
	}
	if containsString(unsubbed.Msg.GetChannel().GetSubscriberAccountIds(), string(newcomer.ID)) {
		t.Fatalf("after unsubscribe, subscribers %v still include %s", unsubbed.Msg.GetChannel().GetSubscriberAccountIds(), newcomer.ID)
	}
	if !containsString(unsubbed.Msg.GetChannel().GetMemberAccountIds(), string(newcomer.ID)) {
		t.Fatalf("unsubscribe dropped %s from members %v; it should keep read access", newcomer.ID, unsubbed.Msg.GetChannel().GetMemberAccountIds())
	}
}

func TestSearchMessagesAuthorizationScoped(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	alice := mustUser(t, st, "alice")
	bob := mustUser(t, st, "bob")

	// alice's private channel with a searchable message; bob is not a member.
	chA, err := st.CreateChannel(ctx, alice.ID, store.NewChannel{Name: "alice-room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, _, err := st.AppendMessage(ctx, store.Message{
		Container: store.ContainerRef{ChannelID: chA.ID}, AuthorAccountID: alice.ID,
		Blocks: []store.MessageBlock{{Text: ptr("peregrine falcon")}},
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// alice, a member, finds her message.
	aliceHits, err := svc.SearchMessages(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.SearchMessagesRequest{Query: "falcon"}))
	if err != nil {
		t.Fatalf("SearchMessages(alice): %v", err)
	}
	if len(aliceHits.Msg.GetMessages()) != 1 {
		t.Fatalf("alice found %d, want her 1 message", len(aliceHits.Msg.GetMessages()))
	}

	// bob, a non-member, sees nothing — the store scopes the search to his
	// visible set, so a private channel never leaks through search.
	bobHits, err := svc.SearchMessages(WithActor(ctx, bob.ID), connect.NewRequest(&compassv1.SearchMessagesRequest{Query: "falcon"}))
	if err != nil {
		t.Fatalf("SearchMessages(bob): %v", err)
	}
	if len(bobHits.Msg.GetMessages()) != 0 {
		t.Fatalf("bob found %d messages in a channel he cannot see, want 0", len(bobHits.Msg.GetMessages()))
	}
}

// TestListMessagesVisibilityScopedAtEdge is a RED regression pinning the D9
// not-found/forbidden contract on the ListMessages read path. store.ListMessages
// (messages.go:104) currently takes no actor and applies no membership scoping —
// pure WHERE channel_id=$1 — so a non-member reads any private channel by id,
// while SearchMessages/AnswerAsk correctly JOIN channel_members. A non-member's
// list must yield nothing (mirroring SearchMessages, which returns an empty set
// for a non-visible channel rather than an error). This SHOULD stay RED until
// the store gains an actor param + membership JOIN, then flip GREEN unchanged.
func TestListMessagesVisibilityScopedAtEdge(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	alice := mustUser(t, st, "alice")
	bob := mustUser(t, st, "bob")

	// alice's private channel with a message; bob is not a member.
	chA, err := st.CreateChannel(ctx, alice.ID, store.NewChannel{Name: "alice-room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, _, err := st.AppendMessage(ctx, store.Message{
		Container: store.ContainerRef{ChannelID: chA.ID}, AuthorAccountID: alice.ID,
		Blocks: []store.MessageBlock{{Text: ptr("private plans")}},
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// alice, a member, reads her channel.
	aliceMsgs, err := svc.ListMessages(WithActor(ctx, alice.ID), connect.NewRequest(&compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: string(chA.ID)},
	}))
	if err != nil {
		t.Fatalf("ListMessages(alice): %v", err)
	}
	if len(aliceMsgs.Msg.GetMessages()) != 1 {
		t.Fatalf("alice read %d messages, want her 1", len(aliceMsgs.Msg.GetMessages()))
	}

	// bob, a non-member, must NOT read alice's private channel by id. Today the
	// unscoped store returns the row — the D9 leak this test pins.
	bobMsgs, err := svc.ListMessages(WithActor(ctx, bob.ID), connect.NewRequest(&compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: string(chA.ID)},
	}))
	if err != nil {
		t.Fatalf("ListMessages(bob): %v", err)
	}
	if len(bobMsgs.Msg.GetMessages()) != 0 {
		t.Fatalf("bob read %d messages from a channel he is not a member of, want 0 (D9 leak: ListMessages is unscoped)", len(bobMsgs.Msg.GetMessages()))
	}
}

func TestRespondToAskHappyPathEmitsMessageUpdated(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()
	author := mustUser(t, h.store, "author")
	ch, err := h.store.CreateChannel(ctx, author.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, _, err := h.store.AppendMessage(ctx, store.Message{
		Container: store.ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []store.MessageBlock{pendingAskStore("ask-1")},
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	events := firstEventAfterBoundary(t, h, author.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})

	if _, err := h.svc.RespondToAsk(WithActor(ctx, author.ID), connect.NewRequest(&compassv1.RespondToAskRequest{
		AskId:   "ask-1",
		Answers: []*compassv1.AskQuestionAnswer{{QuestionId: "q1", ChosenOptionIds: []string{"opt-a"}}},
	})); err != nil {
		t.Fatalf("RespondToAsk: %v", err)
	}

	// The answer emits MessageUpdated carrying the answered ask block.
	got := awaitFirst(t, events)
	mu := got.GetMessageUpdated()
	if mu == nil {
		t.Fatalf("event payload = %T, want MessageUpdated", got.GetPayload())
	}
	var chosen []string
	var answered bool
	for _, b := range mu.GetMessage().GetBlocks() {
		if a := b.GetAsk(); a != nil && a.GetAskId() == "ask-1" {
			answered = a.GetAnswered()
			for _, q := range a.GetQuestions() {
				if q.GetQuestionId() == "q1" {
					chosen = q.GetChosenOptionIds()
				}
			}
		}
	}
	if len(chosen) != 1 || chosen[0] != "opt-a" {
		t.Fatalf("MessageUpdated ask chosen = %v, want [opt-a]", chosen)
	}
	// The live-stream surface is what a UI consumes to flip an ask from
	// answerable to spent, so the flag is asserted where it actually matters,
	// not only on the ListMessages read.
	if !answered {
		t.Fatal("MessageUpdated ask reads answered=false, want true")
	}
}

// TestPostMessageDropsCallerSuppliedAnswerState pins that answer state is
// server-owned on the way IN. A caller can put anything in an Ask block it
// posts, so askFromWire must drop every field only the server may set:
// answered, and the per-question chosen_option_ids / custom_text / timed_out.
//
// It matters because a client renders a settled ask from those fields. An ask
// posted pre-populated would read as already-answered-and-locked while the
// server still holds it pending and answerable — the mirror of the bug the
// answered flag closes, arriving through the door instead of the window.
// ask_id is asserted alongside them since it is dropped by the same rule.
//
// The keep half is pinned here too: every content field the caller DID supply
// must survive. Over-dropping is as wrong as under-dropping — nothing else
// fails if askFromWire also swallowed header / allow_multiple / recommended or
// a per-option description / preview, so agent-supplied question content would
// vanish with the suite green. The drop/keep boundary is the contract; both
// halves belong in one test.
func TestPostMessageDropsCallerSuppliedAnswerState(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	author := mustUser(t, st, "author")
	ch, err := st.CreateChannel(ctx, author.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	// Every field the server owns, set by the caller to a value it must not
	// keep, alongside every content field, which it must.
	recommended := int32(1)
	posted, err := svc.PostMessage(WithActor(ctx, author.ID), connect.NewRequest(&compassv1.PostMessageRequest{
		Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)},
		Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Ask{Ask: &compassv1.Ask{
			AskId:    "forged-ask-id",
			Answered: true,
			Questions: []*compassv1.AskQuestion{{
				QuestionId: "q1",
				Question:   "Which environment?",
				Header:     "Deploy target",
				Options: []*compassv1.AskOption{
					{Id: "opt-a", Label: "staging", Description: "the safe one", Preview: "staging.example"},
					{Id: "opt-b", Label: "prod", Description: "the scary one", Preview: "prod.example"},
				},
				AllowMultiple:   true,
				Recommended:     &recommended,
				ChosenOptionIds: []string{"opt-a"},
				CustomText:      "forged answer",
				TimedOut:        true,
			}},
		}}}},
	}))
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}

	var got *compassv1.Ask
	for _, b := range posted.Msg.GetMessage().GetBlocks() {
		if a := b.GetAsk(); a != nil {
			got = a
		}
	}
	if got == nil {
		t.Fatal("posted message carries no ask block")
	}
	if got.GetAnswered() {
		t.Error("caller-supplied answered=true was honored, want a pending ask")
	}
	if got.GetAskId() == "forged-ask-id" {
		t.Error("caller-supplied ask_id was honored, want a server-minted id")
	}
	if got.GetAskId() == "" {
		t.Error("server minted no ask id")
	}
	q := got.GetQuestions()[0]
	if len(q.GetChosenOptionIds()) != 0 {
		t.Errorf("caller-supplied chosen_option_ids = %v, want none", q.GetChosenOptionIds())
	}
	if q.GetCustomText() != "" {
		t.Errorf("caller-supplied custom_text = %q, want empty", q.GetCustomText())
	}
	if q.GetTimedOut() {
		t.Error("caller-supplied timed_out=true was honored, want false")
	}

	// The other half of the boundary: every content field the caller supplied
	// survives untouched. Nothing else catches an over-broad drop here.
	if q.GetQuestionId() != "q1" {
		t.Errorf("question_id = %q, want %q", q.GetQuestionId(), "q1")
	}
	if q.GetQuestion() != "Which environment?" {
		t.Errorf("question = %q, want %q", q.GetQuestion(), "Which environment?")
	}
	if q.GetHeader() != "Deploy target" {
		t.Errorf("header = %q, want %q", q.GetHeader(), "Deploy target")
	}
	if !q.GetAllowMultiple() {
		t.Error("allow_multiple = false, want true")
	}
	if q.GetRecommended() != recommended {
		t.Errorf("recommended = %d, want %d", q.GetRecommended(), recommended)
	}
	opts := q.GetOptions()
	if len(opts) != 2 {
		t.Fatalf("options length = %d, want 2", len(opts))
	}
	for i, want := range []*compassv1.AskOption{
		{Id: "opt-a", Label: "staging", Description: "the safe one", Preview: "staging.example"},
		{Id: "opt-b", Label: "prod", Description: "the scary one", Preview: "prod.example"},
	} {
		if got := opts[i]; got.GetId() != want.GetId() || got.GetLabel() != want.GetLabel() ||
			got.GetDescription() != want.GetDescription() || got.GetPreview() != want.GetPreview() {
			t.Errorf("options[%d] = {%q %q %q %q}, want {%q %q %q %q}",
				i, got.GetId(), got.GetLabel(), got.GetDescription(), got.GetPreview(),
				want.GetId(), want.GetLabel(), want.GetDescription(), want.GetPreview())
		}
	}

	// The ask is genuinely still answerable — the drop left real pending state,
	// not a cosmetically-blanked row.
	if _, err := svc.RespondToAsk(WithActor(ctx, author.ID), connect.NewRequest(&compassv1.RespondToAskRequest{
		AskId:   got.GetAskId(),
		Answers: []*compassv1.AskQuestionAnswer{{QuestionId: "q1", ChosenOptionIds: []string{"opt-b"}}},
	})); err != nil {
		t.Fatalf("RespondToAsk on the posted ask: %v", err)
	}
}

// TestAskAnsweredFlagReachesTheWire is the L2 mapping regression for the one
// field that tells a client an ask is spent. The store's Answered flag is the
// ONLY reliable answered-signal — a fully-skipped ask leaves every question's
// answer fields empty, so a client inspecting the questions alone cannot
// distinguish spent from pending. Before this projection existed the flag was
// dropped in askToWire, so a second participant saw an answered ask as
// answerable and fired a RespondToAsk the server is guaranteed to reject with
// ErrConflict, silently discarding their click.
//
// It asserts BOTH poles from the same ask: pending reads false, and the same
// ask reads true after RespondToAsk. Asserting only the true pole would pass
// against a hard-coded `Answered: true`.
func TestAskAnsweredFlagReachesTheWire(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	author := mustUser(t, st, "author")
	ch, err := st.CreateChannel(ctx, author.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, _, err := st.AppendMessage(ctx, store.Message{
		Container: store.ContainerRef{ChannelID: ch.ID}, AuthorAccountID: author.ID,
		Blocks: []store.MessageBlock{pendingAskStore("ask-1")},
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// readAsk pulls ask-1 off the ListMessages wire read — the same projection
	// a UI consumes, not the store row.
	readAsk := func() *compassv1.Ask {
		t.Helper()
		list, err := svc.ListMessages(WithActor(ctx, author.ID), connect.NewRequest(&compassv1.ListMessagesRequest{
			Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: string(ch.ID)},
		}))
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		for _, m := range list.Msg.GetMessages() {
			for _, b := range m.GetBlocks() {
				if a := b.GetAsk(); a != nil && a.GetAskId() == "ask-1" {
					return a
				}
			}
		}
		t.Fatal("ask-1 not found on the wire")
		return nil
	}

	if readAsk().GetAnswered() {
		t.Fatal("pending ask reads answered=true on the wire, want false")
	}

	if _, err := svc.RespondToAsk(WithActor(ctx, author.ID), connect.NewRequest(&compassv1.RespondToAskRequest{
		AskId:   "ask-1",
		Answers: []*compassv1.AskQuestionAnswer{{QuestionId: "q1", ChosenOptionIds: []string{"opt-a"}}},
	})); err != nil {
		t.Fatalf("RespondToAsk: %v", err)
	}

	if !readAsk().GetAnswered() {
		t.Fatal("answered ask reads answered=false on the wire, want true — the flag is not projected")
	}
}

// TestPostMessageThreadsParentMessageIDOverWire is the L2 mapping regression: a
// PostMessage carrying parent_message_id (wire field 5) echoes it on the
// response Message.parent_message_id (field 7) and it survives the ListMessages
// read — proving messageToWire copies ParentMessageID → ParentMessageId end to
// end, not just that the store persists it. A root message round-trips as "".
func TestPostMessageThreadsParentMessageIDOverWire(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	author := mustUser(t, st, "author")
	ch, err := st.CreateChannel(ctx, author.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	// A root post round-trips with an empty parent id on the wire.
	root, err := svc.PostMessage(WithActor(ctx, author.ID), connect.NewRequest(&compassv1.PostMessageRequest{
		Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)},
		Blocks:    []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "root"}}},
	}))
	if err != nil {
		t.Fatalf("PostMessage(root): %v", err)
	}
	rootID := root.Msg.GetMessage().GetId()
	if got := root.Msg.GetMessage().GetParentMessageId(); got != "" {
		t.Fatalf("root response ParentMessageId = %q, want \"\"", got)
	}

	// A reply carrying parent_message_id echoes it on the response Message.
	reply, err := svc.PostMessage(WithActor(ctx, author.ID), connect.NewRequest(&compassv1.PostMessageRequest{
		Container:       &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)},
		Blocks:          []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "reply"}}},
		ParentMessageId: rootID,
	}))
	if err != nil {
		t.Fatalf("PostMessage(reply): %v", err)
	}
	replyID := reply.Msg.GetMessage().GetId()
	if got := reply.Msg.GetMessage().GetParentMessageId(); got != rootID {
		t.Fatalf("reply response ParentMessageId = %q, want the root %q", got, rootID)
	}

	// It survives the read path: ListMessages carries each message's parent id
	// on the wire (the mapping copies it on reads too).
	listed, err := svc.ListMessages(WithActor(ctx, author.ID), connect.NewRequest(&compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: string(ch.ID)},
	}))
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	parents := map[string]string{}
	for _, m := range listed.Msg.GetMessages() {
		parents[m.GetId()] = m.GetParentMessageId()
	}
	if got := parents[replyID]; got != rootID {
		t.Fatalf("listed reply ParentMessageId = %q, want the root %q", got, rootID)
	}
	if got := parents[rootID]; got != "" {
		t.Fatalf("listed root ParentMessageId = %q, want \"\"", got)
	}
}

// TestPostMessageStripsCallerAskID pins Fork 1 (SEA-1243): ask_id is server-
// owned. askFromWire drops any caller-supplied Ask.ask_id, so an ask posted over
// the wire always gets a fresh 32-hex id minted by the store, a caller-forged id
// never survives, two posts carrying the SAME forged id get DISTINCT minted ids
// (uniqueness by construction, not caller-controlled collision), and only the
// minted id addresses the ask for RespondToAsk — the forged id resolves to
// nothing. This kills the nondeterministic multi-row match in RespondToAsk.
func TestPostMessageStripsCallerAskID(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	author := mustUser(t, st, "author")
	ch, err := st.CreateChannel(ctx, author.ID, store.NewChannel{Name: "room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	// A block whose Ask carries a caller-chosen, non-empty ask_id.
	forgedBlock := func() *compassv1.MessageBlock {
		return &compassv1.MessageBlock{Block: &compassv1.MessageBlock_Ask{Ask: &compassv1.Ask{
			AskId: "caller-forged-id",
			Questions: []*compassv1.AskQuestion{{
				QuestionId: "q1",
				Question:   "?",
				Options:    []*compassv1.AskOption{{Id: "opt-a", Label: "a"}},
			}},
		}}}
	}

	// postMinted posts one ask and returns the ask_id on the response Message.
	postMinted := func(ctx string) string {
		resp, err := svc.PostMessage(WithActor(context.Background(), author.ID), connect.NewRequest(&compassv1.PostMessageRequest{
			Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)},
			Blocks:    []*compassv1.MessageBlock{forgedBlock()},
		}))
		if err != nil {
			t.Fatalf("PostMessage(%s): %v", ctx, err)
		}
		var id string
		var count int
		for _, b := range resp.Msg.GetMessage().GetBlocks() {
			if a := b.GetAsk(); a != nil {
				id = a.GetAskId()
				count++
			}
		}
		if count != 1 {
			t.Fatalf("PostMessage(%s): got %d ask blocks on response, want 1", ctx, count)
		}
		return id
	}

	first := postMinted("first")
	// (a) the caller-forged id did not survive, and (b) the returned id has the
	// server-minted shape: 32 hex chars (matching newID(), same as
	// TestAppendMessageMintsAskID's len==32 check).
	if first == "caller-forged-id" {
		t.Fatalf("returned ask_id = %q, want the caller-forged value stripped", first)
	}
	if len(first) != 32 {
		t.Fatalf("minted ask_id = %q (len %d), want 32 hex chars", first, len(first))
	}

	// A second post with the SAME forged id gets a DISTINCT minted id: uniqueness
	// is server-guaranteed, not caller-controlled.
	second := postMinted("second")
	if len(second) != 32 {
		t.Fatalf("second minted ask_id = %q (len %d), want 32 hex chars", second, len(second))
	}
	if second == first {
		t.Fatalf("two posts with the same forged id got the same minted id %q, want distinct", first)
	}

	// End-to-end: the minted id addresses the ask (RespondToAsk succeeds), while
	// the caller-forged id addresses nothing (CodeNotFound). The strip is real,
	// not cosmetic.
	if _, err := svc.RespondToAsk(WithActor(ctx, author.ID), connect.NewRequest(&compassv1.RespondToAskRequest{
		AskId:   first,
		Answers: []*compassv1.AskQuestionAnswer{{QuestionId: "q1", ChosenOptionIds: []string{"opt-a"}}},
	})); err != nil {
		t.Fatalf("RespondToAsk(minted id %q): %v", first, err)
	}
	_, forgedErr := svc.RespondToAsk(WithActor(ctx, author.ID), connect.NewRequest(&compassv1.RespondToAskRequest{
		AskId:   "caller-forged-id",
		Answers: []*compassv1.AskQuestionAnswer{{QuestionId: "q1", ChosenOptionIds: []string{"opt-a"}}},
	}))
	connectCodeIs(t, forgedErr, connect.CodeNotFound, "answer by the stripped caller-forged id")
}

func TestRespondToAskVisibilityCollapseNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()
	alice := mustUser(t, st, "alice")
	bob := mustUser(t, st, "bob")

	chA, err := st.CreateChannel(ctx, alice.ID, store.NewChannel{Name: "alice-room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, _, err := st.AppendMessage(ctx, store.Message{
		Container: store.ContainerRef{ChannelID: chA.ID}, AuthorAccountID: alice.ID,
		Blocks: []store.MessageBlock{pendingAskStore("ask-secret")},
	}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// bob (non-member) answering alice's ask maps to CodeNotFound at the edge —
	// NOT a permission-denied variant, so the ask's existence cannot leak.
	_, err = svc.RespondToAsk(WithActor(ctx, bob.ID), connect.NewRequest(&compassv1.RespondToAskRequest{
		AskId:   "ask-secret",
		Answers: []*compassv1.AskQuestionAnswer{{QuestionId: "q1", ChosenOptionIds: []string{"opt-a"}}},
	}))
	connectCodeIs(t, err, connect.CodeNotFound, "answer a non-visible ask")

	// Indistinguishable from a genuinely nonexistent ask id.
	_, ghostErr := svc.RespondToAsk(WithActor(ctx, bob.ID), connect.NewRequest(&compassv1.RespondToAskRequest{
		AskId:   "ask-nonexistent",
		Answers: []*compassv1.AskQuestionAnswer{{QuestionId: "q1", ChosenOptionIds: []string{"opt-a"}}},
	}))
	connectCodeIs(t, ghostErr, connect.CodeNotFound, "answer a nonexistent ask")
}

func TestEdgeErrorMapping(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	// ErrInvalidArgument -> CodeInvalidArgument: an empty required handle.
	_, err := svc.CreateUser(ctx, connect.NewRequest(&compassv1.CreateUserRequest{DisplayName: "no handle"}))
	connectCodeIs(t, err, connect.CodeInvalidArgument, "create user with empty handle")

	// ErrConflict -> CodeAlreadyExists: a duplicate handle.
	if _, err := st.CreateUser(ctx, store.NewUser{Handle: "dup", DisplayName: "First"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	_, dupErr := svc.CreateUser(ctx, connect.NewRequest(&compassv1.CreateUserRequest{Handle: "dup", DisplayName: "Second"}))
	connectCodeIs(t, dupErr, connect.CodeAlreadyExists, "create user with duplicate handle")

	// ErrNotFound -> CodeNotFound: mutating an unknown channel.
	_, nfErr := svc.UpdateChannelMembers(ctx, connect.NewRequest(&compassv1.UpdateChannelMembersRequest{
		ChannelId:           "ghost-channel",
		AddMemberAccountIds: []string{"whoever"},
	}))
	connectCodeIs(t, nfErr, connect.CodeNotFound, "update members of an unknown channel")
}

// pendingAskStore builds a pending (unanswered) store ask block offering opt-a
// and opt-b, single-select, for the handler ask tests.
func pendingAskStore(id string) store.MessageBlock {
	return store.MessageBlock{Ask: &store.Ask{
		AskID: id,
		Questions: []store.AskQuestion{{
			QuestionID: "q1",
			Question:   "Which environment?",
			Options: []store.AskOption{
				{ID: "opt-a", Label: "staging"},
				{ID: "opt-b", Label: "prod"},
			},
		}},
	}}
}

func ptr(s string) *string { return &s }

func containsString(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
