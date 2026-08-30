//go:build pgtest

package store

// Peer-DM store contracts (RIG-2963 T2, design.md:691-741, Decisions R3/R4):
// the reserved per-owner DM group, the deterministic-name upsert (create /
// idempotent resume / concurrent-open race), the R3 create-guard + squat belt,
// and the R4 convert-on-add / two-party floor. These are properties only a real
// Postgres proves (the partial unique index, the in-tx cursor seeds, the
// delivery predicate, a concurrent race), so the file is pgtest-tagged.

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// openDM drives one peer-DM open the way the T3 OpenDM path will: under the
// per-owner DM advisory lock, ensure the owner's reserved group, then upsert the
// deterministic-name channel for the two agent parties. It runs the whole open in
// one WithTx so the lock, group, channel, members, and cursor seeds commit
// atomically — the same shape the real edge uses. Returns the channel id and
// whether it was created this call.
func openDM(t *testing.T, s *Store, ownerUserID AccountID, name string, members []AccountID) (ChannelID, bool) {
	t.Helper()
	var (
		id      ChannelID
		created bool
	)
	if err := s.WithTx(context.Background(), func(tx pgx.Tx) error {
		if err := LockOwnerDMTx(context.Background(), tx, ownerUserID); err != nil {
			return err
		}
		gid, err := s.EnsureOwnerDMGroupTx(context.Background(), tx, ownerUserID)
		if err != nil {
			return err
		}
		id, created, err = s.UpsertDMChannelTx(context.Background(), tx, DMChannelSpec{
			GroupID: gid, Name: name, Members: members,
		})
		return err
	}); err != nil {
		t.Fatalf("openDM(%q): %v", name, err)
	}
	return id, created
}

// dmGroupIDFor resolves the owner's reserved DM group id (the group openDM
// ensured), for tests that plant a row directly into it.
func dmGroupIDFor(t *testing.T, s *Store, ownerUserID AccountID) ChannelGroupID {
	t.Helper()
	var gid string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT id FROM channel_groups WHERE owner_user_id = $1 AND name = $2 AND parent_group_id IS NULL AND visibility = $3`,
		string(ownerUserID), dmGroupName, int32(VisibilityOwner),
	).Scan(&gid); err != nil {
		t.Fatalf("resolve dm group for %s: %v", ownerUserID, err)
	}
	return ChannelGroupID(gid)
}

// cursorExists reports whether an agent has a seeded delivery cursor on channel.
func cursorExists(t *testing.T, s *Store, agent AccountID, channel ChannelID) bool {
	t.Helper()
	var exists bool
	if err := s.pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM agent_delivery_cursors WHERE agent_account_id = $1 AND channel_id = $2)`,
		string(agent), string(channel),
	).Scan(&exists); err != nil {
		t.Fatalf("cursor probe (%s,%s): %v", agent, channel, err)
	}
	return exists
}

// TestUpsertDMChannelCreateInvariants pins the created DM's exact shape: kind=DM,
// zero/OPEN ownerless policy, mandatory_subscription=true, members = both agent
// parties + the pulled-in owner, and a seeded delivery cursor for each agent.
func TestUpsertDMChannelCreateInvariants(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "alice")
	b := mustAgent(t, s, owner.ID, "bob")

	id, created := openDM(t, s, owner.ID, "dm--alice--bob", []AccountID{a.ID, b.ID})
	if !created {
		t.Fatal("first open of a fresh pair reported created=false, want true")
	}

	ch, err := s.GetChannel(context.Background(), id)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if ch.Kind != ChannelKindDM {
		t.Fatalf("kind = %d, want ChannelKindDM", ch.Kind)
	}
	if ch.Policy.PostPolicy != ChannelPostPolicyOpen || ch.Policy.OwnerAccountID != "" {
		t.Fatalf("policy = %+v, want OPEN + ownerless (zero policy)", ch.Policy)
	}
	if !ch.Policy.MandatorySubscription {
		t.Fatal("mandatory_subscription = false, want true (DM is born mandatory)")
	}
	got := memberSet(ch)
	for _, want := range []AccountID{a.ID, b.ID, owner.ID} {
		if !got[want] {
			t.Fatalf("members %v missing %s", ch.MemberAccountIDs, want)
		}
	}
	if len(ch.MemberAccountIDs) != 3 {
		t.Fatalf("members = %v, want exactly both parties + owner", ch.MemberAccountIDs)
	}
	for _, agent := range []AccountID{a.ID, b.ID} {
		if !cursorExists(t, s, agent, id) {
			t.Fatalf("agent party %s has no seeded delivery cursor on the DM", agent)
		}
	}
}

