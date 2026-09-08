package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// The agent-driven state-transition memo (design
// docs/designs/server/compass-forge-state-transition/design.md §Actor
// attribution): the durable carrier of WHICH agent drove a forge state
// transition, across the write→webhook gap. A transition has no body, so the
// DL-050 owner header cannot attribute it, and every Server-credential write
// presents the shared App bot login — so the write chokepoint records the
// acting agent here (strictly AFTER a provider success) and the notify lane
// consumes the memo on match to resolve the echoed STATE event's actor.
//
// The memo is deliberately NOT the forge_authored_artifacts row at the same
// coordinate: that row is a write-once AUTHORSHIP fact whose DO UPDATE would
// destroy the original create's F3 idempotency memo, and keying suppression off
// authorship is the author-row-proxy failure RIG-3326 rejects.

// TransitionStateOpen and TransitionStateClosed are the portable applied-state
// domain a memo records — exactly the forge.Issue.State domain and the
// forge_state_transitions.state CHECK. A memo never carries a provider-native
// workflow-state name: the notify lane matches this value against the echoed
// event's portable state.
const (
	TransitionStateOpen   = "open"
	TransitionStateClosed = "closed"
)

// validTransitionState rejects a state outside the portable domain before any DB
// round trip — the CHECK's job in Go space, so a caller bug is
// ErrInvalidArgument rather than a raw constraint violation.
func validTransitionState(state string) error {
	if state != TransitionStateOpen && state != TransitionStateClosed {
		return fmt.Errorf("%w: transition state must be %q or %q, got %q", ErrInvalidArgument, TransitionStateOpen, TransitionStateClosed, state)
	}
	return nil
}

// RecordStateTransition upserts the memo at the forge coordinate: the portable
// state the transition APPLIED, the agent that drove it, and when it was
// written. Latest transition wins — a re-transition of the same coordinate
// re-lands on the PK and resets the consumed flag, so the newest transition is
// attributable even when the previous one was already consumed. Zero/empty
// coordinate fields, a zero kind, an out-of-domain state, an empty agent, or a
// zero timestamp -> ErrInvalidArgument; an unknown agent -> ErrInvalidArgument
// (the FK RESTRICT).
func (s *Store) RecordStateTransition(ctx context.Context, provider ForgeProvider, host, repo string, kind ForgeArtifactKind, number uint64, state string, agent AccountID, at time.Time) error {
	if err := validCoordinate(provider, host, repo); err != nil {
		return err
	}
	if kind == ForgeArtifactKindUnspecified {
		return fmt.Errorf("%w: artifact kind is required", ErrInvalidArgument)
	}
	if err := validTransitionState(state); err != nil {
		return err
	}
	if agent == "" {
		return fmt.Errorf("%w: agent account id is required", ErrInvalidArgument)
	}
	if at.IsZero() {
		return fmt.Errorf("%w: transition timestamp is required", ErrInvalidArgument)
	}
	if err := s.q.RecordStateTransition(ctx, db.RecordStateTransitionParams{
		ForgeProvider:  int16(provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum (forge_state_transitions.forge_provider), always within int16
		ForgeHost:      host,
		Repo:           repo,
		Kind:           int16(kind),   //nolint:gosec // G115: ForgeArtifactKind is a CHECK-constrained 1/2 enum (forge_state_transitions.kind), always within int16
		Number:         int64(number), //nolint:gosec // G115: number is a canonical forge artifact number (a positive issue/PR number) written to a BIGINT, always well within the int64 domain.
		State:          state,
		AgentAccountID: string(agent),
		WrittenAt:      pgtype.Timestamptz{Time: at, Valid: true},
	}); err != nil {
		if pgErrIs(err, pgForeignKeyViolation) {
			return fmt.Errorf("%w: unknown agent %q", ErrInvalidArgument, agent)
		}
		return fmt.Errorf("store: record state transition: %w", err)
	}
	return nil
}

// ConsumeStateTransition resolves and CLAIMS the memo at the forge coordinate in
// one statement: it returns the acting agent iff an unconsumed memo exists whose
// applied state matches state and whose written_at is at or after fresh (the
// freshness bound). The claim and the read are the same UPDATE … RETURNING, so
// one memo attributes at most one event and a concurrent second reader gets
// ok=false.
//
// A miss — no memo, an already-consumed memo, a state mismatch, or a memo older
// than fresh — is ok=false with NO error: that is the correct answer for every
// human/external transition and the safe answer for every race (fail-open — an
// unattributed self-transition costs one redundant wake, never a lost
// cross-agent signal). Zero/empty coordinate fields, a zero kind, or an
// out-of-domain state -> ErrInvalidArgument.
func (s *Store) ConsumeStateTransition(ctx context.Context, provider ForgeProvider, host, repo string, kind ForgeArtifactKind, number uint64, state string, fresh time.Time) (AccountID, bool, error) {
	if err := validCoordinate(provider, host, repo); err != nil {
		return "", false, err
	}
	if kind == ForgeArtifactKindUnspecified {
		return "", false, fmt.Errorf("%w: artifact kind is required", ErrInvalidArgument)
	}
	if err := validTransitionState(state); err != nil {
		return "", false, err
	}
	agent, err := s.q.ConsumeStateTransition(ctx, db.ConsumeStateTransitionParams{
		ForgeProvider: int16(provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum, always within int16
		ForgeHost:     host,
		Repo:          repo,
		Kind:          int16(kind),   //nolint:gosec // G115: ForgeArtifactKind is a CHECK-constrained 1/2 enum, always within int16
		Number:        int64(number), //nolint:gosec // G115: number is a canonical forge artifact number written to a BIGINT, always well within the int64 domain.
		State:         state,
		WrittenAt:     pgtype.Timestamptz{Time: fresh, Valid: true},
	})
	if err != nil {
		if noRows(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("store: consume state transition: %w", err)
	}
	return AccountID(agent), true, nil
}
