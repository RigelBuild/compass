package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
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
	// WriteStateHeld reserves a request key for one target; nothing was sent yet.
	WriteStateHeld    WriteState = "held"
	WriteStatePending WriteState = "pending"
	WriteStateDone    WriteState = "done"
	WriteStateFailed  WriteState = "failed"
	WriteStateLost    WriteState = "lost"
)

func nullableText(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}

func nullableForgeProvider(coord *ForgeCoord) pgtype.Int2 {
	if coord == nil {
		return pgtype.Int2{}
	}
	return pgtype.Int2{Int16: int16(coord.Provider), Valid: true} //nolint:gosec // G115: normalizedWriteIntent restricts providers to the stored enum.
}

func nullableIssueLinkHost(coord *ForgeCoord) pgtype.Text {
	if coord == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: coord.Host, Valid: true}
}

func nullableIssueLinkRepo(coord *ForgeCoord) pgtype.Text {
	if coord == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: coord.Repo, Valid: true}
}

func nullableIssueLinkNumber(coord *ForgeCoord) pgtype.Int8 {
	if coord == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: int64(coord.Number), Valid: true} //nolint:gosec // G115: validated coordinate numbers fit BIGINT.
}

// WriteIntent records one forge create attempt and the agent that owns its result.
type WriteIntent struct {
	// ID is the attempt id; a caller may set it so it can tell its own new attempt from a replay.
	ID             string
	Provider       ForgeProvider
	Host           string
	Repo           string
	Kind           forge.WriteKind
	Parent         uint64
	HeadRef        string
	BaseRef        string
	BaseIsDefault  bool
	HeadPushed     bool
	IssueLink      *ForgeCoord
	AgentAccountID AccountID
	OwnerUserID    AccountID
	SessionID      string
	RequestKey     string
	State          WriteState
	// ForgeKey, CreatedAt, and TenantID are read back; RecordWriteIntent ignores them on input.
	ForgeKey  string
	ForgeURL  string
	CreatedAt time.Time
	TenantID  TenantID
}