// TestUpsertDMChannelResumeIsIdempotent pins that a second open of the same pair
// (in EITHER member order — the name is the direction-independent sorted-handle
// key, so the store takes a pre-sorted name and either member order resolves the
// same channel) resumes the SAME id with created=false, minting no second row.
func TestUpsertDMChannelResumeIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "alice")
	b := mustAgent(t, s, owner.ID, "bob")

	first, created := openDM(t, s, owner.ID, "dm--alice--bob", []AccountID{a.ID, b.ID})
	if !created {
		t.Fatal("first open reported created=false, want true")
	}
	// Re-open with the members in the REVERSED order — same deterministic name.
	second, created := openDM(t, s, owner.ID, "dm--alice--bob", []AccountID{b.ID, a.ID})
	if created {
		t.Fatal("re-open reported created=true, want false (resume)")
	}
	if second != first {
		t.Fatalf("re-open resolved a different channel: %s vs %s", second, first)
	}

	var count int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM channels c JOIN channel_groups g ON g.id = c.group_id
		   WHERE g.owner_user_id = $1 AND g.name = $2`,
		string(owner.ID), dmGroupName,
	).Scan(&count); err != nil {
		t.Fatalf("count dm channels: %v", err)
	}
	if count != 1 {
		t.Fatalf("owner has %d DM channels, want exactly 1", count)
	}
}

// TestUpsertDMChannelConcurrentOpenRace pins that two concurrent opens of the
// same pair mint exactly one channel and both return the same id — the ON
// CONFLICT DO NOTHING + re-SELECT resume loop absorbs the race. Each open runs on
// its own tx (its own advisory-lock take), so the loser blocks on the winner's
// (group, name) tuple, then resumes it.
func TestUpsertDMChannelConcurrentOpenRace(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "alice")
	b := mustAgent(t, s, owner.ID, "bob")

	const name = "dm--alice--bob"
	members := []AccountID{a.ID, b.ID}

	type result struct {
		id  ChannelID
		err error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	for i := range results {
		go func(i int) {
			defer wg.Done()
			<-start
			var r result
			r.err = s.WithTx(context.Background(), func(tx pgx.Tx) error {
				if err := LockOwnerDMTx(context.Background(), tx, owner.ID); err != nil {
					return err
				}
				gid, err := s.EnsureOwnerDMGroupTx(context.Background(), tx, owner.ID)
				if err != nil {
					return err
				}
				r.id, _, err = s.UpsertDMChannelTx(context.Background(), tx, DMChannelSpec{
					GroupID: gid, Name: name, Members: members,
				})
				return err
			})
			results[i] = r
		}(i)
	}
	close(start)
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Fatalf("concurrent open %d errored: %v", i, r.err)
		}
	}
	if results[0].id != results[1].id {
		t.Fatalf("concurrent opens minted different channels: %s vs %s", results[0].id, results[1].id)
	}

	var count int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM channels c JOIN channel_groups g ON g.id = c.group_id
		   WHERE g.owner_user_id = $1 AND g.name = $2`,
		string(owner.ID), dmGroupName,
	).Scan(&count); err != nil {
		t.Fatalf("count dm channels: %v", err)
	}
	if count != 1 {
		t.Fatalf("concurrent open produced %d channels, want exactly 1", count)
	}
}

// TestUpsertDMChannelIsDeliveryTarget pins that the created DM satisfies the D1
// delivery predicate purely through its mandatory flag: an agent party who has
// NEVER subscribed (subscribed=false, the DM is not its home channel) is still a
// delivery target, because the predicate disjoins ch.mandatory_subscription. This
// is the same predicate SubscribedAgents / SweepChannels enforce, asserted here
// via SweepChannels (the channel enumeration) and SubscribedAgents (the recipient
// resolution).
func TestUpsertDMChannelIsDeliveryTarget(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "alice")
	b := mustAgent(t, s, owner.ID, "bob")

	id, _ := openDM(t, s, owner.ID, "dm--alice--bob", []AccountID{a.ID, b.ID})

	// Neither party subscribed and the DM is not either party's home channel, so
	// only the mandatory disjunct can put them in the sweep/deliver sets.
	swept, err := s.SweepChannels(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("SweepChannels(alice): %v", err)
	}
	if !slices.Contains(swept, id) {
		t.Fatalf("DM %s not in alice's sweep set %v (mandatory disjunct failed)", id, swept)
	}

	// When bob posts, alice is a delivery target of the DM via the same predicate.
	recips, err := s.SubscribedAgents(context.Background(), id, b.ID)
	if err != nil {
		t.Fatalf("SubscribedAgents: %v", err)
	}
	if !containsAccount(recips, a.ID) {
		t.Fatalf("alice %s not a delivery target of the DM (recipients %v)", a.ID, recips)
	}
}

