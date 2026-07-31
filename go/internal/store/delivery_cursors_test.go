//go:build pgtest

package store

// Delivery-cursor contracts (SEA-1569 T2, design record D2): the durable
// per-(agent, channel) low-water cursor. AckDelivery resolves a message id to a
// seq (the overshoot clamp), records out-of-order acks in the above-set, and
// drains the contiguous cursor across acked-or-self-authored seqs; a duplicate,
// reordered, or fabricated ack is a no-op. SeedDeliveryCursor rides the
// member-insert txn (asserted through CreateAgent and an addOrUpdateMember
// subscribe). UndeliveredMessages sweeps the owed-but-unacked tail ascending,
// excluding the above-set and the agent's own posts, treating an absent cursor as
// caught-up (no history replay), and sweeping the home channel via the D1
// disjunct regardless of its subscribed flag. State survives a store restart.
// These are properties only a real Postgres proves, so the file is pgtest-tagged.

import (
	"context"
	"testing"
)

// postAs appends a message to a channel as the given author and returns its id
// and store-space seq. The author must already be a channel member.
func postAs(t *testing.T, s *Store, ch ChannelID, author AccountID, body string) (string, int64) {
	t.Helper()
	m, _, err := s.AppendMessage(context.Background(), Message{
		Container:       ContainerRef{ChannelID: ch},
		AuthorAccountID: author,
		Blocks:          []MessageBlock{textBlock(body)},
	}, "")
	if err != nil {
		t.Fatalf("AppendMessage(%q): %v", body, err)
	}
	return string(m.ID), messageSeq(t, s, string(m.ID))
}

// messageSeq reads a message's store-space seq directly, so a test can assert
// cursor arithmetic against concrete seq values.
func messageSeq(t *testing.T, s *Store, msgID string) int64 {
	t.Helper()
	var seq int64
	if err := s.pool.QueryRow(context.Background(),
		`SELECT seq FROM messages WHERE id = $1`, msgID,
	).Scan(&seq); err != nil {
		t.Fatalf("read seq for %s: %v", msgID, err)
	}
	return seq
}

// readCursor reads the raw cursor row for (agent, channel). ok is false when no
// row exists (the absent-cursor legacy fail-safe case).
func readCursor(t *testing.T, s *Store, agent AccountID, ch ChannelID) (ackedSeq int64, aboveSeqs []int64, ok bool) {
	t.Helper()
	err := s.pool.QueryRow(context.Background(),
		`SELECT acked_seq, above_seqs FROM agent_delivery_cursors
		 WHERE agent_account_id = $1 AND channel_id = $2`,
		string(agent), string(ch),
	).Scan(&ackedSeq, &aboveSeqs)
	if noRows(err) {
		return 0, nil, false
	}
	if err != nil {
		t.Fatalf("read cursor (%s,%s): %v", agent, ch, err)
	}
	return ackedSeq, aboveSeqs, true
}

// subscribeAgent adds an agent to a channel, subscribed — the addOrUpdateMember
// seed path. The actor (a member) drives the mutation.
func subscribeAgent(t *testing.T, s *Store, actor AccountID, ch ChannelID, agent AccountID) {
	t.Helper()
	if _, _, err := s.UpdateChannelMembers(context.Background(), actor, ch, []MemberUpdate{
		{AccountID: agent, Subscribed: true},
	}); err != nil {
		t.Fatalf("UpdateChannelMembers(subscribe agent): %v", err)
	}
}

