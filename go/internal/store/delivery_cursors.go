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

// RecordOwedMention durably records that messageID (posted in channel) is owed
// to agent — the no-loss backstop for a mentioned member outside the sweep set
// (RIG-1641 T1). Idempotent: the PK (agent_account_id, message_id) makes a
// re-record (settle re-fire, at-least-once routing) a no-op upsert.
//
// The caller MUST pass the message's OWN channel: channel_id is stored as
// context (T2 observability) but is not cross-checked against messageID's real
// channel here, and the read path (OwedMentions) derives the channel from the
// message's topic JOIN rather than this column, so a mismatched channel would
// persist a silently-inconsistent row. The settle-edge caller already holds the
// message's channel, so this is an assertion, not a lookup.
func (s *Store) RecordOwedMention(ctx context.Context, agent AccountID, channel ChannelID, messageID string) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO owed_mentions (agent_account_id, message_id, channel_id, recorded_at_unix_ms)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (agent_account_id, message_id) DO NOTHING`,
		string(agent), messageID, string(channel), time.Now().UnixMilli(),
	); err != nil {
		return fmt.Errorf("store: record owed mention: %w", err)
	}
	return nil
}

// OwedMentions returns every message owed to agent, keyed by channel, ascending
// seq per channel — shape mirrors UndeliveredMessages (this file). It re-reads
// the live message rows (JOIN messages) so a swept owed mention carries current
// blocks; an owed row whose message was deleted simply doesn't join (CASCADE
// also removes it, so this is belt-and-suspenders). Channels with no owed
// messages are omitted from the map.
func (s *Store) OwedMentions(ctx context.Context, agent AccountID) (map[ChannelID][]Message, error) {
	const q = `
		SELECT m.id, m.topic_id, t.channel_id, m.author_account_id, m.at_unix_ms, m.blocks
		FROM owed_mentions om
		JOIN messages m ON m.id = om.message_id
		JOIN topics t ON t.id = m.topic_id
		WHERE om.agent_account_id = $1
		ORDER BY t.channel_id, m.seq ASC`
	rows, err := s.pool.Query(ctx, q, string(agent))
	if err != nil {
		return nil, fmt.Errorf("store: read owed mentions: %w", err)
	}
	defer rows.Close()
	return scanMessagesByChannel(rows, "owed mention")
}

// scanMessagesByChannel drains rows of the shared per-channel message projection
// (m.id, m.topic_id, t.channel_id, m.author_account_id, m.at_unix_ms, m.blocks,
// ordered by channel then seq) into a channel-keyed map — the scan half shared by
// UndeliveredMessages (the cursor sweep) and OwedMentions (the mention-gap
// backstop), which differ only in their query. `what` names the row in error
// messages ("undelivered message" / "owed mention"). Channels with no rows are
// absent from the map.
func scanMessagesByChannel(rows pgx.Rows, what string) (map[ChannelID][]Message, error) {
	out := make(map[ChannelID][]Message)
	for rows.Next() {
		var (
			id, topicID, channelID, author string
			atMS                           int64
			blocksJSON                     []byte
		)
		if err := rows.Scan(&id, &topicID, &channelID, &author, &atMS, &blocksJSON); err != nil {
			return nil, fmt.Errorf("store: scan %s: %w", what, err)
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
		return nil, fmt.Errorf("store: iterate %ss: %w", what, err)
	}
	return out, nil
}

// clearOwedMention deletes the owed row for (agent, message_id) inside an
// EXISTING txn (T1's AckDelivery restructure), independent of channel-cursor
// state, and reports whether a row was actually deleted. Clearing an absent row
// is a no-op (cleared=false, err=nil).
func (s *Store) clearOwedMention(ctx context.Context, tx pgx.Tx, agent AccountID, messageID string) (cleared bool, err error) {
	tag, err := tx.Exec(ctx,
		`DELETE FROM owed_mentions WHERE agent_account_id = $1 AND message_id = $2`,
		string(agent), messageID,
	)
	if err != nil {
		return false, fmt.Errorf("store: clear owed mention: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ClearOwedMention deletes the owed row for (agent, messageID) using the pool
// (no txn) — the sweep-path sibling to clearOwedMention, which requires an
// existing txn (AckDelivery). sweepOwedMentions clears a permanently-unreadable
// owed message this way so a vanished message stops re-logging on every start.
// Clearing an absent row is a no-op.
func (s *Store) ClearOwedMention(ctx context.Context, agent AccountID, messageID string) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM owed_mentions WHERE agent_account_id = $1 AND message_id = $2`,
		string(agent), messageID,
	); err != nil {
		return fmt.Errorf("store: clear owed mention: %w", err)
	}
	return nil
}

