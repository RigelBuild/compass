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

// coordChannelByOwner returns the single coordination channel owned by manager
// in manager's owner's coordination group, or fails when the count is not want.
// Returns the channel row (id + policy + members) for further assertions.
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
