//go:build pgtest && unix

package comms

// The conversation write-through (RIG-1364 T3): CommitAgentPost / CommitAgentUpdate
// turn a relayed agent conversation frame into a durable comms row under the
// account the RunnerHub resolved the session to. Every test drives the real
// Postgres store + real bus (the newHandler / newStreamHarness harnesses the rest
// of this package uses — no mocks) and defends one contract:
//
//   - a posted frame lands on the agent's HOME channel, attributed to the AGENT
//     account, and fans out on SubscribeComms — indistinguishable downstream from
//     a human post, because it IS the PostMessage handler path;
//   - an updated frame edits the row in place through the AUTHORIZING store
//     update, and fans out as MessageUpdated;
//   - a cross-account update, an update from a revoked member, an ask block with
//     no id, and an empty message.id are each REFUSED with the code a human
//     caller would get — the refusals the hub then treats as non-fatal;
//   - an empty account is a hard CodeInvalidArgument on both methods, never a
//     silent fall-through to bootstrap-admin attribution.
//
// Gated `pgtest && unix` like agent_caller_pgtest_test.go: it SKIPs (via
// pgtest.RequireDSN in newTestStore) when no Postgres/podman runtime exists, and
// `unix` because agent_caller.go is unix-tagged.

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// postedFrame wraps blocks in the MessagePosted frame shape the Runner relays.
// The Container oneof is deliberately left UNSET — that is what routes the
// commit to the agent's home channel (defaultChannel, agent_caller.go:109).
func postedFrame(blocks []*compassv1.MessageBlock) *compassv1.MessagePosted {
	return &compassv1.MessagePosted{Message: &compassv1.Message{Blocks: blocks}}
}

// updatedFrame wraps an addressed message id + blocks in the MessageUpdated
// frame shape. Unlike a post, an update MUST carry the row's id.
func updatedFrame(id string, blocks []*compassv1.MessageBlock) *compassv1.MessageUpdated {
	return &compassv1.MessageUpdated{Message: &compassv1.Message{Id: id, Blocks: blocks}}
}

// askBlockWire builds a wire ask block carrying one question and NO ask_id — the
// id-less shape a genuinely malformed update frame carries (an update must carry
// back the id the append minted; an ask block with no id at all is a malformed
// update, distinct from one whose id merely disagrees with the stored row).
func askBlockWire() *compassv1.MessageBlock {
	return askBlockWireID("")
}

// askBlockWireID builds a wire ask block carrying one question and the given
// ask_id — used to relay an UPDATE that carries the id the append minted (the
// legitimate ask-bearing update) or a forged one that disagrees with the stored
// row.
func askBlockWireID(askID string) *compassv1.MessageBlock {
	return &compassv1.MessageBlock{Block: &compassv1.MessageBlock_Ask{Ask: &compassv1.Ask{
		AskId: askID,
		Questions: []*compassv1.AskQuestion{{
			QuestionId: "q1",
			Question:   "Which environment?",
			Options: []*compassv1.AskOption{
				{Id: "opt-a", Label: "staging"},
				{Id: "opt-b", Label: "prod"},
			},
		}},
	}}}
}