// Case 1: a duplicate or reordered ack (at or below the contiguous cursor) is a
// no-op — the cursor is unchanged.
func TestAckDeliveryDuplicateIsNoOp(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID

	m1, seq1 := postAs(t, s, ch, owner.ID, "owed 1")

	if err := s.AckDelivery(ctx, agent.ID, ch, m1); err != nil {
		t.Fatalf("AckDelivery(first): %v", err)
	}
	acked, above, ok := readCursor(t, s, agent.ID, ch)
	if !ok || acked != seq1 || len(above) != 0 {
		t.Fatalf("after first ack: acked=%d above=%v ok=%v, want acked=%d above=[]", acked, above, ok, seq1)
	}

	// A second ack of the same (now-below-cursor) message changes nothing.
	if err := s.AckDelivery(ctx, agent.ID, ch, m1); err != nil {
		t.Fatalf("AckDelivery(duplicate): %v", err)
	}
	acked2, above2, _ := readCursor(t, s, agent.ID, ch)
	if acked2 != seq1 || len(above2) != 0 {
		t.Fatalf("duplicate ack moved cursor: acked=%d above=%v, want acked=%d above=[]", acked2, above2, seq1)
	}
}

// Case 2: an ack for a message never dispatched to this agent (a fabricated or
// foreign id) is a no-op — the resolution is the overshoot clamp.
func TestAckDeliveryFabricatedIDIsNoOp(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID

	_, seq1 := postAs(t, s, ch, owner.ID, "owed 1")
	// Seed the cursor to a known state by acking nothing; the seed at subscribe
	// leaves acked_seq at 0 (channel was empty at CreateAgent time).
	acked0, _, ok := readCursor(t, s, agent.ID, ch)
	if !ok {
		t.Fatal("home-channel cursor missing after CreateAgent")
	}

	// A fabricated id resolves to no row → no-op.
	if err := s.AckDelivery(ctx, agent.ID, ch, "no-such-message"); err != nil {
		t.Fatalf("AckDelivery(fabricated): %v", err)
	}
	acked1, above1, _ := readCursor(t, s, agent.ID, ch)
	if acked1 != acked0 || len(above1) != 0 {
		t.Fatalf("fabricated ack advanced cursor: acked=%d above=%v, want acked=%d above=[]", acked1, above1, acked0)
	}

	// A foreign id (a real message in ANOTHER channel) is likewise a no-op.
	other := mustNamedChannel(t, s, owner.ID, "other")
	foreignID, _ := postAs(t, s, other.ID, owner.ID, "elsewhere")
	if err := s.AckDelivery(ctx, agent.ID, ch, foreignID); err != nil {
		t.Fatalf("AckDelivery(foreign): %v", err)
	}
	acked2, above2, _ := readCursor(t, s, agent.ID, ch)
	if acked2 != acked0 || len(above2) != 0 {
		t.Fatalf("foreign ack advanced cursor: acked=%d above=%v, want acked=%d above=[]", acked2, above2, acked0)
	}
	_ = seq1
}

// Case 3: gap-fill — acking a higher seq retains it in above_seqs; acking the
// intervening owed seqs advances the contiguous cursor and drains the above-set.
func TestAckDeliveryGapFillDrainsAboveSet(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID

	m1, seq1 := postAs(t, s, ch, owner.ID, "owed 1")
	m2, seq2 := postAs(t, s, ch, owner.ID, "owed 2")
	m3, seq3 := postAs(t, s, ch, owner.ID, "owed 3")

	// Ack the highest first: it lands in above_seqs, cursor stays at seed (0).
	if err := s.AckDelivery(ctx, agent.ID, ch, m3); err != nil {
		t.Fatalf("AckDelivery(m3): %v", err)
	}
	acked, above, _ := readCursor(t, s, agent.ID, ch)
	if acked != 0 || !equalInt64Set(above, []int64{seq3}) {
		t.Fatalf("after acking m3: acked=%d above=%v, want acked=0 above=[%d]", acked, above, seq3)
	}

	// Ack m2: still a hole at seq1, so cursor stays; above holds {seq2, seq3}.
	if err := s.AckDelivery(ctx, agent.ID, ch, m2); err != nil {
		t.Fatalf("AckDelivery(m2): %v", err)
	}
	acked, above, _ = readCursor(t, s, agent.ID, ch)
	if acked != 0 || !equalInt64Set(above, []int64{seq2, seq3}) {
		t.Fatalf("after acking m2: acked=%d above=%v, want acked=0 above=[%d %d]", acked, above, seq2, seq3)
	}

	// Ack m1: fills the gap → contiguous drains all the way to seq3.
	if err := s.AckDelivery(ctx, agent.ID, ch, m1); err != nil {
		t.Fatalf("AckDelivery(m1): %v", err)
	}
	acked, above, _ = readCursor(t, s, agent.ID, ch)
	if acked != seq3 || len(above) != 0 {
		t.Fatalf("after gap-fill: acked=%d above=%v, want acked=%d above=[]", acked, above, seq3)
	}
	_ = seq1
}

