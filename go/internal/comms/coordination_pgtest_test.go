//go:build pgtest

package comms

// Coordination-channel handler contracts (SEA-1722 T5, design.md:530-592): the
// CreateAgent-with-parent and ReparentAgent RPC paths fire the store's in-tx
// coordination hook (registered here via RegisterCoordinationHook) and emit the
// coordination ChannelChanged post-commit, and the manual entrypoints
// (EnsureCoordinationChannel, ReconcileCoordinationMembership) run the same
// reconcile in their own tx. Reparent-out's removal rides
// ChannelChanged.removed_account_ids. Driven in-process against a real store +
// bus.

import (
	"context"
	"slices"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// coordChannelsFor reads the coordination channels visible to viewer (a member —
// the coordination group is VisibilityOwner, so only members see the channel,
// never the owning user), filtered to those with the coordination policy shape.
func coordChannelsFor(t *testing.T, st *store.Store, viewer store.AccountID) []store.Channel {
	t.Helper()
	chs, err := st.ListChannels(context.Background(), viewer)
	if err != nil {
		t.Fatalf("ListChannels: %v", err)
	}
	var out []store.Channel
	for _, ch := range chs {
		if ch.Policy.OwnerAccountID != "" && ch.Policy.MandatorySubscription && ch.Policy.PostPolicy == store.ChannelPostPolicyOwnerOnly {
			out = append(out, ch)
		}
	}
	return out
}

// TestCreateAgentWithParentProvisionsCoordinationChannel pins the public
// CreateAgent-with-parent RPC path: the first report provisions the manager's
// coordination channel through the registered store hook, and a ChannelChanged
// is emitted (post-commit) carrying the coordination channel.
func TestCreateAgentWithParentProvisionsCoordinationChannel(t *testing.T) {
	h := newStreamHarness(t)
	h.svc.RegisterCoordinationHook(h.store)
	ctx := context.Background()

	owner := mustUser(t, h.store, "owner")
	manager := mustAgent(t, h.store, owner.ID, "manager")

	// A subscriber (the owner) draining the stream will see the coordination
	// ChannelChanged emitted after the report's create commits.
	_, err := h.svc.CreateAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateAgentRequest{
		Handle: "report1", DisplayName: "r1", ParentAgentId: string(manager.ID),
	}))
	if err != nil {
		t.Fatalf("CreateAgent(report1): %v", err)
	}

	chs := coordChannelsFor(t, h.store, manager.ID)
	if len(chs) != 1 {
		t.Fatalf("provisioned %d coordination channels, want 1", len(chs))
	}
	if chs[0].Name != "manager-coordination" {
		t.Fatalf("coordination channel name = %q, want manager-coordination", chs[0].Name)
	}

	// The coordination ChannelChanged reached a member's stream (post-commit
	// emit). The manager is a member (the owner user is NOT — the group is
	// VisibilityOwner), so drain as the manager. Use the canary to bound the set.
	canary := mkCanary(t, h, "canary1")
	evts := drainReplayAsActor(t, h, manager.ID, canary)
	if !hasChannelChangedFor(evts, string(chs[0].ID)) {
		t.Fatalf("no coordination ChannelChanged emitted for %s", chs[0].ID)
	}
}

// TestCreateAgentByAgentCallerProvisionsCoordinationChannel proves owner
// resolution (RIG-1644) and coordination provisioning compose: an AGENT caller
// spawning a report under a same-owner manager resolves to its owner, passes the
// same-owner check, and still fires the store's in-tx coordination hook — the
// manager's channel is provisioned and its ChannelChanged emitted post-commit,
// exactly as for a user caller.
func TestCreateAgentByAgentCallerProvisionsCoordinationChannel(t *testing.T) {
	h := newStreamHarness(t)
	h.svc.RegisterCoordinationHook(h.store)
	ctx := context.Background()

	owner := mustUser(t, h.store, "owner")
	manager := mustAgent(t, h.store, owner.ID, "manager")
	caller := mustAgent(t, h.store, owner.ID, "caller")

	// The agent caller (not the owning user) spawns the report under the manager.
	if _, err := h.svc.CreateAgent(WithActor(ctx, caller.ID), connect.NewRequest(&compassv1.CreateAgentRequest{
		Handle: "report1", DisplayName: "r1", ParentAgentId: string(manager.ID),
	})); err != nil {
		t.Fatalf("CreateAgent by agent caller: %v", err)
	}

	chs := coordChannelsFor(t, h.store, manager.ID)
	if len(chs) != 1 {
		t.Fatalf("provisioned %d coordination channels, want 1", len(chs))
	}
	if chs[0].Name != "manager-coordination" {
		t.Fatalf("coordination channel name = %q, want manager-coordination", chs[0].Name)
	}

	canary := mkCanary(t, h, "canary1")
	evts := drainReplayAsActor(t, h, manager.ID, canary)
	if !hasChannelChangedFor(evts, string(chs[0].ID)) {
		t.Fatalf("no coordination ChannelChanged emitted for %s", chs[0].ID)
	}
}

