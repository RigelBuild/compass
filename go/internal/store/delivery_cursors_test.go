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
	"sync"
	"testing"
)

// postAs appends a message to a channel as the given author and returns its id
// and store-space seq. The author must already be a channel member.
func postAs(t *testing.T, s *Store, ch ChannelID, author AccountID, body string) (string, int64) {
	t.Helper()
	m, _, err := s.AppendMessage(context.Background(), Message{AuthorAccountID: author, Blocks: []MessageBlock{textBlock(body)}}, string(ch), TopicRef{Name: "general"}, "")
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
		`SELECT m.id, m.seq FROM messages m JOIN topics t ON t.id = m.topic_id WHERE t.channel_id = $1 AND m.author_account_id = $2 ORDER BY m.seq ASC LIMIT 1`,
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

// Case 11 — cross-channel gap: NO MESSAGE LOSS. Because messages.seq is a
// table-global BIGSERIAL, a message on another channel sits between two owed
// seqs on ch, so the contiguous cursor wedges at the gap and the higher owed
// seq stays in above_seqs. The point of this test is the SAFE invariant that
// the parked design question must never regress: even wedged, the sweep still
// returns every owed message (no loss). It also documents the current wedge.
func TestAckDeliveryCrossChannelGapIsLossless(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID

	// A second channel with a different member author, to inject a cross-channel
	// global-seq gap between the two owed messages on ch.
	other := mustNamedChannel(t, s, owner.ID, "other")

	// Owed A on ch, then a message on other (the gap), then owed B on ch: A.seq
	// and B.seq are non-contiguous, separated by the other-channel seq.
	mA, seqA := postAs(t, s, ch, owner.ID, "owed A")
	postAs(t, s, other.ID, owner.ID, "cross-channel gap")
	mB, seqB := postAs(t, s, ch, owner.ID, "owed B")

	if err := s.AckDelivery(ctx, agent.ID, ch, mA); err != nil {
		t.Fatalf("AckDelivery(mA): %v", err)
	}
	if err := s.AckDelivery(ctx, agent.ID, ch, mB); err != nil {
		t.Fatalf("AckDelivery(mB): %v", err)
	}

	// No-loss invariant: after acking both, ch has no owed messages — A drained
	// via the contiguous cursor, B is retained in above_seqs; both are excluded.
	got, err := s.UndeliveredMessages(ctx, agent.ID)
	if err != nil {
		t.Fatalf("UndeliveredMessages: %v", err)
	}
	if n := len(got[ch]); n != 0 {
		t.Fatalf("sweep returned %d owed on ch after acking A and B, want 0 (no loss)", n)
	}

	// Post a third owed message C: the sweep returns exactly [C], proving the
	// acked A/B are excluded and C is not lost.
	mC, _ := postAs(t, s, ch, owner.ID, "owed C")
	got, err = s.UndeliveredMessages(ctx, agent.ID)
	if err != nil {
		t.Fatalf("UndeliveredMessages (after C): %v", err)
	}
	owed := got[ch]
	if len(owed) != 1 || string(owed[0].ID) != mC {
		t.Fatalf("sweep = %v, want exactly [%s] (A/B acked, C owed)", owed, mC)
	}

	// Documents the parked cross-channel-wedge limitation (PR #55 Open Questions): the
	// contiguous cursor stops at the cross-channel gap and B stays in above_seqs.
	// Correctness is preserved (the sweep above is lossless); only boundedness is parked.
	acked, above, _ := readCursor(t, s, agent.ID, ch)
	if acked != seqA {
		t.Fatalf("acked=%d, want %d (cursor wedged at the cross-channel gap, did not drain to B)", acked, seqA)
	}
	if !equalInt64Set(above, []int64{seqB}) {
		t.Fatalf("above=%v, want [%d] (B retained above the wedge)", above, seqB)
	}
}

// Case 12 — concurrent acks don't lose a seq (defends FIX B / FOR UPDATE). Two
// concurrent acks for the same (agent, channel) do a read-modify-write on the
// cursor row; without FOR UPDATE the second UPDATE clobbers the first's
// above_seqs (a dropped acked seq → spurious redelivery). With FOR UPDATE the
// RMW serializes so both acked seqs are retained regardless of interleaving. A
// single two-goroutine race can interleave to green by luck even without the
// lock, so the contend section is repeated N times to make the lost-update catch
// reliable, each iteration a real concurrent RMW against a growing above_seqs.
func TestAckDeliveryConcurrentAcksRetainBothSeqs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID
	other := mustNamedChannel(t, s, owner.ID, "other")

	const iterations = 50
	ackedIDs := map[string]bool{}
	for iter := range iterations {
		// Fresh A and B owed on ch, with a cross-channel gap message between
		// them: the table-global BIGSERIAL seq puts an out-of-channel seq
		// between seqA and seqB, so at least one ack lands in above_seqs.
		mA, seqA := postAs(t, s, ch, owner.ID, "owed A")
		postAs(t, s, other.ID, owner.ID, "cross-channel gap")
		mB, seqB := postAs(t, s, ch, owner.ID, "owed B")

		// Gate both goroutines so they contend, then launch concurrently.
		var start, done sync.WaitGroup
		start.Add(1)
		done.Add(2)
		errs := make([]error, 2)
		for i, mID := range []string{mA, mB} {
			go func(i int, mID string) {
				defer done.Done()
				start.Wait()
				errs[i] = s.AckDelivery(ctx, agent.ID, ch, mID)
			}(i, mID)
		}
		start.Done()
		done.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("iter %d: concurrent AckDelivery[%d]: %v", iter, i, err)
			}
		}

		// No lost update: the union of acked_seq and above_seqs must cover BOTH
		// this iteration's seqA and seqB, regardless of the interleaving or which
		// seq drained into the contiguous cursor. A lost update drops one acked
		// seq from above_seqs, so the union would miss it.
		acked, above, _ := readCursor(t, s, agent.ID, ch)
		covered := map[int64]bool{acked: true}
		for _, sq := range above {
			covered[sq] = true
		}
		if !covered[seqA] || !covered[seqB] {
			t.Fatalf("iter %d: acked=%d above=%v does not cover both seqs %d and %d (lost update)", iter, acked, above, seqA, seqB)
		}
		ackedIDs[mA] = true
		ackedIDs[mB] = true
	}

	// And the sweep returns no message that was acked in any iteration.
	got, err := s.UndeliveredMessages(ctx, agent.ID)
	if err != nil {
		t.Fatalf("UndeliveredMessages: %v", err)
	}
	for _, m := range got[ch] {
		if ackedIDs[string(m.ID)] {
			t.Fatalf("sweep redelivered an acked message %s (lost update)", m.ID)
		}
	}
}

