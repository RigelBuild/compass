package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ListTopics returns the topics in channelID, newest-activity-first (last_seq
// descending, then birth time), scoped to the caller's visible set. Archived
// topics are omitted unless includeArchived is set. Visibility is the same D9
// gate the message reads apply: a caller who is not a member of the channel —
// or names a channel it cannot see — gets ErrNotFound (the not-found/forbidden
// merge), never a hint the channel exists or an empty list it could mistake for
// "no topics".
func (s *Store) ListTopics(ctx context.Context, callerAccountID, channelID string, includeArchived bool) ([]Topic, error) {
	if channelID == "" {
		return nil, fmt.Errorf("%w: list topics channel is required", ErrInvalidArgument)
	}
	member, err := isChannelMember(ctx, s.pool, AccountID(callerAccountID), ChannelID(channelID))
	if err != nil {
		return nil, err
	}
	if !member {
		// D9 merge: a non-member cannot tell an unauthorized channel from a
		// nonexistent one, so the refusal enumerates nothing.
		return nil, fmt.Errorf("%w: channel %q", ErrNotFound, channelID)
	}

	const q = `
		SELECT id, channel_id, name, created_by_account_id, created_at_unix_ms, archived, last_seq
		FROM topics
		WHERE channel_id = $1 AND ($2 OR NOT archived)
		ORDER BY last_seq DESC, created_at_unix_ms DESC, id`
	rows, err := s.pool.Query(ctx, q, channelID, includeArchived)
	if err != nil {
		return nil, fmt.Errorf("store: list topics: %w", err)
	}
	defer rows.Close()
	return scanTopics(rows)
}

// UpdateTopic renames and/or archives a topic under an acting account, or —
// when a rename collides with an existing topic name in the same channel —
// MERGES the two: the source topic's messages are re-pointed at the target
// (message rows carry the target's topic_id), the target's last_seq absorbs the
// source's, and the emptied source row is deleted, all in one transaction. The
// surviving topic is returned.
//
// The caller must be a member of the topic's channel; a topic it cannot see —
// or an unknown topicID — is ErrNotFound (the D9 not-found/forbidden merge, so
// topic existence cannot leak across a membership boundary). name and archived
// are each optional (nil = leave unchanged); the archived flag is applied to
// the SURVIVING topic (the target on a merge, the source otherwise).
//
// A rename whose lowercased name matches the topic's own current name is a
// harmless in-place rename (no merge — the collision check excludes self).
func (s *Store) UpdateTopic(ctx context.Context, callerAccountID, topicID string, name *string, archived *bool) (Topic, error) {
	if topicID == "" {
		return Topic{}, fmt.Errorf("%w: topic id is required", ErrInvalidArgument)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Topic{}, fmt.Errorf("store: begin update topic: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // deferred cleanup; the Commit below is the real outcome.

	// Resolve the topic + gate visibility in one statement: the caller must be a
	// member of the topic's channel. Zero rows (unknown topic OR non-member) ->
	// ErrNotFound. FOR UPDATE OF t locks the source topic row for the tx so a
	// concurrent rename/merge serializes.
	var channelID string
	switch err := tx.QueryRow(ctx,
		`SELECT t.channel_id FROM topics t
		 JOIN channel_members cm ON cm.channel_id = t.channel_id AND cm.account_id = $1
		 WHERE t.id = $2
		 FOR UPDATE OF t`,
		callerAccountID, topicID,
	).Scan(&channelID); {
	case noRows(err):
		return Topic{}, fmt.Errorf("%w: topic %q", ErrNotFound, topicID)
	case err != nil:
		return Topic{}, fmt.Errorf("store: resolve topic: %w", err)
	}

	// surviving is the topic id the update ends on: the merge target when a
	// rename collides, else the source itself.
	surviving := topicID
	if name != nil {
		targetID, err := s.applyTopicRename(ctx, tx, channelID, topicID, *name)
		if err != nil {
			return Topic{}, err
		}
		surviving = targetID
	}

	if archived != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE topics SET archived = $2 WHERE id = $1`, surviving, *archived,
		); err != nil {
			return Topic{}, fmt.Errorf("store: set topic archived: %w", err)
		}
	}

	var topic Topic
	if err := tx.QueryRow(ctx,
		`SELECT id, channel_id, name, created_by_account_id, created_at_unix_ms, archived, last_seq
		 FROM topics WHERE id = $1`, surviving,
	).Scan(&topic.ID, &topic.ChannelID, &topic.Name, &topic.CreatedByAccountID, &topic.CreatedAtUnixMS, &topic.Archived, &topic.LastSeq); err != nil {
		return Topic{}, fmt.Errorf("store: read updated topic: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Topic{}, fmt.Errorf("store: commit update topic: %w", err)
	}
	return topic, nil
}

// applyTopicRename renames topic topicID (in channelID) to newName, or merges it
// into a same-channel topic that already holds that name (case-insensitively).
// It returns the surviving topic id: topicID on an in-place rename, the merge
// target on a collision. Runs inside the caller's tx (which already locked the
// source row FOR UPDATE).
func (s *Store) applyTopicRename(ctx context.Context, tx pgx.Tx, channelID, topicID, newName string) (string, error) {
	// A same-channel topic already holding the target name (excluding the source
	// itself) is a merge target.
	var targetID string
	switch err := tx.QueryRow(ctx,
		`SELECT id FROM topics
		 WHERE channel_id = $1 AND lower(name) = lower($2) AND id <> $3
		 FOR UPDATE`,
		channelID, newName, topicID,
	).Scan(&targetID); {
	case noRows(err):
		// No collision: rename in place. The unique index still guards a race
		// with a concurrent create of the same name (that transaction holds its
		// own row lock); such a rename fails the constraint and surfaces as a
		// conflict rather than corrupting the index.
		if _, err := tx.Exec(ctx,
			`UPDATE topics SET name = $2 WHERE id = $1`, topicID, newName,
		); err != nil {
			if pgErrIs(err, pgUniqueViolation) {
				return "", fmt.Errorf("%w: topic name %q already exists in this channel", ErrConflict, newName)
			}
			return "", fmt.Errorf("store: rename topic: %w", err)
		}
		return topicID, nil
	case err != nil:
		return "", fmt.Errorf("store: resolve rename target: %w", err)
	}

	// Collision: merge the source into the target. Every source message carries
	// the target's topic_id, the target absorbs the source's activity marker,
	// and the emptied source row is deleted — all in this tx.
	if _, err := tx.Exec(ctx,
		`UPDATE messages SET topic_id = $1 WHERE topic_id = $2`, targetID, topicID,
	); err != nil {
		return "", fmt.Errorf("store: move messages on topic merge: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE topics dst SET last_seq = GREATEST(dst.last_seq, src.last_seq)
		 FROM topics src WHERE dst.id = $1 AND src.id = $2`,
		targetID, topicID,
	); err != nil {
		return "", fmt.Errorf("store: merge topic last_seq: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM topics WHERE id = $1`, topicID); err != nil {
		return "", fmt.Errorf("store: delete merged topic: %w", err)
	}
	return targetID, nil
}

// scanTopics reads topic rows into Topics.
func scanTopics(rows pgx.Rows) ([]Topic, error) {
	var topics []Topic
	for rows.Next() {
		var t Topic
		if err := rows.Scan(&t.ID, &t.ChannelID, &t.Name, &t.CreatedByAccountID, &t.CreatedAtUnixMS, &t.Archived, &t.LastSeq); err != nil {
			return nil, fmt.Errorf("store: scan topic: %w", err)
		}
		topics = append(topics, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate topics: %w", err)
	}
	return topics, nil
}
