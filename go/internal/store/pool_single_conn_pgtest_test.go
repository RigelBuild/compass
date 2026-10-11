//go:build pgtest

package store

import (
	"context"
	"testing"
	"time"
)

// A deploy may set pool_max_conns=1, so a path that re-reads through the pool
// must first release its transaction's connection or it waits on itself.
func TestSingleConnPoolFallbackReadsDoNotDeadlock(t *testing.T) {
	_, dsn := newTestStoreDSN(t)
	s := reopenStore(t, dsn+"&pool_max_conns=1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	author := mustUser(t, s, "author")
	ch := mustChannel(t, s, author.ID)
	msg := Message{AuthorAccountID: author.ID, Blocks: []MessageBlock{textBlock("once")}}
	topic := TopicRef{Name: "general", Create: true}
	first, _, err := s.AppendMessage(ctx, msg, string(ch.ID), topic, "req-single")
	if err != nil {
		t.Fatalf("AppendMessage(first): %v", err)
	}
	retry, inserted, err := s.AppendMessage(ctx, msg, string(ch.ID), topic, "req-single")
	if err != nil {
		t.Fatalf("AppendMessage(retry) on a one-connection pool: %v", err)
	}
	if inserted || retry.ID != first.ID {
		t.Fatalf("retry = (%q, inserted=%v), want the stored %q", retry.ID, inserted, first.ID)
	}

	admin, err := s.BootstrapAdmin(ctx, NewUser{Handle: "root", DisplayName: "Root"})
	if err != nil {
		t.Fatalf("BootstrapAdmin(first): %v", err)
	}
	again, err := s.BootstrapAdmin(ctx, NewUser{Handle: "root", DisplayName: "Root"})
	if err != nil {
		t.Fatalf("BootstrapAdmin(restart) on a one-connection pool: %v", err)
	}
	if again.ID != admin.ID {
		t.Fatalf("restart returned %q, want the existing admin %q", again.ID, admin.ID)
	}

	for name, ensure := range map[string]func(context.Context) (Account, error){
		"EnsureSystemAccount":       s.EnsureSystemAccount,
		"EnsureLinearBridgeAccount": s.EnsureLinearBridgeAccount,
	} {
		first, err := ensure(ctx)
		if err != nil {
			t.Fatalf("%s(first): %v", name, err)
		}
		again, err := ensure(ctx)
		if err != nil {
			t.Fatalf("%s(restart) on a one-connection pool: %v", name, err)
		}
		if again.ID != first.ID {
			t.Fatalf("%s restart returned %q, want %q", name, again.ID, first.ID)
		}
	}
}
