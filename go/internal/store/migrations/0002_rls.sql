-- 0002_rls: Postgres Row-Level Security tenant isolation (RIG-3106 / T2 of the
-- frozen RIG-2861 record). This migration is the enforcement half of managed
-- multi-tenancy: T1 gave `accounts` a `tenant_id` and the bootstrap-tenant seed;
-- T2 propagates `tenant_id` onto every tenant-owned table, turns on RLS with
-- FORCE, and installs a per-tenant policy that gates every row on a
-- per-transaction GUC the store sets from the request's resolved tenant.
--
-- Threat model closed here: a request-path query, running as the non-owner
-- `compass_app` role with `SET LOCAL compass.tenant_id = <tenant>`, can read and
-- write ONLY its own tenant's rows; a query with no tenant GUC fails CLOSED
-- (zero rows / rejected write), never all rows and never an error escape. The
-- four cross-tenant background loops (delivery-cursor sweep, deliver-ack
-- advance, reattach recovery, lag-resync — N5/OQ-4, Matt-ruled option 1) run
-- under the narrowly-scoped `compass_system` BYPASSRLS role and are the ONLY
-- code allowed past the policies.
--
-- Design notes load-bearing to correctness:
--   * FORCE ROW LEVEL SECURITY (design.md:703-708): the migrating role OWNS every
--     table, and a Postgres table owner bypasses RLS by default — WITHOUT FORCE
--     the policies are silently inert for the exact role they must constrain. A
--     superuser owner bypasses even FORCE, so the request path NEVER runs as the
--     owner: every request statement issues `SET LOCAL ROLE compass_app` (a
--     non-owner, non-BYPASSRLS role) so the policies actually apply.
--   * GUC-unset semantics (design.md:709-719): the policy reads
--     `(SELECT current_setting('compass.tenant_id', true))` (missing_ok = true, so
--     a never-set connection yields NULL, not SQLSTATE 42704) and guards it
--     non-empty. The scalar-subquery wrapper makes the planner evaluate the GUC
--     once per statement, not once per row.
--   * SET LOCAL only (design.md:663-673): tenant scoping is transaction-scoped
--     (`set_config(..., true)`), never a session `SET`, so it cannot leak across a
--     transaction-mode pooler's connection checkouts.
--   * tenant_id stamping: each tenant_id column DEFAULTs to the request GUC
--     (`current_setting('compass.tenant_id', true)`), so a request-path INSERT that
--     omits tenant_id is stamped automatically with the acting tenant — and the
--     policy's WITH CHECK still rejects an INSERT made with no/empty GUC. The one
--     system-role write into a tenant table (owed_mentions via RecordOwedMention,
--     which runs BYPASSRLS with no GUC) stamps tenant_id explicitly from the
--     owning account's FK instead (queries/delivery_cursors.sql).
--
-- Ordering constraint (design brief "HARD ordering"): migrate() runs BEFORE
-- BootstrapTenant(), so at 0002 time the bootstrap tenant row does NOT yet
-- exist. This migration therefore never references a specific tenant id. On a
-- fresh (squashed-0001, seed-forward) database every table is empty at 0002
-- time, so the backfill UPDATEs touch zero rows; they are written correct for a
-- hypothetical data-carrying deployment regardless, resolving each row's tenant
-- through its FK chain to accounts.tenant_id (or, for the account-less forge
-- board tables, the single tenant in the degenerate single-tenant case).
--
-- Classification (Matt-ruled + scout-verified buckets, design brief §Table
-- classification):
--   A. Infrastructure, NO tenant_id / NO RLS: tenants, tokens,
--      agent_config_bundle.
--   B. Account-FK-rooted tenant-owned: tenant_id + FORCE RLS + policy.
--   C1. Forge-board (no account FK): tenant_id INTO the coordinate key.
--   C2. linear_agent_sessions: tenant-owned via manager_account_id.
--   `accounts` itself already carries tenant_id (T1) and IS tenant-owned, so it
--   is enabled+forced+policied here alongside bucket B (the bucket-B list
--   enumerates its FK-rooted dependents; the root gets the same treatment, the
--   fail-closed choice — otherwise the account rows themselves leak cross-tenant
--   and the policied subtype joins go inconsistent).

