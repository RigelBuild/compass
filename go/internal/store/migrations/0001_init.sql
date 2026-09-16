-- 0001_init: the WHOLE compass v0.6 store of record, as a single squashed
-- schema. This one file produces the complete schema the Server serves —
-- accounts and their user/agent subtypes, the agent tree, channel groups,
-- channels + membership + policy, topics and topic-scoped messages, the pinned
-- board, agent workspaces, delivery cursors, session ownership + placement, the
-- two-tier transcript store, the secrets names registry, the fleet config
-- bundle, board issues, the forge webhook-lane target machinery, forge
-- authored-artifact ownership, and tenants — the isolation root every
-- tenant-owned table hangs off (RIG-2861).
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

-- ── Tenants ─────────────────────────────────────────────────────────────────
-- One row per managed-service tenant; the isolation root every tenant-owned
-- table hangs off (RIG-2861 T1). slug is the stable idempotency key the
-- bootstrap-tenant seed finds-or-creates on (BootstrapTenant), mirroring the
-- unique-handle key BootstrapAdmin uses. created_at_unix_ms is BIGINT ms since
-- epoch, matching the newer unix-ms columns in this schema (sessions,
-- delivery), NOT a TIMESTAMPTZ. OSS single-tenant runs with exactly one row.
CREATE TABLE tenants (
    id                 TEXT PRIMARY KEY,
    slug               TEXT NOT NULL UNIQUE,
    display_name       TEXT NOT NULL,
    created_at_unix_ms BIGINT NOT NULL
);

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
    tenant_id    TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE) REFERENCES tenants (id) ON DELETE RESTRICT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The "accounts of this tenant" lookup direction for tenant-scoped reads.
CREATE INDEX accounts_tenant_idx ON accounts (tenant_id);

-- Human accounts: a permission role (0 member, 1 admin). PK is also the FK to
-- accounts, so a user row is exactly one account and cannot coexist with an
-- agent row of the same id.
CREATE TABLE user_accounts (
    account_id TEXT PRIMARY KEY REFERENCES accounts (id) ON DELETE RESTRICT,
    role       SMALLINT NOT NULL DEFAULT 0 CHECK (role IN (0, 1)),
    tenant_id  TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE)
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
-- registered coordination hook (RIG-1722 T5) — the manager-comms coordination
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
    tenant_id       TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
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
    account_id TEXT PRIMARY KEY REFERENCES accounts (id) ON DELETE RESTRICT,
    tenant_id  TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE)
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
    owner_user_id TEXT REFERENCES user_accounts (account_id) ON DELETE RESTRICT,
    tenant_id     TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE)
);

-- The two partial-unique indexes ARE the resolution index and enforce the
-- two-namespace contract:
--   * user/system handles (owner_user_id IS NULL) are globally unique — the
--     tier that preserves today's accounts.handle global-unique invariant;
--   * agent handles (owner_user_id IS NOT NULL) are unique only per owner.
-- An agent handle MAY overlap a global user handle with no collision at resolve
-- time: a user is only ever looked up bare (first index) and an agent only ever
-- owner-qualified (second index), so the two never contend on one lookup.
CREATE UNIQUE INDEX account_handles_global_key ON account_handles (tenant_id, handle) WHERE owner_user_id IS NULL;
CREATE UNIQUE INDEX account_handles_owner_key ON account_handles (tenant_id, owner_user_id, handle) WHERE owner_user_id IS NOT NULL;

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
    visibility      SMALLINT NOT NULL DEFAULT 0 CHECK (visibility IN (0, 1)),
    tenant_id       TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE)
);

CREATE INDEX channel_groups_parent_idx ON channel_groups (parent_group_id);
CREATE INDEX channel_groups_owner_idx ON channel_groups (owner_user_id);

-- ── Channels ────────────────────────────────────────────────────────────────
-- A named conversation in a group (group_id NULL = ungrouped, owner-scoped).
-- kind is 0 channel / 1 DM / 2 GROUP_DM. Membership lives in channel_members.
--
-- Channel-policy fields (0014, RIG-1722 T4): post_policy mirrors the
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
    mandatory_subscription BOOLEAN NOT NULL DEFAULT FALSE,
    tenant_id              TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE)
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
    tenant_id  TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
    PRIMARY KEY (channel_id, account_id)
);

CREATE INDEX channel_members_account_idx ON channel_members (account_id);