// A relayed posted frame becomes a real Message row on the agent's HOME channel,
// authored by the AGENT account. This is the core dogfood contract the stub
// broke: the frame was acked as committed and then discarded, so nothing landed.
//
// Mutation that reddens it: dropping the AppendMessage delegation (back to a log
// line) → the read-back finds no row; hardcoding an actor → the author assertion
// fails.
func TestCommitAgentPostLandsOnHomeChannelAsTheAgent(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")

	resp, err := svc.CommitAgentPost(ctx, agent.ID, postedFrame(textBlocks("the agent speaks")))
	if err != nil {
		t.Fatalf("CommitAgentPost: %v", err)
	}
	// A message carries only its topic now; that it landed in the HOME channel is
	// proven by the home-scoped read-back below (the channel is resolved through
	// the topic server-side). The unset Container+Topic must route it to the home
	// channel's home topic.
	if got := resp.GetMessage().GetTopicId(); got == "" {
		t.Fatal("committed message TopicId = \"\", want the home channel's home topic id")
	}
	if got := resp.GetMessage().GetAuthorAccountId(); got != string(agent.ID) {
		t.Fatalf("committed message author = %q, want the agent account %q (never the bootstrap admin)", got, agent.ID)
	}

	// Durable: the row is readable back out of the store of record.
	msgs, err := st.ListMessages(ctx, store.ListMessagesQuery{Actor: agent.ID, ChannelID: agent.Agent.HomeChannelID})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("home channel holds %d messages, want exactly 1 (the relayed frame must COMMIT, not be observed)", len(msgs))
	}
	if msgs[0].ID != store.MessageID(resp.GetMessage().GetId()) {
		t.Fatalf("stored message id = %q, want the returned %q", msgs[0].ID, resp.GetMessage().GetId())
	}
}

// The fan-out half: a committed post reaches a live SubscribeComms subscriber as
// MessagePosted, so a relayed agent turn is visible to a watching client exactly
// as a human post is. Driven over the real stream (newStreamHarness) with the
// subscribe running concurrently with the commit — connect server-streaming over
// HTTP/1 is half-duplex, so the subscribe must not precede the mutation.
func TestCommitAgentPostFansOutOnSubscribeComms(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()

	owner := mustUser(t, h.store, "owner")
	agent := mustAgent(t, h.store, owner.ID, "agent")

	// Subscribe AS the agent: it is a member of its own home channel, so the D9
	// stream filter passes the event to it.
	events := firstEventAfterBoundary(t, h, agent.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})

	resp, err := h.svc.CommitAgentPost(ctx, agent.ID, postedFrame(textBlocks("fanned out")))
	if err != nil {
		t.Fatalf("CommitAgentPost: %v", err)
	}

	got := awaitFirst(t, events)
	posted := got.GetMessagePosted()
	if posted == nil {
		t.Fatalf("stream event payload = %T, want a MessagePosted", got.GetPayload())
	}
	if posted.GetMessage().GetId() != resp.GetMessage().GetId() {
		t.Fatalf("fanned message id = %q, want the committed %q", posted.GetMessage().GetId(), resp.GetMessage().GetId())
	}
	if got := posted.GetMessage().GetAuthorAccountId(); got != string(agent.ID) {
		t.Fatalf("fanned message author = %q, want the agent account %q", got, agent.ID)
	}
}

// A relayed updated frame edits the addressed row IN PLACE — same id, new blocks,
// no second row — and fans out as MessageUpdated. This is the streaming-turn
// path: the agent appends a block and re-sends the full current set.
func TestCommitAgentUpdateEditsInPlaceAndFansOut(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()

	owner := mustUser(t, h.store, "owner")
	agent := mustAgent(t, h.store, owner.ID, "agent")

	// Seed the row to be edited through the STORE, not CommitAgentPost: a post
	// publishes MessagePosted onto the bus, and a since_seq=0 subscribe replays
	// it, so the update's own fan-out would not be the first event. Appending
	// directly writes the row without an event, isolating the assertion to the
	// MessageUpdated this test is about.
	seedText := "partial"
	seed, _, err := h.store.AppendMessage(ctx, store.Message{
		AuthorAccountID: agent.ID,
		Blocks:          []store.MessageBlock{{Text: &seedText}},
	}, string(agent.Agent.HomeChannelID), store.TopicRef{Name: "general", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage(seed): %v", err)
	}
	id := string(seed.ID)

	events := firstEventAfterBoundary(t, h, agent.ID, &compassv1.SubscribeCommsRequest{SinceSeq: 0})

	updated, err := h.svc.CommitAgentUpdate(ctx, agent.ID, updatedFrame(id, textBlocks("partial and settled")))
	if err != nil {
		t.Fatalf("CommitAgentUpdate: %v", err)
	}
	if updated.GetMessage().GetId() != id {
		t.Fatalf("updated message id = %q, want the edited row %q (an update must not insert)", updated.GetMessage().GetId(), id)
	}

	got := awaitFirst(t, events)
	if ev := got.GetMessageUpdated(); ev == nil {
		t.Fatalf("stream event payload = %T, want a MessageUpdated", got.GetPayload())
	} else if ev.GetMessage().GetId() != id {
		t.Fatalf("fanned updated id = %q, want %q", ev.GetMessage().GetId(), id)
	}

	// Exactly one row, carrying the new text.
	msgs, err := h.store.ListMessages(ctx, store.ListMessagesQuery{Actor: agent.ID, ChannelID: agent.Agent.HomeChannelID})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("home channel holds %d messages after an update, want exactly 1 (in-place edit)", len(msgs))
	}
	if msgs[0].Blocks[0].Text == nil || *msgs[0].Blocks[0].Text != "partial and settled" {
		t.Fatalf("stored text = %v, want the updated body", msgs[0].Blocks[0].Text)
	}
}

