//go:build pgtest

package store

// The presence component's read side (RIG-1569 T8, design record D4):
// AgentHasOpenAsk (the WAITING overlay input) and SharesVisibleChannel (the
// AgentPresenceChanged fan-out scoping). Both are properties only a real
// Postgres proves — the JSONB path-existence probe that must catch an
// omitted-answered-field ask, and the channel_members self-join — so the file is
// pgtest-tagged, mirroring delivery_reads_test.go. context.Background() is the
// test root (rule://go-thread-context exemption for _test.go).

import (
	"context"
	"strings"
	"testing"
)

// TestAgentHasOpenAskCatchesOmittedAnsweredField is the load-bearing case: an
// unanswered ask stores with the answered field OMITTED (omitempty), so a naive
// containment `@> '[{"kind":"ask","ask":{"answered":false}}]'` MISSES it. The
// path-existence probe must catch an ask whose answered is absent OR false.
func TestAgentHasOpenAskCatchesOmittedAnsweredField(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := mustNamedChannelWith(t, s, owner.ID, "room", agent.ID)

	// Seed an unanswered (pending) ask authored by the agent. pendingAsk carries
	// no Answered field, so the stored JSONB omits "answered" entirely — the
	// exact shape the naive containment would miss.
	if _, _, err := s.AppendMessage(ctx, Message{AuthorAccountID: agent.ID, Blocks: []MessageBlock{textBlock("choose one"), pendingAsk("ask-open", false)}}, string(ch), TopicRef{Name: "general", Create: true}, ""); err != nil {
		t.Fatalf("AppendMessage(pending ask): %v", err)
	}

	// The correct predicate catches the omitted-field open ask.
	open, err := s.AgentHasOpenAsk(ctx, agent.ID)
	if err != nil {
		t.Fatalf("AgentHasOpenAsk: %v", err)
	}
	if !open {
		t.Fatalf("AgentHasOpenAsk = false, want true (an unanswered ask omits the answered field; a naive @> containment misses it — the path probe must catch it)")
	}

	// Prove RED-first here in the SAME database: the naive containment probe
	// that this method deliberately does NOT use returns false for the very same
	// row, demonstrating the omitted-field miss the path probe repairs.
	var naive bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM messages WHERE author_account_id = $1
		   AND blocks @> '[{"kind":"ask","ask":{"answered":false}}]'::jsonb)`,
		string(agent.ID),
	).Scan(&naive); err != nil {
		t.Fatalf("naive containment probe: %v", err)
	}
	if naive {
		t.Fatalf("naive containment matched the omitted-field ask; the RED premise is invalid (it should MISS, which is why the path probe is required)")
	}
}

// TestAgentHasOpenAskFalseOnceAnswered: answering the ask flips answered=true,
// so the open-ask overlay clears — the WAITING->prior-state transition.
func TestAgentHasOpenAskFalseOnceAnswered(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := mustNamedChannelWith(t, s, owner.ID, "room", agent.ID)

	if _, _, err := s.AppendMessage(ctx, Message{AuthorAccountID: agent.ID, Blocks: []MessageBlock{textBlock("choose one"), pendingAsk("ask-1", false)}}, string(ch), TopicRef{Name: "general", Create: true}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	open, err := s.AgentHasOpenAsk(ctx, agent.ID)
	if err != nil {
		t.Fatalf("AgentHasOpenAsk (pending): %v", err)
	}
	if !open {
		t.Fatalf("AgentHasOpenAsk before answer = false, want true")
	}

	// A member answers it; answered flips to true.
	if _, _, err := s.AnswerAsk(ctx, owner.ID, "ask-1", []AskAnswer{
		{QuestionID: "q1", ChosenOptionIDs: []string{"opt-a"}},
	}); err != nil {
		t.Fatalf("AnswerAsk: %v", err)
	}

	open, err = s.AgentHasOpenAsk(ctx, agent.ID)
	if err != nil {
		t.Fatalf("AgentHasOpenAsk (answered): %v", err)
	}
	if open {
		t.Fatalf("AgentHasOpenAsk after answer = true, want false (an answered ask clears the WAITING overlay)")
	}
}

// TestAgentHasOpenAskFalseWithNoAsks: an agent that authored no asks (only text)
// has no open ask.
func TestAgentHasOpenAskFalseWithNoAsks(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := mustNamedChannelWith(t, s, owner.ID, "room", agent.ID)

	if _, _, err := s.AppendMessage(ctx, Message{AuthorAccountID: agent.ID, Blocks: []MessageBlock{textBlock("just talking")}}, string(ch), TopicRef{Name: "general", Create: true}, ""); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	open, err := s.AgentHasOpenAsk(ctx, agent.ID)
	if err != nil {
		t.Fatalf("AgentHasOpenAsk: %v", err)
	}
	if open {
		t.Fatalf("AgentHasOpenAsk = true, want false (no authored asks)")
	}
}

// TestSharesVisibleChannel: two accounts co-member of a channel share one; an
// account in no shared channel does not.
func TestSharesVisibleChannel(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	actor := mustUser(t, s, "actor")
	stranger := mustUser(t, s, "stranger")

	// actor and agent are both members of the same channel; stranger is not.
	mustNamedChannelWith(t, s, owner.ID, "shared", agent.ID, actor.ID)

	shares, err := s.SharesVisibleChannel(ctx, actor.ID, agent.ID)
	if err != nil {
		t.Fatalf("SharesVisibleChannel(actor, agent): %v", err)
	}
	if !shares {
		t.Fatalf("SharesVisibleChannel(actor, agent) = false, want true (co-members of one channel)")
	}

	shares, err = s.SharesVisibleChannel(ctx, stranger.ID, agent.ID)
	if err != nil {
		t.Fatalf("SharesVisibleChannel(stranger, agent): %v", err)
	}
	if shares {
		t.Fatalf("SharesVisibleChannel(stranger, agent) = true, want false (no shared channel)")
	}
}

// TestMessagesAuthorIndexExists guards the btree index serving AgentHasOpenAsk's
// author-only equality probe (author_account_id = $1), which fires on every
// presence edge. The index is created by a migration; this asserts it survives
// into a freshly-migrated database, defending against a future migration
// collapse silently dropping it (RIG-1649). It asserts the indexed column too
// (via indexdef), so a future rename that moves the index off author_account_id
// also fails rather than passing on the name alone.
func TestMessagesAuthorIndexExists(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	var indexdef string
	if err := s.pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes
		   WHERE tablename = 'messages' AND indexname = 'messages_author_idx'`,
	).Scan(&indexdef); err != nil {
		t.Fatalf("index messages_author_idx missing; AgentHasOpenAsk's author-only equality probe would seq-scan messages on every presence edge: %v", err)
	}
	if !strings.Contains(indexdef, "(author_account_id)") {
		t.Fatalf("messages_author_idx is not on author_account_id (indexdef = %q); AgentHasOpenAsk's author-only probe would not be served", indexdef)
	}
}
