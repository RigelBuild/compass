//go:build pgtest && unix

package server

import (
	"errors"
	"log/slog"
	"slices"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// TestLinearRoutingSeedAndFallbackTarget walks the fallback through the seed with
// look-alike channels planted: they never resolve, and the seed mints one channel.
func TestLinearRoutingSeedAndFallbackTarget(t *testing.T) {
	ctx := t.Context()
	h := newSeedHarness(t)
	bridge, err := h.store.EnsureLinearBridgeAccount(ctx)
	if err != nil {
		t.Fatalf("EnsureLinearBridgeAccount: %v", err)
	}
	lookalikes := plantRoutingLookalikes(t, h.store, h.dsn, h.adminID)
	routing := &linearRouting{st: h.store, adminID: h.adminID}

	if _, _, err := routing.RoutingTarget(ctx); !errors.Is(err, errRoutingSupervisor) {
		t.Fatalf("before any seed: err = %v, want errRoutingSupervisor", err)
	}

	attachFakeRunner(t, h.store, h.hub, false) // the harness hook seeds with Linear off
	h.awaitSeed(t)
	if _, _, err := routing.RoutingTarget(ctx); !errors.Is(err, errRoutingChannel) {
		t.Fatalf("after a Linear-off seed: err = %v, want errRoutingChannel (a look-alike must not resolve)", err)
	}

	var channel store.ChannelID
	for i := range 2 {
		seedRootSupervisor(ctx, h.store, h.svc, h.commsSvc, h.adminID, h.compassID, bridge.ID, slog.New(slog.DiscardHandler))
		got, err := h.store.LinearRoutingChannel(ctx, h.adminID)
		if err != nil {
			t.Fatalf("LinearRoutingChannel after seed %d: %v", i+1, err)
		}
		if i > 0 && got != channel {
			t.Fatalf("seed %d resolved channel %s, want the first seed's %s", i+1, got, channel)
		}
		channel = got
	}
	if slices.Contains(lookalikes, channel) {
		t.Fatalf("seed adopted look-alike channel %s", channel)
	}

	supervisor, err := h.store.AgentByHandle(ctx, h.adminID, rootSupervisorHandle)
	if err != nil {
		t.Fatalf("AgentByHandle(supervisor): %v", err)
	}
	for _, member := range []store.AccountID{supervisor.ID, bridge.ID} {
		if ok, err := h.store.IsChannelMember(ctx, member, channel); err != nil || !ok {
			t.Errorf("IsChannelMember(%s) = (%v, %v), want (true, nil)", member, ok, err)
		}
	}
	for _, lookalike := range lookalikes {
		if ok, err := h.store.IsChannelMember(ctx, supervisor.ID, lookalike); err != nil || ok {
			t.Errorf("supervisor membership of look-alike %s = (%v, %v), want (false, nil)", lookalike, ok, err)
		}
	}
	recipients, err := h.store.SubscribedAgents(ctx, channel, bridge.ID)
	if err != nil {
		t.Fatalf("SubscribedAgents: %v", err)
	}
	if !slices.Contains(recipients, supervisor.ID) {
		t.Errorf("bridge-post recipients = %v, want the supervisor (else a routed session wakes no one)", recipients)
	}

	gotSupervisor, gotChannel, err := routing.RoutingTarget(ctx)
	if err != nil {
		t.Fatalf("after a Linear-on seed: RoutingTarget: %v", err)
	}
	if gotSupervisor != supervisor.ID || gotChannel != string(channel) {
		t.Errorf("RoutingTarget = (%q, %q), want (%q, %q)", gotSupervisor, gotChannel, supervisor.ID, channel)
	}
}

// plantRoutingLookalikes plants a stranger's routing channel in the admin's
// SHARED __linear__ group and an admin-owned ungrouped routing channel.
func plantRoutingLookalikes(t *testing.T, st *store.Store, dsn string, adminID store.AccountID) []store.ChannelID {
	t.Helper()
	ctx := t.Context()
	stranger, err := st.CreateUser(ctx, store.NewUser{Handle: "stranger", DisplayName: "Stranger"})
	if err != nil {
		t.Fatalf("CreateUser(stranger): %v", err)
	}
	// The store refuses the reserved group name (store.linearRoutingGroupName), so plant with raw SQL.
	const sharedID, plantedID = "linear-lookalike-group", "linear-lookalike-channel"
	execSQL(t, ctx, dsn,
		`INSERT INTO channel_groups (id, name, parent_group_id, owner_user_id, namespace_owner_id, visibility, tenant_id)
		 SELECT $1, '__linear__', NULL, a.id, a.id, $2, a.tenant_id FROM accounts a WHERE a.id = $3`,
		sharedID, int16(store.VisibilityShared), string(adminID))
	execSQL(t, ctx, dsn,
		`INSERT INTO channels (id, name, group_id, kind, post_policy, owner_account_id, mandatory_subscription, tenant_id)
		 SELECT $1, $2, $3, $4, $5, a.id, FALSE, a.tenant_id FROM accounts a WHERE a.id = $6`,
		plantedID, store.LinearRoutingChannelName, sharedID, int32(store.ChannelKindChannel),
		int32(store.ChannelPostPolicyOpen), string(stranger.ID))
	userOwned, err := st.CreateChannel(ctx, adminID, store.NewChannel{Name: store.LinearRoutingChannelName})
	if err != nil {
		t.Fatalf("CreateChannel(admin's ungrouped): %v", err)
	}
	return []store.ChannelID{plantedID, userOwned.ID}
}

// TestLinearRoutingOwningManager: a live author resolves to itself, a despawned peer to
// its live parent, and a line with no live agent to ErrNotFound (the supervisor fallback).
func TestLinearRoutingOwningManager(t *testing.T) {
	ctx := t.Context()
	st := forgeTestStore(t)
	owner, err := st.CreateUser(ctx, store.NewUser{Handle: "owner", DisplayName: "Owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	manager, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "lane-manager", DisplayName: "Lane Manager", Role: "manager"})
	if err != nil {
		t.Fatalf("CreateAgent(manager): %v", err)
	}
	peer, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "lane-peer", DisplayName: "Lane Peer", ParentAgentID: manager.ID})
	if err != nil {
		t.Fatalf("CreateAgent(peer): %v", err)
	}
	routing := &linearRouting{st: st, adminID: owner.ID}

	if _, _, err := routing.OwningManager(ctx, peer.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("OwningManager(no live ancestor) err = %v, want store.ErrNotFound", err)
	}

	if err := st.RecordAgentPlacement(ctx, manager.ID, "runner-1", "compass-"+string(manager.ID)); err != nil {
		t.Fatalf("RecordAgentPlacement(manager): %v", err)
	}
	gotManager, gotChannel, err := routing.OwningManager(ctx, peer.ID)
	if err != nil {
		t.Fatalf("OwningManager(despawned peer): %v", err)
	}
	if gotManager != manager.ID || gotChannel != string(manager.Agent.HomeChannelID) {
		t.Errorf("OwningManager(despawned peer) = (%q, %q), want its live parent (%q, %q)", gotManager, gotChannel, manager.ID, manager.Agent.HomeChannelID)
	}

	if err := st.RecordAgentPlacement(ctx, peer.ID, "runner-1", "compass-"+string(peer.ID)); err != nil {
		t.Fatalf("RecordAgentPlacement(peer): %v", err)
	}
	gotManager, gotChannel, err = routing.OwningManager(ctx, peer.ID)
	if err != nil {
		t.Fatalf("OwningManager(live peer): %v", err)
	}
	if gotManager != peer.ID || gotChannel != string(peer.Agent.HomeChannelID) {
		t.Errorf("OwningManager(live peer) = (%q, %q), want itself (%q, %q)", gotManager, gotChannel, peer.ID, peer.Agent.HomeChannelID)
	}

	if _, _, err := routing.OwningManager(ctx, owner.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("OwningManager(non-agent) err = %v, want store.ErrNotFound", err)
	}
}

// TestLinearRoutingOwningManagerWalksPastDeadParent: the walk keeps climbing past an
// unplaced parent, so a live grandparent owns a despawned author's issue.
func TestLinearRoutingOwningManagerWalksPastDeadParent(t *testing.T) {
	ctx := t.Context()
	st := forgeTestStore(t)
	owner, err := st.CreateUser(ctx, store.NewUser{Handle: "owner", DisplayName: "Owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	root, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "root-manager", DisplayName: "Root", Role: "manager"})
	if err != nil {
		t.Fatalf("CreateAgent(root): %v", err)
	}
	mid, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "mid-manager", DisplayName: "Mid", ParentAgentID: root.ID})
	if err != nil {
		t.Fatalf("CreateAgent(mid): %v", err)
	}
	author, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "author", DisplayName: "Author", ParentAgentID: mid.ID})
	if err != nil {
		t.Fatalf("CreateAgent(author): %v", err)
	}
	if err := st.RecordAgentPlacement(ctx, root.ID, "runner-1", "compass-"+string(root.ID)); err != nil {
		t.Fatalf("RecordAgentPlacement(root): %v", err)
	}

	gotManager, gotChannel, err := (&linearRouting{st: st, adminID: owner.ID}).OwningManager(ctx, author.ID)
	if err != nil {
		t.Fatalf("OwningManager(author): %v", err)
	}
	if gotManager != root.ID || gotChannel != string(root.Agent.HomeChannelID) {
		t.Errorf("OwningManager(author) = (%q, %q), want the live grandparent (%q, %q)", gotManager, gotChannel, root.ID, root.Agent.HomeChannelID)
	}
}

