-- ── Channel pins (the pinned board) ─────────────────────────────────────────
-- A channel's pinned board is a small, ordered set of POINTERS to existing
-- messages — it never writes a message row (DL-099/OQ-8): pin/unpin/repoint only
-- add, remove, or move an entry in this table, and PinMessage validates that the
-- target message already lives in the channel (join messages → topics on
-- messages.topic_id = topics.id, then topics.channel_id = the pinned channel,
-- since a message carries no channel_id post-0010, only topic_id — DL-098).
--
-- Every mutating op takes ONE channels-row lock (SELECT 1 FROM channels WHERE
-- id = $1 FOR UPDATE) before touching this table (design.md T6:604-608). That
-- single lock serializes BOTH races on the board at once: the per-channel cap
-- check (at most 5 pins per channel, OQ-5 — enforced in-txn under the lock, not
-- a DB constraint) and the repoint compare-and-swap (a repoint that names a
-- no-longer-pinned message loses the CAS). Concurrent pins on one channel thus
-- serialize on its channels row; pins on different channels never contend.
--
-- ON DELETE RESTRICT on every FK: a pin is a live reference, so deleting a
-- pinned channel, message, or the pinning account is refused rather than
-- silently orphaning or dropping a board entry.
CREATE TABLE channel_pins (
    channel_id           TEXT NOT NULL REFERENCES channels (id) ON DELETE RESTRICT,
    message_id           TEXT NOT NULL REFERENCES messages (id) ON DELETE RESTRICT,
    position             INTEGER NOT NULL,
    pinned_at_unix_ms    BIGINT NOT NULL,
    pinned_by_account_id TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    PRIMARY KEY (channel_id, message_id)
);
