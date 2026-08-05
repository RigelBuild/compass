package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// maxChannelPins is the per-channel cap on the pinned board: at most this many
// pins may be live in a channel at once (OQ-5, design.md T6). It is enforced
// in-txn under the channels-row FOR UPDATE lock (see PinMessage), not as a DB
// constraint, because a plain row count is the natural check and the lock
// already serializes the race that a constraint would guard.
const maxChannelPins = 5

// PinnedEntry is one pointer on a channel's pinned board: a reference to an
// existing message (the board never owns message rows), its ordering position,
// and the who/when of the pin. It mirrors a channel_pins row.
type PinnedEntry struct {
	// MessageID is the pinned message — a message already in the channel.
	MessageID MessageID
	// Position orders the board; PinnedEntries and the pin/unpin returns are
	// sorted ascending by it.
	Position int32
	// PinnedAtUnixMs is the pin time in Unix milliseconds.
	PinnedAtUnixMs int64
	// PinnedByAccountID is the account that created the pin.
	PinnedByAccountID AccountID
}

// PinMessage pins msg on ch's board, or repoints an existing pin, returning the
// full updated board ordered by position (design.md T6). It writes no message
// row ever — a pin is a pointer to a message that already lives in the channel,
// so msg is validated to belong to a topic under ch (join messages → topics on
// topic_id, check topics.channel_id = ch) and a message elsewhere is ErrNotFound
// (the not-found/forbidden merge — a caller cannot probe another channel's ids).
//
// The whole op rides one transaction that opens by taking ch's channels row FOR
// UPDATE; that single lock serializes both the cap race and the repoint CAS, so
// concurrent pins on one channel are ordered while pins on different channels
// never contend.
//
//   - replace == "" is a fresh pin: the cap is enforced under the lock (a board
//     already at maxChannelPins refuses the pin with ErrFailedPrecondition), then
//     the pointer is inserted at the next position.
//   - replace != "" is a repoint (compare-and-swap): replace must name a
//     currently-pinned message in ch, else the CAS is lost and ErrConflict is
//     returned ("board changed, re-read"). On success the replace entry is removed
//     and msg inserted at the SAME position, so a repoint preserves order and does
//     not change the pin count (no cap check needed).
//
// A repoint whose msg is already pinned (a duplicate board entry) surfaces the
// primary-key conflict as ErrConflict.
func (s *Store) PinMessage(ctx context.Context, ch ChannelID, msg MessageID, replace MessageID, by AccountID) ([]PinnedEntry, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin pin message: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // deferred cleanup; the Commit below is the real outcome.

	if err := lockChannelForPins(ctx, tx, ch); err != nil {
		return nil, err
	}

	// The pin target must be an existing message in THIS channel — the board is
	// pointers only, never a message write. A message in another channel (or no
	// message) is ErrNotFound.
	if err := requireMessageInChannel(ctx, tx, ch, msg); err != nil {
		return nil, err
	}

	if replace == "" {
		if err := pinFresh(ctx, tx, ch, msg, by); err != nil {
			return nil, err
		}
	} else {
		if err := pinRepoint(ctx, tx, ch, msg, replace, by); err != nil {
			return nil, err
		}
	}

	entries, err := pinnedEntriesTx(ctx, tx, ch)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit pin message: %w", err)
	}
	return entries, nil
}

// UnpinMessage removes the (ch, msg) pin under ch's channels-row lock and returns
// the remaining board ordered by position (design.md T6). Unpinning a message
// that is not pinned is a no-op: the delete affects zero rows and the unchanged
// board is returned (never an error), so a duplicate or racing unpin is benign.
func (s *Store) UnpinMessage(ctx context.Context, ch ChannelID, msg MessageID) ([]PinnedEntry, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin unpin message: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // deferred cleanup; the Commit below is the real outcome.

	if err := lockChannelForPins(ctx, tx, ch); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM channel_pins WHERE channel_id = $1 AND message_id = $2`,
		string(ch), string(msg),
	); err != nil {
		return nil, fmt.Errorf("store: delete channel pin: %w", err)
	}

	entries, err := pinnedEntriesTx(ctx, tx, ch)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit unpin message: %w", err)
	}
	return entries, nil
}

// PinnedEntries returns ch's pinned board ordered by position. It is a read-only
// view and takes no lock — a snapshot of the board as committed, safe to run
// against the pool directly.
func (s *Store) PinnedEntries(ctx context.Context, ch ChannelID) ([]PinnedEntry, error) {
	return pinnedEntriesTx(ctx, s.pool, ch)
}

// lockChannelForPins takes the channels-row FOR UPDATE lock that serializes every
// mutating board op on ch (the cap race and the repoint CAS both ride it). An
// unknown channel matches zero rows and is ErrNotFound.
func lockChannelForPins(ctx context.Context, tx pgx.Tx, ch ChannelID) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM channels WHERE id = $1 FOR UPDATE`, string(ch)).Scan(&one)
	if err != nil {
		if err == pgx.ErrNoRows {
			return fmt.Errorf("%w: channel %q", ErrNotFound, ch)
		}
		return fmt.Errorf("store: lock channel for pins: %w", err)
	}
	return nil
}

