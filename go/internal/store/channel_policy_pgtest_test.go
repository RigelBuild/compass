//go:build pgtest

package store

// Channel-policy store contracts (RIG-1722 T4, design.md:488-528): the post
// policy (OWNER_ONLY rejects a non-owner with the SAME ErrNotFound a non-member
// gets — no oracle), the mandatory-subscription flag (an explicit unsubscribe is
// refused, and the D1 read-side delivers to a member whose row says
// subscribed=false), and SetChannelPolicy's transactional seed of every member's
// delivery cursor when the mandatory flag is newly set (no un-seeded delivery
// target). These are properties only a real Postgres proves (the enforcement SQL
// and the in-txn seed), so the file is pgtest-tagged.
import (
	"context"
	"testing"
	"time"
)

// mustPolicyChannel creates a channel owned+operated by owner with the given
// initial policy and extra members.
func mustPolicyChannel(t *testing.T, s *Store, owner AccountID, name string, p ChannelPolicy, members ...AccountID) Channel {
	t.Helper()
	ch, err := s.CreateChannel(context.Background(), owner, NewChannel{
		Name: name, Kind: ChannelKindChannel, MemberAccountIDs: members, Policy: p,
	})
	if err != nil {
		t.Fatalf("CreateChannel(%q): %v", name, err)
	}
	return ch
}

// TestPostMessageOwnerOnlyRejectsNonOwnerInBand pins the OWNER_ONLY post gate: a
// member who is not the owner is refused with ErrNotFound — the exact error a
// non-member gets — so the policy leaks no oracle (a member who may not post is
// indistinguishable from a non-member).
func TestPostMessageOwnerOnlyRejectsNonOwnerInBand(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	other := mustUser(t, s, "other")
	ch := mustPolicyChannel(t, s, owner.ID, "locked", ChannelPolicy{
		PostPolicy:     ChannelPostPolicyOwnerOnly,
		OwnerAccountID: owner.ID,
	}, other.ID)

	// `other` is a member (read access) but not the owner: its post is refused
	// with the same not-found a non-member would get.
	_, _, err := s.AppendMessage(ctx, Message{
		AuthorAccountID: other.ID, Blocks: []MessageBlock{textBlock("not allowed")},
	}, string(ch.ID), TopicRef{Name: "general", Create: true}, "")
	sentinelIs(t, err, ErrNotFound, "non-owner post on OWNER_ONLY channel")
}

// TestPostMessageOwnerOnlyOwnerPostLands is the positive companion: the owner's
// own post on an OWNER_ONLY channel succeeds.
func TestPostMessageOwnerOnlyOwnerPostLands(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	ch := mustPolicyChannel(t, s, owner.ID, "locked", ChannelPolicy{
		PostPolicy:     ChannelPostPolicyOwnerOnly,
		OwnerAccountID: owner.ID,
	})

	m, inserted, err := s.AppendMessage(ctx, Message{
		AuthorAccountID: owner.ID, Blocks: []MessageBlock{textBlock("owner speaks")},
	}, string(ch.ID), TopicRef{Name: "general", Create: true}, "")
	if err != nil {
		t.Fatalf("AppendMessage(owner): %v", err)
	}
	if !inserted || m.ID == "" {
		t.Fatalf("owner post: inserted=%v id=%q, want a real insert", inserted, m.ID)
	}
}

// TestUpdateChannelMembersUnsubscribeRejectedOnMandatory pins the
// mandatory-subscription guard: an explicit unsubscribe on a mandatory channel
// is ErrInvalidArgument, while a plain add (subscribed=false but not an
// unsubscribe) on the same channel is unaffected.
func TestUpdateChannelMembersUnsubscribeRejectedOnMandatory(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	member := mustUser(t, s, "member")
	ch := mustPolicyChannel(t, s, owner.ID, "mandatory", ChannelPolicy{
		MandatorySubscription: true,
	}, member.ID)

	// An explicit unsubscribe of member is refused.
	_, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: member.ID, Unsubscribe: true},
	}, MemberUpdatesOptions{})
	sentinelIs(t, err, ErrInvalidArgument, "unsubscribe on mandatory channel")

	// A plain add (not an unsubscribe) still works: the guard is scoped to the
	// unsubscribe arm.
	newMember := mustUser(t, s, "newcomer")
	if _, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: newMember.ID},
	}, MemberUpdatesOptions{}); err != nil {
		t.Fatalf("plain add on mandatory channel: %v", err)
	}
}

