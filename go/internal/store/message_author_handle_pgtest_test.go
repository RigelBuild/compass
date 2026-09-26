//go:build pgtest

package store

import (
	"context"
	"testing"
)

// TestMessageReadsKeepAuthorWithoutHandleRow pins author_handle as a LEFT JOIN:
// an author with no account_handles row still has its message read back, with
// an empty AuthorHandle. An inner join would silently drop the message.
//
// Mutation: making ListMessages or MessageByID an INNER JOIN on account_handles
// drops the row, so the list comes back empty or MessageByID is ErrNotFound.
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
	id, _ := postAs(t, s, ch, author, "no handle row")

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
}
