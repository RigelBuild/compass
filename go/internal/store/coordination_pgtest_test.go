//go:build pgtest

package store

// Coordination-channel store contracts (SEA-1722 T5, design.md:530-592): the
// two parent-edge writers (CreateAgent, ReparentAgent) invoke the registered
// coordination hook on their OWN tx right after writing parent_agent_id, and the
// tx-level store helpers the comms reconcile drives (per-owner group get-or-
// create, channel upsert with the same-owner collision/suffix rule, membership
// resync with in-tx cursor seeds) behave as the record's invariants require.
// These are properties only a real Postgres proves (the in-tx hook, the partial
// unique index, the cursor seeds), so the file is pgtest-tagged.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// recordingHook is a coordination hook that records the manager ids it was
// invoked with, so a test can assert the parent-edge writers fire it (and with
// which manager). It also runs the tx-level provision so the in-tx behavior is
// exercised end to end, mirroring what the comms closure does — but with the
// naming/policy inlined here (the store package cannot import comms).
type recordingHook struct {
	s       *Store
	invoked []AccountID
}

func (h *recordingHook) reconcile(ctx context.Context, tx pgx.Tx, manager AccountID) error {
	h.invoked = append(h.invoked, manager)
	handle, owner, err := h.s.ResolveCoordinationManagerTx(ctx, tx, manager)
	if err != nil {
		return err
	}
	if err := LockOwnerCoordinationTx(ctx, tx, owner); err != nil {
		return err
	}
	groupID, err := h.s.EnsureOwnerCoordinationGroupTx(ctx, tx, owner)
	if err != nil {
		return err
	}
	channelID, err := h.s.UpsertCoordinationChannelTx(ctx, tx, CoordinationChannelSpec{
		GroupID:        groupID,
		BaseName:       handle + "-coordination",
		OwnerAccountID: manager,
		Policy: ChannelPolicy{
			PostPolicy:            ChannelPostPolicyOwnerOnly,
			OwnerAccountID:        manager,
			MandatorySubscription: true,
		},
	})
	if err != nil {
		return err
	}
	want, err := h.s.CoordinationReports(ctx, tx, manager)
	if err != nil {
		return err
	}
	_, err = h.s.SetCoordinationMembersTx(ctx, tx, channelID, want)
	return err
}

// coordChannels returns all coordination channels in owner's __coordination__
// group, ordered by name (no count assertion — callers assert the count they
// expect). Each row carries id + policy + members for further assertions.
func coordChannels(t *testing.T, s *Store, owner AccountID) []Channel {
	t.Helper()
	ctx := context.Background()
	rows, err := s.pool.Query(ctx,
		`SELECT c.id, c.name, COALESCE(c.group_id,''), c.kind, c.post_policy, COALESCE(c.owner_account_id,''), c.mandatory_subscription
		   FROM channels c
		   JOIN channel_groups g ON g.id = c.group_id
		  WHERE g.owner_user_id = $1 AND g.name = '__coordination__'
		  ORDER BY c.name`, string(owner))
	if err != nil {
		t.Fatalf("query coordination channels: %v", err)
	}
	chs, err := scanChannels(ctx, s.pool, rows)
	if err != nil {
		t.Fatalf("scan coordination channels: %v", err)
	}
	return chs
}

// memberSet returns channel members as a set for order-independent comparison.
func memberSet(ch Channel) map[AccountID]bool {
	m := make(map[AccountID]bool, len(ch.MemberAccountIDs))
	for _, id := range ch.MemberAccountIDs {
		m[id] = true
	}
	return m
}