// TestUndeliveredMessagesReachesUnsubscribedMandatoryMember is the read-side
// guarantee: a mandatory channel delivers to an agent member whose
// channel_members row says subscribed=false — the D1 mandatory disjunct,
// independent of the stored flag.
func TestUndeliveredMessagesReachesUnsubscribedMandatoryMember(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	author := mustUser(t, s, "author")
	recip := mustAgent(t, s, owner.ID, "recip")
	ch := mustPolicyChannel(t, s, owner.ID, "mandatory", ChannelPolicy{
		MandatorySubscription: true,
	}, author.ID, recip.ID)

	// The agent's member row is explicitly NOT subscribed, and this is not its
	// home channel — only the mandatory disjunct can carry the deliver.
	unsubscribeMember(t, s, ch.ID, recip.ID)
	// Seed the cursor to head so the message posted after is genuinely owed.
	if err := seedCursorNow(t, s, recip.ID, ch.ID); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}

	msgID, _ := postAs(t, s, ch.ID, author.ID, "for the mandatory member")

	owed, err := s.UndeliveredMessages(ctx, recip.ID)
	if err != nil {
		t.Fatalf("UndeliveredMessages: %v", err)
	}
	msgs := owed[ch.ID]
	if len(msgs) != 1 || string(msgs[0].ID) != msgID {
		t.Fatalf("owed on mandatory channel = %v, want the one message %q (delivered despite subscribed=false)", msgs, msgID)
	}
}

// TestSetChannelPolicySeedsCursorsForNewlyMandatory pins the fail-DANGEROUS D2
// hazard closure: flipping mandatory_subscription=true on a channel with members
// who have no cursor row seeds a cursor for each agent member in the SAME txn as
// the flag flip — after the call there is no un-seeded delivery target.
func TestSetChannelPolicySeedsCursorsForNewlyMandatory(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a1 := mustAgent(t, s, owner.ID, "a1")
	a2 := mustAgent(t, s, owner.ID, "a2")
	// A non-mandatory channel with two agent members, neither subscribed — so
	// neither has a delivery cursor row yet. author is a member so it can post
	// the pre-flip head.
	author := mustUser(t, s, "author")
	ch := mustPolicyChannel(t, s, owner.ID, "coord", ChannelPolicy{}, author.ID, a1.ID, a2.ID)
	unsubscribeMember(t, s, ch.ID, a1.ID)
	unsubscribeMember(t, s, ch.ID, a2.ID)

	// Precondition: no cursor rows exist for the agents on this channel.
	for _, a := range []AccountID{a1.ID, a2.ID} {
		if _, _, ok := readCursor(t, s, a, ch.ID); ok {
			t.Fatalf("precondition: agent %s already has a cursor on %s", a, ch.ID)
		}
	}

	// A message BEFORE the flip establishes the head the seed must catch up to.
	_, headSeq := postAs(t, s, ch.ID, author.ID, "before the flip")

	// Flip mandatory on.
	updated, err := s.SetChannelPolicy(ctx, owner.ID, ch.ID, ChannelPolicy{
		MandatorySubscription: true,
	})
	if err != nil {
		t.Fatalf("SetChannelPolicy: %v", err)
	}
	if !updated.Policy.MandatorySubscription {
		t.Fatalf("updated channel mandatory = false, want true")
	}

	// Every agent member is now a delivery target WITH a seeded cursor, seeded TO
	// HEAD (acked == pre-flip head, NOT 0 — no backlog replay), and owed nothing
	// (caught-up). A regression that seeded acked=0 would replay the pre-flip
	// message.
	for _, a := range []AccountID{a1.ID, a2.ID} {
		acked, _, ok := readCursor(t, s, a, ch.ID)
		if !ok {
			t.Fatalf("agent %s has no cursor after mandatory flip — an un-seeded delivery target (the D2 hazard)", a)
		}
		if acked != headSeq {
			t.Fatalf("agent %s cursor acked_seq = %d, want head %d (seed-to-head, no replay)", a, acked, headSeq)
		}
		owed, err := s.UndeliveredMessages(ctx, a)
		if err != nil {
			t.Fatalf("UndeliveredMessages(%s): %v", a, err)
		}
		if len(owed[ch.ID]) != 0 {
			t.Fatalf("agent %s owed %d messages after seed-to-head, want 0 (no backlog replay)", a, len(owed[ch.ID]))
		}
	}
}

