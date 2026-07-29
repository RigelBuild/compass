//go:build pgtest && unix

package comms

// The DURABLE, at-most-once commit path (#24 / OQ-3): CommitAgentPostKeyed /
// CommitAgentUpdateKeyed thread the agent-minted idempotency_key so a retried
// conversation frame commits at most once. This is the keyed counterpart to the
// loss-tolerant, unkeyed CommitAgentPost/CommitAgentUpdate the Deliver-path sink
// drives (agent_conversation_pgtest_test.go). Every test drives the real
// Postgres store + real bus (newHandler — no mocks) and defends one clause of
// the frozen contract:
//
//   - a keyed post commits exactly one row and returns its id;
//   - a REPLAY with the SAME key writes NO second row (count stays 1), returns
//     the ORIGINAL row id, and does NOT re-fan MessagePosted — proven by the
//     store-space message head NOT advancing across the replay, since PostMessage
//     fans out iff the store reports inserted=true (comms.go:270) and an insert
//     is exactly what advances the head;
//   - a keyed post under an EMPTY account fails closed CodeInvalidArgument,
//     writing nothing;
//   - a keyed UPDATE edits in place regardless of the key (the documented
//     no-thread decision: an update is idempotent by replacement).
//
// Gated `pgtest && unix` like agent_conversation_pgtest_test.go: it SKIPs (via
// pgtest.RequireDSN in newTestStore) when no Postgres/podman runtime exists.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/sealedsecurity/compass/go/internal/store"
)

// A keyed post commits a durable row and returns its id — the fresh-commit
// baseline the replay test builds on. The key rides through to AppendMessage's
// (author_account_id, client_request_id) index but, being the first send,
// inserts normally.
func TestCommitAgentPostKeyedCommitsARow(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")

	resp, err := svc.CommitAgentPostKeyed(ctx, agent.ID, postedFrame(textBlocks("the agent speaks")), "idem-key-1")
	if err != nil {
		t.Fatalf("CommitAgentPostKeyed: %v", err)
	}
	if resp.GetMessage().GetId() == "" {
		t.Fatal("committed message id is empty, want the stored row id")
	}
	if got := resp.GetMessage().GetAuthorAccountId(); got != string(agent.ID) {
		t.Fatalf("committed message author = %q, want the agent account %q", got, agent.ID)
	}

	msgs, err := st.ListMessages(ctx, agent.ID, store.ContainerRef{ChannelID: agent.Agent.HomeChannelID}, store.Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("home channel holds %d messages, want exactly 1", len(msgs))
	}
	if msgs[0].ID != store.MessageID(resp.GetMessage().GetId()) {
		t.Fatalf("stored id %q != returned id %q", msgs[0].ID, resp.GetMessage().GetId())
	}
}

// THE at-most-once contract, driven end to end through the keyed path: a replay
// with the SAME idempotency key writes NO second row, returns the ORIGINAL
// message_id, and does NOT re-fan MessagePosted. This is what makes the ack's
// committed=true + original-id true on a retry — the store's ON CONFLICT
// (author, client_request_id) DO NOTHING path re-reads the already-committed row
// (messages.go:82,94-100) and PostMessage suppresses the fan-out on
// inserted=false (comms.go:270).
//
// Three assertions pin it: the row count stays 1 (no second row), the returned
// ids are identical (original id on replay), and the store-space message head
// does NOT advance across the replay. The head is the deterministic re-fan
// proof: PostMessage fans out MessagePosted iff the store reports inserted=true,
// and an insert is exactly what advances messages.seq — so a head that does not
// move means no insert AND no fan-out, with no flaky "assert an event did not
// arrive" over the stream.
//
// Mutation that reddens it: dropping ClientRequestId from CommitAgentPostKeyed's
// request (back to the unkeyed shape) → the retry inserts a second row, the
// count becomes 2, the ids differ, and the head advances.
func TestCommitAgentPostKeyedReplayIsAtMostOnce(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")

	const key = "idem-replay-1"

	first, err := svc.CommitAgentPostKeyed(ctx, agent.ID, postedFrame(textBlocks("said once")), key)
	if err != nil {
		t.Fatalf("first CommitAgentPostKeyed: %v", err)
	}
	headAfterFirst, err := st.MessagesHeadSeq(ctx)
	if err != nil {
		t.Fatalf("MessagesHeadSeq(after first): %v", err)
	}

	replay, err := svc.CommitAgentPostKeyed(ctx, agent.ID, postedFrame(textBlocks("said once")), key)
	if err != nil {
		t.Fatalf("replay CommitAgentPostKeyed: %v", err)
	}
	if replay.GetMessage().GetId() != first.GetMessage().GetId() {
		t.Fatalf("replay returned a new id %q, want the ORIGINAL %q (at-most-once on the idempotency key)", replay.GetMessage().GetId(), first.GetMessage().GetId())
	}

	// The store holds exactly one row: the replay wrote nothing.
	msgs, err := st.ListMessages(ctx, agent.ID, store.ContainerRef{ChannelID: agent.Agent.HomeChannelID}, store.Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("home channel holds %d messages after a keyed replay, want exactly 1 (the replay must write NO second row)", len(msgs))
	}
	if msgs[0].ID != store.MessageID(first.GetMessage().GetId()) {
		t.Fatalf("the single stored row id %q != the first commit's id %q", msgs[0].ID, first.GetMessage().GetId())
	}

	// The head did not advance: no insert, so no MessagePosted fan-out.
	headAfterReplay, err := st.MessagesHeadSeq(ctx)
	if err != nil {
		t.Fatalf("MessagesHeadSeq(after replay): %v", err)
	}
	if headAfterReplay != headAfterFirst {
		t.Fatalf("message head advanced from %d to %d across the replay, want unchanged (a replay must neither insert nor re-fan MessagePosted)", headAfterFirst, headAfterReplay)
	}
}

