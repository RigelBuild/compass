//go:build pgtest

package comms

import (
	"context"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
)

// An open client learns a new agent's home channel id from AccountChanged, so the
// channel itself must arrive on the stream too, or the client cannot render it.
func TestCreateAgentEmitsHomeChannelChanged(t *testing.T) {
	h := newStreamHarness(t)
	ctx := context.Background()
	owner := mustUser(t, h.store, "home-event-owner")

	resp, err := h.svc.CreateAgent(WithActor(ctx, owner.ID), &compassv1.CreateAgentRequest{
		Handle: "home-event-agent", DisplayName: "Home Event Agent", Role: "manager",
	})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	homeID := resp.GetAccount().GetAgent().GetHomeChannelId()
	if homeID == "" {
		t.Fatal("CreateAgent returned no home channel id")
	}

	canary := mkCanary(t, h, "home-event-canary")
	evts := drainReplayAsActor(t, h, owner.ID, canary)
	if n := len(channelChanges(evts, homeID)); n != 1 {
		t.Fatalf("ChannelChanged events for the home channel = %d, want 1", n)
	}
}

// The event follows a committed create, so a caller that has already hung up
// must not suppress it for every other open client.
func TestPublishHomeChannelChangedSurvivesCanceledRequest(t *testing.T) {
	h := newStreamHarness(t)
	owner := mustUser(t, h.store, "home-cancel-owner")
	agent := mustAgent(t, h.store, owner.ID, "home-cancel-agent")

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	h.svc.publishHomeChannelChanged(canceled, agent.Agent.HomeChannelID)

	canary := mkCanary(t, h, "home-cancel-canary")
	evts := drainReplayAsActor(t, h, owner.ID, canary)
	if n := len(channelChanges(evts, string(agent.Agent.HomeChannelID))); n != 1 {
		t.Fatalf("ChannelChanged events for the home channel = %d, want 1", n)
	}
}