// THE cross-account security case at the comms seam: agent B cannot edit agent
// A's message, even in a channel B can read. It collapses to CodeNotFound — the
// same answer B gets for a message that does not exist — so B cannot enumerate
// A's messages by id. The positive control (A editing its own row) keeps this
// from passing vacuously.
//
// Mutation: route this through updateMessageBlocksExec (the bare-id, no-authz
// core this fork exists to avoid) → B's edit succeeds and BOTH the error and the
// untouched-content assertions redden.
func TestCommitAgentUpdateCrossAccountIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agentA := mustAgent(t, st, owner.ID, "agent-a")
	agentB := mustAgent(t, st, owner.ID, "agent-b")

	// A shared channel both agents belong to, so B can READ A's message: only
	// authorship refuses the edit.
	ch, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name: "shared", Kind: store.ChannelKindChannel,
		MemberAccountIDs: []store.AccountID{agentA.ID, agentB.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	postedA, err := svc.PostAsAccount(ctx, agentA.ID, &compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, CreateTopic: true, Blocks: textBlocks("agent A's words")})
	if err != nil {
		t.Fatalf("PostAsAccount(A): %v", err)
	}
	id := postedA.GetMessage().GetId()

	_, err = svc.CommitAgentUpdate(ctx, agentB.ID, updatedFrame(id, textBlocks("put into A's mouth")))
	connectCodeIs(t, err, connect.CodeNotFound, "CommitAgentUpdate(cross-account)")

	// Positive control: A can edit its own row, so B's refusal is authorship.
	if _, err := svc.CommitAgentUpdate(ctx, agentA.ID, updatedFrame(id, textBlocks("agent A's own edit"))); err != nil {
		t.Fatalf("agent A cannot edit its own message: %v", err)
	}

	msgs, err := st.ListMessages(ctx, store.ListMessagesQuery{Actor: agentA.ID, ChannelID: ch.ID})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if msgs[0].Blocks[0].Text == nil || *msgs[0].Blocks[0].Text != "agent A's own edit" {
		t.Fatalf("stored text = %v, want A's own edit (B's rejected update must leave no trace)", msgs[0].Blocks[0].Text)
	}
}

