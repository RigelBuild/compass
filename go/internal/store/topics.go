package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/RigelBuild/compass/go/internal/store/db"
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

	rows, err := s.q.ListTopics(ctx, db.ListTopicsParams{ChannelID: channelID, Column2: includeArchived})
	if err != nil {
		return nil, fmt.Errorf("store: list topics: %w", err)
	}
	return topicsFromRows(rows), nil
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
	q := db.New(tx)
	channelID, err := q.ResolveTopicForUpdate(ctx, db.ResolveTopicForUpdateParams{
		AccountID: callerAccountID,
		ID:        topicID,
	})
	switch {
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
		if err := q.SetTopicArchived(ctx, db.SetTopicArchivedParams{ID: surviving, Archived: *archived}); err != nil {
			return Topic{}, fmt.Errorf("store: set topic archived: %w", err)
		}
	}

	topic, err := q.GetTopic(ctx, surviving)
	if err != nil {
		return Topic{}, fmt.Errorf("store: read updated topic: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Topic{}, fmt.Errorf("store: commit update topic: %w", err)
	}
	return topicFromRow(topic), nil
}

// applyTopicRename renames topic topicID (in channelID) to newName, or merges it
// into a same-channel topic that already holds that name (case-insensitively).
// It returns the surviving topic id: topicID on an in-place rename, the merge
// target on a collision. Runs inside the caller's tx (which already locked the
// source row FOR UPDATE).
func (s *Store) applyTopicRename(ctx context.Context, tx pgx.Tx, channelID, topicID, newName string) (string, error) {
	// A same-channel topic already holding the target name (excluding the source
	// itself) is a merge target.
	q := db.New(tx)
	targetID, err := q.ResolveTopicRenameTarget(ctx, db.ResolveTopicRenameTargetParams{
		ChannelID: channelID,
		Lower:     newName,
		ID:        topicID,
	})
	switch {
	case noRows(err):
		// No collision: rename in place. The unique index still guards a race
		// with a concurrent create of the same name (that transaction holds its
		// own row lock); such a rename fails the constraint and surfaces as a
		// conflict rather than corrupting the index.
		if err := q.RenameTopic(ctx, db.RenameTopicParams{ID: topicID, Name: newName}); err != nil {
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
	if err := q.MoveMessagesToTopic(ctx, db.MoveMessagesToTopicParams{TopicID: targetID, TopicID_2: topicID}); err != nil {
		return "", fmt.Errorf("store: move messages on topic merge: %w", err)
	}
	if err := q.MergeTopicLastSeq(ctx, db.MergeTopicLastSeqParams{ID: targetID, ID_2: topicID}); err != nil {
		return "", fmt.Errorf("store: merge topic last_seq: %w", err)
	}
	if err := q.DeleteTopic(ctx, topicID); err != nil {
		return "", fmt.Errorf("store: delete merged topic: %w", err)
	}
	return targetID, nil
}

// topicFromRow maps the generated db.Topic (the shared topic projection) to the
// domain Topic; topicsFromRows applies it across a list read. They replace the
// former scanTopics pgx.Rows helper.
func topicFromRow(t db.Topic) Topic {
	return Topic{
		ID:                 t.ID,
		ChannelID:          t.ChannelID,
		Name:               t.Name,
		CreatedByAccountID: t.CreatedByAccountID,
		CreatedAtUnixMS:    t.CreatedAtUnixMs,
		Archived:           t.Archived,
		LastSeq:            t.LastSeq,
	}
}

func topicsFromRows(rows []db.Topic) []Topic {
	if len(rows) == 0 {
		return nil
	}
	out := make([]Topic, 0, len(rows))
	for _, t := range rows {
		out = append(out, topicFromRow(t))
	}
	return out
}
