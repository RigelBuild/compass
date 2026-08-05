-- ── Issues: the durable board issue ──────────────────────────────────────────
-- The store-of-record for a Compass board issue (SEA-1728, DL-019): the
-- forge-derived facts a poll ingests, plus the Compass-owned machinery a board
-- item carries. Durable in Postgres so a Server restart rehydrates the board
-- from here rather than re-deriving it from the forge.
--
-- The forge coordinate (forge_provider, forge_host, repo, number) is the
-- IDEMPOTENCY KEY, enforced by the UNIQUE index below. A re-poll of the same
-- coordinate UPDATES the existing row rather than minting a second board item
-- (#1018 "one board item keyed by its Compass issue id, not two"). The id is a
-- surrogate minted once on first insert and never derived from the coordinate,
-- so it stays stable across every re-poll and is the join key other tables use.
--
-- state DEFAULT BACKLOG(1): a freshly ingested issue enters the board in
-- Backlog until promoted (#1018 two-population board; promote-from-backlog is
-- the entry). CHECK (state BETWEEN 1 AND 8) says a persisted issue ALWAYS has a
-- real lifecycle — it is NEVER UNSPECIFIED(0), the proto zero. The forge-only
-- upsert never touches state, so a human-set lifecycle survives a re-poll.
--
-- The machinery columns (priority, assignee, summary, branch) default empty and
-- get their writers in later slices (assignee = Dispatcher, summary = events,
-- branch = VCS, priority = tracker-ingest); this slice writes only state, and
-- only through SetIssueState. tracker and prs are DELIBERATELY absent here —
-- PR ingestion and the write-path tracker mirror own their storage and add it
-- in their own slices.
CREATE TABLE issues (
    id             TEXT PRIMARY KEY,

    -- forge coordinate: the idempotency key. A re-poll of the same coordinate
    -- updates the existing row rather than minting a second board item (#1018
    -- "one board item keyed by its Compass issue id, not two").
    forge_provider SMALLINT NOT NULL CHECK (forge_provider IN (1, 2, 3)),  -- GitHub/GitLab/Forgejo; never UNSPECIFIED(0) — every issue is forge-backed with a real provider
    forge_host     TEXT     NOT NULL,
    repo           TEXT     NOT NULL,
    number         BIGINT   NOT NULL,  -- canonical uint32; BIGINT holds the clamped MaxUint32

    -- forge fields (translated at ingestion; owner header already stripped)
    title          TEXT     NOT NULL DEFAULT '',
    body           TEXT     NOT NULL DEFAULT '',
    forge_state    TEXT     NOT NULL DEFAULT '',
    url            TEXT     NOT NULL DEFAULT '',
    forge_account  TEXT     NOT NULL DEFAULT '',
    labels         TEXT[]   NOT NULL DEFAULT '{}',
    agent_handle   TEXT     NOT NULL DEFAULT '',  -- '' = non-Compass author

    -- Compass machinery (server-owned; none on the forge). state defaults to
    -- BACKLOG: a freshly ingested issue enters the board in Backlog until
    -- promoted (#1018 two-population board; promote-from-backlog is the entry).
    -- CHECK 1..8: a persisted issue is NEVER UNSPECIFIED(0).
    state          SMALLINT NOT NULL DEFAULT 1 CHECK (state BETWEEN 1 AND 8),
    priority       TEXT     NOT NULL DEFAULT '',
    assignee       TEXT     NOT NULL DEFAULT '',
    summary        TEXT     NOT NULL DEFAULT '',
    branch         TEXT     NOT NULL DEFAULT ''
);

-- The idempotency key: one board item per forge coordinate.
CREATE UNIQUE INDEX issues_coordinate_key
    ON issues (forge_provider, forge_host, repo, number);