// The membership half at the comms seam: an agent removed from the channel it
// posted in can no longer edit its own past message — CodeNotFound, so write
// access dies with read access. A bare authorship check would let this through.
func TestCommitAgentUpdateRevokedMemberIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")
	ch, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name: "room", Kind: store.ChannelKindChannel,
		MemberAccountIDs: []store.AccountID{agent.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	posted, err := svc.PostAsAccount(ctx, agent.ID, &compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, CreateTopic: true, Blocks: textBlocks("posted while a member")})
	if err != nil {
		t.Fatalf("PostAsAccount: %v", err)
	}
	id := posted.GetMessage().GetId()

	// Positive control before the revoke.
	if _, err := svc.CommitAgentUpdate(ctx, agent.ID, updatedFrame(id, textBlocks("edited while a member"))); err != nil {
		t.Fatalf("the agent cannot edit while still a member: %v", err)
	}

	if _, _, err := st.UpdateChannelMembers(ctx, owner.ID, ch.ID, []store.MemberUpdate{{AccountID: agent.ID, Remove: true}}, store.MemberUpdatesOptions{}); err != nil {
		t.Fatalf("UpdateChannelMembers(remove): %v", err)
	}

	_, err = svc.CommitAgentUpdate(ctx, agent.ID, updatedFrame(id, textBlocks("edited after the revoke")))
	connectCodeIs(t, err, connect.CodeNotFound, "CommitAgentUpdate(revoked member)")

	msgs, err := st.ListMessages(ctx, store.ListMessagesQuery{Actor: owner.ID, ChannelID: ch.ID})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if msgs[0].Blocks[0].Text == nil || *msgs[0].Blocks[0].Text != "edited while a member" {
		t.Fatalf("stored text = %v, want the pre-revoke text", msgs[0].Blocks[0].Text)
	}
}

// The two malformed-input refusals, both CodeInvalidArgument: an update whose
// message.id is empty (a frame the agent never stamped) and an update carrying
// an ask block with NO ask_id that has no stored counterpart to reconcile it
// from — a genuinely id-less frame, distinct from an ask-bearing update carrying
// its stored id (which now PERSISTS, see TestCommitAgentUpdatePersistsAskBlock)
// and from one whose id merely disagrees with the stored row (see
// TestCommitAgentUpdateRejectsAskIDMismatch). An id-less ask that cannot be
// reconciled is still an error, because the store's immutable-ask_id contract
// requires an update to carry the id the append minted.
func TestCommitAgentUpdateRejectsMalformedFrames(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")
	posted, err := svc.CommitAgentPost(ctx, agent.ID, postedFrame(textBlocks("untouched")))
	if err != nil {
		t.Fatalf("CommitAgentPost: %v", err)
	}

	t.Run("empty message id", func(t *testing.T) {
		_, err := svc.CommitAgentUpdate(ctx, agent.ID, updatedFrame("", textBlocks("nowhere to land")))
		connectCodeIs(t, err, connect.CodeInvalidArgument, "CommitAgentUpdate(empty message id)")
	})
	t.Run("ask block with no ask id", func(t *testing.T) {
		// The seeded row is text-only, so an ask block on the update has NO
		// stored counterpart for reconcileUpdateAskIDs to fill its id from: it
		// stays id-less and the store rejects it. This pins the truly id-less
		// case (an update that cannot carry the immutable id the append would
		// have minted), NOT that ask-bearing updates are unsupported — one
		// carrying its stored id persists (TestCommitAgentUpdatePersistsAskBlock).
		_, err := svc.CommitAgentUpdate(ctx, agent.ID,
			updatedFrame(posted.GetMessage().GetId(), []*compassv1.MessageBlock{askBlockWire()}))
		connectCodeIs(t, err, connect.CodeInvalidArgument, "CommitAgentUpdate(ask with no ask_id)")
	})

	// Neither refusal wrote anything: the original row is intact.
	msgs, err := st.ListMessages(ctx, store.ListMessagesQuery{Actor: agent.ID, ChannelID: agent.Agent.HomeChannelID})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("home channel holds %d messages, want 1", len(msgs))
	}
	if msgs[0].Blocks[0].Text == nil || *msgs[0].Blocks[0].Text != "untouched" {
		t.Fatalf("stored text = %v, want the untouched original", msgs[0].Blocks[0].Text)
	}
}

