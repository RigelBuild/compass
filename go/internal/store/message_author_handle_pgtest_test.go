//go:build pgtest

package store

import (
	"context"
	"errors"
	"testing"
)

// TestMessageReadsKeepAuthorWithoutHandleRow pins author_handle as a LEFT JOIN:
// an author with no account_handles row still has its message read back, with
// an empty AuthorHandle, through list, id, search, and request-id replay reads.
//
// Mutation: making any of those reads an INNER JOIN on account_handles drops the row.
func TestMessageReadsKeepAuthorWithoutHandleRow(t *testing.T) {
	ctx := context.Background() // test root context
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")

	// A user account written without the account_handles row CreateUser adds.
	author := AccountID(newID())
	if _, err := s.pool.Exec(ctx,
		"INSERT INTO accounts (id, handle, display_name, tenant_id) VALUES ($1, $2, $3, $4)",
		string(author), "unhandled", "Unhandled", string(s.resolveTenant(ctx)),
	); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		"INSERT INTO user_accounts (account_id, role, tenant_id) VALUES ($1, $2, $3)",
		string(author), int32(UserRoleMember), string(s.resolveTenant(ctx)),
	); err != nil {
		t.Fatalf("insert user_account: %v", err)
	}

	ch := mustNamedChannelWith(t, s, owner.ID, "room", author)
	id, _ := postAs(t, s, ch, author, "unhandled author note")

	list, err := s.ListMessages(ctx, ListMessagesQuery{Actor: owner.ID, ChannelID: ch})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(list) != 1 || string(list[0].ID) != id {
		t.Fatalf("ListMessages = %+v, want the one message %s", list, id)
	}
	if list[0].AuthorAccountID != author || list[0].AuthorHandle != "" {
		t.Fatalf("ListMessages author = %q handle %q, want %q with an empty handle", list[0].AuthorAccountID, list[0].AuthorHandle, author)
	}

	m, err := s.MessageByID(ctx, id)
	if err != nil {
		t.Fatalf("MessageByID(%s): %v", id, err)
	}
	if m.AuthorAccountID != author || m.AuthorHandle != "" {
		t.Fatalf("MessageByID author = %q handle %q, want %q with an empty handle", m.AuthorAccountID, m.AuthorHandle, author)
	}

	found, err := s.SearchMessages(ctx, owner.ID, SearchScope{ChannelID: ch}, "unhandled", Page{})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(found) != 1 || string(found[0].ID) != id || found[0].AuthorHandle != "" {
		t.Fatalf("SearchMessages = %+v, want the one message %s with an empty handle", found, id)
	}

	// A replayed client_request_id returns the stored row via the request-id read.
	msg := Message{AuthorAccountID: author, Blocks: []MessageBlock{textBlock("replayed")}}
	first, _, err := s.AppendMessage(ctx, msg, string(ch), TopicRef{Name: "general"}, "crid-1")
	if err != nil {
		t.Fatalf("AppendMessage(first): %v", err)
	}
	again, inserted, err := s.AppendMessage(ctx, msg, string(ch), TopicRef{Name: "general"}, "crid-1")
	if err != nil || inserted || again.ID != first.ID || again.AuthorHandle != "" {
		t.Fatalf("AppendMessage(replay) = %+v inserted=%v err=%v, want %s replayed with an empty handle", again, inserted, err, first.ID)
	}
}