// CountOwedMentions returns the total number of owed_mention rows across all
// agents — the startup visibility count (RIG-1641 T2 observability) so a
// silently-growing owed table is surfaced.
func (s *Store) CountOwedMentions(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM owed_mentions`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count owed mentions: %w", err)
	}
	return n, nil
}

// MessageWithChannel is a message plus its resolved channel and store-space seq
// — the projection of the pre-settle mention-loss recovery scan
// (UnroutedMentionMessages). The channel comes from topics.channel_id (one join
// from the message) and Seq is messages.seq, the scan-local cursor the caller
// advances across batches.
type MessageWithChannel struct {
	Message
	Channel ChannelID
	Seq     int64
}

// MarkMentionsRouted stamps messageID's settle-edge mention pass complete
// (mentions_routed_at = now, unix ms). Idempotent: a re-mark overwrites the
// timestamp; the contract readers rely on is NULL vs non-NULL only (RIG-2490
// T1). Marking an unknown id is a no-op.
func (s *Store) MarkMentionsRouted(ctx context.Context, messageID string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE messages SET mentions_routed_at = $1 WHERE id = $2`,
		time.Now().UnixMilli(), messageID,
	); err != nil {
		return fmt.Errorf("store: mark mentions routed: %w", err)
	}
	return nil
}

