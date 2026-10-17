//go:build unix

package server

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect/v2"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/store"
)

// Step ids are short code-defined names; the bound stops one account storing megabytes.
const maxTourStepIDBytes = 128

func (s *service) GetTourState(
	ctx context.Context,
	_ *compassv1.GetTourStateRequest,
) (*compassv1.GetTourStateResponse, error) {
	caller, ok := auth.CallerFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errNoCaller.Error()).WithCause(errNoCaller)
	}
	state, err := s.store.GetTourState(ctx, caller)
	if err != nil {
		wrappedErr := fmt.Errorf("get tour state: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}
	return &compassv1.GetTourStateResponse{
		Outcome: tourOutcomeToProto(state.Outcome),
		StepId:  state.StepID,
	}, nil
}

func (s *service) ClaimTourStart(
	ctx context.Context,
	req *compassv1.ClaimTourStartRequest,
) (*compassv1.ClaimTourStartResponse, error) {
	caller, ok := auth.CallerFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errNoCaller.Error()).WithCause(errNoCaller)
	}
	if err := checkTourStepID(req.GetStepId()); err != nil {
		return nil, err
	}
	claimed, err := s.store.ClaimTourStart(ctx, caller, req.GetStepId())
	if err != nil {
		wrappedErr := fmt.Errorf("claim tour start: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}
	return &compassv1.ClaimTourStartResponse{Claimed: claimed}, nil
}

func (s *service) SetTourState(
	ctx context.Context,
	req *compassv1.SetTourStateRequest,
) (*compassv1.SetTourStateResponse, error) {
	caller, ok := auth.CallerFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errNoCaller.Error()).WithCause(errNoCaller)
	}
	outcome, err := tourOutcomeFromProto(req.GetOutcome())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	if err := checkTourStepID(req.GetStepId()); err != nil {
		return nil, err
	}
	if err := s.store.SetTourState(ctx, caller, outcome, req.GetStepId()); err != nil {
		if errors.Is(err, store.ErrInvalidArgument) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
		}
		wrappedErr := fmt.Errorf("set tour state: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}
	return &compassv1.SetTourStateResponse{}, nil
}

func checkTourStepID(stepID string) error {
	if len(stepID) > maxTourStepIDBytes {
		return connect.Errorf(connect.CodeInvalidArgument,
			"tour step_id exceeds %d bytes", maxTourStepIDBytes)
	}
	return nil
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