-- ── Agent workspaces ────────────────────────────────────────────────────────
-- The observation pane for one agent (superseded decision 4): no participant
-- list — access is a projection of home-channel membership (fork f). One
-- workspace per agent (UNIQUE), created idempotently on first OpenAgentWorkspace.
CREATE TABLE agent_workspaces (
    id               TEXT PRIMARY KEY,
    agent_account_id TEXT NOT NULL UNIQUE REFERENCES agent_accounts (account_id) ON DELETE RESTRICT,
    tenant_id        TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE)
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
    last_seq              BIGINT NOT NULL DEFAULT 0,  -- denormalized activity order
    tenant_id             TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE)
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
    mentions_routed_at BIGINT,
    tenant_id          TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE)
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
CREATE INDEX messages_search_idx ON messages USING gin (search_tsv);

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
    tenant_id            TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
    PRIMARY KEY (channel_id, message_id)
);

-- ── Token hashes ────────────────────────────────────────────────────────────
-- Subject-typed token store (design.md:1175-1183): the SHA-256 hash is the PK
-- (the plaintext token is returned once and never stored). subject_kind is 0
-- account / 1 runner / 2 service; subject_id spans those id spaces. revoked_at
-- is set on RevokeToken so ResolveTokenHash tells revoked from never-issued.
CREATE TABLE tokens (
    hash         BYTEA PRIMARY KEY,
    subject_kind SMALLINT NOT NULL CHECK (subject_kind IN (0, 1, 2)),
    subject_id   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at   TIMESTAMPTZ
);

CREATE INDEX tokens_subject_idx ON tokens (subject_kind, subject_id);

-- ── Secrets registry (scoped, encrypted at rest) ────────────────────────────
-- The user-secret store: a declared secret's name + routing AND its value,
-- AES-256-GCM-encrypted at rest (design record compass-user-secret-store, A1).
-- Values were formerly provider-held and this table names-only; ruling D1 moved
-- them here, encrypted, so a DB dump/replica/operator SELECT is not a
-- compromise. Rows are scoped tenant/user/agent with most-specific-wins
-- resolution (A9); a tenant row is a real shared value several users resolve,
-- not a placeholder.
CREATE TABLE secrets (
    -- The secret's name, validated at the store door against SecretSpec's
    -- env-var-name grammar (^[A-Za-z_][A-Za-z0-9_]*$) before it can reach a row.
    name        TEXT NOT NULL,
    -- scope_kind: 0 tenant, 1 user, 2 agent. scope_id is '' for a tenant row,
    -- else the owning accounts.id. No FK: a tenant row's '' can never satisfy
    -- one, and Postgres has no partial FK (A9) — the store door resolves the
    -- account instead.
    scope_kind  SMALLINT NOT NULL CHECK (scope_kind IN (0, 1, 2)),
    scope_id    TEXT NOT NULL DEFAULT '',
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
    -- value_ciphertext/value_nonce: the AES-256-GCM ciphertext and its fresh
    -- 96-bit nonce. NOT NULL in the final schema (T5): the upsert is the sole
    -- writer and always writes both, so a value-free row can no longer exist (A1).
    value_ciphertext BYTEA NOT NULL,
    value_nonce      BYTEA NOT NULL,
    -- key_version: which master-key generation encrypted this row (A3, reserved
    -- for the deferred rotation record).
    key_version SMALLINT NOT NULL DEFAULT 1,
    -- declared_by: the account that declared this secret. FK ON DELETE RESTRICT
    -- so a referenced account cannot be orphaned out from under a declaration.
    declared_by TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id   TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
    -- (name, scope_kind, scope_id) is the identity: the same name resolves to a
    -- different value at each tier, so the value store cannot key on name alone.
    PRIMARY KEY (name, scope_kind, scope_id),
    -- kind↔provider/host invariant, enforced (not merely documented): a provider
    -- row (kind=1) carries a non-empty provider and no host; a gh row (kind=2) a
    -- non-empty host and no provider; a generic row (kind=0) neither. Without
    -- this a malformed row persists silently and misroutes at the T5 materializer;
    -- the CHECK fails it at write time. UpsertSecret guards the same invariant so
    -- a caller gets ErrInvalidArgument, not a raw constraint violation.
    CONSTRAINT secrets_kind_routing CHECK (
        (kind = 0 AND provider = '' AND host = '')
        OR (kind = 1 AND provider <> '' AND host = '')
        OR (kind = 2 AND host <> '' AND provider = '')
    ),
    -- scope↔id shape, mirrored at the UpsertSecret door so a caller gets
    -- ErrInvalidArgument: a tenant row carries no id; a user/agent row must.
    CONSTRAINT secrets_scope_shape CHECK (
        (scope_kind = 0 AND scope_id = '')
        OR (scope_kind IN (1, 2) AND scope_id <> '')
    )
);