// UnroutedMentionMessages returns committed messages whose settle-edge mention
// pass never completed (mentions_routed_at IS NULL) AND whose seq is > afterSeq,
// ascending seq, each with its channel resolved through topics.channel_id — the
// recovery scan read (RIG-2490 T1). limit bounds one batch so a long-idle deploy
// cannot hold the whole backlog in memory; the caller loops, advancing afterSeq
// to the last returned seq, until a batch is short. afterSeq is a scan-LOCAL
// cursor (start each recovery scan at 0), never persisted — a held row skipped by
// the caller stays NULL and is re-scanned from 0 at the next recovery point, so
// this is not the killed high-water.
func (s *Store) UnroutedMentionMessages(ctx context.Context, afterSeq int64, limit int) ([]MessageWithChannel, error) {
	const q = `
		SELECT m.id, m.topic_id, m.author_account_id, m.at_unix_ms, m.blocks, t.channel_id, m.seq
		FROM messages m
		JOIN topics t ON t.id = m.topic_id
		WHERE m.mentions_routed_at IS NULL AND m.seq > $1
		ORDER BY m.seq ASC
		LIMIT $2`
	rows, err := s.pool.Query(ctx, q, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("store: read unrouted mention messages: %w", err)
	}
	defer rows.Close()
	var out []MessageWithChannel
	for rows.Next() {
		var (
			id, topicID, author, channelID string
			atMS, seq                      int64
			blocksJSON                     []byte
		)
		if err := rows.Scan(&id, &topicID, &author, &atMS, &blocksJSON, &channelID, &seq); err != nil {
			return nil, fmt.Errorf("store: scan unrouted mention message: %w", err)
		}
		blocks, err := unmarshalBlocks(blocksJSON)
		if err != nil {
			return nil, err
		}
		out = append(out, MessageWithChannel{
			Message: Message{
				ID:              MessageID(id),
				TopicID:         topicID,
				AuthorAccountID: AccountID(author),
				At:              time.UnixMilli(atMS).UTC(),
				Blocks:          blocks,
			},
			Channel: ChannelID(channelID),
			Seq:     seq,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate unrouted mention messages: %w", err)
	}
	return out, nil
}

// AckDelivery resolves messageID → messages.seq for THIS (agent, channel); a
// message never dispatched to this agent for this channel is a no-op (the
// resolution IS the overshoot clamp — a fabricated id cannot advance the
// cursor). Once the message is resolved to this channel it ALSO clears any
// owed_mention row for (agent, message_id) — the RIG-1641 T1 no-loss backstop —
// inside this same txn. It then marks the seq acked (retained in above_seqs) and
// advances the contiguous cursor across every seq that is EITHER acked (in
// above_seqs) OR self-authored in this channel (author_account_id = agent —
// never dispatched).
//
// The owed-clear runs FIRST, before the cursor arm, because the mention-gap
// population it exists for (unsubscribed, non-home, non-mandatory) has NO
// agent_delivery_cursors row: the cursor load below would hit noRows and return
// early, so a clear placed after it is unreachable and one placed before it
// would roll back with the no-op return. So whenever a row was cleared the txn
// COMMITS before returning, even when the cursor arm no-ops (no cursor, or a
// duplicate/reordered ack); the full-advance path's single commit at the end
// covers both the cursor advance and the clear.
//
// Because messages.seq is a table-global BIGSERIAL (0001_init.sql:202), a
// channel's owed seqs are sparse: a seq belonging to another channel sits
// between two owed seqs and currently stops the advance, so above_seqs can
// accumulate acked seqs on a busy multi-channel deployment. Tightening this
// advance to drain across cross-channel gaps without reintroducing commit-lag
// loss is a parked design question (RIG-1569 review, PR #55 Open Questions) — do
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
		// Never dispatched to this (agent, channel): a no-op. The owed row (if
		// any) is keyed (agent, message_id) for a valid ack of THIS channel, and
		// this ack does not name a message in this channel, so leave it alone.
		return nil
	case err != nil:
		return fmt.Errorf("store: resolve ack message: %w", err)
	}

	// The message belongs to this channel, so this is a valid ack: clear any
	// owed_mention for (agent, message_id) FIRST, keyed independently of cursor
	// state. cleared drives whether a cursor-arm no-op still commits (below).
	cleared, err := s.clearOwedMention(ctx, tx, agent, messageID)
	if err != nil {
		return err
	}

	// commitIfCleared commits when the owed-clear did work, so the clear is not
	// lost when the cursor arm no-ops; otherwise the ack changed nothing and the
	// deferred rollback ends the txn.
	commitIfCleared := func() error {
		if !cleared {
			return nil
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("store: commit ack delivery: %w", err)
		}
		return nil
	}

	// Load the current cursor. An absent row means no cursor was seeded for this
	// (agent, channel) — the mention-gap population — so there is nothing to
	// advance; commit the owed-clear if it did work, otherwise no-op.
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
		return commitIfCleared() // no cursor to advance; commit the clear if any.
	case err != nil:
		return fmt.Errorf("store: load delivery cursor: %w", err)
	}

	// A duplicate or reordered ack (at or below the contiguous cursor) advances
	// nothing; commit the owed-clear if it did work, otherwise no-op.
	if seq <= ackedSeq {
		return commitIfCleared()
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
	return scanMessagesByChannel(rows, "undelivered message")
}

// InSweepSet reports whether agent is in channel's D2 sweep set — subscribed OR
// channel is its home OR channel is mandatory_subscription. An agent OUTSIDE the
// sweep set (unsubscribed, non-home, non-mandatory member) has no cursor-sweep
// backstop, so an offline mention to it needs a durable owed_mentions row (T1).
// The disjunct mirrors UndeliveredMessages EXACTLY so the two never drift.
func (s *Store) InSweepSet(ctx context.Context, agent AccountID, channel ChannelID) (bool, error) {
	const q = `
		SELECT EXISTS(
			SELECT 1
			FROM channel_members cm
			JOIN agent_accounts aa ON aa.account_id = cm.account_id
			JOIN channels ch ON ch.id = cm.channel_id
			WHERE cm.account_id = $1
			  AND cm.channel_id = $2
			  AND (cm.subscribed OR cm.channel_id = aa.home_channel_id OR ch.mandatory_subscription))`
	var in bool
	if err := s.pool.QueryRow(ctx, q, string(agent), string(channel)).Scan(&in); err != nil {
		return false, fmt.Errorf("store: in sweep set: %w", err)
	}
	return in, nil
}