// Fail-closed identity on BOTH write-through methods, mirroring
// TestPostAsAccountEmptyAccountFailsClosedNoAdminWrite for this leg: an empty
// resolved account is a hard CodeInvalidArgument and writes NOTHING. Without the
// guard, actorFromContext's admin fallback (comms.go:331-336) would attribute the
// agent's words to the bootstrap admin — the exact silent misattribution the
// fail-closed posture exists to prevent.
func TestCommitAgentFramesEmptyAccountFailsClosed(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")
	seeded, err := svc.CommitAgentPost(ctx, agent.ID, postedFrame(textBlocks("the agent's own")))
	if err != nil {
		t.Fatalf("CommitAgentPost(seed): %v", err)
	}

	_, err = svc.CommitAgentPost(ctx, "", postedFrame(textBlocks("should never be written")))
	connectCodeIs(t, err, connect.CodeInvalidArgument, "CommitAgentPost(empty account)")

	_, err = svc.CommitAgentUpdate(ctx, "", updatedFrame(seeded.GetMessage().GetId(), textBlocks("should never be applied")))
	connectCodeIs(t, err, connect.CodeInvalidArgument, "CommitAgentUpdate(empty account)")

	// Nothing was written or altered under any fallback identity.
	msgs, err := st.ListMessages(ctx, store.ListMessagesQuery{Actor: agent.ID, ChannelID: agent.Agent.HomeChannelID})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("home channel holds %d messages, want exactly 1 (the unresolved calls must write nothing)", len(msgs))
	}
	if msgs[0].Blocks[0].Text == nil || *msgs[0].Blocks[0].Text != "the agent's own" {
		t.Fatalf("stored text = %v, want the agent's own seeded text", msgs[0].Blocks[0].Text)
	}
}

// Idempotent dedup at the seam the relayed key WILL enter. The relayed frame
// carries no idempotency key on this base — PublishEventsRequest is
// {runner_seq, session_id, frame} (runner.proto:169-183) and the agent-minted
// key terminates at the Runner (agent_gateway.proto:113-120) — so the
// write-through leaves ClientRequestId unset and this contract cannot be
// exercised THROUGH a relayed frame yet. It is pinned here, on the
// PostMessageRequest the write-through builds and delegates, because that is the
// exact field #894/T2 populates: when the key arrives, this test is already the
// proof that a retry commits no second row.
func TestCommitAgentPostRequestIsDedupedByClientRequestID(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")

	// The request shape CommitAgentPost builds (home-channel default via an
	// unset Container), plus the key T2 will thread onto it.
	post := func(text string) (*compassv1.PostMessageResponse, error) {
		return svc.PostAsAccount(ctx, agent.ID, &compassv1.PostMessageRequest{Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, CreateTopic: true, Blocks: textBlocks(text), ClientRequestId: "relayed-frame-1"})
	}
	first, err := post("the turn")
	if err != nil {
		t.Fatalf("first commit: %v", err)
	}
	retry, err := post("the turn")
	if err != nil {
		t.Fatalf("retry commit: %v", err)
	}
	if first.GetMessage().GetId() != retry.GetMessage().GetId() {
		t.Fatalf("retry stored a new message %q, want the first's %q (dedup on (author, client_request_id))", retry.GetMessage().GetId(), first.GetMessage().GetId())
	}

	msgs, err := st.ListMessages(ctx, store.ListMessagesQuery{Actor: agent.ID, ChannelID: agent.Agent.HomeChannelID})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("home channel holds %d messages after a keyed retry, want exactly 1", len(msgs))
	}
}