// RecordWriteIntent commits an idempotency record before any forge create.
// It returns an existing non-failed attempt for the same agent and request key.
func (s *Store) RecordWriteIntent(ctx context.Context, in WriteIntent) (WriteIntent, error) {
	in, err := normalizedWriteIntent(in)
	if err != nil {
		return WriteIntent{}, err
	}
	row, err := s.q.RecordForgeWriteIntent(ctx, db.RecordForgeWriteIntentParams{
		ID:                     in.ID,
		ForgeProvider:          int16(in.Provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum.
		ForgeHost:              in.Host,
		Repo:                   in.Repo,
		Kind:                   int16(in.Kind),
		ParentNumber:           int64(in.Parent), //nolint:gosec // G115: normalizedWriteIntent bounds Parent to MaxInt64.
		HeadRef:                in.HeadRef,
		BaseRef:                nullableText(in.BaseRef),
		BaseIsDefault:          in.BaseIsDefault,
		IssueLinkForgeProvider: nullableForgeProvider(in.IssueLink),
		IssueLinkForgeHost:     nullableIssueLinkHost(in.IssueLink),
		IssueLinkRepo:          nullableIssueLinkRepo(in.IssueLink),
		IssueLinkNumber:        nullableIssueLinkNumber(in.IssueLink),
		AgentAccountID:         string(in.AgentAccountID),
		OwnerUserID:            string(in.OwnerUserID),
		SessionID:              in.SessionID,
		ClientRequestID:        in.RequestKey,
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
	return writeIntentFromRow(db.ForgeWriteIntent{
		ID: row.ID, ForgeProvider: row.ForgeProvider, ForgeHost: row.ForgeHost, Repo: row.Repo,
		Kind: row.Kind, ParentNumber: row.ParentNumber, HeadRef: row.HeadRef, BaseRef: row.BaseRef,
		BaseIsDefault: row.BaseIsDefault, HeadPushed: row.HeadPushed,
		AgentAccountID: row.AgentAccountID, OwnerUserID: row.OwnerUserID, SessionID: row.SessionID,
		ClientRequestID: row.ClientRequestID, IssueLinkForgeProvider: row.IssueLinkForgeProvider,
		IssueLinkForgeHost: row.IssueLinkForgeHost, IssueLinkRepo: row.IssueLinkRepo, IssueLinkNumber: row.IssueLinkNumber,
		ForgeKey: row.ForgeKey, ForgeUrl: row.ForgeUrl, State: row.State, CreatedAt: row.CreatedAt, TenantID: row.TenantID,
	}), nil
}

// HoldWriteIntent reserves the agent's request key for in's target without
// claiming a PR head. It returns the existing non-failed attempt for the key if
// there is one, so two calls racing on a key agree on one target.
func (s *Store) HoldWriteIntent(ctx context.Context, in WriteIntent) (WriteIntent, error) {
	in, err := normalizedWriteIntent(in)
	if err != nil {
		return WriteIntent{}, err
	}
	row, err := s.q.HoldForgeWriteIntent(ctx, db.HoldForgeWriteIntentParams{
		ID: in.ID, ForgeProvider: int16(in.Provider), ForgeHost: in.Host, //nolint:gosec // G115: normalizedWriteIntent restricts providers to the stored enum.
		Repo: in.Repo, Kind: int16(in.Kind), ParentNumber: int64(in.Parent), HeadRef: in.HeadRef, //nolint:gosec // G115: normalizedWriteIntent bounds Parent to MaxInt64.
		BaseRef: nullableText(in.BaseRef), BaseIsDefault: in.BaseIsDefault, AgentAccountID: string(in.AgentAccountID),
		OwnerUserID: string(in.OwnerUserID), SessionID: in.SessionID, ClientRequestID: in.RequestKey,
	})
	if err != nil {
		if pgErrIs(err, pgForeignKeyViolation) {
			return WriteIntent{}, fmt.Errorf("%w: unknown agent or owner account", ErrInvalidArgument)
		}
		return WriteIntent{}, fmt.Errorf("store: hold forge write intent: %w", err)
	}
	return writeIntentFromRow(db.ForgeWriteIntent{
		ID: row.ID, ForgeProvider: row.ForgeProvider, ForgeHost: row.ForgeHost, Repo: row.Repo,
		Kind: row.Kind, ParentNumber: row.ParentNumber, HeadRef: row.HeadRef, BaseRef: row.BaseRef,
		BaseIsDefault: row.BaseIsDefault, HeadPushed: row.HeadPushed,
		AgentAccountID: row.AgentAccountID, OwnerUserID: row.OwnerUserID, SessionID: row.SessionID,
		ClientRequestID: row.ClientRequestID, IssueLinkForgeProvider: row.IssueLinkForgeProvider,
		IssueLinkForgeHost: row.IssueLinkForgeHost, IssueLinkRepo: row.IssueLinkRepo, IssueLinkNumber: row.IssueLinkNumber,
		ForgeKey: row.ForgeKey, ForgeUrl: row.ForgeUrl, State: row.State, CreatedAt: row.CreatedAt, TenantID: row.TenantID,
	}), nil
}

// PromoteWriteIntent turns a held attempt pending just before its create is
// sent. ErrConflict means it is no longer held, or another attempt holds the PR head.
func (s *Store) PromoteWriteIntent(ctx context.Context, in WriteIntent) error {
	if in.ID == "" {
		return fmt.Errorf("%w: write intent id is required", ErrInvalidArgument)
	}
	if in.BaseRef != "" && in.Kind != forge.WriteKindPullRequest {
		return fmt.Errorf("%w: base ref is only valid for pull requests", ErrInvalidArgument)
	}
	if in.IssueLink != nil && in.Kind != forge.WriteKindPullRequest {
		return fmt.Errorf("%w: issue link is only valid for pull requests", ErrInvalidArgument)
	}
	var link *ForgeCoord
	if in.IssueLink != nil {
		normalized := in.IssueLink.Normalized()
		if normalized.Provider < ForgeProviderGitHub || normalized.Provider > ForgeProviderLinear {
			return fmt.Errorf("%w: invalid pull request issue link provider", ErrInvalidArgument)
		}
		if err := normalized.valid(); err != nil || normalized.Number > math.MaxInt64 {
			return fmt.Errorf("%w: invalid pull request issue link", ErrInvalidArgument)
		}
		link = &normalized
	}
	affected, err := s.q.PromoteForgeWriteIntent(ctx, db.PromoteForgeWriteIntentParams{
		ID:                     in.ID,
		BaseRef:                nullableText(in.BaseRef),
		BaseIsDefault:          in.BaseIsDefault,
		IssueLinkForgeProvider: nullableForgeProvider(link),
		IssueLinkForgeHost:     nullableIssueLinkHost(link),
		IssueLinkRepo:          nullableIssueLinkRepo(link),
		IssueLinkNumber:        nullableIssueLinkNumber(link),
	})
	if err != nil {
		if pgErrIs(err, pgUniqueViolation) {
			return fmt.Errorf("%w: pull request head is already claimed", ErrConflict)
		}
		return fmt.Errorf("store: promote forge write intent: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("%w: write intent %q is not held", ErrConflict, in.ID)
	}
	return nil
}
func normalizedWriteIntent(in WriteIntent) (WriteIntent, error) {
	if in.Provider < ForgeProviderGitHub || in.Provider > ForgeProviderLinear {
		return WriteIntent{}, fmt.Errorf("%w: unsupported forge provider %d", ErrInvalidArgument, in.Provider)
	}
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
	if in.Kind != forge.WriteKindPullRequest && (in.HeadRef != "" || in.BaseRef != "" || in.IssueLink != nil) {
		return WriteIntent{}, fmt.Errorf("%w: PR metadata is only valid for pull requests", ErrInvalidArgument)
	}
	if in.IssueLink != nil {
		normalized := in.IssueLink.Normalized()
		if normalized.Provider < ForgeProviderGitHub || normalized.Provider > ForgeProviderLinear {
			return WriteIntent{}, fmt.Errorf("%w: invalid pull request issue link provider", ErrInvalidArgument)
		}
		if err := normalized.valid(); err != nil || normalized.Number > math.MaxInt64 {
			return WriteIntent{}, fmt.Errorf("%w: invalid pull request issue link", ErrInvalidArgument)
		}
		in.IssueLink = &normalized
	}
	if in.ID == "" {
		in.ID = uuid.NewString()
	} else if _, err := uuid.Parse(in.ID); err != nil {
		return WriteIntent{}, fmt.Errorf("%w: write intent id must be a UUID", ErrInvalidArgument)
	}
	in.Repo = (ForgeCoord{Provider: in.Provider, Repo: in.Repo}).Normalized().Repo
	return in, nil
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
	return writeIntentFromRow(db.ForgeWriteIntent{
		ID: row.ID, ForgeProvider: row.ForgeProvider, ForgeHost: row.ForgeHost, Repo: row.Repo,
		Kind: row.Kind, ParentNumber: row.ParentNumber, HeadRef: row.HeadRef, BaseRef: row.BaseRef,
		BaseIsDefault: row.BaseIsDefault, HeadPushed: row.HeadPushed,
		AgentAccountID: row.AgentAccountID, OwnerUserID: row.OwnerUserID, SessionID: row.SessionID,
		ClientRequestID: row.ClientRequestID, IssueLinkForgeProvider: row.IssueLinkForgeProvider,
		IssueLinkForgeHost: row.IssueLinkForgeHost, IssueLinkRepo: row.IssueLinkRepo, IssueLinkNumber: row.IssueLinkNumber,
		ForgeKey: row.ForgeKey, ForgeUrl: row.ForgeUrl, State: row.State, CreatedAt: row.CreatedAt, TenantID: row.TenantID,
	}), nil
}

// HasPushedPRHead reports whether this agent previously pushed the PR head.
func (s *Store) HasPushedPRHead(ctx context.Context, provider ForgeProvider, host, repo, headRef string, agent AccountID) (bool, error) {
	if err := validCoordinate(provider, host, repo); err != nil {
		return false, err
	}
	if headRef == "" || agent == "" {
		return false, fmt.Errorf("%w: pull request head ref and agent are required", ErrInvalidArgument)
	}
	repo = (ForgeCoord{Provider: provider, Repo: repo}).Normalized().Repo
	hasPushed, err := s.q.HasPushedPRHead(ctx, db.HasPushedPRHeadParams{
		ForgeProvider:  int16(provider), //nolint:gosec // G115: ForgeProvider is a CHECK-constrained 1..4 enum.
		ForgeHost:      host,
		Repo:           repo,
		HeadRef:        headRef,
		AgentAccountID: string(agent),
	})
	if err != nil {
		return false, fmt.Errorf("store: check pushed PR head: %w", err)
	}
	return hasPushed, nil
}

// MarkWriteIntentHeadPushed records that an intent's stack bundle reached the forge.
func (s *Store) MarkWriteIntentHeadPushed(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("%w: write intent id is required", ErrInvalidArgument)
	}
	affected, err := s.q.MarkWriteIntentHeadPushed(ctx, id)
	if err != nil {
		return fmt.Errorf("store: mark write intent head pushed: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: write intent %q", ErrNotFound, id)
	}
	return nil
}

// CompleteWriteIntent records artifact ownership or a comment/review forge key
// and marks the pending attempt done in one transaction.
// It also stores the forge URL for same-key replay.
// CompleteWriteIntent writes the ownership row (issue, PR) or keeps the forge
// key (comment, review) and marks the pending attempt done.
func (s *Store) CompleteWriteIntent(ctx context.Context, id string, artifact *AuthoredArtifact, forgeKey, forgeURL string) (retErr error) {
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
		// The number lets a same-key replay return the object with no forge call.
		forgeKey = strconv.FormatUint(artifact.Number, 10)
	} else if artifact != nil || forgeKey == "" {
		return fmt.Errorf("%w: comment and review intents require a forge key only", ErrInvalidArgument)
	}

	affected, err := qtx.CompleteForgeWriteIntent(ctx, db.CompleteForgeWriteIntentParams{
		ID: id, ForgeKey: forgeKey, ForgeUrl: forgeURL,
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

// CompletePullRequestWriteIntent atomically persists the PR row and issue link
// alongside ownership and intent completion.
func (s *Store) CompletePullRequestWriteIntent(ctx context.Context, id string, artifact AuthoredArtifact, pr PullRequestRow, forgeURL string) (retErr error) {
	if id == "" {
		return fmt.Errorf("%w: write intent id is required", ErrInvalidArgument)
	}
	pr, err := pr.normalized()
	if err != nil {
		return err
	}
	if err := artifact.valid(); err != nil {
		return err
	}
	coord := ForgeCoord{Provider: artifact.Provider, Host: artifact.Host, Repo: artifact.Repo, Number: artifact.Number}.Normalized()
	if artifact.Kind != ForgeArtifactKindPullRequest || coord != pr.Coord || artifact.Number > math.MaxInt64 {
		return fmt.Errorf("%w: authored artifact does not name the pull request", ErrInvalidArgument)
	}
	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return fmt.Errorf("store: begin complete pull request intent: %w", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			retErr = errors.Join(retErr, fmt.Errorf("store: roll back complete pull request intent: %w", err))
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
	if WriteState(row.State) != WriteStatePending || writeKindFromRow(row.Kind) != forge.WriteKindPullRequest {
		return fmt.Errorf("%w: write intent %q is not a pending pull request", ErrConflict, id)
	}
	if artifact.Provider != ForgeProvider(row.ForgeProvider) || artifact.Host != row.ForgeHost ||
		artifact.AgentAccountID != AccountID(row.AgentAccountID) || artifact.OwnerUserID != AccountID(row.OwnerUserID) ||
		artifact.SessionID != row.SessionID || coord.Repo != (ForgeCoord{Provider: ForgeProvider(row.ForgeProvider), Repo: row.Repo}).Normalized().Repo {
		return fmt.Errorf("%w: authored artifact does not match write intent %q", ErrInvalidArgument, id)
	}
	issue := row.IssueLinkForgeProvider.Valid
	if issue {
		if row.IssueLinkNumber.Int64 <= 0 {
			return errors.New("store: invalid pull request issue link number in intent")
		}
		normalized := ForgeCoord{
			Provider: ForgeProvider(row.IssueLinkForgeProvider.Int16),
			Host:     row.IssueLinkForgeHost.String, Repo: row.IssueLinkRepo.String,
			Number: uint64(row.IssueLinkNumber.Int64),
		}.Normalized()
		if err := normalized.valid(); err != nil {
			return fmt.Errorf("store: read pull request issue link from intent: %w", err)
		}
	}
	if artifact.Number == 0 || artifact.Number > math.MaxInt64 {
		return fmt.Errorf("%w: pull request number is outside PostgreSQL bigint", ErrInvalidArgument)
	}
	prCoord := pr.Coord.db()
	prNumber := int64(artifact.Number)
	affected, err := qtx.CompletePullRequestWriteIntent(ctx, db.CompletePullRequestWriteIntentParams{
		ForgeUrl: forgeURL, ID: id, Number: prNumber, CreatedAtUnixMs: artifact.CreatedAtUnixMS,
		PrForgeProvider: prCoord.provider, PrForgeHost: prCoord.host, PrRepo: prCoord.repo,
		ForgeState: pr.State, ForgeCreatedAt: timestamptz(pr.CreatedAt),
		ForgeUpdatedAt: timestamptz(pr.UpdatedAt), Pr: pr.PR,
	})
	if err != nil {
		return fmt.Errorf("store: complete pull request intent: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("%w: write intent %q is no longer pending", ErrConflict, id)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit complete pull request intent: %w", err)
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

// DeleteExpiredHeldWriteIntents removes up to limit held keys older than
// olderThan, oldest first; past that age a same-key retry is outside the idempotency window.
func (s *Store) DeleteExpiredHeldWriteIntents(ctx context.Context, olderThan time.Time, limit int) (int64, error) {
	if limit < 1 || int64(limit) > math.MaxInt32 {
		return 0, fmt.Errorf("%w: held write intent limit must be between 1 and %d", ErrInvalidArgument, math.MaxInt32)
	}
	n, err := s.q.DeleteExpiredHeldWriteIntents(ctx, db.DeleteExpiredHeldWriteIntentsParams{
		OlderThan:  pgtype.Timestamptz{Time: olderThan, Valid: true},
		QueryLimit: int32(limit),
	})
	if err != nil {
		return 0, fmt.Errorf("store: delete expired held forge write intents: %w", err)
	}
	return n, nil
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
		intents = append(intents, writeIntentFromRow(db.ForgeWriteIntent{
			ID: row.ID, ForgeProvider: row.ForgeProvider, ForgeHost: row.ForgeHost, Repo: row.Repo,
			Kind: row.Kind, ParentNumber: row.ParentNumber, HeadRef: row.HeadRef, BaseRef: row.BaseRef,
			BaseIsDefault: row.BaseIsDefault, HeadPushed: row.HeadPushed,
			AgentAccountID: row.AgentAccountID, OwnerUserID: row.OwnerUserID, SessionID: row.SessionID,
			ClientRequestID: row.ClientRequestID, IssueLinkForgeProvider: row.IssueLinkForgeProvider,
			IssueLinkForgeHost: row.IssueLinkForgeHost, IssueLinkRepo: row.IssueLinkRepo, IssueLinkNumber: row.IssueLinkNumber,
			ForgeKey: row.ForgeKey, ForgeUrl: row.ForgeUrl, State: row.State, CreatedAt: row.CreatedAt, TenantID: row.TenantID,
		}))
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
		ID: row.ID, Provider: ForgeProvider(row.ForgeProvider), Host: row.ForgeHost,
		Repo: row.Repo, Kind: writeKindFromRow(row.Kind), Parent: uint64(row.ParentNumber), //nolint:gosec // G115: parent is nonnegative and bounded before insert.
		HeadRef: row.HeadRef, BaseRef: row.BaseRef.String, BaseIsDefault: row.BaseIsDefault,
		IssueLink:      forgeCoordFromNullable(row.IssueLinkForgeProvider, row.IssueLinkForgeHost, row.IssueLinkRepo, row.IssueLinkNumber),
		AgentAccountID: AccountID(row.AgentAccountID), OwnerUserID: AccountID(row.OwnerUserID),
		SessionID: row.SessionID, RequestKey: row.ClientRequestID,
		State: WriteState(row.State), ForgeKey: row.ForgeKey, ForgeURL: row.ForgeUrl,
		CreatedAt: row.CreatedAt.Time, TenantID: TenantID(row.TenantID), HeadPushed: row.HeadPushed,
	}
}

func forgeCoordFromNullable(provider pgtype.Int2, host, repo pgtype.Text, number pgtype.Int8) *ForgeCoord {
	if !provider.Valid || !host.Valid || !repo.Valid || !number.Valid || number.Int64 <= 0 {
		return nil
	}
	return &ForgeCoord{Provider: ForgeProvider(provider.Int16), Host: host.String, Repo: repo.String, Number: uint64(number.Int64)}
}
