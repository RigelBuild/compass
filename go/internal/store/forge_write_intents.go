package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RigelBuild/compass/go/internal/forge"
	"github.com/RigelBuild/compass/go/internal/store/db"
)

// WriteState is the durable outcome of one attempt to create a forge object.
type WriteState string

const (
	WriteStatePending WriteState = "pending"
	WriteStateDone    WriteState = "done"
	WriteStateFailed  WriteState = "failed"
	WriteStateLost    WriteState = "lost"
)

// WriteIntent records one forge create attempt and the agent that owns its result.
type WriteIntent struct {
	ID             string
	Provider       ForgeProvider
	Host           string
	Repo           string
	Kind           forge.WriteKind
	Parent         uint64
	HeadRef        string
	AgentAccountID AccountID
	OwnerUserID    AccountID
	SessionID      string
	RequestKey     string
	State          WriteState
}

// RecordWriteIntent commits an idempotency record before any forge create.
// It returns an existing non-failed attempt for the same agent and request key.
func (s *Store) RecordWriteIntent(ctx context.Context, in WriteIntent) (WriteIntent, error) {
	if err := validCoordinate(in.Provider, in.Host, in.Repo); err != nil {
		return WriteIntent{}, err
	}
	if in.Kind < forge.WriteKindIssue || in.Kind > forge.WriteKindReview {
		return WriteIntent{}, fmt.Errorf("%w: write kind is required", ErrInvalidArgument)
	}
	if in.RequestKey == "" {
		return WriteIntent{}, fmt.Errorf("%w: client request id is required", ErrInvalidArgument)
	}
	if in.AgentAccountID == "" || in.OwnerUserID == "" {
		return WriteIntent{}, fmt.Errorf("%w: agent and owner account ids are required", ErrInvalidArgument)
	}
	if in.Parent > math.MaxInt64 {
		return WriteIntent{}, fmt.Errorf("%w: parent number is out of range", ErrInvalidArgument)
	}
	if in.Kind == forge.WriteKindPullRequest && in.HeadRef == "" {
		return WriteIntent{}, fmt.Errorf("%w: pull request head ref is required", ErrInvalidArgument)
	}
	if (in.Kind == forge.WriteKindComment || in.Kind == forge.WriteKindReview) && in.Parent == 0 {
		return WriteIntent{}, fmt.Errorf("%w: parent number is required", ErrInvalidArgument)
	}
	if in.Kind != forge.WriteKindPullRequest && in.HeadRef != "" {
		return WriteIntent{}, fmt.Errorf("%w: head ref is only valid for pull requests", ErrInvalidArgument)
	}
	in.Repo = (ForgeCoord{Provider: in.Provider, Repo: in.Repo}).Normalized().Repo
	row, err := s.q.RecordForgeWriteIntent(ctx, db.RecordForgeWriteIntentParams{
		ID:              uuid.NewString(),
		ForgeProvider:   int16(in.Provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum.
		ForgeHost:       in.Host,
		Repo:            in.Repo,
		Kind:            int16(in.Kind),
		ParentNumber:    int64(in.Parent),
		HeadRef:         in.HeadRef,
		AgentAccountID:  string(in.AgentAccountID),
		OwnerUserID:     string(in.OwnerUserID),
		SessionID:       in.SessionID,
		ClientRequestID: in.RequestKey,
	})
	if err != nil {
		if pgErrIs(err, pgUniqueViolation) {
			return WriteIntent{}, fmt.Errorf("%w: write intent key or pull request head is already claimed", ErrConflict)
		}
		if pgErrIs(err, pgForeignKeyViolation) {
			return WriteIntent{}, fmt.Errorf("%w: unknown agent or owner account", ErrInvalidArgument)
		}
		return WriteIntent{}, fmt.Errorf("store: record forge write intent: %w", err)
	}
	return writeIntentFromRow(row), nil
}

// PRIntentForHead returns the newest attempt associated with a pull-request head.
func (s *Store) PRIntentForHead(ctx context.Context, provider ForgeProvider, host, repo, headRef string) (WriteIntent, error) {
	if err := validCoordinate(provider, host, repo); err != nil {
		return WriteIntent{}, err
	}
	if headRef == "" {
		return WriteIntent{}, fmt.Errorf("%w: pull request head ref is required", ErrInvalidArgument)
	}
	repo = (ForgeCoord{Provider: provider, Repo: repo}).Normalized().Repo
	row, err := s.q.PRWriteIntentByHead(ctx, db.PRWriteIntentByHeadParams{
		ForgeProvider: int16(provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum.
		ForgeHost:     host,
		Repo:          repo,
		HeadRef:       headRef,
	})
	if err != nil {
		if noRows(err) {
			return WriteIntent{}, fmt.Errorf("%w: pull request intent for head %q", ErrNotFound, headRef)
		}
		return WriteIntent{}, fmt.Errorf("store: read forge write intent by head: %w", err)
	}
	return writeIntentFromRow(row), nil
}

// CompleteWriteIntent records artifact ownership or a comment/review forge key
// and marks the pending attempt done in one transaction.
func (s *Store) CompleteWriteIntent(ctx context.Context, id string, artifact *AuthoredArtifact, forgeKey string) (retErr error) {
	if id == "" {
		return fmt.Errorf("%w: write intent id is required", ErrInvalidArgument)
	}
	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return fmt.Errorf("store: begin complete forge write intent: %w", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			retErr = errors.Join(retErr, fmt.Errorf("store: roll back complete forge write intent: %w", err))
		}
	}()
	qtx := s.q.WithTx(tx)
	row, err := qtx.LockWriteIntent(ctx, id)
	if err != nil {
		if noRows(err) {
			return fmt.Errorf("%w: write intent %q", ErrNotFound, id)
		}
		return fmt.Errorf("store: lock forge write intent: %w", err)
	}
	if WriteState(row.State) != WriteStatePending {
		return fmt.Errorf("%w: write intent %q is %s", ErrConflict, id, row.State)
	}

	kind := writeKindFromRow(row.Kind)
	if kind == 0 {
		return fmt.Errorf("store: invalid forge write intent kind %d", row.Kind)
	}
	if kind == forge.WriteKindIssue || kind == forge.WriteKindPullRequest {
		if artifact == nil {
			return fmt.Errorf("%w: authored artifact is required", ErrInvalidArgument)
		}
		if err := artifact.valid(); err != nil {
			return err
		}
		wantKind := ForgeArtifactKindIssue
		if kind == forge.WriteKindPullRequest {
			wantKind = ForgeArtifactKindPullRequest
		}
		wantRepo := (ForgeCoord{Provider: ForgeProvider(row.ForgeProvider), Repo: row.Repo}).Normalized().Repo
		gotRepo := (ForgeCoord{Provider: artifact.Provider, Repo: artifact.Repo}).Normalized().Repo
		if artifact.Provider != ForgeProvider(row.ForgeProvider) || artifact.Host != row.ForgeHost ||
			gotRepo != wantRepo || artifact.Kind != wantKind || artifact.Number == 0 || artifact.Number > math.MaxInt64 ||
			artifact.AgentAccountID != AccountID(row.AgentAccountID) || artifact.OwnerUserID != AccountID(row.OwnerUserID) ||
			artifact.SessionID != row.SessionID {
			return fmt.Errorf("%w: authored artifact does not match write intent %q", ErrInvalidArgument, id)
		}
		record := *artifact
		record.ClientRequestID = ""
		if err := recordAuthoredArtifact(ctx, qtx, record); err != nil {
			return err
		}
		forgeKey = ""
	} else if artifact != nil || forgeKey == "" {
		return fmt.Errorf("%w: comment and review intents require a forge key only", ErrInvalidArgument)
	}

	affected, err := qtx.CompleteForgeWriteIntent(ctx, db.CompleteForgeWriteIntentParams{
		ID:       id,
		ForgeKey: forgeKey,
	})
	if err != nil {
		return fmt.Errorf("store: complete forge write intent: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("%w: write intent %q is no longer pending", ErrConflict, id)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit complete forge write intent: %w", err)
	}
	return nil
}

// EndWriteIntent changes a pending attempt to failed or lost; terminal rows are immutable.
func (s *Store) EndWriteIntent(ctx context.Context, id string, state WriteState) error {
	if id == "" {
		return fmt.Errorf("%w: write intent id is required", ErrInvalidArgument)
	}
	if state != WriteStateFailed && state != WriteStateLost {
		return fmt.Errorf("%w: terminal write intent state must be failed or lost", ErrInvalidArgument)
	}
	affected, err := s.q.EndForgeWriteIntent(ctx, db.EndForgeWriteIntentParams{ID: id, State: string(state)})
	if err != nil {
		return fmt.Errorf("store: end forge write intent: %w", err)
	}
	if affected == 1 {
		return nil
	}
	_, err = s.q.WriteIntentByID(ctx, id)
	if err != nil {
		if noRows(err) {
			return fmt.Errorf("%w: write intent %q", ErrNotFound, id)
		}
		return fmt.Errorf("store: read forge write intent after state change: %w", err)
	}
	return fmt.Errorf("%w: write intent %q is no longer pending", ErrConflict, id)
}

// PendingWriteIntents returns pending intents older than olderThan, oldest first.
func (s *Store) PendingWriteIntents(ctx context.Context, olderThan time.Time, limit int) ([]WriteIntent, error) {
	if limit < 1 || int64(limit) > math.MaxInt32 {
		return nil, fmt.Errorf("%w: pending write intent limit must be between 1 and %d", ErrInvalidArgument, math.MaxInt32)
	}
	rows, err := s.q.PendingForgeWriteIntents(ctx, db.PendingForgeWriteIntentsParams{
		OlderThan:  pgtype.Timestamptz{Time: olderThan, Valid: true},
		QueryLimit: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("store: list pending forge write intents: %w", err)
	}
	intents := make([]WriteIntent, 0, len(rows))
	for _, row := range rows {
		intents = append(intents, writeIntentFromRow(row))
	}
	return intents, nil
}

// AuthoredArtifactByCoordinateFold folds GitHub repo case and returns the oldest matching ownership row.
func (s *Store) AuthoredArtifactByCoordinateFold(ctx context.Context, provider ForgeProvider, host, repo string, kind ForgeArtifactKind, number uint64) (AuthoredArtifact, error) {
	if err := validCoordinate(provider, host, repo); err != nil {
		return AuthoredArtifact{}, err
	}
	if kind == ForgeArtifactKindUnspecified {
		return AuthoredArtifact{}, fmt.Errorf("%w: artifact kind is required", ErrInvalidArgument)
	}
	if number > math.MaxInt64 {
		return AuthoredArtifact{}, fmt.Errorf("%w: artifact number is out of range", ErrInvalidArgument)
	}
	repo = (ForgeCoord{Provider: provider, Repo: repo}).Normalized().Repo
	row, err := s.q.AuthoredArtifactByCoordinateFold(ctx, db.AuthoredArtifactByCoordinateFoldParams{
		ForgeProvider: int16(provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum.
		ForgeHost:     host,
		Repo:          repo,
		Kind:          int16(kind),   //nolint:gosec // G115: ForgeArtifactKind is a CHECK-constrained 1/2 enum.
		Number:        int64(number), // guarded to the signed BIGINT domain above.
	})
	if err != nil {
		if noRows(err) {
			return AuthoredArtifact{}, fmt.Errorf("%w: authored artifact at coordinate %d/%s/%s kind %d number %d", ErrNotFound, provider, host, repo, kind, number)
		}
		return AuthoredArtifact{}, fmt.Errorf("store: read authored artifact by folded coordinate: %w", err)
	}
	return authoredArtifactFromRow(row), nil
}

func writeKindFromRow(kind int16) forge.WriteKind {
	switch kind {
	case int16(forge.WriteKindIssue):
		return forge.WriteKindIssue
	case int16(forge.WriteKindPullRequest):
		return forge.WriteKindPullRequest
	case int16(forge.WriteKindComment):
		return forge.WriteKindComment
	case int16(forge.WriteKindReview):
		return forge.WriteKindReview
	default:
		return 0
	}
}

func writeIntentFromRow(row db.ForgeWriteIntent) WriteIntent {
	return WriteIntent{
		ID:             row.ID,
		Provider:       ForgeProvider(row.ForgeProvider),
		Host:           row.ForgeHost,
		Repo:           row.Repo,
		Kind:           writeKindFromRow(row.Kind),
		Parent:         uint64(row.ParentNumber), //nolint:gosec // G115: parent numbers are stored from canonical non-negative BIGINT values.
		HeadRef:        row.HeadRef,
		AgentAccountID: AccountID(row.AgentAccountID),
		OwnerUserID:    AccountID(row.OwnerUserID),
		SessionID:      row.SessionID,
		RequestKey:     row.ClientRequestID,
		State:          WriteState(row.State),
	}
}