// TestReparentInEmitsMembershipMove pins the reparent path: moving a report from
// manager A to manager B adds it to B's channel (reparent-in) and removes it from
// A's, with the removal carried in A's ChannelChanged.removed_account_ids
// (design.md:567-568).
func TestReparentInEmitsMembershipMove(t *testing.T) {
	h := newStreamHarness(t)
	h.svc.RegisterCoordinationHook(h.store)
	ctx := context.Background()

	owner := mustUser(t, h.store, "owner")
	mgrA := mustAgent(t, h.store, owner.ID, "mgra")
	mgrB := mustAgent(t, h.store, owner.ID, "mgrb")
	report, err := h.store.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "report", DisplayName: "r", ParentAgentID: mgrA.ID})
	if err != nil {
		t.Fatalf("CreateAgent(report): %v", err)
	}
	if _, err := h.store.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "bseed", DisplayName: "bs", ParentAgentID: mgrB.ID}); err != nil {
		t.Fatalf("CreateAgent(bseed): %v", err)
	}

	if _, err := h.svc.ReparentAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.ReparentAgentRequest{
		AgentAccountId:   string(report.ID),
		NewParentAgentId: string(mgrB.ID),
	})); err != nil {
		t.Fatalf("ReparentAgent: %v", err)
	}
	aChans := coordChannelsFor(t, h.store, mgrA.ID)
	bChans := coordChannelsFor(t, h.store, mgrB.ID)
	var aCh, bCh store.Channel
	for _, ch := range aChans {
		if ch.Name == "mgra-coordination" {
			aCh = ch
		}
	}
	for _, ch := range bChans {
		if ch.Name == "mgrb-coordination" {
			bCh = ch
		}
	}
	if aCh.ID == "" || bCh.ID == "" {
		t.Fatalf("missing coordination channels: A=%q B=%q", aCh.ID, bCh.ID)
	}
	if containsAccount(aCh.MemberAccountIDs, report.ID) {
		t.Fatalf("report still in A's channel after move: %v", aCh.MemberAccountIDs)
	}
	if !containsAccount(bCh.MemberAccountIDs, report.ID) {
		t.Fatalf("report not in B's channel after move: %v", bCh.MemberAccountIDs)
	}

	// The departing member (report) gets its final ChannelChanged for A's channel
	// carrying itself in removed_account_ids (the removed-member carve-out,
	// design.md:567-568). Drain as the report.
	canary := mkCanary(t, h, "canary2")
	evts := drainReplayAsActor(t, h, report.ID, canary)
	if !hasRemoval(evts, string(aCh.ID), string(report.ID)) {
		t.Fatalf("no ChannelChanged carrying report %s in removed_account_ids for A's channel %s", report.ID, aCh.ID)
	}
}

// TestEnsureCoordinationChannelManualBackfill pins the manual entrypoint: it runs
// the reconcile in its own tx and returns the channel id, idempotently (a second
// call resumes the same channel).
func TestEnsureCoordinationChannelManualBackfill(t *testing.T) {
	h := newStreamHarness(t)
	h.svc.RegisterCoordinationHook(h.store)
	ctx := context.Background()

	owner := mustUser(t, h.store, "owner")
	manager := mustAgent(t, h.store, owner.ID, "manager")
	// A report exists but suppose provisioning was somehow skipped; backfill it.
	if _, err := h.store.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "report", DisplayName: "r", ParentAgentID: manager.ID}); err != nil {
		t.Fatalf("CreateAgent(report): %v", err)
	}

	first, err := h.svc.EnsureCoordinationChannel(ctx, manager.ID)
	if err != nil {
		t.Fatalf("EnsureCoordinationChannel: %v", err)
	}
	second, err := h.svc.EnsureCoordinationChannel(ctx, manager.ID)
	if err != nil {
		t.Fatalf("EnsureCoordinationChannel (2nd): %v", err)
	}
	if first == "" || first != second {
		t.Fatalf("manual backfill not idempotent: first=%q second=%q", first, second)
	}
}