// Case 13 — re-subscribe never resets an existing cursor (design.md:293-304):
// the seed's ON CONFLICT DO NOTHING keeps the stale acked_seq, so backlog owed
// since the ORIGINAL seed still replays after an unsubscribe/re-subscribe. Pins
// the ratified DO-NOTHING choice against a silent future flip. No code change.
func TestReSubscribeDoesNotResetCursor(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	// A non-home channel so the subscribed flag governs the sweep (the home
	// channel sweeps regardless — Case 10). Owner creates and subscribes agent,
	// which seeds the cursor to the current head (0 messages yet).
	ch := mustNamedChannel(t, s, owner.ID, "room").ID
	subscribeAgent(t, s, owner.ID, ch, agent.ID)
	seededAcked, _, ok := readCursor(t, s, agent.ID, ch)
	if !ok {
		t.Fatal("precondition: subscribe should have seeded a cursor")
	}

	// M1 owed while subscribed.
	m1, _ := postAs(t, s, ch, owner.ID, "owed M1")

	// Unsubscribe, post M2 during the unsubscribed window, then re-subscribe.
	if _, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch, []MemberUpdate{
		{AccountID: agent.ID, Subscribed: false},
	}); err != nil {
		t.Fatalf("UpdateChannelMembers(unsubscribe): %v", err)
	}
	m2, _ := postAs(t, s, ch, owner.ID, "owed M2 during unsub")
	subscribeAgent(t, s, owner.ID, ch, agent.ID) // ON CONFLICT DO NOTHING: keeps the old cursor.

	// The cursor row was NOT reset to a new head by the re-subscribe.
	acked, _, _ := readCursor(t, s, agent.ID, ch)
	if acked != seededAcked {
		t.Fatalf("re-subscribe reset acked_seq to %d, want unchanged %d (DO NOTHING must not reset)", acked, seededAcked)
	}

	// The backlog owed since the original seed still replays — both M1 and M2.
	got, err := s.UndeliveredMessages(ctx, agent.ID)
	if err != nil {
		t.Fatalf("UndeliveredMessages: %v", err)
	}
	owed := got[ch]
	if len(owed) != 2 || string(owed[0].ID) != m1 || string(owed[1].ID) != m2 {
		t.Fatalf("sweep = %v, want ascending [%s %s] (backlog replays since original seed)", owed, m1, m2)
	}
}

