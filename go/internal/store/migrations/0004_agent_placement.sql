-- 0004_agent_placement: collapse the container hop out of the session-ownership
-- chain, and record which Runner each agent is placed on (SEA-1516 reattach).
--
-- Two changes, one motivation: agent_containers sat in the middle of a security
-- boundary to carry a fact that is not authorization, while the fact reattach
-- actually needs was stored nowhere.
--
--   * agent_containers held container_name -> agent_account_id, and its ONLY
--     read anywhere was the SubscribeAgentSession authz JOIN — where it could
--     never do more than pass the account through (container_name PK, NOT NULL
--     FK to the account: a 1:1 hop). So agent_sessions now points at the agent
--     account DIRECTLY and the chain shortens from
--     session_id -> container_name -> agent_account_id -> home_channel_id to
--     session_id -> agent_account_id -> home_channel_id. The name/account
--     mapping itself is not lost — it moves to agent_placements below, out of
--     the authz path and into the operational state where it belongs.
--
--   * NOTHING recorded which Runner an agent runs on. After a Runner restart
--     its surviving containers are orphaned, and the Server must re-drive
--     Provision for exactly the agents that Runner held — a set it could not
--     name. agent_placements records it, alongside the container name that
--     re-drive needs.
--
-- The authz semantics are UNCHANGED. Shortening the chain removes a hop that
-- could only ever be a 1:1 pass-through (container_name PK, NOT NULL FK to the
-- account), so the set of (session, caller) pairs the JOIN authorizes is
-- identical; only the table count differs. The rooting argument from 0003 also
-- survives: session_id is still the SERVER-MINTED StartAgentSession response,
-- written only after the Runner call succeeds, so a row still never claims a
-- session the Runner failed to create.
--
-- Convention (0001_init.sql:7-11, 0003_agent_ownership.sql:15-16): text ids, FK
-- ON DELETE RESTRICT so a referenced agent cannot be orphaned out from under a
-- mapping.

-- ── agent_sessions: session_id -> agent_account_id ──────────────────────────
-- Added nullable, backfilled through the old chain so no existing session row
-- loses its ownership, then tightened to NOT NULL. Doing it in that order (not
-- a bare NOT NULL add) is what makes the migration non-destructive on a
-- database that already has sessions.
ALTER TABLE agent_sessions
    ADD COLUMN agent_account_id TEXT REFERENCES agent_accounts (account_id) ON DELETE RESTRICT;

UPDATE agent_sessions se
   SET agent_account_id = c.agent_account_id
  FROM agent_containers c
 WHERE c.container_name = se.container_name;

ALTER TABLE agent_sessions
    ALTER COLUMN agent_account_id SET NOT NULL;

-- Dropping the column takes its FK and agent_sessions_container_idx with it.
ALTER TABLE agent_sessions DROP COLUMN container_name;

-- The equivalent of the old agent_sessions_container_idx: look a session up by
-- the party that owns it. Also the "sessions of this agent" direction reattach
-- reads once it knows which agents a Runner held.
CREATE INDEX agent_sessions_agent_idx ON agent_sessions (agent_account_id);

