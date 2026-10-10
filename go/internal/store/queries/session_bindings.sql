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
-- Under WithSystemRole (BYPASSRLS, no tenant GUC) these queries run cross-tenant:
--
--   * SessionBindingForAccount / SessionBindingAccount / SessionBindingForUpdate
--     are :one, so a predicate matching rows in several tenants returns an
--     arbitrary tenant's row without error.
--   * RecordSessionBinding does not fail closed: tenant_id DEFAULTs from the GUC,
--     which a pooled connection may leave empty, landing a row no tenant can see.
--   * DeleteSessionBindingsForRunner sweeps every tenant's bindings for that
--     runner id. Hub.enroll calls it this way on purpose, since a Runner is shared
--     across tenants; the returned tenant_id scopes each archive and end event.
--
-- SessionBindingTenants is the other deliberate system-role read.
-- session_bindings_pgtest_test.go pins the :one behaviour.
--
-- updated_at is maintained by the set_updated_at() trigger, never here.
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
-- clock_timestamp records after lock waits, unlike now() which uses tx start time.
-- name: StartComputeUsageInterval :exec
INSERT INTO compute_usage_events (
    id, interval_id, kind, occurred_at, agent_account_id, owner_user_id,
    session_id, runner_id
)
SELECT gen_random_uuid()::text, @interval_id::text, 'start', clock_timestamp(),
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
SELECT gen_random_uuid()::text, @interval_id::text, 'end', clock_timestamp(),
       a.account_id, a.owner_user_id, @session_id::text, @runner_id::text
  FROM agent_accounts AS a
 WHERE a.account_id = @agent_account_id::text
ON CONFLICT (tenant_id, interval_id, kind) DO NOTHING;

-- A binding an older server wrote during a rolling deploy has no start event.
-- Its created_at is the best start we hold, so the start is marked estimated.
-- name: EnsureComputeUsageIntervalStart :exec
INSERT INTO compute_usage_events (
    id, interval_id, kind, occurred_at, agent_account_id, owner_user_id,
    session_id, runner_id, estimated
)
SELECT gen_random_uuid()::text, b.usage_interval_id, 'start', b.created_at,
       b.agent_account_id, a.owner_user_id, b.session_id, b.runner_id, TRUE
  FROM session_bindings AS b
  JOIN agent_accounts AS a ON a.account_id = b.agent_account_id
 WHERE b.agent_account_id = @agent_account_id::text
ON CONFLICT (tenant_id, interval_id, kind) DO NOTHING;

-- What it DISPLACED comes from SessionBindingForUpdate above, not from a
-- RETURNING here. The binding update and event writes share the Store tx.
-- name: RecordSessionBinding :exec
INSERT INTO session_bindings (agent_account_id, session_id, runner_id, usage_interval_id, binding_version)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (tenant_id, agent_account_id) DO UPDATE
    SET session_id = EXCLUDED.session_id,
        runner_id = EXCLUDED.runner_id,
        usage_interval_id = EXCLUDED.usage_interval_id,
        binding_version = EXCLUDED.binding_version;

-- name: SessionBinding :one
SELECT agent_account_id, runner_id, binding_version FROM session_bindings WHERE session_id = $1;

-- name: SessionBindingForAccount :one
SELECT session_id, runner_id, binding_version FROM session_bindings WHERE agent_account_id = $1;

-- Both deletes also write an estimated start for a binding an older server made
-- without one; ON CONFLICT keeps any real start.
-- name: DeleteSessionBinding :exec
WITH d AS (
    DELETE FROM session_bindings AS b
     WHERE b.session_id = $1
    RETURNING b.tenant_id, b.usage_interval_id, b.agent_account_id,
              b.session_id, b.runner_id, b.created_at
), starts AS (
    INSERT INTO compute_usage_events (
        tenant_id, id, interval_id, kind, occurred_at, agent_account_id,
        owner_user_id, session_id, runner_id, estimated
    )
    SELECT d.tenant_id, gen_random_uuid()::text, d.usage_interval_id, 'start', d.created_at,
           d.agent_account_id, a.owner_user_id, d.session_id, d.runner_id, TRUE
      FROM d
      JOIN agent_accounts AS a ON a.account_id = d.agent_account_id
    ON CONFLICT DO NOTHING
    RETURNING 1
)
INSERT INTO compute_usage_events (
    tenant_id, id, interval_id, kind, occurred_at, agent_account_id,
    owner_user_id, session_id, runner_id
)
SELECT d.tenant_id, gen_random_uuid()::text, d.usage_interval_id, 'end', clock_timestamp(),
       d.agent_account_id, a.owner_user_id, d.session_id, d.runner_id
  FROM d
  JOIN agent_accounts AS a ON a.account_id = d.agent_account_id
 ORDER BY d.tenant_id, d.usage_interval_id