// A relayed frame with no topic routing lands in the agent's home channel's home
// topic: the committed row records that topic, and two frames from the same agent
// share it. Threading is by topic now, not a parent pointer — a frame carries no
// parent and no topic, so CommitAgentPost's unset Container+Topic route it to the
// home topic (store.AppendMessage), which is what keeps a relayed agent's turns
// in one conversation.
//
// Mutation: route CommitAgentPost's request to a non-home channel/topic → the two
// frames land in different topics and the shared-topic assertion reddens.
func TestCommitAgentPostLandsInHomeTopic(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")

	first, err := svc.CommitAgentPost(ctx, agent.ID, postedFrame(textBlocks("the question")))
	if err != nil {
		t.Fatalf("CommitAgentPost(first): %v", err)
	}
	topicID := first.GetMessage().GetTopicId()
	if topicID == "" {
		t.Fatal("first committed message TopicId = \"\", want the home topic id")
	}

	second, err := svc.CommitAgentPost(ctx, agent.ID, postedFrame(textBlocks("the follow-up")))
	if err != nil {
		t.Fatalf("CommitAgentPost(second): %v", err)
	}
	if got := second.GetMessage().GetTopicId(); got != topicID {
		t.Fatalf("second committed message TopicId = %q, want the same home topic %q", got, topicID)
	}

	// Durable, not merely echoed: the store of record holds both rows under the
	// one home topic.
	msgs, err := st.ListMessages(ctx, store.ListMessagesQuery{Actor: agent.ID, ChannelID: agent.Agent.HomeChannelID})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	topics := map[store.MessageID]string{}
	for _, m := range msgs {
		topics[m.ID] = m.TopicID
	}
	if got := topics[store.MessageID(first.GetMessage().GetId())]; got != topicID {
		t.Fatalf("stored first TopicID = %q, want the home topic %q", got, topicID)
	}
	if got := topics[store.MessageID(second.GetMessage().GetId())]; got != topicID {
		t.Fatalf("stored second TopicID = %q, want the home topic %q", got, topicID)
	}
}

// A session bound to a NON-AGENT account is refused, not a panic. homeChannel
// reads acc.Agent.HomeChannelID, and scanAccount (store/accounts.go:313-322) sets
// acc.Agent only for agent accounts — for a user account it is nil, so before
// this guard the deref panicked inside the PublishEvents handler goroutine,
// taking the relay down with it (rule://go-no-panic-in-lib).
//
// It is a CONTRACT DEFECT rather than an ordinary refusal, hence
// CodeFailedPrecondition: a user-account binding is not a request the caller
// could pose differently, it is the hub and the store disagreeing about what a
// session resolves to. The hub counts it separately and logs it as a
// misconfiguration (runnerhub/hub.go, isContractDefect) instead of burying it
// among expected per-frame refusals.
//
// Both write-through methods and both *AsAccount methods are covered: every one
// of them reaches homeChannel through the empty-container/empty-channel default,
// so a guard on only one call site would leave the panic reachable.
func TestAgentCallsWithANonAgentAccountAreRefusedNotPanics(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	// A plain USER account: real and resolvable, but acc.Agent is nil.
	user := mustUser(t, st, "human")

	_, err := svc.CommitAgentPost(ctx, user.ID, postedFrame(textBlocks("never committed")))
	connectCodeIs(t, err, connect.CodeFailedPrecondition, "CommitAgentPost(user account)")

	_, err = svc.PostAsAccount(ctx, user.ID, &compassv1.PostMessageRequest{Blocks: textBlocks("never committed")})
	connectCodeIs(t, err, connect.CodeFailedPrecondition, "PostAsAccount(user account, empty channel)")

	_, err = svc.ListAsAccount(ctx, user.ID, &compassv1.ListMessagesRequest{})
	connectCodeIs(t, err, connect.CodeFailedPrecondition, "ListAsAccount(user account, empty channel)")
}

