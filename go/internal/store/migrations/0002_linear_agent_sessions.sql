-- 0002_linear_agent_sessions: the Linear Agent Session ↔ Compass conversation
-- association (compass-linear-agent-responder design.md §Part 2 / §T3). One row
-- per Linear AgentSession the responder has handled: the resolved Manager, that
-- Manager's home channel, the comms topic the delegated conversation landed in,
-- and the issue it was delegated on (provenance). Read on a `prompted` event to
-- route the follow-up to the same topic (LinearAgentSession); written on
-- `created` (UpsertLinearAgentSession, ON CONFLICT DO NOTHING).
--
-- NO dedup column: message-level dedup is the comms rail's client_request_id
-- (keyed on the Linear-Delivery UUID, §Part 1), not this table's concern. The
-- association insert is itself idempotent on the linear_session_id PK, so a
-- replayed `created` re-lands on the key rather than forking a second row.
--
-- Convention (0001_init): text ids are server/forge-assigned; created_at is a
-- TIMESTAMPTZ DEFAULT now() birth marker. No FKs: manager_account_id, channel_id
-- and topic_id name live Compass rows, but the association is written from the
-- webhook path against ids the responder just resolved, and a Manager/channel/
-- topic teardown must not be blocked by a stale Linear association — so these
-- are unconstrained id columns, matching the schema in the record (§Part 2).
CREATE TABLE linear_agent_sessions (
    linear_session_id  TEXT PRIMARY KEY,               -- Linear AgentSession.id
    manager_account_id TEXT NOT NULL,                  -- the resolved Compass Manager
    channel_id         TEXT NOT NULL,                  -- the Manager's home channel
    topic_id           TEXT NOT NULL,                  -- comms topic of the conversation
    linear_issue_id    TEXT,                           -- provenance (issue delegated on); NULL if none
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
