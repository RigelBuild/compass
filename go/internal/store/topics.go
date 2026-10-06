package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

// ListTopics returns a channel's topics newest-activity-first. It checks caller
// participation through a member row or TREE derivation. Archived topics are
// omitted unless requested. Unauthorized and unknown channels map to ErrNotFound
// so existence cannot leak through this read.
func (s *Store) ListTopics(ctx context.Context, callerAccountID, channelID string, includeArchived bool) ([]Topic, error) {
	if channelID == "" {
		return nil, fmt.Errorf("%w: list topics channel is required", ErrInvalidArgument)
	}
	member, err := isChannelMember(ctx, s.scopedPool(), AccountID(callerAccountID), ChannelID(channelID))
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

// UpdateTopic renames and/or archives a topic under the caller's participation
// gate. A name collision moves source messages into the target and deletes the
// source in one transaction; the surviving topic is returned.
//
// The caller must participate in the channel through a member row or TREE
// derivation. A non-participant or unknown topic maps to ErrNotFound. Name and
// archived are optional. Renaming to the current case-folded name updates the
// source in place.
func (s *Store) UpdateTopic(ctx context.Context, callerAccountID, topicID string, name *string, archived *bool) (Topic, error) {
	if topicID == "" {
		return Topic{}, fmt.Errorf("%w: topic id is required", ErrInvalidArgument)
	}

	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return Topic{}, fmt.Errorf("store: begin update topic: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // deferred cleanup; the Commit below is the real outcome.

	// Resolve the topic and channel-participant gate in one statement. An explicit
	// member row or TREE derivation grants participation; no row (unknown topic or
	// non-participant) maps to ErrNotFound. FOR UPDATE locks the topic for this tx.
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

// GetOrCreateTopic returns the id of the topic named name in channelID, minting
// it on first use; it shares the append path's race-safe get-or-create.
func (s *Store) GetOrCreateTopic(ctx context.Context, channelID, name string, author AccountID) (string, error) {
	if channelID == "" || name == "" {
		return "", fmt.Errorf("%w: get-or-create topic needs a channel and a name", ErrInvalidArgument)
	}
	var topicID string
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		id, err := resolveTopicForAppend(ctx, tx, channelID, TopicRef{Name: name, Create: true}, author, time.Now().UnixMilli())
		topicID = id
		return err
	})
	if err != nil {
		return "", err
	}
	return topicID, nil
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