CREATE INDEX secrets_declared_by_idx ON secrets (declared_by);

-- server_secrets: the physically separate registry for SERVER-owned secret
-- names (mechanism C1, design record D6). Server secrets are never delivered
-- into an agent container; keeping them in their own table means the
-- inject-all container path (which reads `secrets`) can NEVER see them, by
-- construction rather than by a filter someone can forget to apply.
--
-- Mirrors `secrets` above minus the container-delivery routing: no `delivery`
-- column (server secrets are never container-delivered — that IS the point) and
-- no `kind` column (they never reach the T5 materializer).
--
-- Names-only, like `secrets`: a row records that a name is declared, never its
-- VALUE. Values live in the SecretSpec provider keyspace.
CREATE TABLE server_secrets (
    -- The secret's name, validated at the store door against SecretSpec's
    -- env-var-name grammar (^[A-Za-z_][A-Za-z0-9_]*$) before it can reach a
    -- row, and additionally required to carry a reserved server-secret prefix
    -- (SERVER_, GATEWAY_CREDENTIALS_, or COMPASS_) so this table can never hold
    -- a name the user keyspace owns.
    name        TEXT PRIMARY KEY,
    -- declared_by: the account that declared this secret, or NULL for a
    -- server-provisioned row (the master key). Contrast `secrets.declared_by`,
    -- which is NOT NULL: the boot provisioner declares the master key with no
    -- human actor, so NULL is honest provenance — attributing the row to the
    -- bootstrap-admin account would falsify the audit trail and couple key
    -- provisioning to account-bootstrap ordering. ON DELETE RESTRICT still
    -- protects operator-declared rows.
    declared_by TEXT REFERENCES accounts (id) ON DELETE RESTRICT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The reserved-prefix half of the F1 keyspace partition, enforced (not
    -- merely documented) — the same belt-and-braces shape `secrets` uses for
    -- its kind-routing invariant above. DeclareServerSecret guards the same
    -- rule at the store door so a caller gets an actionable ErrInvalidArgument
    -- rather than a raw constraint violation; this CHECK is what keeps the
    -- partition standing if a future writer reaches the table without going
    -- through that door. Violating it is not a mere bad row: an unprefixed name
    -- here (or a reserved one in `secrets`) is what ships a deployment secret
    -- into every agent container.
    --
    -- The backslash escapes are REQUIRED: `_` is a LIKE single-character
    -- wildcard, so the unescaped 'SERVER_%' also matches 'SERVERX_Y' —
    -- precisely the near-miss the guard must reject. Verified on the pinned
    -- image: 'SERVERX_Y' LIKE 'SERVER_%' is TRUE, LIKE 'SERVER\_%' is FALSE,
    -- and 'SERVER_Y' LIKE 'SERVER\_%' is TRUE.
    CONSTRAINT server_secrets_reserved_prefix CHECK (
        name LIKE 'SERVER\_%' OR name LIKE 'GATEWAY\_CREDENTIALS\_%' OR name LIKE 'COMPASS\_%'
    )
);

CREATE INDEX server_secrets_declared_by_idx ON server_secrets (declared_by);

-- server_key_state: the master-key tripwire substrate. Single-row by
-- construction (CHECK (id = 1)), holding a NON-SECRET salted digest of the
-- active master key plus its version. This is NOT a declared-name registry, so
-- the names-only invariant is untouched — a salted digest of a key is not that
-- key's value. Boot recomputes the digest and fails closed on a mismatch,
-- which is what catches a swapped key before it decrypts anything.
--
-- DELETE is revoked below, after the schema-wide grant: the tripwire row is
-- written once at provision and updated in place on rotation, so DELETE is
-- never needed — and withholding it means the digest cannot be dropped to
-- defeat the key-swap check.
CREATE TABLE server_key_state (
    id               SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    key_version      SMALLINT NOT NULL DEFAULT 1,
    key_fingerprint  BYTEA NOT NULL,
    fingerprint_salt BYTEA NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ── Agent session ownership & placement ──────────────────────────────────────
-- The durable session-ownership chain (RIG-1342 / RIG-1516): SubscribeAgentSession
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
    recorded_at_unix_ms BIGINT NOT NULL DEFAULT 0,
    tenant_id           TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE)
);

-- Look a session up by the party that owns it; also the "sessions of this agent"
-- direction reattach reads once it knows which agents a Runner held.
CREATE INDEX agent_sessions_agent_idx ON agent_sessions (agent_account_id);