// TestSeedWarnsNoLinearRoutingTarget: with Linear on, each early-return arm of the
// seed warns once that cold delegations have no routing target; Linear off stays quiet.
func TestSeedWarnsNoLinearRoutingTarget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, st *store.Store, adminID store.AccountID)
	}{
		{name: "operator-built root", plant: func(t *testing.T, st *store.Store, adminID store.AccountID) {
			t.Helper()
			mustRootAgent(t, st, adminID, "operator-root")
		}},
		{name: "supervisor handle on a non-root agent", plant: func(t *testing.T, st *store.Store, adminID store.AccountID) {
			t.Helper()
			root := mustRootAgent(t, st, adminID, "operator-root")
			if _, err := st.CreateAgent(t.Context(), adminID, store.NewAgent{Handle: rootSupervisorHandle, DisplayName: rootSupervisorDisplayName, ParentAgentID: root.ID}); err != nil {
				t.Fatalf("CreateAgent(child supervisor): %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			h := newSeedHarness(t)
			bridge, err := h.store.EnsureLinearBridgeAccount(ctx)
			if err != nil {
				t.Fatalf("EnsureLinearBridgeAccount: %v", err)
			}
			tc.plant(t, h.store, h.adminID)

			on := &capHandler{}
			seedRootSupervisor(ctx, h.store, h.svc, h.commsSvc, h.adminID, h.compassID, bridge.ID, slog.New(on))
			if n := warnCount(on.recs); n != 1 {
				t.Fatalf("Warn count with Linear on = %d, want 1", n)
			}

			off := &capHandler{}
			seedRootSupervisor(ctx, h.store, h.svc, h.commsSvc, h.adminID, h.compassID, "", slog.New(off))
			if n := warnCount(off.recs); n != 0 {
				t.Fatalf("Warn count with Linear off = %d, want 0", n)
			}
		})
	}
}

// mustRootAgent creates a root agent under adminID or fails the test.
func mustRootAgent(t *testing.T, st *store.Store, adminID store.AccountID, handle string) store.Account {
	t.Helper()
	agent, err := st.CreateAgent(t.Context(), adminID, store.NewAgent{Handle: handle, DisplayName: handle})
	if err != nil {
		t.Fatalf("CreateAgent(%s): %v", handle, err)
	}
	return agent
}
