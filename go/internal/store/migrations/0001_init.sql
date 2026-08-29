-- 0001_init: the WHOLE compass v0.6 store of record, as a single squashed
-- schema. This one file produces the complete schema the Server serves —
-- accounts and their user/agent subtypes, the agent tree, channel groups,
-- channels + membership + policy, topics and topic-scoped messages, the pinned
-- board, agent workspaces, delivery cursors, session ownership + placement, the
-- two-tier transcript store, the secrets names registry, the fleet config
-- bundle, board issues, the forge-poll fetch machinery, and forge
-- authored-artifact ownership.
--
-- History note: this replaces the original sequential 0001..0016 migration
-- chain PLUS the two migrations added after it (the forge authored-artifact
-- ownership table and the messages author-index), all folded back into this
-- single init. Pre-dogfood — zero users, zero deployed databases — so migration
-- history was dead weight and Matt ruled (2026-08-07) to collapse it; the same
-- reasoning folds each later migration in as it accretes. It is a schema RESET,
-- correct ONLY because no deployed DB exists to migrate; the resulting schema
-- is byte-identical (pg_dump) to applying the collapsed 0001..0016 chain and
-- those later files in order. The `schema_migrations` bookkeeping table is
-- deliberately NOT here: the Go runner creates it (store.go ensureMigrationsTable)
-- so it can record v1 itself.
--
-- Convention: text ids are server-assigned (the store generates a UUID per
-- row); FKs are ON DELETE RESTRICT so a referenced account/channel/agent cannot
-- be orphaned out from under a dependent row. Enums are stored as the small
-- ints the compass.v1 wire uses, with CHECK constraints pinning the valid range
-- so a bad value can never reach a row.
--
-- Statement order is FK order: a referenced table is created before any table
-- that references it (accounts first, then its subtypes, then everything that
-- hangs off them; topics before messages; messages before channel_pins).

