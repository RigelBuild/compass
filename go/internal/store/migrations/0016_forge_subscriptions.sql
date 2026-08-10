-- ── Forge subscriptions & fetch cursors ──────────────────────────────────────
-- The DL-053 forge-poll fetch machinery (SEA-1810, design
-- docs/designs/product/compass-forge-poll-driver/design.md §T2). Four tables
-- land unconditionally (OQ-D1 resolved land-now):
--
--   forge_repo_subscriptions  — the board's per-REPO poll targets (OQ-C, new)
--   forge_list_cursors        — the repo-LIST per-page FETCH cursor (this slice's writer)
--   agent_forge_subscriptions — DL-053's forge_subscriptions, RENAMED (OQ-C); writer-less here
--   forge_artifact_cursors    — DL-053's per-artifact FETCH cursor; writer-less here
--
-- Coordinate alignment to the 0013 issue convention (0013_issues.sql:29-35):
-- SMALLINT provider enum + forge_host in every key. Every provider CHECK admits
-- the full declared enum IN (1, 2, 3, 4) (OQ-D2): the CHECK's job is "never
-- UNSPECIFIED(0)", not gating rollout — rollout is gated by which forge.Provider
-- has a real client (GitHub only, this slice). The proto declares all four
-- (FORGE_PROVIDER_GITHUB=1 .. FORGE_PROVIDER_LINEAR=4).
--
-- Convention (0001_init.sql:7-11, 0006_delivery_cursors.sql:6-9): text ids, FK
-- ON DELETE RESTRICT so a referenced agent cannot be orphaned out from under a
-- subscription row.

-- The board's repo-level poll targets (OQ-C, the table model): one row per
-- (provider, host, repo) the poll driver walks. enabled=FALSE soft-disables a
-- target without deleting its cursor history. Populated in v1 by the T4 boot
-- seed reconcile (--forge-repos, bootstrap-only insert: ON CONFLICT DO NOTHING —
-- the table is authoritative after the first insert); a mutation RPC/admin
-- surface is a named non-goal of this slice. For GITHUB the repo string is
-- lowercased at the seed/upsert boundary (GitHub owner/name is
-- case-insensitive-but-case-preserving, so Owner/Name and owner/name must NOT
-- mint two PK rows -> two poll targets -> two issues rows under the 0013
-- coordinate). updated_at is touched on every upsert/enable-flip, giving an
-- operator a timestamp to correlate a state change against (the audit posture of
-- advanced_at on forge_list_cursors).
CREATE TABLE forge_repo_subscriptions (
    forge_provider SMALLINT NOT NULL CHECK (forge_provider IN (1, 2, 3, 4)),
    forge_host     TEXT     NOT NULL,
    repo           TEXT     NOT NULL,
    enabled        BOOLEAN  NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (forge_provider, forge_host, repo)
);

-- DL-053's forge_subscriptions, renamed agent_forge_subscriptions (OQ-C) and
-- coordinate-aligned; columns otherwise per the spec'd DDL
-- (compass-server-ownership-layer/design.md:978-999). Writer-less this slice —
-- the agent-notification slice brings its writers.
CREATE TABLE agent_forge_subscriptions (
    id               TEXT PRIMARY KEY,
    agent_account_id TEXT NOT NULL REFERENCES agent_accounts (account_id) ON DELETE RESTRICT,
    forge_provider   SMALLINT NOT NULL CHECK (forge_provider IN (1, 2, 3, 4)),
    forge_host       TEXT NOT NULL,
    repo             TEXT NOT NULL,
    kind             SMALLINT NOT NULL CHECK (kind IN (1, 2)),
    number           BIGINT NOT NULL,
    delivered_revision TEXT NOT NULL DEFAULT '',
    delivered_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (agent_account_id, forge_provider, forge_host, repo, kind, number)
);
CREATE INDEX agent_forge_subscriptions_artifact_idx
    ON agent_forge_subscriptions (forge_provider, forge_host, repo, kind, number);

-- DL-053's per-artifact FETCH cursor, coordinate-aligned; columns otherwise per
-- the spec'd DDL (compass-server-ownership-layer/design.md:1007-1019).
-- Writer-less this slice — PR-C / the agent-notification slice bring its
-- writers; its DL-053-spec'd garbage-collection ("collected when the artifact's
-- last subscription is deleted") is likewise a writer-slice concern, deferred
-- with the writers.
CREATE TABLE forge_artifact_cursors (
    forge_provider SMALLINT NOT NULL CHECK (forge_provider IN (1, 2, 3, 4)),
    forge_host     TEXT NOT NULL,
    repo           TEXT NOT NULL,
    kind           SMALLINT NOT NULL CHECK (kind IN (1, 2)),
    number         BIGINT NOT NULL,
    etag           TEXT NOT NULL DEFAULT '',   -- issue/PR endpoint
    comments_etag  TEXT NOT NULL DEFAULT '',   -- comments endpoint
    checks_etag    TEXT NOT NULL DEFAULT '',   -- check-runs endpoint (PRs only)
    revision       TEXT NOT NULL DEFAULT '',
    snapshot       JSONB,                      -- last observed state, for DetectChanges
    polled_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (forge_provider, forge_host, repo, kind, number)
);

-- FETCH cursor for a repo-level issue LIST walk: one row per (coordinate, page).
-- The repo-LIST analogue of forge_artifact_cursors — a durable conditional-GET
-- cache, one writer (the poll driver), N read-side consumers. etag advances ONLY
-- after every row of that page's content is durably sunk (the projection upsert
-- is the snapshot store — see the record's Idempotency constraint). has_next
-- persists the Link-chain fact so a 304 (which need not re-send Link) can keep
-- walking a multi-page repo. advanced_at records the last content advance (an
-- etag-storing 200+sink), NOT the last poll: an all-304 tick reads the page but
-- rewrites no row, so the column deliberately names "last change".
CREATE TABLE forge_list_cursors (
    forge_provider SMALLINT NOT NULL CHECK (forge_provider IN (1, 2, 3, 4)),
    forge_host     TEXT     NOT NULL,
    repo           TEXT     NOT NULL,
    page           INTEGER  NOT NULL CHECK (page >= 1),
    etag           TEXT     NOT NULL DEFAULT '',
    has_next       BOOLEAN  NOT NULL DEFAULT FALSE,
    advanced_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (forge_provider, forge_host, repo, page)
);
