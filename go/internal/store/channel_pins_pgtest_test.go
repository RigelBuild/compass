//go:build pgtest

package store

// Channel-pin (pinned board) store contracts. The board is a set of POINTERS to
// existing messages: PinMessage never writes a message, only an entry pointing at
// one that already lives in the channel. These tests defend the guard (a message
// in another channel cannot be pinned), the repoint compare-and-swap (position
// preserved on success, in-band failure when the replaced id is no longer
// pinned), the per-channel cap (the 6th pin is refused; the 5 survive), the
// FOR-UPDATE serialization of two cap-edge pins (exactly one admitted), and the
// unpin/read round-trip. context.Background() is the sanctioned test root here.

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// mustPinnableMessage appends a real message under (ch, "general") and returns
// its id — a valid pin target living in ch. Pins point at existing messages, so
// the tests need genuine message rows (not fabricated ids).
func mustPinnableMessage(t *testing.T, s *Store, ch ChannelID, author AccountID, body string) MessageID {
	t.Helper()
	msg, _, err := s.AppendMessage(context.Background(),
		Message{AuthorAccountID: author, Blocks: []MessageBlock{textBlock(body)}},
		string(ch), TopicRef{Name: "general"}, "")
	if err != nil {
		t.Fatalf("AppendMessage(%q): %v", body, err)
	}
	return msg.ID
}

// pinnedIDs projects the ordered message ids of a board, for order/membership
// assertions.
func pinnedIDs(entries []PinnedEntry) []MessageID {
	ids := make([]MessageID, len(entries))
	for i, e := range entries {
		ids[i] = e.MessageID
	}
	return ids
}