// Case 14 — owed mention record → read (RIG-1641 T1): RecordOwedMention then
// OwedMentions returns the message under its channel.
func TestOwedMentionsRecordThenRead(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID

	msg, _ := postAs(t, s, ch, owner.ID, "you were mentioned")
	if err := s.RecordOwedMention(ctx, agent.ID, ch, msg); err != nil {
		t.Fatalf("RecordOwedMention: %v", err)
	}

	got, err := s.OwedMentions(ctx, agent.ID)
	if err != nil {
		t.Fatalf("OwedMentions: %v", err)
	}
	owed := got[ch]
	if len(owed) != 1 || string(owed[0].ID) != msg {
		t.Fatalf("OwedMentions[%s] = %v, want single %s", ch, owed, msg)
	}
}

// Case 15 — idempotent record (RIG-1641 T1): recording the same (agent, msg)
// twice is a no-op upsert (PK dedup); OwedMentions still returns exactly one.
func TestOwedMentionsRecordIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID

	msg, _ := postAs(t, s, ch, owner.ID, "mentioned once")
	if err := s.RecordOwedMention(ctx, agent.ID, ch, msg); err != nil {
		t.Fatalf("RecordOwedMention(first): %v", err)
	}
	if err := s.RecordOwedMention(ctx, agent.ID, ch, msg); err != nil {
		t.Fatalf("RecordOwedMention(second): %v", err)
	}

	got, err := s.OwedMentions(ctx, agent.ID)
	if err != nil {
		t.Fatalf("OwedMentions: %v", err)
	}
	if owed := got[ch]; len(owed) != 1 || string(owed[0].ID) != msg {
		t.Fatalf("OwedMentions[%s] = %v, want single %s after double record", ch, owed, msg)
	}
}

// Case 16 — ack clears an owed mention WITHOUT a cursor row (the gap population,
// RIG-1641 T1): the case the AckDelivery restructure exists for. An agent that
// is a MEMBER of the channel but has NO agent_delivery_cursors row for it (an
// unsubscribed non-home member) is owed a mention; AckDelivery must clear the
// owed row even though the cursor arm hits noRows and advances nothing. If the
// clear is placed wrong (rolled back with the no-cursor early return), this fails.
func TestAckDeliveryClearsOwedMentionWithoutCursor(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	// A channel the agent is a MEMBER of but NOT subscribed to — so no cursor is
	// seeded (seeding rides the subscribe txn). EnsureChannelMember adds the
	// agent as an unsubscribed member without seeding a cursor.
	ch := mustNamedChannel(t, s, owner.ID, "room").ID
	if err := s.EnsureChannelMember(ctx, ch, agent.ID); err != nil {
		t.Fatalf("EnsureChannelMember: %v", err)
	}
	if _, _, ok := readCursor(t, s, agent.ID, ch); ok {
		t.Fatal("precondition: agent unexpectedly has a delivery cursor for the channel")
	}

	msg, _ := postAs(t, s, ch, owner.ID, "@agent look here")
	if err := s.RecordOwedMention(ctx, agent.ID, ch, msg); err != nil {
		t.Fatalf("RecordOwedMention: %v", err)
	}

	if err := s.AckDelivery(ctx, agent.ID, ch, msg); err != nil {
		t.Fatalf("AckDelivery: %v", err)
	}

	got, err := s.OwedMentions(ctx, agent.ID)
	if err != nil {
		t.Fatalf("OwedMentions: %v", err)
	}
	if owed := got[ch]; len(owed) != 0 {
		t.Fatalf("owed mention not cleared by ack without a cursor row: %v", owed)
	}
}

// Case 17 — an unrelated ack leaves an owed mention (RIG-1641 T1): an owed row
// for msgA is untouched by AckDelivery for a different msgB in the same channel.
func TestAckDeliveryUnrelatedLeavesOwedMention(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID

	msgA, _ := postAs(t, s, ch, owner.ID, "owed A")
	msgB, _ := postAs(t, s, ch, owner.ID, "unrelated B")
	if err := s.RecordOwedMention(ctx, agent.ID, ch, msgA); err != nil {
		t.Fatalf("RecordOwedMention(A): %v", err)
	}

	if err := s.AckDelivery(ctx, agent.ID, ch, msgB); err != nil {
		t.Fatalf("AckDelivery(B): %v", err)
	}

	got, err := s.OwedMentions(ctx, agent.ID)
	if err != nil {
		t.Fatalf("OwedMentions: %v", err)
	}
	if owed := got[ch]; len(owed) != 1 || string(owed[0].ID) != msgA {
		t.Fatalf("unrelated ack disturbed owed row: OwedMentions[%s] = %v, want single %s", ch, owed, msgA)
	}
}