-- ── Accounts ────────────────────────────────────────────────────────────────
-- One row per account; the user/agent split lives in the two subtype tables
-- below, mirroring the compass.v1 Account `kind` oneof. handle is a display
-- column kept in sync with the account's current handle (CreateUser/CreateAgent
-- populate it, a rename UPDATEs it); it is NO LONGER the resolution key — that
-- moved to account_handles below, whose partial-unique indexes express the
-- two-namespace contract (global-unique user/system handles, per-owner agent
-- handles) a single global column could not (RIG-2751 handle cutover).
CREATE TABLE accounts (
    id           TEXT PRIMARY KEY,
    handle       TEXT NOT NULL,
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
--
-- parent_agent_id (0007 agent tree): the fleet's reporting spine — who spawned
-- or supervises whom — as a model fact every surface re-derives from. Nullable,
-- NULL = a root agent (mirrors channel_groups.parent_group_id); a
-- self-referential FK ON DELETE RESTRICT so a parent cannot be orphaned out from
-- under its children. Same-owner and no-cycle are validated server-side on every
-- write, not by the schema — the FK only guarantees the referent exists and is
-- an agent account. INVARIANT: every write of parent_agent_id must invoke the
-- registered coordination hook (SEA-1722 T5) — the manager-comms coordination
-- channel is auto-provisioned/reconciled from this edge, so a writer that sets
-- it without invoking the hook (store.CreateAgent, store.ReparentAgent) leaves
-- the tree and channel state divergent.
--
-- persona (0005): the agent's system-prompt APPEND overlay, its source-of-truth
-- field on the agent account (Matt-ruled: source = AgentAccount). Empty = no
-- override. role (0015): the operator-set block-0 selector that REPLACES block-0
-- via customSystemPrompt (config/prompts/<role>/SYSTEM.md); empty = no role.
-- Both NOT NULL DEFAULT '' so every agent row always has a value (empty = the
-- no-override contract), keeping the create/read path branch-free.
CREATE TABLE agent_accounts (
    account_id      TEXT PRIMARY KEY REFERENCES accounts (id) ON DELETE RESTRICT,
    owner_user_id   TEXT NOT NULL REFERENCES user_accounts (account_id) ON DELETE RESTRICT,
    home_channel_id TEXT,
    persona         TEXT NOT NULL DEFAULT '',
    parent_agent_id TEXT REFERENCES agent_accounts (account_id) ON DELETE RESTRICT,
    role            TEXT NOT NULL DEFAULT '',
    -- Composite-FK target for forge_authored_artifacts: (account_id,
    -- owner_user_id) must be UNIQUE so a composite FK can reference the exact
    -- pair (account_id alone is the PK, but a composite FK requires a UNIQUE on
    -- the exact referenced column list). Guarantees the store cannot record a
    -- forge artifact under an (agent, user) pair that is not a real
    -- (agent, that-agent's-owner) pair. This UNIQUE subsumes the plain
    -- agent_account_id FK, and the transitive owner_user_id -> user_accounts FK
    -- already rides on agent_accounts.
    UNIQUE (account_id, owner_user_id)
);

CREATE INDEX agent_accounts_owner_idx ON agent_accounts (owner_user_id);
-- The "children of this parent" read direction for the agent tree.
CREATE INDEX agent_accounts_parent_idx ON agent_accounts (parent_agent_id);

-- System accounts: the reserved platform sender (@compass), a distinct
-- first-class subtype alongside user_accounts and agent_accounts. No payload
-- columns — the row's existence is the discriminator (there is exactly one,
-- seeded at startup by store.EnsureSystemAccount, not by this migration). PK is
-- also the FK to accounts, so a system row is exactly one account and cannot
-- coexist with a user or agent row of the same id. ON DELETE RESTRICT so the
-- reserved account cannot be orphaned out from under its subtype row.
CREATE TABLE system_accounts (
    account_id TEXT PRIMARY KEY REFERENCES accounts (id) ON DELETE RESTRICT
);

-- ── Account handles (the resolution index) ──────────────────────────────────
-- Handle→id resolution's source of truth (RIG-2751 handle cutover). One row per
-- account. owner_user_id is NULL for user and system accounts and the owning
-- user's id for agent accounts (it mirrors agent_accounts.owner_user_id), which
-- is what makes an agent handle unique only within its owner's namespace while a
-- user/system handle is globally unique. account_id is PK/FK to accounts (one
-- handle row per account); owner_user_id FKs user_accounts so an agent handle's
-- owner is a real user. Both FKs ON DELETE RESTRICT so a referenced account or
-- owner cannot be orphaned out from under a handle row.
CREATE TABLE account_handles (
    account_id    TEXT PRIMARY KEY REFERENCES accounts (id) ON DELETE RESTRICT,
    handle        TEXT NOT NULL,
    owner_user_id TEXT REFERENCES user_accounts (account_id) ON DELETE RESTRICT
);

-- The two partial-unique indexes ARE the resolution index and enforce the
-- two-namespace contract:
--   * user/system handles (owner_user_id IS NULL) are globally unique — the
--     tier that preserves today's accounts.handle global-unique invariant;
--   * agent handles (owner_user_id IS NOT NULL) are unique only per owner.
-- An agent handle MAY overlap a global user handle with no collision at resolve
-- time: a user is only ever looked up bare (first index) and an agent only ever
-- owner-qualified (second index), so the two never contend on one lookup.
CREATE UNIQUE INDEX account_handles_global_key ON account_handles (handle) WHERE owner_user_id IS NULL;
CREATE UNIQUE INDEX account_handles_owner_key ON account_handles (owner_user_id, handle) WHERE owner_user_id IS NOT NULL;

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
--
-- Channel-policy fields (0014, SEA-1722 T4): post_policy mirrors the
-- ChannelPostPolicy enum — 0 OPEN (any member may post, default), 1 OWNER_ONLY
-- (only owner_account_id may post). owner_account_id is the owner/operator
-- account for policy operations; NULL leaves the channel unowned (the only legal
-- state when OPEN). mandatory_subscription, when true, makes every member a
-- delivery target regardless of its per-member channel_members.subscribed flag
-- (the D1 read-side disjunct); SetChannelPolicy is the ONLY mutation path and
-- transactionally seeds each newly-mandatory member's delivery cursor so a flip
-- never mints an un-seeded delivery target (the fail-DANGEROUS D2 hazard). All
-- three default to the pre-substrate behavior (OPEN, unowned, opt-in).
CREATE TABLE channels (
    id                     TEXT PRIMARY KEY,
    name                   TEXT NOT NULL,
    group_id               TEXT REFERENCES channel_groups (id) ON DELETE RESTRICT,
    kind                   SMALLINT NOT NULL DEFAULT 0 CHECK (kind IN (0, 1, 2)),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    post_policy            SMALLINT NOT NULL DEFAULT 0 CHECK (post_policy IN (0, 1)),
    owner_account_id       TEXT REFERENCES accounts (id) ON DELETE RESTRICT,
    mandatory_subscription BOOLEAN NOT NULL DEFAULT FALSE
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

-- ── Topics ────────────────────────────────────────────────────────────────
-- The Zulip-style threading model (compass-zulip-threading-model design.md D2):
-- a channel's conversation is partitioned into named topics, and every message
-- lives in exactly one topic. Topics are born via a post naming a topic (there
-- is no separate CreateTopic — a topic with zero messages is not a thing), so
-- the get-or-create on (channel_id, lower(name)) is the birth path. last_seq is
-- a denormalized activity marker (the highest messages.seq under the topic),
-- maintained in the same tx as each append, so a topic index can order by
-- recency without scanning messages. archived is a tidiness flag, not a lock: a
-- post addressed at an archived name revives it (get-or-create clears archived).
-- Created before messages, which reference it.
CREATE TABLE topics (
    id                    TEXT PRIMARY KEY,
    channel_id            TEXT NOT NULL REFERENCES channels (id) ON DELETE RESTRICT,
    name                  TEXT NOT NULL,
    created_by_account_id TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    created_at_unix_ms    BIGINT NOT NULL,
    archived              BOOLEAN NOT NULL DEFAULT FALSE,
    last_seq              BIGINT NOT NULL DEFAULT 0  -- denormalized activity order
);

-- Case-insensitive uniqueness per channel is the get-or-create key: two racing
-- posts naming the same topic converge on one row via ON CONFLICT DO NOTHING +
-- re-SELECT (never a surfaced unique-violation), and a case-variant of an
-- existing name resolves into it rather than forking a duplicate.
CREATE UNIQUE INDEX topics_channel_name_idx ON topics (channel_id, lower(name));

-- ── Messages ────────────────────────────────────────────────────────────────
-- The durable conversation, in its post-threading (F10) shape: a message
-- records only its topic_id; the channel is topics.channel_id, one join away
-- (there is no channel_id or parent_message_id column — the pre-dogfood reshape
-- collapsed them, DL-098). blocks is the ordered content as JSONB (round-trips
-- the MessageBlock oneof, narrowed to text + ask); text_content is the
-- concatenation of the text blocks, maintained by the store, over which the
-- generated tsvector drives full-text SearchMessages. client_request_id is the
-- optional idempotency key (unique per author when non-empty).
--
-- seq is a table-global BIGSERIAL and therefore channel-monotonic via the topic
-- join — the stable total order (even when two messages share at_unix_ms) that
-- ListMessages pages on and the delivery cursor (D3) relies on. The column order
-- below (search_tsv before topic_id) matches the physical order the sequential
-- chain produced (topic_id was appended after the reshape dropped the two old
-- columns), keeping the schema byte-identical.
CREATE TABLE messages (
    id                TEXT PRIMARY KEY,
    author_account_id TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    at_unix_ms        BIGINT NOT NULL,
    seq               BIGSERIAL NOT NULL,
    blocks            JSONB NOT NULL,
    text_content      TEXT NOT NULL DEFAULT '',
    client_request_id TEXT NOT NULL DEFAULT '',
    search_tsv        TSVECTOR GENERATED ALWAYS AS (to_tsvector('english', text_content)) STORED,
    topic_id          TEXT NOT NULL REFERENCES topics (id) ON DELETE RESTRICT,
    -- Settle-edge mention pass marker (RIG-2490 T1): the unix-ms time the
    -- message's settle-edge mention routing pass completed. NULL means that
    -- pass never completed (a fault between commit and mark), so the message is
    -- re-scannable by the recovery pass; readers care only about NULL vs
    -- non-NULL. Unix-ms BIGINT per the schema convention (at_unix_ms above),
    -- never a SQL TIMESTAMP.
    mentions_routed_at BIGINT
);

-- Pre-settle mention-loss recovery scan (RIG-2490 T1). The scan's only query is
-- `WHERE mentions_routed_at IS NULL ORDER BY seq ASC LIMIT n`; this partial
-- index serves both the filter and the order, and in steady state holds only
-- the thin in-flight/unsettled set (nearly every row is marked). None of the
-- existing messages indexes serve it: messages_topic_seq_idx leads on topic_id,
-- messages_search_idx is a GIN on search_tsv, and the partial-unique idempotency
-- index covers other predicates. Plain (non-CONCURRENT) index — migrations run
-- inside a transaction (store.go applyMigration), so CREATE INDEX CONCURRENTLY
-- is invalid here. Seed-forward (RD-2): the squashed 0001_init.sql is the only
-- migration and pre-dogfood databases are recreated on schema change, so every
-- post-migration row was inserted with the column present — there is no
-- historical backfill. Were this ever carried into a first real incremental
-- migration, existing rows would be seeded mentions_routed_at = <migration time>
-- (so a first scan sees nothing), never backfilled from zero.
CREATE INDEX messages_mentions_unrouted_idx ON messages (seq) WHERE mentions_routed_at IS NULL;

-- Newest-first paging within a topic; channel-level paging joins to topics and
-- filters channel_id, keying on the same table-monotonic seq.
CREATE INDEX messages_topic_seq_idx ON messages (topic_id, seq DESC);

-- Full-text search index (design.md:1137-1139): GIN over the generated tsvector.
CREATE INDEX messages_search_idx ON messages USING GIN (search_tsv);

-- Idempotency: at most one stored message per (author, client_request_id) when
-- the key is supplied, so a retried PostMessage returns the stored row instead
-- of duplicating (comms.proto:470-474). The scope is (author, client_request_id)
-- — not per-topic — because a client's request-id is unique per author by
-- construction. Partial — empty keys are not deduped.
CREATE UNIQUE INDEX messages_idem_idx
    ON messages (author_account_id, client_request_id)
    WHERE client_request_id <> '';

-- AgentHasOpenAsk (presence_reads.go) probes messages by author-only equality
-- (WHERE author_account_id = $1 AND blocks @? ...) on EVERY presence edge, so
-- this equality lookup is hot. The other messages indexes do not serve it:
-- messages_topic_seq_idx leads on topic_id, messages_search_idx is a GIN on
-- search_tsv, and the partial-unique idempotency index excludes the rows an ask
-- probe cares about. Absent this index the probe seq-scans messages. Plain
-- (non-partial, non-CONCURRENT) btree: migrations run inside a transaction
-- (store.go applyMigration), so CREATE INDEX CONCURRENTLY is invalid here.
CREATE INDEX messages_author_idx ON messages (author_account_id);

-- ── Channel pins (the pinned board) ─────────────────────────────────────────
-- A channel's pinned board is a small, ordered set of POINTERS to existing
-- messages — it never writes a message row (DL-099/OQ-8): pin/unpin/repoint only
-- add, remove, or move an entry in this table, and PinMessage validates that the
-- target message already lives in the channel (join messages → topics on
-- messages.topic_id = topics.id, then topics.channel_id = the pinned channel,
-- since a message carries no channel_id, only topic_id — DL-098).
--
-- Every mutating op takes ONE channels-row lock (SELECT 1 FROM channels WHERE
-- id = $1 FOR UPDATE) before touching this table (design.md T6:604-608). That
-- single lock serializes BOTH races on the board at once: the per-channel cap
-- check (at most 5 pins per channel, OQ-5 — enforced in-txn under the lock, not
-- a DB constraint) and the repoint compare-and-swap. Pins on different channels
-- never contend.
--
-- ON DELETE RESTRICT on every FK: a pin is a live reference, so deleting a
-- pinned channel, message, or the pinning account is refused rather than
-- silently orphaning or dropping a board entry.
CREATE TABLE channel_pins (
    channel_id           TEXT NOT NULL REFERENCES channels (id) ON DELETE RESTRICT,
    message_id           TEXT NOT NULL REFERENCES messages (id) ON DELETE RESTRICT,
    position             INTEGER NOT NULL,
    pinned_at_unix_ms    BIGINT NOT NULL,
    pinned_by_account_id TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    PRIMARY KEY (channel_id, message_id)
);

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

-- ── Secrets names registry ──────────────────────────────────────────────────
-- The Server-side secrets NAMES registry (SEA-1327 T3): the DECLARED set of
-- secrets — their names and how each is delivered/routed — and NOTHING about
-- their values. Values live only in the SecretSpec provider (keyring/1Password/
-- Vault/…); the Server resolves them at fetch time and never persists them.
-- Deliberately absent and load-bearing: NO value column (encryption-at-rest is
-- the provider's job) and NO per-agent grant column (the MVP injects the whole
-- store into every agent; per-agent scoping is a named FUTURE seam).
CREATE TABLE secrets (
    -- The secret's name, validated at the store door against SecretSpec's
    -- env-var-name grammar (^[A-Za-z_][A-Za-z0-9_]*$) before it can reach a row.
    name        TEXT PRIMARY KEY,
    -- delivery: the file-vs-env split that determines how a secret rotates
    -- (0 file, 1 env). CHECK-pinned so a bad value can never reach a row.
    delivery    SMALLINT NOT NULL CHECK (delivery IN (0, 1)),
    -- kind: the routing class the T5 materializer switches on (0 generic,
    -- 1 provider/LLM, 2 gh).
    kind        SMALLINT NOT NULL CHECK (kind IN (0, 1, 2)),
    -- provider: the SecretSpec/SDK provider id for a provider (LLM) secret.
    -- Empty for non-provider kinds.
    provider    TEXT NOT NULL DEFAULT '',
    -- host: the forge host for a gh secret. Empty for non-gh kinds.
    host        TEXT NOT NULL DEFAULT '',
    -- declared_by: the account that declared this secret. FK ON DELETE RESTRICT
    -- so a referenced account cannot be orphaned out from under a declaration.
    declared_by TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- kind↔provider/host invariant, enforced (not merely documented): a provider
    -- row (kind=1) carries a non-empty provider and no host; a gh row (kind=2) a
    -- non-empty host and no provider; a generic row (kind=0) neither. Without
    -- this a malformed row persists silently and misroutes at the T5 materializer;
    -- the CHECK fails it at write time. DeclareSecret guards the same invariant so
    -- a caller gets ErrInvalidArgument, not a raw constraint violation.
    CONSTRAINT secrets_kind_routing CHECK (
        (kind = 0 AND provider = '' AND host = '')
        OR (kind = 1 AND provider <> '' AND host = '')
        OR (kind = 2 AND host <> '' AND provider = '')
    )
);

CREATE INDEX secrets_declared_by_idx ON secrets (declared_by);

-- ── Agent session ownership & placement ──────────────────────────────────────
-- The durable session-ownership chain (SEA-1342 / SEA-1516): SubscribeAgentSession
-- resolves a session_id to the home channel it must authorize the caller against,
-- persisted so the resolution survives a Server restart. The chain is
-- session_id -> agent_account_id -> home_channel_id (the container hop that
-- 0003 introduced was collapsed out — it could only ever be a 1:1 pass-through
-- on the authz boundary, so agent_sessions points at the agent DIRECTLY). It is
-- rooted non-spoofably: session_id is the SERVER-MINTED StartAgentSession
-- response, written only after the Runner call succeeds, so a row never claims a
-- session the Runner failed to create.
--
-- base_entry_seq (0009) is the write-once per-lifetime rebase base for the
-- transcript store: the wire entry_seq is agent-stamped, monotonic from 1 PER
-- CONTAINER LIFETIME; at lifetime bind the server snapshots
-- base = max(entry_seq) over this session's transcript rows ONCE onto this
-- column (BindLifetime) and persists each frame at base + frame.entry_seq, so the
-- persisted entry_seq is monotonic per SESSION across lifetimes. DEFAULT 0: a
-- brand-new session's first lifetime rebases onto 0, first frame lands at 1.
CREATE TABLE agent_sessions (
    session_id          TEXT PRIMARY KEY,
    agent_account_id    TEXT NOT NULL REFERENCES agent_accounts (account_id) ON DELETE RESTRICT,
    base_entry_seq      BIGINT NOT NULL DEFAULT 0,
    -- recorded_at_unix_ms is the wall-clock (ms since epoch) the session row was
    -- written at RecordAgentSession — the recency key the wake path orders by to
    -- resume an offline agent's MOST RECENT session (LatestSessionForAccount,
    -- RIG-1641 T3). DEFAULT 0 keeps a NOT NULL add safe on this squashed
    -- migration: every RecordAgentSession supplies the value, and the default is
    -- only a floor for any row a future path forgets to stamp (it sorts oldest).
    recorded_at_unix_ms BIGINT NOT NULL DEFAULT 0
);

-- Look a session up by the party that owns it; also the "sessions of this agent"
-- direction reattach reads once it knows which agents a Runner held.
CREATE INDEX agent_sessions_agent_idx ON agent_sessions (agent_account_id);

-- Operational placement state (SEA-1516 reattach): where each agent runs, and
-- under what name, written at ProvisionAgentWorkspace. Deliberately NOT part of
-- the authz chain — placement is where an agent runs, not who may watch it.
-- PK on the agent, not a surrogate: an agent is on AT MOST ONE Runner under one
-- container name, so a re-provision REPLACES the row (upsert on agent_account_id)
-- rather than accumulating an ambiguous second placement. runner_id is
-- deliberately NOT a FK: Runners are enrolled in memory under a token subject
-- with no runners table to reference, and a placement must OUTLIVE the Runner's
-- attachment ('' is the deliberate unknown-runner sentinel, self-healed by the
-- next provision — so runner_id stays NOT NULL rather than nullable).
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

-- StartAgentSession arrives holding only container_name, so it resolves the
-- owning account by this index before recording the session. UNIQUE because the
-- mapping is 1:1 — a container belongs to exactly one agent — so a second agent
-- can never claim a live container name and record a session under the wrong
-- owner.
CREATE UNIQUE INDEX agent_placements_container_key ON agent_placements (container_name);

-- ── Agent session transcripts (two-tier store) ───────────────────────────────
-- The durable TWO-TIER transcript store (SEA-1667 T4): a Postgres HOT TAIL
-- holding [latest checkpoint .. now] = the normal resume set, plus a manifest of
-- the object-store COLD ARCHIVE (verbatim JSONL segments). Both tables are
-- FK-rooted in agent_sessions ON DELETE RESTRICT.
--
-- HOT TAIL. entry_seq is SESSION-scoped and monotonic across container lifetimes
-- (rebased via agent_sessions.base_entry_seq). idempotency_key carries the
-- durable lane's at-most-once guarantee; its UNIQUE is GLOBAL (not
-- (session_id, key)-scoped) because the agent mints each key from a per-sink
-- random nonce + monotonic counter, unique across sessions AND restarts by
-- construction — the load-bearing invariant that keeps the ON CONFLICT DO NOTHING
-- dedup collision-free. checkpoint marks a full-body snapshot that supersedes all
-- prior entries (the read view starts at the latest checkpoint).
CREATE TABLE agent_session_transcript_entries (
    session_id      TEXT   NOT NULL REFERENCES agent_sessions (session_id) ON DELETE RESTRICT,
    entry_seq       BIGINT NOT NULL,
    checkpoint      BOOLEAN NOT NULL DEFAULT FALSE,
    entry_json      TEXT   NOT NULL,
    idempotency_key TEXT   NOT NULL UNIQUE,
    PRIMARY KEY (session_id, entry_seq)
);