// TestPinMessageRejectsMessageFromAnotherChannel pins the join guard: a message
// living in a DIFFERENT channel's topic is not a valid target and is refused
// in-band, with no board entry written.
func TestPinMessageRejectsMessageFromAnotherChannel(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	chA := mustNamedChannel(t, s, owner.ID, "room-a").ID
	chB := mustNamedChannel(t, s, owner.ID, "room-b").ID
	// A message that lives in chB's topic — not in chA.
	msgB := mustPinnableMessage(t, s, chB, owner.ID, "over in B")

	_, err := s.PinMessage(ctx, chA, msgB, MessageID(""), owner.ID)
	sentinelIs(t, err, ErrNotFound, "message from another channel")

	entries, err := s.PinnedEntries(ctx, chA)
	if err != nil {
		t.Fatalf("PinnedEntries: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("board has %d entries after a rejected pin, want 0", len(entries))
	}
}

// TestPinMessageFreshPinAppends pins that a fresh pin (replace == "") appends an
// entry pointing at the message, at the next position.
func TestPinMessageFreshPinAppends(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	ch := mustNamedChannel(t, s, owner.ID, "room").ID
	m1 := mustPinnableMessage(t, s, ch, owner.ID, "first")
	m2 := mustPinnableMessage(t, s, ch, owner.ID, "second")

	if _, err := s.PinMessage(ctx, ch, m1, MessageID(""), owner.ID); err != nil {
		t.Fatalf("PinMessage(m1): %v", err)
	}
	entries, err := s.PinMessage(ctx, ch, m2, MessageID(""), owner.ID)
	if err != nil {
		t.Fatalf("PinMessage(m2): %v", err)
	}
	got := pinnedIDs(entries)
	if len(got) != 2 || got[0] != m1 || got[1] != m2 {
		t.Fatalf("board = %v, want [%s %s] in position order", got, m1, m2)
	}
	if entries[0].Position >= entries[1].Position {
		t.Fatalf("positions not ascending: %d then %d", entries[0].Position, entries[1].Position)
	}
	if entries[1].PinnedByAccountID != owner.ID {
		t.Fatalf("pinned_by = %q, want %q", entries[1].PinnedByAccountID, owner.ID)
	}
}

// TestPinMessageDuplicateFreshPinConflicts pins the documented dup-pin contract
// (PinMessage doc): a fresh pin of an already-pinned message surfaces the
// (channel_id, message_id) primary-key conflict as ErrConflict, and the board is
// left unchanged (the failed insert rolls back with the whole tx). Without the
// pgUniqueViolation→ErrConflict mapping this would leak a raw wrapped error.
func TestPinMessageDuplicateFreshPinConflicts(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	ch := mustNamedChannel(t, s, owner.ID, "room").ID
	m1 := mustPinnableMessage(t, s, ch, owner.ID, "first")

	if _, err := s.PinMessage(ctx, ch, m1, MessageID(""), owner.ID); err != nil {
		t.Fatalf("PinMessage(m1): %v", err)
	}
	// Pinning m1 again as a fresh pin hits the PK conflict.
	_, err := s.PinMessage(ctx, ch, m1, MessageID(""), owner.ID)
	sentinelIs(t, err, ErrConflict, "a duplicate fresh pin of an already-pinned message")

	entries, err := s.PinnedEntries(ctx, ch)
	if err != nil {
		t.Fatalf("PinnedEntries: %v", err)
	}
	got := pinnedIDs(entries)
	if len(got) != 1 || got[0] != m1 {
		t.Fatalf("board = %v after a rejected duplicate pin, want [%s] unchanged", got, m1)
	}
}

// TestPinMessageRepointPreservesPosition pins the repoint CAS success path: a
// replace naming a currently-pinned id atomically swaps in the new message at the
// SAME position — old id gone, new id present, order preserved, count unchanged.
func TestPinMessageRepointPreservesPosition(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	ch := mustNamedChannel(t, s, owner.ID, "room").ID
	m1 := mustPinnableMessage(t, s, ch, owner.ID, "one")
	m2 := mustPinnableMessage(t, s, ch, owner.ID, "two")
	m3 := mustPinnableMessage(t, s, ch, owner.ID, "three")

	if _, err := s.PinMessage(ctx, ch, m1, MessageID(""), owner.ID); err != nil {
		t.Fatalf("PinMessage(m1): %v", err)
	}
	if _, err := s.PinMessage(ctx, ch, m2, MessageID(""), owner.ID); err != nil {
		t.Fatalf("PinMessage(m2): %v", err)
	}
	// Capture m1's position, then repoint m1 → m3.
	before, err := s.PinnedEntries(ctx, ch)
	if err != nil {
		t.Fatalf("PinnedEntries(before): %v", err)
	}
	var m1pos int32
	for _, e := range before {
		if e.MessageID == m1 {
			m1pos = e.Position
		}
	}

	after, err := s.PinMessage(ctx, ch, m3, m1, owner.ID)
	if err != nil {
		t.Fatalf("PinMessage(repoint m1→m3): %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("repoint changed the pin count to %d, want 2", len(after))
	}
	var found bool
	for _, e := range after {
		if e.MessageID == m1 {
			t.Fatalf("m1 still pinned after repoint")
		}
		if e.MessageID == m3 {
			found = true
			if e.Position != m1pos {
				t.Fatalf("m3 landed at position %d, want m1's old %d", e.Position, m1pos)
			}
		}
	}
	if !found {
		t.Fatalf("m3 not present after repoint: %v", pinnedIDs(after))
	}
}

// TestPinMessageRepointStaleFailsCAS pins the CAS failure path: a replace naming
// a message that is NOT currently pinned fails in-band (board changed), and the
// board is untouched.
func TestPinMessageRepointStaleFailsCAS(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	ch := mustNamedChannel(t, s, owner.ID, "room").ID
	m1 := mustPinnableMessage(t, s, ch, owner.ID, "one")
	m2 := mustPinnableMessage(t, s, ch, owner.ID, "two")
	m3 := mustPinnableMessage(t, s, ch, owner.ID, "three")

	if _, err := s.PinMessage(ctx, ch, m1, MessageID(""), owner.ID); err != nil {
		t.Fatalf("PinMessage(m1): %v", err)
	}
	// m2 was never pinned → repoint m2→m3 loses the CAS.
	_, err := s.PinMessage(ctx, ch, m3, m2, owner.ID)
	sentinelIs(t, err, ErrConflict, "repoint of a no-longer-pinned message")

	entries, err := s.PinnedEntries(ctx, ch)
	if err != nil {
		t.Fatalf("PinnedEntries: %v", err)
	}
	got := pinnedIDs(entries)
	if len(got) != 1 || got[0] != m1 {
		t.Fatalf("board = %v after a failed CAS, want [%s] unchanged", got, m1)
	}
}

// TestPinMessageCapRejectsSixth pins the per-channel cap: with maxChannelPins
// already pinned, the cap+1-th fresh pin is refused in-band and the existing set
// is untouched.
func TestPinMessageCapRejectsSixth(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	ch := mustNamedChannel(t, s, owner.ID, "room").ID

	for i := range maxChannelPins {
		m := mustPinnableMessage(t, s, ch, owner.ID, "msg")
		if _, err := s.PinMessage(ctx, ch, m, MessageID(""), owner.ID); err != nil {
			t.Fatalf("PinMessage(#%d): %v", i+1, err)
		}
	}
	over := mustPinnableMessage(t, s, ch, owner.ID, "over cap")
	_, err := s.PinMessage(ctx, ch, over, MessageID(""), owner.ID)
	sentinelIs(t, err, ErrFailedPrecondition, "pinning past the cap")

	entries, err := s.PinnedEntries(ctx, ch)
	if err != nil {
		t.Fatalf("PinnedEntries: %v", err)
	}
	if len(entries) != maxChannelPins {
		t.Fatalf("board has %d entries after a rejected over-cap pin, want %d", len(entries), maxChannelPins)
	}
}

// TestPinMessageConcurrentCapEdge drives the FOR UPDATE serialization: with
// maxChannelPins-1 already pinned, two goroutines each race to add the last slot
// (the two would take it to cap and cap+1). The one channels-row lock serializes
// them, so exactly one is admitted and the final count is exactly the cap. Runs
// under -race.
func TestPinMessageConcurrentCapEdge(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	ch := mustNamedChannel(t, s, owner.ID, "room").ID

	for i := range maxChannelPins - 1 {
		m := mustPinnableMessage(t, s, ch, owner.ID, "seed")
		if _, err := s.PinMessage(ctx, ch, m, MessageID(""), owner.ID); err != nil {
			t.Fatalf("seed PinMessage(#%d): %v", i+1, err)
		}
	}
	// Two candidate messages, pre-created so the pin ops race only on the board.
	c1 := mustPinnableMessage(t, s, ch, owner.ID, "cand1")
	c2 := mustPinnableMessage(t, s, ch, owner.ID, "cand2")

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	cands := []MessageID{c1, c2}
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = s.PinMessage(ctx, ch, cands[i], MessageID(""), owner.ID)
		}(i)
	}
	close(start)
	wg.Wait()

	admitted, rejected := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, ErrFailedPrecondition):
			rejected++
		default:
			t.Fatalf("unexpected error from concurrent pin: %v", err)
		}
	}
	if admitted != 1 || rejected != 1 {
		t.Fatalf("concurrent cap-edge: %d admitted, %d rejected; want exactly 1 each", admitted, rejected)
	}
	entries, err := s.PinnedEntries(ctx, ch)
	if err != nil {
		t.Fatalf("PinnedEntries: %v", err)
	}
	if len(entries) != maxChannelPins {
		t.Fatalf("final board has %d entries, want exactly the cap %d", len(entries), maxChannelPins)
	}
}