// Case 18 — ack clears an owed mention on the DUP-ACK arm (RIG-1641 T1): the
// second no-op arm that must still commit the clear (design §T1 lines 476-477).
// An agent whose cursor sits at or above the owed message's seq — owed while
// unsubscribed, then subscribed so the cursor seeds at head — acks that message.
// The seq resolves at/below acked_seq, so the cursor arm no-ops (advances
// nothing), but the owed-clear must still COMMIT. If the dup-ack arm were
// mis-edited to `return nil` (its pre-restructure behavior), the owed row would
// survive and re-deliver on every session start forever — and the suite would
// stay green without this case.
func TestAckDeliveryClearsOwedMentionOnDuplicateAck(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")

	// Post m1 as owner FIRST so it sits at the channel head, then subscribe the
	// agent — the subscribe seeds the cursor at head (= m1's seq), so an ack of
	// m1 lands on the dup-ack arm (seq <= acked_seq).
	ch := mustNamedChannel(t, s, owner.ID, "room").ID
	m1, seq1 := postAs(t, s, ch, owner.ID, "@agent look here")
	subscribeAgent(t, s, owner.ID, ch, agent.ID)
	if acked, _, ok := readCursor(t, s, agent.ID, ch); !ok || acked != seq1 {
		t.Fatalf("precondition: cursor should be seeded at head %d, got acked=%d ok=%v", seq1, acked, ok)
	}
	if err := s.RecordOwedMention(ctx, agent.ID, ch, m1); err != nil {
		t.Fatalf("RecordOwedMention: %v", err)
	}

	if err := s.AckDelivery(ctx, agent.ID, ch, m1); err != nil {
		t.Fatalf("AckDelivery: %v", err)
	}

	got, err := s.OwedMentions(ctx, agent.ID)
	if err != nil {
		t.Fatalf("OwedMentions: %v", err)
	}
	if owed := got[ch]; len(owed) != 0 {
		t.Fatalf("owed mention not cleared on dup-ack arm: %v", owed)
	}
	// The dup-ack arm must NOT advance the cursor — the clear is the only effect.
	if acked, _, ok := readCursor(t, s, agent.ID, ch); !ok || acked != seq1 {
		t.Fatalf("dup-ack arm advanced the cursor: acked=%d ok=%v, want unchanged %d", acked, ok, seq1)
	}
}

// Case 19 — ack clears an owed mention on the FULL-ADVANCE path (RIG-1641 T1):
// when the acked message is above the cursor, the single final commit must carry
// BOTH the cursor advance AND the owed-clear. Pins that the clear rides the
// full-advance commit (not just the two no-op arms).
func TestAckDeliveryClearsOwedMentionOnFullAdvance(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID // home cursor seeded at head 0 at agent creation.

	m1, seq1 := postAs(t, s, ch, owner.ID, "@agent above the cursor")
	if err := s.RecordOwedMention(ctx, agent.ID, ch, m1); err != nil {
		t.Fatalf("RecordOwedMention: %v", err)
	}

	if err := s.AckDelivery(ctx, agent.ID, ch, m1); err != nil {
		t.Fatalf("AckDelivery: %v", err)
	}

	got, err := s.OwedMentions(ctx, agent.ID)
	if err != nil {
		t.Fatalf("OwedMentions: %v", err)
	}
	if owed := got[ch]; len(owed) != 0 {
		t.Fatalf("owed mention not cleared on full-advance path: %v", owed)
	}
	// The cursor advanced to the acked seq on the same commit that cleared the row.
	if acked, above, ok := readCursor(t, s, agent.ID, ch); !ok || acked != seq1 || len(above) != 0 {
		t.Fatalf("full advance did not reach acked seq: acked=%d above=%v ok=%v, want acked=%d above=[]", acked, above, ok, seq1)
	}
}