// TestSetChannelPolicyMandatoryDoesNotSeedHumanMember pins the set-based seed's
// agent-only guard (the JOIN agent_accounts): flipping mandatory on a channel
// with both a human and an agent member seeds ONLY the agent — the human member
// gets no cursor row. This fails if the JOIN were dropped or loosened to seed
// every member.
func TestSetChannelPolicyMandatoryDoesNotSeedHumanMember(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	agent := mustAgent(t, s, owner.ID, "a1")
	human := mustUser(t, s, "human")
	// A non-mandatory channel with one agent and one human member, neither
	// subscribed — so neither has a delivery cursor row yet.
	ch := mustPolicyChannel(t, s, owner.ID, "coord", ChannelPolicy{}, agent.ID, human.ID)
	unsubscribeMember(t, s, ch.ID, agent.ID)
	unsubscribeMember(t, s, ch.ID, human.ID)

	// Precondition: no cursor rows exist for either member on this channel.
	for _, a := range []AccountID{agent.ID, human.ID} {
		if _, _, ok := readCursor(t, s, a, ch.ID); ok {
			t.Fatalf("precondition: member %s already has a cursor on %s", a, ch.ID)
		}
	}

	if _, err := s.SetChannelPolicy(ctx, owner.ID, ch.ID, ChannelPolicy{
		MandatorySubscription: true,
	}); err != nil {
		t.Fatalf("SetChannelPolicy: %v", err)
	}

	// The agent member is seeded (a delivery target); the human member is NOT —
	// the agent-only JOIN admits no human_account row.
	if _, _, ok := readCursor(t, s, agent.ID, ch.ID); !ok {
		t.Fatalf("agent %s has no cursor after mandatory flip — an un-seeded delivery target", agent.ID)
	}
	if _, _, ok := readCursor(t, s, human.ID, ch.ID); ok {
		t.Fatalf("human %s got a cursor after mandatory flip — the agent-only JOIN was bypassed", human.ID)
	}
}

