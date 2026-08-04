-- ── Topics ────────────────────────────────────────────────────────────────
-- The Zulip-style threading model (compass-zulip-threading-model design.md D2):
-- a channel's conversation is partitioned into named topics, and every message
-- lives in exactly one topic. Topics are born via a post naming a topic (there
-- is no separate CreateTopic — a topic with zero messages is not a thing), so
-- the get-or-create on (channel_id, lower(name)) is the birth path. last_seq is
-- a denormalized activity marker (the highest messages.seq under the topic),
-- maintained in the same tx as each append, so a topic index can order by
-- recency without scanning messages. archived is a tidiness flag, not a lock: a
-- post addressed at an archived name revives it (get-or-create clears archived).
CREATE TABLE topics (
    id                    TEXT PRIMARY KEY,
    channel_id            TEXT NOT NULL REFERENCES channels (id) ON DELETE RESTRICT,
    name                  TEXT NOT NULL,
    created_by_account_id TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    created_at_unix_ms    BIGINT NOT NULL,
    archived              BOOLEAN NOT NULL DEFAULT FALSE,
    last_seq              BIGINT NOT NULL DEFAULT 0  -- denormalized activity order
);

-- Case-insensitive uniqueness per channel is the get-or-create key: two racing
-- posts naming the same topic converge on one row via ON CONFLICT DO NOTHING +
-- re-SELECT (never a surfaced unique-violation), and a case-variant of an
-- existing name resolves into it rather than forking a duplicate.
CREATE UNIQUE INDEX topics_channel_name_idx ON topics (channel_id, lower(name));

-- ── messages reshape (F10, pre-dogfood collapsed schema) ─────────────────────
-- A message records only its topic; the channel is topics.channel_id, one join
-- away. Both the old channel_id and parent_message_id columns go — there is no
-- shipped data to convert (pre-dogfood, zero rows), so this is the final shape
-- expressed directly, not an ADD/backfill/DROP dance. seq stays a table-global
-- BIGSERIAL and therefore channel-monotonic via the topic join, the property
-- the delivery cursor (D3) relies on.
DROP INDEX messages_channel_seq_idx;
ALTER TABLE messages DROP COLUMN channel_id;
ALTER TABLE messages DROP COLUMN parent_message_id;
ALTER TABLE messages ADD COLUMN topic_id TEXT NOT NULL REFERENCES topics (id) ON DELETE RESTRICT;

-- Newest-first paging within a topic; channel-level paging joins to topics and
-- filters channel_id, keying on the same table-monotonic seq.
CREATE INDEX messages_topic_seq_idx ON messages (topic_id, seq DESC);
