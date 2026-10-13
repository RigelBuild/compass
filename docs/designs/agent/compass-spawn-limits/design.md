# Spawn limits: live-placement caps at the spawn handler

## Open Questions

- **OQ-1: Owner only, or also per parent? Load-bearing.** A runaway subtree
  can spend the owner's budget and starve its siblings. A per-parent limit
  costs one more indexed count and a second knob. **Recommendation:** owner
  only; T1 is the way out.
- **OQ-2: No unlimited mode. Non-load-bearing.** **Recommendation:** keep it.
  A large value is the escape hatch.
- **OQ-3: Operator door exempt but counted. Non-load-bearing.** An exempt
  spawn can push an owner past `N`. **Recommendation:** keep it. A capped seed
  could stop the supervisor from booting.
- **OQ-4: Per-Runner host ceiling. Non-load-bearing.** Only the Runner sees a
  container outside every row, such as one whose cleanup failed (OQ-9), and
  only it bounds several owners on one host. **Recommendation:** a follow-up
  Runner-side refusal when the host is full.
- **OQ-5: Default 16. Non-load-bearing.** Unmeasured: before ruling, count
  placements per owner with `compass.tenant_id` set (RLS hides rows without
  it). **Recommendation:** 16, raised per deployment for larger trees.
- **OQ-6: Human recovery path. Load-bearing.** (a) `compass agent despawn`
  (T1), (b) a prerequisite issue, or (c) agent despawn only.
  **Recommendation:** (a); without it, only an agent can free a slot.
- **OQ-7: Loops of failing spawns. Non-load-bearing.** Each failure leaves an
  account, a home channel, and a tree edge. **Recommendation:** a follow-up
  per-owner attempt budget.
- **OQ-8: Durable reservations, or an in-process limiter? Load-bearing.**
  - (a) Postgres rows under a per-owner advisory lock, released on a
    placement or a confirmed Remove; a renewed lease is the crash backstop.
    It holds across restarts and replicas, for one migration and one
    transaction per spawn.
  - (b) An in-process limiter: no migration, but it conflicts with DL-312 and
    forgets pending spawns on restart.

  The cap is core capability
  (`docs/designs/meta/oss-core-managed-boundary/design.md`:
  "core-capability-under-managed-motivation"). **Recommendation:** (a).
- **OQ-9: A Remove success does not prove removal. Load-bearing; depends on
  OQ-8 (a).** See §Ambiguous outcomes keep the slot. Until (c), a dropped
  Runner holds an owner's slots until that server restarts, and despawn cannot
  free them.
  - (a) Ship as designed. Cost: a container leaked by a failed `Launch`
    cleanup, or a failed host or microVM kill, releases its slot uncounted.
  - (b) On a registry miss, `removeLocked` fails while `h.engine.Exists` is
    true. Host and microVM `Remove` keeps its entry, marked removing, until
    teardown succeeds, so `Exists` stays true; start and exec lookups,
    including the `h.handles[id]` and `m.sessions[id]` re-checks, skip it.
    `MicroVMRuntime.Start` keeps, marked removing, a partial boot whose
    `Shutdown` failed, and joins that error, so
    `AgentRuntime.createAndStart`'s Remove fails. A failed teardown keeps its
    slot until a retry succeeds or that server restarts. MicroVM is partial:
    `(*microvm.VM).Shutdown` (`microvm/launch.go`) "runs at most once", so no
    retry succeeds, and a failed `microvm.Launch` drops its cleanup error.
    Tests 11–13.
  - (c) Follow-up proto change: the Runner reports its sweep result or its
    owned containers at enroll, so a new enrollment can release.

  **Recommendation:** (b) in T2, so the cap counts leaked containers, and
  (c) as a follow-up.

## Problem / Intent

Agent-driven spawn has no limit: the spawn/despawn record ruled "no limit for
MVP" (`docs/designs/agent/compass-agent-spawn-despawn/design.md` §OQ-3).
Each spawn is a real container or microVM, so one looping agent can fill the
host. This record caps placed agents per owner at the agent spawn tool, and
adds `compass agent despawn` so a human can free a slot (OQ-6). It bounds one
owner's tree, not the host (OQ-4).

## Approach

