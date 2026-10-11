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
best-effort ("releasing placement failed; handle may stay burned"). A failed
Provision never reaches the rollback; the hold below covers it. The count
joins rows to `agent_accounts.owner_user_id`. A spawned peer inherits its
caller's owner (`SpawnAsAccount`: "the spawned peer inherits the CALLER'S
OWNER"), so the whole tree shares one budget.

### The gate

`SpawnAsAccount` runs it once, after its existing role, owner, and user-shadow
checks:

1. `AgentByHandle(ctx, callerOwner, handle)`. If the agent exists and
   `PlacementForAgent` finds its row, return it with no reservation, because
   `resumeOrReject` promises "a completed-call retry gets its original
   answer".
2. Otherwise reserve a slot. At the cap this refuses before any write, so a
   looping agent at the cap adds no account, home channel, or tree edge.
3. Under that one slot, run `CreateAgent` and then `provisionAndStart`. For an
   unplaced existing agent, run `provisionAndStart` alone. A `CreateAgent`
   conflict still goes to `resumeOrReject`, with the slot held.
4. `provisionAndStart` releases the slot once `RecordAgentPlacement` succeeds,
   because the row now holds it. Every other exit releases it, except a timed
   out Provision.

### Holding a slot across a timed-out Provision

The chain runs under `spawnChainTimeout` (`var spawnChainTimeout = 60 *
time.Second`). The hub does not cancel the Runner's work at that deadline.
`waitCall` (`go/internal/runnerhub/router.go`) says "A ctx timeout leaves the
call in-flight on purpose: a retry with the same id joins it", and the
Runner's Provision arm has no deadline. Releasing the slot at the timeout
would let the Runner finish a container that nothing counts. A fresh-handle
loop would then pass the cap exactly when the host is slow.

