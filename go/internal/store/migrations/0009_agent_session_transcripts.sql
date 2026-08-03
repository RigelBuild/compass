-- 0009_agent_session_transcripts: the durable TWO-TIER transcript store
-- (SEA-1667 T4, the server half of SEA-1570 session persistence). A Postgres
-- HOT TAIL holding [latest checkpoint .. now] = the normal resume set, plus a
-- manifest of the object-store COLD ARCHIVE (verbatim JSONL segments) that holds
-- superseded/evicted/ended history. Resume reads the PG hot-tail ONLY in normal
-- operation (T5); the archive is the permanent, analytics-ready history.
--
-- Both tables are FK-rooted in agent_sessions (0003/0004) ON DELETE RESTRICT, so
-- a transcript row can never be orphaned out from under its session — the same
-- convention as every other agent_* mapping (0001_init.sql:7-11,
-- 0003_agent_ownership.sql:15-16).

-- HOT TAIL. Holds only [latest checkpoint .. now] = the resume set, not the
-- whole history: superseded entries are flushed to the object store and PRUNED
-- from this table (only at flush, never on teardown). entry_seq is
-- SESSION-scoped and monotonic across container lifetimes (the server rebases
-- each lifetime's agent-stamped sequence onto the session's stored maximum at
-- lifetime bind — see agent_sessions.base_entry_seq below); idempotency_key
-- carries the durable lane's at-most-once guarantee into this table (a duplicate
-- key is a silent retry dedup). The UNIQUE is GLOBAL, not (session_id,
-- idempotency_key) scoped like the comms Message path (0001_init.sql:133-138):
-- the agent mints each key from a per-sink random nonce + a monotonic counter
-- (design.md:135-136), so a key is unique across sessions AND agent restarts by
-- construction. A per-session scope would be strictly weaker here — a
-- non-globally-unique key scheme would silently drop a colliding entry via the
-- ON CONFLICT DO NOTHING dedup, so global uniqueness is the load-bearing
-- invariant that keeps that dedup collision-free. checkpoint marks a full-body
-- snapshot that supersedes all prior entries for the session (the read view
-- starts at the latest checkpoint).
CREATE TABLE agent_session_transcript_entries (
    session_id      TEXT   NOT NULL REFERENCES agent_sessions (session_id) ON DELETE RESTRICT,
    entry_seq       BIGINT NOT NULL,
    checkpoint      BOOLEAN NOT NULL DEFAULT FALSE,
    entry_json      TEXT   NOT NULL,
    idempotency_key TEXT   NOT NULL UNIQUE,
    PRIMARY KEY (session_id, entry_seq)
);

-- ARCHIVE MANIFEST. One row per flushed object-store segment (verbatim JSONL).
-- kind='superseded' segments hold pre-checkpoint history and are NEVER read on
-- resume; kind='safety_valve' segments hold entries AFTER the latest checkpoint
-- evicted by the high size-cap and ARE spliced back on resume (T5) — a later
-- checkpoint re-marks any now-pre-checkpoint safety_valve row to 'superseded'
-- (the PRIMARY flush's manifest UPDATE), so a safety_valve row is
-- post-latest-checkpoint by construction; kind='session_end' segments archive
-- the retained post-checkpoint tail at teardown for analytics completeness and
-- are NEVER read on resume (the PG tail stays authoritative). The object key is
-- prefixed sessions/<session_id>/; bucket/endpoint are server config, not
-- per-row.
CREATE TABLE agent_session_archive_segments (
    session_id    TEXT   NOT NULL REFERENCES agent_sessions (session_id) ON DELETE RESTRICT,
    object_key    TEXT   NOT NULL,
    min_entry_seq BIGINT NOT NULL,
    max_entry_seq BIGINT NOT NULL,
    kind          TEXT   NOT NULL CHECK (kind IN ('superseded', 'safety_valve', 'session_end')),
    PRIMARY KEY (session_id, object_key)
);

-- The write-once per-lifetime rebase base. The wire entry_seq is agent-stamped,
-- monotonic from 1 PER CONTAINER LIFETIME (the agent has no durable memory of
-- prior lifetimes). At lifetime bind the server snapshots
-- base = max(entry_seq) over this session's transcript rows ONCE onto this
-- column (BindLifetime), and persists each incoming frame at
-- base + frame.entry_seq — read from this row per frame, never recomputed. So
-- the persisted entry_seq is monotonic per SESSION across lifetimes and the PK
-- (session_id, entry_seq) holds across resumes. DEFAULT 0: a brand-new session's
-- first lifetime rebases onto 0, so its first frame lands at entry_seq 1.
ALTER TABLE agent_sessions ADD COLUMN base_entry_seq BIGINT NOT NULL DEFAULT 0;