// Case 4: an agent's own post interleaved below un-acked deliveries does not
// wedge the contiguous cursor (author-exclusion gap-fill), and above_seqs stays
// bounded across a run of self-posts.
func TestAckDeliverySelfPostsDoNotWedge(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID

	// Interleave: owner (owed), then a run of agent self-posts, then owner again.
	_, _ = postAs(t, s, ch, owner.ID, "owed A")      // seqA, owed
	postAs(t, s, ch, agent.ID, "self 1")             // self
	postAs(t, s, ch, agent.ID, "self 2")             // self
	postAs(t, s, ch, agent.ID, "self 3")             // self
	mB, seqB := postAs(t, s, ch, owner.ID, "owed B") // seqB, owed

	mA, seqA := firstOwed(t, s, ch, owner.ID)

	// Ack the later owed message: the intervening self-posts must not sit in the
	// above-set as un-drainable holes, and the cursor must not advance past the
	// still-owed seqA.
	if err := s.AckDelivery(ctx, agent.ID, ch, mB); err != nil {
		t.Fatalf("AckDelivery(mB): %v", err)
	}
	acked, above, _ := readCursor(t, s, agent.ID, ch)
	if acked != 0 {
		t.Fatalf("cursor advanced past un-acked owed seqA=%d: acked=%d", seqA, acked)
	}
	// above_seqs holds only the genuinely-acked seqB — self-posts are drained by
	// author-exclusion, never retained.
	if !equalInt64Set(above, []int64{seqB}) {
		t.Fatalf("above=%v, want exactly [%d] (self-posts must not linger)", above, seqB)
	}

	// Now ack the earlier owed message: the gap fills and the contiguous cursor
	// drains across seqA, the self-post run, and seqB.
	if err := s.AckDelivery(ctx, agent.ID, ch, mA); err != nil {
		t.Fatalf("AckDelivery(mA): %v", err)
	}
	acked, above, _ = readCursor(t, s, agent.ID, ch)
	if acked != seqB || len(above) != 0 {
		t.Fatalf("after gap-fill over self-posts: acked=%d above=%v, want acked=%d above=[]", acked, above, seqB)
	}
}