// THE positive contract this fix restores: an addressed conversation UPDATE
// carrying an ask block WITH the stored ask_id PERSISTS, rather than being
// refused. Before the fix, updateBlocksFromWire did not exist and the update
// went through blocksFromWire -> askFromWire, which unconditionally stripped the
// ask_id, so UpdateMessageBlocksAsAuthor rejected the id-less ask as
// CodeInvalidArgument and the update never committed. Now the update path
// preserves the wire ask_id and reconciles it against the stored row, so the
// ask survives the write and round-trips its id.
//
// Mutation that reddens it: routing the update back through blocksFromWire (the
// POST mapper) reintroduces the strip and the ask is refused again.
func TestCommitAgentUpdatePersistsAskBlock(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")

	// Seed a real ask through the POST path so the store mints the ask_id the
	// update must carry back. The minted id is read off the committed row.
	posted, err := svc.CommitAgentPost(ctx, agent.ID,
		postedFrame([]*compassv1.MessageBlock{askBlockWire()}))
	if err != nil {
		t.Fatalf("CommitAgentPost(ask): %v", err)
	}
	id := posted.GetMessage().GetId()
	storedAskID := posted.GetMessage().GetBlocks()[0].GetAsk().GetAskId()
	if storedAskID == "" {
		t.Fatalf("post did not mint an ask_id; got %+v", posted.GetMessage().GetBlocks())
	}

	// The streaming turn re-sends the full block set: a new text block plus the
	// SAME ask, carrying the id the append minted. This is the frame that used
	// to be refused.
	updated, err := svc.CommitAgentUpdate(ctx, agent.ID, updatedFrame(id,
		[]*compassv1.MessageBlock{
			{Block: &compassv1.MessageBlock_Text{Text: "settled turn"}},
			askBlockWireID(storedAskID),
		}))
	if err != nil {
		t.Fatalf("CommitAgentUpdate(ask with stored ask_id) was refused, want persisted: %v", err)
	}
	if got := updated.GetMessage().GetBlocks(); len(got) != 2 {
		t.Fatalf("updated message has %d blocks, want 2 (text + ask)", len(got))
	} else if got[1].GetAsk().GetAskId() != storedAskID {
		t.Fatalf("updated ask_id = %q, want the preserved %q", got[1].GetAsk().GetAskId(), storedAskID)
	}

	// Durable: the row on the home channel carries the updated set and the same
	// ask_id, so a pending RespondToAsk still correlates.
	msgs, err := st.ListMessages(ctx, store.ListMessagesQuery{Actor: agent.ID, ChannelID: agent.Agent.HomeChannelID})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("home channel holds %d messages, want 1 (in-place edit)", len(msgs))
	}
	blocks := msgs[0].Blocks
	if len(blocks) != 2 || blocks[0].Text == nil || *blocks[0].Text != "settled turn" {
		t.Fatalf("stored blocks = %+v, want [text 'settled turn', ask]", blocks)
	}
	if blocks[1].Ask == nil || blocks[1].Ask.AskID != storedAskID {
		t.Fatalf("stored ask_id = %v, want the preserved %q", blocks[1].Ask, storedAskID)
	}
}

// The distinct misclassification the review flagged: an ask-bearing update that
// carries a NON-EMPTY ask_id disagreeing with the stored one is a forged/
// malformed frame, refused CodeInvalidArgument — and refused by
// reconcileUpdateAskIDs's own mismatch error, never conflated with the generic
// id-less case and never silently overwriting the stored id. It stays
// CodeInvalidArgument (still a caller error), but a distinct, clearly-messaged
// one.
//
// Mutation that reddens it: blindly trusting the wire ask_id (dropping the
// mismatch branch) lets the forged id through and the refusal disappears.
func TestCommitAgentUpdateRejectsAskIDMismatch(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")
	posted, err := svc.CommitAgentPost(ctx, agent.ID,
		postedFrame([]*compassv1.MessageBlock{askBlockWire()}))
	if err != nil {
		t.Fatalf("CommitAgentPost(ask): %v", err)
	}
	id := posted.GetMessage().GetId()
	storedAskID := posted.GetMessage().GetBlocks()[0].GetAsk().GetAskId()
	_, err = svc.CommitAgentUpdate(ctx, agent.ID, updatedFrame(id,
		[]*compassv1.MessageBlock{askBlockWireID("forged-" + storedAskID)}))
	connectCodeIs(t, err, connect.CodeInvalidArgument, "CommitAgentUpdate(ask_id mismatch)")
	// The classification must be the DISTINCT mismatch, not the generic id-less
	// refusal ("ask has no ask_id"): asserting the code alone would pass against
	// the old strip-everything behavior too, which is the misclassification the
	// review flagged. Pinning the message keeps the two errors distinct.
	if err == nil || !strings.Contains(err.Error(), "does not match the stored ask_id") {
		t.Fatalf("mismatch error = %v, want the distinct 'does not match the stored ask_id' classification", err)
	}

	// The forged update left no trace: the row still carries the original ask
	// with its minted id, so RespondToAsk against that id still resolves.
	msgs, err := st.ListMessages(ctx, store.ListMessagesQuery{Actor: agent.ID, ChannelID: agent.Agent.HomeChannelID})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].Blocks) != 1 || msgs[0].Blocks[0].Ask == nil {
		t.Fatalf("stored blocks = %+v, want the single untouched ask", msgs[0].Blocks)
	}
	if msgs[0].Blocks[0].Ask.AskID != storedAskID {
		t.Fatalf("stored ask_id = %q, want the untouched %q", msgs[0].Blocks[0].Ask.AskID, storedAskID)
	}
}

