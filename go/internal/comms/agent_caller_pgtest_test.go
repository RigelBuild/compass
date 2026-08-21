//go:build pgtest && unix

package comms

// The agent-comms execution leg (comms-tools design T2, OQ-2): PostAsAccount /
// ListAsAccount execute one agent-initiated comms call as a resolved agent
// account, reusing the SAME PostMessage / ListMessages handler paths a human
// takes. Every test here drives the real Postgres store + real bus (no mocks —
// the newHandler harness comms_test.go uses) and defends one contract:
//
//   - attribution: the stored message's author IS the agent account, so the
//     transport (T3) can trust "author = agent account";
//   - D9 authz reuse: a non-member channel collapses to CodeNotFound, identical
//     to a human non-member — the agent never learns a channel it cannot see;
//   - home-channel default: an empty channel_id lands in the agent's home
//     channel, so the container needs no channel id plumbed in;
//   - fail-closed identity (SECURITY): an empty account is a hard
//     CodeInvalidArgument, never a silent fall-through to bootstrap-admin
//     attribution;
//   - idempotency reuse: the same client_request_id stores exactly one message.
//
// Gated `pgtest && unix`: it SKIPs (via pgtest.RequireDSN in newTestStore) when
// no Postgres/podman runtime is available, so the default gate stays green while
// the assertions are real wherever a runtime exists. `unix` because
// agent_caller.go is unix-tagged.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// textBlocks builds a single-text-block message body.
func textBlocks(text string) []*compassv1.MessageBlock {
	return []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: text}}}
}

// 11. PostAsAccount attributes the stored message to the AGENT account, not to
// the bootstrap admin. Posting to a channel the agent is a member of, the
// response message's author is the agent, and reading it back through
// ListAsAccount confirms it persisted under the agent — the "author = agent
// account" proof the transport T3 depends on.
func TestPostAsAccountAttributesToAgentAccount(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")
	// A channel the owner creates with the agent as a founding member.
	ch, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{
		Name:             "room",
		Kind:             store.ChannelKindChannel,
		MemberAccountIDs: []store.AccountID{agent.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	resp, err := svc.PostAsAccount(ctx, agent.ID, &compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, Blocks: textBlocks("from the agent")})
	if err != nil {
		t.Fatalf("PostAsAccount: %v", err)
	}
	if got := resp.GetMessage().GetAuthorAccountId(); got != string(agent.ID) {
		t.Fatalf("posted message author = %q, want the agent account %q (not admin)", got, agent.ID)
	}

	// Read it back as the agent (a member) — it persisted under the agent.
	listed, err := svc.ListAsAccount(ctx, agent.ID, &compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: string(ch.ID)},
	})
	if err != nil {
		t.Fatalf("ListAsAccount: %v", err)
	}
	var found bool
	for _, m := range listed.GetMessages() {
		if m.GetId() == resp.GetMessage().GetId() {
			found = true
			if got := m.GetAuthorAccountId(); got != string(agent.ID) {
				t.Fatalf("stored message author = %q, want the agent account %q", got, agent.ID)
			}
		}
	}
	if !found {
		t.Fatalf("posted message %q not found on read-back", resp.GetMessage().GetId())
	}
}

// 12. PostAsAccount to a channel the agent is NOT a member of collapses to
// CodeNotFound — the D9 write-authz gate, identical to a human non-member. The
// agent never learns the channel exists. (Store precedent: AppendMessage's
// requireChannelMember returns ErrNotFound; mirrors comms_test.go's
// non-member SearchMessages scoping.)
func TestPostAsAccountNonMemberChannelIsNotFound(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")
	// A private channel with NO agent membership (only the owner).
	ch, err := st.CreateChannel(ctx, owner.ID, store.NewChannel{Name: "private", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	_, err = svc.PostAsAccount(ctx, agent.ID, &compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, Blocks: textBlocks("sneaking in")})
	connectCodeIs(t, err, connect.CodeNotFound, "PostAsAccount(non-member)")
}

// 13. An empty channel_id defaults to the agent's home channel: the post lands
// in agent.Agent.HomeChannelID, so a container posting "in my own channel" needs
// no id plumbed in. Read back the home channel and find the message.
func TestPostAsAccountEmptyChannelDefaultsToHome(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")

	resp, err := svc.PostAsAccount(ctx, agent.ID, &compassv1.PostMessageRequest{
		// No channel_id set — defaults to the agent's home channel.
		Topic:  &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
		Blocks: textBlocks("home post"),
	})
	if err != nil {
		t.Fatalf("PostAsAccount(empty channel): %v", err)
	}
	// A message carries only its topic now; that it landed in the HOME channel is
	// proven by reading it back through the home channel below (the channel is
	// resolved through the topic server-side, not echoed on the wire message).
	if got := resp.GetMessage().GetTopicId(); got == "" {
		t.Fatal("posted message TopicId = \"\", want the home channel's home topic id")
	}

	// The home channel now holds exactly this message.
	listed, err := svc.ListAsAccount(ctx, agent.ID, &compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: string(agent.Agent.HomeChannelID)},
	})
	if err != nil {
		t.Fatalf("ListAsAccount(home): %v", err)
	}
	var found bool
	for _, m := range listed.GetMessages() {
		if m.GetId() == resp.GetMessage().GetId() {
			found = true
		}
	}
	if !found {
		t.Fatalf("home post %q not found in the agent's home channel", resp.GetMessage().GetId())
	}
}