// TestCreateChannelBornMandatorySeedsCursors pins the create-with-policy path's
// D2-hazard closure: a channel created with Policy.MandatorySubscription=true
// makes every member a delivery target (D1 disjunct), so CreateChannel MUST seed
// each agent member's cursor in the create txn — else the channel is born with
// un-seeded delivery targets — and a human member gets none (the seed is
// agent-only). Symmetric with the SetChannelPolicy flip and its human test.
func TestCreateChannelBornMandatorySeedsCursors(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a1 := mustAgent(t, s, owner.ID, "a1")
	a2 := mustAgent(t, s, owner.ID, "a2")
	// A human member proves the born-mandatory seed is agent-only on the create
	// path too (symmetric with the SetChannelPolicy human test): the JOIN
	// agent_accounts admits no human row.
	human := mustUser(t, s, "human")

	ch := mustPolicyChannel(t, s, owner.ID, "coord", ChannelPolicy{
		MandatorySubscription: true,
	}, a1.ID, a2.ID, human.ID)

	// Every agent member has a seeded cursor from birth (at head 0, an empty
	// channel) and is caught-up (owed nothing) — no un-seeded delivery target,
	// and no spurious backlog. A regression that skipped the seed would leave the
	// member with no cursor row (the absent-cursor fail-safe hazard).
	for _, a := range []AccountID{a1.ID, a2.ID} {
		acked, _, ok := readCursor(t, s, a, ch.ID)
		if !ok {
			t.Fatalf("agent %s has no cursor after born-mandatory create — an un-seeded delivery target (the D2 hazard)", a)
		}
		if acked != 0 {
			t.Fatalf("agent %s cursor acked_seq = %d on empty channel, want head 0", a, acked)
		}
		owed, err := s.UndeliveredMessages(ctx, a)
		if err != nil {
			t.Fatalf("UndeliveredMessages(%s): %v", a, err)
		}
		if len(owed[ch.ID]) != 0 {
			t.Fatalf("agent %s owed %d messages after born-mandatory seed, want 0", a, len(owed[ch.ID]))
		}
	}

	// The human member is NOT a delivery target — the agent-only JOIN admits no
	// human row, so it has no cursor (symmetric with the SetChannelPolicy path).
	if _, _, ok := readCursor(t, s, human.ID, ch.ID); ok {
		t.Fatalf("human %s got a cursor after born-mandatory create — the agent-only JOIN was bypassed", human.ID)
	}
}

// TestSetChannelPolicyNonOwnerOnOwnedChannelIsNotFound pins Matt's owner-only
// gate: on a channel that already has an owner, a member who is not the owner is
// refused with the SAME ErrNotFound a non-member gets (the not-found/forbidden
// no-oracle merge, mirroring PostMessage), and the stored policy is unchanged —
// so a member cannot reassign ownership to itself and bypass the OWNER_ONLY
// post-gate.
func TestSetChannelPolicyNonOwnerOnOwnedChannelIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	member := mustUser(t, s, "member")
	ch := mustPolicyChannel(t, s, owner.ID, "owned", ChannelPolicy{
		PostPolicy:     ChannelPostPolicyOwnerOnly,
		OwnerAccountID: owner.ID,
	}, member.ID)

	// The member (not the owner) tries to seize policy + ownership.
	_, err := s.SetChannelPolicy(ctx, member.ID, ch.ID, ChannelPolicy{
		PostPolicy:     ChannelPostPolicyOwnerOnly,
		OwnerAccountID: member.ID,
	})
	sentinelIs(t, err, ErrNotFound, "non-owner SetChannelPolicy on owned channel")

	// The policy did NOT change: owner is still the original owner.
	got, err := s.getChannel(ctx, ch.ID)
	if err != nil {
		t.Fatalf("getChannel: %v", err)
	}
	if got.Policy.OwnerAccountID != owner.ID {
		t.Fatalf("owner after refused set = %q, want unchanged %q", got.Policy.OwnerAccountID, owner.ID)
	}
}

// TestSetChannelPolicyMemberEstablishesPolicyOnOwnerlessChannel pins the
// create-or-update first-owner path: an ownerless (OPEN, empty owner) channel
// has no owner to be yet, so any member may set the first policy + owner.
func TestSetChannelPolicyMemberEstablishesPolicyOnOwnerlessChannel(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	creator := mustUser(t, s, "creator")
	member := mustUser(t, s, "member")
	// An OPEN, ownerless channel with an extra member.
	ch := mustPolicyChannel(t, s, creator.ID, "open", ChannelPolicy{}, member.ID)

	// The (non-creator) member establishes policy + ownership on the ownerless
	// channel — allowed because there is no established owner yet.
	updated, err := s.SetChannelPolicy(ctx, member.ID, ch.ID, ChannelPolicy{
		PostPolicy:     ChannelPostPolicyOwnerOnly,
		OwnerAccountID: member.ID,
	})
	if err != nil {
		t.Fatalf("SetChannelPolicy(member establishes on ownerless): %v", err)
	}
	if updated.Policy.OwnerAccountID != member.ID || updated.Policy.PostPolicy != ChannelPostPolicyOwnerOnly {
		t.Fatalf("established policy = {owner:%q policy:%d}, want {owner:%q OWNER_ONLY}", updated.Policy.OwnerAccountID, updated.Policy.PostPolicy, member.ID)
	}
}