ON CONFLICT DO NOTHING;

-- DeleteSessionBinding limited to one write of the row: a re-bind since that
-- write set a new binding_version, so it is left alone. Returns rows removed;
-- Postgres runs every data-modifying CTE to completion.
-- name: DeleteSessionBindingVersion :one
WITH d AS (
    DELETE FROM session_bindings AS b
     WHERE b.session_id = $1 AND b.binding_version = $2
    RETURNING b.tenant_id, b.usage_interval_id, b.agent_account_id,
              b.session_id, b.runner_id, b.created_at
), starts AS (
    INSERT INTO compute_usage_events (
        tenant_id, id, interval_id, kind, occurred_at, agent_account_id,
        owner_user_id, session_id, runner_id, estimated
    )
    SELECT d.tenant_id, gen_random_uuid()::text, d.usage_interval_id, 'start', d.created_at,
           d.agent_account_id, a.owner_user_id, d.session_id, d.runner_id, TRUE
      FROM d
      JOIN agent_accounts AS a ON a.account_id = d.agent_account_id
    ON CONFLICT DO NOTHING
    RETURNING 1
), ends AS (
    INSERT INTO compute_usage_events (
        tenant_id, id, interval_id, kind, occurred_at, agent_account_id,
        owner_user_id, session_id, runner_id
    )
    SELECT d.tenant_id, gen_random_uuid()::text, d.usage_interval_id, 'end', clock_timestamp(),
           d.agent_account_id, a.owner_user_id, d.session_id, d.runner_id
      FROM d
      JOIN agent_accounts AS a ON a.account_id = d.agent_account_id
     ORDER BY d.tenant_id, d.usage_interval_id
    ON CONFLICT DO NOTHING
    RETURNING 1
)
SELECT count(*) FROM d;

-- The reconnect sweep, run by Hub.enroll under the system role because a Runner
-- is shared across tenants. :many with RETURNING: each removed row drives a
-- presence DISCONNECTED edge, a held-deliver reap, and a tenant-scoped archive.
-- DELETE ... RETURNING takes no ORDER BY, so the Store method sorts by session id.
-- name: DeleteSessionBindingsForRunner :many
WITH d AS (
    DELETE FROM session_bindings AS b
     WHERE b.runner_id = $1
    RETURNING b.tenant_id, b.usage_interval_id, b.agent_account_id,
              b.session_id, b.runner_id, b.created_at
), starts AS (
    INSERT INTO compute_usage_events (
        tenant_id, id, interval_id, kind, occurred_at, agent_account_id,
        owner_user_id, session_id, runner_id, estimated
    )
    SELECT d.tenant_id, gen_random_uuid()::text, d.usage_interval_id, 'start', d.created_at,
           d.agent_account_id, a.owner_user_id, d.session_id, d.runner_id, TRUE
      FROM d
      JOIN agent_accounts AS a ON a.account_id = d.agent_account_id
    ON CONFLICT DO NOTHING
    RETURNING 1
), ins AS (
    INSERT INTO compute_usage_events (
        tenant_id, id, interval_id, kind, occurred_at, agent_account_id,
        owner_user_id, session_id, runner_id
    )
    SELECT d.tenant_id, gen_random_uuid()::text, d.usage_interval_id, 'end', clock_timestamp(),
           d.agent_account_id, a.owner_user_id, d.session_id, d.runner_id
      FROM d
      JOIN agent_accounts AS a ON a.account_id = d.agent_account_id
     ORDER BY d.tenant_id, d.usage_interval_id
    ON CONFLICT DO NOTHING
    RETURNING 1
)
SELECT d.tenant_id, d.session_id, d.agent_account_id FROM d;

-- The one query here meant for the system role: a Runner-originated call carries
-- no tenant, so the hub reads the session's tenant cross-tenant, then acts under it.
-- :many so a session id minted in two tenants is refused, not resolved arbitrarily.
-- name: SessionBindingTenants :many
SELECT tenant_id FROM session_bindings WHERE session_id = $1 AND runner_id = $2;
