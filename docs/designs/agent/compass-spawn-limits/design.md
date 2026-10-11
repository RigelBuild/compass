# Spawn limits: live-placement caps at the spawn handler

## Problem / Intent

Agent-driven spawn has no limit. The spawn/despawn record resolved its OQ-3 as
"no limit for MVP, we can follow up with a better limits design later (per
user, per agent, etc)" (Matt, 2026-07-29) and deferred limits to RIG-1574
(`docs/designs/agent/compass-agent-spawn-despawn/design.md` §OQ-3). Each spawn
is a real container or microVM, so one looping agent can provision until the
host runs out of memory. This record caps placed agents per owner where the
agent spawn tool lands, so a runaway tree gets an in-band `resource_exhausted`
tool error. The cap bounds one owner's tree, not the host. All owners share
the one Runner, so host capacity is OQ-4. The record also gives the operator a
way to free a slot: `compass agent despawn` (OQ-6).

## Approach

**One cap, one gate.** An agent spawn that would add a placement reserves a
slot for the caller's owner. At `N` placed-or-reserved agents the spawn is
refused with `connect.CodeResourceExhausted`. The relay already renders that
in-band: `lifecycleCallError` in `go/internal/runnerhub/relay_lifecycle.go`
sets "`Code: connect.CodeOf(err).String(),`". `LifecycleCallError.code` is an
open token ("`e.g. "not_found", "already_exists", "invalid_argument"`",
`proto/compass/v1/agent_gateway.proto`). The forge relay already sends this
token (`forgeCallError`, `go/internal/runnerhub/relay_forge.go`:
"`"resource_exhausted" for a rate limit`"). So **no proto or wire change** is
needed.

### What is counted: `agent_placements` rows

A row is a placed agent: one the server will make a container for. It is not
proof of a live container. The Runner's startup sweep
(`sweepStaleAgentContainers`, `go/internal/runner/run.go`) removes containers
and leaves the rows. Then `reprovision` (`go/server/lifecycle.go`)
"re-creates the container for an agent whose placement names one the Runner no
longer has". So the rows bound the containers that can exist. A session count
does not, because a stopped session keeps its container
(`go/internal/runner/host.go`: "The socket outlives the session (Stop/Start
reuses the container and its socket)").

Three paths delete a row: `DespawnAsAccount`, `RemoveAgentWorkspace`
(`go/server/service.go`), and `rollbackSpawn`. The rollback delete is
best-effort ("releasing placement failed; handle may stay burned"). A
Provision failure never reaches the rollback: either the Runner answered with
no container, or the hold below waits for its answer. The count joins rows to
`agent_accounts.owner_user_id`. A spawned peer inherits its caller's owner
(`SpawnAsAccount`: "the spawned peer inherits the CALLER'S OWNER"), so the
whole tree shares one budget.

### Reservations are durable rows

A slot in flight is a row in a new tenant-owned table,
`agent_spawn_reservations`, keyed by (owner, handle). DL-312 makes servers
"stateless L7-load-balanced instances (any Server handles any client
request)". An in-process count would let two replicas each admit the last
slot, and a restart would forget pending spawns (OQ-8). The row is not an
`agent_placements` row, because `freshStart`, `resumeSession`, and
`resumeOrReject` read any placement as a live container.

`ReserveSpawn` runs in one `beginTenantTx` transaction, the way
`ReparentAgent` takes `AcquireOwnerTreeLock`:

1. `pg_advisory_xact_lock(hashtext('spawn:' || owner))`, the per-owner idiom
   of `LockOwnerDM` (`queries/dm.sql`: "`hashtext('dm:' || $1)`"). Every
   replica waits on the same Postgres lock.
2. Delete the owner's rows whose `expires_at` has passed. Lease expiry is
   this delete, so no sweeper loop is needed.
3. If the handle's agent is placed, return `SpawnAlreadyPlaced`.
4. If the handle has a row, `pending` or `cleanup_pending`, return
   `SpawnInFlight`.
5. Count the owner's placements plus its rows. At `limit`, return
   `SpawnAtLimit`.
6. Insert a `pending` row with a fresh id and `expires_at = now() + lease`.
   Return `SpawnReserved` and the id.

The key is the handle, not the account id. `CreateAgent` mints the id itself
("`accountID := newID()`"), and the slot must be held before the account
exists. `CreateAgent` conflicts on the same key (`account_handles_owner_key
ON account_handles (tenant_id, owner_user_id, handle)`), so one key is one
agent. A release matches the row id, so a holder whose row expired cannot
delete a newer holder's row. `now()` is the database clock, which every
replica shares.

### The gate

`SpawnAsAccount` runs it once, after its existing role, owner, and user-shadow
checks:

1. `AgentByHandle(ctx, callerOwner, handle)`. If the agent exists and
   `PlacementForAgent` finds its row, return it with no reservation, because
   `resumeOrReject` promises "a completed-call retry gets its original
   answer".