-- ── agent_placements: where each agent runs, and under what name ────────────
-- Operational placement state, written at ProvisionAgentWorkspace — the one hop
-- where every fact is in hand: agent_account_id is the Server's own request
-- field, container_name is the Runner's response, and runner_id is the Runner
-- the Server relayed to. Before this the Server learned that triple and
-- remembered it only in RAM (runnerhub's in-memory container->account binding),
-- so a Server restart or a Runner re-enroll lost it.
--
-- Deliberately NOT part of the authz chain. SubscribeAgentSession authorizes
-- through agent_sessions -> agent_accounts -> channel_members and never reads
-- this table; placement is where an agent runs, not who may watch it. Keeping
-- the two apart is what stops the container hop 0003 introduced from growing
-- back into the security boundary.
--
-- PK on the agent, not a surrogate: an agent is on AT MOST ONE Runner under one
-- container name, so a re-provision REPLACES the row (upsert on
-- agent_account_id, updating runner_id AND container_name together) rather than
-- accumulating a second placement that would make "which Runner owns this
-- agent, under which name" ambiguous — the exact ambiguity reattach must not
-- face, and the reason a stale container_name alongside a fresh runner_id would
-- be worse than no row at all.
--
-- runner_id is deliberately NOT a FK: Runners are enrolled in memory under a
-- token subject (store.SubjectRunner), with no runners table to reference, and
-- a placement must OUTLIVE the Runner's attachment — that it survives the
-- Runner going away is the whole point of recording it.
--
-- This table is created BEFORE agent_containers is dropped, because the
-- backfill below reads it. Order is load-bearing, and the whole file is one
-- transaction, so the create/backfill/drop is atomic.
CREATE TABLE agent_placements (
    agent_account_id TEXT PRIMARY KEY REFERENCES agent_accounts (account_id) ON DELETE RESTRICT,
    runner_id        TEXT NOT NULL,
    container_name   TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The reattach query is "every agent on this Runner", so runner_id is the read
-- direction that needs the index (the agent direction is already the PK).
CREATE INDEX agent_placements_runner_idx ON agent_placements (runner_id);

-- StartAgentSession arrives holding only container_name (the frozen
-- StartAgentSessionRequest field), so it resolves the owning account by this
-- index before recording the session. UNIQUE because the mapping is 1:1 — a
-- container belongs to exactly one agent — and enforcing it here means a
-- second agent can never claim a live container name, which would let Start
-- record a session under the wrong owner and hand the authz JOIN the wrong
-- home channel.
CREATE UNIQUE INDEX agent_placements_container_key ON agent_placements (container_name);

-- ── Backfill: every pre-upgrade container keeps working ─────────────────────
-- Without this, the upgrade BREAKS every already-provisioned container:
-- StartAgentSession now resolves its owner through AgentForContainer
-- (agent_placements.go), so a container with no placement row is permanently
-- un-Startable — and the mapping it would need is right here in
-- agent_containers, about to be dropped.
--
-- runner_id = '' is the deliberate UNKNOWN-RUNNER SENTINEL, not a placeholder
-- we tolerate. agent_containers recorded no Runner, and we genuinely do not
-- know which one held the container, so the row must be readable in the one
-- direction where the answer is known and invisible in the one where it is not:
--
--   * AgentForContainer resolves it (it reads only container_name), so Start
--     keeps working for every pre-upgrade container. This is the break we fix.
--   * ListAgentPlacementsForRunner CANNOT return it: that read rejects an empty
--     runner id outright (agent_placements.go ErrInvalidArgument guard), and no
--     enrolled Runner has an empty id, so a backfilled row is never re-driven
--     against a Runner we only guessed at. Exactly right — a wrong reattach is
--     worse than none.
--
-- Self-healing: the next ProvisionAgentWorkspace upserts on agent_account_id
-- and overwrites '' with the Runner that actually served the call, so the
-- sentinel is transient per agent. runner_id therefore stays NOT NULL ('' being
-- a value that satisfies it) rather than becoming nullable — nullable would
-- widen the column's contract permanently to encode a one-time migration state.
--
-- ON CONFLICT DO NOTHING: agent_containers keyed on container_name, so one
-- agent could in principle hold several containers while a placement is one per
-- agent (PK). Keeping the first rather than failing the migration is right —
-- any of them restores Start, and the next provision replaces it with truth.
INSERT INTO agent_placements (agent_account_id, runner_id, container_name)
SELECT c.agent_account_id, '', c.container_name
  FROM agent_containers c
ON CONFLICT DO NOTHING;

-- Superseded, not merely redundant: agent_placements above records the same
-- container_name -> agent_account_id fact and MORE (which Runner, and when),
-- keyed on the agent rather than the name — and the backfill has just carried
-- every existing mapping across. Dropping the table drops
-- agent_containers_agent_idx with it.
DROP TABLE agent_containers;