-- Operational placement state (RIG-1516 reattach): where each agent runs, and
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
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id        TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE)
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

-- Session bindings (RIG-3108 / RIG-2861 §T4): the DURABLE (session -> agent
-- account, Runner) binding the RunnerHub has so far held only in RAM. Placement
-- (above) is where an agent RUNS; a binding is which LIVE session speaks for it,
-- so the two are siblings and neither is authorization: SubscribeAgentSession
-- still authorizes through agent_sessions -> agent_accounts -> channel_members
-- and never reads this table.
--
-- KEYED ON THE ACCOUNT, tenant folded in: PRIMARY KEY (tenant_id,
-- agent_account_id). The account is the identity because this table represents
-- the hub's 1:1 in-RAM accountSessions map (account -> its ONE live session),
-- where re-pointing an account at a newer session is an assignment, not a
-- collision. So a re-point here is an upsert on the account
-- (ON CONFLICT DO UPDATE), never a conflict to be refused: the hub legitimately
-- holds two sessions for one account transiently — it promotes the new session
-- before unbinding the stale one — and RecordSessionBinding's caller has nowhere
-- to put a refusal. Keying on the session instead would have made the
-- displacement a unique violation and the reap below impossible.
--
-- It represents accountSessions ONLY. The hub also keeps sessionAccounts
-- (session -> account, MANY-to-one), and this table is not that map: a re-point
-- OVERWRITES the row, so the displaced session stops resolving IMMEDIATELY —
-- ResolveSessionAccount returns not-found for it the moment the newer bind
-- commits, which the suite asserts directly. There is deliberately no tombstone
-- and no history: this table answers "which session speaks for this account
-- NOW", nothing else.
--
-- So it cannot answer STALE vs UNKNOWN. A caller that needs to tell "a session
-- this account used to hold" from "a session id we have never seen" — the hub's
-- re-point guard at runnerhub/relay_comms.go:112-116 — must keep that
-- distinction in RAM. Demoting the hub's maps to caches over this table (PR3)
-- does not change that: the guard's state has no column here to live in.
--
-- What a bind DISPLACED is still reported to the caller, so it can reap the
-- displaced session from the held-deliver registry. It comes from the prior-value
-- read RecordSessionBinding takes under FOR UPDATE in the same transaction as the
-- write (queries/session_bindings.sql), not from a RETURNING — ON CONFLICT DO
-- UPDATE's RETURNING sees the post-update row.
--
-- tenant_id leads the key for the reason the RLS header below states: two
-- tenants may hold the same coordinate without collision. Its declaration text
-- is character-identical to every other tenant table's, because the policy
-- compares it to the same GUC.
--
-- session_id is UNIQUE PER TENANT (the index below) but is NOT the identity: it
-- is the accountForSession read direction the relay resolves on every inbound
-- comms call, and the uniqueness only says one session speaks for one account.
--
-- A binding is NOT cross-checked against agent_sessions: there is deliberately
-- no FK from session_id, so a binding may name a session with no agent_sessions
-- row, or disagree with one about the owner. That is intentional — a binding is
-- independent of the session record's lifetime — and it is why agent_sessions,
-- not this table, remains the authz root. Nothing here may be read as proof a
-- session exists or as proof of who owns it.
--
-- runner_id is deliberately NOT a FK, for the same reason agent_placements'
-- isn't: Runners are enrolled in memory under a token subject with no runners
-- table to reference. It stays NOT NULL — a binding with no Runner cannot be
-- swept by the reconnect sweep below, and an unswept binding is a stale session
-- that outlives its Runner.
CREATE TABLE session_bindings (
    tenant_id        TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
    agent_account_id TEXT NOT NULL REFERENCES agent_accounts (account_id) ON DELETE RESTRICT,
    session_id       TEXT NOT NULL,
    runner_id        TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, agent_account_id)
);

-- The session -> account lookup (ResolveSessionAccount): the relay's read on
-- every inbound comms call, where the request carries only the session id.
-- UNIQUE so one session id speaks for exactly one account — a second account
-- claiming a live session id is refused (mapped to ErrConflict) rather than
-- letting the relay resolve whichever row Postgres happened to return.
-- Tenant-folded for the same reason the PK is: two tenants may mint the same
-- session id without colliding.
CREATE UNIQUE INDEX session_bindings_session_key ON session_bindings (tenant_id, session_id);

-- The reconnect sweep deletes every binding of a re-enrolling Runner
-- (DeleteSessionBindingsForRunner), so runner_id is the read direction that
-- needs an index — the same direction, and the same reason, as
-- agent_placements_runner_idx above. Non-unique: one Runner holds many sessions.
CREATE INDEX session_bindings_runner_idx ON session_bindings (runner_id);