// TestSetChannelPolicyOwnerCanUpdate pins that the established owner may change
// policy on its own channel.
func TestSetChannelPolicyOwnerCanUpdate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	ch := mustPolicyChannel(t, s, owner.ID, "owned", ChannelPolicy{
		PostPolicy:     ChannelPostPolicyOwnerOnly,
		OwnerAccountID: owner.ID,
	})

	updated, err := s.SetChannelPolicy(ctx, owner.ID, ch.ID, ChannelPolicy{
		PostPolicy:            ChannelPostPolicyOwnerOnly,
		OwnerAccountID:        owner.ID,
		MandatorySubscription: true,
	})
	if err != nil {
		t.Fatalf("SetChannelPolicy(owner update): %v", err)
	}
	if !updated.Policy.MandatorySubscription {
		t.Fatalf("owner update did not take: mandatory = false, want true")
	}
}

// TestSetChannelPolicyOwnerOnlyEmptyOwnerRejected pins the coherence guard:
// OWNER_ONLY with no owner account would brick the channel (the post gate
// coalesces the NULL owner to an empty string that matches no author), so it is
// refused with ErrInvalidArgument before the write.
func TestSetChannelPolicyOwnerOnlyEmptyOwnerRejected(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	ch := mustPolicyChannel(t, s, owner.ID, "open", ChannelPolicy{})

	_, err := s.SetChannelPolicy(ctx, owner.ID, ch.ID, ChannelPolicy{
		PostPolicy: ChannelPostPolicyOwnerOnly,
		// OwnerAccountID deliberately empty.
	})
	sentinelIs(t, err, ErrInvalidArgument, "OWNER_ONLY with empty owner")
}

// TestUpdateChannelMembersSeedsUnsubscribedAddOnMandatory pins the
// fail-DANGEROUS seed gap (MED #1): a plain (unsubscribed) add to an
// already-mandatory channel MUST seed the new member's delivery cursor, because
// a mandatory channel makes every member a delivery target (D1 disjunct). Absent
// the seed the absent-cursor fail-safe treats the member as permanently
// caught-up and it silently never receives anything. Asserts the cursor exists,
// is seeded TO HEAD (acked == pre-add head, no replay), and a message posted
// after the add is owed to the new member.
func TestUpdateChannelMembersSeedsUnsubscribedAddOnMandatory(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	author := mustUser(t, s, "author")
	late := mustAgent(t, s, owner.ID, "late")
	// A mandatory channel; author is a member so it may post the pre-add head.
	ch := mustPolicyChannel(t, s, owner.ID, "mandatory", ChannelPolicy{
		MandatorySubscription: true,
	}, author.ID)

	// A message BEFORE the add establishes the head the seed must catch up to.
	_, headSeq := postAs(t, s, ch.ID, author.ID, "before the add")

	// Plain add (subscribed defaults false) of the agent to the mandatory channel.
	if _, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: late.ID},
	}, MemberUpdatesOptions{}); err != nil {
		t.Fatalf("UpdateChannelMembers(plain add on mandatory): %v", err)
	}

	// The member has a seeded cursor, at head (no backlog replay of the pre-add
	// message).
	acked, _, ok := readCursor(t, s, late.ID, ch.ID)
	if !ok {
		t.Fatalf("late member has no cursor after add to mandatory channel — an un-seeded delivery target (the D2 hazard)")
	}
	if acked != headSeq {
		t.Fatalf("seeded cursor acked_seq = %d, want head %d (seed-to-head, no replay)", acked, headSeq)
	}

	// A message posted AFTER the add is genuinely owed to the new member.
	msgID, _ := postAs(t, s, ch.ID, author.ID, "after the add")
	owed, err := s.UndeliveredMessages(ctx, late.ID)
	if err != nil {
		t.Fatalf("UndeliveredMessages: %v", err)
	}
	msgs := owed[ch.ID]
	if len(msgs) != 1 || string(msgs[0].ID) != msgID {
		t.Fatalf("owed to late member = %v, want the one post-add message %q", msgs, msgID)
	}
}