2. Otherwise `ReserveSpawn`. `SpawnAtLimit` refuses before any write, so a
   looping agent at the cap adds no account, home channel, or tree edge.
   `SpawnInFlight` refuses with `connect.CodeAborted` (GC-3).
   `SpawnAlreadyPlaced` means a racing spawn placed the agent after step 1,
   so the gate runs step 1 again and returns its answer.
3. Under that one reservation, run the existing chain: `CreateAgent`, then
   `provisionAndStart`. A `CreateAgent` conflict still goes to
   `resumeOrReject`, with the reservation held.
4. The reservation is released when the chain ends: on success, because the
   placement row now holds the slot, or on a failure that left no container.
   Two exits keep it: a rollback whose Remove failed, and a timed-out
   Provision.

Between `RecordAgentPlacement` and the release, the agent counts twice. A
double count can only refuse, never over-admit.

### One in-flight spawn per agent

A retry of a handle whose row is `pending` gets `CodeAborted`. It runs no
Provision, Start, or rollback. Today such a retry reaches
`provisionAndStart` a second time. With the same request id the hub joins the
first Provision ("a retry with the same id joins it"). Both calls then Start
the one container, the loser's Start fails, and its rollback removes the
winner's container. The `SpawnAsAccount` "Concurrent-spawn window" comment
accepts this window. The reservation closes it, and T2 rewrites that comment.
A retry after the chain ends sees the placement in step 1.

### A failed rollback keeps its slot

`rollbackSpawn` logs a failed Remove and goes on, so the container may still
run. T2 makes `rollbackSpawn` report whether Remove succeeded. After a failed
Remove, `provisionAndStart` marks the row `cleanup_pending` with a fresh lease
and does not release it. Until that lease ends, the slot stays charged and a
retry of the handle gets `CodeAborted`. When the lease ends, a still-running
container is the leak OQ-4 backstops. A later retry's Provision of a live
container then fails with `errAlreadyProvisioned`, so no second container
starts.

### Holding a slot across a timed-out Provision

The chain runs under `spawnChainTimeout` (`var spawnChainTimeout = 60 *
time.Second`). The hub does not cancel the Runner's work at that deadline.
`waitCall` (`go/internal/runnerhub/router.go`) says "A ctx timeout leaves the
call in-flight on purpose: a retry with the same id joins it", and the
Runner's Provision arm has no deadline. Releasing the slot at the timeout
would let the Runner finish a container that nothing counts.

