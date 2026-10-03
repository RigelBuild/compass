-- Session-binding queries (RIG-3108 / RIG-2861 §T4): the durable
-- (session -> agent account, Runner) binding the RunnerHub has so far held only
-- in RAM. The hand-written Store methods in internal/store/session_bindings.go
-- map these rows into the SessionBinding domain struct (the AccountID newtype is
-- done inline in the Go, as agent_placements does).
--
-- These request-path queries rely on SET LOCAL ROLE compass_app and the
-- compass.tenant_id GUC for RLS and tenant defaults. Delete queries carry the
-- deleted binding's tenant_id into their end-event insert explicitly.
--
-- It is NOT complete under WithSystemRole (tenant_tx.go), which arms the
-- BYPASSRLS compass_system role and NO tenant GUC. Every query here then runs
-- cross-tenant and unscoped, and each one changes meaning:
--
--   * SessionBindingForAccount / SessionBindingAccount / SessionBindingForUpdate
--     are :one, but with RLS gone their single predicate can match rows in
--     SEVERAL tenants. pgx's QueryRow takes the first and discards the rest
--     without error, so the caller gets a plausible answer from an arbitrary
--     tenant.
--   * DeleteSessionBindingsForRunner sweeps EVERY tenant's bindings for that
--     runner id — runner ids are not tenant-unique either.
--   * RecordSessionBinding does not fail closed. tenant_id DEFAULTs to
--     current_setting('compass.tenant_id', TRUE); on a pooled connection that
--     previously served an ARMED statement, the ended SET LOCAL leaves that
--     custom GUC defined-and-EMPTY rather than undefined, so the DEFAULT
--     resolves to '' and the NOT NULL is satisfied. The row lands stamped with a
--     tenant that does not exist — and tenant_id here has no FK to tenants
--     (accounts.tenant_id does), so nothing catches it. No RLS policy matches
--     '', so that row is then invisible to every tenant and releasable by
--     nothing on the request path. On a connection that never carried an armed
--     statement the GUC is genuinely undefined, the DEFAULT is NULL, and the
--     insert fails not-null instead — so which of the two a caller gets depends
--     on the pooled connection it draws.
--
-- Nothing calls these under the system role today (WithSystemRole is set at
-- delivery/consumer.go and runnerhub/hub.go); a PR3 caller that wants to must
-- scope them deliberately rather than inherit scoping from here.
-- session_bindings_pgtest_test.go pins the observed behaviour so it is recorded
-- rather than latent.
--
-- updated_at is NEVER assigned here: the set_updated_at() BEFORE UPDATE trigger
-- (0001_init.sql, RIG-3495) is the one mechanism, and a hand-written
-- `updated_at = now()` is the exact defect that convention removes.
--

-- The per-account serialization the bind takes FIRST, before it reads anything.
-- Auto-released at transaction end. It mirrors LockOwnerDM / LockOwnerCoordination
-- / AcquireOwnerTreeLock, with a DISTINCT key domain ('binding:') so a bind never
-- serializes behind a DM open, a coordination reconcile, or a reparent. hashtext
-- widens the text key to the int the advisory lock takes; a hash collision across
-- two accounts is a benign redundant wait, never a wrong result.
--
-- It is keyed on TENANT AND ACCOUNT. Every other lock in this package keys on an
-- account id alone, which is globally unique — but two tenants can legitimately
-- hold bindings for one account id (the PK is folded, see 0001_init.sql), and
-- those two binds are independent operations that must not block each other.
--
-- Why an advisory lock and not just the row lock below: FOR UPDATE on a row that
-- does NOT exist locks NOTHING, so two concurrent FIRST binds for one account
-- both read no prior value and neither blocks the other. The PK does serialize
-- their WRITES — the second INSERT waits on the first's uncommitted tuple and
-- resolves as ON CONFLICT DO UPDATE — but by then both have already read, so both
-- report "displaced nothing" while the second has in fact destroyed the first's
-- live binding. That session is then reported to NOBODY and its held deliveries
-- are stranded. Measured, not assumed: the PK is a write-ordering guarantee and
-- the displaced value is a READ, so the PK cannot cover it. This lock is taken
-- before the read, so it does.
-- name: LockSessionBindingAccount :exec
SELECT pg_advisory_xact_lock(hashtext('binding:' || $1 || ':' || $2));

-- The prior-value read follows the account advisory lock. It shares a tx with
-- event writes and the binding upsert. FOR UPDATE also protects against a writer
-- that reaches the row without taking the advisory lock.
--
-- The single-statement form this replaces (a `prev` CTE beside the upsert) could
-- not do that: under READ COMMITTED the CTE reads the statement-start snapshot
-- while ON CONFLICT DO UPDATE blocks on the row lock and then re-reads the LATEST
-- committed row, so the two halves saw different versions and the reported
-- displacement was wrong — two concurrent re-points away from sess-A both
-- reported sess-A, so the genuinely displaced sess-B was never reaped.
--
-- A MISS is not an error: a first-ever bind returns pgx.ErrNoRows and the Store
-- maps that to the empty displaced id.
-- The prior binding, including the original Runner and interval identity. The
-- Store closes a displaced interval before it writes a replacement binding.
-- name: SessionBindingForUpdate :one
SELECT b.session_id, b.usage_interval_id, b.runner_id
  FROM session_bindings AS b
 WHERE b.agent_account_id = $1
   FOR UPDATE;

