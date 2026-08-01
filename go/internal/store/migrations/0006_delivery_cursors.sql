-- 0006_delivery_cursors: the durable per-(agent, channel) delivery cursor
-- (SEA-1569 T2, design record D2). One row records how far an agent has
-- confirmed delivery on a channel, so a sweep after a restart / reconnect
-- replays exactly the owed-but-unacked tail and never the full history.
--
-- Convention (0001_init.sql:7-11, 0003_agent_ownership.sql:15-16): text ids, FK
-- ON DELETE RESTRICT so a referenced agent/channel cannot be orphaned out from
-- under a cursor. The cursor is agent-only: agent_account_id references
-- agent_accounts, so a user id can never carry a cursor row.
CREATE TABLE agent_delivery_cursors (
    agent_account_id TEXT NOT NULL REFERENCES agent_accounts (account_id) ON DELETE RESTRICT,
    channel_id       TEXT NOT NULL REFERENCES channels (id) ON DELETE RESTRICT,
    -- The contiguous low-water cursor: highest messages.seq at or below which
    -- every message OWED to this agent on this channel is acked (a
    -- self-authored seq is vacuously satisfied — never a hole). Seeded
    -- to the channel head at subscribe time.
    acked_seq        BIGINT NOT NULL DEFAULT 0,
    -- Acked seqs ABOVE the contiguous cursor (out-of-order acks: steer jumps,
    -- lost live acks), bounded to a small out-of-order window; drained into
    -- acked_seq as gaps fill. Mirrors ControlAck's acked_seq + applied_above.
    above_seqs       BIGINT[] NOT NULL DEFAULT '{}',
    acked_at         TIMESTAMPTZ,
    PRIMARY KEY (agent_account_id, channel_id)
);
