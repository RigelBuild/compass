package store

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// ForgeMirrorMode is the user-selected ask mirroring policy for one forge repo.
type ForgeMirrorMode int16

const (
	ForgeMirrorOff ForgeMirrorMode = iota
	ForgeMirrorManual
	ForgeMirrorAuto
)

// ForgeMirrorSetting describes a coordinate's configured mirroring mode.
type ForgeMirrorSetting struct {
	Provider ForgeProvider
	Host     string
	Repo     string
	Mode     ForgeMirrorMode
}

// ForgeMirrorRow is one durable mirror outbox entry.
type ForgeMirrorRow struct {
	ID              string
	Source          int16
	AskID           string
	SourceKey       string
	Actor           AccountID
	Agent           AccountID
	ClientRequestID string
	TargetRef       string
	BodyRev         int16
	PostedRev       int16
	State           int16
	Attempts        int
	CommentID       uint64
	CommentKey      string
	CommentURL      string
	LastError       string
	// LastErrorCode is the Connect code of a failed row's error, so a retry
	// reports the same status the failing drive did.
	LastErrorCode   string
	FirstAttemptAt  *time.Time
	LastAttemptAt   *time.Time
	ClientCommentID string
	RecheckAt       *time.Time
	LeaseUntil      *time.Time
	LeaseToken      string
	UpdatedAt       time.Time
	// SessionID is the agent session the owner header names; empty on user rows.
	SessionID string
	// SourcePayload holds an agent message mirror's render inputs, so a redrive
	// after restart can rebuild the body; nil on ask and user rows.
	SourcePayload []byte
}

// ForgeMirrorMark is the result of one mirror delivery or reconciliation pass.
type ForgeMirrorMark struct {
	PostedRev    int16
	State        int16
	CommentID    uint64
	CommentKey   string
	URL          string
	Err          string
	ErrCode      string
	Recheck      bool
	ClearRecheck bool
}

// ForgeMirrorMode returns the coordinate's configured mode, defaulting to Off.
func (s *Store) ForgeMirrorMode(ctx context.Context, provider ForgeProvider, host, repo string) (ForgeMirrorMode, error) {
	coord := (ForgeCoord{Provider: provider, Host: host, Repo: repo}).Normalized()
	if err := validCoordinate(coord.Provider, coord.Host, coord.Repo); err != nil {
		return ForgeMirrorOff, err
	}
	mode, err := s.q.ForgeMirrorMode(ctx, db.ForgeMirrorModeParams{
		ForgeProvider: int16(coord.Provider), //nolint:gosec // G115: provider is a CHECK-constrained 1..4 enum.
		ForgeHost:     coord.Host,
		Repo:          coord.Repo,
	})
	if err != nil {
		return ForgeMirrorOff, fmt.Errorf("store: read forge mirror mode: %w", err)
	}
	return ForgeMirrorMode(mode), nil
}

// ListForgeMirrorSettings lists each enabled subscription and its current mode.
func (s *Store) ListForgeMirrorSettings(ctx context.Context) ([]ForgeMirrorSetting, error) {
	rows, err := s.q.ListForgeMirrorSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: list forge mirror settings: %w", err)
	}
	out := make([]ForgeMirrorSetting, 0, len(rows))
	for _, row := range rows {
		out = append(out, ForgeMirrorSetting{
			Provider: ForgeProvider(row.ForgeProvider),
			Host:     row.ForgeHost,
			Repo:     row.Repo,
			Mode:     ForgeMirrorMode(row.Mode),
		})
	}
	return out, nil
}

