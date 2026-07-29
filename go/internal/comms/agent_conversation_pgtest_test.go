//go:build pgtest && unix

package comms

// The conversation write-through (SEA-1364 T3): CommitAgentPost / CommitAgentUpdate
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
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
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

// askBlockWire builds a wire ask block carrying one question, for the malformed
// -ask rejection case (the id an update must carry is the store's, minted at
// append; a wire ask always enters id-less, so an UPDATE carrying one is
// necessarily missing its immutable id).
func askBlockWire() *compassv1.MessageBlock {
	return &compassv1.MessageBlock{Block: &compassv1.MessageBlock_Ask{Ask: &compassv1.Ask{
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
	if got := resp.GetMessage().GetChannelId(); got != string(agent.Agent.HomeChannelID) {
		t.Fatalf("committed message channel = %q, want the agent's home channel %q (the Container oneof must be left unset)", got, agent.Agent.HomeChannelID)
	}
	if got := resp.GetMessage().GetAuthorAccountId(); got != string(agent.ID) {
		t.Fatalf("committed message author = %q, want the agent account %q (never the bootstrap admin)", got, agent.ID)
	}

	// Durable: the row is readable back out of the store of record.
	msgs, err := st.ListMessages(ctx, agent.ID, store.ContainerRef{ChannelID: agent.Agent.HomeChannelID}, store.Page{})
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
		Container:       store.ContainerRef{ChannelID: agent.Agent.HomeChannelID},
		AuthorAccountID: agent.ID,
		Blocks:          []store.MessageBlock{{Text: &seedText}},
	}, "")
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
	msgs, err := h.store.ListMessages(ctx, agent.ID, store.ContainerRef{ChannelID: agent.Agent.HomeChannelID}, store.Page{})
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
	postedA, err := svc.PostAsAccount(ctx, agentA.ID, &compassv1.PostMessageRequest{
		Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)},
		Blocks:    textBlocks("agent A's words"),
	})
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

	msgs, err := st.ListMessages(ctx, agentA.ID, store.ContainerRef{ChannelID: ch.ID}, store.Page{})
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
	posted, err := svc.PostAsAccount(ctx, agent.ID, &compassv1.PostMessageRequest{
		Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)},
		Blocks:    textBlocks("posted while a member"),
	})
	if err != nil {
		t.Fatalf("PostAsAccount: %v", err)
	}
	id := posted.GetMessage().GetId()

	// Positive control before the revoke.
	if _, err := svc.CommitAgentUpdate(ctx, agent.ID, updatedFrame(id, textBlocks("edited while a member"))); err != nil {
		t.Fatalf("the agent cannot edit while still a member: %v", err)
	}

	if _, _, err := st.UpdateChannelMembers(ctx, owner.ID, ch.ID, []store.MemberUpdate{{AccountID: agent.ID, Remove: true}}); err != nil {
		t.Fatalf("UpdateChannelMembers(remove): %v", err)
	}

	_, err = svc.CommitAgentUpdate(ctx, agent.ID, updatedFrame(id, textBlocks("edited after the revoke")))
	connectCodeIs(t, err, connect.CodeNotFound, "CommitAgentUpdate(revoked member)")

	msgs, err := st.ListMessages(ctx, owner.ID, store.ContainerRef{ChannelID: ch.ID}, store.Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if msgs[0].Blocks[0].Text == nil || *msgs[0].Blocks[0].Text != "edited while a member" {
		t.Fatalf("stored text = %v, want the pre-revoke text", msgs[0].Blocks[0].Text)
	}
}

// The two malformed-input refusals, both CodeInvalidArgument: an update whose
// message.id is empty (a frame the agent never stamped) and an update carrying
// an ask block with no ask_id (which would orphan a pending RespondToAsk).
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
		// A wire ask always enters id-less (askFromWire strips any caller value),
		// so an UPDATE carrying one is necessarily missing the immutable id the
		// store minted at append.
		_, err := svc.CommitAgentUpdate(ctx, agent.ID,
			updatedFrame(posted.GetMessage().GetId(), []*compassv1.MessageBlock{askBlockWire()}))
		connectCodeIs(t, err, connect.CodeInvalidArgument, "CommitAgentUpdate(ask with no ask_id)")
	})

	// Neither refusal wrote anything: the original row is intact.
	msgs, err := st.ListMessages(ctx, agent.ID, store.ContainerRef{ChannelID: agent.Agent.HomeChannelID}, store.Page{})
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
	msgs, err := st.ListMessages(ctx, agent.ID, store.ContainerRef{ChannelID: agent.Agent.HomeChannelID}, store.Page{})
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
		return svc.PostAsAccount(ctx, agent.ID, &compassv1.PostMessageRequest{
			Blocks:          textBlocks(text),
			ClientRequestId: "relayed-frame-1",
		})
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

	msgs, err := st.ListMessages(ctx, agent.ID, store.ContainerRef{ChannelID: agent.Agent.HomeChannelID}, store.Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("home channel holds %d messages after a keyed retry, want exactly 1", len(msgs))
	}
}

// A relayed frame that names a parent keeps it: the committed row is threaded
// under the message it replies to. parent_message_id is plumbed end to end on
// both the wire Message (comms.proto:250) and PostMessageRequest
// (comms.proto:565), so the write-through dropping it would silently flatten
// every threaded agent reply to a root message — a data loss that no error
// reports and that reads, downstream, as the agent simply never having replied
// in-thread.
//
// Mutation: drop ParentMessageId from the request CommitAgentPost builds → the
// committed message comes back with an empty parent and this reddens twice (the
// response echo and the store read-back).
func TestCommitAgentPostThreadsTheFramesParentMessageID(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")

	root, err := svc.CommitAgentPost(ctx, agent.ID, postedFrame(textBlocks("the question")))
	if err != nil {
		t.Fatalf("CommitAgentPost(root): %v", err)
	}
	rootID := root.GetMessage().GetId()
	if got := root.GetMessage().GetParentMessageId(); got != "" {
		t.Fatalf("root committed message ParentMessageId = %q, want \"\" (a frame with no parent must stay a root)", got)
	}

	// The frame the agent relays for an in-thread reply: same shape as any
	// posted frame, plus the parent it answers.
	reply := postedFrame(textBlocks("the threaded answer"))
	reply.Message.ParentMessageId = rootID

	committed, err := svc.CommitAgentPost(ctx, agent.ID, reply)
	if err != nil {
		t.Fatalf("CommitAgentPost(threaded reply): %v", err)
	}
	if got := committed.GetMessage().GetParentMessageId(); got != rootID {
		t.Fatalf("committed reply ParentMessageId = %q, want the root %q — the relayed frame's parent must survive the write-through", got, rootID)
	}

	// Durable, not merely echoed: the store of record holds the threading.
	msgs, err := st.ListMessages(ctx, agent.ID, store.ContainerRef{ChannelID: agent.Agent.HomeChannelID}, store.Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	parents := map[store.MessageID]store.MessageID{}
	for _, m := range msgs {
		parents[m.ID] = m.ParentMessageID
	}
	if got := parents[store.MessageID(committed.GetMessage().GetId())]; got != store.MessageID(rootID) {
		t.Fatalf("stored reply ParentMessageID = %q, want the root %q", got, rootID)
	}
	if got := parents[store.MessageID(rootID)]; got != "" {
		t.Fatalf("stored root ParentMessageID = %q, want \"\"", got)
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