// 14. An empty account is a hard CodeInvalidArgument (errNoActor) and writes
// NOTHING — the fail-closed guard that refuses to attribute an unresolved
// caller to the bootstrap admin. THE SECURITY TEST for this leg.
//
// The target channel is one the ADMIN is a member of, so if the guard were
// removed the call would fall through to actorFromContext's admin fallback (an
// empty actor on the context resolves to adminID) and genuinely write a message
// there. Asserting the channel stays empty proves no admin write happened.
//
// Mutation: remove `if account == ""` in PostAsAccount → the call attributes to
// admin and writes; this test fails twice (code becomes NotFound/nil, and the
// channel gains a message).
func TestPostAsAccountEmptyAccountFailsClosedNoAdminWrite(t *testing.T) {
	st := newTestStore(t)
	bus := newBus(t)
	admin, err := st.BootstrapAdmin(context.Background(), store.NewUser{Handle: "root", DisplayName: "Root"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	svc := NewComms(st, bus, admin.ID)
	ctx := context.Background()

	// A channel the admin founds (so admin is a member) — the write target that
	// a missing guard would silently attribute to admin.
	ch, err := st.CreateChannel(ctx, admin.ID, store.NewChannel{Name: "admin-room", Kind: store.ChannelKindChannel})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	_, err = svc.PostAsAccount(ctx, "", &compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(ch.ID)}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, Blocks: textBlocks("should never be written")})
	connectCodeIs(t, err, connect.CodeInvalidArgument, "PostAsAccount(empty account)")

	// No message was written as admin: the admin's own channel is empty.
	listed, err := svc.ListMessages(WithActor(ctx, admin.ID), connect.NewRequest(&compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: string(ch.ID)},
	}))
	if err != nil {
		t.Fatalf("ListMessages(admin): %v", err)
	}
	if got := len(listed.Msg.GetMessages()); got != 0 {
		t.Fatalf("admin channel holds %d messages after an empty-account post, want 0 (no admin attribution)", got)
	}
}

// 15. Idempotency reuse: two PostAsAccount calls carrying the SAME
// client_request_id store exactly one message (the store's
// (author_account_id, client_request_id) dedup). Both calls return the same
// message id, and the channel holds one message.
func TestPostAsAccountIdempotentByClientRequestID(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")

	req := &compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(agent.Agent.HomeChannelID)}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, Blocks: textBlocks("once"), ClientRequestId: "agent-req-1"}
	first, err := svc.PostAsAccount(ctx, agent.ID, req)
	if err != nil {
		t.Fatalf("PostAsAccount(first): %v", err)
	}
	// A retry with the same id (even a different body) returns the stored row.
	retry, err := svc.PostAsAccount(ctx, agent.ID, &compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: string(agent.Agent.HomeChannelID)}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, Blocks: textBlocks("different body, same id"), ClientRequestId: "agent-req-1"})
	if err != nil {
		t.Fatalf("PostAsAccount(retry): %v", err)
	}
	if first.GetMessage().GetId() != retry.GetMessage().GetId() {
		t.Fatalf("retry stored a new message %q, want the first's %q (idempotent dedup)", retry.GetMessage().GetId(), first.GetMessage().GetId())
	}

	// Exactly one message in the home channel.
	listed, err := svc.ListAsAccount(ctx, agent.ID, &compassv1.ListMessagesRequest{
		Container: &compassv1.ListMessagesRequest_ChannelId{ChannelId: string(agent.Agent.HomeChannelID)},
	})
	if err != nil {
		t.Fatalf("ListAsAccount(home): %v", err)
	}
	if got := len(listed.GetMessages()); got != 1 {
		t.Fatalf("home channel holds %d messages after a duplicate post, want exactly 1", got)
	}
}