-- ── Roles ─────────────────────────────────────────────────────────────────
-- compass_app: the request-path role. NOLOGIN (assumed via SET LOCAL ROLE from
-- the owner connection, never dialed directly), NO BYPASSRLS — it is the role
-- the policies constrain. compass_system: the cross-tenant background/system
-- role, BYPASSRLS, used ONLY by the N5 loops. Both are CLUSTER-GLOBAL objects,
-- so creation is idempotent (this migration re-runs per test schema against one
-- shared cluster) and the whole migration runs under store.go's cross-process
-- advisory lock, which serializes concurrent creators. Granted to the current
-- (owner) role so the owner connection may SET LOCAL ROLE into them.
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
-- ALL TABLES / ALL SEQUENCES covers every 0001 object; messages.seq (BIGSERIAL)
-- is among the sequences compass_app needs to INSERT a message. Idempotent.
DO $$
DECLARE
    sch text := current_schema();
BEGIN
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO compass_app, compass_system', sch);
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA %I TO compass_app, compass_system', sch);
    EXECUTE format('GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA %I TO compass_app, compass_system', sch);
END $$;

-- ── Bucket B + accounts: add tenant_id, backfill, DEFAULT, NOT NULL ─────────
-- FK-order: parents before children (channels after channel_groups; transcript
-- + archive after agent_sessions), so a data-carrying backfill resolves through
-- an already-populated parent. `accounts` already has tenant_id (T1) — it only
-- gains the request-GUC DEFAULT so a policied INSERT that omits tenant_id is
-- still stamped.
ALTER TABLE accounts ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);

-- Account subtypes + handles: tenant_id via account_id -> accounts.
ALTER TABLE user_accounts ADD COLUMN tenant_id TEXT;
UPDATE user_accounts u SET tenant_id = (SELECT a.tenant_id FROM accounts a WHERE a.id = u.account_id);
ALTER TABLE user_accounts ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE user_accounts ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE agent_accounts ADD COLUMN tenant_id TEXT;
UPDATE agent_accounts ag SET tenant_id = (SELECT a.tenant_id FROM accounts a WHERE a.id = ag.account_id);
ALTER TABLE agent_accounts ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE agent_accounts ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE system_accounts ADD COLUMN tenant_id TEXT;
UPDATE system_accounts sa SET tenant_id = (SELECT a.tenant_id FROM accounts a WHERE a.id = sa.account_id);
ALTER TABLE system_accounts ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE system_accounts ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE account_handles ADD COLUMN tenant_id TEXT;
UPDATE account_handles ah SET tenant_id = (SELECT a.tenant_id FROM accounts a WHERE a.id = ah.account_id);
ALTER TABLE account_handles ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE account_handles ALTER COLUMN tenant_id SET NOT NULL;

-- Channel groups: owner_user_id -> accounts; a shared/owner-less group
-- (owner_user_id = '') has no owner to resolve from, so it falls back to the
-- single tenant in the degenerate case (no such rows exist on a fresh DB, and a
-- future shared-group create path stamps the acting tenant via the GUC DEFAULT).
ALTER TABLE channel_groups ADD COLUMN tenant_id TEXT;
UPDATE channel_groups cg SET tenant_id = COALESCE(
    (SELECT a.tenant_id FROM accounts a WHERE a.id = cg.owner_user_id),
    (SELECT id FROM tenants LIMIT 1));
ALTER TABLE channel_groups ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE channel_groups ALTER COLUMN tenant_id SET NOT NULL;

-- Channels: group_id -> channel_groups.tenant_id, else owner_account_id ->
-- accounts, else the degenerate single tenant.
ALTER TABLE channels ADD COLUMN tenant_id TEXT;
UPDATE channels c SET tenant_id = COALESCE(
    (SELECT g.tenant_id FROM channel_groups g WHERE g.id = c.group_id),
    (SELECT a.tenant_id FROM accounts a WHERE a.id = c.owner_account_id),
    (SELECT id FROM tenants LIMIT 1));
