package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// seedDeliveryCursorSQL seeds a per-(agent, channel) delivery cursor to the
// current channel head — MAX(seq) over the channel's messages, 0 if empty — with
// NO history replay (design record D2). It is self-guarding and idempotent in one
// race-free statement: the WHERE EXISTS admits the row only for an agent account
// (a user id yields zero rows, so a non-agent member is a silent no-op rather
// than an FK violation), and ON CONFLICT DO NOTHING means a re-subscribe never
// resets an existing cursor. $1 is the agent account id, $2 the channel id.
const seedDeliveryCursorSQL = `
	INSERT INTO agent_delivery_cursors (agent_account_id, channel_id, acked_seq)
	SELECT $1, $2, COALESCE((SELECT MAX(seq) FROM messages WHERE channel_id = $2), 0)
	WHERE EXISTS (SELECT 1 FROM agent_accounts WHERE account_id = $1)
	ON CONFLICT (agent_account_id, channel_id) DO NOTHING`

// seedDeliveryCursor is the shared in-txn seed: it rides the caller's existing
// transaction (the channel_members insert txn) so a missed seed is a loud
// constraint failure in that same commit, never a silent skip. Called by the two
// membership-insert sites (CreateAgent home-channel seed, addOrUpdateMember
// subscribe upsert) and by the exported SeedDeliveryCursor wrapper. Self-guarding
// (see seedDeliveryCursorSQL), so it is safe to call unconditionally for a member
// whose agent-ness is not separately known.
func seedDeliveryCursor(ctx context.Context, tx pgx.Tx, agent AccountID, channel ChannelID) error {
	if _, err := tx.Exec(ctx, seedDeliveryCursorSQL, string(agent), string(channel)); err != nil {
		return fmt.Errorf("store: seed delivery cursor: %w", err)
	}
	return nil
}

// SeedDeliveryCursor seeds acked_seq to the current channel head (MAX(seq) over
// the channel's messages, 0 if empty) — NO history replay. It MUST be called in
// the SAME txn as the channel_members row insert (the seed rides that commit, so
// a missed seed is a loud constraint failure), which is why it takes an explicit
// pgx.Tx. An INSERT; a duplicate seed for an existing (agent, channel) is a no-op
// (ON CONFLICT DO NOTHING) so a re-subscribe does not reset the cursor. A thin
// wrapper over the shared seedDeliveryCursor helper.
func (s *Store) SeedDeliveryCursor(ctx context.Context, tx pgx.Tx, agent AccountID, channel ChannelID) error {
	return seedDeliveryCursor(ctx, tx, agent, channel)
}

