//go:build pgtest

package store

import (
	"context"
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