// Case 18 — InSweepSet true for a subscribed member (RIG-1641 T2): an agent
// subscribed to a non-home channel is in the sweep set (the cursor sweep is its
// backstop), so no owed row is needed for an offline mention.
func TestInSweepSetTrueForSubscribed(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := mustNamedChannelWith(t, s, owner.ID, "shared", agent.ID)
	subscribeAgent(t, s, owner.ID, ch, agent.ID)

	in, err := s.InSweepSet(ctx, agent.ID, ch)
	if err != nil {
		t.Fatalf("InSweepSet: %v", err)
	}
	if !in {
		t.Fatal("InSweepSet = false for a subscribed member, want true")
	}
}

// Case 19 — InSweepSet true for the home channel (RIG-1641 T2): the home channel
// is always in the sweep set regardless of the subscribed flag (the D1 home
// disjunct), so a home-channel mention never needs an owed row.
func TestInSweepSetTrueForHomeChannel(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID
	// Flip the home row to subscribed=false: the home disjunct must still hold.
	unsubscribeMember(t, s, ch, agent.ID)

	in, err := s.InSweepSet(ctx, agent.ID, ch)
	if err != nil {
		t.Fatalf("InSweepSet: %v", err)
	}
	if !in {
		t.Fatal("InSweepSet = false for the home channel with subscribed=false, want true (home disjunct)")
	}
}

// Case 20 — InSweepSet false for an unsubscribed non-home member (RIG-1641 T2):
// the mention-gap population. Such a member has NO cursor-sweep backstop, so an
// offline mention to it needs a durable owed_mentions row.
func TestInSweepSetFalseForUnsubscribedNonHome(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := mustNamedChannelWith(t, s, owner.ID, "shared", agent.ID)
	unsubscribeMember(t, s, ch, agent.ID)

	in, err := s.InSweepSet(ctx, agent.ID, ch)
	if err != nil {
		t.Fatalf("InSweepSet: %v", err)
	}
	if in {
		t.Fatal("InSweepSet = true for an unsubscribed non-home member, want false (the mention gap)")
	}
}

// Case 21 — ClearOwedMention (pool-based, RIG-1641 T2): the sweep-path clear
// removes the owed row outside a txn; a subsequent OwedMentions read returns
// nothing, and clearing an absent row is a no-op.
func TestClearOwedMentionPoolBased(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID

	msg, _ := postAs(t, s, ch, owner.ID, "@agent paged")
	if err := s.RecordOwedMention(ctx, agent.ID, ch, msg); err != nil {
		t.Fatalf("RecordOwedMention: %v", err)
	}

	if err := s.ClearOwedMention(ctx, agent.ID, msg); err != nil {
		t.Fatalf("ClearOwedMention: %v", err)
	}
	got, err := s.OwedMentions(ctx, agent.ID)
	if err != nil {
		t.Fatalf("OwedMentions: %v", err)
	}
	if owed := got[ch]; len(owed) != 0 {
		t.Fatalf("owed mention not cleared by ClearOwedMention: %v", owed)
	}
	// Clearing again (now absent) is a no-op, not an error.
	if err := s.ClearOwedMention(ctx, agent.ID, msg); err != nil {
		t.Fatalf("ClearOwedMention(absent) = %v, want nil (no-op)", err)
	}
}

// Case 22 — CountOwedMentions (RIG-1641 T2 observability): the total row count
// across agents reflects records and clears.
func TestCountOwedMentions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID

	if n, err := s.CountOwedMentions(ctx); err != nil || n != 0 {
		t.Fatalf("CountOwedMentions(empty) = %d, %v, want 0, nil", n, err)
	}

	m1, _ := postAs(t, s, ch, owner.ID, "@agent one")
	m2, _ := postAs(t, s, ch, owner.ID, "@agent two")
	if err := s.RecordOwedMention(ctx, agent.ID, ch, m1); err != nil {
		t.Fatalf("RecordOwedMention(m1): %v", err)
	}
	if err := s.RecordOwedMention(ctx, agent.ID, ch, m2); err != nil {
		t.Fatalf("RecordOwedMention(m2): %v", err)
	}
	if n, err := s.CountOwedMentions(ctx); err != nil || n != 2 {
		t.Fatalf("CountOwedMentions(two) = %d, %v, want 2, nil", n, err)
	}

	if err := s.ClearOwedMention(ctx, agent.ID, m1); err != nil {
		t.Fatalf("ClearOwedMention: %v", err)
	}
	if n, err := s.CountOwedMentions(ctx); err != nil || n != 1 {
		t.Fatalf("CountOwedMentions(after clear) = %d, %v, want 1, nil", n, err)
	}
}