-- ── Agent session transcripts (two-tier store) ───────────────────────────────
-- The durable TWO-TIER transcript store (RIG-1667 T4): a Postgres HOT TAIL
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
    tenant_id       TEXT   NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
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
    tenant_id     TEXT   NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
    PRIMARY KEY (session_id, object_key)
);

-- ── Delivery cursors ──────────────────────────────────────────────────────────
-- The durable per-(agent, channel) delivery cursor (RIG-1569 T2, design record
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
    tenant_id        TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
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
    tenant_id           TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
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
    activity_at_unix_ms BIGINT NOT NULL,
    tenant_id           TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE)
);

-- ── Agent config bundle (fleet singleton) ─────────────────────────────────────
-- The Server-side fleet CONFIG-BUNDLE store (RIG-1624 T1): the ONE fleet-wide
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

-- ── Model registry (fleet singleton, CAS-versioned) ──────────────────────────
-- The Server-side STABLE-NAME registry (RIG-3122 P2): the ONE fleet-wide map
-- from a stable model name to its ordered candidate chain + listing metadata,
-- which the gateway resolver reads to route a request's modelId (design.md
-- compass-stable-name-routing §P1/P2). Like agent_config_bundle it is a fleet
-- SINGLETON — one registry for the whole fleet — but UNLIKE it the write path
-- is COMPARE-AND-SET on a monotonic version, not a content-hash upsert:
--   * singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton) — the PK is a
--     constant TRUE (same one-row pin as agent_config_bundle): a second INSERT
--     collides on the PK and the CHECK forbids any other value, so the table
--     holds exactly one row.
--   * version BIGINT — a monotonic whole-registry version (NOT a content hash).
--     It supplies the CAS substrate the gateway credentials store also uses (a
--     monotonic version per row, compass-server-llm-gateway/design.md:324-329):
--     a Put carries the version it read and only lands if the row still holds
--     it (version = version + 1), so a racing operator write is never clobbered.
--     The version also keys the gateway resolver's in-memory ref (P1), which
--     re-reads only on a version change.
--   * registry JSONB — the whole registry payload (name -> {display_name,
--     ordered candidates [{provider, model_id}], metadata}); validated
--     fail-closed at the RPC boundary (ValidateModelRegistry) before any write.
--   * CURRENT-ONLY retention — no history; a Put REPLACES the row's payload in
--     place and bumps the version.
CREATE TABLE model_registry (
    singleton  BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    version    BIGINT NOT NULL,
    registry   JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ── Issues: the durable board issue ──────────────────────────────────────────
-- The store-of-record for a Compass board issue (RIG-1728, DL-019): the
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
    branch         TEXT     NOT NULL DEFAULT '',
    tenant_id      TEXT     NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE)
);

-- The idempotency key: one board item per forge coordinate.
CREATE UNIQUE INDEX issues_coordinate_key
    ON issues (tenant_id, forge_provider, forge_host, repo, number);

-- ── Forge subscriptions & reconcile watermarks ───────────────────────────────
-- The DL-053 forge webhook-lane target machinery (RIG-1810; webhook-driven per
-- DL-281). Coordinate-aligned to the 0013 issue convention: SMALLINT provider
-- enum + forge_host in every key. Every provider CHECK admits the full declared
-- enum IN (1, 2, 3, 4) — the CHECK's job is "never UNSPECIFIED(0)", not gating
-- rollout (rollout is gated by which forge.Provider has a real client).
-- Convention: text ids, FK ON DELETE RESTRICT.

