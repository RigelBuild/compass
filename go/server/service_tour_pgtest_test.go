//go:build pgtest && unix

package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/auth"
)

func TestNonAdminBearerCanUseTourRPCs(t *testing.T) {
	ctx := t.Context()
	st, admin, member := newNetworkStore(t)
	memberToken, err := auth.IssueAccountToken(ctx, st, member)
	if err != nil {
		t.Fatalf("IssueAccountToken(member): %v", err)
	}
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	client := networkDoorHandler(t, newService("tour-test", bus, st, nil, nil, nil, nil), st, admin)

	get := connect.NewRequest(&compassv1.GetTourStateRequest{})
	get.Header().Set("Authorization", "Bearer "+memberToken)
	state, err := client.GetTourState(ctx, get)
	if err != nil {
		t.Fatalf("non-admin GetTourState: %v", err)
	}
	if state.Msg.GetOutcome() != compassv1.TourOutcome_TOUR_OUTCOME_UNSPECIFIED {
		t.Fatalf("initial outcome = %v, want unspecified", state.Msg.GetOutcome())
	}

	claim := connect.NewRequest(&compassv1.ClaimTourStartRequest{StepId: "welcome"})
	claim.Header().Set("Authorization", "Bearer "+memberToken)
	claimed, err := client.ClaimTourStart(ctx, claim)
	if err != nil {
		t.Fatalf("non-admin ClaimTourStart: %v", err)
	}
	if !claimed.Msg.GetClaimed() {
		t.Fatal("first non-admin ClaimTourStart returned false, want true")
	}

	set := connect.NewRequest(&compassv1.SetTourStateRequest{
		Outcome: compassv1.TourOutcome_TOUR_OUTCOME_DISMISSED,
		StepId:  "last-step",
	})
	set.Header().Set("Authorization", "Bearer "+memberToken)
	if _, err := client.SetTourState(ctx, set); err != nil {
		t.Fatalf("non-admin SetTourState: %v", err)
	}

	verify := connect.NewRequest(&compassv1.GetTourStateRequest{})
	verify.Header().Set("Authorization", "Bearer "+memberToken)
	state, err = client.GetTourState(ctx, verify)
	if err != nil {
		t.Fatalf("non-admin GetTourState after SetTourState: %v", err)
	}
	if state.Msg.GetOutcome() != compassv1.TourOutcome_TOUR_OUTCOME_DISMISSED || state.Msg.GetStepId() != "last-step" {
		t.Fatalf("state after set = %v at %q, want dismissed at last-step", state.Msg.GetOutcome(), state.Msg.GetStepId())
	}
}

func TestTourRPCsWithoutCallerAreUnauthenticatedOnDoor(t *testing.T) {
	ctx := context.Background()
	st, admin, _ := newNetworkStore(t)
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	client := networkDoorHandler(t, newService("tour-test", bus, st, nil, nil, nil, nil), st, admin)

	_, err := client.GetTourState(ctx, connect.NewRequest(&compassv1.GetTourStateRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("GetTourState without bearer = %v, want CodeUnauthenticated", err)
	}
}