So when `hub.Provision` fails after the spawn ctx has expired, the
reservation stays `pending`. A goroutine rejoins the same Provision with the
same request id. When the spawn has no `client_request_id`, the server mints
one (`rand.Text()`), so the rejoin is exact. While the call is in flight the
hub joins it. After it completes, the Runner replays its result
(`dispatcher.handled` in `go/internal/runner/dispatch.go`: "a completed id
returns the recorded result"). When the rejoin returns:

- **Success.** Record the placement, then release. The agent is placed with no
  session, and the next message wakes it (`freshStart`).
- **Failure, stream detach, or its own deadline.** Release. A container that
  a detach orphans, or that a failed late `RecordAgentPlacement` leaves
  unrecorded, is the leak OQ-4 backstops.

The rejoin runs on `context.WithoutCancel(ctx)`, bounded by
`spawnReservationLease - spawnChainTimeout`, so it gives up before its row's
lease ends. If the server dies during the hold, the row stays charged until
the lease ends.

### The lease

`spawnReservationLease = 15 * time.Minute`. The lease must outlast any
Provision, or an expired row reopens the leak. The Runner gives Provision no
deadline, but each one-shot engine command has one. `defaultCommandTimeout =
120 * time.Second` (`go/internal/runtime/podman.go`: "a hung podman is
killed, never blocks the caller forever"), and Provision runs only a few such
commands. Provision pulls no image, because the image ensure runs at stack
start for "the runner (which refuses to boot without the image, DL-112)"
(`ImageEnsurer`, `go/internal/stack/adapters/image.go`). The lease is also
three times the CLI's whole `spawnTimeout = 5 * time.Minute`.

### Scope decisions

- **Owner only.** The issue names per-owner, per-user, and per-agent caps,
  which can layer. Per-user is per-owner here, because an agent's owner is a
  user. A per-agent (per-parent) limit on top of the owner cap is OQ-1.
- **The operator door is exempt but counted.** `SpawnAgent`,
  `ProvisionAgentWorkspace`, and the root seed (`svc.spawnAccount` in
  `go/server/serve_seed.go`) do not reserve. They are admin acts:
  `classifyProcedure` (`go/internal/auth/admin_gate.go`) lists
  `CompassServiceSpawnAgentProcedure` under `adminOnly` (OQ-3).
- **The wake path adds no rows.** `freshStart` and `reprovision` reuse a
  placement, and `RecordAgentPlacement` "is an UPSERT keyed on the agent".
- **Human recovery is in scope.** `newAgentCmd` (`go/cmd/compass/agent.go`) is
  "Spawn and inspect agent sessions (spawn, status)", so only an agent can free
  a slot today. T1 adds `compass agent despawn` over the existing admin RPCs
  (OQ-6).
- **Known gap: loops of failing spawns.** Below the cap, a spawn that fails at
  Start frees its slot and leaves its account ("the account is left
  UNPLACED"). Each attempt adds an account, a home channel, and a tree edge,
  and the cap never engages (OQ-7).

### Head-of-line blocking is already solved

The issue cites the Runner's serial command plane. Concurrent dispatch has
shipped since. `runSessions` (`go/internal/runner/dispatch.go`) runs each
command in its own goroutine, "so a slow Provision does not head-of-line-block
every other command". `provisionConcurrency = 8` bounds concurrent Provisions.
This record bounds the total.

### Configuration

`--max-agents-per-owner`, then `$COMPASS_MAX_AGENTS_PER_OWNER`, in the
flag-then-env order of `resolveUsageEventRetention`
(`go/cmd/compass-server/main.go`). Empty means the default, 16 (OQ-5). A
non-integer or a value below 1 fails boot with the flag named (OQ-2).
`ServeConfig.MaxAgentsPerOwner == 0` also means the default.

### Ledger

DL-479 records this decision. DL-077 stays Active. Its ruling is the despawn
RPC, and its "no spawn rate cap" clause names this issue as the follow-up.
Flipping it would retire the despawn ruling.

## Alternatives considered

- **Count sessions.** A spawn-then-stop loop passes a session cap while
  containers pile up.
- **Two checks:** an early refusal with no reservation, plus a reservation in
  `provisionAndStart`. That costs two counts per spawn, and two racing fresh
  spawns both pass the early check, so the loser leaves an unplaced account.
- **The reservation as an `agent_placements` row.** The name is derivable
  (`BuildSpec`, `go/internal/runner/spec.go`: "`name := d.NamePrefix +
  accountID`"). But `freshStart`, `resumeSession`, and `resumeOrReject` read
  any row as placed. A wake during the spawn would start a container that does
  not exist yet, and a retry would return the half-made agent as spawned.
- **An in-process limiter with a single-instance constraint.** It needs no
  migration, but it contradicts DL-312 and forgets pending spawns on restart
  (OQ-8 (b)).
- **No lease, hold until the Runner answers.** A server crash during the hold
  would charge the slot forever.
- **A rate limit instead of a cap.** A slow loop still fills the host. As a
  complement it is OQ-7.
- **Gate in the runnerhub relay.** DL-076 keeps the hub to identity ("the
  runnerhub resolves caller identity only"). The owner and the spawn outcome
  live in `lifecycleService`.

## Global Constraints

1. Go `1.26.0` (`go/go.mod`), `connectrpc.com/connect v1.20.0`. No proto or
   RPC change. One new migration, `0002_spawn_reservations.sql`. `0001` is
   never edited: `verifyChecksum` (`go/internal/store/store.go`) refuses "an
   applied migration … edited after it ran … add a new migration instead".
2. New SQL goes in `go/internal/store/queries/spawn_reservations.sql`, then
   `moon run compass-go:sqlc-gen`. `sqlc-drift` stays green.
3. Refusals:
   - at the cap, `connect.CodeResourceExhausted` with the message `spawn
     limit reached: this owner is at its maximum of %d placed agents; stop
     spawning and ask your owner or manager`. It never names another owner
     and never tells the agent to despawn;
   - a spawn of a handle that is already in flight, `connect.CodeAborted`
     with `a spawn of this agent is already in progress; wait, then retry`;
   - a store failure, `connect.CodeInternal`. The gate never fails open.
4. Every reservation query runs on the spawn's own ctx, as `AgentOwner` and
   `CreateAgent` do in `SpawnAsAccount`, never under `store.WithSystemRole`.
   That ctx carries the caller's tenant only after RIG-5110 (Deps).
5. Code comments cite `per DL-479`.
6. DB tests carry `pgtest` (`pgtest && unix` in `go/server`). Unit tests are
   untagged and pass `-race`. A test that shortens `spawnChainTimeout` or
   `spawnReservationLease` does not call `t.Parallel()`. A raw SQL insert sets
   `tenant_id` itself, because no GUC stamps the default.
7. Spec-impact for this design PR: none. T2 updates
   `docs/specs/product/compass.md` in its own PR.
8. One linear stack, T1 → T2 → T3. The cap must not ship before the human
   recovery path. T2 lands after RIG-5110.

## Deps

- **RIG-5110 (In Review).** `RelayLifecycleCall` passes the Runner's ctx
  with no tenant, so the store reads run as the bootstrap tenant. Under RLS a
  non-bootstrap caller's `AgentOwner` then misses. Its fix applies
  `runnerSessionCtx` (`go/internal/runnerhub/relay_comms.go`: "scopes a
  Runner-originated ctx to the tenant that binds sessionID to runnerID") to
  the lifecycle relay. This record does not re-solve it. The T2 pgtest proves
  the cap on that path for a non-bootstrap tenant.
- **DL-312 (Active).** The reservation table is what keeps the cap correct
  across replicas (OQ-8).

## Plan

### T1 — `compass agent despawn`

**Goal.** An operator tears down one of their agents without an agent. That
covers its sessions, its container, and its placement.

**Files.** New `go/cmd/compass/agent_despawn.go` and `agent_despawn_test.go`
(`//go:build unix`); `go/cmd/compass/agent.go`, `agent_spawn.go`;
`docs/self-host.md`.

**Interfaces.**

```go
func newAgentDespawnCmd() *cobra.Command // Use: "despawn --handle <handle>"
func runAgentDespawn(ctx context.Context, c agentSpawnClients, handle string, out, errOut io.Writer) error

// agent_spawn.go: now also returns the matched account's id; runAgentSpawn ignores it.
func qualifiedAgentHandle(ctx context.Context, c agentSpawnClients, handle string) (qualified, accountID string, err error)
```

**Semantics.** `newAgentCmd` adds the verb, and its `Short` becomes "Spawn,
inspect, and despawn agent sessions (spawn, status, despawn)". Each RPC runs
under `rpcTimeout`. The order mirrors `DespawnAsAccount`:

1. Resolve the handle with `qualifiedAgentHandle`. A miss returns its error,
   and no other RPC runs.
2. `GetAgentStatus` with no session id. `StopAgentSession` each status whose
   `agent_account_id` is the agent's. A Stop error is a warning on `errOut`.
3. `RemoveAgentWorkspace` with `runner.AgentContainerNamePrefix + accountID`.
   `AgentContainerNamePrefix` (`go/internal/runner/run.go`) is "prepended to
   the agent account id to form the container name", and the Runner's Remove
   also retires any live session the container hosts.
4. Print `despawned <owner/handle> (container <name>)`.

**Test cycle (red first).** `TestRunAgentDespawn*`, adding `GetAgentStatus`,
`StopAgentSession`, and `RemoveAgentWorkspace` to the `spawnCompass` fake.
Cases:

- only the matching session is stopped, and Remove names `compass-agent-<id>`;
- an unknown handle errors with no Stop or Remove;
- a Stop failure warns and Remove still runs;
- a Remove failure is returned and names the agent.

`TestRunAgentSpawn` still passes.

**Docs.** `docs/self-host.md` §Spawning an agent documents
`compass agent despawn --handle <handle>`. It stops the sessions and removes
the container and placement. The account stays.

**Smoke and done.** On a running stack:

1. `compass agent spawn --handle smoke --role manager`.
2. `compass agent despawn --handle smoke`.
3. `compass agent status` no longer lists its session.
4. A second `despawn` succeeds, because Remove is idempotent.

Done when `moon run compass-go:ci` is green.

### T2 — Per-owner cap on the agent spawn path

**Goal.** An agent spawn past the cap fails in-band with `resource_exhausted`,
at `DefaultMaxAgentsPerOwner` until T3. T2 lands after RIG-5110 (GC-8).

**Files.**

- Store: new `go/internal/store/migrations/0002_spawn_reservations.sql`,
  `queries/spawn_reservations.sql`, `spawn_reservations.go`, and
  `spawn_reservations_pgtest_test.go`; the generated `go/internal/store/db/`.
  Add the table to `tenantOwned` (`rls_pgtest_test.go`) and to the floor list
  in `TestUpdatedAtTriggerCatalogFloor` (`updated_at_pgtest_test.go`). Fix the
  `applyV1Only` comment ("With 0001 the only migration, that is the full
  schema") in `migrate_checksum_pgtest_test.go`. Correct the
  `RecordAgentPlacement` godoc in `agent_placements.go`: "Placements are
  PERMANENT for now: nothing anywhere deletes a row" is false, because
  `DeleteAgentPlacement` exists.
- Server: new `go/server/spawn_limit.go`, `spawn_limit_test.go`, and
  `lifecycle_limit_pgtest_test.go`; `lifecycle.go`, including the
  "Concurrent-spawn window" comment; `service_placement_pgtest_test.go`, which
  gains two `recordingRunner` knobs.
- Docs: `docs/specs/product/compass.md`, `docs/concepts/tools.md`.

**Interfaces.** The migration applies the RLS, grant, and trigger that 0001's
loops apply, so the new table matches every tenant-owned table:

```sql
-- 0002_spawn_reservations.sql: in-flight spawn slots (per DL-479).
CREATE TABLE agent_spawn_reservations (
    id            TEXT PRIMARY KEY,
    owner_user_id TEXT NOT NULL REFERENCES user_accounts (account_id) ON DELETE RESTRICT,
    handle        TEXT NOT NULL,
    state         TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'cleanup_pending')),
    expires_at    TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id     TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE)
);
-- One in-flight spawn per agent, keyed like account_handles_owner_key.
CREATE UNIQUE INDEX agent_spawn_reservations_handle_key
    ON agent_spawn_reservations (tenant_id, owner_user_id, handle);
ALTER TABLE agent_spawn_reservations ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_spawn_reservations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON agent_spawn_reservations
    USING ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
           AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)))
    WITH CHECK ((SELECT current_setting('compass.tenant_id', TRUE)) <> ''
           AND tenant_id = (SELECT current_setting('compass.tenant_id', TRUE)));
GRANT SELECT, INSERT, UPDATE, DELETE ON agent_spawn_reservations TO compass_app, compass_system;
CREATE TRIGGER set_updated_at BEFORE UPDATE ON agent_spawn_reservations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- queries/spawn_reservations.sql
-- name: LockOwnerSpawn :exec
SELECT pg_advisory_xact_lock(hashtext('spawn:' || $1));
-- name: DeleteExpiredSpawnReservations :exec
DELETE FROM agent_spawn_reservations WHERE owner_user_id = $1 AND expires_at <= now();
-- name: SpawnHandlePlaced :one
SELECT EXISTS (SELECT 1 FROM account_handles ah
                 JOIN agent_placements p ON p.agent_account_id = ah.account_id
                WHERE ah.owner_user_id = $1 AND ah.handle = $2);
-- name: SpawnHandleReserved :one
SELECT EXISTS (SELECT 1 FROM agent_spawn_reservations WHERE owner_user_id = $1 AND handle = $2);
-- name: CountOwnerSpawnSlots :one
SELECT (SELECT count(*) FROM agent_placements p
          JOIN agent_accounts a ON a.account_id = p.agent_account_id
         WHERE a.owner_user_id = $1)
     + (SELECT count(*) FROM agent_spawn_reservations r WHERE r.owner_user_id = $1);
-- name: InsertSpawnReservation :exec
INSERT INTO agent_spawn_reservations (id, owner_user_id, handle, expires_at)
VALUES (@id, @owner_user_id, @handle, now() + make_interval(secs => @lease_seconds::int));
-- name: DeleteSpawnReservation :exec
DELETE FROM agent_spawn_reservations WHERE id = $1;
-- name: MarkSpawnCleanupPending :execrows
UPDATE agent_spawn_reservations
   SET state = 'cleanup_pending', expires_at = now() + make_interval(secs => @lease_seconds::int)
 WHERE id = @id;
```

```go
// go/internal/store/spawn_reservations.go
type SpawnOutcome int

const (
	SpawnReserved SpawnOutcome = iota + 1
	SpawnAlreadyPlaced
	SpawnInFlight
	SpawnAtLimit
)

// ReserveSpawn runs §Reservations are durable rows in one beginTenantTx; id is
// set only for SpawnReserved. An empty owner or handle, limit < 1, or
// lease < time.Second is ErrInvalidArgument. Errors wrap as "store: reserve spawn: %w".
func (s *Store) ReserveSpawn(ctx context.Context, owner AccountID, handle string, limit int, lease time.Duration) (id string, outcome SpawnOutcome, err error)
// ReleaseSpawnReservation deletes row id; a missing row is nil.
func (s *Store) ReleaseSpawnReservation(ctx context.Context, id string) error
// MarkSpawnCleanupPending sets row id to cleanup_pending with a fresh lease; a missing row is ErrNotFound.
func (s *Store) MarkSpawnCleanupPending(ctx context.Context, id string, lease time.Duration) error

// go/server/spawn_limit.go
const DefaultMaxAgentsPerOwner = 16

var spawnReservationLease = 15 * time.Minute // a var so tests can shorten it (GC-6)
var errSpawnInFlight = errors.New("a spawn of this agent is already in progress; wait, then retry")

func spawnLimitError(limit int) error // CodeResourceExhausted with GC-3's text

type spawnReservations interface {
	ReleaseSpawnReservation(ctx context.Context, id string) error
	MarkSpawnCleanupPending(ctx context.Context, id string, lease time.Duration) error
}

// spawnSlot is one held row. The first of release, markCleanupPending, or
// handOff settles it; later calls, and every call on a nil slot, are no-ops.
type spawnSlot struct {
	st      spawnReservations
	id      string
	settled bool
}

func (s *spawnSlot) release(ctx context.Context)
func (s *spawnSlot) markCleanupPending(ctx context.Context)
func (s *spawnSlot) handOff() *spawnSlot // an unsettled slot on the same row

// go/server/lifecycle.go: lifecycleService gains maxAgentsPerOwner int; 0 means
// DefaultMaxAgentsPerOwner.
func (l *lifecycleService) placedPeer(ctx context.Context, agent store.AccountID) (*compassv1internal.SpawnPeerResponse, bool, error)
func (l *lifecycleService) reserveSpawn(ctx context.Context, owner store.AccountID, handle string) (*spawnSlot, error) // nil, nil for SpawnAlreadyPlaced
func (l *lifecycleService) resumeOrReject(ctx context.Context, slot *spawnSlot, callerOwner store.AccountID,
	req *compassv1internal.SpawnPeerRequest) (*compassv1internal.SpawnPeerResponse, error)
func (l *lifecycleService) provisionAndStart(ctx context.Context, slot *spawnSlot, agentID store.AccountID,
	persona, role string, req *compassv1internal.SpawnPeerRequest) (*compassv1internal.SpawnPeerResponse, error)
func (l *lifecycleService) rollbackSpawn(ctx context.Context, container, sessionID string) (removed bool)
func (l *lifecycleService) holdTimedOutProvision(ctx context.Context, slot *spawnSlot, provID string,
	agentID store.AccountID, preq *compassv1.ProvisionAgentWorkspaceRequest)

// service_placement_pgtest_test.go
func (r *recordingRunner) setProvisionGate(gate chan struct{}) // holds each Provision until gate closes, like setStartGate
func (r *recordingRunner) setFailRemove(v bool)               // Remove answers RUNNER_ERROR_CODE_INTERNAL
```

**Semantics.**

- After the shadow guard, `SpawnAsAccount` runs §The gate. Step 1 is
  `AgentByHandle`, then `placedPeer` on a hit. `ErrNotFound` and
  `ErrInvalidArgument` fall through, because `CreateAgent` still validates
  the handle. Any other error is `CodeInternal`. A placed result still
  reaches the DM auto-open tail.
- `reserveSpawn` passes `maxAgentsPerOwner` (or the default) and
  `spawnReservationLease`, and maps outcomes per GC-3. On `SpawnAlreadyPlaced`
  the gate reruns step 1. If the agent is unplaced again, it returns
  `errSpawnInFlight`.
- `SpawnAsAccount` defers `slot.release(ctx)` and threads the slot through
  the existing chain, so the row is released when the chain ends, whether it
  succeeded or failed. A slot settled by `markCleanupPending` or `handOff`
  ignores that release.
- `placedPeer` is `resumeOrReject`'s placed branch, factored out: the
  container and `SessionForAccount`, or `false` when unplaced.
- `provisionAndStart` uses `provID := req.GetClientRequestId()`, or
  `rand.Text()` when that is empty, as `hub.Provision`'s request id. If
  Provision fails while `ctx.Err() != nil`, it runs
  `go l.holdTimedOutProvision(ctx, slot.handOff(), provID, agentID, preq)` and
  returns the error.
- `rollbackSpawn` returns false when Remove fails. `DeleteAgentPlacement`
  still runs. Each of its three callers then calls `slot.markCleanupPending`.
- `holdTimedOutProvision` defers `slot.release`, then rejoins on
  `context.WithTimeout(context.WithoutCancel(ctx), spawnReservationLease-spawnChainTimeout)`.
  On success it calls `RecordAgentPlacement`. Every error is logged.
- `release` and `markCleanupPending` run on `context.WithoutCancel(ctx)`,
  bounded by `spawnChainTimeout` as `rollbackSpawn` is. The tenant therefore
  survives a spent deadline. A failed call is logged, and its row ends with
  its lease.

**Test cycle (red first).**

1. `TestReserveSpawn*` (store, pgtest):
   - it admits up to the limit, then returns `SpawnAtLimit`;
   - placements count, and another owner is independent;
   - a reserved handle is `SpawnInFlight`, and `SpawnReserved` again after
     release;
   - a placed handle is `SpawnAlreadyPlaced`, even at the cap;
   - a `cleanup_pending` row still counts and still blocks its handle;
   - the next reserve deletes a row whose `expires_at` raw SQL backdated, and
     frees its slot;
   - bad arguments are `ErrInvalidArgument`.
2. `TestReserveSpawnAcrossStores`: two `Store`s on one DSN, 32 goroutines
   split across them, limit 3, distinct handles. Exactly 3 are admitted.
   `TestReserveSpawnSurvivesRestart`: a row reserved on one `Store` still
   counts after that `Store` closes and a second one opens.
3. `TestOpenUpgradesV1AddsSpawnReservations`: `applyV1Only`, then `Open`
   applies 0002, and `ReserveSpawn` works.
4. `TestSpawnSlotSettlesOnce` (unit, fake `spawnReservations`): after
   `markCleanupPending` or `handOff`, `release` makes no store call, and the
   handed-off slot releases once. `TestSpawnLimitError`: its code and
   `Message()` match GC-3.
5. `TestSpawnLimit*` (server pgtest), on `newLifecycleFixture` with
   `f.lc.maxAgentsPerOwner = 2`. Use distinct `setContainerNames` and
   `setStartIDs`. At the cap:
   - a new handle is refused, with `provisionCount` unchanged and no account
     created;
   - a placed handle returns its container;
   - an unplaced existing handle is refused;
   - a second owner still spawns;
   - stopped sessions still count, and `f.svc.RemoveAgentWorkspace` or an
     agent despawn frees a slot;
   - a refused Start (`setFailStart(true)`) whose Remove succeeds frees its
     slot.
6. `TestSpawnInFlightRetryRefused`, with limit 4 so capacity is spare, and
   `setProvisionGate`.
   - Spawn A blocks in Provision.
   - A retry with the same handle and the same `client_request_id` gets
     `CodeAborted`.
   - Close the gate, and A succeeds.
   - Then `provisionCount() == 1`, `startCount() == 1`, one placement, and
     `!sawRemove(container)`.
   - A later retry returns the placed container.
7. `TestSpawnLimitHoldsTimedOutProvision`, with limit 1, a shortened
   `spawnChainTimeout`, and `setProvisionGate`.
   - A fails at the deadline.
   - A fresh spawn gets `CodeResourceExhausted`. A's handle, retried with its
     request id, gets `CodeAborted`, and `provisionCount()` is still 1.
   - Close the gate, then poll `PlacementForAgent` until A is placed.
   - There is no Start and no Remove, and the row is gone. A fresh spawn is
     still refused.
8. `TestSpawnFailedRollbackKeepsSlot`, with limit 1, `setFailStart(true)`, and
   `setFailRemove(true)`.
   - The spawn fails, and its row is `cleanup_pending`.
   - A fresh spawn gets `CodeResourceExhausted`, and the same handle gets
     `CodeAborted`.
   - Backdate `expires_at` and clear both knobs. A new handle then spawns.
9. `TestSpawnLimitNonBootstrapTenant`.
   - Seed tenant B with a raw `INSERT INTO tenants`, as
     `bearer_tenant_pgtest_test.go` does.
   - Under `store.WithTenant`, create its owner and caller agent, and
     `RecordSessionBinding` the caller on `fakeRunnerID`.
   - With `f.hub.SetLifecycleCaller(f.lc)` and limit 1, drive
     `f.hub.RelayLifecycleCall` on a background ctx.
   - The first spawn succeeds, and its reservation and placement rows carry
     tenant B. The second is an in-band `resource_exhausted`.
   - Without RIG-5110 this test fails.

**Spec and docs, same PR.** In `docs/specs/product/compass.md`, add
`### Requirement: Agent-initiated spawn is capped per owner` after "One
container per agent account", with these rules:

- a spawn that would add a placement past the per-owner maximum is refused
  with `RESOURCE_EXHAUSTED` before any account or container is created;
- the cap holds across server instances and restarts;
- a retry for a placed agent still succeeds;
- a spawn of an agent whose spawn is in flight is refused with `ABORTED` and
  starts nothing;
- a slot whose container may still exist stays charged until the Runner
  answers or its lease ends;
- the operator door is not refused.

Add two `#### Scenario:` blocks, one for the refusal and one for the retry. In
`docs/concepts/tools.md`, the `agents_spawn_peer` bullet says the call fails
with `resource_exhausted` at the owner's maximum. A despawn, by an agent or by
`compass agent despawn`, frees a slot.

**Smoke and done.** From `go/`, with `COMPASS_TEST_DATABASE_DSN` set, run
`go test -race -tags pgtest -run 'ReserveSpawn|UpgradesV1|SpawnSlot|SpawnLimit|SpawnInFlight|SpawnFailedRollback' ./internal/store/ ./server/`.
Done when this passes and `moon run compass-go:ci` is green.

### T3 — Configure the cap

**Goal.** The operator sets the cap with `--max-agents-per-owner` or
`$COMPASS_MAX_AGENTS_PER_OWNER`.

**Files.** `go/cmd/compass-server/main.go` and `main_test.go`;
`go/server/serve.go`, `sinks.go`, `lifecycle.go`; `docs/self-host.md`.

**Interfaces.**

```go
// main.go: serveFlags gains maxAgentsPerOwner *string from
// fs.String("max-agents-per-owner", "", ...), resolved flag-then-env with firstNonEmpty.
// "" gives server.DefaultMaxAgentsPerOwner; a non-integer or < 1 is
// fmt.Errorf("invalid --max-agents-per-owner %q: ...", v).
func resolveMaxAgentsPerOwner(v string) (int, error)

// serve.go: new ServeConfig field; 0 means DefaultMaxAgentsPerOwner.
MaxAgentsPerOwner int

// lifecycle.go: sets l.maxAgentsPerOwner = n; mirrors service.withRev.
func (l *lifecycleService) withMaxAgentsPerOwner(n int) *lifecycleService

// sinks.go: installs newLifecycleService(st, hub, commsSvc).withMaxAgentsPerOwner(n)
// and logs "agent spawn limit set" with max_agents_per_owner = the effective limit.
func wireHubServiceCycles(hub *runnerhub.Hub, commsSvc *comms.Comms, st *store.Store,
	issueBrd *board.IssueProjection, maxAgentsPerOwner int)
```

`Serve` passes `cfg.MaxAgentsPerOwner`. The waker's `lifecycleService` never
spawns, so it keeps the zero value.

**Test cycle (red first).** `TestBuildServeConfigMaxAgentsPerOwner`, shaped
like `TestBuildServeConfigUsageEventRetention`:

- unset gives 16;
- the flag sets the value, the env is the fallback, and the flag beats the
  env;
- `0`, `-1`, and `lots` fail with an error naming `--max-agents-per-owner`.

**Docs, same PR.** `docs/self-host.md` §Spawning an agent gains one paragraph:

- Agent spawns are capped at 16 placed agents per owner. The flag or env var
  sets the cap, minimum 1.
- `compass agent spawn` is never refused, but its agents count.
- An owner at or over the cap after an upgrade keeps its running agents, and
  retries and wakes still work.
- New spawns stay refused until `compass agent despawn` brings the owner below
  the cap, or the operator raises it.

**Smoke and done.** With the test's `--database`, `--socket`, and
`COMPASS_NATS_URL`:

- `compass-server --max-agents-per-owner 0` exits with the flag-named error;
- a boot with `COMPASS_MAX_AGENTS_PER_OWNER=3` logs `max_agents_per_owner=3`.

Done when the test passes and `moon run compass-go:ci` is green.

## Tasks

- [ ] T1: `compass agent despawn`, `qualifiedAgentHandle` account id,
      self-host doc
- [ ] T2: `0002_spawn_reservations.sql`, `ReserveSpawn`, single gate,
      timed-out Provision and failed-rollback holds, spec requirement, tools
      doc; lands after RIG-5110
- [ ] T3: flag and env, `ServeConfig.MaxAgentsPerOwner`, startup log,
      self-host upgrade note
- [ ] When Matt rules, set DL-479's status to `Active (Matt, <date>)`, or
      revise this record and DL-479 to match

## Open Questions

- **OQ-1: Owner only, or owner plus a per-parent limit on direct children?
  Load-bearing.**
  - With only the owner cap, one runaway subtree can use the whole budget.
    Later refusals then hit innocent siblings and the supervisor, not only the
    looping agent.
  - A per-parent limit keeps that blast radius inside the looping parent. It
    costs one more indexed count in `ReserveSpawn`
    (`agent_accounts_parent_idx`) and a second knob.

  **Recommendation:** owner only for now. The refusal is visible, T1 gives a
  way out, and a per-parent limit can be added later without changing the
  owner cap.
- **OQ-2: No unlimited mode. Non-load-bearing.** A value below 1 fails boot.
  **Recommendation:** keep it. A large value is the escape hatch.
- **OQ-3: Operator door exempt but counted. Non-load-bearing.** An exempt
  spawn can push an owner past `N`. New agent spawns then stay refused until T1
  or an agent despawn brings the owner below `N`. **Recommendation:** keep it.
  A capped seed could keep the supervisor from booting.
- **OQ-4: Per-Runner host ceiling. Non-load-bearing while reservations
  ship.** A ceiling enforced by the Runner is the only counter that sees
  orphan containers. It backstops the leaks a reservation cannot see: a
  stream detach during the hold, a failed late `RecordAgentPlacement`, or a
  lease that ends while a container still runs. It also bounds several owners
  on one host. **Recommendation:** a follow-up in which the Runner refuses a
  Provision when the host is full.
- **OQ-5: Default of 16. Non-load-bearing.** 16 is unmeasured: no real
  deployment's per-owner count was read. Check before ruling. Run
  `SELECT a.owner_user_id, count(*) FROM agent_placements p JOIN agent_accounts
  a ON a.account_id = p.agent_account_id GROUP BY 1` with the deployment's
  `compass.tenant_id` set, because RLS hides rows without it. An owner at or
  over 16 is refused new spawns from the first call after the upgrade (T3's
  self-host note). **Recommendation:** 16, raised per deployment if the check
  shows larger trees.
- **OQ-6: Human recovery path. Load-bearing.** The options:
  - (a) `compass agent despawn` over `RemoveAgentWorkspace`, in this record as
    T1;
  - (b) a named prerequisite issue;
  - (c) accept that an agent despawn is the only path.

  **Recommendation:** (a). Without it, an owner at the cap can recover only by
  having an agent tear down a sibling. This record is designed against (a).
- **OQ-7: Loops of failing spawns. Non-load-bearing.** Below the cap, each
  failed spawn leaves an account, a home channel, and a tree edge.
  **Recommendation:** a follow-up per-owner attempt budget, for example on
  unplaced accounts created within a time window.
- **OQ-8: Durable reservations, or an in-process limiter on one server.
  Load-bearing.** DL-312 (Active) makes servers "stateless L7-load-balanced
  instances (any Server handles any client request)". The options:
  - (a) durable Postgres reservations, one row per in-flight spawn keyed by
    (owner, handle). They are counted with placements under a per-owner
    advisory xact lock. A row is released on placement, on a known failure,
    or at lease expiry, and the lease outlasts any Provision (§The lease).
    The cap survives a restart and holds across replicas. The cost is one
    migration and one transaction per spawn.
  - (b) an in-process limiter plus a single-instance constraint. It needs no
    migration. It conflicts with DL-312, and a restart forgets pending spawns.
    `spawnMemoTTL` (`go/server/spawn.go`) already assumes "the server is
    single-instance". (b) would add a second such assumption.

  The cap is core capability, not managed product. It must hold for one
  self-hosted server and for many. `docs/concepts/self-host-and-managed.md`
  says "**The core must not assume it is single-tenant, nor assume it is
  managed.**" `docs/designs/meta/oss-core-managed-boundary/design.md` calls
  this "core-capability-under-managed-motivation". **Recommendation:** (a).
  This record is designed against (a).
