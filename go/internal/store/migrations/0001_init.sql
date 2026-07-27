-- 0001_init: the compass v0.6 store of record — accounts, channel groups,
-- channels + membership, agent workspaces, conversation messages, and
-- subject-typed token hashes. Postgres is THE substrate (design.md T1); this
-- is the whole schema the Server serves from day one, applied at Server start
-- (embedded, versioned, one transaction, refuse-to-serve on failure).
--
-- Convention: text ids are server-assigned (the store generates a UUID per
-- row); FKs are ON DELETE RESTRICT so a referenced account/channel cannot be
-- orphaned out from under a message or membership row. Enums are stored as the
-- small ints the compass.v1 wire uses, with CHECK constraints pinning the
-- valid range so a bad value can never reach a row.

-- ── Accounts ────────────────────────────────────────────────────────────────
-- One row per account; the user/agent split lives in the two subtype tables
-- below, mirroring the compass.v1 Account `kind` oneof. handle is globally
-- unique (the ErrConflict source for CreateUser/CreateAgent).
CREATE TABLE accounts (
    id           TEXT PRIMARY KEY,
    handle       TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Human accounts: a permission role (0 member, 1 admin). PK is also the FK to
-- accounts, so a user row is exactly one account and cannot coexist with an
-- agent row of the same id.
CREATE TABLE user_accounts (
    account_id TEXT PRIMARY KEY REFERENCES accounts (id) ON DELETE RESTRICT,
    role       SMALLINT NOT NULL DEFAULT 0 CHECK (role IN (0, 1))
);

-- Agent accounts: an owned subtype gated by its owning user. home_channel_id
-- (RT-2) is set once the home channel is minted at CreateAgent; it is deferred
-- (nullable) only across the create transaction and NOT NULL-enforced by the
-- store's create path, since the channel and the agent are minted together.
CREATE TABLE agent_accounts (
    account_id      TEXT PRIMARY KEY REFERENCES accounts (id) ON DELETE RESTRICT,
    owner_user_id   TEXT NOT NULL REFERENCES user_accounts (account_id) ON DELETE RESTRICT,
    home_channel_id TEXT
);

CREATE INDEX agent_accounts_owner_idx ON agent_accounts (owner_user_id);

-- ── Channel groups ──────────────────────────────────────────────────────────
-- Namespace nodes. parent_group_id nests them (NULL = a top-level root);
-- owner_user_id is the user whose space this is (empty string for a
-- shared/global group). visibility is the group's own value (0 owner, 1 shared);
-- the store enforces child ≤ parent and computes effective visibility on read.
CREATE TABLE channel_groups (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    parent_group_id TEXT REFERENCES channel_groups (id) ON DELETE RESTRICT,
    owner_user_id   TEXT NOT NULL DEFAULT '',
    visibility      SMALLINT NOT NULL DEFAULT 0 CHECK (visibility IN (0, 1))
);

CREATE INDEX channel_groups_parent_idx ON channel_groups (parent_group_id);
CREATE INDEX channel_groups_owner_idx ON channel_groups (owner_user_id);

-- ── Channels ────────────────────────────────────────────────────────────────
-- A named conversation in a group (group_id NULL = ungrouped, owner-scoped).
-- kind is 0 channel / 1 DM / 2 GROUP_DM. Membership lives in channel_members.
CREATE TABLE channels (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    group_id   TEXT REFERENCES channel_groups (id) ON DELETE RESTRICT,
    kind       SMALLINT NOT NULL DEFAULT 0 CHECK (kind IN (0, 1, 2)),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX channels_group_idx ON channels (group_id);

-- A channel name is unique within its group, so name-based navigation inside a
-- group is unambiguous (the ErrConflict contract in errors.go). Partial on
-- group_id IS NOT NULL: ungrouped channels (group_id NULL, owner-scoped) share
-- no group namespace to collide in, and SQL NULLs are distinct under a plain
-- unique index anyway, so they are deliberately exempt.
CREATE UNIQUE INDEX channels_group_name_key ON channels (group_id, name) WHERE group_id IS NOT NULL;

-- Channel membership: the (channel, account) party set plus the per-member
-- subscribed flag (RT-1). Composite PK makes each account at most one member
-- row per channel. Both directions are indexed: by channel (list a channel's
-- members) and by account (the visible-channels query for a caller).
CREATE TABLE channel_members (
    channel_id TEXT NOT NULL REFERENCES channels (id) ON DELETE RESTRICT,
    account_id TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    subscribed BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (channel_id, account_id)
);

CREATE INDEX channel_members_account_idx ON channel_members (account_id);

-- ── Agent workspaces ────────────────────────────────────────────────────────
-- The observation pane for one agent (superseded decision 4): no participant
-- list — access is a projection of home-channel membership (fork f). One
-- workspace per agent (UNIQUE), created idempotently on first OpenAgentWorkspace.
CREATE TABLE agent_workspaces (
    id               TEXT PRIMARY KEY,
    agent_account_id TEXT NOT NULL UNIQUE REFERENCES agent_accounts (account_id) ON DELETE RESTRICT
);

-- ── Messages ────────────────────────────────────────────────────────────────
-- The durable conversation (superseded decision 4): channel-only container
-- (OQ-C), blocks narrowed to text + ask (OQ-A). blocks is the ordered content
-- as JSONB (round-trips the MessageBlock oneof); text_content is the
-- concatenation of the text blocks, maintained by the store, over which the
-- generated tsvector drives full-text SearchMessages. client_request_id is the
-- optional idempotency key (unique per author when non-empty).
CREATE TABLE messages (
    id                TEXT PRIMARY KEY,
    channel_id        TEXT NOT NULL REFERENCES channels (id) ON DELETE RESTRICT,
    author_account_id TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    at_unix_ms        BIGINT NOT NULL,
    seq               BIGSERIAL NOT NULL,
    blocks            JSONB NOT NULL,
    text_content      TEXT NOT NULL DEFAULT '',
    client_request_id TEXT NOT NULL DEFAULT '',
    -- The message this one replies to (threading); NULL for a root message. FK
    -- to messages so a reply cannot name a nonexistent parent; ON DELETE
    -- RESTRICT keeps a parent alive while replies reference it.
    parent_message_id TEXT REFERENCES messages (id) ON DELETE RESTRICT,
    search_tsv        TSVECTOR GENERATED ALWAYS AS (to_tsvector('english', text_content)) STORED
);

-- Newest-first paging within a channel keys on the monotonic seq (a stable
-- total order even when two messages share at_unix_ms), so ListMessages pages
-- deterministically.
CREATE INDEX messages_channel_seq_idx ON messages (channel_id, seq DESC);

-- Full-text search index (design.md:1137-1139): GIN over the generated tsvector.
CREATE INDEX messages_search_idx ON messages USING GIN (search_tsv);

-- Idempotency: at most one stored message per (author, client_request_id) when
-- the key is supplied, so a retried PostMessage returns the stored row instead
-- of duplicating (comms.proto:470-474). Partial — empty keys are not deduped.
CREATE UNIQUE INDEX messages_idem_idx
    ON messages (author_account_id, client_request_id)
    WHERE client_request_id <> '';

-- ── Token hashes ────────────────────────────────────────────────────────────
-- Subject-typed token store (design.md:1175-1183): the SHA-256 hash is the PK
-- (the plaintext token is returned once and never stored). subject_kind is
-- 0 account / 1 runner; subject_id spans both id spaces. revoked_at is set on
-- RevokeToken so ResolveTokenHash can distinguish revoked from never-issued.
CREATE TABLE tokens (
    hash         BYTEA PRIMARY KEY,
    subject_kind SMALLINT NOT NULL CHECK (subject_kind IN (0, 1)),
    subject_id   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at   TIMESTAMPTZ
);

CREATE INDEX tokens_subject_idx ON tokens (subject_kind, subject_id);