// TestCreateAgentInvokesCoordinationHookForParent pins that CreateAgent with a
// parent fires the hook with the PARENT (the manager gaining the report), and a
// ROOT create (no parent) fires it not at all.
func TestCreateAgentInvokesCoordinationHookForParent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	h := &recordingHook{s: s}
	s.SetCoordinationHook(h.reconcile)

	// A root manager: created without a parent, so the hook does NOT fire.
	manager := mustAgent(t, s, owner.ID, "manager")
	if len(h.invoked) != 0 {
		t.Fatalf("root create fired hook %d times, want 0", len(h.invoked))
	}

	// A first report under the manager: the hook fires once, with the manager.
	if _, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "report1", DisplayName: "r1", ParentAgentID: manager.ID}); err != nil {
		t.Fatalf("CreateAgent(report1): %v", err)
	}
	if len(h.invoked) != 1 || h.invoked[0] != manager.ID {
		t.Fatalf("first report fired hook %v, want exactly [%s]", h.invoked, manager.ID)
	}
}

// TestFirstReportProvisionsExactlyOnce pins the core trigger: the manager's
// first report provisions exactly one coordination channel; a second report
// joins the SAME channel (no second channel), and the channel is born with the
// coordination policy and the manager + both reports as members.
func TestFirstReportProvisionsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	h := &recordingHook{s: s}
	s.SetCoordinationHook(h.reconcile)
	manager := mustAgent(t, s, owner.ID, "manager")

	r1, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "report1", DisplayName: "r1", ParentAgentID: manager.ID})
	if err != nil {
		t.Fatalf("CreateAgent(report1): %v", err)
	}
	r2, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "report2", DisplayName: "r2", ParentAgentID: manager.ID})
	if err != nil {
		t.Fatalf("CreateAgent(report2): %v", err)
	}

	chs := coordChannels(t, s, owner.ID)
	if len(chs) != 1 {
		t.Fatalf("provisioned %d coordination channels, want exactly 1", len(chs))
	}
	ch := chs[0]
	if ch.Name != "manager-coordination" {
		t.Fatalf("coordination channel name = %q, want manager-coordination", ch.Name)
	}
	if ch.Policy.PostPolicy != ChannelPostPolicyOwnerOnly || !ch.Policy.MandatorySubscription || ch.Policy.OwnerAccountID != manager.ID {
		t.Fatalf("coordination policy = %+v, want OWNER_ONLY + mandatory + owner=%s", ch.Policy, manager.ID)
	}
	got := memberSet(ch)
	for _, want := range []AccountID{manager.ID, r1.ID, r2.ID} {
		if !got[want] {
			t.Fatalf("coordination members %v missing %s", ch.MemberAccountIDs, want)
		}
	}
	if len(ch.MemberAccountIDs) != 3 {
		t.Fatalf("coordination members = %v, want exactly manager+2 reports", ch.MemberAccountIDs)
	}

	// Each agent member has a seeded delivery cursor (mandatory channel: no
	// un-seeded delivery target).
	for _, m := range []AccountID{manager.ID, r1.ID, r2.ID} {
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM agent_delivery_cursors WHERE agent_account_id = $1 AND channel_id = $2)`,
			string(m), string(ch.ID),
		).Scan(&exists); err != nil {
			t.Fatalf("cursor probe %s: %v", m, err)
		}
		if !exists {
			t.Fatalf("agent member %s has no seeded delivery cursor on the coordination channel", m)
		}
	}
}

// TestReparentMovesCoordinationMembership pins reparent-in adds and reparent-out
// removes: moving a report from manager A to manager B adds it to B's channel and
// removes it from A's, and SetCoordinationMembersTx reports the removal so the
// edge can carry it in removed_account_ids.
func TestReparentMovesCoordinationMembership(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	h := &recordingHook{s: s}
	s.SetCoordinationHook(h.reconcile)

	mgrA := mustAgent(t, s, owner.ID, "mgra")
	mgrB := mustAgent(t, s, owner.ID, "mgrb")
	// A report under A provisions A's channel; B needs its own first report to
	// provision B's channel before the move.
	report, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "report", DisplayName: "r", ParentAgentID: mgrA.ID})
	if err != nil {
		t.Fatalf("CreateAgent(report): %v", err)
	}
	if _, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "bseed", DisplayName: "bs", ParentAgentID: mgrB.ID}); err != nil {
		t.Fatalf("CreateAgent(bseed): %v", err)
	}

	// Move report from A to B.
	if _, err := s.ReparentAgent(ctx, owner.ID, report.ID, mgrB.ID); err != nil {
		t.Fatalf("ReparentAgent: %v", err)
	}

	chs := coordChannels(t, s, owner.ID)
	byName := map[string]Channel{}
	for _, ch := range chs {
		byName[ch.Name] = ch
	}
	aCh, bCh := byName["mgra-coordination"], byName["mgrb-coordination"]
	if memberSet(aCh)[report.ID] {
		t.Fatalf("after move, report still in A's channel members %v", aCh.MemberAccountIDs)
	}
	if !memberSet(bCh)[report.ID] {
		t.Fatalf("after move, report not in B's channel members %v", bCh.MemberAccountIDs)
	}
}

// TestDespawnedReportKeepsMembership pins DL-077: an agent account persists
// through despawn (teardown is compute-only), so nothing dissolves its
// coordination membership. There is no store despawn that removes the account, so
// this asserts the invariant the hook upholds: membership is only reconciled by a
// parent-edge write, never by any other lifecycle event.
func TestDespawnedReportKeepsMembership(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	h := &recordingHook{s: s}
	s.SetCoordinationHook(h.reconcile)
	manager := mustAgent(t, s, owner.ID, "manager")
	report, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "report", DisplayName: "r", ParentAgentID: manager.ID})
	if err != nil {
		t.Fatalf("CreateAgent(report): %v", err)
	}

	// No parent-edge write happens on despawn (the account persists), so a fresh
	// re-run of the reconcile (a manual resync) still finds the report as a member
	// — its parent_agent_id is unchanged.
	if err := s.WithTx(ctx, func(tx pgx.Tx) error {
		return h.reconcile(ctx, tx, manager.ID)
	}); err != nil {
		t.Fatalf("manual reconcile: %v", err)
	}
	chs := coordChannels(t, s, owner.ID)
	if len(chs) != 1 || !memberSet(chs[0])[report.ID] {
		t.Fatalf("despawned report lost coordination membership: channels=%d members=%v", len(chs), chs)
	}
}

// TestCollisionManagerOwnedResumes pins the resume rule: a re-provision resumes
// the SAME channel when its owner_account_id matches the manager — no second
// channel, no suffix.
func TestCollisionManagerOwnedResumes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	h := &recordingHook{s: s}
	s.SetCoordinationHook(h.reconcile)
	manager := mustAgent(t, s, owner.ID, "manager")
	if _, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "report1", DisplayName: "r1", ParentAgentID: manager.ID}); err != nil {
		t.Fatalf("CreateAgent(report1): %v", err)
	}
	first := coordChannels(t, s, owner.ID)
	if len(first) != 1 {
		t.Fatalf("first provision: %d channels, want 1", len(first))
	}

	// A second report re-runs the reconcile; the manager-owned channel resumes.
	if _, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "report2", DisplayName: "r2", ParentAgentID: manager.ID}); err != nil {
		t.Fatalf("CreateAgent(report2): %v", err)
	}
	second := coordChannels(t, s, owner.ID)
	if len(second) != 1 || second[0].ID != first[0].ID {
		t.Fatalf("re-provision did not resume: first=%v second=%v", first, second)
	}
}

// TestCollisionUserOwnedSuffixes pins the never-adopt rule: a user's manually
// created `<handle>-coordination` channel in the owner's coordination group is
// NOT adopted; the reconcile deterministically suffixes `-2`, and the user's
// channel is untouched (its owner is unchanged, it is not forced mandatory).
func TestCollisionUserOwnedSuffixes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	h := &recordingHook{s: s}
	s.SetCoordinationHook(h.reconcile)
	manager := mustAgent(t, s, owner.ID, "manager")

	// The user pre-creates the coordination group and a channel with the name the
	// manager's channel would take, owned by the USER (not the manager).
	var userChID ChannelID
	if err := s.WithTx(ctx, func(tx pgx.Tx) error {
		gid, err := s.EnsureOwnerCoordinationGroupTx(ctx, tx, owner.ID)
		if err != nil {
			return err
		}
		id, err := s.UpsertCoordinationChannelTx(ctx, tx, CoordinationChannelSpec{
			GroupID:        gid,
			BaseName:       "manager-coordination",
			OwnerAccountID: owner.ID, // user owns it
			Policy:         ChannelPolicy{OwnerAccountID: owner.ID},
		})
		userChID = id
		return err
	}); err != nil {
		t.Fatalf("pre-create user channel: %v", err)
	}

	// Now a report under the manager triggers provisioning. It must NOT adopt the
	// user's channel; it suffixes to manager-coordination-2.
	if _, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "report", DisplayName: "r", ParentAgentID: manager.ID}); err != nil {
		t.Fatalf("CreateAgent(report): %v", err)
	}

	chs := coordChannels(t, s, owner.ID)
	byName := map[string]Channel{}
	for _, ch := range chs {
		byName[ch.Name] = ch
	}
	userCh, ok := byName["manager-coordination"]
	if !ok || userCh.ID != userChID {
		t.Fatalf("user's channel gone or changed id: %v", chs)
	}
	if userCh.Policy.OwnerAccountID == manager.ID || userCh.Policy.MandatorySubscription {
		t.Fatalf("user's channel was adopted/forced: policy=%+v", userCh.Policy)
	}
	mgrCh, ok := byName["manager-coordination-2"]
	if !ok {
		t.Fatalf("manager's channel not suffixed to -2: %v", chs)
	}
	if mgrCh.Policy.OwnerAccountID != manager.ID || !mgrCh.Policy.MandatorySubscription {
		t.Fatalf("suffixed channel not manager-owned+mandatory: %+v", mgrCh.Policy)
	}
	// The parent-edge write still succeeded: the report account exists.
	if _, err := s.AgentByHandle(ctx, "report"); err != nil {
		t.Fatalf("report account not created (parent-edge write wedged by collision): %v", err)
	}
}

// TestReconcileIgnoresMisVisibilityUserGroup pins MED #1 (owner-private +
// never-adopt): a user can plant a top-level group named __coordination__ at
// VisibilityShared (CreateChannelGroup has no reserved-name guard). The
// visibility-discriminated get-half must NOT adopt it — adopting it would insert
// the OWNER_ONLY coordination channel into a SHARED group, making an
// owner-private channel visible to every account (a cross-tenant leak). The
// reconcile instead creates its own owner-visibility __coordination__ group, and
// an unrelated third account cannot see the channel.
//
// Red-first: drop `AND visibility = $3` from EnsureOwnerCoordinationGroupTx's
// get-half SELECT -> the shared group is adopted -> the channel's group is
// VisibilityShared -> the stranger CAN see it -> both assertions below fail.
func TestReconcileIgnoresMisVisibilityUserGroup(t *testing.T) {
	ctx := context.Background() // test root context (legitimate per go-thread-context test exemption)
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	stranger := mustUser(t, s, "stranger") // an unrelated third account (different owner)
	h := &recordingHook{s: s}
	s.SetCoordinationHook(h.reconcile)
	manager := mustAgent(t, s, owner.ID, "manager")

	// The user plants a top-level SHARED group named __coordination__ (the
	// reserved segment is not guarded at the CreateChannelGroup boundary).
	if _, err := s.CreateChannelGroup(ctx, owner.ID, NewChannelGroup{
		Name: "__coordination__", Visibility: VisibilityShared,
	}); err != nil {
		t.Fatalf("plant shared __coordination__ group: %v", err)
	}

	// A first report triggers the reconcile.
	if _, err := s.CreateAgent(ctx, owner.ID, NewAgent{Handle: "report", DisplayName: "r", ParentAgentID: manager.ID}); err != nil {
		t.Fatalf("CreateAgent(report): %v", err)
	}

	chs := coordChannels(t, s, owner.ID)
	if len(chs) != 1 {
		t.Fatalf("provisioned %d coordination channels, want exactly 1", len(chs))
	}
	ch := chs[0]

	// The channel's hosting group must be OWNER visibility — the reconcile did
	// NOT adopt the planted shared group.
	var vis int32
	if err := s.pool.QueryRow(ctx,
		`SELECT g.visibility FROM channels c JOIN channel_groups g ON g.id = c.group_id WHERE c.id = $1`,
		string(ch.ID),
	).Scan(&vis); err != nil {
		t.Fatalf("read coordination channel group visibility: %v", err)
	}
	if ChannelGroupVisibility(vis) != VisibilityOwner {
		t.Fatalf("coordination channel landed in a %d-visibility group, want VisibilityOwner (mis-visibility group was adopted)", vis)
	}

	// The cross-tenant leak the discriminator prevents: an unrelated account must
	// NOT be able to see the owner-private coordination channel.
	visible, err := s.ChannelVisibleTo(ctx, stranger.ID, ch.ID)
	if err != nil {
		t.Fatalf("ChannelVisibleTo(stranger): %v", err)
	}
	if visible {
		t.Fatalf("owner-private coordination channel is visible to an unrelated account (cross-tenant leak)")
	}
}

// TestUpsertConcurrentUserInsertSuffixesWithoutWedge pins MED #2 (never-wedge +
// never-adopt + poison-free, no savepoint): a user's concurrent CreateChannel
// commits the same (group, name) AFTER the reconcile's SELECT missed it but
// BEFORE the reconcile's INSERT — the exact window a plain INSERT would hit a
// unique-violation on the partial index channels_group_name_key, poisoning the
// savepoint-less parent-edge tx and wedging report creation. The ON CONFLICT DO
// NOTHING INSERT absorbs it: the INSERT affects zero rows (no raised error), the
// reconcile re-SELECTs, sees the user's now-committed row, and suffixes to -2.
//
// The race is deterministic: an uncommitted user insert in txUser holds the
// (group, name) unique-index tuple; the reconcile's INSERT blocks on txUser
// (speculative insertion waits for the concurrent inserter under both the ON
// CONFLICT and the plain-INSERT forms); a pg_blocking_pids gate — scoped to
// txUser's own backend pid, so no parallel test can spuriously satisfy it —
// confirms the block before txUser commits and unblocks the reconcile.
//
// Red-first: revert the reconcile INSERT to a plain `INSERT ... VALUES (...)`
// (no ON CONFLICT) -> when txUser commits, the blocked INSERT raises
// unique-violation -> WithTx rolls back -> the reconcile returns a non-nil error
// -> the "returned no error" assertion below fails.
func TestUpsertConcurrentUserInsertSuffixesWithoutWedge(t *testing.T) {
	ctx := context.Background() // test root context (legitimate per go-thread-context test exemption)
	s := newTestStore(t)
	owner := mustUser(t, s, "owner")
	manager := mustAgent(t, s, owner.ID, "manager")

	// Pre-create (committed) the shared coordination group both transactions
	// resolve, so the race is purely on the channel row.
	var groupID ChannelGroupID
	if err := s.WithTx(ctx, func(tx pgx.Tx) error {
		gid, err := s.EnsureOwnerCoordinationGroupTx(ctx, tx, owner.ID)
		groupID = gid
		return err
	}); err != nil {
		t.Fatalf("pre-create coordination group: %v", err)
	}

	// txUser: a concurrent USER channel of the same (group, name), left
	// uncommitted so the reconcile's SELECT misses it (READ COMMITTED) but its
	// INSERT collides on the partial unique index.
	txUser, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin user tx: %v", err)
	}
	// Rollback is a no-op after the Commit below; the discard is deliberate test
	// cleanup for the failure path (mirrors TestReparentAgentConcurrentCycleSerialized).
	defer func() { _ = txUser.Rollback(ctx) }()

	var userPID int
	if err := txUser.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&userPID); err != nil {
		t.Fatalf("read user tx backend pid: %v", err)
	}
	userChID := newID()
	if _, err := txUser.Exec(ctx,
		`INSERT INTO channels (id, name, group_id, kind, post_policy, owner_account_id, mandatory_subscription) `+
			`VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		userChID, "manager-coordination", string(groupID), int32(ChannelKindChannel),
		int32(ChannelPostPolicyOpen), string(owner.ID), false,
	); err != nil {
		t.Fatalf("user tx insert channel: %v", err)
	}

	// The reconcile runs on its OWN tx: its SELECT misses the uncommitted user
	// row, then its INSERT blocks on txUser until txUser commits below.
	type result struct {
		id  ChannelID
		err error
	}
	done := make(chan result, 1)
	go func() {
		var id ChannelID
		err := s.WithTx(ctx, func(tx pgx.Tx) error {
			var e error
			id, e = s.UpsertCoordinationChannelTx(ctx, tx, CoordinationChannelSpec{
				GroupID:        groupID,
				BaseName:       "manager-coordination",
				OwnerAccountID: manager.ID,
				Policy: ChannelPolicy{
					PostPolicy:            ChannelPostPolicyOwnerOnly,
					OwnerAccountID:        manager.ID,
					MandatorySubscription: true,
				},
			})
			return e
		})
		done <- result{id, err}
	}()

	// Readiness gate (bounded poll, not a fixed sleep): wait until some backend
	// is blocked BY txUser — precisely the reconcile's INSERT parked on the
	// uncommitted unique-index tuple. Scoped to txUser's own pid so a parallel
	// test's lock wait cannot satisfy it. On timeout, fall through and commit
	// anyway (the RED case may block identically, so the assertion still fires).
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
gate:
	for {
		var blocked bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))`,
			userPID,
		).Scan(&blocked); err != nil {
			t.Fatalf("poll pg_blocking_pids: %v", err)
		}
		if blocked {
			break gate
		}
		select {
		case <-deadline:
			break gate
		case <-tick.C:
		}
	}

	// Commit txUser: the reconcile's INSERT unblocks. ON CONFLICT DO NOTHING ->
	// zero rows -> re-SELECT sees the user's committed row -> suffix to -2.
	if err := txUser.Commit(ctx); err != nil {
		t.Fatalf("commit user tx: %v", err)
	}

	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("reconcile did not complete: likely deadlocked on the (group, name) tuple")
	}

	// Never-wedge: the reconcile returned NO error (a plain INSERT would have
	// raised unique-violation here and rolled back the parent-edge tx).
	if r.err != nil {
		t.Fatalf("reconcile returned error under concurrent user insert (parent-edge would wedge): %v", r.err)
	}

	// Never-adopt: the manager's channel suffixed to -2, manager-owned + mandatory.
	chs := coordChannels(t, s, owner.ID)
	byName := map[string]Channel{}
	for _, ch := range chs {
		byName[ch.Name] = ch
	}
	userCh, ok := byName["manager-coordination"]
	if !ok || userCh.ID != ChannelID(userChID) {
		t.Fatalf("user's channel gone or changed id: %v", chs)
	}
	if userCh.Policy.OwnerAccountID == manager.ID || userCh.Policy.MandatorySubscription {
		t.Fatalf("user's channel was adopted/forced: policy=%+v", userCh.Policy)
	}
	mgrCh, ok := byName["manager-coordination-2"]
	if !ok {
		t.Fatalf("manager's channel not suffixed to -2: %v", chs)
	}
	if mgrCh.ID != r.id {
		t.Fatalf("reconcile returned id %q, want the -2 channel %q", r.id, mgrCh.ID)
	}
	if mgrCh.Policy.OwnerAccountID != manager.ID || !mgrCh.Policy.MandatorySubscription {
		t.Fatalf("suffixed channel not manager-owned+mandatory: %+v", mgrCh.Policy)
	}
}
