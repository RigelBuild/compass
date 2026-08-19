-- 0002_forge_authored_artifacts: the DL-055 forge ownership index. One row per
-- forge artifact Compass AUTHORED on behalf of an agent — the coordinate the
-- write path minted, who authored it, and the F3 idempotency memo. The write
-- chokepoint (T4) writes the row AND the memo in a single statement on a create
-- success; a provider error writes NOTHING (no row, no memo).
--
-- Convention (mirrors 0001_init): the SMALLINT provider enum + forge_host in
-- the key, FK ON DELETE RESTRICT so a referenced account cannot be orphaned out
-- from under an ownership row, and a provider CHECK IN (1,2,3,4) whose job is
-- "never UNSPECIFIED(0)", not rollout gating.
--
-- (agent_account_id, owner_user_id) is a COMPOSITE FK into agent_accounts, so
-- the pair is schema-guaranteed to be a real (agent, that-agent's-owner) pair —
-- the store cannot record an agent under a user who is not its owner. This needs
-- a UNIQUE(account_id, owner_user_id) target on agent_accounts (account_id alone
-- is the PK, but a composite FK requires a unique constraint on the exact
-- referenced column list); it subsumes the plain agent_account_id FK, and the
-- transitive owner_user_id -> user_accounts FK already rides on agent_accounts.
ALTER TABLE agent_accounts ADD CONSTRAINT agent_accounts_id_owner_key UNIQUE (account_id, owner_user_id);

-- PK is the forge coordinate (provider, host, repo, kind, number) — the same
-- coordinate shape forge_artifact_cursors keys on. A retry of the same authored
-- create idempotently re-lands on this key (ON CONFLICT upsert). kind CHECK
-- IN (1, 2): 1=issue, 2=pull_request, matching agent_forge_subscriptions.kind.
--
-- client_request_id is NULLABLE: NULL when the caller supplied no idempotency
-- key. The UNIQUE PARTIAL index on (agent_account_id, client_request_id) WHERE
-- client_request_id IS NOT NULL is the F3 memo — it dedups a per-agent retry
-- carrying the same key, while NULL-key rows never collide (a NULL is distinct
-- from every other NULL under the partial index's WHERE filter).
CREATE TABLE forge_authored_artifacts (
    forge_provider     SMALLINT NOT NULL CHECK (forge_provider IN (1, 2, 3, 4)),
    forge_host         TEXT     NOT NULL,
    repo               TEXT     NOT NULL,
    kind               SMALLINT NOT NULL CHECK (kind IN (1, 2)),
    number             BIGINT   NOT NULL,  -- canonical uint64
    agent_account_id   TEXT     NOT NULL,
    owner_user_id      TEXT     NOT NULL,
    session_id         TEXT     NOT NULL DEFAULT '',
    client_request_id  TEXT,  -- NULL = caller supplied no idempotency key (F3)
    created_at_unix_ms BIGINT   NOT NULL,
    PRIMARY KEY (forge_provider, forge_host, repo, kind, number),
    -- Composite FK: the pair must be a real (agent, that-agent's-owner). ON
    -- DELETE RESTRICT so a referenced agent/owner cannot be orphaned out from
    -- under an ownership row.
    FOREIGN KEY (agent_account_id, owner_user_id)
        REFERENCES agent_accounts (account_id, owner_user_id) ON DELETE RESTRICT
);

-- The F3 memo: a per-agent idempotency key is unique across the agent's
-- authored artifacts. Partial so NULL-key rows (no key supplied) never collide.
CREATE UNIQUE INDEX forge_authored_artifacts_request_memo_idx
    ON forge_authored_artifacts (agent_account_id, client_request_id)
    WHERE client_request_id IS NOT NULL;

-- By-agent scan (ListAuthoredArtifactsByAgent): every artifact one agent authored.
CREATE INDEX forge_authored_artifacts_agent_idx
    ON forge_authored_artifacts (agent_account_id);
