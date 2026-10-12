# Spawn limits: live-placement caps at the spawn handler

## Problem / Intent

Agent-driven spawn has no limit: the spawn/despawn record ruled "no limit for
MVP, we can follow up with a better limits design later (per user, per agent,
etc)" (`docs/designs/agent/compass-agent-spawn-despawn/design.md` §OQ-3).
Each spawn is a real container or microVM, so one looping agent can fill the
host. This record caps placed agents per owner at the agent spawn tool, and
adds `compass agent despawn` so a human can free a slot (OQ-6). It bounds one
owner's tree, not the host (OQ-4).

## Approach

**One cap, one gate.** An agent spawn that would add a placement reserves a
slot for the caller's owner. At `N` placed-or-reserved agents it is refused
with `connect.CodeResourceExhausted`. The relay renders that in-band:
`lifecycleCallError` (`go/internal/runnerhub/relay_lifecycle.go`) sets
"`Code: connect.CodeOf(err).String(),`", an open token that the forge relay
already uses for "`"resource_exhausted" for a rate limit`" (`forgeCallError`,
`go/internal/runnerhub/relay_forge.go`). No proto change.

### What is counted

A slot is an `agent_placements` row or a reservation row, joined to
`agent_accounts.owner_user_id`. A spawned peer "inherits the CALLER'S OWNER"
(`SpawnAsAccount`), so the whole tree shares one budget. A placement outlives
its container: `reprovision` (`go/server/lifecycle.go`) "re-creates the
container for an agent whose placement names one the Runner no longer has".
Sessions are not counted, because a stopped session keeps its container
(`go/internal/runner/host.go`: "Stop/Start reuses the container and its
socket").

### Reservations are durable rows

A reservation is a row in a new tenant-owned table,
`agent_spawn_reservations`, keyed by (owner, handle). DL-312 runs "stateless
L7-load-balanced instances (any Server handles any client request)", so an
in-process count would let two replicas admit the last slot, and a restart
would forget pending spawns (OQ-8).

`ReserveSpawn` runs in one `beginTenantTx`:

1. `pg_advisory_xact_lock(hashtext('spawn:' || owner))`, the `LockOwnerDM`
   idiom (`queries/dm.sql`).
2. Delete the owner's expired rows. This is lease expiry, with no sweeper.
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
  first because `RecordAgentPlacement` runs before Start, so a retry must not
  report a half-made agent as spawned.
- `SpawnAtLimit` is refused before any write, so a looping agent adds no
  account, channel, or tree edge.
- `SpawnAlreadyPlaced` returns the placement and session with no row, even at
  the cap: `resumeOrReject` promises "a completed-call retry gets its original
  answer".
- `SpawnReserved` runs the existing chain under the row.

The chain releases the row on a known outcome: success, because the placement
now holds the slot, or a failure that left no container. The in-flight
refusal also closes the "Concurrent-spawn window" that `SpawnAsAccount`
accepts today, where a same-id retry's failed Start removes the winner's
container.

### Ambiguous outcomes keep the slot

Two exits leave a container that may exist:

- **A timed-out Provision.** `waitCall` (`go/internal/runnerhub/router.go`):
  "A ctx timeout leaves the call in-flight on purpose". The Runner's Provision
  has no deadline. It queues on `provisionSem` ("ctx.Done releases a caller
  blocked for a slot on shutdown"), then runs engine commands and the config
  `Materialize`.
- **A failed rollback Remove.** `rollbackSpawn` logs it and goes on.

Neither has a time bound, so no timed release is safe. `provisionAndStart`
hands the row to `go settleSpawn` on `context.WithoutCancel(ctx)`, like
`dropLostSessionDetached` (`go/internal/runnerhub/relay_comms.go`). It renews
the lease every `spawnReservationLease / 3` and releases on the first observed
outcome:

1. **The Runner answers the Provision.** `settleSpawn` re-sends the same id.
   The hub joins an in-flight call, and the Runner replays a completed one
   ("a completed id returns the recorded result",
   `go/internal/runner/dispatch.go`). Success records the placement, and the
   next message wakes the agent (`freshStart`). A Runner error releases.
2. **The Runner answers a Remove.** After a failed rollback Remove, or a
   failed late `RecordAgentPlacement`, `settleSpawn` retries Remove every
   `spawnSettleRetry`. It mints a fresh id after an answered failure, because
   the Runner replays recorded failures. A `compass agent despawn` (T1) also
   clears it.
3. **The enrollment ends.** `Run` returns on "a dropped session stream", and
   `runSessions` cancels, "then join every in-flight command goroutine". The
   deferred `Close` tears down every container, "so no agent container
   outlives the Runner". A crashed Runner skips `Close`. On restart, the
   podman and Apple container engines sweep its containers "before sessions
   start" (`ListByOwner`). The microVM and host backends have no sweep, so a
   crash there can leak a container that nothing charges (OQ-4).

`settleSpawn` talks only to the spawn's own enrollment. `enroll` builds a new
router ("`router := newCommandRouter()`") for a Runner whose `handled` map is
empty, so a re-push there would provision a second container. T2 adds
`runnerhub.Pin`, the router and Runner id that `routerFor` reads together. The
first Provision, the rejoin, and every retried Remove use it.

A wedged, connected Runner keeps its slots, which is correct while it may
still build containers. A dead server stops renewing, but its Runner streams
drop too, so `Close` runs first. The lease, `5 * time.Minute`, is only that
crash backstop and exceeds the chain plus its rollback. Only a Postgres outage
longer than the lease can expire a held row early.

### Scope decisions

- **Owner only.** An agent's owner is a user, so per-user is per-owner. A
  per-parent limit is OQ-1.
- **The operator door is exempt but counted.** `SpawnAgent`,
  `ProvisionAgentWorkspace`, and the root seed (`svc.spawnAccount`,
  `go/server/serve_seed.go`) do not reserve. `classifyProcedure`
  (`go/internal/auth/admin_gate.go`) lists
  `CompassServiceSpawnAgentProcedure` under `adminOnly` (OQ-3).
- **The wake path adds no rows.** `freshStart` and `reprovision` reuse a
  placement. `RecordAgentPlacement` "is an UPSERT keyed on the agent".
- **Human recovery is in scope.** `newAgentCmd` is "Spawn and inspect agent
  sessions (spawn, status)", so today only an agent can free a slot (OQ-6).
- **Known gap.** A spawn failing at Start frees its slot and leaves the
  account "UNPLACED", so a failing loop never reaches the cap (OQ-7).

### Configuration and ledger

The flag is `--max-agents-per-owner`, then `$COMPASS_MAX_AGENTS_PER_OWNER`, in
the flag-then-env order of `resolveUsageEventRetention`
(`go/cmd/compass-server/main.go`). Empty is 16 (OQ-5). A non-integer or a
value below 1 fails boot (OQ-2). DL-479 records this. DL-077 stays Active: its
"no spawn rate cap" clause names this issue as the follow-up.

## Alternatives considered

- **Count sessions.** A spawn-then-stop loop passes while containers pile up.
- **Reserve with an `agent_placements` row.** A wake during the spawn would
  Start a container that does not exist yet.
- **An in-process limiter.** It needs no migration, but it contradicts DL-312
  and forgets pending spawns on restart (OQ-8 (b)).
- **A timed release under an end-to-end Provision bound.** The Runner has no
  such bound. `provisionSem` waits only on shutdown, and each engine command
  has its own timeout (`go/internal/runtime/podman.go`:
  "`defaultCommandTimeout = 120 * time.Second`"). A bound would change every
  engine backend.
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
   `store.WithSystemRole`. `settleSpawn` derives its ctx from it with
   `context.WithoutCancel`, so the tenant survives.
5. Code comments cite `per DL-479`.
6. DB tests carry `pgtest` (`pgtest && unix` in `go/server`). Unit tests pass
   `-race`. A test that shortens `spawnChainTimeout`,
   `spawnReservationLease`, or `spawnSettleRetry` does not call
   `t.Parallel()`. A raw insert sets `tenant_id` itself.
7. Spec impact for this design PR: none. T2 updates the spec.
8. One linear stack, T1 → T2 → T3, so the cap never ships before T1.

## Deps

- **RIG-5110 (Done, merged to `main` in #2123).** `RelayLifecycleCall` runs
  under the session's tenant: "`ctx, account, ok := h.runnerSessionAccount(ctx,
  runnerID, req.GetSessionId())`" (`go/internal/runnerhub/relay_lifecycle.go`
  on `main`). T2 rebases onto it, and test 9 pins the cap on that path.
- **DL-312 (Active).** Servers run as replicas (OQ-8).

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
`lifecycle.go`, `service_placement_pgtest_test.go`. Docs: the spec and
`docs/concepts/tools.md`.

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
// Ended reports that the pin's router has no live stream. Callers ask only after
// a command was sent on it, so it means the stream detached; a Runner opens one
// stream per process and enrolls anew on restart, so the router never re-attaches.
func (p Pin) Ended() bool

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
func (l *lifecycleService) rollbackSpawn(ctx context.Context, container, sessionID string) (removed bool)
func (l *lifecycleService) settleSpawn(ctx context.Context, slot *spawnSlot, s spawnSettle) // run with go

// service_placement_pgtest_test.go: recordingRunner gains three knobs, and
// commands() labels a Remove "remove <container>" instead of "other".
func (r *recordingRunner) setProvisionGate(gate chan struct{}) // holds each Provision, like setStartGate
func (r *recordingRunner) setFailRemove(v bool)               // Remove answers RUNNER_ERROR_CODE_INTERNAL
func (r *recordingRunner) hangUp()                            // closes the Sessions stream now; cleanup's close becomes a no-op
```

**Semantics.** `SpawnAsAccount` runs §The gate and defers `slot.release(ctx)`;
a despawn racing `SpawnAlreadyPlaced` returns `errSpawnInFlight`.
`provisionAndStart` provisions on its pin with `provID :=
req.GetClientRequestId()`, or `rand.Text()`, so a rejoin is exact. A
Provision error with `ctx.Err() != nil`, or a `rollbackSpawn` that returns
false, starts `go l.settleSpawn(ctx, slot.handOff(), …)`. Each settle attempt
runs on `context.WithTimeout(context.WithoutCancel(ctx), spawnChainTimeout)`,
and a transport error releases only when `pin.Ended()`.

**Test cycle (red first).**

1. `TestReserveSpawn*` (store pgtest): limit, placements, and owners; a
   reserved-and-placed handle is `SpawnInFlight`; a placed handle at the cap
   is `SpawnAlreadyPlaced`; expiry, renewal, and bad arguments.
2. `TestReserveSpawnAcrossStores`: two `Store`s, 32 goroutines, limit 3 admit
   exactly 3. `TestOpenUpgradesV1AddsSpawnReservations`: `Open` applies 0002.
3. `TestProvisionOnStaysOnItsEnrollment` (runnerhub, `synctest`,
   `enrollAttached`): after a re-enroll the old pin is `Ended`, and
   `ProvisionOn` on it sends nothing to the new router.
4. `TestSpawnSlotSettlesOnce`, `TestSpawnLimitError` (unit).
5. `TestSpawnLimit*` (server pgtest, limit 2): a new handle at the cap is
   refused with no Provision or account; a placed handle returns its
   container; another owner spawns; stopped sessions count; a despawn frees a
   slot.
6. `TestSpawnRetryRefusedInFlight`, with Provision gated, then Start gated
   (`setStartGate`, `waitForOneInflightStart`) after `RecordAgentPlacement`:
   a retry gets `CodeAborted`, never success, and the chain sends one
   Provision, one Start, and no Remove.
7. `TestSpawnSettlesTimedOutProvision` (limit 1, Provision gated): the row
   outlives two leases with `provisionCount()` 1; opening the gate places the
   agent and drops the row; `hangUp` drops it with no placement.
8. `TestSpawnSettlesFailedRollback` (limit 1, `setFailRemove(true)`), once per
   rollback: a refused Start; a failed `RecordAgentPlacement` (`execSQL` of
   `REVOKE INSERT ON agent_placements FROM compass_app`); a failed
   `RecordAgentSession` (`REVOKE INSERT ON agent_sessions FROM compass_app`),
   where `stop <session>` precedes `remove <container>`. Each leaves no
   placement and a held row; after `setFailRemove(false)` the row drops.
9. `TestSpawnLimitNonBootstrapTenant`: tenant B, seeded as
   `bearer_tenant_pgtest_test.go` does, spawns through
   `f.hub.RelayLifecycleCall` at limit 1. Its rows carry tenant B, and its
   second spawn gets an in-band `resource_exhausted`.

**Spec and docs, same PR.** `docs/specs/product/compass.md` gains
`### Requirement: Agent-initiated spawn is capped per owner` after "One
container per agent account", stating §The gate's four outcomes, the
observed-outcome release, and the exempt operator door, with a refusal
scenario and a retry scenario. The `agents_spawn_peer` bullet in
`docs/concepts/tools.md` names `resource_exhausted` and despawn.

**Smoke and done.** From `go/`, with `COMPASS_TEST_DATABASE_DSN` set, run
`go test -race -tags pgtest -run 'ReserveSpawn|UpgradesV1|ProvisionOn|SpawnSlot|SpawnLimit|SpawnRetry|SpawnSettles' ./internal/store/ ./internal/runnerhub/ ./server/`.
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
- [ ] T2: `0002_spawn_reservations.sql` with the composite owner FK,
      `ReserveSpawn` (in-flight before placed), `runnerhub.Pin`, the gate,
      `settleSpawn` with lease renewal, tests 1–9, spec requirement, and tools
      doc
- [ ] T3: flag and env, `ServeConfig.MaxAgentsPerOwner`, startup log,
      self-host upgrade note
- [ ] When Matt rules, set DL-479's status to `Active (Matt, <date>)`, or
      revise this record and DL-479 to match

## Open Questions

- **OQ-1: Owner only, or also per parent? Load-bearing.** One runaway subtree
  can spend the owner's budget, so refusals hit its siblings. A per-parent
  limit contains it for one more indexed count (`agent_accounts_parent_idx`)
  and a second knob. **Recommendation:** owner only. The refusal is visible,
  T1 is the way out, and a per-parent limit can be added later.
- **OQ-2: No unlimited mode. Non-load-bearing.** **Recommendation:** keep it.
  A large value is the escape hatch.
- **OQ-3: Operator door exempt but counted. Non-load-bearing.** An exempt
  spawn can push an owner past `N`. **Recommendation:** keep it. A capped seed
  could stop the supervisor from booting.
- **OQ-4: Per-Runner host ceiling. Non-load-bearing.** Only the Runner sees a
  container outside every row, such as one a crashed microVM or host-backend
  Runner leaves, and only it bounds several owners on one host.
  **Recommendation:** a follow-up Runner-side refusal when the host is full.
- **OQ-5: Default 16. Non-load-bearing.** It is unmeasured. Before ruling, run
  `SELECT a.owner_user_id, count(*) FROM agent_placements p JOIN
  agent_accounts a ON a.account_id = p.agent_account_id GROUP BY 1` with the
  deployment's `compass.tenant_id` set (RLS hides rows without it).
  **Recommendation:** 16, raised per deployment if trees are larger.
- **OQ-6: Human recovery path. Load-bearing.** (a) `compass agent despawn`
  (T1), (b) a prerequisite issue, or (c) agent despawn only.
  **Recommendation:** (a). Without it, an owner at the cap recovers only by
  having an agent tear down a sibling. Designed against (a).
- **OQ-7: Loops of failing spawns. Non-load-bearing.** Each failure leaves an
  account, a home channel, and a tree edge. **Recommendation:** a follow-up
  per-owner attempt budget.
- **OQ-8: Durable reservations, or an in-process limiter? Load-bearing.**
  - (a) Postgres rows under a per-owner advisory lock, released on an
    observed outcome, with a renewed lease as the crash backstop. The cap
    holds across restarts and replicas, for one migration and one
    transaction per spawn.
  - (b) An in-process limiter. It needs no migration, but it conflicts with
    DL-312 and forgets pending spawns on restart. It also adds a second
    single-instance assumption beside `spawnMemoTTL` (`go/server/spawn.go`:
    "the server is single-instance").

  The cap is core capability. `docs/concepts/self-host-and-managed.md` says
  "**The core must not assume it is single-tenant, nor assume it is
  managed.**", and `docs/designs/meta/oss-core-managed-boundary/design.md`
  calls this "core-capability-under-managed-motivation".
  **Recommendation:** (a). This record is designed against (a).