// Case 5: an absent cursor row (the legacy fail-safe) makes UndeliveredMessages
// sweep as caught-up to the channel head — no history replay from seq 0.
func TestUndeliveredMessagesAbsentCursorIsCaughtUp(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID

	// Post owed messages, then delete the cursor row to simulate a legacy agent
	// with membership but no cursor.
	postAs(t, s, ch, owner.ID, "pre 1")
	postAs(t, s, ch, owner.ID, "pre 2")
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM agent_delivery_cursors WHERE agent_account_id = $1 AND channel_id = $2`,
		string(agent.ID), string(ch),
	); err != nil {
		t.Fatalf("delete cursor: %v", err)
	}

	got, err := s.UndeliveredMessages(ctx, agent.ID)
	if err != nil {
		t.Fatalf("UndeliveredMessages: %v", err)
	}
	if n := len(got[ch]); n != 0 {
		t.Fatalf("absent-cursor sweep replayed %d messages, want 0 (caught-up, no replay)", n)
	}
}

// Case 6: a subscribe txn that inserts a channel_members row also seeds the
// delivery cursor in the same txn — asserted after CreateAgent (home channel)
// and after an addOrUpdateMember subscribe of an agent.
func TestSeedDeliveryCursorRidesMemberInsert(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	// CreateAgent home-channel seed.
	if _, _, ok := readCursor(t, s, agent.ID, agent.Agent.HomeChannelID); !ok {
		t.Fatal("CreateAgent did not seed the home-channel delivery cursor")
	}

	// addOrUpdateMember subscribe seed: subscribe the agent to a second channel.
	other := mustNamedChannel(t, s, owner.ID, "other")
	subscribeAgent(t, s, owner.ID, other.ID, agent.ID)
	if _, _, ok := readCursor(t, s, agent.ID, other.ID); !ok {
		t.Fatal("addOrUpdateMember subscribe did not seed the delivery cursor")
	}

	// A subscribed USER member must NOT get a cursor row (self-guarding seed:
	// agent-only via WHERE EXISTS, no FK violation).
	newcomer := mustUser(t, s, "newcomer")
	if _, _, err := s.UpdateChannelMembers(context.Background(), owner.ID, other.ID, []MemberUpdate{
		{AccountID: newcomer.ID, Subscribed: true},
	}); err != nil {
		t.Fatalf("UpdateChannelMembers(subscribe user): %v", err)
	}
	if _, _, ok := readCursor(t, s, newcomer.ID, other.ID); ok {
		t.Fatal("a user member wrongly got a delivery-cursor row")
	}
}

// Case 7: seed-at-subscribe yields an empty sweep — nothing posted before the
// subscribe replays.
func TestSeedAtSubscribeYieldsEmptySweep(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	// Post to a channel BEFORE the agent subscribes, then subscribe.
	other := mustNamedChannel(t, s, owner.ID, "other")
	postAs(t, s, other.ID, owner.ID, "before subscribe 1")
	postAs(t, s, other.ID, owner.ID, "before subscribe 2")
	subscribeAgent(t, s, owner.ID, other.ID, agent.ID)

	got, err := s.UndeliveredMessages(ctx, agent.ID)
	if err != nil {
		t.Fatalf("UndeliveredMessages: %v", err)
	}
	if n := len(got[other.ID]); n != 0 {
		t.Fatalf("seed-at-subscribe replayed %d pre-subscribe messages, want 0", n)
	}
}

// Case 8: the sweep returns exactly the un-acked gap, ascending, above-set
// excluded, the agent's own messages excluded, home channel included.
func TestUndeliveredMessagesSweepShape(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID // home channel included in the sweep.

	o1, _ := postAs(t, s, ch, owner.ID, "owed 1")
	postAs(t, s, ch, agent.ID, "self post")         // excluded: agent's own.
	o2ID, _ := postAs(t, s, ch, owner.ID, "owed 2") // will be acked → in above-set.
	o3, _ := postAs(t, s, ch, owner.ID, "owed 3")

	// Ack o2 out of order: it enters above_seqs and must be excluded from sweep.
	if err := s.AckDelivery(ctx, agent.ID, ch, o2ID); err != nil {
		t.Fatalf("AckDelivery(o2): %v", err)
	}

	got, err := s.UndeliveredMessages(ctx, agent.ID)
	if err != nil {
		t.Fatalf("UndeliveredMessages: %v", err)
	}
	owed := got[ch]
	if len(owed) != 2 {
		t.Fatalf("sweep returned %d messages, want 2 (owed 1, owed 3)", len(owed))
	}
	// Ascending seq order, exactly the two un-acked owed messages.
	if string(owed[0].ID) != o1 || string(owed[1].ID) != o3 {
		t.Fatalf("sweep = [%s %s], want ascending [%s %s]", owed[0].ID, owed[1].ID, o1, o3)
	}
	for _, m := range owed {
		if m.AuthorAccountID == agent.ID {
			t.Fatalf("sweep included the agent's own post %s", m.ID)
		}
	}
}

// Case 9: cursor state survives a store restart (reopen against the same DSN).
func TestDeliveryCursorSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	s, dsn := newTestStoreDSN(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID

	m1, seq1 := postAs(t, s, ch, owner.ID, "owed 1")
	m2, seq2 := postAs(t, s, ch, owner.ID, "owed 2")
	if err := s.AckDelivery(ctx, agent.ID, ch, m1); err != nil {
		t.Fatalf("AckDelivery(m1): %v", err)
	}
	if err := s.AckDelivery(ctx, agent.ID, ch, m2); err != nil {
		t.Fatalf("AckDelivery(m2): %v", err)
	}
	wantAcked, _, _ := readCursor(t, s, agent.ID, ch)
	if wantAcked != seq2 {
		t.Fatalf("precondition: acked=%d, want %d", wantAcked, seq2)
	}
	s.Close()

	s2 := reopenStore(t, dsn)
	acked, above, ok := readCursor(t, s2, agent.ID, ch)
	if !ok || acked != seq2 || len(above) != 0 {
		t.Fatalf("after restart: acked=%d above=%v ok=%v, want acked=%d above=[]", acked, above, ok, seq2)
	}
	// The reopened store sweeps the same (empty) owed set — cursor state is live.
	got, err := s2.UndeliveredMessages(ctx, agent.ID)
	if err != nil {
		t.Fatalf("UndeliveredMessages after restart: %v", err)
	}
	if n := len(got[ch]); n != 0 {
		t.Fatalf("after restart sweep returned %d, want 0 (both acked)", n)
	}
	_ = seq1
}

// firstOwed returns the id and seq of the earliest message authored by author in
// channel ch — the lowest-seq owed message, for the interleaving test.
func firstOwed(t *testing.T, s *Store, ch ChannelID, author AccountID) (string, int64) {
	t.Helper()
	var (
		id  string
		seq int64
	)
	if err := s.pool.QueryRow(context.Background(),
		`SELECT id, seq FROM messages WHERE channel_id = $1 AND author_account_id = $2 ORDER BY seq ASC LIMIT 1`,
		string(ch), string(author),
	).Scan(&id, &seq); err != nil {
		t.Fatalf("firstOwed(%s,%s): %v", ch, author, err)
	}
	return id, seq
}

// equalInt64Set reports whether got and want hold the same seq values, order
// insensitive (above_seqs is a set, not an ordered list).
func equalInt64Set(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[int64]int, len(got))
	for _, v := range got {
		seen[v]++
	}
	for _, v := range want {
		seen[v]--
		if seen[v] < 0 {
			return false
		}
	}
	return true
}

// Case 10: the home channel sweeps via the D1 disjunct regardless of its
// subscribed flag — a home channel_members row flipped subscribed=false STILL
// delivers an owed message (design.md:118-120, :127-128, :343, :708).
func TestUndeliveredMessagesHomeChannelSweepsWhenUnsubscribed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID

	// An owed message on the agent's home channel.
	owedID, _ := postAs(t, s, ch, owner.ID, "owed on home")

	// Flip the agent's home membership subscribed=false (addOrUpdateMember DO
	// UPDATE SET subscribed). The owner is a home-channel member and may mutate.
	if _, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch, []MemberUpdate{
		{AccountID: agent.ID, Subscribed: false},
	}); err != nil {
		t.Fatalf("UpdateChannelMembers(unsubscribe agent home): %v", err)
	}
	if memberSubscribed(t, s, ch, agent.ID) {
		t.Fatal("precondition: agent home membership should be subscribed=false")
	}

	// The frozen guarantee: the home channel still sweeps despite subscribed=false.
	got, err := s.UndeliveredMessages(ctx, agent.ID)
	if err != nil {
		t.Fatalf("UndeliveredMessages: %v", err)
	}
	owed := got[ch]
	if len(owed) != 1 || string(owed[0].ID) != owedID {
		t.Fatalf("home sweep = %v, want exactly [%s] (D1 disjunct, flag-independent)", owed, owedID)
	}
}