So when `hub.Provision` fails and the spawn ctx has expired, the slot is kept.
A goroutine rejoins the same Provision, with the same request id, on
`context.WithoutCancel(ctx)`. While the call is in flight the hub joins it.
After it completes, the Runner replays its result (`dispatcher.handled` in
`go/internal/runner/dispatch.go`: "a completed id returns the recorded
result"). When the rejoin returns:

- **Success.** Record the placement, then release. The agent is placed with no
  session, and the next message wakes it (`freshStart`). Do not remove the
  container. Its name is derived from the account, so a concurrent resume of
  the same handle may own it.
- **Failure or stream detach.** Release. Any container that a detach orphans,
  or that a failed late `RecordAgentPlacement` leaves unrecorded, is the leak
  OQ-4 backstops.

There is one goroutine per held slot, so the cap bounds the goroutines. When
the spawn has no `client_request_id`, the server mints the Provision id itself
(`rand.Text()`), so every rejoin is exact. A same-id retry during the hold
joins the same call but holds a second slot meanwhile. That double count can
only refuse, never over-admit.

### Concurrency

`spawnLimiter` keeps one mutex and in-flight count per owner. `reserve` holds
the owner's mutex across the store count and the increment, and `release`
takes the same mutex. The invariant is *rows + reservations ≤ N* per owner,
and owners never contend. Per-owner entries are never deleted, like the map
behind `lockContainer` (`go/internal/runner/host.go`: "entries are NEVER
deleted"). A count error fails closed. The limiter assumes one server process,
as the spawn memo does (`spawnMemoTTL`, `go/server/spawn.go`: "the server is
single-instance and re-derives live truth from the Runner on restart"). After
a restart the rows are the truth.

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
- **A store-authoritative intent row before Provision.** The name is derivable
  (`BuildSpec`, `go/internal/runner/spec.go`: "`name := d.NamePrefix +
  accountID`"), and a row survives a restart. But `freshStart`,
  `resumeSession`, and `resumeOrReject` read any row as placed. A wake during
  the spawn would start a container that does not exist yet, and a retry would
  return the half-made agent as spawned.
- **Release a held slot after an expiry.** The Runner's Provision has no
  deadline, so any expiry is a guess, and a short one reopens the leak.
- **A rate limit instead of a cap.** A slow loop still fills the host. As a
  complement it is OQ-7.
- **Gate in the runnerhub relay.** DL-076 keeps the hub to identity ("the
  runnerhub resolves caller identity only"). The owner and the spawn outcome
  live in `lifecycleService`.

## Global Constraints

1. Go `1.26.0` (`go/go.mod`), `connectrpc.com/connect v1.20.0`. No proto, RPC,
   or migration change.
2. New SQL goes in `go/internal/store/queries/agent_placements.sql`, then
   `moon run compass-go:sqlc-gen`. `sqlc-drift` stays green.
3. The refusal is `connect.CodeResourceExhausted` with the message `spawn
   limit reached: this owner is at its maximum of %d placed agents; stop
   spawning and ask your owner or manager`. It never names another owner and
   never tells the agent to despawn. A count failure is `connect.CodeInternal`.
   The gate never fails open.
4. The count runs on the spawn's own ctx, as `AgentOwner` and `CreateAgent` do
   in `SpawnAsAccount`, never under `store.WithSystemRole`.
5. Code comments cite `per DL-479`.
6. DB tests carry `pgtest` (`pgtest && unix` in `go/server`). Unit tests are
   untagged and pass `-race`. A test that shortens `spawnChainTimeout` does
   not call `t.Parallel()`.
7. Spec-impact for this design PR: none. T2 updates
   `docs/specs/product/compass.md` in its own PR.
8. One linear stack, T1 → T2 → T3. The cap must not ship before the human
   recovery path.

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
at `DefaultMaxAgentsPerOwner` until T3.

**Files.**

- Store: `go/internal/store/queries/agent_placements.sql`, the generated
  `go/internal/store/db/`, `agent_placements.go`, and
  `agent_placements_test.go`. Also correct the stale "Placements are PERMANENT
  for now" paragraph in the `RecordAgentPlacement` godoc.
- Server: new `go/server/spawn_limit.go`, `spawn_limit_test.go`, and
  `lifecycle_limit_pgtest_test.go`; `lifecycle.go`;
  `service_placement_pgtest_test.go`, which gains the Provision gate.
- Docs: `docs/specs/product/compass.md`, `docs/concepts/tools.md`.

**Interfaces.**

```sql
-- name: CountAgentPlacementsForOwner :one
SELECT COUNT(*)
  FROM agent_placements p
  JOIN agent_accounts a ON a.account_id = p.agent_account_id
 WHERE a.owner_user_id = $1;
```

```go
// store: an empty owner is ErrInvalidArgument; errors wrap as
// "store: count agent placements for owner: %w".
func (s *Store) CountAgentPlacementsForOwner(ctx context.Context, owner AccountID) (int, error)

// go/server/spawn_limit.go
const DefaultMaxAgentsPerOwner = 16

type placementCounter interface {
	CountAgentPlacementsForOwner(ctx context.Context, owner store.AccountID) (int, error)
}
type spawnLimiter struct {
	limit   int
	counter placementCounter
	mu      sync.Mutex // guards owners get-or-create only
	owners  map[store.AccountID]*ownerSlots
}
type ownerSlots struct {
	mu       sync.Mutex // held across the count and the inflight change
	inflight int
}
func newSpawnLimiter(limit int, counter placementCounter) *spawnLimiter // limit < 1 means the default
// reserve refuses (GC-3) when rows+inflight >= limit.
func (l *spawnLimiter) reserve(ctx context.Context, owner store.AccountID) (*spawnSlot, error)
func (s *spawnSlot) release()     // idempotent; a no-op after keep
func (s *spawnSlot) keep() func() // hands the slot over; the func releases it once

// go/server/lifecycle.go: lifecycleService gains limits *spawnLimiter, set by
// newLifecycleService to newSpawnLimiter(0, st).
func (l *lifecycleService) placedPeer(ctx context.Context, agent store.AccountID) (*compassv1internal.SpawnPeerResponse, bool, error)
func (l *lifecycleService) resumeOrReject(ctx context.Context, slot *spawnSlot, callerOwner store.AccountID,
	req *compassv1internal.SpawnPeerRequest) (*compassv1internal.SpawnPeerResponse, error)
func (l *lifecycleService) provisionAndStart(ctx context.Context, slot *spawnSlot, agentID store.AccountID,
	persona, role string, req *compassv1internal.SpawnPeerRequest) (*compassv1internal.SpawnPeerResponse, error)
func (l *lifecycleService) holdTimedOutProvision(ctx context.Context, release func(), provID string,
	agentID store.AccountID, preq *compassv1.ProvisionAgentWorkspaceRequest)

// service_placement_pgtest_test.go: blocks each Provision until gate closes, like setStartGate.
func (r *recordingRunner) setProvisionGate(gate chan struct{})
```

**Semantics.**

- `SpawnAsAccount` runs §The gate and defers `slot.release()`. A placed or
  idempotent result still reaches the existing DM auto-open tail. An
  `AgentByHandle` error other than `ErrNotFound` is `CodeInternal`.
- `placedPeer` is `resumeOrReject`'s placed branch, factored out. It returns
  the container and `SessionForAccount`, or `false` when unplaced.
- `provisionAndStart` uses `provID := req.GetClientRequestId()`, or
  `rand.Text()` when that is empty. `provID` is the request id for
  `hub.Provision`.
- If Provision fails while `ctx.Err() != nil`, run
  `go l.holdTimedOutProvision(ctx, slot.keep(), provID, agentID, preq)` and
  return the error. After `RecordAgentPlacement` succeeds, call
  `slot.release()`.
- `holdTimedOutProvision` runs `defer release()` and then
  `l.hub.Provision(context.WithoutCancel(ctx), provID, agentID, preq)`. On
  success it calls `RecordAgentPlacement`. Every error is logged.

**Test cycle (red first).**

1. `TestCountAgentPlacementsForOwner` (store, pgtest):
   - a new owner has 0, and an unplaced agent adds nothing;
   - two placed agents give 2, and another owner's row is not counted;
   - `DeleteAgentPlacement` lowers the count;
   - an empty owner is `ErrInvalidArgument`.
2. `TestSpawnLimiter*`, with a fake counter:
   - it admits up to the limit, then `CodeResourceExhausted`, whose
     `Message()` equals GC-3's text;
   - rows count against the limit;
   - a double release frees one slot;
   - after `keep`, `release` is a no-op and the kept func frees one slot;
   - owners are independent;
   - a counter error is `CodeInternal` and holds nothing;
   - 32 goroutines at limit 3 admit exactly 3;
   - limit 0 admits 16.
3. `TestSpawnLimit*` (pgtest), on `newLifecycleFixture` with
   `f.lc.limits = newSpawnLimiter(2, f.store)`. Use distinct
   `setContainerNames` and `setStartIDs`. At the cap:
   - a new handle is refused, with `provisionCount` unchanged and no account
     created;
   - a placed handle returns its container;
   - an unplaced existing handle is refused;
   - a second owner still spawns;
   - stopped sessions still count, and `f.svc.RemoveAgentWorkspace` or an
     agent despawn frees a slot, so the next fresh spawn succeeds;
   - a refused Start (`setFailStart(true)`) leaves the slot free.
4. `TestSpawnLimitHoldsTimedOutProvision`. Use limit 1, a shortened
   `spawnChainTimeout`, and `setProvisionGate`.
   - The gated spawn fails, and a second fresh spawn is refused.
   - After the gate closes, poll until `PlacementForAgent` finds the first
     agent's container. A fresh spawn is then still refused.

**Spec and docs, same PR.** In `docs/specs/product/compass.md`, add
`### Requirement: Agent-initiated spawn is capped per owner` after "One
container per agent account", with four rules:

- a spawn that would add a placement past the per-owner maximum is refused
  with `RESOURCE_EXHAUSTED` before any account or container is created;
- a retry for a placed agent still succeeds;
- a Provision that outlives the spawn deadline holds its slot until the Runner
  answers;
- the operator door is not refused.

Add two `#### Scenario:` blocks, one for the refusal and one for the retry. In
`docs/concepts/tools.md`, the `agents_spawn_peer` bullet says the call fails
with `resource_exhausted` at the owner's maximum. A despawn, by an agent or by
`compass agent despawn`, frees a slot.

**Smoke and done.** From `go/`, with `COMPASS_TEST_DATABASE_DSN` set, run
`go test -race -tags pgtest -run 'CountAgentPlacementsForOwner|SpawnLimit' ./internal/store/ ./server/`.
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

// lifecycle.go: sets l.limits to newSpawnLimiter(n, l.store); mirrors service.withRev.
func (l *lifecycleService) withMaxAgentsPerOwner(n int) *lifecycleService

// sinks.go: installs newLifecycleService(st, hub, commsSvc).withMaxAgentsPerOwner(n)
// and logs "agent spawn limit set" with max_agents_per_owner = the effective limit.
func wireHubServiceCycles(hub *runnerhub.Hub, commsSvc *comms.Comms, st *store.Store,
	issueBrd *board.IssueProjection, maxAgentsPerOwner int)
```

`Serve` passes `cfg.MaxAgentsPerOwner`. The waker's `lifecycleService` never
spawns, so it keeps the default limiter.

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
- [ ] T2: store count, `spawnLimiter`, single gate, timed-out Provision hold,
      spec requirement, tools doc
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
    costs one more indexed count (`agent_accounts_parent_idx`), a second knob,
    and a limiter keyed on both.

  **Recommendation:** owner only for now. The refusal is visible, T1 gives a
  way out, and a per-parent limit can be added later without changing the
  owner cap.
- **OQ-2: No unlimited mode. Non-load-bearing.** A value below 1 fails boot.
  **Recommendation:** keep it. A large value is the escape hatch.
- **OQ-3: Operator door exempt but counted. Non-load-bearing.** An exempt
  spawn can push an owner past `N`. New agent spawns then stay refused until T1
  or an agent despawn brings the owner below `N`. **Recommendation:** keep it.
  A capped seed could keep the supervisor from booting.
- **OQ-4: Per-Runner host ceiling. Non-load-bearing while the timed-out
  Provision hold ships.** A ceiling enforced by the Runner is the only counter
  that sees orphan containers. It backstops the leaks the hold cannot see (a
  stream detach during the hold, a failed late `RecordAgentPlacement`). It also
  bounds several owners on one host. **Recommendation:** a follow-up in which
  the Runner refuses a Provision when the host is full.
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
