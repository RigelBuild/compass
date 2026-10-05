//go:build unix

package server

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/store"
)

func (s *service) GetTourState(
	ctx context.Context,
	_ *connect.Request[compassv1.GetTourStateRequest],
) (*connect.Response[compassv1.GetTourStateResponse], error) {
	caller, ok := auth.CallerFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errNoCaller)
	}
	state, err := s.store.GetTourState(ctx, caller)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get tour state: %w", err))
	}
	return connect.NewResponse(&compassv1.GetTourStateResponse{
		Outcome: tourOutcomeToProto(state.Outcome),
		StepId:  state.StepID,
	}), nil
}

func (s *service) ClaimTourStart(
	ctx context.Context,
	req *connect.Request[compassv1.ClaimTourStartRequest],
) (*connect.Response[compassv1.ClaimTourStartResponse], error) {
	caller, ok := auth.CallerFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errNoCaller)
	}
	claimed, err := s.store.ClaimTourStart(ctx, caller, req.Msg.GetStepId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("claim tour start: %w", err))
	}
	return connect.NewResponse(&compassv1.ClaimTourStartResponse{Claimed: claimed}), nil
}

func (s *service) SetTourState(
	ctx context.Context,
	req *connect.Request[compassv1.SetTourStateRequest],
) (*connect.Response[compassv1.SetTourStateResponse], error) {
	caller, ok := auth.CallerFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errNoCaller)
	}
	outcome, err := tourOutcomeFromProto(req.Msg.GetOutcome())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := s.store.SetTourState(ctx, caller, outcome, req.Msg.GetStepId()); err != nil {
		if errors.Is(err, store.ErrInvalidArgument) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("set tour state: %w", err))
	}
	return connect.NewResponse(&compassv1.SetTourStateResponse{}), nil
}

func tourOutcomeFromProto(outcome compassv1.TourOutcome) (store.TourOutcome, error) {
	switch outcome {
	case compassv1.TourOutcome_TOUR_OUTCOME_STARTED:
		return store.TourOutcomeStarted, nil
	case compassv1.TourOutcome_TOUR_OUTCOME_DISMISSED:
		return store.TourOutcomeDismissed, nil
	case compassv1.TourOutcome_TOUR_OUTCOME_COMPLETED:
		return store.TourOutcomeCompleted, nil
	default:
		return store.TourOutcomeUnspecified, errors.New("tour outcome must be specified")
	}
}

func tourOutcomeToProto(outcome store.TourOutcome) compassv1.TourOutcome {
	switch outcome {
	case store.TourOutcomeStarted:
		return compassv1.TourOutcome_TOUR_OUTCOME_STARTED
	case store.TourOutcomeDismissed:
		return compassv1.TourOutcome_TOUR_OUTCOME_DISMISSED
	case store.TourOutcomeCompleted:
		return compassv1.TourOutcome_TOUR_OUTCOME_COMPLETED
	default:
		return compassv1.TourOutcome_TOUR_OUTCOME_UNSPECIFIED
	}
}