// The forgery vector the review flagged: a relayed UPDATE that appends a SURPLUS
// ask block (beyond the stored ask count) carrying a caller-chosen non-empty
// ask_id must be refused, not persisted. A surplus ask has no stored counterpart
// to reconcile against, and an update cannot introduce a new ask (ask_id is
// minted only on POST), so accepting a caller-supplied id would reopen exactly
// the collision askFromWire strips to prevent on POST: a forged id shared with
// another message makes RespondToAsk's containment SELECT match both rows. The
// store guards only the empty-id case, so the edge must reject the non-empty
// surplus.
//
// Mutation that reddens it: skipping surplus asks in reconcileUpdateAskIDs (the
// pre-fix `if askIdx < len(storedAskIDs)` gate) lets the forged id through and
// the row is silently updated with it.
func TestCommitAgentUpdateRejectsSurplusForgedAsk(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")
	posted, err := svc.CommitAgentPost(ctx, agent.ID,
		postedFrame([]*compassv1.MessageBlock{askBlockWire()}))
	if err != nil {
		t.Fatalf("CommitAgentPost(ask): %v", err)
	}
	id := posted.GetMessage().GetId()
	storedAskID := posted.GetMessage().GetBlocks()[0].GetAsk().GetAskId()

	// Re-send the stored ask (correctly carrying its minted id) AND a surplus
	// ask carrying a forged, caller-chosen id. The first reconciles cleanly; the
	// surplus has no stored counterpart and must be rejected.
	_, err = svc.CommitAgentUpdate(ctx, agent.ID, updatedFrame(id,
		[]*compassv1.MessageBlock{
			askBlockWireID(storedAskID),
			askBlockWireID("forged-surplus-ask"),
		}))
	connectCodeIs(t, err, connect.CodeInvalidArgument, "CommitAgentUpdate(surplus forged ask)")
	if err == nil || !strings.Contains(err.Error(), "an update cannot introduce a new ask") {
		t.Fatalf("surplus error = %v, want the distinct 'an update cannot introduce a new ask' classification", err)
	}

	// The forged surplus left no trace: the row still carries only the original
	// ask with its minted id, and the forged id was never persisted.
	msgs, err := st.ListMessages(ctx, store.ListMessagesQuery{Actor: agent.ID, ChannelID: agent.Agent.HomeChannelID})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].Blocks) != 1 || msgs[0].Blocks[0].Ask == nil {
		t.Fatalf("stored blocks = %+v, want the single untouched ask", msgs[0].Blocks)
	}
	if msgs[0].Blocks[0].Ask.AskID != storedAskID {
		t.Fatalf("stored ask_id = %q, want the untouched %q", msgs[0].Blocks[0].Ask.AskID, storedAskID)
	}
}