// SetForgeMirrorSetting removes an Off setting or upserts Manual and Auto.
func (s *Store) SetForgeMirrorSetting(ctx context.Context, actor AccountID, set ForgeMirrorSetting) error {
	coord := (ForgeCoord{Provider: set.Provider, Host: set.Host, Repo: set.Repo}).Normalized()
	if err := validCoordinate(coord.Provider, coord.Host, coord.Repo); err != nil {
		return err
	}
	if set.Mode < ForgeMirrorOff || set.Mode > ForgeMirrorAuto {
		return fmt.Errorf("%w: forge mirror mode %d", ErrInvalidArgument, set.Mode)
	}
	arg := db.DeleteForgeMirrorSettingParams{
		ForgeProvider: int16(coord.Provider), //nolint:gosec // G115: provider is a CHECK-constrained 1..4 enum.
		ForgeHost:     coord.Host,
		Repo:          coord.Repo,
	}
	if set.Mode == ForgeMirrorOff {
		if err := s.q.DeleteForgeMirrorSetting(ctx, arg); err != nil {
			return fmt.Errorf("store: delete forge mirror setting: %w", err)
		}
		return nil
	}
	if actor == "" {
		return fmt.Errorf("%w: forge mirror setting actor is required", ErrInvalidArgument)
	}
	if err := s.q.UpsertForgeMirrorSetting(ctx, db.UpsertForgeMirrorSettingParams{
		ForgeProvider: arg.ForgeProvider,
		ForgeHost:     arg.ForgeHost,
		Repo:          arg.Repo,
		Mode:          int16(set.Mode),
		UpdatedBy:     string(actor),
	}); err != nil {
		return fmt.Errorf("store: set forge mirror setting: %w", err)
	}
	return nil
}

// ClaimForgeMirror inserts a row or returns the row at its conflicting key.
// An agent request key is reserved in forge_mirror_requests in the same
// transaction as the row, so a claim and a re-mirror racing on one key resolve
// to the one reserved row.
func (s *Store) ClaimForgeMirror(ctx context.Context, mirror ForgeMirrorRow) (ForgeMirrorRow, bool, error) {
	if (mirror.Source != 1 && mirror.Source != 2) || mirror.Actor == "" || mirror.TargetRef == "" || mirror.BodyRev < 1 || mirror.State < 1 || mirror.State > 3 {
		return ForgeMirrorRow{}, false, fmt.Errorf("%w: invalid forge mirror row", ErrInvalidArgument)
	}
	if (mirror.Source == 1 && (mirror.AskID == "" || mirror.Agent == "")) || (mirror.Source == 2 && mirror.SourceKey == "") {
		return ForgeMirrorRow{}, false, fmt.Errorf("%w: forge mirror idempotency key is required", ErrInvalidArgument)
	}
	keyed := mirror.Agent != "" && mirror.ClientRequestID != ""
	row, inserted, err := s.insertForgeMirror(ctx, mirror, keyed)
	if err != nil {
		return ForgeMirrorRow{}, false, err
	}
	if inserted {
		return row, true, nil
	}
	if keyed {
		reserved, found, err := s.ForgeMirrorByRequest(ctx, mirror.Agent, mirror.ClientRequestID)
		if err != nil {
			return ForgeMirrorRow{}, false, err
		}
		if found {
			return reserved, false, nil
		}
	}
	conflict, err := s.forgeMirrorByConflictKeys(ctx, mirror)
	if err != nil {
		return ForgeMirrorRow{}, false, fmt.Errorf("store: find claimed forge mirror: %w", err)
	}
	if conflict.ConflictKind == 3 && mirror.Source == 2 && conflict.ActorAccountID == string(mirror.Actor) && conflict.AgentAccountID == string(mirror.Agent) {
		reservedID, err := s.remirrorSource(ctx, conflict.ID, mirror)
		if err != nil {
			return ForgeMirrorRow{}, false, err
		}
		if reservedID != conflict.ID {
			// Another call reserved this key for a different row first.
			reserved, err := s.ForgeMirrorByID(ctx, reservedID)
			return reserved, false, err
		}
		conflict, err = s.forgeMirrorByConflictKeys(ctx, mirror)
		if err != nil {
			return ForgeMirrorRow{}, false, fmt.Errorf("store: reload claimed forge mirror: %w", err)
		}
	}
	return forgeMirrorFromDB(db.ClaimForgeMirrorRow{
		ID: conflict.ID, Source: conflict.Source, AskID: conflict.AskID, SourceKey: conflict.SourceKey,
		ActorAccountID: conflict.ActorAccountID, AgentAccountID: conflict.AgentAccountID,
		ClientRequestID: conflict.ClientRequestID, TargetRef: conflict.TargetRef,
		BodyRev: conflict.BodyRev, PostedRev: conflict.PostedRev, State: conflict.State, Attempts: conflict.Attempts,
		CommentID: conflict.CommentID, CommentKey: conflict.CommentKey, CommentUrl: conflict.CommentUrl, LastError: conflict.LastError,
		FirstAttemptAt: conflict.FirstAttemptAt, LastAttemptAt: conflict.LastAttemptAt,
		ClientCommentID: conflict.ClientCommentID, RecheckAt: conflict.RecheckAt, LeaseUntil: conflict.LeaseUntil,
		LeaseToken: conflict.LeaseToken, UpdatedAt: conflict.UpdatedAt,
		SessionID: conflict.SessionID, SourcePayload: conflict.SourcePayload, LastErrorCode: conflict.LastErrorCode,
	}), false, nil
}

