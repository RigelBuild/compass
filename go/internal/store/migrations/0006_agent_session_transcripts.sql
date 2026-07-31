-- 0006_agent_session_transcripts: the transcript pointer, where a session's
-- S3-persisted log lives (SEA-1570 session persistence).
--
-- One row per STABLE logical session_id, written at StartAgentSession success
-- beside agent_sessions; resume resolves it back. Bucket + key PREFIX are stored
-- (not a single object key) because the log is a per-epoch segment set under the
-- prefix. endpoint is nullable transcript provenance: which S3 endpoint holds
-- the segments, so a later second-Runner / R2 deployment can resolve the row
-- (NULL = the deployment's default endpoint). Storing it now, not later, is what
-- keeps "shared/regional S3 is config not schema" true.
--
-- Convention (0004_agent_placement.sql:32-34): text ids, FK ON DELETE RESTRICT
-- so a referenced session cannot be orphaned out from under its pointer. The FK
-- targets agent_sessions(session_id), the PK carried since 0004.
CREATE TABLE agent_session_transcripts (
    session_id TEXT PRIMARY KEY REFERENCES agent_sessions (session_id) ON DELETE RESTRICT,
    bucket     TEXT NOT NULL,
    prefix     TEXT NOT NULL,
    endpoint   TEXT
);