// TestUnpinMessageRemoves pins the unpin path: unpinning a pinned message removes
// it and the remaining entries reflect that; unpinning a non-pinned message is an
// in-band no-op (the board is unchanged), the documented UnpinMessage semantics.
func TestUnpinMessageRemoves(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	ch := mustNamedChannel(t, s, owner.ID, "room").ID
	m1 := mustPinnableMessage(t, s, ch, owner.ID, "one")
	m2 := mustPinnableMessage(t, s, ch, owner.ID, "two")

	if _, err := s.PinMessage(ctx, ch, m1, MessageID(""), owner.ID); err != nil {
		t.Fatalf("PinMessage(m1): %v", err)
	}
	if _, err := s.PinMessage(ctx, ch, m2, MessageID(""), owner.ID); err != nil {
		t.Fatalf("PinMessage(m2): %v", err)
	}

	remaining, err := s.UnpinMessage(ctx, ch, m1, owner.ID)
	if err != nil {
		t.Fatalf("UnpinMessage(m1): %v", err)
	}
	got := pinnedIDs(remaining)
	if len(got) != 1 || got[0] != m2 {
		t.Fatalf("after unpin board = %v, want [%s]", got, m2)
	}

	// Unpinning a message that is not pinned is a no-op: same board back.
	afterNoop, err := s.UnpinMessage(ctx, ch, m1, owner.ID)
	if err != nil {
		t.Fatalf("UnpinMessage(non-pinned): %v", err)
	}
	if ids := pinnedIDs(afterNoop); len(ids) != 1 || ids[0] != m2 {
		t.Fatalf("no-op unpin changed the board to %v, want [%s]", ids, m2)
	}
}
