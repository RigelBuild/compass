package store

import (
	"context"
	"fmt"
	"time"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

type TourOutcome string

const (
	TourOutcomeUnspecified TourOutcome = ""
	TourOutcomeStarted     TourOutcome = "started"
	TourOutcomeDismissed   TourOutcome = "dismissed"
	TourOutcomeCompleted   TourOutcome = "completed"
)

type TourState struct {
	Outcome   TourOutcome
	StepID    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// GetTourState returns the account's tour state, or the zero state when unseen.
func (s *Store) GetTourState(ctx context.Context, account AccountID) (TourState, error) {
	row, err := s.q.GetTourState(ctx, string(account))
	if err != nil {
		if noRows(err) {
			return TourState{}, nil
		}
		return TourState{}, fmt.Errorf("store: get account tour state: %w", err)
	}
	state := TourState{
		Outcome:   TourOutcome(row.Outcome),
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
	}
	if row.StepID.Valid {
		state.StepID = row.StepID.String
	}
	return state, nil
}

// ClaimTourStart creates the first-run row only if no state exists yet.
func (s *Store) ClaimTourStart(ctx context.Context, account AccountID, stepID string) (bool, error) {
	_, err := s.q.ClaimTourStart(ctx, db.ClaimTourStartParams{AccountID: string(account), Column2: stepID})
	if err != nil {
		if noRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: claim account tour start: %w", err)
	}
	return true, nil
}

// SetTourState upserts the account's outcome and resume cursor.
func (s *Store) SetTourState(ctx context.Context, account AccountID, outcome TourOutcome, stepID string) error {
	if outcome != TourOutcomeStarted && outcome != TourOutcomeDismissed && outcome != TourOutcomeCompleted {
		return fmt.Errorf("%w: invalid tour outcome %q", ErrInvalidArgument, outcome)
	}
	if err := s.q.SetTourState(ctx, db.SetTourStateParams{
		AccountID: string(account),
		Outcome:   string(outcome),
		Column3:   stepID,
	}); err != nil {
		return fmt.Errorf("store: set account tour state: %w", err)
	}
	return nil
}
