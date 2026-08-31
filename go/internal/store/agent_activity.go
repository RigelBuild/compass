package store

import (
	"context"
	"fmt"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// AgentActivity is an agent's durable activity: the free-text string it last
// authored plus the unix-millis timestamp of that write. Presence (online/away)
// is NOT here — it stays in-memory per DL-074 because it is a live-connection
// property, while the activity string is a durable statement the agent made,
// DB-backed so a Server restart recovers it (design.md :305-309).
type AgentActivity struct {
	// Activity is the free-text "what I'm doing" line.
	Activity string
	// ActivityAtUnixMs is when the activity was written, unix milliseconds.
	ActivityAtUnixMs int64
}

// SetActivity records agentAccountID's current activity string, last-write-wins.
// It is an UPSERT keyed on the agent's account id: the first call inserts the
// row, every later call overwrites both the string and its timestamp, so an
// agent has at most one activity of record. This is the durable half of the
// presence/activity split (DL-074) — presence lives in memory, but the activity
// survives a restart because it lands here.
func (s *Store) SetActivity(ctx context.Context, agentAccountID AccountID, activity string, atUnixMs int64) error {
	if err := s.q.SetActivity(ctx, db.SetActivityParams{
		AgentAccountID:   string(agentAccountID),
		Activity:         activity,
		ActivityAtUnixMs: atUnixMs,
	}); err != nil {
		return fmt.Errorf("store: set agent activity: %w", err)
	}
	return nil
}

// ActivityFor bulk-reads the durable activity for a set of agent account ids,
// returning a map keyed by id. An id with no row is simply absent from the map
// (never an empty-string entry): absent-from-table agents report empty activity
// at the caller, and the map's absence IS that signal (design.md T2:443). An
// empty input slice short-circuits to an empty map with no query. The id slice
// is passed as a single pgx array param and matched with `= ANY($1)`.
func (s *Store) ActivityFor(ctx context.Context, accountIDs []AccountID) (map[AccountID]AgentActivity, error) {
	out := make(map[AccountID]AgentActivity, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}

	ids := make([]string, len(accountIDs))
	for i, id := range accountIDs {
		ids[i] = string(id)
	}

	rows, err := s.q.ActivityFor(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("store: read agent activity: %w", err)
	}
	for _, row := range rows {
		out[AccountID(row.AgentAccountID)] = AgentActivity{
			Activity:         row.Activity,
			ActivityAtUnixMs: row.ActivityAtUnixMs,
		}
	}
	return out, nil
}