// insertForgeMirror inserts a new row. A keyed agent row also reserves its
// request key in the same transaction; when the key is already reserved the
// row is rolled back and inserted is false.
func (s *Store) insertForgeMirror(ctx context.Context, mirror ForgeMirrorRow, keyed bool) (ForgeMirrorRow, bool, error) {
	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return ForgeMirrorRow{}, false, fmt.Errorf("store: begin claim forge mirror: %w", err)
	}
	// After Commit this rollback is a no-op; a failure before Commit is returned below.
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)
	row, err := qtx.ClaimForgeMirror(ctx, db.ClaimForgeMirrorParams{
		ID:              newID(),
		Source:          mirror.Source,
		AskID:           mirror.AskID,
		SourceKey:       mirror.SourceKey,
		ActorAccountID:  string(mirror.Actor),
		AgentAccountID:  nullableString(string(mirror.Agent)),
		ClientRequestID: nullableString(mirror.ClientRequestID),
		TargetRef:       mirror.TargetRef,
		BodyRev:         mirror.BodyRev,
		State:           mirror.State,
		SessionID:       mirror.SessionID,
		SourcePayload:   mirror.SourcePayload,
	})
	if noRows(err) {
		return ForgeMirrorRow{}, false, nil
	}
	if err != nil {
		return ForgeMirrorRow{}, false, fmt.Errorf("store: claim forge mirror: %w", err)
	}
	if keyed {
		recorded, err := qtx.RecordForgeMirrorRequest(ctx, db.RecordForgeMirrorRequestParams{
			AgentAccountID: string(mirror.Agent), ClientRequestID: mirror.ClientRequestID, MirrorID: row.ID,
		})
		if err != nil {
			return ForgeMirrorRow{}, false, fmt.Errorf("store: reserve forge mirror request: %w", err)
		}
		if recorded == 0 {
			return ForgeMirrorRow{}, false, nil
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ForgeMirrorRow{}, false, fmt.Errorf("store: commit claim forge mirror: %w", err)
	}
	return forgeMirrorFromDB(row), true, nil
}

