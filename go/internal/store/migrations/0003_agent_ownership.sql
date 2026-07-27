-- 0003_agent_ownership: the durable session-ownership chain (SEA-1342 T-P2).
-- Two mapping tables that let SubscribeAgentSession resolve a session_id to the
-- home channel it must authorize the caller against — persisted, not in-memory,
-- so the resolution survives a Server restart (the in-memory RunnerHub
-- enrollment does not; a durable authz boundary cannot depend on it).
--
-- The chain is session_id -> container_name -> agent_account_id ->
-- home_channel_id. It is rooted non-spoofably: agent_account_id is the
-- ProvisionAgentWorkspace REQUEST field, but container_name and session_id are
-- SERVER-MINTED RESPONSE values (compass.proto ProvisionAgentWorkspaceResponse
-- / StartAgentSessionResponse), written only after the Runner call succeeds, so
-- a row never claims a container/session the Runner failed to create and a
-- client cannot forge a mapping it does not own.
--
-- Convention (0001_init.sql:7-11): text ids, FK ON DELETE RESTRICT so a
-- referenced agent/container cannot be orphaned out from under a mapping.

-- container_name -> agent_account_id, written at ProvisionAgentWorkspace (the
-- one hop where the agent identity is known: agent_account_id is on the request,
-- container_name on the response). One container belongs to exactly one agent
-- and never changes owner, so the ProvisionAgentWorkspace write is idempotent
-- (ON CONFLICT (container_name) DO NOTHING) to match the client_request_id retry
-- contract (compass.proto: a retried provision returns the same container_name).
CREATE TABLE agent_containers (
    container_name   TEXT PRIMARY KEY,
    agent_account_id TEXT NOT NULL REFERENCES agent_accounts (account_id) ON DELETE RESTRICT
);

CREATE INDEX agent_containers_agent_idx ON agent_containers (agent_account_id);

-- session_id -> container_name, written at StartAgentSession (where both are
-- known: container_name on the request, session_id minted on the response). The
-- FK to agent_containers means a session cannot bind to a container the Server
-- never provisioned — the chain is complete or it does not exist.
CREATE TABLE agent_sessions (
    session_id     TEXT PRIMARY KEY,
    container_name TEXT NOT NULL REFERENCES agent_containers (container_name) ON DELETE RESTRICT
);

CREATE INDEX agent_sessions_container_idx ON agent_sessions (container_name);