**One cap, one gate.** An agent spawn that would add a placement reserves a
slot for the caller's owner. At `N` placed-or-reserved agents it is refused
with `connect.CodeResourceExhausted`. The relay renders that in-band:
`lifecycleCallError` (`go/internal/runnerhub/relay_lifecycle.go`) sets
"`Code: connect.CodeOf(err).String(),`".

### What is counted

A slot is an `agent_placements` row or a reservation row, joined to
`agent_accounts.owner_user_id`. A spawned peer "inherits the CALLER'S OWNER"
(`SpawnAsAccount`, `go/server/lifecycle.go`), so the whole tree shares one
budget. A placement outlives its container, because `reprovision`
(`go/server/lifecycle.go`) "re-creates the container".
Sessions are not counted, because a stopped session keeps its container
(`go/internal/runner/host.go`: "Stop/Start reuses the container and its
socket").

### Reservations are durable rows

A reservation is a row in a new tenant-owned table,
`agent_spawn_reservations`, keyed by (owner, handle). DL-312 runs "stateless
L7-load-balanced instances", so an in-process count would let two replicas
admit the last slot (OQ-8).

`ReserveSpawn` runs in one `beginTenantTx`:

1. `pg_advisory_xact_lock(hashtext('spawn:' || owner))`, the `LockOwnerDM`
   idiom (`go/internal/store/queries/dm.sql`: "`SELECT
   pg_advisory_xact_lock(hashtext('dm:' || $1));`").
2. Delete the owner's expired rows: lease expiry, with no sweeper.
3. A row for the handle: `SpawnInFlight`.
4. A placement for the handle: `SpawnAlreadyPlaced`.
5. Placements plus rows at `limit`: `SpawnAtLimit`.
6. Otherwise insert a row with `expires_at = now() + lease`: `SpawnReserved`.

The key is the handle, because `CreateAgent` mints the id ("`accountID :=
newID()`") after the slot is held and conflicts on the same key
(`account_handles_owner_key`). Release and renewal match the row id, so a
holder whose row expired cannot touch a newer holder's row.

### The gate

`SpawnAsAccount` calls `ReserveSpawn` once, after its role, owner, and shadow
checks:

- `SpawnInFlight` is refused with `connect.CodeAborted` (GC-3). It is checked
  first because `provisionAndStart` (`go/server/lifecycle.go`) runs "record
  the durable placement, Start", so a retry must not report a half-made agent
  as spawned.
- `SpawnAtLimit` is refused before any write, so a looping agent adds no
  account, channel, or tree edge.
- `SpawnAlreadyPlaced` returns the placement and session with no row, even at
  the cap: `resumeOrReject` promises "a completed-call retry gets its original
  answer".
- `SpawnReserved` runs the existing chain under the row.

The chain releases the row once the placement holds the slot or a Remove is
confirmed (§Ambiguous outcomes keep the slot). The in-flight refusal also
closes `SpawnAsAccount`'s "Concurrent-spawn window", where a same-id retry's
failed Start removes the winner's container.

### Ambiguous outcomes keep the slot

A container may exist after three exits:

- **A Provision error.** `waitCall` (`go/internal/runnerhub/router.go`): "A
  ctx timeout leaves the call in-flight on purpose", and the Runner's
  Provision has no deadline. A Runner error proves no more, because
  `AgentRuntime.Launch` (`go/internal/runtime/agent.go`) cleans up
  "Best-effort".
- **A failed rollback Remove.** `rollbackSpawn` logs it and goes on.
- **A dropped enrollment**, below.

`provisionAndStart` hands the row to `go settleSpawn` on
`context.WithoutCancel(ctx)`. It renews the lease every
`spawnReservationLease / 3` and releases only when:

1. **The Provision succeeds.** After a timeout, `settleSpawn` re-sends the
   same id. The hub joins an in-flight call, and the Runner replays a
   completed one ("a completed id returns the recorded result",
   `go/internal/runner/dispatch.go`). Success records the placement. A Runner
   error goes to 2.
2. **A Remove succeeds.** `settleSpawn` retries Remove every
   `spawnSettleRetry`. It mints a fresh id after an answered failure, because
   the Runner replays recorded failures.

One exception: a first Provision that `dispatch` refuses before it enqueues,
with no live stream or a full queue, is `runnerhub.ErrNotSent` and releases
at once.

`settleSpawn` talks only to the spawn's own enrollment, through
`runnerhub.Pin`. A new enrollment fronts a Runner whose `handled` map is
empty, so a re-push there provisions a second container.

**A Remove success is not yet proof.** `removeLocked`
(`go/internal/runner/host.go`) treats "A container the registry no longer
resolves" as "already gone", and `Launch` registers only after it provisions,
so a Remove after a failed `Launch` cleanup succeeds over a leak. Podman and
Apple container `Exists` ask the engine, but `HostRuntime.Exists`
(`go/internal/runtime/host_backend.go`) checks "handle-existence" and
`MicroVMRuntime.Exists` (`microvm_lifecycle.go`) the session table. Each
`Remove` drops that entry before it kills, so after a failed kill the next
Remove succeeds (OQ-9).

**No Runner signal confirms removal after a drop.** `agentHost.Close` "logs
each per-container error and discards it", the restart sweep
(`sweepStaleAgentContainers`, `go/internal/runner/run.go`) returns nothing,
and `EnrollRequest` carries no container list. After the pin's stream drops,
`RemoveOn` returns `ErrNotSent`, so the row stays charged while this server
lives (OQ-9).

The lease is only the crash backstop: it lapses once renewal stops, or early
if a Postgres outage outlasts it.

### Scope decisions

- **Owner only.** An agent's owner is a user, so per-user is per-owner (OQ-1).
- **The operator door is exempt but counted.** `SpawnAgent`,
  `ProvisionAgentWorkspace`, and the root seed (`svc.spawnAccount`,
  `go/server/serve_seed.go`) do not reserve (OQ-3).
- **The wake path adds no rows.** `freshStart` and `reprovision` reuse a
  placement. `RecordAgentPlacement` (`go/internal/store/agent_placements.go`)
  "is an UPSERT keyed on the agent".
- **Known gap.** A spawn failing at Start frees its slot and leaves the
  account "UNPLACED", so a failing loop never reaches the cap (OQ-7).

### Configuration and ledger

The flag `--max-agents-per-owner` beats `$COMPASS_MAX_AGENTS_PER_OWNER`, as in
`resolveUsageEventRetention` (`go/cmd/compass-server/main.go`). Empty is 16
(OQ-5). A non-integer or a value below 1 fails boot (OQ-2). DL-479 records
this. DL-077 stays Active: its "no spawn rate cap" clause names this issue as
the follow-up.

## Alternatives considered

- **Reserve with an `agent_placements` row.** A wake during the spawn would
  Start a container that does not exist yet.
- **A timed release.** The Runner has no end-to-end Provision bound, and one
  would change every engine backend.
- **No lease.** A server crash during a hold would charge the slot forever.
- **A rate limit.** A slow loop still fills the host (OQ-7).
- **Gate in the runnerhub relay.** DL-076: "the runnerhub resolves caller
  identity only".

## Global Constraints

1. Go `1.26.0` and `connectrpc.com/connect v1.20.0`. No proto change. One new
   migration, `0002_spawn_reservations.sql`. `0001` is never edited
   (`verifyChecksum`: "add a new migration instead").
2. New SQL goes in `go/internal/store/queries/spawn_reservations.sql`, then
   `moon run compass-go:sqlc-gen`. `sqlc-drift` stays green.
3. Refusals. The gate never fails open.
   - At the cap: `connect.CodeResourceExhausted`, `spawn limit reached: this
     owner is at its maximum of %d placed agents; stop spawning and ask your
     owner or manager`. It never names another owner or says to despawn.
   - In flight: `connect.CodeAborted`, `a spawn of this agent is already in
     progress; wait, then retry`.
   - A store failure: `connect.CodeInternal`.
4. Reservation queries run on the spawn's own ctx, never under
   `store.WithSystemRole`; `settleSpawn` keeps the tenant through
   `context.WithoutCancel`.
5. Code comments cite `per DL-479`.
6. DB tests carry `pgtest` (`pgtest && unix` in `go/server`). Unit tests pass
   `-race`. Tests that shorten `spawnChainTimeout`, `spawnReservationLease`,
   or `spawnSettleRetry` skip `t.Parallel()`. A raw insert sets `tenant_id`
   itself.
7. Spec impact: none here; T2 updates the spec.
8. One linear stack, T1 → T2 → T3, so the cap never ships before T1.

## Deps

- **RIG-5110 (Done, #2123).** `RelayLifecycleCall` runs under the session's
  tenant (`runnerSessionAccount`, `go/internal/runnerhub/relay_lifecycle.go`);
  test 9 pins the cap there.

## Plan

### T1 — `compass agent despawn`

**Goal.** An operator tears down one agent without an agent's help.

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

**Semantics.** In `DespawnAsAccount`'s order, each RPC under `rpcTimeout`:
resolve the handle (a miss sends nothing else); `StopAgentSession` for each
matching `GetAgentStatus` session (a failure warns); `RemoveAgentWorkspace` on
`runner.AgentContainerNamePrefix + accountID`; print `despawned <owner/handle>
(container <name>)`.

**Test cycle (red first).** `TestRunAgentDespawn*` on the `spawnCompass`
fake: only matching sessions stop; an unknown handle sends nothing; a Stop
failure warns and Remove runs; a Remove failure is returned.

**Docs, smoke, and done.** `docs/self-host.md` §Spawning an agent documents
the verb. Smoke: spawn `smoke`, despawn it, `compass agent status` omits it,
and a second despawn succeeds. Done when `moon run compass-go:ci` is green.

### T2 — Per-owner cap on the agent spawn path

**Goal.** An agent spawn past the cap fails in-band with `resource_exhausted`.

**Files.** Store: the migration, query file, `spawn_reservations.go`, its
pgtest, and regenerated `db/`; add the table to `tenantOwned` and
`TestUpdatedAtTriggerCatalogFloor`; fix the stale `applyV1Only` and
`RecordAgentPlacement` comments. Runnerhub: `commands.go`, `hub.go`. Server:
new `spawn_limit.go`, `spawn_limit_test.go`, `lifecycle_limit_pgtest_test.go`;
`lifecycle.go`, `service_placement_pgtest_test.go`. Runner and runtime, under
OQ-9 (b): `host.go`, `host_backend.go`, `microvm_lifecycle.go`, and their
package tests. Docs: the spec and `docs/concepts/tools.md`.

**Interfaces.** The migration copies 0001's per-table RLS, grant, and trigger.
The composite FK targets `user_accounts_account_tenant_key UNIQUE (account_id,
tenant_id)`, as `user_peers` does.

```sql
-- 0002_spawn_reservations.sql: in-flight spawn slots (per DL-479).
CREATE TABLE agent_spawn_reservations (
    id            TEXT PRIMARY KEY,
    owner_user_id TEXT NOT NULL,
    handle        TEXT NOT NULL,
    expires_at    TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    tenant_id     TEXT NOT NULL DEFAULT current_setting('compass.tenant_id', TRUE),
    FOREIGN KEY (owner_user_id, tenant_id) REFERENCES user_accounts (account_id, tenant_id) ON DELETE RESTRICT
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
-- name: SpawnHandleReserved :one
SELECT EXISTS (SELECT 1 FROM agent_spawn_reservations WHERE owner_user_id = $1 AND handle = $2);
-- name: SpawnHandlePlaced :one
SELECT EXISTS (SELECT 1 FROM account_handles ah
                 JOIN agent_placements p ON p.agent_account_id = ah.account_id
                WHERE ah.owner_user_id = $1 AND ah.handle = $2);
-- name: CountOwnerSpawnSlots :one
SELECT (SELECT count(*) FROM agent_placements p
          JOIN agent_accounts a ON a.account_id = p.agent_account_id
         WHERE a.owner_user_id = $1)
     + (SELECT count(*) FROM agent_spawn_reservations r WHERE r.owner_user_id = $1);
-- name: InsertSpawnReservation :exec
INSERT INTO agent_spawn_reservations (id, owner_user_id, handle, expires_at)
VALUES (@id, @owner_user_id, @handle, now() + make_interval(secs => @lease_seconds::int));
-- name: RenewSpawnReservation :execrows
UPDATE agent_spawn_reservations
   SET expires_at = now() + make_interval(secs => @lease_seconds::int)
 WHERE id = @id;
-- name: DeleteSpawnReservation :exec
DELETE FROM agent_spawn_reservations WHERE id = $1;
```

```go
// go/internal/runnerhub/commands.go. Provision becomes PinRunner then
// ProvisionOn, and relay becomes routerFor then the same dispatch on a router,
// so existing callers are unchanged.

// Pin is one Runner enrollment's command router and Runner id, read together.
type Pin struct {
	router   *commandRouter
	runnerID string
}

func (h *Hub) PinRunner() (Pin, error) // routerFor under h.mu
func (p Pin) RunnerID() string
// ErrNotSent marks a dispatch refused before the send queue: no live stream, or
// a full queue. dispatch appends it with %w, so ErrMsgNoSessionsStream stays the
// prefix, and connect.Error's Unwrap keeps it visible to errors.Is.
var ErrNotSent = errors.New("command not sent")

// ProvisionOn and RemoveOn relay on the pin's router only, never a newer
// enrollment's. They keep Provision's dedup id and bindContainer, and Remove's
// unbindContainer.
func (h *Hub) ProvisionOn(ctx context.Context, pin Pin, requestID string, accountID store.AccountID,
	req *compassv1.ProvisionAgentWorkspaceRequest) (*compassv1.ProvisionAgentWorkspaceResponse, error)
func (h *Hub) RemoveOn(ctx context.Context, pin Pin, requestID string,
	req *compassv1.RemoveAgentWorkspaceRequest) (*compassv1.RemoveAgentWorkspaceResponse, error)

// go/internal/store/spawn_reservations.go
type SpawnOutcome int

const (
	SpawnReserved SpawnOutcome = iota + 1
	SpawnInFlight
	SpawnAlreadyPlaced
	SpawnAtLimit
)

// ReserveSpawn runs §Reservations are durable rows in one beginTenantTx; id is
// set only for SpawnReserved. An empty owner or handle, limit < 1, or
// lease < time.Second is ErrInvalidArgument. Errors wrap as "store: reserve spawn: %w".
func (s *Store) ReserveSpawn(ctx context.Context, owner AccountID, handle string, limit int, lease time.Duration) (id string, outcome SpawnOutcome, err error)
// RenewSpawnReservation sets row id's expires_at to now() + lease; a missing row is ErrNotFound.
func (s *Store) RenewSpawnReservation(ctx context.Context, id string, lease time.Duration) error
// ReleaseSpawnReservation deletes row id; a missing row is nil.
func (s *Store) ReleaseSpawnReservation(ctx context.Context, id string) error

// go/server/spawn_limit.go
const DefaultMaxAgentsPerOwner = 16

var (
	spawnReservationLease = 5 * time.Minute  // crash backstop; settleSpawn renews every lease/3
	spawnSettleRetry      = 30 * time.Second // between settleSpawn's Remove attempts
)
var errSpawnInFlight = errors.New("a spawn of this agent is already in progress; wait, then retry")

func spawnLimitError(limit int) error // CodeResourceExhausted with GC-3's text

type spawnReservations interface {
	RenewSpawnReservation(ctx context.Context, id string, lease time.Duration) error
	ReleaseSpawnReservation(ctx context.Context, id string) error
}

// spawnSlot is one held row. The first release or handOff settles it; later
// calls, and every call on a nil slot, are no-ops.
type spawnSlot struct {
	st      spawnReservations
	id      string
	settled bool
}

func (s *spawnSlot) release(ctx context.Context) // on WithoutCancel(ctx), bounded by spawnChainTimeout
func (s *spawnSlot) renew(ctx context.Context)   // logs a failure
func (s *spawnSlot) handOff() *spawnSlot         // an unsettled slot on the same row

// spawnSettle is what settleSpawn waits on: a Provision to rejoin (provID and
// preq set), or a container to remove (container set).
type spawnSettle struct {
	pin       runnerhub.Pin
	agentID   store.AccountID
	provID    string
	preq      *compassv1.ProvisionAgentWorkspaceRequest
	container string
}

// go/server/lifecycle.go: lifecycleService gains maxAgentsPerOwner int; 0 means the default.
func (l *lifecycleService) reserveSpawn(ctx context.Context, owner store.AccountID, handle string) (*spawnSlot, error) // nil, nil for SpawnAlreadyPlaced
func (l *lifecycleService) placedPeer(ctx context.Context, agent store.AccountID) (*compassv1internal.SpawnPeerResponse, bool, error)
func (l *lifecycleService) resumeOrReject(ctx context.Context, slot *spawnSlot, callerOwner store.AccountID,
	req *compassv1internal.SpawnPeerRequest) (*compassv1internal.SpawnPeerResponse, error)
func (l *lifecycleService) provisionAndStart(ctx context.Context, slot *spawnSlot, agentID store.AccountID,
	persona, role string, req *compassv1internal.SpawnPeerRequest) (*compassv1internal.SpawnPeerResponse, error)
func (l *lifecycleService) rollbackSpawn(ctx context.Context, pin runnerhub.Pin, container, sessionID string) (removed bool) // Remove via RemoveOn
func (l *lifecycleService) settleSpawn(ctx context.Context, slot *spawnSlot, s spawnSettle) // run with go

// service_placement_pgtest_test.go: recordingRunner gains four knobs, and
// commands() labels a Remove "remove <container>" instead of "other".
func (r *recordingRunner) setProvisionGate(gate chan struct{}) // holds each Provision, like setStartGate
func (r *recordingRunner) setFailProvision(v bool)            // Provision answers RUNNER_ERROR_CODE_INTERNAL
func (r *recordingRunner) setFailRemove(v bool)               // Remove answers RUNNER_ERROR_CODE_INTERNAL
func (r *recordingRunner) hangUp()                            // closes the Sessions stream now; cleanup's close becomes a no-op
```

**Semantics.** `SpawnAsAccount` runs §The gate and defers `slot.release(ctx)`;
a despawn racing `SpawnAlreadyPlaced` returns `errSpawnInFlight`.
`provisionAndStart` provisions on its pin with `provID :=
req.GetClientRequestId()`, or `rand.Text()`, so a rejoin is exact. A
`PinRunner` error or `ErrNotSent` returns; the deferred release frees the
row. `ctx.Err() != nil` hands the rejoin to `settleSpawn`. Any other Provision
error runs `rollbackSpawn` on `runner.AgentContainerNamePrefix + agentID`; if
it returns false, `go l.settleSpawn(ctx, slot.handOff(), …)`. Each settle
attempt runs on
`context.WithTimeout(context.WithoutCancel(ctx), spawnChainTimeout)`.
`settleSpawn` never releases on `ErrNotSent`; it renews and retries.

**Test cycle (red first).**

1. `TestReserveSpawn*` (store pgtest): limit, placements, and owners; a
   reserved-and-placed handle is `SpawnInFlight`; a placed handle at the cap
   is `SpawnAlreadyPlaced`; expiry, renewal, and bad arguments.
2. `TestReserveSpawnAcrossStores`: two `Store`s, 32 goroutines, limit 3 admit
   exactly 3. `TestOpenUpgradesV1AddsSpawnReservations`: `Open` applies 0002.
3. `TestProvisionOnStaysOnItsEnrollment` (runnerhub, `synctest`,
   `enrollAttached`): after a re-enroll, `ProvisionOn` and `RemoveOn` on the
   old pin return `ErrNotSent` and send nothing to the new router.
4. `TestSpawnSlotSettlesOnce`, `TestSpawnLimitError` (unit).
5. `TestSpawnLimit*` (server pgtest, limit 2): a new handle at the cap is
   refused with no Provision or account; a placed handle returns its
   container; another owner spawns; stopped sessions count; a despawn frees a
   slot.
6. `TestSpawnRetryRefusedInFlight`, with Provision gated, then Start gated
   (`setStartGate`, `waitForOneInflightStart`) after `RecordAgentPlacement`:
   a retry gets `CodeAborted`; the chain sends one Provision, one Start, and
   no Remove.
7. `TestSpawnSettlesTimedOutProvision` (limit 1, Provision gated): the row
   outlives two leases with `provisionCount()` 1; opening the gate places the
   agent and drops the row. In a second run, `hangUp` with the Provision still
   gated: the row outlives two more leases, a new handle gets
   `resource_exhausted`, and no Remove is sent.
8. `TestSpawnSettlesFailedRollback` (limit 1, `setFailRemove(true)`), once per
   rollback: a refused Provision (`setFailProvision(true)`), where the Remove
   names `runner.AgentContainerNamePrefix + agentID`; a refused Start; a
   failed `RecordAgentPlacement` or `RecordAgentSession` (`execSQL` of
   `REVOKE INSERT` on its table `FROM compass_app`; for the session, `stop
   <session>` precedes `remove <container>`). Each leaves no placement and a
   row held past two leases; after `setFailRemove(false)` the row drops.
9. `TestSpawnLimitNonBootstrapTenant`: tenant B, seeded as
   `bearer_tenant_pgtest_test.go` does, spawns through
   `f.hub.RelayLifecycleCall` at limit 1. Its rows carry tenant B, and its
   second spawn gets an in-band `resource_exhausted`.
10. `TestSpawnLimitReleasesUnsentProvision` (limit 1): after `hangUp` and a
    `hub.Status` probe failing with `ErrMsgNoSessionsStream`, a spawn fails
    `CodeUnavailable` and leaves no row; after a new `attachRecordingRunner`,
    a second handle spawns.
11. Under OQ-9 (b): `TestRemoveFailsWhileUnregisteredContainerExists`
    (runner, stub engine, `newHostFixtureWithRecordingExec`, `liveSpec`).
    With `execErr` and `removeErr` set, Provision fails and leaves an
    unregistered container that `engine.Exists` reports; `host.Remove`
    errors. A never-provisioned name still removes as nil.
12. Under OQ-9 (b), `Test*FailedRemoveStaysVisible` per backend: `Remove`
    errors and `Exists` stays true. Podman and Apple container: an engine stub
    (`WithProgram`, `AppleContainerConfig.Program`) whose `rm` fails, which
    proves decoding only. Host, skipped as root: a `0o500` state-root `host`
    dir fails the state-dir delete; restored, `Remove` succeeds. No seam fails
    `killGroup`. MicroVM: `seamStart` and a new `fakeGuestVM.shutdownErr`,
    started or with `provErr` failing `Start`.
13. Under OQ-9 (b), `Test*RemovingEntryRefused`: host and microVM `Start`
    and `ExecStreaming` refuse a removing entry, also one marked mid-call by
    `afterSpawn` or `launchFunc`, and publish no live process or VM.

**Spec and docs, same PR.** `docs/specs/product/compass.md` gains
`### Requirement: Agent-initiated spawn is capped per owner` after "One
container per agent account", stating §The gate's four outcomes, the release
rule, and the exempt operator door, with refusal and retry scenarios. The
`agents_spawn_peer` bullet in `docs/concepts/tools.md` names
`resource_exhausted` and despawn.

**Smoke and done.** From `go/`, with `COMPASS_TEST_DATABASE_DSN` set, run
`go test -race -tags pgtest -run 'ReserveSpawn|UpgradesV1|ProvisionOn|SpawnSlot|SpawnLimit|SpawnRetry|SpawnSettles|RemoveFailsWhileUnregistered|FailedRemoveStaysVisible|RemovingEntryRefused' ./internal/store/ ./internal/runnerhub/ ./internal/runner/ ./internal/runtime/ ./server/`.
Done when this passes and `moon run compass-go:ci` is green.

### T3 — Configure the cap

**Goal.** The operator sets the cap.

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
like `TestBuildServeConfigUsageEventRetention`: unset gives 16; the flag beats
the env; `0`, `-1`, and `lots` fail naming `--max-agents-per-owner`.

**Docs, same PR.** `docs/self-host.md` §Spawning an agent states the default,
the knob, that `compass agent spawn` is never refused but counts, and that an
owner over the cap after an upgrade keeps its agents but cannot spawn more.

**Smoke and done.** `--max-agents-per-owner 0` exits with the flag-named
error; `COMPASS_MAX_AGENTS_PER_OWNER=3` logs `max_agents_per_owner=3`. Done
when the test passes and `moon run compass-go:ci` is green.

## Tasks

- [ ] T1: `compass agent despawn`, `qualifiedAgentHandle` account id,
      self-host doc
- [ ] T2: the migration, `ReserveSpawn`, `runnerhub.Pin` and `ErrNotSent`,
      the gate, `settleSpawn`, tests 1–10 (11–13 under OQ-9 (b)), spec, and
      tools doc
- [ ] T3: flag and env, `ServeConfig.MaxAgentsPerOwner`, startup log,
      self-host upgrade note
- [ ] When Matt rules, set DL-479's status to `Active (Matt, <date>)`, or
      revise this record and DL-479 to match