// TestCreateChannelIntoReservedDMGroupIsNotFound pins the R3 primary defense: a
// manual CreateChannel targeting the reserved DM group is rejected with the
// merged ErrNotFound (never confirming the group exists), so a squat is
// impossible via the create path.
func TestCreateChannelIntoReservedDMGroupIsNotFound(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "alice")
	b := mustAgent(t, s, owner.ID, "bob")

	// Materialize the reserved DM group by opening a real DM first.
	openDM(t, s, owner.ID, "dm--alice--bob", []AccountID{a.ID, b.ID})
	gid := dmGroupIDFor(t, s, owner.ID)

	_, err := s.CreateChannel(context.Background(), owner.ID, NewChannel{
		Name: "dm--alice--carol", GroupID: gid, Kind: ChannelKindChannel,
	})
	sentinelIs(t, err, ErrNotFound, "manual create into the reserved DM group")
}

// TestUpsertDMChannelSquatBeltRejectsWrongKind pins the R3 belt: if a wrong-kind
// row is planted at the deterministic name in the reserved group (impossible via
// the create-guard, but the belt defends any other write path), a resume returns
// ErrNotFound rather than adopting it.
func TestUpsertDMChannelSquatBeltRejectsWrongKind(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "alice")
	b := mustAgent(t, s, owner.ID, "bob")

	// Open a real DM to materialize the group, then hand-plant a squat under a
	// DIFFERENT deterministic name (a raw INSERT bypasses the create-guard).
	openDM(t, s, owner.ID, "dm--alice--bob", []AccountID{a.ID, b.ID})
	gid := dmGroupIDFor(t, s, owner.ID)
	const squatName = "dm--alice--carol"
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO channels (id, name, group_id, kind, post_policy, owner_account_id, mandatory_subscription)
		   VALUES ($1, $2, $3, $4, $5, NULL, $6)`,
		newID(), squatName, string(gid), int32(ChannelKindChannel), int32(ChannelPostPolicyOpen), true,
	); err != nil {
		t.Fatalf("plant squat row: %v", err)
	}

	// A resume that resolves the squat name must refuse the wrong-kind adoptee.
	err := s.WithTx(context.Background(), func(tx pgx.Tx) error {
		if err := LockOwnerDMTx(context.Background(), tx, owner.ID); err != nil {
			return err
		}
		_, _, e := s.UpsertDMChannelTx(context.Background(), tx, DMChannelSpec{
			GroupID: gid, Name: squatName, Members: []AccountID{a.ID, b.ID},
		})
		return e
	})
	sentinelIs(t, err, ErrNotFound, "resume onto a wrong-kind squat row")
}

// TestUpsertDMChannelResumeReconcilesDrift pins the R3 belt's RECOVERABLE-drift
// path (the half TestUpsertDMChannelSquatBeltRejectsWrongKind does not cover): a
// resume onto a real DM that has drifted — a wanted member row deleted AND the
// mandatory flag flipped to FALSE — restores the member row, re-seeds its
// delivery cursor, and re-asserts mandatory, all in the resume tx. This is the
// D2-hazard-adjacent path: a regression that skipped the seed-on-readd would
// silently mint an un-seeded delivery target on a resumed DM.
func TestUpsertDMChannelResumeReconcilesDrift(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "alice")
	b := mustAgent(t, s, owner.ID, "bob")

	const name = "dm--alice--bob"
	id, created := openDM(t, s, owner.ID, name, []AccountID{a.ID, b.ID})
	if !created {
		t.Fatal("first open reported created=false, want true")
	}

	// Simulate recoverable drift directly in the DB (bypassing the API, which has
	// no path to produce it): drop agent b's member row + its cursor, and flip the
	// channel non-mandatory.
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM channel_members WHERE channel_id = $1 AND account_id = $2`,
		string(id), string(b.ID),
	); err != nil {
		t.Fatalf("drop member row: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM agent_delivery_cursors WHERE agent_account_id = $1 AND channel_id = $2`,
		string(b.ID), string(id),
	); err != nil {
		t.Fatalf("drop cursor: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE channels SET mandatory_subscription = FALSE WHERE id = $1`, string(id),
	); err != nil {
		t.Fatalf("flip non-mandatory: %v", err)
	}

	// Re-open the same pair: a resume that runs the belt reconcile.
	gotID, created := openDM(t, s, owner.ID, name, []AccountID{a.ID, b.ID})
	if created {
		t.Fatal("resume onto a drifted DM reported created=true, want false")
	}
	if gotID != id {
		t.Fatalf("resume minted a new channel %s, want the drifted %s", gotID, id)
	}

	ch, err := s.GetChannel(ctx, id)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if !ch.Policy.MandatorySubscription {
		t.Fatal("mandatory_subscription still FALSE after resume, want re-asserted TRUE")
	}
	if got := memberSet(ch); !got[b.ID] {
		t.Fatalf("dropped member %s not restored on resume; members = %v", b.ID, ch.MemberAccountIDs)
	}
	if !cursorExists(t, s, b.ID, id) {
		t.Fatalf("restored member %s has no re-seeded delivery cursor (D2 hazard)", b.ID)
	}
}

// TestConvertOnAddRequiresNameAndConverts pins R4: adding a third agent to a DM
// without a convert name is invalid_argument; with a name the channel becomes
// kind=CHANNEL, takes the new name, leaves the reserved DM group (group_id NULL),
// and the third member is added. Per Matt's M1 ruling the converted channel is a
// normal opt-in channel: mandatory_subscription is cleared, the two incumbent DM
// parties are flipped subscribed=TRUE (so they keep receiving the conversation —
// there is no add/drop notification, delivery is the only signal), and the newly
// added third member joins opt-in (subscribed=FALSE unless its update said so).
func TestConvertOnAddRequiresNameAndConverts(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "alice")
	b := mustAgent(t, s, owner.ID, "bob")
	c := mustAgent(t, s, owner.ID, "carol")

	id, _ := openDM(t, s, owner.ID, "dm--alice--bob", []AccountID{a.ID, b.ID})

	// Add the third agent WITHOUT a convert name → invalid_argument.
	_, _, err := s.UpdateChannelMembers(context.Background(), a.ID, id,
		[]MemberUpdate{{AccountID: c.ID}}, MemberUpdatesOptions{})
	sentinelIs(t, err, ErrInvalidArgument, "third-member add without a convert name")

	// The DM is untouched by the rejected add.
	ch, err := s.GetChannel(context.Background(), id)
	if err != nil {
		t.Fatalf("GetChannel after rejected add: %v", err)
	}
	if ch.Kind != ChannelKindDM || containsAccount(ch.MemberAccountIDs, c.ID) {
		t.Fatalf("rejected add mutated the DM: kind=%d members=%v", ch.Kind, ch.MemberAccountIDs)
	}

	// Add the third agent WITH a convert name → conversion.
	converted, _, err := s.UpdateChannelMembers(context.Background(), a.ID, id,
		[]MemberUpdate{{AccountID: c.ID}}, MemberUpdatesOptions{ConvertChannelName: "war-room"})
	if err != nil {
		t.Fatalf("convert-on-add: %v", err)
	}
	if converted.Kind != ChannelKindChannel {
		t.Fatalf("post-convert kind = %d, want ChannelKindChannel", converted.Kind)
	}
	if converted.Name != "war-room" {
		t.Fatalf("post-convert name = %q, want war-room", converted.Name)
	}
	if converted.GroupID != "" {
		t.Fatalf("post-convert group = %q, want empty (left the reserved DM group)", converted.GroupID)
	}
	if !containsAccount(converted.MemberAccountIDs, c.ID) {
		t.Fatalf("third member %s not added: %v", c.ID, converted.MemberAccountIDs)
	}
	for _, m := range []AccountID{a.ID, b.ID} {
		if !containsAccount(converted.MemberAccountIDs, m) {
			t.Fatalf("existing member %s dropped by conversion: %v", m, converted.MemberAccountIDs)
		}
	}
	// M1: the converted channel is a normal opt-in channel, not force-subscribe.
	if converted.Policy.MandatorySubscription {
		t.Fatal("converted channel still mandatory_subscription=true, want FALSE (normal opt-in channel)")
	}
	// The two incumbent DM parties are kept subscribed so they keep receiving.
	for _, m := range []AccountID{a.ID, b.ID} {
		if !memberSubscribed(t, s, id, m) {
			t.Fatalf("incumbent DM party %s not subscribed after convert — would silently drop from the conversation", m)
		}
	}
	// The newly-added third member joins opt-in (its update set no subscribe flag).
	if memberSubscribed(t, s, id, c.ID) {
		t.Fatalf("third member %s force-subscribed; a normal-channel add is opt-in", c.ID)
	}
	// M1 seed discipline: an opt-in (unsubscribed) add on the now-non-mandatory
	// channel owes NO delivery cursor — it is seeded only when it subscribes (D2
	// seed-at-subscribe). A spurious seed here (the stale-mandatory bug) would
	// later replay backlog from convert-time, so assert the cursor is absent.
	if cursorExists(t, s, c.ID, id) {
		t.Fatalf("third member %s has a delivery cursor after an opt-in add on a non-mandatory channel — spurious seed (stale mandatory flag)", c.ID)
	}
	// A member CAN now unsubscribe (the DM's force-subscribe is gone).
	if _, _, err := s.UpdateChannelMembers(context.Background(), a.ID, id,
		[]MemberUpdate{{AccountID: a.ID, Unsubscribe: true}}, MemberUpdatesOptions{}); err != nil {
		t.Fatalf("unsubscribe on converted channel rejected, want allowed: %v", err)
	}
}