-- The board's repo-level webhook targets (OQ-C): one row per (provider, host,
-- repo) the board ingest lane accepts events for and the reconciler sweeps.
-- enabled=FALSE soft-disables a target without deleting its watermark. For
-- GITHUB the repo string is lowercased at the seed/upsert boundary (owner/name
-- is case-insensitive-but-case-preserving, so Owner/Name and owner/name must NOT
-- mint two PK rows). swept_updated_at is the per-repo updated-at reconcile
-- watermark (NULL = never swept); list_etag is the conditional-GET etag for the
-- reconcile LIST walk. updated_at is touched on every upsert/enable-flip.
CREATE TABLE forge_repo_subscriptions (
    forge_provider SMALLINT NOT NULL CHECK (forge_provider IN (1, 2, 3, 4)),
    forge_host     TEXT     NOT NULL,
    repo           TEXT     NOT NULL,
    enabled        BOOLEAN  NOT NULL DEFAULT TRUE,
    swept_updated_at TIMESTAMPTZ,              -- last swept forge updated_at watermark; NULL = never swept
    list_etag      TEXT     NOT NULL DEFAULT '', -- conditional-GET etag for the repo LIST walk
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id      TEXT     NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
    PRIMARY KEY (tenant_id, forge_provider, forge_host, repo)
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
    tenant_id        TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
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
    tenant_id      TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
    PRIMARY KEY (tenant_id, forge_provider, forge_host, repo, kind, number)
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
    tenant_id          TEXT     NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
    PRIMARY KEY (tenant_id, forge_provider, forge_host, repo, kind, number),
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

-- One row per forge coordinate an AGENT-DRIVEN state transition last landed on
-- (compass-forge-state-transition design.md §Actor attribution): the consumable
-- memo that carries the acting agent's identity across the write→webhook gap.
-- A transition has no body, so the DL-050 owner header cannot attribute it, and
-- every Server-credential write presents the shared App bot login — durable
-- server-side correlation is the only channel that can name WHICH agent drove
-- the transition. The write chokepoint upserts this row strictly AFTER a
-- provider success (a rejected transition leaves no memo); the notify lane
-- resolves a STATE event's actor by consuming it.
--
-- Coordinate-aligned to forge_authored_artifacts: the SAME (tenant_id,
-- forge_provider, forge_host, repo, kind, number) PK, so a re-transition of one
-- artifact re-lands on the key (latest transition wins) rather than accreting
-- rows. This table is deliberately NOT forge_authored_artifacts itself: that
-- row is a write-once AUTHORSHIP fact whose DO UPDATE would destroy the
-- original create's F3 idempotency memo.
--
-- tenant_id is load-bearing, not incidental: two tenants legitimately hold the
-- SAME forge coordinate (TestForgeAuthoredTwoTenantsSameCoordinate), so without
-- it one tenant's memo could attribute another tenant's STATE event.
--
-- state is the APPLIED PORTABLE target, matched against the echoed event's
-- state — never a provider-native workflow-state name, hence CHECK IN
-- ('open', 'closed'). consumed_at NULL means "unconsumed"; the consume is a
-- single UPDATE … RETURNING that stamps it, so one memo attributes at most one
-- event and a concurrent second reader matches nothing. written_at is the
-- freshness anchor: a memo older than the reader's bound resolves no actor
-- (fail-open — an unattributed self-transition costs one redundant wake, never
-- a lost cross-agent signal). written_at is NOT this row's local mutation time
-- and is deliberately distinct from the created_at/updated_at pair below: it is
-- the chokepoint-supplied anchor ConsumeStateTransition compares against its
-- freshness bound, so it is set explicitly by the writer, never by the trigger.
CREATE TABLE forge_state_transitions (
    forge_provider   SMALLINT NOT NULL CHECK (forge_provider IN (1, 2, 3, 4)),
    forge_host       TEXT     NOT NULL,
    repo             TEXT     NOT NULL,
    kind             SMALLINT NOT NULL CHECK (kind IN (1, 2)),
    number           BIGINT   NOT NULL,
    state            TEXT     NOT NULL CHECK (state IN ('open', 'closed')),
    -- SINGLE-column FK, deliberately diverging from the composite
    -- (agent_account_id, owner_user_id) FK the otherwise field-for-field
    -- sibling forge_authored_artifacts carries: that row records an
    -- AUTHORSHIP pair whose (agent, that-agent's-owner) halves must be
    -- validated together, while this memo records only the ACTING agent, so
    -- there is no pair to validate. account_id is agent_accounts' PK, hence
    -- globally unique, so the single-column reference is fully constrained.
    agent_account_id TEXT     NOT NULL REFERENCES agent_accounts (account_id) ON DELETE RESTRICT,
    written_at       TIMESTAMPTZ NOT NULL,
    consumed_at      TIMESTAMPTZ,  -- NULL = unconsumed; stamped by the one-shot consume
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id        TEXT     NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
    PRIMARY KEY (tenant_id, forge_provider, forge_host, repo, kind, number)
);

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
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id          TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE)
);