-- ARCHIVE MANIFEST. One row per flushed object-store segment (verbatim JSONL).
-- kind='superseded' segments hold pre-checkpoint history, NEVER read on resume;
-- kind='safety_valve' segments hold post-checkpoint entries evicted by the high
-- size-cap and ARE spliced back on resume (T5); kind='session_end' segments
-- archive the retained post-checkpoint tail at teardown for analytics and are
-- NEVER read on resume. The object key is prefixed sessions/<session_id>/;
-- bucket/endpoint are server config, not per-row.
CREATE TABLE agent_session_archive_segments (
    session_id    TEXT   NOT NULL REFERENCES agent_sessions (session_id) ON DELETE RESTRICT,
    object_key    TEXT   NOT NULL,
    min_entry_seq BIGINT NOT NULL,
    max_entry_seq BIGINT NOT NULL,
    kind          TEXT   NOT NULL CHECK (kind IN ('superseded', 'safety_valve', 'session_end')),
    PRIMARY KEY (session_id, object_key)
);

-- ── Delivery cursors ──────────────────────────────────────────────────────────
-- The durable per-(agent, channel) delivery cursor (SEA-1569 T2, design record
-- D2). One row records how far an agent has confirmed delivery on a channel, so
-- a sweep after a restart / reconnect replays exactly the owed-but-unacked tail
-- and never the full history. The cursor is agent-only: agent_account_id
-- references agent_accounts, so a user id can never carry a cursor row.
CREATE TABLE agent_delivery_cursors (
    agent_account_id TEXT NOT NULL REFERENCES agent_accounts (account_id) ON DELETE RESTRICT,
    channel_id       TEXT NOT NULL REFERENCES channels (id) ON DELETE RESTRICT,
    -- The contiguous low-water cursor: highest messages.seq at or below which
    -- every message OWED to this agent on this channel is acked (a self-authored
    -- seq is vacuously satisfied — never a hole). Seeded to the channel head at
    -- subscribe time.
    acked_seq        BIGINT NOT NULL DEFAULT 0,
    -- Acked seqs ABOVE the contiguous cursor (out-of-order acks), bounded to a
    -- small window; drained into acked_seq as gaps fill. Mirrors ControlAck's
    -- acked_seq + applied_above.
    above_seqs       BIGINT[] NOT NULL DEFAULT '{}',
    acked_at         TIMESTAMPTZ,
    PRIMARY KEY (agent_account_id, channel_id)
);

