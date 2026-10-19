package store

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// Notice keeps a foreign rebase message recoverable if posting fails.
type Notice struct {
	ID            string
	TenantID      TenantID
	FromAccountID AccountID
	ToAccountID   AccountID
	RequestKey    string
	Text          string
	CreatedAt     time.Time
	DeliveredAt   *time.Time
	Attempts      int
}

// RecordNotice persists before posting so a failed attempt can be replayed.
func (s *Store) RecordNotice(ctx context.Context, n Notice) (Notice, error) {
	if n.FromAccountID == "" || n.ToAccountID == "" || n.RequestKey == "" {
		return Notice{}, fmt.Errorf("%w: notice sender, recipient, and request key are required", ErrInvalidArgument)
	}
	if n.ID == "" {
		n.ID = uuid.NewString()
	}
	id, err := noticeUUID(n.ID)
	if err != nil {
		return Notice{}, err
	}
	row, err := s.q.RecordForgeNotice(ctx, db.RecordForgeNoticeParams{
		ID:            id,
		TenantID:      string(s.resolveTenant(ctx)),
		FromAccountID: string(n.FromAccountID),
		ToAccountID:   string(n.ToAccountID),
		RequestKey:    n.RequestKey,
		Text:          n.Text,
	})
	if err == nil {
		return noticeFromFields(
			row.ID, row.TenantID, row.FromAccountID, row.ToAccountID, row.RequestKey, row.Text,
			row.CreatedAt, row.DeliveredAt, row.Attempts,
		), nil
	}
	if !noRows(err) {
		return Notice{}, fmt.Errorf("store: record forge notice: %w", err)
	}
	existing, err := s.q.ForgeNoticeByRequestKey(ctx, db.ForgeNoticeByRequestKeyParams{
		TenantID:   string(s.resolveTenant(ctx)),
		RequestKey: n.RequestKey,
	})
	if err != nil {
		return Notice{}, fmt.Errorf("store: read existing forge notice: %w", err)
	}
	return noticeFromFields(
		existing.ID, existing.TenantID, existing.FromAccountID, existing.ToAccountID, existing.RequestKey, existing.Text,
		existing.CreatedAt, existing.DeliveredAt, existing.Attempts,
	), nil
}

// MarkNoticeDelivered excludes a message from future sweep passes.
func (s *Store) MarkNoticeDelivered(ctx context.Context, id string) error {
	noticeID, err := noticeUUID(id)
	if err != nil {
		return err
	}
	affected, err := s.q.MarkForgeNoticeDelivered(ctx, noticeID)
	if err != nil {
		return fmt.Errorf("store: mark forge notice delivered: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("%w: forge notice %q", ErrNotFound, id)
	}
	return nil
}

// UndeliveredNotices lets the cross-tenant sweep retry after restart.
func (s *Store) UndeliveredNotices(ctx context.Context, limit int) ([]Notice, error) {
	if limit < 1 || int64(limit) > math.MaxInt32 {
		return nil, fmt.Errorf("%w: undelivered notice limit must be between 1 and %d", ErrInvalidArgument, math.MaxInt32)
	}
	rows, err := s.q.UndeliveredForgeNotices(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("store: list undelivered forge notices: %w", err)
	}
	notices := make([]Notice, 0, len(rows))
	for _, row := range rows {
		notices = append(notices, noticeFromFields(
			row.ID, row.TenantID, row.FromAccountID, row.ToAccountID, row.RequestKey, row.Text,
			row.CreatedAt, row.DeliveredAt, row.Attempts,
		))
	}
	return notices, nil
}

// BumpNoticeAttempt records failed deliveries for the outbox.
func (s *Store) BumpNoticeAttempt(ctx context.Context, id string) error {
	noticeID, err := noticeUUID(id)
	if err != nil {
		return err
	}
	affected, err := s.q.BumpForgeNoticeAttempt(ctx, noticeID)
	if err != nil {
		return fmt.Errorf("store: bump forge notice attempt: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("%w: undelivered forge notice %q", ErrNotFound, id)
	}
	return nil
}

func noticeUUID(id string) (pgtype.UUID, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("%w: notice id must be a UUID", ErrInvalidArgument)
	}
	return pgtype.UUID{Bytes: parsed, Valid: true}, nil
}

func noticeFromFields(
	id, tenantID, from, to, requestKey, text string,
	createdAt, deliveredAt pgtype.Timestamptz,
	attempts int32,
) Notice {
	return Notice{
		ID:            id,
		TenantID:      TenantID(tenantID),
		FromAccountID: AccountID(from),
		ToAccountID:   AccountID(to),
		RequestKey:    requestKey,
		Text:          text,
		CreatedAt:     createdAt.Time,
		DeliveredAt:   optionalTime(deliveredAt),
		Attempts:      int(attempts),
	}
}
