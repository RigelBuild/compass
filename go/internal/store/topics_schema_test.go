//go:build pgtest

package store

// Schema shape after the T2 collapse (compass-zulip-threading-model §Red-first
// (e)): messages carries topic_id NOT NULL and NO channel_id / parent_message_id
// column. Asserted against a real migrated database so a migration that failed
// to drop the old columns — or to make topic_id NOT NULL — is caught, not just a
// Go-side struct change.

import (
	"context"
	"testing"
)

func TestMessagesSchemaAfterTopicCollapse(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	columnExists := func(column string) bool {
		t.Helper()
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_name = 'messages' AND column_name = $1
			)`, column,
		).Scan(&exists); err != nil {
			t.Fatalf("probe messages.%s: %v", column, err)
		}
		return exists
	}

	// topic_id is present and NOT NULL.
	if !columnExists("topic_id") {
		t.Fatal("messages has no topic_id column")
	}
	var isNullable string
	if err := s.pool.QueryRow(ctx,
		`SELECT is_nullable FROM information_schema.columns
		 WHERE table_name = 'messages' AND column_name = 'topic_id'`,
	).Scan(&isNullable); err != nil {
		t.Fatalf("read topic_id nullability: %v", err)
	}
	if isNullable != "NO" {
		t.Fatalf("messages.topic_id is_nullable = %q, want NO (topic_id NOT NULL)", isNullable)
	}

	// The old columns are gone.
	if columnExists("channel_id") {
		t.Fatal("messages still carries a channel_id column (F10: the channel is topics.channel_id)")
	}
	if columnExists("parent_message_id") {
		t.Fatal("messages still carries a parent_message_id column (threading is by topic now)")
	}
}
