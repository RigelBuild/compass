package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/RigelBuild/compass/go/internal/store/db"
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
	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin pin message: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // deferred cleanup; the Commit below is the real outcome.

	postPolicy, ownerAcct, err := lockChannelForPins(ctx, tx, ch)
	if err != nil {
		return nil, err
	}

	// Board-mutation authz, enforced IN this tx under the FOR UPDATE lock the
	// lock helper just took, BEFORE the pin write: `by` must be a member and,
	// on OWNER_ONLY, the owner — the SAME two-gate no-oracle check PostMessage
	// runs (store/messages.go:80-82). Under the row lock the who-may-act
	// decision is serialized against a concurrent membership/policy change, so a
	// just-removed member or now-unauthorized non-owner cannot race the write.
	if err := requireBoardMutator(ctx, tx, ch, by, postPolicy, ownerAcct); err != nil {
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
// the remaining board ordered by position (design.md T6). `by` must be authorized
// to mutate the board (a member, and the owner on OWNER_ONLY) — the same in-tx,
// under-lock authz PinMessage enforces. Unpinning a message that is not pinned is
// a no-op: the delete affects zero rows and the unchanged board is returned
// (never an error), so a duplicate or racing unpin is benign.
func (s *Store) UnpinMessage(ctx context.Context, ch ChannelID, msg MessageID, by AccountID) ([]PinnedEntry, error) {
	tx, err := s.beginTenantTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin unpin message: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // deferred cleanup; the Commit below is the real outcome.

	postPolicy, ownerAcct, err := lockChannelForPins(ctx, tx, ch)
	if err != nil {
		return nil, err
	}

	// Same in-tx, under-lock authz as PinMessage: `by` must be a member and, on
	// OWNER_ONLY, the owner — refused with the no-oracle ErrNotFound otherwise,
	// serialized by the row lock against a concurrent membership/policy change.
	if err := requireBoardMutator(ctx, tx, ch, by, postPolicy, ownerAcct); err != nil {
		return nil, err
	}
	if err := db.New(tx).DeleteChannelPin(ctx, db.DeleteChannelPinParams{
		ChannelID: string(ch),
		MessageID: string(msg),
	}); err != nil {
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
	return pinnedEntriesTx(ctx, s.scopedPool(), ch)
}

// lockChannelForPins takes the channels-row FOR UPDATE lock that serializes every
// mutating board op on ch (the cap race, the repoint CAS, and the board-mutation
// authz all ride it) and, in the same locking round-trip, reads the row's
// post_policy and owner so the caller can gate the mutator under the lock without
// a second query. An unknown channel matches zero rows and is ErrNotFound.
func lockChannelForPins(ctx context.Context, tx pgx.Tx, ch ChannelID) (postPolicy int32, ownerAcct string, err error) {
	row, err := db.New(tx).LockChannelForPins(ctx, string(ch))
	if err != nil {
		if noRows(err) {
			return 0, "", fmt.Errorf("%w: channel %q", ErrNotFound, ch)
		}
		return 0, "", fmt.Errorf("store: lock channel for pins: %w", err)
	}
	return int32(row.PostPolicy), row.OwnerAccountID, nil
}

// requireBoardMutator enforces board-mutation authz for `by` under the caller's
// held channels-row FOR UPDATE lock, mirroring PostMessage's two-gate no-oracle
// check (store/messages.go:80-82): `by` must be a member of ch (else ErrNotFound,
// the not-found/forbidden merge), and on an OWNER_ONLY channel only the owner may
// mutate — a non-owner member is refused with the SAME ErrNotFound a non-member
// gets, so the policy leaks no existence oracle. postPolicy/ownerAcct come from
// the locking SELECT lockChannelForPins already ran, so the check is serialized
// against a concurrent membership/policy change committing between gate and write.
func requireBoardMutator(ctx context.Context, tx pgx.Tx, ch ChannelID, by AccountID, postPolicy int32, ownerAcct string) error {
	if err := requireChannelMember(ctx, tx, by, ch); err != nil {
		return err
	}
	if ChannelPostPolicy(postPolicy) == ChannelPostPolicyOwnerOnly && string(by) != ownerAcct {
		return fmt.Errorf("%w: channel %q", ErrNotFound, ch)
	}
	return nil
}

// requireMessageInChannel verifies msg is an existing message whose topic lives
// under ch (messages carry topic_id, not channel_id, post-0010 — DL-098), so a
// pin points only at a message the channel actually contains. A message in
// another channel or no message at all is ErrNotFound (the not-found/forbidden
// merge).
func requireMessageInChannel(ctx context.Context, tx pgx.Tx, ch ChannelID, msg MessageID) error {
	_, err := db.New(tx).MessageInChannel(ctx, db.MessageInChannelParams{
		ID:        string(msg),
		ChannelID: string(ch),
	})
	if err != nil {
		if noRows(err) {
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
	counts, err := db.New(tx).CountChannelPins(ctx, string(ch))
	if err != nil {
		return fmt.Errorf("store: count channel pins: %w", err)
	}
	count := counts.Count
	nextPos := counts.NextPosition
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
	pos, err := db.New(tx).DeleteChannelPinReturningPosition(ctx, db.DeleteChannelPinReturningPositionParams{
		ChannelID: string(ch),
		MessageID: string(replace),
	})
	if err != nil {
		if noRows(err) {
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
	if err := db.New(tx).InsertChannelPin(ctx, db.InsertChannelPinParams{
		ChannelID:         string(ch),
		MessageID:         string(msg),
		Position:          pos,
		PinnedAtUnixMs:    time.Now().UTC().UnixMilli(),
		PinnedByAccountID: string(by),
	}); err != nil {
		if pgErrIs(err, pgUniqueViolation) {
			return fmt.Errorf("%w: message %q is already pinned in channel %q", ErrConflict, msg, ch)
		}
		return fmt.Errorf("store: insert channel pin: %w", err)
	}
	return nil
}

// pinnedEntriesTx reads ch's board ordered by position from any querier (the pool
// for the read-only PinnedEntries, or the open tx for the mutating returns).
func pinnedEntriesTx(ctx context.Context, q db.DBTX, ch ChannelID) ([]PinnedEntry, error) {
	rows, err := db.New(q).PinnedEntries(ctx, string(ch))
	if err != nil {
		return nil, fmt.Errorf("store: query channel pins: %w", err)
	}
	entries := make([]PinnedEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, PinnedEntry{
			MessageID:         MessageID(row.MessageID),
			Position:          row.Position,
			PinnedAtUnixMs:    row.PinnedAtUnixMs,
			PinnedByAccountID: AccountID(row.PinnedByAccountID),
		})
	}
	return entries, nil
}