// TestCreateAgentSuffixesAroundUserChannelWithoutWedge pins the never-adopt +
// never-wedge invariants through the REAL registered reconcile closure
// (reconcileCoordinationTx, fired by the CreateAgent-with-parent RPC hook), not
// the store helper: a user-owned `<handle>-coordination` channel pre-exists in
// the owner's coordination group, and the manager's provisioning must (a) land
// at the `-2` suffix without adopting the user's channel, AND (b) leave the
// CreateAgent RPC succeeding (the parent-edge write is NOT wedged by the
// collision). This closes the loop on the registered closure end to end.
func TestCreateAgentSuffixesAroundUserChannelWithoutWedge(t *testing.T) {
	h := newStreamHarness(t)
	h.svc.RegisterCoordinationHook(h.store)
	ctx := context.Background() // test root context (legitimate per go-thread-context test exemption)

	owner := mustUser(t, h.store, "owner")
	manager := mustAgent(t, h.store, owner.ID, "manager")

	// The user pre-creates the coordination group and a channel with the exact
	// name the manager's channel would take, owned by the USER (mirrors the store
	// test TestCollisionUserOwnedSuffixes setup, via UpsertCoordinationChannelTx).
	if err := h.store.WithTx(ctx, func(tx pgx.Tx) error {
		gid, err := h.store.EnsureOwnerCoordinationGroupTx(ctx, tx, owner.ID)
		if err != nil {
			return err
		}
		_, err = h.store.UpsertCoordinationChannelTx(ctx, tx, store.CoordinationChannelSpec{
			GroupID:        gid,
			BaseName:       "manager-coordination",
			OwnerAccountID: owner.ID, // user owns it
			Policy:         store.ChannelPolicy{OwnerAccountID: owner.ID},
		})
		return err
	}); err != nil {
		t.Fatalf("pre-create user channel: %v", err)
	}

	// The real RPC that fires the hook: a first report under the manager.
	if _, err := h.svc.CreateAgent(WithActor(ctx, owner.ID), connect.NewRequest(&compassv1.CreateAgentRequest{
		Handle: "report", DisplayName: "r", ParentAgentId: string(manager.ID),
	})); err != nil {
		// (b) The parent-edge write must NOT be wedged by the name collision.
		t.Fatalf("CreateAgent(report) wedged by coordination collision: %v", err)
	}

	// (a) The manager's channel landed at -2 (never adopted the user's), and the
	// manager is a member of the -2 channel, not the user's.
	chs := coordChannelsFor(t, h.store, manager.ID)
	var mgrCh store.Channel
	for _, ch := range chs {
		if ch.Name == "manager-coordination-2" {
			mgrCh = ch
		}
		if ch.Name == "manager-coordination" {
			t.Fatalf("manager adopted the user's channel: %+v", ch)
		}
	}
	if mgrCh.ID == "" {
		t.Fatalf("manager's channel not suffixed to -2: %v", chs)
	}
	if mgrCh.Policy.OwnerAccountID != manager.ID || !mgrCh.Policy.MandatorySubscription || mgrCh.Policy.PostPolicy != store.ChannelPostPolicyOwnerOnly {
		t.Fatalf("suffixed channel not manager-owned+mandatory+owner-only: %+v", mgrCh.Policy)
	}
}

// hasChannelChangedFor reports whether any event is a ChannelChanged for chID.
func hasChannelChangedFor(evts []*compassv1.SubscribeCommsResponse, chID string) bool {
	for _, e := range evts {
		if cc := e.GetChannelChanged(); cc != nil && cc.GetChannel().GetId() == chID {
			return true
		}
	}
	return false
}

// hasRemoval reports whether any ChannelChanged for chID carries account in its
// removed_account_ids.
func hasRemoval(evts []*compassv1.SubscribeCommsResponse, chID, account string) bool {
	for _, e := range evts {
		cc := e.GetChannelChanged()
		if cc == nil || cc.GetChannel().GetId() != chID {
			continue
		}
		if slices.Contains(cc.GetRemovedAccountIds(), account) {
			return true
		}
	}
	return false
}

// containsAccount reports whether ids contains want.
func containsAccount(ids []store.AccountID, want store.AccountID) bool {
	return slices.Contains(ids, want)
}