// RIG-2490 T1 — the pre-settle mention-loss marker column + scan surface.
// A fresh insert has a NULL marker and is returned by UnroutedMentionMessages
// (ascending seq > afterSeq, right channel); after MarkMentionsRouted it is
// excluded; a re-mark is a contract-level no-op (still excluded); limit bounds
// one batch and a follow-up read with afterSeq = the last returned seq excludes
// the already-read prefix (the batch-walk termination contract).
func TestUnroutedMentionMessagesScan(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "agent")
	ch := agent.Agent.HomeChannelID

	m1, seq1 := postAs(t, s, ch, owner.ID, "@agent one")
	m2, seq2 := postAs(t, s, ch, owner.ID, "@agent two")
	m3, seq3 := postAs(t, s, ch, owner.ID, "@agent three")

	// Fresh inserts: all three NULL, returned ascending seq, right channel.
	got, err := s.UnroutedMentionMessages(ctx, 0, 100)
	if err != nil {
		t.Fatalf("UnroutedMentionMessages(all): %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("UnroutedMentionMessages(all) = %d rows, want 3", len(got))
	}
	wantIDs := []string{m1, m2, m3}
	wantSeqs := []int64{seq1, seq2, seq3}
	for i, r := range got {
		if string(r.ID) != wantIDs[i] {
			t.Fatalf("row[%d].ID = %s, want %s", i, r.ID, wantIDs[i])
		}
		if r.Seq != wantSeqs[i] {
			t.Fatalf("row[%d].Seq = %d, want %d", i, r.Seq, wantSeqs[i])
		}
		if r.Channel != ch {
			t.Fatalf("row[%d].Channel = %s, want %s", i, r.Channel, ch)
		}
	}
	if got[0].Seq >= got[1].Seq || got[1].Seq >= got[2].Seq {
		t.Fatalf("rows not ascending by seq: %d, %d, %d", got[0].Seq, got[1].Seq, got[2].Seq)
	}

	// Mark m2 routed: it is excluded; m1 and m3 remain.
	if err := s.MarkMentionsRouted(ctx, m2); err != nil {
		t.Fatalf("MarkMentionsRouted(m2): %v", err)
	}
	got, err = s.UnroutedMentionMessages(ctx, 0, 100)
	if err != nil {
		t.Fatalf("UnroutedMentionMessages(after mark): %v", err)
	}
	if len(got) != 2 || string(got[0].ID) != m1 || string(got[1].ID) != m3 {
		t.Fatalf("after mark = %v, want [%s %s]", got, m1, m3)
	}

	// Re-mark m2: contract-level no-op — still excluded, count unchanged.
	if err := s.MarkMentionsRouted(ctx, m2); err != nil {
		t.Fatalf("MarkMentionsRouted(m2 re-mark): %v", err)
	}
	got, err = s.UnroutedMentionMessages(ctx, 0, 100)
	if err != nil {
		t.Fatalf("UnroutedMentionMessages(after re-mark): %v", err)
	}
	if len(got) != 2 || string(got[0].ID) != m1 || string(got[1].ID) != m3 {
		t.Fatalf("after re-mark = %v, want [%s %s]", got, m1, m3)
	}

	// Batch-walk termination: limit bounds one batch; a follow-up read with
	// afterSeq = the last returned seq excludes the already-read prefix.
	batch1, err := s.UnroutedMentionMessages(ctx, 0, 1)
	if err != nil {
		t.Fatalf("UnroutedMentionMessages(batch1): %v", err)
	}
	if len(batch1) != 1 || string(batch1[0].ID) != m1 {
		t.Fatalf("batch1 = %v, want single %s", batch1, m1)
	}
	batch2, err := s.UnroutedMentionMessages(ctx, batch1[0].Seq, 1)
	if err != nil {
		t.Fatalf("UnroutedMentionMessages(batch2): %v", err)
	}
	if len(batch2) != 1 || string(batch2[0].ID) != m3 {
		t.Fatalf("batch2 = %v, want single %s (m2 marked, excluded)", batch2, m3)
	}
	batch3, err := s.UnroutedMentionMessages(ctx, batch2[0].Seq, 1)
	if err != nil {
		t.Fatalf("UnroutedMentionMessages(batch3): %v", err)
	}
	if len(batch3) != 0 {
		t.Fatalf("batch3 = %v, want empty (walk terminated)", batch3)
	}
}
