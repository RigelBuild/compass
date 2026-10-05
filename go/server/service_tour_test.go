//go:build unix

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
)

func TestTourRPCsWithoutCallerAreUnauthenticated(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	service := newService("tour-test", bus, nil, nil, nil, nil, nil)
	client := newH2CClient(t, newH2CTestServer(t, service))

	checks := []struct {
		name string
		call func() error
	}{
		{
			name: "GetTourState",
			call: func() error {
				_, err := client.GetTourState(t.Context(), connect.NewRequest(&compassv1.GetTourStateRequest{}))
				return err
			},
		},
		{
			name: "ClaimTourStart",
			call: func() error {
				_, err := client.ClaimTourStart(t.Context(), connect.NewRequest(&compassv1.ClaimTourStartRequest{StepId: "welcome"}))
				return err
			},
		},
		{
			name: "SetTourState",
			call: func() error {
				_, err := client.SetTourState(t.Context(), connect.NewRequest(&compassv1.SetTourStateRequest{Outcome: compassv1.TourOutcome_TOUR_OUTCOME_COMPLETED}))
				return err
			},
		},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.call(); connect.CodeOf(err) != connect.CodeUnauthenticated {
				t.Fatalf("RPC without caller error = %v, want CodeUnauthenticated", err)
			}
		})
	}
}

func TestTourRPCsRejectInvalidInput(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	service := newService("tour-test", bus, nil, nil, nil, nil, nil)
	path, handler := compassv1connect.NewCompassServiceHandler(service,
		connect.WithInterceptors(auth.AmbientIdentity("tour-account")))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	client := newH2CClient(t, srv.URL)
	oversized := strings.Repeat("x", maxTourStepIDBytes+1)

	checks := map[string]func() error{
		"SetTourState UNSPECIFIED": func() error {
			_, err := client.SetTourState(t.Context(), connect.NewRequest(&compassv1.SetTourStateRequest{}))
			return err
		},
		"SetTourState oversized step_id": func() error {
			_, err := client.SetTourState(t.Context(), connect.NewRequest(&compassv1.SetTourStateRequest{
				Outcome: compassv1.TourOutcome_TOUR_OUTCOME_STARTED, StepId: oversized,
			}))
			return err
		},
		"ClaimTourStart oversized step_id": func() error {
			_, err := client.ClaimTourStart(t.Context(), connect.NewRequest(&compassv1.ClaimTourStartRequest{StepId: oversized}))
			return err
		},
	}
	for name, call := range checks {
		t.Run(name, func(t *testing.T) {
			if err := call(); connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("error = %v, want CodeInvalidArgument", err)
			}
		})
	}
}