// remirrorSource bumps a source row for a later agent request and returns the
// row id the request key resolves to. Only a first-seen key bumps, so a retry
// of the re-mirror call never renders or edits again; a key another call
// already reserved resolves to that call's row.
func (s *Store) remirrorSource(ctx context.Context, id string, mirror ForgeMirrorRow) (string, error) {
	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return "", fmt.Errorf("store: begin re-mirror: %w", err)
	}
	// After Commit this rollback is a no-op; a failure before Commit is returned below.
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)
	if mirror.Agent != "" && mirror.ClientRequestID != "" {
		recorded, err := qtx.RecordForgeMirrorRequest(ctx, db.RecordForgeMirrorRequestParams{
			AgentAccountID: string(mirror.Agent), ClientRequestID: mirror.ClientRequestID, MirrorID: id,
		})
		if err != nil {
			return "", fmt.Errorf("store: record forge mirror request: %w", err)
		}
		if recorded == 0 {
			reserved, err := qtx.ForgeMirrorByRequest(ctx, db.ForgeMirrorByRequestParams{AgentAccountID: string(mirror.Agent), ClientRequestID: mirror.ClientRequestID})
			if err != nil {
				return "", fmt.Errorf("store: read reserved forge mirror request: %w", err)
			}
			return reserved.ID, nil
		}
	}
	// A re-mirror of the same source carries its new render inputs and session.
	if _, err := qtx.BumpForgeMirrorSourceRevision(ctx, db.BumpForgeMirrorSourceRevisionParams{
		SourcePayload: mirror.SourcePayload, SessionID: mirror.SessionID, ID: id,
	}); err != nil {
		return "", fmt.Errorf("store: bump forge mirror revision: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("store: commit re-mirror: %w", err)
	}
	return id, nil
}

// ForgeMirrorByRequest finds the row an agent request key reserved, by claim
// or by re-mirror; ok is false when the key is unused.
func (s *Store) ForgeMirrorByRequest(ctx context.Context, agent AccountID, clientRequestID string) (ForgeMirrorRow, bool, error) {
	if agent == "" || clientRequestID == "" {
		return ForgeMirrorRow{}, false, fmt.Errorf("%w: forge mirror agent and request key are required", ErrInvalidArgument)
	}
	row, err := s.q.ForgeMirrorByRequest(ctx, db.ForgeMirrorByRequestParams{AgentAccountID: string(agent), ClientRequestID: clientRequestID})
	if noRows(err) {
		return ForgeMirrorRow{}, false, nil
	}
	if err != nil {
		return ForgeMirrorRow{}, false, fmt.Errorf("store: read forge mirror by request: %w", err)
	}
	return forgeMirrorFromDB(db.ClaimForgeMirrorRow(row)), true, nil
}

func (s *Store) forgeMirrorByConflictKeys(ctx context.Context, mirror ForgeMirrorRow) (db.ForgeMirrorByConflictKeysRow, error) {
	return s.q.ForgeMirrorByConflictKeys(ctx, db.ForgeMirrorByConflictKeysParams{
		AskID: mirror.AskID, AgentAccountID: string(mirror.Agent),
		ClientRequestID: nullableString(mirror.ClientRequestID), SourceKey: mirror.SourceKey, TargetRef: mirror.TargetRef,
	})
}

// ForgeMirrorByID reads one mirror row under the current tenant scope.
func (s *Store) ForgeMirrorByID(ctx context.Context, id string) (ForgeMirrorRow, error) {
	if id == "" {
		return ForgeMirrorRow{}, fmt.Errorf("%w: forge mirror id is required", ErrInvalidArgument)
	}
	row, err := s.q.ForgeMirrorByID(ctx, id)
	if noRows(err) {
		return ForgeMirrorRow{}, fmt.Errorf("%w: forge mirror %q", ErrNotFound, id)
	}
	if err != nil {
		return ForgeMirrorRow{}, fmt.Errorf("store: read forge mirror: %w", err)
	}
	return forgeMirrorFromDB(db.ClaimForgeMirrorRow(row)), nil
}

// OpenForgeMirrors returns pending and failed rows, and done rows due for recheck.
func (s *Store) OpenForgeMirrors(ctx context.Context, agent AccountID) ([]ForgeMirrorRow, error) {
	if agent == "" {
		return nil, fmt.Errorf("%w: forge mirror agent is required", ErrInvalidArgument)
	}
	rows, err := s.q.OpenForgeMirrors(ctx, string(agent))
	if err != nil {
		return nil, fmt.Errorf("store: list open forge mirrors: %w", err)
	}
	out := make([]ForgeMirrorRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, forgeMirrorFromDB(db.ClaimForgeMirrorRow(row)))
	}
	return out, nil
}

