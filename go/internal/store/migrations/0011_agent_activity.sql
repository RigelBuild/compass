-- ── Agent activity ───────────────────────────────────────────────────────────
-- The durable store-of-record for an agent's free-text activity string (the
-- "what am I doing right now" line the agent-set roster renders). Presence — the
-- online/away/offline ENUM — stays in-memory per DL-074: it is a property of a
-- live connection and is meaningless across a restart, so it is deliberately NOT
-- persisted. The activity string is different: it is a durable statement the
-- agent authored, so it is DB-backed and a Server restart recovers it from here
-- rather than blanking it (design.md Global Constraints :305-309, DL-074).
--
-- One row per agent, keyed by its account id. ON DELETE RESTRICT is deliberate
-- and mirrors the agent tree's FK discipline: an agent with a recorded activity
-- cannot be deleted out from under this row — the activity is cleared through
-- the store, not by cascade.
CREATE TABLE agent_activity (
    agent_account_id    TEXT PRIMARY KEY REFERENCES agent_accounts (account_id) ON DELETE RESTRICT,
    activity            TEXT NOT NULL,
    activity_at_unix_ms BIGINT NOT NULL
);