-- ── Owed mentions ─────────────────────────────────────────────────────────────
-- The durable no-loss backstop for the mention-gap population (RIG-1641 T1): an
-- @-mentioned agent member that is unsubscribed, non-home, non-mandatory, and
-- offline is on NO delivery path — the cursor sweep is subscription-gated and so
-- skips it. When the settle edge routes a mention to such a member with no live
-- session, it records an owed row here; the session-start sweep surfaces it
-- regardless of subscription, and the restructured AckDelivery clears it on ack.
-- One row per (agent, message); the PK is also the read index — OwedMentions
-- reads by agent_account_id, the PK's leading column, so no extra index is
-- needed. recorded_at_unix_ms is read by T2's observability (owed-row age) and
-- bounds a future retention sweep.
--
-- ON DELETE CASCADE on all three FKs is DELIBERATE and the ONE place this schema
-- departs from the surrounding ON DELETE RESTRICT convention: an owed mention of
-- a deleted message, account, or channel is moot, so it should vanish with its
-- referent rather than stand as a lien blocking three tables' delete paths.
-- The message_id and channel_id CASCADE FKs are intentionally unindexed: no
-- delete path exists for messages or channels today, so the cascade never fires
-- and a supporting index would be dead weight. When a message/channel delete
-- path lands, add owed_mentions(message_id) / owed_mentions(channel_id) to avoid
-- a seq-scan per parent delete.
CREATE TABLE owed_mentions (
    agent_account_id    TEXT NOT NULL REFERENCES agent_accounts (account_id) ON DELETE CASCADE,
    message_id          TEXT NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    channel_id          TEXT NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    recorded_at_unix_ms BIGINT NOT NULL,
    PRIMARY KEY (agent_account_id, message_id)
);

