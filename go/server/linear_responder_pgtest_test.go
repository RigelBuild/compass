//go:build pgtest && unix

package server

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/RigelBuild/compass/go/internal/store"
)

// TestLinearRoutingSeedAndFallbackTarget walks the fallback through the seed: no
// supervisor, no channel while Linear is off, then one channel that wakes the supervisor.
func TestLinearRoutingSeedAndFallbackTarget(t *testing.T) {
	ctx := context.Background()
	h := newSeedHarness(t)
	bridge, err := h.store.EnsureLinearBridgeAccount(ctx)
	if err != nil {
		t.Fatalf("EnsureLinearBridgeAccount: %v", err)
	}
	routing := &linearRouting{st: h.store, adminID: h.adminID}

	if _, _, err := routing.RoutingTarget(ctx); !errors.Is(err, errRoutingSupervisor) {
		t.Fatalf("before any seed: err = %v, want errRoutingSupervisor", err)
	}

	attachFakeRunner(t, h.store, h.hub, false) // the harness hook seeds with Linear off
	h.awaitSeed(t)
	if _, _, err := routing.RoutingTarget(ctx); !errors.Is(err, errRoutingChannel) {
		t.Fatalf("after a Linear-off seed: err = %v, want errRoutingChannel", err)
	}

	for range 2 {
		seedRootSupervisor(ctx, h.store, h.svc, h.commsSvc, h.adminID, h.compassID, bridge.ID, slog.New(slog.DiscardHandler))
	}

	supervisor, err := h.store.AgentByHandle(ctx, h.adminID, rootSupervisorHandle)
	if err != nil {
		t.Fatalf("AgentByHandle(supervisor): %v", err)
	}
	// ChannelByNameForViewer rejects an ambiguous name, so success means exactly one.
	channel, err := h.store.ChannelByNameForViewer(ctx, h.adminID, linearRoutingChannelName)
	if err != nil {
		t.Fatalf("routing channel after two seeds: %v, want exactly one", err)
	}
	for _, member := range []store.AccountID{supervisor.ID, bridge.ID} {
		if ok, err := h.store.IsChannelMember(ctx, member, channel.ID); err != nil || !ok {
			t.Errorf("IsChannelMember(%s) = (%v, %v), want (true, nil)", member, ok, err)
		}
	}
	recipients, err := h.store.SubscribedAgents(ctx, channel.ID, bridge.ID)
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
	if gotSupervisor != supervisor.ID || gotChannel != string(channel.ID) {
		t.Errorf("RoutingTarget = (%q, %q), want (%q, %q)", gotSupervisor, gotChannel, supervisor.ID, channel.ID)
	}
}

// TestEnsureLinearRoutingChannelConcurrentOneChannel pins the seed mutex: ready
// hooks racing on their own goroutines still converge on one routing channel.
func TestEnsureLinearRoutingChannelConcurrentOneChannel(t *testing.T) {
	ctx := context.Background()
	st := forgeTestStore(t)
	admin, err := st.CreateUser(ctx, store.NewUser{Handle: "routing-admin", DisplayName: "Routing Admin"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	supervisor, err := st.CreateAgent(ctx, admin.ID, store.NewAgent{Handle: rootSupervisorHandle, DisplayName: rootSupervisorDisplayName, Role: rootSupervisorRole})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	bridge, err := st.EnsureLinearBridgeAccount(ctx)
	if err != nil {
		t.Fatalf("EnsureLinearBridgeAccount: %v", err)
	}

	// Unguarded, 16 racers minted duplicates in 2 of 5 measured runs.
	const racers = 16
	start := make(chan struct{})
	errs := make(chan error, racers)
	var wg sync.WaitGroup
	for range racers {
		wg.Go(func() {
			<-start
			errs <- ensureLinearRoutingChannel(ctx, st, admin.ID, supervisor.ID, bridge.ID)
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("ensureLinearRoutingChannel: %v", err)
		}
	}
	if _, err := st.ChannelByNameForViewer(ctx, admin.ID, linearRoutingChannelName); err != nil {
		t.Fatalf("routing channel after %d racing ensures: %v, want exactly one", racers, err)
	}
}

// TestLinearRoutingOwningManager pins ancestor-or-self: an agent resolves to
// itself and its home channel, and a non-agent is ErrNotFound.
func TestLinearRoutingOwningManager(t *testing.T) {
	ctx := context.Background()
	st := forgeTestStore(t)
	owner, err := st.CreateUser(ctx, store.NewUser{Handle: "owner", DisplayName: "Owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "lane-manager", DisplayName: "Lane Manager", Role: "manager"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	routing := &linearRouting{st: st, adminID: owner.ID}

	gotManager, gotChannel, err := routing.OwningManager(ctx, agent.ID)
	if err != nil {
		t.Fatalf("OwningManager(agent): %v", err)
	}
	if gotManager != agent.ID || gotChannel != string(agent.Agent.HomeChannelID) {
		t.Errorf("OwningManager(agent) = (%q, %q), want (%q, %q)", gotManager, gotChannel, agent.ID, agent.Agent.HomeChannelID)
	}

	if _, _, err := routing.OwningManager(ctx, owner.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("OwningManager(non-agent) err = %v, want store.ErrNotFound", err)
	}
}
