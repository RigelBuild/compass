package store

import (
	"context"
	"fmt"
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
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO agent_activity (agent_account_id, activity, activity_at_unix_ms)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (agent_account_id)
		 DO UPDATE SET activity = EXCLUDED.activity,
		               activity_at_unix_ms = EXCLUDED.activity_at_unix_ms`,
		string(agentAccountID), activity, atUnixMs,
	); err != nil {
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

	rows, err := s.pool.Query(ctx,
		`SELECT agent_account_id, activity, activity_at_unix_ms
		 FROM agent_activity
		 WHERE agent_account_id = ANY($1)`,
		ids,
	)
	if err != nil {
		return nil, fmt.Errorf("store: read agent activity: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id       string
			activity string
			atMs     int64
		)
		if err := rows.Scan(&id, &activity, &atMs); err != nil {
			return nil, fmt.Errorf("store: scan agent activity: %w", err)
		}
		out[AccountID(id)] = AgentActivity{Activity: activity, ActivityAtUnixMs: atMs}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate agent activity: %w", err)
	}
	return out, nil
}
