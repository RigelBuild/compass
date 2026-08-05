//go:build pgtest

package store

// Channel-policy store contracts (SEA-1722 T4, design.md:488-528): the post
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
	}, string(ch.ID), TopicRef{Name: "general"}, "")
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
	}, string(ch.ID), TopicRef{Name: "general"}, "")
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
	})
	sentinelIs(t, err, ErrInvalidArgument, "unsubscribe on mandatory channel")

	// A plain add (not an unsubscribe) still works: the guard is scoped to the
	// unsubscribe arm.
	newMember := mustUser(t, s, "newcomer")
	if _, _, err := s.UpdateChannelMembers(ctx, owner.ID, ch.ID, []MemberUpdate{
		{AccountID: newMember.ID},
	}); err != nil {
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
	flipSubscribed(t, s, ch.ID, recip.ID, false)
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
	// neither has a delivery cursor row yet.
	ch := mustPolicyChannel(t, s, owner.ID, "coord", ChannelPolicy{}, a1.ID, a2.ID)
	flipSubscribed(t, s, ch.ID, a1.ID, false)
	flipSubscribed(t, s, ch.ID, a2.ID, false)

	// Precondition: no cursor rows exist for the agents on this channel.
	for _, a := range []AccountID{a1.ID, a2.ID} {
		if _, _, ok := readCursor(t, s, a, ch.ID); ok {
			t.Fatalf("precondition: agent %s already has a cursor on %s", a, ch.ID)
		}
	}

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

	// Every agent member is now a delivery target WITH a seeded cursor — no
	// un-seeded delivery target survives the flip.
	for _, a := range []AccountID{a1.ID, a2.ID} {
		if _, _, ok := readCursor(t, s, a, ch.ID); !ok {
			t.Fatalf("agent %s has no cursor after mandatory flip — an un-seeded delivery target (the D2 hazard)", a)
		}
	}
}

// TestCreateChannelBornMandatorySeedsCursors pins the create-with-policy path's
// D2-hazard closure: a channel created with Policy.MandatorySubscription=true
// makes every member a delivery target (D1 disjunct), so CreateChannel MUST seed
// each agent member's cursor in the create txn — else the channel is born with
// un-seeded delivery targets. Symmetric with the SetChannelPolicy flip.
func TestCreateChannelBornMandatorySeedsCursors(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a1 := mustAgent(t, s, owner.ID, "a1")
	a2 := mustAgent(t, s, owner.ID, "a2")

	ch := mustPolicyChannel(t, s, owner.ID, "coord", ChannelPolicy{
		MandatorySubscription: true,
	}, a1.ID, a2.ID)

	// Every agent member has a seeded cursor from birth — no un-seeded delivery
	// target survives the mandatory create.
	for _, a := range []AccountID{a1.ID, a2.ID} {
		if _, _, ok := readCursor(t, s, a, ch.ID); !ok {
			t.Fatalf("agent %s has no cursor after born-mandatory create — an un-seeded delivery target (the D2 hazard)", a)
		}
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