// Fail-closed identity on the keyed path, mirroring
// TestCommitAgentFramesEmptyAccountFailsClosed for the unkeyed one: a keyed post
// under an EMPTY resolved account is a hard CodeInvalidArgument and writes
// NOTHING — never a silent fall-through to bootstrap-admin attribution, even
// with a key present.
func TestCommitAgentPostKeyedEmptyAccountFailsClosed(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")
	// Seed a row so the "wrote nothing" assertion has a positive control.
	if _, err := svc.CommitAgentPostKeyed(ctx, agent.ID, postedFrame(textBlocks("the agent's own")), "seed-key"); err != nil {
		t.Fatalf("CommitAgentPostKeyed(seed): %v", err)
	}

	_, err := svc.CommitAgentPostKeyed(ctx, "", postedFrame(textBlocks("should never be written")), "key-under-empty-account")
	connectCodeIs(t, err, connect.CodeInvalidArgument, "CommitAgentPostKeyed(empty account)")

	msgs, err := st.ListMessages(ctx, agent.ID, store.ContainerRef{ChannelID: agent.Agent.HomeChannelID}, store.Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("home channel holds %d messages, want exactly 1 (the empty-account call must write nothing)", len(msgs))
	}
	if msgs[0].Blocks[0].Text == nil || *msgs[0].Blocks[0].Text != "the agent's own" {
		t.Fatalf("stored text = %v, want the agent's own seeded text", msgs[0].Blocks[0].Text)
	}
}

// A keyed UPDATE is idempotent by replacement, so the key threading is a no-op:
// CommitAgentUpdateKeyed applies the frame to the row it addresses (same id, new
// blocks) exactly as CommitAgentUpdate does, regardless of the key. This pins
// the documented decision — the UPDATE path does NOT thread the key into the
// store (which has no dedup column for it) because addressing an existing row by
// id and replacing its blocks is already idempotent on re-send.
func TestCommitAgentUpdateKeyedEditsInPlaceIgnoringKey(t *testing.T) {
	svc, st := newHandler(t)
	ctx := context.Background()

	owner := mustUser(t, st, "owner")
	agent := mustAgent(t, st, owner.ID, "agent")

	seeded, err := svc.CommitAgentPostKeyed(ctx, agent.ID, postedFrame(textBlocks("partial")), "post-key")
	if err != nil {
		t.Fatalf("CommitAgentPostKeyed(seed): %v", err)
	}
	id := seeded.GetMessage().GetId()

	updated, err := svc.CommitAgentUpdateKeyed(ctx, agent.ID, updatedFrame(id, textBlocks("partial and settled")), "update-key")
	if err != nil {
		t.Fatalf("CommitAgentUpdateKeyed: %v", err)
	}
	if updated.GetMessage().GetId() != id {
		t.Fatalf("updated id = %q, want the edited row %q (an update must not insert)", updated.GetMessage().GetId(), id)
	}

	msgs, err := st.ListMessages(ctx, agent.ID, store.ContainerRef{ChannelID: agent.Agent.HomeChannelID}, store.Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("home channel holds %d messages, want exactly 1 (an update edits in place)", len(msgs))
	}
	if msgs[0].Blocks[0].Text == nil || *msgs[0].Blocks[0].Text != "partial and settled" {
		t.Fatalf("stored text = %v, want the edited text", msgs[0].Blocks[0].Text)
	}
}