-- Event writes share RecordSessionBinding's transaction, so neither half of an
-- interval can commit without its binding transition.
-- name: StartComputeUsageInterval :exec
INSERT INTO compute_usage_events (
    id, interval_id, kind, occurred_at, agent_account_id, owner_user_id,
    session_id, runner_id
)
SELECT gen_random_uuid()::text, @interval_id::text, 'start', now(),
       a.account_id, a.owner_user_id, @session_id::text, @runner_id::text
  FROM agent_accounts AS a
 WHERE a.account_id = @agent_account_id::text
ON CONFLICT (tenant_id, interval_id, kind) DO NOTHING;

-- The account owner is read in SQL so callers continue to supply only the
-- session binding identity.
-- name: EndComputeUsageInterval :exec
INSERT INTO compute_usage_events (
    id, interval_id, kind, occurred_at, agent_account_id, owner_user_id,
    session_id, runner_id
)
SELECT gen_random_uuid()::text, @interval_id::text, 'end', now(),
       a.account_id, a.owner_user_id, @session_id::text, @runner_id::text
  FROM agent_accounts AS a
 WHERE a.account_id = @agent_account_id::text
ON CONFLICT (tenant_id, interval_id, kind) DO NOTHING;

-- What it DISPLACED comes from SessionBindingForUpdate above, not from a
-- RETURNING here. The binding update and event writes share the Store tx.
-- name: RecordSessionBinding :exec
INSERT INTO session_bindings (agent_account_id, session_id, runner_id, usage_interval_id)
VALUES ($1, $2, $3, $4)
ON CONFLICT (tenant_id, agent_account_id) DO UPDATE
    SET session_id = EXCLUDED.session_id,
        runner_id = EXCLUDED.runner_id,
        usage_interval_id = EXCLUDED.usage_interval_id;

-- name: SessionBinding :one
SELECT agent_account_id, runner_id FROM session_bindings WHERE session_id = $1;

-- name: SessionBindingForAccount :one
SELECT session_id FROM session_bindings WHERE agent_account_id = $1;

-- name: DeleteSessionBinding :exec
WITH d AS (
    DELETE FROM session_bindings AS b
     WHERE b.session_id = $1
    RETURNING b.tenant_id, b.usage_interval_id, b.agent_account_id,
              b.session_id, b.runner_id
)
INSERT INTO compute_usage_events (
    tenant_id, id, interval_id, kind, occurred_at, agent_account_id,
    owner_user_id, session_id, runner_id
)
SELECT d.tenant_id, gen_random_uuid()::text, d.usage_interval_id, 'end', now(),
       d.agent_account_id, a.owner_user_id, d.session_id, d.runner_id
  FROM d
  JOIN agent_accounts AS a ON a.account_id = d.agent_account_id
ON CONFLICT DO NOTHING;

-- The reconnect sweep. Hub.enroll (internal/runnerhub/hub.go:905-957) clears
-- every binding when a Runner (re-)enrolls: a reconnecting Runner has no live
-- sessions, so a surviving binding would resolve a re-minted session id to a
-- stale account. A durable table does not forget on reconnect, so the sweep must
-- be explicit.
--
-- :many with RETURNING, deliberately NOT :exec. enroll snapshots the bindings
-- BEFORE clearing them (hub.go:912-928) because each cleared binding drives a
-- presence DISCONNECTED edge (RIG-1569 T8) and each cleared session id must be
-- reaped from the delivery held-deliver registry (RIG-1569 T3). A bare DELETE
-- would satisfy the invariant while silently dropping both side-effects, leaving
-- a long-WORKING agent stuck WORKING in the projection forever. RETURNING is
-- what preserves them, so the returned rows are load-bearing, not diagnostic.
-- A DELETE ... RETURNING takes no ORDER BY, so the Store method sorts the
-- returned slice by session id to keep a sweep pass deterministic and diffable.
-- name: DeleteSessionBindingsForRunner :many
WITH d AS (
    DELETE FROM session_bindings AS b
     WHERE b.runner_id = $1
    RETURNING b.tenant_id, b.usage_interval_id, b.agent_account_id,
              b.session_id, b.runner_id
), ins AS (
    INSERT INTO compute_usage_events (
        tenant_id, id, interval_id, kind, occurred_at, agent_account_id,
        owner_user_id, session_id, runner_id
    )
    SELECT d.tenant_id, gen_random_uuid()::text, d.usage_interval_id, 'end', now(),
           d.agent_account_id, a.owner_user_id, d.session_id, d.runner_id
      FROM d
      JOIN agent_accounts AS a ON a.account_id = d.agent_account_id
    ON CONFLICT DO NOTHING
    RETURNING 1
)
SELECT d.session_id, d.agent_account_id FROM d;

-- The one query here meant for the system role: a Runner-originated call carries
-- no tenant, so the hub reads the session's tenant cross-tenant, then acts under it.
-- :many so a session id minted in two tenants is refused, not resolved arbitrarily.
-- name: SessionBindingTenants :many
SELECT tenant_id FROM session_bindings WHERE session_id = $1 AND runner_id = $2;
