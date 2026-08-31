package store

import (
	"context"
	"fmt"
	"time"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// The Linear Agent Session association (compass-linear-agent-responder
// design.md §Part 2 / §T3): the durable link between a Linear AgentSession and
// the Compass conversation the responder routed it to. Written on a `created`
// event (UpsertLinearAgentSession, idempotent on the session-id PK) and read on
// a `prompted` event (LinearAgentSession) to route the follow-up to the same
// Manager/topic. No dedup column — message-level dedup is the comms rail's
// client_request_id (§Part 1); the association insert is idempotent on its own.

// LinearAgentSessionRow is one association row: the Linear session id, the Compass
// Manager the delegated issue routed to, that Manager's home channel, the comms
// topic the conversation landed in, and the issue it was delegated on
// (provenance; "" when none). CreatedAt is the server-assigned birth time.
type LinearAgentSessionRow struct {
	LinearSessionID  string
	ManagerAccountID AccountID
	ChannelID        ChannelID
	TopicID          string
	LinearIssueID    string // provenance; "" = no issue recorded (stored as SQL NULL)
	CreatedAt        time.Time
}

// UpsertLinearAgentSession idempotently records the association at row's
// linear_session_id: INSERT … ON CONFLICT (linear_session_id) DO NOTHING. It
// returns created=true when this call inserted the row and created=false on a
// replay (the session was already associated) — the caller uses that to skip
// the one-time `created`-side work (routing, ack thought, deep link) on a
// redelivered `created` event. An empty linear_session_id is a caller bug
// (ErrInvalidArgument). LinearIssueID "" is stored as SQL NULL.
func (s *Store) UpsertLinearAgentSession(ctx context.Context, row LinearAgentSessionRow) (created bool, err error) {
	if row.LinearSessionID == "" {
		return false, fmt.Errorf("%w: linear session id is required", ErrInvalidArgument)
	}
	affected, err := s.q.UpsertLinearAgentSession(ctx, db.UpsertLinearAgentSessionParams{
		LinearSessionID:  row.LinearSessionID,
		ManagerAccountID: string(row.ManagerAccountID),
		ChannelID:        string(row.ChannelID),
		TopicID:          row.TopicID,
		LinearIssueID:    textOrNull(row.LinearIssueID),
	})
	if err != nil {
		return false, fmt.Errorf("store: upsert linear agent session: %w", err)
	}
	return affected == 1, nil
}

// LinearAgentSession reads the association for linearSessionID — the `prompted`
// lookup that routes a follow-up to the recorded Manager/topic. An unknown
// session id is ErrNotFound; an empty id is ErrInvalidArgument.
func (s *Store) LinearAgentSession(ctx context.Context, linearSessionID string) (LinearAgentSessionRow, error) {
	if linearSessionID == "" {
		return LinearAgentSessionRow{}, fmt.Errorf("%w: linear session id is required", ErrInvalidArgument)
	}
	row, err := s.q.LinearAgentSession(ctx, linearSessionID)
	if err != nil {
		if noRows(err) {
			return LinearAgentSessionRow{}, fmt.Errorf("%w: linear agent session %q", ErrNotFound, linearSessionID)
		}
		return LinearAgentSessionRow{}, fmt.Errorf("store: read linear agent session: %w", err)
	}
	out := LinearAgentSessionRow{
		LinearSessionID:  row.LinearSessionID,
		ManagerAccountID: AccountID(row.ManagerAccountID),
		ChannelID:        ChannelID(row.ChannelID),
		TopicID:          row.TopicID,
	}
	if row.LinearIssueID.Valid {
		out.LinearIssueID = row.LinearIssueID.String
	}
	if row.CreatedAt.Valid {
		out.CreatedAt = row.CreatedAt.Time
	}
	return out, nil
}