ALTER TABLE channels ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE channels ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE channel_members ADD COLUMN tenant_id TEXT;
UPDATE channel_members cm SET tenant_id = (SELECT a.tenant_id FROM accounts a WHERE a.id = cm.account_id);
ALTER TABLE channel_members ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE channel_members ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE agent_workspaces ADD COLUMN tenant_id TEXT;
UPDATE agent_workspaces w SET tenant_id = (SELECT a.tenant_id FROM accounts a WHERE a.id = w.agent_account_id);
ALTER TABLE agent_workspaces ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE agent_workspaces ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE topics ADD COLUMN tenant_id TEXT;
UPDATE topics t SET tenant_id = (SELECT a.tenant_id FROM accounts a WHERE a.id = t.created_by_account_id);
ALTER TABLE topics ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE topics ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE messages ADD COLUMN tenant_id TEXT;
UPDATE messages m SET tenant_id = (SELECT a.tenant_id FROM accounts a WHERE a.id = m.author_account_id);
ALTER TABLE messages ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE messages ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE channel_pins ADD COLUMN tenant_id TEXT;
UPDATE channel_pins cp SET tenant_id = (SELECT a.tenant_id FROM accounts a WHERE a.id = cp.pinned_by_account_id);
ALTER TABLE channel_pins ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE channel_pins ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE secrets ADD COLUMN tenant_id TEXT;
UPDATE secrets s SET tenant_id = (SELECT a.tenant_id FROM accounts a WHERE a.id = s.declared_by);
ALTER TABLE secrets ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE secrets ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE agent_sessions ADD COLUMN tenant_id TEXT;
UPDATE agent_sessions s SET tenant_id = (SELECT a.tenant_id FROM accounts a WHERE a.id = s.agent_account_id);
ALTER TABLE agent_sessions ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE agent_sessions ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE agent_placements ADD COLUMN tenant_id TEXT;
UPDATE agent_placements p SET tenant_id = (SELECT a.tenant_id FROM accounts a WHERE a.id = p.agent_account_id);
ALTER TABLE agent_placements ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE agent_placements ALTER COLUMN tenant_id SET NOT NULL;

-- Transcript hot-tail + archive manifest: session_id -> agent_sessions.tenant_id
-- (backfilled just above).
ALTER TABLE agent_session_transcript_entries ADD COLUMN tenant_id TEXT;
UPDATE agent_session_transcript_entries e SET tenant_id = (SELECT s.tenant_id FROM agent_sessions s WHERE s.session_id = e.session_id);
ALTER TABLE agent_session_transcript_entries ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE agent_session_transcript_entries ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE agent_session_archive_segments ADD COLUMN tenant_id TEXT;
UPDATE agent_session_archive_segments seg SET tenant_id = (SELECT s.tenant_id FROM agent_sessions s WHERE s.session_id = seg.session_id);
ALTER TABLE agent_session_archive_segments ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE agent_session_archive_segments ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE agent_delivery_cursors ADD COLUMN tenant_id TEXT;
UPDATE agent_delivery_cursors dc SET tenant_id = (SELECT a.tenant_id FROM accounts a WHERE a.id = dc.agent_account_id);
ALTER TABLE agent_delivery_cursors ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE agent_delivery_cursors ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE owed_mentions ADD COLUMN tenant_id TEXT;
UPDATE owed_mentions om SET tenant_id = (SELECT a.tenant_id FROM accounts a WHERE a.id = om.agent_account_id);
ALTER TABLE owed_mentions ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE owed_mentions ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE agent_activity ADD COLUMN tenant_id TEXT;
UPDATE agent_activity aa SET tenant_id = (SELECT a.tenant_id FROM accounts a WHERE a.id = aa.agent_account_id);
ALTER TABLE agent_activity ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE agent_activity ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE agent_forge_subscriptions ADD COLUMN tenant_id TEXT;
UPDATE agent_forge_subscriptions s SET tenant_id = (SELECT a.tenant_id FROM accounts a WHERE a.id = s.agent_account_id);
ALTER TABLE agent_forge_subscriptions ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE agent_forge_subscriptions ALTER COLUMN tenant_id SET NOT NULL;

ALTER TABLE forge_authored_artifacts ADD COLUMN tenant_id TEXT;
UPDATE forge_authored_artifacts f SET tenant_id = (SELECT a.tenant_id FROM accounts a WHERE a.id = f.agent_account_id);
ALTER TABLE forge_authored_artifacts ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE forge_authored_artifacts ALTER COLUMN tenant_id SET NOT NULL;

-- ── C2: linear_agent_sessions via manager_account_id (no FK, backfill by id) ─
ALTER TABLE linear_agent_sessions ADD COLUMN tenant_id TEXT;
UPDATE linear_agent_sessions l SET tenant_id = (SELECT a.tenant_id FROM accounts a WHERE a.id = l.manager_account_id);
ALTER TABLE linear_agent_sessions ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE linear_agent_sessions ALTER COLUMN tenant_id SET NOT NULL;

