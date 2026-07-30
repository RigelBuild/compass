//go:build pgtest && unix

// The keyed write-through, driven end to end THROUGH the production sink. The
// unit tests around this seam each prove one leg but not the join:
// TestDeliverConversationThreadsIdempotencyKeyToSink (runnerhub) proves the hub
// threads the key to a FAKE sink, and TestCommitAgentPostKeyedReplayIsAtMostOnce
// (comms) proves the comms method dedups at the store when called DIRECTLY. What
// neither covers is the four-line adapter between them: commsConversationSink
// forwarding the key into CommitAgentPostKeyed rather than the unkeyed
// CommitAgentPost. This test closes that gap — a revert of the sink to the
// keyless method (dropping the key) reddens it, where the whole unit suite stays
// green.
//
// Store-gated (pgtest lane): the sink holds a real *comms.Comms over the
// Postgres store of record, so at-most-once is only provable against the
// database whose (author_account_id, client_request_id) unique index enforces
// it (design.md:1188-1190). Skips when no runtime is available.

package server

import (
	"context"
	"testing"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/comms"
	"github.com/sealedsecurity/compass/go/internal/pgtest"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// newSinkFixture stands up a real commsConversationSink over the Postgres store
// and returns it alongside the store and the agent account a relayed frame
// commits under. The agent's home channel (minted at CreateAgent, RT-2) is the
// container a Container-unset MessagePosted lands in, mirroring the production
// Deliver path (agent_caller.go).
func newSinkFixture(t *testing.T) (commsConversationSink, *store.Store, store.AccountID) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, pgtest.RequireDSN(t))
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)

	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	owner, err := st.CreateUser(ctx, store.NewUser{Handle: "owner", DisplayName: "owner"})
	if err != nil {
		t.Fatalf("CreateUser(owner): %v", err)
	}
	agent, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "agent", DisplayName: "agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	bus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(bus.Close)

	return commsConversationSink{comms: comms.NewComms(st, bus, admin.ID)}, st, agent.ID
}

// postedTextFrame is a Container-unset MessagePosted carrying one text block —
// the shape Deliver hands the sink, committed to the author's home channel.
func postedTextFrame(text string) *compassv1.MessagePosted {
	return &compassv1.MessagePosted{
		Message: &compassv1.Message{
			Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: text}}},
		},
	}
}

// A replayed durable conversation frame — same idempotency key, driven THROUGH
// commsConversationSink.PostAgentMessage — commits at most once: the store holds
// exactly one row and its head does not advance across the replay. This is the
// frozen at-most-once invariant observed at the real sink, not the bare comms
// method.
//
// Mutation that reddens it: revert sinks.go's POSTED branch to the unkeyed
// CommitAgentPost(ctx, account, posted) — or forward "" instead of
// idempotencyKey — and the replay inserts a second row (the store cannot dedup a
// commit that carries no client_request_id), the count becomes 2, and the head
// advances. The unit suite stays green under that revert; this does not.
func TestConversationSinkForwardsKeyForAtMostOnceCommit(t *testing.T) {
	sink, st, agent := newSinkFixture(t)
	ctx := context.Background()

	const key = "sink-idem-1"

	if err := sink.PostAgentMessage(ctx, agent, "sess-conv", key, postedTextFrame("said once"), nil); err != nil {
		t.Fatalf("first PostAgentMessage: %v", err)
	}
	headAfterFirst, err := st.MessagesHeadSeq(ctx)
	if err != nil {
		t.Fatalf("MessagesHeadSeq(after first): %v", err)
	}

	if err := sink.PostAgentMessage(ctx, agent, "sess-conv", key, postedTextFrame("said once"), nil); err != nil {
		t.Fatalf("replay PostAgentMessage: %v", err)
	}

	msgs, err := st.ListMessages(ctx, agent, store.ContainerRef{ChannelID: homeChannel(t, st, agent)}, store.Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("home channel holds %d messages after a keyed replay, want exactly 1 — the sink must forward the key so the store dedups the replay", len(msgs))
	}

	headAfterReplay, err := st.MessagesHeadSeq(ctx)
	if err != nil {
		t.Fatalf("MessagesHeadSeq(after replay): %v", err)
	}
	if headAfterReplay != headAfterFirst {
		t.Fatalf("message head advanced from %d to %d across the replay, want unchanged — a replay must neither insert nor re-fan (the key never reached the keyed commit)", headAfterFirst, headAfterReplay)
	}
}

// homeChannel reads back the agent's minted home channel — the container a
// Container-unset frame commits to.
func homeChannel(t *testing.T, st *store.Store, agent store.AccountID) store.ChannelID {
	t.Helper()
	acc, err := st.GetAccount(context.Background(), agent)
	if err != nil {
		t.Fatalf("GetAccount(%q): %v", agent, err)
	}
	if acc.Agent == nil {
		t.Fatalf("account %q is not an agent", agent)
	}
	return acc.Agent.HomeChannelID
}