// seedCursorNow seeds a delivery cursor for (agent, ch) to the current head in
// its own txn — a test setup helper for the read-side delivery case.
func seedCursorNow(t *testing.T, s *Store, agent AccountID, ch ChannelID) error {
	t.Helper()
	tx, err := s.pool.Begin(context.Background())
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := s.SeedDeliveryCursor(context.Background(), tx, agent, ch); err != nil {
		return err
	}
	return tx.Commit(context.Background())
}

// backendsBlockedBy counts the Postgres backends currently blocked waiting on a
// lock held by bpid — scoped to bpid so an unrelated concurrent test's lock
// waiter cannot spuriously satisfy the gate. Used as a deterministic readiness
// signal (poll-until, no sleep) that a concurrent writer has parked on the
// channels-row FOR UPDATE lock this test holds.
func backendsBlockedBy(t *testing.T, s *Store, bpid int) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_stat_activity a
		 WHERE a.pid <> $1 AND $1 = ANY(pg_blocking_pids(a.pid))`,
		bpid,
	).Scan(&n); err != nil {
		t.Fatalf("poll pg_blocking_pids: %v", err)
	}
	return n
}

// TestUpdateChannelMembersConcurrentFlipSeedsLateMember pins the seed-presence
// invariant under the concurrent flip+add race (the unlocked-read defect): a
// member added concurrently with a mandatory-subscription flip MUST end up with
// a seeded delivery cursor, seeded by exactly one of the two writers, never
// dropped by both. Absent the FOR UPDATE lock on the channels row, the losing
// interleave is: SetChannelPolicy flips mandatory=true and seeds all CURRENT
// members (not the not-yet-inserted new member M), while UpdateChannelMembers
// reads the stale committed mandatory=false and so skips seeding M — leaving M
// an unseeded member of a mandatory channel, which the absent-cursor fail-safe
// treats as permanently caught-up (silent delivery loss). The FOR UPDATE read
// serializes the two writers so M is always seeded.
//
// Deterministic, no sleeps: tx B replicates SetChannelPolicy's critical section
// (FOR UPDATE lock -> flip -> seed every current member) and holds it OPEN. The
// real UpdateChannelMembers runs as goroutine A. With the fix A's FOR UPDATE read
// blocks on B's lock (observed via pg_blocking_pids, scoped to B's backend pid);
// we then commit B and A re-reads mandatory=true and seeds M. Without the fix A's
// unlocked read sees the pre-commit mandatory=false, races ahead, and commits M
// unseeded; the gate poll simply times out and we commit B anyway. Either way
// both writers commit and the post-condition (M seeded) is the assertion — RED
// against the unlocked read, GREEN with FOR UPDATE.
func TestUpdateChannelMembersConcurrentFlipSeedsLateMember(t *testing.T) {
	ctx := context.Background() // test root
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	author := mustUser(t, s, "author")
	existing := mustAgent(t, s, owner.ID, "existing")
	late := mustAgent(t, s, owner.ID, "late")
	// A non-mandatory channel; author is a member so it may post the head, and
	// `existing` is an agent member that B will seed under the flip. `late` (M)
	// is NOT yet a member — A adds it concurrently with B's flip.
	ch := mustPolicyChannel(t, s, owner.ID, "flip-race", ChannelPolicy{}, author.ID, existing.ID)
	// A committed message establishes a non-zero head the seed must catch up to.
	postAs(t, s, ch.ID, author.ID, "before the race")

	// tx B: SetChannelPolicy's critical section, held open (uncommitted) so it is
	// the "first" writer holding the channels-row lock while A contends.
	txB, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx B: %v", err)
	}
	// Deferred rollback is a no-op after the Commit below; discarded because it
	// only runs on an early-return failure path (test-file cleanup exemption).
	defer func() { _ = txB.Rollback(ctx) }()

	var bpid int
	if err := txB.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&bpid); err != nil {
		t.Fatalf("read tx B backend pid: %v", err)
	}
	// Lock the channels row (same lock SetChannelPolicy takes), flip mandatory,
	// and seed every CURRENT agent member — M is not among them.
	var locked bool
	if err := txB.QueryRow(ctx,
		"SELECT mandatory_subscription FROM channels WHERE id = $1 FOR UPDATE", string(ch.ID),
	).Scan(&locked); err != nil {
		t.Fatalf("tx B lock channels row: %v", err)
	}
	if _, err := txB.Exec(ctx,
		"UPDATE channels SET mandatory_subscription = TRUE WHERE id = $1", string(ch.ID),
	); err != nil {
		t.Fatalf("tx B flip mandatory: %v", err)
	}
	if _, err := txB.Exec(ctx,
		`INSERT INTO agent_delivery_cursors (agent_account_id, channel_id, acked_seq)
		 SELECT cm.account_id, $1,
		        COALESCE((SELECT MAX(m.seq) FROM messages m JOIN topics t ON t.id = m.topic_id WHERE t.channel_id = $1), 0)
		 FROM channel_members cm
		 WHERE cm.channel_id = $1
		   AND EXISTS (SELECT 1 FROM agent_accounts WHERE account_id = cm.account_id)
		 ON CONFLICT (agent_account_id, channel_id) DO NOTHING`,
		string(ch.ID),
	); err != nil {
		t.Fatalf("tx B seed current members: %v", err)
	}

	// Goroutine A: the REAL UpdateChannelMembers add of M — the production read
	// under test. With the fix its FOR UPDATE read blocks on B's lock.
	done := make(chan error, 1)
	go func() {
		_, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{{AccountID: late.ID}}, MemberUpdatesOptions{})
		done <- err
	}()

	// Gate (poll-until, no sleep): wait until A has parked on B's lock (the fixed
	// path) or has already finished without blocking (the unlocked-read RED path).
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	var aErr error
	aDone := false
gate:
	for {
		if backendsBlockedBy(t, s, bpid) >= 1 {
			break gate // A is blocked on B's lock — the serialized path.
		}
		select {
		case aErr = <-done:
			aDone = true // A finished without ever blocking — the RED interleave.
			break gate
		case <-deadline:
			break gate
		case <-tick.C:
		}
	}

	// Release B: a blocked A unblocks, re-reads mandatory=TRUE, and seeds M.
	if err := txB.Commit(ctx); err != nil {
		t.Fatalf("commit tx B: %v", err)
	}
	if !aDone {
		aErr = <-done
	}
	if aErr != nil {
		t.Fatalf("concurrent UpdateChannelMembers(add M): %v", aErr)
	}

	// Post-condition: M ended up with a seeded delivery cursor. RED against the
	// unlocked read (M dropped by both writers), GREEN with FOR UPDATE.
	if _, _, ok := readCursor(t, s, late.ID, ch.ID); !ok {
		t.Fatalf("late member M has no delivery cursor after a concurrent mandatory flip — dropped by both writers (the unlocked-read race, permanent silent delivery loss)")
	}
	// And M is a live delivery target: a message posted after the race is owed.
	msgID, _ := postAs(t, s, ch.ID, author.ID, "after the race")
	owed, err := s.UndeliveredMessages(ctx, late.ID)
	if err != nil {
		t.Fatalf("UndeliveredMessages: %v", err)
	}
	msgs := owed[ch.ID]
	if len(msgs) != 1 || string(msgs[0].ID) != msgID {
		t.Fatalf("owed to late member = %v, want the one post-race message %q", msgs, msgID)
	}
}

// TestSetChannelPolicyOwnerNotMemberRejected pins coherence facet 1: setting an
// owner_account_id that is not a member of the channel is refused with
// ErrInvalidArgument (an OWNER_ONLY channel with a non-member owner is unpostable
// — the post gate demands the author be BOTH a member AND the owner), and the
// stored policy/owner is unchanged.
func TestSetChannelPolicyOwnerNotMemberRejected(t *testing.T) {
	ctx := context.Background() // test root
	s := newTestStore(t)
	member := mustUser(t, s, "member")
	outsider := mustUser(t, s, "outsider")
	// An OPEN, ownerless channel; `member` is a member, `outsider` is not.
	ch := mustPolicyChannel(t, s, member.ID, "ownerless", ChannelPolicy{})

	_, err := s.SetChannelPolicy(ctx, member.ID, ch.ID, ChannelPolicy{
		PostPolicy:     ChannelPostPolicyOwnerOnly,
		OwnerAccountID: outsider.ID,
	})
	sentinelIs(t, err, ErrInvalidArgument, "SetChannelPolicy owner not a member")

	got, err := s.getChannel(ctx, ch.ID)
	if err != nil {
		t.Fatalf("getChannel: %v", err)
	}
	if got.Policy.OwnerAccountID != "" || got.Policy.PostPolicy != ChannelPostPolicyOpen {
		t.Fatalf("policy after refused set = {owner:%q policy:%d}, want unchanged {owner:\"\" OPEN}", got.Policy.OwnerAccountID, got.Policy.PostPolicy)
	}
}

// TestSetChannelPolicyOpenWithOwnerRejected pins coherence facet 2: an OPEN
// channel must not name an owner account — owner-empty is the only legal state
// when OPEN (a non-empty owner would let a member silently claim the operator
// slot). Refused with ErrInvalidArgument.
func TestSetChannelPolicyOpenWithOwnerRejected(t *testing.T) {
	ctx := context.Background() // test root
	s := newTestStore(t)
	member := mustUser(t, s, "member")
	ch := mustPolicyChannel(t, s, member.ID, "ownerless", ChannelPolicy{})

	// The member is a member of its own ownerless channel, so it passes the owner
	// gate (no owner yet) and reaches the coherence guard — OPEN + non-empty owner.
	_, err := s.SetChannelPolicy(ctx, member.ID, ch.ID, ChannelPolicy{
		PostPolicy:     ChannelPostPolicyOpen,
		OwnerAccountID: member.ID,
	})
	sentinelIs(t, err, ErrInvalidArgument, "SetChannelPolicy OPEN with owner")
}

// TestCreateChannelOwnerOnlyNonMemberOwnerRejected pins coherence facet 1 at the
// create path: a channel born OWNER_ONLY with an owner not in its member set is
// unpostable from birth, so CreateChannel refuses it with ErrInvalidArgument.
func TestCreateChannelOwnerOnlyNonMemberOwnerRejected(t *testing.T) {
	ctx := context.Background() // test root
	s := newTestStore(t)
	creator := mustUser(t, s, "creator")
	outsider := mustUser(t, s, "outsider")

	_, err := s.CreateChannel(ctx, creator.ID, NewChannel{
		Name: "born-broken", Kind: ChannelKindChannel,
		Policy: ChannelPolicy{PostPolicy: ChannelPostPolicyOwnerOnly, OwnerAccountID: outsider.ID},
	})
	sentinelIs(t, err, ErrInvalidArgument, "CreateChannel OWNER_ONLY with non-member owner")
}

// TestCreateChannelOpenWithOwnerRejected pins coherence facet 2 at the create
// path: an OPEN channel must not name an owner account. Refused with
// ErrInvalidArgument.
func TestCreateChannelOpenWithOwnerRejected(t *testing.T) {
	ctx := context.Background() // test root
	s := newTestStore(t)
	creator := mustUser(t, s, "creator")

	_, err := s.CreateChannel(ctx, creator.ID, NewChannel{
		Name: "open-with-owner", Kind: ChannelKindChannel,
		Policy: ChannelPolicy{PostPolicy: ChannelPostPolicyOpen, OwnerAccountID: creator.ID},
	})
	sentinelIs(t, err, ErrInvalidArgument, "CreateChannel OPEN with owner")
}