// TestOpenDMAfterConvertMintsFreshPair pins the R4 open_dm semantics after a
// convert: the conversion freed the deterministic name, so a subsequent open of
// the same pair mints a FRESH pair DM at that name (created=true, a new id).
func TestOpenDMAfterConvertMintsFreshPair(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "alice")
	b := mustAgent(t, s, owner.ID, "bob")
	c := mustAgent(t, s, owner.ID, "carol")

	const name = "dm--alice--bob"
	first, _ := openDM(t, s, owner.ID, name, []AccountID{a.ID, b.ID})

	// Convert the DM away by adding a third member with a name — frees `name`.
	if _, _, err := s.UpdateChannelMembers(context.Background(), a.ID, first,
		[]MemberUpdate{{AccountID: c.ID}}, MemberUpdatesOptions{ConvertChannelName: "war-room"}); err != nil {
		t.Fatalf("convert: %v", err)
	}

	// Re-open the same pair: the freed name is available, so this is a FRESH DM.
	second, created := openDM(t, s, owner.ID, name, []AccountID{a.ID, b.ID})
	if !created {
		t.Fatal("re-open after convert reported created=false, want true (fresh pair DM)")
	}
	if second == first {
		t.Fatal("re-open after convert resolved the converted channel, want a fresh id")
	}
	ch, err := s.GetChannel(context.Background(), second)
	if err != nil {
		t.Fatalf("GetChannel(fresh): %v", err)
	}
	if ch.Kind != ChannelKindDM || ch.Name != name {
		t.Fatalf("fresh DM shape wrong: kind=%d name=%q", ch.Kind, ch.Name)
	}
}

// TestRemoveBelowTwoPartiesRejected pins R4: a remove that would strand a DM
// below two agent parties is invalid_argument (a one-party DM is not a thing).
func TestRemoveBelowTwoPartiesRejected(t *testing.T) {
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	a := mustAgent(t, s, owner.ID, "alice")
	b := mustAgent(t, s, owner.ID, "bob")

	id, _ := openDM(t, s, owner.ID, "dm--alice--bob", []AccountID{a.ID, b.ID})

	_, _, err := s.UpdateChannelMembers(context.Background(), a.ID, id,
		[]MemberUpdate{{AccountID: b.ID, Remove: true}}, MemberUpdatesOptions{})
	sentinelIs(t, err, ErrInvalidArgument, "remove leaving a DM below two agent parties")

	// The remove rolled back: both parties remain.
	ch, err := s.GetChannel(context.Background(), id)
	if err != nil {
		t.Fatalf("GetChannel after rejected remove: %v", err)
	}
	for _, m := range []AccountID{a.ID, b.ID} {
		if !containsAccount(ch.MemberAccountIDs, m) {
			t.Fatalf("party %s dropped by a rejected remove: %v", m, ch.MemberAccountIDs)
		}
	}
}