// AskForgeMirrors returns rows matching the supplied ask ids.
func (s *Store) AskForgeMirrors(ctx context.Context, askIDs []string) ([]ForgeMirrorRow, error) {
	if len(askIDs) == 0 {
		return nil, nil
	}
	rows, err := s.q.AskForgeMirrors(ctx, askIDs)
	if err != nil {
		return nil, fmt.Errorf("store: list ask forge mirrors: %w", err)
	}
	out := make([]ForgeMirrorRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, forgeMirrorFromDB(db.ClaimForgeMirrorRow(row)))
	}
	return out, nil
}

// LeaseForgeMirror claims one eligible row until ttl elapses.
func (s *Store) LeaseForgeMirror(ctx context.Context, id, token string, ttl time.Duration) (ForgeMirrorRow, bool, error) {
	if id == "" || token == "" || ttl <= 0 {
		return ForgeMirrorRow{}, false, fmt.Errorf("%w: forge mirror lease id, token, and ttl are required", ErrInvalidArgument)
	}
	row, err := s.q.LeaseForgeMirror(ctx, db.LeaseForgeMirrorParams{Ttl: pgtype.Interval{Microseconds: ttl.Microseconds(), Valid: true}, LeaseToken: token, ID: id})
	if noRows(err) {
		return ForgeMirrorRow{}, false, nil
	}
	if err != nil {
		return ForgeMirrorRow{}, false, fmt.Errorf("store: lease forge mirror: %w", err)
	}
	return forgeMirrorFromDB(db.ClaimForgeMirrorRow(row)), true, nil
}

// RenewForgeMirror extends a live lease held by token.
func (s *Store) RenewForgeMirror(ctx context.Context, id, token string, ttl time.Duration) (bool, error) {
	if id == "" || token == "" || ttl <= 0 {
		return false, fmt.Errorf("%w: forge mirror lease id, token, and ttl are required", ErrInvalidArgument)
	}
	n, err := s.q.RenewForgeMirror(ctx, db.RenewForgeMirrorParams{Ttl: pgtype.Interval{Microseconds: ttl.Microseconds(), Valid: true}, ID: id, LeaseToken: token})
	if err != nil {
		return false, fmt.Errorf("store: renew forge mirror lease: %w", err)
	}
	return n != 0, nil
}

// BeginForgeMirrorAttempt records one POST attempt under its current lease.
func (s *Store) BeginForgeMirrorAttempt(ctx context.Context, id, token string) (ForgeMirrorRow, bool, error) {
	if id == "" || token == "" {
		return ForgeMirrorRow{}, false, fmt.Errorf("%w: forge mirror attempt id and token are required", ErrInvalidArgument)
	}
	row, err := s.q.BeginForgeMirrorAttempt(ctx, db.BeginForgeMirrorAttemptParams{ID: id, LeaseToken: token})
	if noRows(err) {
		return ForgeMirrorRow{}, false, nil
	}
	if err != nil {
		return ForgeMirrorRow{}, false, fmt.Errorf("store: begin forge mirror attempt: %w", err)
	}
	return forgeMirrorFromDB(db.ClaimForgeMirrorRow(row)), true, nil
}

// MarkForgeMirror stores delivery state only for the current live lease holder.
func (s *Store) MarkForgeMirror(ctx context.Context, id, token string, mark ForgeMirrorMark) error {
	if id == "" || token == "" || mark.State < 1 || mark.State > 3 {
		return fmt.Errorf("%w: forge mirror id, token, and state are required", ErrInvalidArgument)
	}
	if mark.CommentID > math.MaxInt64 {
		return fmt.Errorf("%w: forge comment id exceeds BIGINT range", ErrInvalidArgument)
	}
	n, err := s.q.MarkForgeMirror(ctx, db.MarkForgeMirrorParams{
		ID: id, LeaseToken: token, PostedRev: mark.PostedRev, State: mark.State,
		CommentID: int64(mark.CommentID), CommentKey: mark.CommentKey, CommentUrl: mark.URL,
		LastError: mark.Err, LastErrorCode: mark.ErrCode, Recheck: mark.Recheck, ClearRecheck: mark.ClearRecheck,
	})
	if err != nil {
		return fmt.Errorf("store: mark forge mirror: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: forge mirror %q", ErrLeaseLost, id)
	}
	return nil
}