-- ── Agent activity ───────────────────────────────────────────────────────────
-- The durable store-of-record for an agent's free-text activity string (the
-- "what am I doing right now" line the agent-set roster renders). Presence — the
-- online/away/offline ENUM — stays in-memory per DL-074 (a property of a live
-- connection, meaningless across a restart), so it is deliberately NOT persisted.
-- The activity string is a durable statement the agent authored, so a Server
-- restart recovers it from here. One row per agent; ON DELETE RESTRICT mirrors
-- the agent tree's FK discipline — the activity is cleared through the store,
-- not by cascade.
CREATE TABLE agent_activity (
    agent_account_id    TEXT PRIMARY KEY REFERENCES agent_accounts (account_id) ON DELETE RESTRICT,
    activity            TEXT NOT NULL,
    activity_at_unix_ms BIGINT NOT NULL
);

-- ── Agent config bundle (fleet singleton) ─────────────────────────────────────
-- The Server-side fleet CONFIG-BUNDLE store (SEA-1624 T1): the ONE fleet-wide
-- agent config bundle — the gzip-tarball of skills/, extensions/, and mcp/
-- material every agent materializes into its scoped config dir. Unlike secrets
-- (a set of named rows), config is a SINGLETON: exactly one current bundle for
-- the whole fleet.
--   * singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton) — the PK is a
--     constant TRUE, so a second INSERT collides on the PK and the CHECK forbids
--     any other value ever reaching the column. Together they pin the table to a
--     single row. PutAgentConfig upserts on this key (whole-bundle replace).
--   * CURRENT-ONLY retention — no history; a new bundle REPLACES the row in place.
--   * version IS the content hash (sha256 over the DECOMPRESSED, metadata-zeroed
--     content) — stable across transport re-packing, so a re-put of byte-identical
--     content yields a stable version and agents skip a redundant re-materialize.
--   * bundle content is CREDENTIAL-FREE by MVP rule (CD-3) — secrets ride the
--     separate names-registry + SecretSpec resolve path, never this bundle.
CREATE TABLE agent_config_bundle (
    singleton  BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    version    TEXT NOT NULL,
    bundle     BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ── Issues: the durable board issue ──────────────────────────────────────────
-- The store-of-record for a Compass board issue (SEA-1728, DL-019): the
-- forge-derived facts a poll ingests, plus the Compass-owned machinery a board
-- item carries. The forge coordinate (forge_provider, forge_host, repo, number)
-- is the IDEMPOTENCY KEY (issues_coordinate_key): a re-poll UPDATES the existing
-- row rather than minting a second board item. The id is a surrogate minted once
-- on first insert and never derived from the coordinate, so it stays stable
-- across every re-poll. state DEFAULT BACKLOG(1) with CHECK 1..8 says a persisted
-- issue ALWAYS has a real lifecycle — NEVER UNSPECIFIED(0); the forge-only upsert
-- never touches state, so a human-set lifecycle survives a re-poll.
CREATE TABLE issues (
    id             TEXT PRIMARY KEY,

    -- forge coordinate: the idempotency key.
    forge_provider SMALLINT NOT NULL CHECK (forge_provider IN (1, 2, 3)),  -- GitHub/GitLab/Forgejo; never UNSPECIFIED(0)
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

    -- OQ-6(a) recency-guard column (RIG-2883 T4): the forge's last-updated
    -- timestamp for the artifact. INERT until T4a threads the write path
    -- (Issue.UpdatedAt reaches no writer today); the bare column is a no-op.
    forge_updated_at TIMESTAMPTZ,

    -- Compass machinery (server-owned; none on the forge). state defaults to
    -- BACKLOG; CHECK 1..8: a persisted issue is NEVER UNSPECIFIED(0). The
    -- machinery columns get their writers in later slices.
    state          SMALLINT NOT NULL DEFAULT 1 CHECK (state BETWEEN 1 AND 8),
    priority       TEXT     NOT NULL DEFAULT '',
    assignee       TEXT     NOT NULL DEFAULT '',
    summary        TEXT     NOT NULL DEFAULT '',
    branch         TEXT     NOT NULL DEFAULT ''
);

-- The idempotency key: one board item per forge coordinate.
CREATE UNIQUE INDEX issues_coordinate_key
    ON issues (forge_provider, forge_host, repo, number);

-- ── Forge subscriptions & fetch cursors ──────────────────────────────────────
-- The DL-053 forge-poll fetch machinery (SEA-1810). Coordinate-aligned to the
-- 0013 issue convention: SMALLINT provider enum + forge_host in every key. Every
-- provider CHECK admits the full declared enum IN (1, 2, 3, 4) — the CHECK's job
-- is "never UNSPECIFIED(0)", not gating rollout (rollout is gated by which
-- forge.Provider has a real client). Convention: text ids, FK ON DELETE RESTRICT.

-- The board's repo-level poll targets (OQ-C): one row per (provider, host, repo)
-- the poll driver walks. enabled=FALSE soft-disables a target without deleting
-- its cursor history. For GITHUB the repo string is lowercased at the seed/upsert
-- boundary (owner/name is case-insensitive-but-case-preserving, so Owner/Name and
-- owner/name must NOT mint two PK rows). updated_at is touched on every
-- upsert/enable-flip.
CREATE TABLE forge_repo_subscriptions (
    forge_provider SMALLINT NOT NULL CHECK (forge_provider IN (1, 2, 3, 4)),
    forge_host     TEXT     NOT NULL,
    repo           TEXT     NOT NULL,
    enabled        BOOLEAN  NOT NULL DEFAULT TRUE,
    swept_updated_at TIMESTAMPTZ,              -- last swept forge updated_at watermark; NULL = never swept
    list_etag      TEXT     NOT NULL DEFAULT '', -- conditional-GET etag for the repo LIST walk
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (forge_provider, forge_host, repo)
);

-- DL-053's forge_subscriptions, renamed agent_forge_subscriptions (OQ-C) and
-- coordinate-aligned. The UNIQUE (agent, coordinate, kind, number, project)
-- makes an agent's subscription to one artifact (or one container) idempotent.
-- scope (RIG-2732 T3, OQ-1 ruled (i)) discriminates ARTIFACT(1) rows (number>0,
-- project='') from CONTAINER(2) rows (number=0; project=the Linear project id on
-- LINEAR, '' on GitHub). project rides the UNIQUE so two Linear project
-- containers on one team do not collide.
CREATE TABLE agent_forge_subscriptions (
    id               TEXT PRIMARY KEY,
    agent_account_id TEXT NOT NULL REFERENCES agent_accounts (account_id) ON DELETE RESTRICT,
    forge_provider   SMALLINT NOT NULL CHECK (forge_provider IN (1, 2, 3, 4)),
    forge_host       TEXT NOT NULL,
    repo             TEXT NOT NULL,
    kind             SMALLINT NOT NULL CHECK (kind IN (1, 2)),
    number           BIGINT NOT NULL,
    scope            SMALLINT NOT NULL DEFAULT 1 CHECK (scope IN (1, 2)),  -- 1 artifact, 2 container
    project          TEXT NOT NULL DEFAULT '',  -- Linear CONTAINER rows: project id; else ''
    delivered_revision TEXT NOT NULL DEFAULT '',
    delivered_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (agent_account_id, forge_provider, forge_host, repo, kind, number, project)
);

CREATE INDEX agent_forge_subscriptions_artifact_idx
    ON agent_forge_subscriptions (forge_provider, forge_host, repo, kind, number);

-- DL-053's per-artifact FETCH cursor, coordinate-aligned: a durable
-- conditional-GET cache keyed by the artifact coordinate. snapshot holds the last
-- observed state for DetectChanges.
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
-- A durable conditional-GET cache; etag advances ONLY after every row of that
-- page's content is durably sunk. has_next persists the Link-chain fact so a 304
-- can keep walking a multi-page repo. advanced_at records the last content
-- advance (an etag-storing 200+sink), NOT the last poll. Retires with the poll
-- driver (RIG-2883 T5), atomically with its serve.go consumer.
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

-- One row per forge artifact Compass AUTHORED on behalf of an agent (DL-055):
-- the coordinate the write path minted, who authored it, and the F3 idempotency
-- memo. The write chokepoint (T4) writes the row AND the memo in a single
-- statement on a create success; a provider error writes NOTHING.
--
-- forge_provider is a SMALLINT enum; the CHECK IN (1, 2, 3, 4) exists to reject
-- UNSPECIFIED(0), not to gate rollout. FK ON DELETE RESTRICT (below) so a
-- referenced account cannot be orphaned out from under an ownership row.
--
-- PK is the forge coordinate (provider, host, repo, kind, number) — the same
-- coordinate shape forge_artifact_cursors keys on. A retry of the same authored
-- create idempotently re-lands on this key (ON CONFLICT upsert). kind CHECK
-- IN (1, 2): 1=issue, 2=pull_request, matching agent_forge_subscriptions.kind.
--
-- client_request_id is NULLABLE: NULL when the caller supplied no idempotency
-- key. The UNIQUE PARTIAL index on (agent_account_id, client_request_id) WHERE
-- client_request_id IS NOT NULL is the F3 memo — it dedups a per-agent retry
-- carrying the same key, while NULL-key rows never collide.
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

-- linear_agent_sessions: the Linear Agent Session ↔ Compass conversation
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
-- text ids are server/forge-assigned; created_at is a TIMESTAMPTZ DEFAULT now()
-- birth marker. No FKs: manager_account_id, channel_id and topic_id name live
-- Compass rows, but the association is written from the webhook path against ids
-- the responder just resolved, and a Manager/channel/topic teardown must not be
-- blocked by a stale Linear association — so these are unconstrained id columns,
-- matching the schema in the record (§Part 2).
CREATE TABLE linear_agent_sessions (
    linear_session_id  TEXT PRIMARY KEY,               -- Linear AgentSession.id
    manager_account_id TEXT NOT NULL,                  -- the resolved Compass Manager
    channel_id         TEXT NOT NULL,                  -- the Manager's home channel
    topic_id           TEXT NOT NULL,                  -- comms topic of the conversation
    linear_issue_id    TEXT,                           -- provenance (issue delegated on); NULL if none
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