// TestMessageAuthorHandleOwnerQualification pins owner-qualified author addresses
// across message writes, reads, replay, delivery cursors, and ask answering.
func TestMessageAuthorHandleOwnerQualification(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "matt")
	author := mustAgent(t, s, owner.ID, "compass-ux")
	recipient := mustAgent(t, s, owner.ID, "reader")
	ch := mustNamedChannelWith(t, s, owner.ID, "shared", author.ID, recipient.ID)
	subscribeAgent(t, s, owner.ID, ch, author.ID)
	subscribeAgent(t, s, owner.ID, ch, recipient.ID)

	const requestID = "owner-qualified-replay"
	agentMessage, inserted, err := s.AppendMessage(ctx, Message{
		AuthorAccountID: author.ID,
		Blocks:          []MessageBlock{textBlock("agent note")},
	}, string(ch), TopicRef{Name: "general", Create: true}, requestID)
	if err != nil {
		t.Fatalf("AppendMessage(agent): %v", err)
	}
	if !inserted {
		t.Fatal("AppendMessage(agent) returned inserted=false, want an inserted message")
	}
	qualified := owner.Handle + "/" + author.Handle
	if agentMessage.AuthorHandle != qualified {
		t.Fatalf("AppendMessage(agent) author_handle = %q, want %q", agentMessage.AuthorHandle, qualified)
	}

	userMessage, inserted, err := s.AppendMessage(ctx, Message{
		AuthorAccountID: owner.ID,
		Blocks:          []MessageBlock{textBlock("user note")},
	}, string(ch), TopicRef{Name: "general"}, "")
	if err != nil {
		t.Fatalf("AppendMessage(user): %v", err)
	}
	if !inserted {
		t.Fatal("AppendMessage(user) returned inserted=false, want an inserted message")
	}
	if userMessage.AuthorHandle != owner.Handle {
		t.Fatalf("AppendMessage(user) author_handle = %q, want bare %q", userMessage.AuthorHandle, owner.Handle)
	}

	assertAuthorHandleReads(t, ctx, s, owner, recipient.ID, ch, agentMessage.ID, userMessage.ID, qualified)

	askPosted, inserted, err := s.AppendMessage(ctx, Message{
		AuthorAccountID: author.ID,
		Blocks:          []MessageBlock{pendingAsk("owner-qualified-ask", false)},
	}, string(ch), TopicRef{Name: "general"}, "")
	if err != nil {
		t.Fatalf("AppendMessage(ask): %v", err)
	}
	if !inserted {
		t.Fatal("AppendMessage(ask) returned inserted=false, want an inserted message")
	}

	readers := authorHandleReaders(ctx, s, owner.ID, author.ID, ch, agentMessage.ID, askPosted.ID, requestID)
	for _, reader := range readers {
		t.Run(reader.name, func(t *testing.T) {
			message, err := reader.read()
			if err != nil {
				t.Fatalf("read message: %v", err)
			}
			if message.ID != reader.wantID {
				t.Fatalf("read message id = %q, want %q", message.ID, reader.wantID)
			}
			if message.AuthorAccountID != author.ID || message.AuthorHandle != qualified {
				t.Fatalf("read author = %q handle %q, want %q and %q", message.AuthorAccountID, message.AuthorHandle, author.ID, qualified)
			}
		})
	}
}

// authorHandleReader reads one message through a store path that must carry
// the owner-qualified author handle.
type authorHandleReader struct {
	name   string
	wantID MessageID
	read   func() (Message, error)
}

func authorHandleReaders(ctx context.Context, s *Store, owner, author AccountID, ch ChannelID, agentID, askID MessageID, requestID string) []authorHandleReader {
	return []authorHandleReader{
		{
			name:   "author block update",
			wantID: agentID,
			read: func() (Message, error) {
				return s.UpdateMessageBlocksAsAuthor(ctx, author, agentID, []MessageBlock{textBlock("settled searchable marker")})
			},
		},
		{
			name:   "MessageByID",
			wantID: agentID,
			read: func() (Message, error) {
				return s.MessageByID(ctx, string(agentID))
			},
		},
		{
			name:   "SearchMessages",
			wantID: agentID,
			read: func() (Message, error) {
				found, err := s.SearchMessages(ctx, owner, SearchScope{ChannelID: ch}, "searchable marker", Page{})
				if err != nil {
					return Message{}, err
				}
				for _, message := range found {
					if message.ID == agentID {
						return message, nil
					}
				}
				return Message{}, nil
			},
		},
		{
			name:   "UnroutedMentionMessages",
			wantID: agentID,
			read: func() (Message, error) {
				unrouted, err := s.UnroutedMentionMessages(ctx, 0, 100)
				if err != nil {
					return Message{}, err
				}
				for _, message := range unrouted {
					if message.ID == agentID {
						return message.Message, nil
					}
				}
				return Message{}, nil
			},
		},
		{
			name:   "replay via GetMessageByRequestID",
			wantID: agentID,
			read: func() (Message, error) {
				message, inserted, err := s.AppendMessage(ctx, Message{
					AuthorAccountID: author,
					Blocks:          []MessageBlock{textBlock("agent note")},
				}, string(ch), TopicRef{Name: "general"}, requestID)
				if err == nil && inserted {
					return Message{}, errors.New("AppendMessage replay inserted a new row")
				}
				return message, err
			},
		},
		{
			name:   "FindAskMessage via AnswerAsk",
			wantID: askID,
			read: func() (Message, error) {
				ask, _, err := s.AnswerAsk(ctx, owner, "owner-qualified-ask", []AskAnswer{{
					QuestionID:      "q1",
					ChosenOptionIDs: []string{"opt-a"},
				}})
				return ask, err
			},
		},
	}
}