-- ── Row-Level Security: tenant isolation (RIG-2861 T2 / RIG-3106) ────────────
-- The enforcement half of managed multi-tenancy, folded inline (Matt-ruled:
-- pre-live, no incremental migrations yet, so the ALTER/backfill/DROP-INDEX
-- mechanics 0002 used are unnecessary — every tenant-owned table above declares
-- tenant_id NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE) inline,
-- and the forge-coordinate tables fold tenant_id INTO their key so two tenants
-- may hold the same coordinate without collision). This section turns on RLS
-- with FORCE and installs the per-tenant policy.
--
-- Threat model: a request-path query runs as the non-owner compass_app role with
-- SET LOCAL compass.tenant_id = <tenant>, and reads/writes ONLY its own tenant's
-- rows; a query with no tenant GUC fails CLOSED (zero rows / rejected write),
-- never all rows and never an error escape. The four cross-tenant background
-- loops run under the narrowly-scoped compass_system BYPASSRLS role and are the
-- ONLY code allowed past the policies.
--
-- Load-bearing correctness notes:
--   * FORCE ROW LEVEL SECURITY: the migrating role OWNS every table, and a table
--     owner bypasses RLS by default — WITHOUT FORCE the policies are silently
--     inert for the exact role they must constrain. A superuser owner bypasses
--     even FORCE, so the request path NEVER runs as the owner: every request
--     statement issues SET LOCAL ROLE compass_app (a non-owner, non-BYPASSRLS
--     role) so the policies actually apply.
--   * GUC-unset semantics: the policy reads
--     (SELECT current_setting('compass.tenant_id', TRUE)) (missing_ok = TRUE, so
--     a never-set connection yields NULL, not SQLSTATE 42704) and guards it
--     non-empty. The scalar-subquery wrapper makes the planner evaluate the GUC
--     once per statement, not once per row.
--   * SET LOCAL only: tenant scoping is transaction-scoped, never a session SET,
--     so it cannot leak across a transaction-mode pooler's connection checkouts.
--   * tenant_id stamping: each tenant_id column DEFAULTs to the request GUC, so a
--     request-path INSERT that omits tenant_id is stamped with the acting tenant
--     — and the policy's WITH CHECK still rejects an INSERT made with no/empty GUC.
--     The one system-role write into a tenant table (owed_mentions via
--     RecordOwedMention, BYPASSRLS with no GUC) stamps tenant_id explicitly from
--     the owning account's FK instead (queries/delivery_cursors.sql).

-- compass_app: the request-path role. NOLOGIN (assumed via SET LOCAL ROLE from
-- the owner connection, never dialed directly), NO BYPASSRLS — the role the
-- policies constrain. compass_system: the cross-tenant background/system role,
-- BYPASSRLS, used ONLY by the N5 loops. Both are CLUSTER-GLOBAL objects, so
-- creation is idempotent (this migration re-runs per test schema against one
-- shared cluster) under store.go's cross-process advisory lock. Granted to the
-- current (owner) role so the owner connection may SET LOCAL ROLE into them.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'compass_app') THEN
        CREATE ROLE compass_app NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'compass_system') THEN
        CREATE ROLE compass_system NOLOGIN BYPASSRLS;
    END IF;
    -- Idempotent even if a prior partial run left compass_system without the
    -- attribute (or a future edit flips it): assert it explicitly.
    ALTER ROLE compass_system BYPASSRLS;
    EXECUTE format('GRANT compass_app, compass_system TO %I', current_user);
END $$;

-- Schema + object grants for both roles, resolved against the schema this
-- migration is applied into (the per-test isolation schema, or public in prod).
-- ALL TABLES / ALL SEQUENCES covers every object above; messages.seq (BIGSERIAL)
-- is among the sequences compass_app needs to INSERT a message. Idempotent.
DO $$
DECLARE
    sch text := current_schema();
BEGIN
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO compass_app, compass_system', sch);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %I TO compass_app, compass_system', sch);
    EXECUTE format('GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA %I TO compass_app, compass_system', sch);
END $$;

-- server_key_state withholds DELETE, revoked here because the schema-wide grant
-- above hands it out with everything else. The tripwire row is written once at
-- provision and updated in place on rotation, so DELETE is never needed — and
-- withholding it means the digest cannot be dropped to defeat the key-swap
-- check, which is a security property worth keeping. The pgtest asserts the
-- three remaining privileges AND the absence of DELETE, so this asymmetry is
-- pinned rather than incidental.
REVOKE DELETE ON server_key_state FROM compass_app, compass_system;