-- ── C1: forge-board tables — tenant_id INTO the coordinate key ──────────────
-- No account FK; backfill from the single tenant in the degenerate case (a
-- no-op on the empty fresh DB). The coordinate unique key/PK gains tenant_id so
-- two tenants may watch the SAME forge coordinate without colliding.
ALTER TABLE issues ADD COLUMN tenant_id TEXT;
UPDATE issues SET tenant_id = (SELECT id FROM tenants LIMIT 1) WHERE tenant_id IS NULL;
ALTER TABLE issues ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE issues ALTER COLUMN tenant_id SET NOT NULL;
DROP INDEX issues_coordinate_key;
CREATE UNIQUE INDEX issues_coordinate_key
    ON issues (tenant_id, forge_provider, forge_host, repo, number);

ALTER TABLE forge_repo_subscriptions ADD COLUMN tenant_id TEXT;
UPDATE forge_repo_subscriptions SET tenant_id = (SELECT id FROM tenants LIMIT 1) WHERE tenant_id IS NULL;
ALTER TABLE forge_repo_subscriptions ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE forge_repo_subscriptions ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE forge_repo_subscriptions DROP CONSTRAINT forge_repo_subscriptions_pkey;
ALTER TABLE forge_repo_subscriptions ADD PRIMARY KEY (tenant_id, forge_provider, forge_host, repo);

ALTER TABLE forge_artifact_cursors ADD COLUMN tenant_id TEXT;
UPDATE forge_artifact_cursors SET tenant_id = (SELECT id FROM tenants LIMIT 1) WHERE tenant_id IS NULL;
ALTER TABLE forge_artifact_cursors ALTER COLUMN tenant_id SET DEFAULT current_setting('compass.tenant_id', true);
ALTER TABLE forge_artifact_cursors ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE forge_artifact_cursors DROP CONSTRAINT forge_artifact_cursors_pkey;
ALTER TABLE forge_artifact_cursors ADD PRIMARY KEY (tenant_id, forge_provider, forge_host, repo, kind, number);

-- ── N7 (RIG-2921): account_handles org-scoped uniqueness ────────────────────
-- The two partial-unique indexes become tenant-scoped: two organizations may
-- each hold `@matt` (global tier) or each own an agent handle with no
-- cross-tenant collision. RLS on account_handles already filters a resolver to
-- the caller's tenant, so the resolver queries need no text change — the tenant
-- filter is the policy.
DROP INDEX account_handles_global_key;
DROP INDEX account_handles_owner_key;
CREATE UNIQUE INDEX account_handles_global_key
    ON account_handles (tenant_id, handle) WHERE owner_user_id IS NULL;
CREATE UNIQUE INDEX account_handles_owner_key
    ON account_handles (tenant_id, owner_user_id, handle) WHERE owner_user_id IS NOT NULL;

-- ── ENABLE + FORCE RLS + per-tenant policy on every tenant-owned table ──────
-- Applied AFTER all backfills so the owner-run backfill UPDATEs above ran
-- unconstrained. The policy shape is the frozen T2 form: a scalar-subquery GUC
-- read (evaluated once per statement), a non-empty guard (fail-closed on an
-- unset/empty GUC), and tenant_id equality — as both USING (reads) and WITH
-- CHECK (writes). Done for each table in a DO loop so the identical policy is
-- never copy-pasted 25 times.
DO $$
DECLARE
    t text;
    tenant_tables text[] := ARRAY[
        'accounts',
        'user_accounts', 'agent_accounts', 'system_accounts', 'account_handles',
        'channel_groups', 'channels', 'channel_members', 'agent_workspaces',
        'topics', 'messages', 'channel_pins', 'secrets',
        'agent_sessions', 'agent_placements',
        'agent_session_transcript_entries', 'agent_session_archive_segments',
        'agent_delivery_cursors', 'owed_mentions', 'agent_activity',
        'agent_forge_subscriptions', 'forge_authored_artifacts',
        'linear_agent_sessions',
        'issues', 'forge_repo_subscriptions', 'forge_artifact_cursors'
    ];
BEGIN
    FOREACH t IN ARRAY tenant_tables LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($f$
            CREATE POLICY tenant_isolation ON %I
                USING ((SELECT current_setting('compass.tenant_id', true)) <> ''
                       AND tenant_id = (SELECT current_setting('compass.tenant_id', true)))
                WITH CHECK ((SELECT current_setting('compass.tenant_id', true)) <> ''
                       AND tenant_id = (SELECT current_setting('compass.tenant_id', true)))
        $f$, t);
    END LOOP;
END $$;
