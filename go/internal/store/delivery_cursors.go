package store

import (
	"context"
	"fmt"
	"time"

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
	SELECT $1, $2, COALESCE((SELECT MAX(m.seq) FROM messages m JOIN topics t ON t.id = m.topic_id WHERE t.channel_id = $2), 0)
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

// seedChannelDeliveryCursorsSQL seeds EVERY agent member of the channel to the
// current channel head in one statement — MAX(seq) over the channel's messages,
// 0 if empty, a same-channel constant across all seeded rows — with NO history
// replay (design record D2). The JOIN agent_accounts is the agent-only guard
// (the set form of the per-row WHERE EXISTS in seedDeliveryCursorSQL): a human
// member has no agent_accounts row and so is a silent no-op rather than an FK
// violation. ON CONFLICT DO NOTHING keeps a re-subscribe / re-run idempotent —
// it never resets an existing cursor. $1 is the channel id.
const seedChannelDeliveryCursorsSQL = `
	INSERT INTO agent_delivery_cursors (agent_account_id, channel_id, acked_seq)
	SELECT cm.account_id, $1,
	       COALESCE((SELECT MAX(m.seq) FROM messages m JOIN topics t ON t.id = m.topic_id WHERE t.channel_id = $1), 0)
	FROM channel_members cm
	JOIN agent_accounts aa ON aa.account_id = cm.account_id
	WHERE cm.channel_id = $1
	ON CONFLICT (agent_account_id, channel_id) DO NOTHING`

// seedChannelDeliveryCursors is the set-based counterpart to seedDeliveryCursor:
// it seeds all agent members of the channel in a single statement (collapsing the
// per-member seed loop), riding the caller's existing transaction so a missed
// seed is a loud failure in that same commit. Self-guarding (agent-only, see
// seedChannelDeliveryCursorsSQL) and idempotent, so it is safe to call for a
// channel whose member set includes humans.
func seedChannelDeliveryCursors(ctx context.Context, tx pgx.Tx, channel ChannelID) error {
	if _, err := tx.Exec(ctx, seedChannelDeliveryCursorsSQL, string(channel)); err != nil {
		return fmt.Errorf("store: seed channel delivery cursors: %w", err)
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
// contiguous cursor across every seq that is EITHER acked (in above_seqs) OR
// self-authored in this channel (author_account_id = agent — never dispatched).
// Because messages.seq is a table-global BIGSERIAL (0001_init.sql:114), a
// channel's owed seqs are sparse: a seq belonging to another channel sits
// between two owed seqs and currently stops the advance, so above_seqs can
// accumulate acked seqs on a busy multi-channel deployment. Tightening this
// advance to drain across cross-channel gaps without reintroducing commit-lag
// loss is a parked design question (SEA-1569 review, PR #55 Open Questions) — do
// not "fix" it by jumping the low-water past an un-acked lower owed seq, which
// loses commit-lagged messages. A duplicate or reordered ack is a no-op.
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
		`SELECT m.seq FROM messages m JOIN topics t ON t.id = m.topic_id WHERE m.id = $1 AND t.channel_id = $2`,
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
		 WHERE agent_account_id = $1 AND channel_id = $2
		 FOR UPDATE`,
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
	// (in the above-set) or self-authored in this channel (author_account_id =
	// agent — never dispatched, so vacuously satisfied). Query the
	// author-exclusion set once for the span above the cursor so a run of
	// self-posts cannot wedge the contiguous advance. Note: this advance stops
	// at the first un-acked owed seq, and because messages.seq is a table-global
	// BIGSERIAL a cross-channel seq can sit in that position — so on a busy
	// multi-channel deployment acked seqs above such a gap remain in above_seqs
	// rather than draining. That boundedness gap is the parked design question
	// (PR #55 Open Questions); correctness (no message loss) is unaffected.
	rows, err := tx.Query(ctx,
		`SELECT m.seq FROM messages m JOIN topics t ON t.id = m.topic_id
		 WHERE t.channel_id = $1 AND m.seq > $2 AND m.author_account_id = $3`,
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
		SELECT m.id, m.topic_id, t.channel_id, m.author_account_id, m.at_unix_ms, m.blocks
		FROM channel_members cm
		JOIN agent_accounts aa ON aa.account_id = cm.account_id
		JOIN topics t ON t.channel_id = cm.channel_id
		JOIN messages m ON m.topic_id = t.id
		JOIN channels ch ON ch.id = cm.channel_id
		LEFT JOIN agent_delivery_cursors dc
		       ON dc.agent_account_id = cm.account_id AND dc.channel_id = cm.channel_id
		WHERE cm.account_id = $1
		  AND (cm.subscribed OR cm.channel_id = aa.home_channel_id OR ch.mandatory_subscription)
		  AND m.author_account_id <> $1
		  AND m.seq > COALESCE(
		        dc.acked_seq,
		        (SELECT COALESCE(MAX(mh.seq), 0) FROM messages mh JOIN topics th ON th.id = mh.topic_id WHERE th.channel_id = cm.channel_id))
		  AND m.seq <> ALL(COALESCE(dc.above_seqs, '{}'::BIGINT[]))
		ORDER BY t.channel_id, m.seq ASC`
	rows, err := s.pool.Query(ctx, q, string(agent))
	if err != nil {
		return nil, fmt.Errorf("store: sweep undelivered messages: %w", err)
	}
	defer rows.Close()

	// Scan channel alongside each message (the message row no longer carries it);
	// the channel keys the returned map.
	out := make(map[ChannelID][]Message)
	for rows.Next() {
		var (
			id, topicID, channelID, author string
			atMS                           int64
			blocksJSON                     []byte
		)
		if err := rows.Scan(&id, &topicID, &channelID, &author, &atMS, &blocksJSON); err != nil {
			return nil, fmt.Errorf("store: scan undelivered message: %w", err)
		}
		blocks, err := unmarshalBlocks(blocksJSON)
		if err != nil {
			return nil, err
		}
		out[ChannelID(channelID)] = append(out[ChannelID(channelID)], Message{
			ID:              MessageID(id),
			TopicID:         topicID,
			AuthorAccountID: AccountID(author),
			At:              time.UnixMilli(atMS).UTC(),
			Blocks:          blocks,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate undelivered messages: %w", err)
	}
	return out, nil
}