// requireMessageInChannel verifies msg is an existing message whose topic lives
// under ch (messages carry topic_id, not channel_id, post-0010 — DL-098), so a
// pin points only at a message the channel actually contains. A message in
// another channel or no message at all is ErrNotFound (the not-found/forbidden
// merge).
func requireMessageInChannel(ctx context.Context, tx pgx.Tx, ch ChannelID, msg MessageID) error {
	var one int
	err := tx.QueryRow(ctx,
		`SELECT 1 FROM messages m JOIN topics t ON t.id = m.topic_id
		 WHERE m.id = $1 AND t.channel_id = $2`,
		string(msg), string(ch),
	).Scan(&one)
	if err != nil {
		if err == pgx.ErrNoRows {
			return fmt.Errorf("%w: message %q not in channel %q", ErrNotFound, msg, ch)
		}
		return fmt.Errorf("store: check message in channel: %w", err)
	}
	return nil
}

// pinFresh enforces the per-channel cap under the caller's held lock and inserts
// a new pointer at the next position. A board already at maxChannelPins refuses
// the pin with ErrFailedPrecondition; a duplicate pin of an already-pinned
// message surfaces the primary-key conflict as ErrConflict.
func pinFresh(ctx context.Context, tx pgx.Tx, ch ChannelID, msg MessageID, by AccountID) error {
	// Count and next-position under the lock: the lock makes this read-modify
	// -write race-free, so the cap cannot be exceeded by a concurrent pin.
	var count int
	var nextPos int32
	if err := tx.QueryRow(ctx,
		`SELECT count(*), COALESCE(MAX(position), -1) + 1 FROM channel_pins WHERE channel_id = $1`,
		string(ch),
	).Scan(&count, &nextPos); err != nil {
		return fmt.Errorf("store: count channel pins: %w", err)
	}
	if count >= maxChannelPins {
		return fmt.Errorf("%w: channel %q already has the maximum of %d pins", ErrFailedPrecondition, ch, maxChannelPins)
	}
	if err := insertPin(ctx, tx, ch, msg, nextPos, by); err != nil {
		return err
	}
	return nil
}

// pinRepoint performs the compare-and-swap under the caller's held lock: replace
// must name a currently-pinned message in ch (else the CAS is lost, ErrConflict),
// and on success its entry is removed and msg inserted at the same position, so
// order is preserved and the pin count is unchanged.
func pinRepoint(ctx context.Context, tx pgx.Tx, ch ChannelID, msg, replace MessageID, by AccountID) error {
	// Delete the replaced entry and capture its position in one statement; zero
	// rows means replace was not pinned — the CAS is lost.
	var pos int32
	err := tx.QueryRow(ctx,
		`DELETE FROM channel_pins WHERE channel_id = $1 AND message_id = $2 RETURNING position`,
		string(ch), string(replace),
	).Scan(&pos)
	if err != nil {
		if err == pgx.ErrNoRows {
			return fmt.Errorf("%w: pin %q is no longer on channel %q's board, re-read", ErrConflict, replace, ch)
		}
		return fmt.Errorf("store: repoint delete channel pin: %w", err)
	}
	if err := insertPin(ctx, tx, ch, msg, pos, by); err != nil {
		return err
	}
	return nil
}

// insertPin writes one channel_pins pointer, mapping a primary-key conflict (the
// message already pinned in ch) to ErrConflict.
func insertPin(ctx context.Context, tx pgx.Tx, ch ChannelID, msg MessageID, pos int32, by AccountID) error {
	if _, err := tx.Exec(ctx,
		`INSERT INTO channel_pins (channel_id, message_id, position, pinned_at_unix_ms, pinned_by_account_id)
		 VALUES ($1, $2, $3, $4, $5)`,
		string(ch), string(msg), pos, time.Now().UTC().UnixMilli(), string(by),
	); err != nil {
		if pgErrIs(err, pgUniqueViolation) {
			return fmt.Errorf("%w: message %q is already pinned in channel %q", ErrConflict, msg, ch)
		}
		return fmt.Errorf("store: insert channel pin: %w", err)
	}
	return nil
}

// pinnedEntriesTx reads ch's board ordered by position from any querier (the pool
// for the read-only PinnedEntries, or the open tx for the mutating returns).
func pinnedEntriesTx(ctx context.Context, q querier, ch ChannelID) ([]PinnedEntry, error) {
	rows, err := q.Query(ctx,
		`SELECT message_id, position, pinned_at_unix_ms, pinned_by_account_id
		 FROM channel_pins WHERE channel_id = $1 ORDER BY position`,
		string(ch),
	)
	if err != nil {
		return nil, fmt.Errorf("store: query channel pins: %w", err)
	}
	defer rows.Close()

	var entries []PinnedEntry
	for rows.Next() {
		var e PinnedEntry
		var msgID, by string
		if err := rows.Scan(&msgID, &e.Position, &e.PinnedAtUnixMs, &by); err != nil {
			return nil, fmt.Errorf("store: scan channel pin: %w", err)
		}
		e.MessageID = MessageID(msgID)
		e.PinnedByAccountID = AccountID(by)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate channel pins: %w", err)
	}
	return entries, nil
}