// assertAuthorHandleReads checks the list, delivery, and mention reads carry
// the qualified agent handle and the bare user handle.
func assertAuthorHandleReads(t *testing.T, ctx context.Context, s *Store, owner Account, recipient AccountID, ch ChannelID, agentID, userID MessageID, qualified string) {
	t.Helper()
	list, err := s.ListMessages(ctx, ListMessagesQuery{Actor: owner.ID, ChannelID: ch})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	byID := make(map[MessageID]string, len(list))
	for _, message := range list {
		byID[message.ID] = message.AuthorHandle
	}
	if byID[agentID] != qualified || byID[userID] != owner.Handle {
		t.Fatalf("ListMessages author handles = agent %q, user %q; want %q and %q", byID[agentID], byID[userID], qualified, owner.Handle)
	}

	owed, err := s.UndeliveredMessages(ctx, recipient)
	if err != nil {
		t.Fatalf("UndeliveredMessages: %v", err)
	}
	messages := owed[ch]
	if len(messages) != 2 {
		t.Fatalf("UndeliveredMessages[%s] = %v, want both messages", ch, messages)
	}
	for _, message := range messages {
		want := owner.Handle
		if message.ID == agentID {
			want = qualified
		}
		if message.AuthorHandle != want {
			t.Errorf("UndeliveredMessages author_handle for %s = %q, want %q", message.ID, message.AuthorHandle, want)
		}
	}

	if err := s.RecordOwedMention(ctx, recipient, ch, string(agentID)); err != nil {
		t.Fatalf("RecordOwedMention: %v", err)
	}
	owedMentions, err := s.OwedMentions(ctx, recipient)
	if err != nil {
		t.Fatalf("OwedMentions: %v", err)
	}
	mentions := owedMentions[ch]
	if len(mentions) != 1 || mentions[0].AuthorHandle != qualified {
		t.Fatalf("OwedMentions author_handle = %v, want one message with %q", mentions, qualified)
	}
}

// TestMessageAuthorHandleEmptyWhenOwnerHandleMissing pins the empty-on-miss
// contract for an agent whose owner account has no account_handles row.
func TestMessageAuthorHandleEmptyWhenOwnerHandleMissing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "unhandled-owner")
	agent := mustAgent(t, s, owner.ID, "orphaned-owner-handle-agent")

	// Keep the owner account but remove only its handle row.
	if _, err := s.pool.Exec(ctx, "DELETE FROM account_handles WHERE account_id = $1", string(owner.ID)); err != nil {
		t.Fatalf("delete owner account handle: %v", err)
	}

	message, inserted, err := s.AppendMessage(ctx, Message{
		AuthorAccountID: agent.ID,
		Blocks:          []MessageBlock{textBlock("owner handle unresolved")},
	}, string(agent.Agent.HomeChannelID), TopicRef{Name: "general", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage(agent): %v", err)
	}
	if !inserted {
		t.Fatal("AppendMessage(agent) returned inserted=false, want an inserted message")
	}
	if message.AuthorHandle != "" {
		t.Fatalf("AppendMessage(agent) author_handle = %q, want empty for unresolved owner", message.AuthorHandle)
	}

	list, err := s.ListMessages(ctx, ListMessagesQuery{Actor: owner.ID, ChannelID: agent.Agent.HomeChannelID})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(list) != 1 || list[0].ID != message.ID || list[0].AuthorHandle != "" {
		t.Fatalf("ListMessages = %+v, want the inserted message %q with an empty author_handle", list, message.ID)
	}
}