func insertAskMirrors(ctx context.Context, q *db.Queries, msg Message, author AccountID) error {
	hasMirrorAsk := false
	for _, block := range msg.Blocks {
		if block.Ask != nil && block.Ask.RefCoord != nil {
			hasMirrorAsk = true
			break
		}
	}
	if !hasMirrorAsk {
		return nil
	}
	isAgent, err := q.IsAgentAccount(ctx, string(author))
	if err != nil {
		return fmt.Errorf("store: identify ask mirror author: %w", err)
	}
	if !isAgent {
		return nil
	}
	for _, block := range msg.Blocks {
		if block.Ask == nil || block.Ask.RefCoord == nil {
			continue
		}
		coord := block.Ask.RefCoord.Normalized()
		if err := validCoordinate(coord.Provider, coord.Host, coord.Repo); err != nil {
			slog.WarnContext(ctx, "skip ask mirror with invalid forge coordinate", "ask_id", block.Ask.AskID, "error", err)
			continue
		}
		mode, err := q.ForgeMirrorMode(ctx, db.ForgeMirrorModeParams{
			ForgeProvider: int16(coord.Provider), //nolint:gosec // G115: provider is a CHECK-constrained 1..4 enum.
			ForgeHost:     coord.Host,
			Repo:          coord.Repo,
		})
		if err != nil {
			return fmt.Errorf("store: read ask forge mirror mode: %w", err)
		}
		if ForgeMirrorMode(mode) != ForgeMirrorAuto {
			continue
		}
		// The relay puts the posting session on ctx; a post with none stores an
		// empty session, which the poster refuses rather than misattributes.
		if err := q.InsertAskForgeMirror(ctx, db.InsertAskForgeMirrorParams{
			ID: newID(), AskID: block.Ask.AskID, ActorAccountID: string(author), TargetRef: block.Ask.Ref,
			SessionID: AgentSessionFromContext(ctx),
		}); err != nil {
			return fmt.Errorf("store: insert ask forge mirror: %w", err)
		}
	}
	return nil
}

func bumpAskMirror(ctx context.Context, q *db.Queries, askID string) error {
	if err := q.BumpAskForgeMirror(ctx, askID); err != nil {
		return fmt.Errorf("store: bump ask forge mirror: %w", err)
	}
	return nil
}

func nullableString(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}

func optionalTime(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	at := value.Time
	return &at
}

// forgeMirrorFromDB converts any forge-mirror RETURNING row; sqlc emits one
// identical struct per query, so callers convert to this shape.
func forgeMirrorFromDB(row db.ClaimForgeMirrorRow) ForgeMirrorRow {
	return ForgeMirrorRow{
		ID: row.ID, Source: row.Source, AskID: row.AskID, SourceKey: row.SourceKey,
		Actor: AccountID(row.ActorAccountID), Agent: AccountID(row.AgentAccountID),
		ClientRequestID: row.ClientRequestID.String, TargetRef: row.TargetRef,
		BodyRev: row.BodyRev, PostedRev: row.PostedRev, State: row.State, Attempts: int(row.Attempts),
		CommentID:  uint64(row.CommentID), //nolint:gosec // G115: written only from a uint64 checked against MaxInt64 in MarkForgeMirror
		CommentKey: row.CommentKey, CommentURL: row.CommentUrl, LastError: row.LastError,
		FirstAttemptAt: optionalTime(row.FirstAttemptAt), LastAttemptAt: optionalTime(row.LastAttemptAt),
		ClientCommentID: row.ClientCommentID, RecheckAt: optionalTime(row.RecheckAt), LeaseUntil: optionalTime(row.LeaseUntil),
		LeaseToken: row.LeaseToken, UpdatedAt: row.UpdatedAt.Time,
		SessionID: row.SessionID, SourcePayload: row.SourcePayload, LastErrorCode: row.LastErrorCode,
	}
}