-- ENABLE + FORCE RLS + the per-tenant policy on every tenant-owned table. The
-- policy shape is the frozen T2 form: a scalar-subquery GUC read (evaluated once
-- per statement), a non-empty guard (fail-closed on an unset/empty GUC), and
-- tenant_id equality — as both USING (reads) and WITH CHECK (writes). Done in a
-- DO loop so the identical policy is never copy-pasted 25 times.
DO $$
DECLARE
    t text;
    tenant_tables text[] := ARRAY[
        'accounts',
        'user_accounts', 'agent_accounts', 'system_accounts', 'account_handles',
        'channel_groups', 'channels', 'channel_members', 'agent_workspaces',
        'topics', 'messages', 'channel_pins', 'secrets',
        'agent_sessions', 'agent_placements', 'session_bindings',
        'agent_session_transcript_entries', 'agent_session_archive_segments',
        'agent_delivery_cursors', 'owed_mentions', 'agent_activity',
        'agent_forge_subscriptions', 'forge_authored_artifacts',
        'linear_agent_sessions',
        'issues', 'forge_repo_subscriptions', 'forge_artifact_cursors',
        'forge_state_transitions'
    ];
BEGIN
    FOREACH t IN ARRAY tenant_tables LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($f$
            CREATE POLICY tenant_isolation ON %I
                USING ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
                       AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)))
                WITH CHECK ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
                       AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)))
        $f$, t);
    END LOOP;
END $$;

-- ── updated_at maintenance (RIG-3495) ───────────────────────────────────────
-- created_at + updated_at is the schema convention for every table from here
-- on, and updated_at is maintained by THIS trigger — never by a hand-written
-- `updated_at = now()` in a query file.
--
-- Hand-maintenance is the failure mode, not a hypothetical one: secrets.updated_at
-- rotted exactly that way. The column was declared here, read by DeclaredSecrets
-- and surfaced on SecretDeclaration.UpdatedAt, but no write statement in
-- queries/secrets.sql ever set it — so the value could only ever equal
-- created_at, and every caller reading it was reading a lie. A per-statement
-- convention that is invisible at the point a NEW write statement is added
-- cannot survive; a trigger is enforced by the table, so a write path added
-- later inherits it without anyone remembering.
--
-- The rule for a new table: give it created_at + updated_at with the usual
-- DEFAULT now(), add its name to updated_at_tables below, and set updated_at
-- NOWHERE else. There must be exactly one mechanism.
--
-- NOT covered, deliberately: issues.forge_updated_at and
-- forge_repo_subscriptions.swept_updated_at. Those are forge-supplied
-- watermarks — the remote's mutation time and the sweep high-water mark — not
-- this row's local mutation time, and their writers set them explicitly
-- (issues.sql's upsert even guards on the incoming value going forward). They
-- are differently named precisely so this trigger cannot reach them.
--
-- search_path safety: the function is SECURITY INVOKER (the default, and what
-- we want — a trigger doing NEW.updated_at = now() needs no elevated rights),
-- but it still runs with whatever search_path the CALLING session has set, so
-- an unqualified name in the body could be resolved against a schema the caller
-- controls. Pinning search_path on the function makes the body's resolution
-- independent of the caller. pg_catalog alone is enough and is the right pin
-- here: the body resolves exactly one name, now(), which lives in pg_catalog —
-- and this migration is applied into a per-test isolation schema as often as
-- into public (internal/pgshare hands each test its own schema via search_path),
-- so naming `public` would pin to a schema that is NOT the one holding these
-- tables. NEW/OLD are parser-level, not search_path-resolved.
CREATE FUNCTION set_updated_at() RETURNS TRIGGER
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END $$;

-- One BEFORE UPDATE ... FOR EACH ROW trigger per table carrying updated_at.
-- BEFORE (not AFTER) because the trigger mutates the row being written, and
-- UPDATE only (not INSERT) because the column's DEFAULT now() already stamps an
-- inserted row — a BEFORE INSERT here would merely re-derive the same value
-- while destroying the ability to insert a row with a deliberate updated_at.
-- An INSERT ... ON CONFLICT DO UPDATE fires this on the conflict path, which is
-- what makes every upsert in queries/ keep the column live for free. Done in a
-- DO loop for the same reason the RLS block above is: so the identical trigger
-- is never copy-pasted per table, and adding a table is one array entry.
DO $$
DECLARE
    t text;
    updated_at_tables text[] := ARRAY[
        'secrets',
        'agent_placements',
        'session_bindings',
        'agent_config_bundle',
        'model_registry',
        'forge_repo_subscriptions',
        'forge_state_transitions',
        'server_secrets',
        'server_key_state'
    ];
BEGIN
    FOREACH t IN ARRAY updated_at_tables LOOP
        EXECUTE format(
            'CREATE TRIGGER set_updated_at BEFORE UPDATE ON %I
                 FOR EACH ROW EXECUTE FUNCTION set_updated_at()', t);
    END LOOP;
END $$;