// AckDelivery resolves messageID → messages.seq for THIS (agent, channel); a
// message never dispatched to this agent for this channel is a no-op (the
// resolution IS the overshoot clamp — a fabricated id cannot advance the
// cursor). It marks the seq acked (retained in above_seqs), then advances the
// contiguous cursor across every seq that is EITHER acked (in above_seqs) OR not
// owed to this agent (author_account_id = agent — the agent's own posts, never
// dispatched), retaining sparse above-cursor seqs in above_seqs. A duplicate or
// reordered ack is a no-op.
func (s *Store) AckDelivery(ctx context.Context, agent AccountID, channel ChannelID, messageID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin ack delivery: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // deferred cleanup; the Commit below is the real outcome.

	// Resolve messageID → seq scoped to THIS channel. A message id that names no
	// row in this channel (fabricated, foreign, or in another channel) resolves
	// to no row: the overshoot clamp — a fabricated id cannot advance the cursor.
	var seq int64
	switch err := tx.QueryRow(ctx,
		`SELECT seq FROM messages WHERE id = $1 AND channel_id = $2`,
		messageID, string(channel),
	).Scan(&seq); {
	case noRows(err):
		return nil // never dispatched to this (agent, channel): a no-op.
	case err != nil:
		return fmt.Errorf("store: resolve ack message: %w", err)
	}

	// Load the current cursor. An absent row means no cursor was seeded for this
	// (agent, channel): there is nothing to advance, so the ack is a no-op (the
	// seed-at-subscribe invariant owns cursor creation, not an ack).
	var (
		ackedSeq  int64
		aboveSeqs []int64
	)
	switch err := tx.QueryRow(ctx,
		`SELECT acked_seq, above_seqs FROM agent_delivery_cursors
		 WHERE agent_account_id = $1 AND channel_id = $2`,
		string(agent), string(channel),
	).Scan(&ackedSeq, &aboveSeqs); {
	case noRows(err):
		return nil // no cursor to advance.
	case err != nil:
		return fmt.Errorf("store: load delivery cursor: %w", err)
	}

	// A duplicate or reordered ack (at or below the contiguous cursor) is a
	// no-op: the seq is already vacuously satisfied.
	if seq <= ackedSeq {
		return nil
	}

	// Record the acked seq in the above-set (idempotent), then drain the
	// contiguous prefix.
	above := make(map[int64]bool, len(aboveSeqs)+1)
	for _, s := range aboveSeqs {
		above[s] = true
	}
	above[seq] = true

	// Advance the contiguous cursor across every next seq that is either acked
	// (in the above-set) or not owed to this agent (its own post — never
	// dispatched, so vacuously satisfied). Query the author-exclusion set once
	// for the span above the cursor so a run of self-posts cannot wedge the
	// contiguous advance and does not linger in above_seqs.
	rows, err := tx.Query(ctx,
		`SELECT seq FROM messages
		 WHERE channel_id = $1 AND seq > $2 AND author_account_id = $3`,
		string(channel), ackedSeq, string(agent),
	)
	if err != nil {
		return fmt.Errorf("store: load self-authored seqs: %w", err)
	}
	ownSeqs := make(map[int64]bool)
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan self-authored seq: %w", err)
		}
		ownSeqs[s] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate self-authored seqs: %w", err)
	}

	for {
		next := ackedSeq + 1
		if above[next] || ownSeqs[next] {
			ackedSeq = next
			delete(above, next)
			continue
		}
		break
	}

	// The retained above-set is exactly the acked seqs strictly above the
	// advanced contiguous cursor.
	remaining := make([]int64, 0, len(above))
	for s := range above {
		if s > ackedSeq {
			remaining = append(remaining, s)
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE agent_delivery_cursors
		 SET acked_seq = $3, above_seqs = $4, acked_at = now()
		 WHERE agent_account_id = $1 AND channel_id = $2`,
		string(agent), string(channel), ackedSeq, remaining,
	); err != nil {
		return fmt.Errorf("store: advance delivery cursor: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit ack delivery: %w", err)
	}
	return nil
}

// UndeliveredMessages is the sweep read: over the D1 disjunct channel set —
// every channel the agent is subscribed to PLUS its home channel (which sweeps
// regardless of its subscribed flag) — returns the messages still owed to this
// agent, ascending seq per channel:
// seq > acked_seq AND seq <> ALL(above_seqs) AND author_account_id <> agent. An
// absent cursor row is the legacy fail-safe: the agent is treated as caught-up to
// the current channel head (no history replay), NOT seq 0 — so a subscribed
// channel with no cursor contributes nothing rather than a full replay. Channels
// with no owed messages are omitted from the map.
func (s *Store) UndeliveredMessages(ctx context.Context, agent AccountID) (map[ChannelID][]Message, error) {
	// One query over the agent's sweep channel set. The set is the D1 disjunct
	// (design.md:118-120, :127-128, :343, :708): a channel the agent is
	// subscribed to OR its home channel — the home channel always sweeps,
	// independent of its channel_members.subscribed flag, so a home row flipped
	// subscribed=false (addOrUpdateMember DO UPDATE) still delivers. $1 is always
	// an agent, so the inner JOIN to agent_accounts matches exactly one row (no
	// fan-out) and yields its home_channel_id. The cursor is LEFT JOINed: a
	// present row gives its acked_seq/above_seqs; an absent row (legacy fail-safe)
	// is coalesced to the channel head via a correlated MAX(seq), so the seq >
	// cursor predicate admits nothing (caught-up, no replay). author_account_id
	// <> agent excludes the agent's own posts; the array predicate excludes the
	// retained above-set.
	const q = `
		SELECT m.id, m.channel_id, m.author_account_id, m.at_unix_ms, m.blocks, COALESCE(m.parent_message_id, '')
		FROM channel_members cm
		JOIN agent_accounts aa ON aa.account_id = cm.account_id
		JOIN messages m ON m.channel_id = cm.channel_id
		LEFT JOIN agent_delivery_cursors dc
		       ON dc.agent_account_id = cm.account_id AND dc.channel_id = cm.channel_id
		WHERE cm.account_id = $1
		  AND (cm.subscribed OR cm.channel_id = aa.home_channel_id)
		  AND m.author_account_id <> $1
		  AND m.seq > COALESCE(
		        dc.acked_seq,
		        (SELECT COALESCE(MAX(mh.seq), 0) FROM messages mh WHERE mh.channel_id = cm.channel_id))
		  AND m.seq <> ALL(COALESCE(dc.above_seqs, '{}'::BIGINT[]))
		ORDER BY m.channel_id, m.seq ASC`
	rows, err := s.pool.Query(ctx, q, string(agent))
	if err != nil {
		return nil, fmt.Errorf("store: sweep undelivered messages: %w", err)
	}
	defer rows.Close()

	msgs, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return map[ChannelID][]Message{}, nil
	}
	out := make(map[ChannelID][]Message)
	for _, m := range msgs {
		out[m.Container.ChannelID] = append(out[m.Container.ChannelID], m)
	}
	return out, nil
}
