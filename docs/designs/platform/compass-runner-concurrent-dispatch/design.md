# Design: Runner concurrent per-command Sessions dispatch

Status: Draft

Linear: SEA-1575. Follow-up Matt ordered when resolving the spawn/despawn
record's OQ-6 ("accept for MVP, file follow up to design fully later",
2026-07-29).

## Problem / Intent

The Runner executes Sessions commands strictly serially: `RunSessions`
(`go/internal/runner/dispatch.go:189-206`) is one loop —

> ```go
> for {
>   cmd, err := stream.Receive()
>   ...
>   result := d.handle(ctx, cmd)
>   ...
>   if err := stream.Send(result); err != nil {
> ```

— one command executed inline before the next `Receive`. A Provision holds
that loop for a full container build/pull (minutes), so no Stop, Status,
despawn, or second spawn can execute on that Runner meanwhile. The
spawn/despawn record made Provision agent-triggerable in a loop and named the
exposure (`docs/designs/product/compass-agent-spawn-despawn/design.md:231-233`:
"A spawn's minutes-long container build/pull therefore head-of-line-blocks
every other command on that Runner — Stop, Status, despawn, a second spawn, an
operator reload"). Matt resolved its OQ-6 as accept-for-MVP (`:886-888`:
"**Resolved (Matt, 2026-07-29): (a) accept for MVP** — 'accept for MVP, file
follow up to design fully later.'"). This record is that follow-up: make
dispatch concurrent per command so a slow Provision never blocks the rest of
the Runner's command plane.

Shipped mitigation today: the **bounded relay deadline** — the spawn chain
runs under a deadline so a wedged provision fails the tool call in-band
(spawn/despawn record `:234-237`; the gateway forwards it —
`go/internal/runner/gateway/lifecycle.go:46-47`: "The inbound deadline rides
ctx into the forward"). It is the **sole** MVP guard: spawn caps were deferred
(spawn/despawn record `:852-855`: "Proper spawn limits are deferred to
**SEA-1574**. Consequence for OQ-6: with no cap shipping, the **bounded relay
deadline (Approach) is the sole MVP guard** on how long an agent can
monopolize the Runner's serial command plane."). The deadline bounds how long
one caller waits; it does nothing for the other commands queued behind the
provision. Removing the head-of-line blocking requires concurrency.

## Global Constraints

- **Single-Runner dogfood target.** The design serves the current one-Runner,
  one-stream deployment; multi-stream/high-volume hardening stays T9's
  (SEA-1328) scope per `dispatch.go:85-87` ("A future high-volume /
  multi-stream Runner needs bounded eviction + in-flight-sentinel dedup here;
  deferred to T9") — except the pieces this record deliberately pulls forward
  (in-flight sentinel, per-container transition lock; see Approach and OQ-1).
- **Go; `go test -race` is the merge gate** (CGO_ENABLED=1). Every new test in
  the Plan must be race-clean and is expected to redden under `-race` if a
  serialization piece is dropped — the posture
  `go/internal/runnerhub/concurrent_dispatch_test.go:11-14` already
  establishes server-side ("this test drives many concurrent dispatches over
  the real mounted handler under -race, so a regression that dropped sendMu
  reddens here").
- **Frozen dial-out inversion untouched.** The Runner still dials out; no new
  inbound RPC, no new stream. This change is entirely Runner-local goroutine
  structure.
- **No wire/proto change.** The Server router already correlates out-of-order
  completions by request id: `go/internal/runnerhub/router.go:34-38`
  ("inflight maps a request id to the pending call awaiting its result") and
  `complete()` delivers "a result the Runner returned on its request stream to
  the waiting call, keyed by request id" (`router.go:234-235`). Out-of-order
  result frames are already legal on the wire; `Sessions` /
  `PublishEvents` protos are untouched.
- **Leak-free goroutine lifecycle.** Every goroutine this record spawns is
  joined on ctx cancel / stream end on every return path, mirroring the
  existing config-worker discipline (`dispatch.go:174-177` — `defer func() {
  cancel(); <-d.configWorkerDone }()`). No leak under any Receive/Send error
  path.
- **Forward dependency on T9 (SEA-1328).** This record lands the scoped
  per-container transition lock T9's issue body lists as its own items (see
  OQ-1). Whoever picks up T9 MUST consume/extend that lock, not re-introduce
  it. T4 updates the code comments that currently defer to T9
  (`host.go:250-252`, `dispatch.go:85-87`) to point at the landed lock.
- Commit identity per repo convention (seal + Matt co-author trailer);
  squash-merge; AGENTS.md comment rules (no issue-id metadata in code — code
  comments cite this record by path).

## Approach

**Per-command goroutine dispatch with three explicit serialization
invariants**: an in-flight-sentinel idempotency map, a `sendMu` around
`stream.Send`, and a per-container transition lock in `agentHost`. Unbounded
goroutine-per-command (no worker pool) — justified in (e). Each in-flight
goroutine is joined on shutdown via a `sync.WaitGroup`, mirroring the config
worker join.

The existing precedent is `dispatch.go:90-148`: the ConfigVersion path already
runs off the receive loop — a coalescing `configSignal chan struct{}` (cap 1,
`dispatch.go:99`) drained by `runConfigWorker`, which "exits on ctx cancel,
closing configWorkerDone so the caller can join it leak-free"
(`dispatch.go:133-134`). This record generalizes that discipline: the receive
loop only receives and spawns; execution happens off-loop; every off-loop
goroutine is joined.

### (a) Receive loop spawns per-command goroutines

`RunSessions` keeps its Receive loop, but replaces the inline
`handle`+`Send` (`dispatch.go:197-205`) with:

```go
d.wg.Add(1)
go func() {
  defer d.wg.Done()
  result := d.handle(ctx, cmd)
  if result == nil {
    return // signal-only command, no result frame
  }
  if err := d.send(result); err != nil {
    d.log.ErrorContext(ctx, "sending session result failed", ...)
    cancelCause(fmt.Errorf("session result send failed: %w", err))
  }
}()
```

ctx is the already-derived cancelable ctx (`dispatch.go:168`), so every
command execution is cancelled when RunSessions unwinds — same propagation the
inline call has today.

**Send-failure classification (a defect the naïve `cancel()` would introduce).**
A Send failure no longer returns from RunSessions directly (the failing Send is
off-loop) — it must unwind the Receive loop, but it must NOT be misread as a
clean shutdown. RunSessions today classifies its Receive error as clean via
`errors.Is(ctx.Err(), context.Canceled)` (`dispatch.go:194-197`). A bare
`cancel()` after a failed Send would make that arm true → RunSessions returns
nil → Run returns nil (`run.go:118-121` wraps only a non-nil error), so a
**broken stream would read as a clean cancel** — a silent regression, since the
serial loop returns the Send error directly today (`dispatch.go:203-205`). The
cancel is therefore a `context.WithCancelCause`: RunSessions derives
`ctx, cancelCause := context.WithCancelCause(ctx)` (replacing the plain
`context.WithCancel` at `dispatch.go:167-168`), the send path cancels with a
sentinel-wrapped cause, and the Receive-error classification checks
`context.Cause(ctx)` — a `context.Canceled` cause is the clean external
shutdown (returns nil); a send-failure cause is returned as the loop error.
Cancelling ctx still closes the response side (`dispatch.go:185-188` — the
watcher goroutine calls `stream.CloseResponse()` on ctx.Done) and pops the
blocked Receive, so the loop unwinds on a broken stream — now with the error
preserved, not swallowed. (T3 pins this: a forced Send failure must surface as a
non-nil RunSessions error, not nil.)

Shutdown join: extend the existing deferred join to
`defer func() { cancelCause(nil); d.wg.Wait(); <-d.configWorkerDone }()` —
cancel first (with a nil cause, the clean-shutdown disposition) so in-flight
host calls see ctx.Done, then join every command goroutine, then the config
worker. This preserves the current guarantee ("cancel precedes the join
regardless of return path", `dispatch.go:171-172`) for the new goroutine class.
The join TERMINATES but is not instantaneous: an in-flight Provision mid-`podman
pull` dies on ctx-cancel kill promptly, but a goroutine inside a non-ctx-bounded
section (`AgentStream.Stop`'s `drainGrace`, a teardown grace) can hold
`wg.Wait()` for up to that section's bound, so worst-case join latency
approaches the per-command kill/drain bounds — not zero. Because concurrency
lets more ops be mid-flight at cancel time than the serial loop ever did,
cancellation now leaves more partial container state (a killed teardown, a
launched-but-unrecorded container), leaning harder on the existing crash-reclaim
paths (`gateway.reclaimStaleSocket`, idempotent re-Provision) that already cover
a serial loop cancelled mid-command. Signal-only arms (SecretsVersion,
ConfigVersion) are cheap (`signalConfig` is non-blocking, `dispatch.go:122-128`)
and MAY stay inline on the receive loop; routing them through the same goroutine
path is also harmless — T2 keeps them inline to minimize diff.

### (b) In-flight-sentinel idempotency (the `handled` map)

Today `handle` (`dispatch.go:225-236`) is check → unlock → execute → record:

> ```go
> d.mu.Lock()
> if prev, ok := d.handled[id]; ok {
>   d.mu.Unlock()
>   return prev
> }
> d.mu.Unlock()
> result := d.execute(ctx, id, cmd)
> d.mu.Lock()
> d.handled[id] = result
> d.mu.Unlock()
> ```

The map records only COMPLETED results, and the comment is explicit that
correctness currently rides on serial delivery (`dispatch.go:83-87`:
"single-delivery-per-id (so this dedup is not itself raced by two concurrent
pushes of one id) relies on the upstream Server router joining retries on the
one Sessions stream. A future high-volume / multi-stream Runner needs bounded
eviction + in-flight-sentinel dedup here; deferred to T9"). Under concurrent
dispatch the check-then-record window is open concurrently: a retry of an
IN-FLIGHT id would miss the map and double-execute — defeating the OQ6
idempotency contract the map exists for.

Fix: the map value becomes a two-state entry — the exact shape the Server
router already uses for the same problem (`router.go:71-75`, `pendingCall`
with a `done` channel; "Multiple retriers of the same request id all wait on
the same pendingCall, so they observe one identical outcome"):

```go
// dispatcher
handled map[string]*inflightResult

type inflightResult struct {
  done   chan struct{}                        // closed when result is set
  result *compassv1internal.SessionsRequest   // valid after done closes
}
```

`handle` becomes: lock `mu`; if entry exists, unlock and wait
(`select { case <-entry.done: return entry.result; case <-ctx.Done(): return nil }`)
— a concurrent same-id push JOINS the in-flight execution and re-Sends the
identical recorded result when it lands (the router ignores a result frame for
an id it already completed — the "truly-unknown id... ignored" arm,
`router.go:241-242` — so a duplicate Send is harmless). If no entry: create
`{done: make(chan struct{})}`, record it, unlock, execute, then set `result`
and `close(done)`. Execute-once holds even under concurrent same-id pushes.
The `ctx.Done` join arm means a retry that lands while the Runner is shutting
down returns nil and sends nothing — not a dangling contract: the Server's
retry then observes the Runner detach (which fails its in-flight calls
router-side), so the unsent frame is covered by the server-side contract, not
lost.
No eviction (unchanged from today: "the set is small and lives for the
stream's life — it is not evicted", `dispatch.go:81-83`); bounded eviction
stays T9's.

This is defense-in-depth today — the Server router still never re-pushes a
live id (`router.go:128-133` joins retries server-side) — but concurrency
removes the serial-delivery assumption the current code names as
load-bearing, so the sentinel lands with it, not after it.

### (c) `sendMu` — serialize the local Send call

`stream.Send(result)` is called only from the one receive-loop goroutine today
(`dispatch.go:203`). connect-go's bidi Send is not safe for concurrent
callers — the Server side already documents and guards the identical hazard
(`router.go:61-65`: "sendMu serializes concurrent calls into the live stream's
Send. connect's server-side BidiStream.Send is not safe for concurrent use...
a dedicated lock (never held with mu) keeps map bookkeeping off the Send
critical section"). Mirror it exactly on the Runner side: a
`sendMu sync.Mutex` on the dispatcher, taken only around `stream.Send`, never
while holding `mu`. RunSessions hands the dispatcher a `send` closure over the
stream so per-command goroutines never touch the stream directly. Ordering on
the wire is NOT preserved across commands — deliberately: the router
correlates by request id (Global Constraints), so out-of-order completions are
already legal.

### (d) Per-container transition lock — the T9 fork, pulled forward (crux)

Concurrent dispatch makes concurrent `SessionHost` callers reachable.
`agentHost` guards its maps with `h.mu` (`host.go:64-65`) but the LIFECYCLE
ops are not mutually serialized per container — the code says so verbatim at
the Start TOCTOU (`host.go:244-252`):

> "Two concurrent Starts for one container could both pass the check in that
> window (TOCTOU). Unreachable in the single-Runner MVP: the Sessions dispatch
> loop is strictly sequential (dispatch.go — Receive→execute→Send, no
> per-command goroutine) and Run is single-shot... so only one lifecycle op is
> ever in flight against this host. A per-session transition lock is deferred
> to T9, where in-process reattach against a persistent host first makes
> concurrent callers reachable"

This record makes those callers reachable FIRST, so the lock cannot stay
deferred. Critically, this is not a new one-off lock: SEA-1575 and T9 share
the same locking machinery. T9's issue body (SEA-1328) documents the
per-session transition lock and in-flight sentinel as its own items and notes
the concurrent-host-caller races are "verified unreachable in the single-Runner
MVP ... reachable only when T9 builds in-process reattach against a persistent
host". The recommendation (OQ-1, option A) is therefore: **build T9's
load-bearing per-container transition lock EARLY, scoped to exactly the
lifecycle paths concurrent dispatch reaches** — and leave the forward
dependency note so T9 consumes it rather than re-introducing it.

Design of the lock:

- **Granularity: per container name**, in `agentHost`. Container name is the
  stable lifecycle key: Provision/Remove take it directly; Start resolves it
  from the request (`host.go:228`); Stop/Reload take a session id that
  resolves to a container under `h.mu`. Sessions are 1:1 with containers
  (`host.go:235-239` — Start refuses a second session per container), so a
  per-container lock IS the per-session transition lock for every reachable
  op.
- **Shape**: `containerLocks map[string]*sync.Mutex` on `agentHost`, guarded
  by `h.mu` for get-or-create only; a helper
  `lockContainer(name string) (unlock func())` acquires. An entry is created
  on first use and NEVER deleted — not even by Remove. The non-deletion is
  load-bearing, not laziness: a Remove that deleted the lock entry could race a
  concurrent op that already resolved the same `*sync.Mutex`, so keeping the
  entry is what makes the resolve-then-lock protocol safe. Growth is therefore
  bounded by DISTINCT container names ever provisioned (not live count) — the
  same unbounded-over-time retention class as the `handled` map and
  `configVersions` (`host.go:69-79`); T9's bounded-eviction work covers all
  three together.
- **Lock ordering**: acquire the container lock FIRST, then `h.mu` briefly
  inside the op (h.mu is a short-lived map guard, never held across a slow
  call today — e.g. Start releases it before StartAgent, `host.go:242-244`).
  `h.mu` is never held while acquiring a container lock except the map
  get-or-create in `lockContainer`, which touches no container lock while
  holding `h.mu`. No lock is ever held across `stream.Send`.
- **Which ops lock**: Provision, Start, Stop, Remove, Reload — every
  state-transitioning op. For Stop/Reload the session→container resolution
  happens under `h.mu` first, then the container lock is taken and the session
  re-checked (both ops are idempotent on a vanished session — Stop:
  "unknown/already-stopped session succeeds", `host.go:361-362`). **Status
  does NOT take container locks** — it answers from the session set under
  `h.mu` only, so a Status never queues behind a slow Provision (that
  responsiveness is the point of this record).
- **Reload must split to avoid a self-deadlock via `RefreshConfig`.** The
  config worker's per-container fan-out calls `h.Reload` directly inside its
  loop (`host.go:629` — `if err := h.Reload(ctx, t.sessionID); err != nil`).
  If BOTH the `RefreshConfig` leg AND `Reload` acquire the same non-reentrant
  `sync.Mutex` for that container, the worker holding container C's lock then
  entering `Reload(C)` blocks on its own lock forever — and since the lock is
  never released, every later dispatch-driven Stop/Reload/Remove of C wedges
  behind it too (nothing times this out — the worker's ctx select is outside
  `RefreshConfig`, `dispatch.go:139-148`). So `Reload` splits into a public
  locking wrapper `Reload` (takes the container lock, calls `reloadLocked`) and
  an unexported `reloadLocked` that assumes the lock is already held. The
  dispatch path calls `Reload` (wrapper locks); the `RefreshConfig` leg takes
  the container lock ITSELF and calls `reloadLocked` — so its `MountLabel` +
  `Materialize` + reload run as ONE critical section under the container lock,
  which is exactly what the config-Reload-cannot-race-a-concurrent-Stop/Remove
  guarantee requires (the accepted-for-MVP unserialized window at
  `host.go:578-588` is what this closes). The rejected alternative — the leg
  takes no lock and leans on `Reload`'s acquisition — leaves `MountLabel` +
  `Materialize` unserialized against a concurrent Remove of the same container,
  reopening the very race (d) exists to close.
- **What it buys, concretely**: two concurrent Starts for one container
  serialize through the lock, closing the documented TOCTOU; a
  Provision-in-progress blocks a concurrent Remove of the SAME container (and
  nothing else); Stop/Reload/Remove interleavings on one container become
  linearizable. Ops on DIFFERENT containers proceed fully in parallel.

The lock lives in `agentHost`, not the dispatcher, because the invariant is a
host invariant: T9's in-process reattach reaches the host through paths that
never cross the dispatcher, and the fake-host tests exercise the dispatcher
without it.

### (e) Unbounded goroutine-per-command; no worker pool

Recommended default: spawn one goroutine per command, no pool (OQ-2). Grounds:

- Goroutine COUNT is a non-issue: the goroutine itself is cheap, and the heavy
  ops self-serialize per container via (d). The real question is concurrent
  podman WORK, addressed as a load-bearing fork in OQ-4 below — not dismissed
  here. The earlier framing that in-flight commands are "bounded upstream by
  the Server's deadline-bounded `pendingCall`s" is only half true and must not
  be leaned on: the Server-side per-call deadline bounds WAITERS, not
  Runner-side concurrent work — `waitCall` deliberately leaves a timed-out call
  in flight on the Runner (`router.go:226-231`: "A ctx timeout leaves the call
  in-flight on purpose"), and nothing caps the number of DISTINCT request ids
  the (agent-triggerable, spawn-cap-deferred) command stream can carry. So the
  goroutine structure is unbounded-per-command by design; the guard that
  matters is the Provision-concurrency decision in OQ-4, not an upstream count
  bound that does not exist.
- A worker pool of size k reintroduces exactly the failure being removed: k
  slow Provisions head-of-line-block the (k+1)th command — a Stop — behind
  the queue. A pool is the wrong shape for a latency-isolation problem.

If a bound is ever wanted, it composes trivially later (a semaphore acquired
in the spawned goroutine before `handle`), with zero interface change.

### Alternatives considered

1. **Partial concurrency: only Provision runs async, everything else stays
   inline.** Smallest diff, but it still needs ALL THREE invariants ((b) the
   sentinel — an async Provision's id can now race its own retry; (c) sendMu —
   the async Provision's Send races the loop's inline Sends; (d) the container
   lock — an async Provision races an inline Remove of the same container),
   while adding a second dispatch convention (two code paths, two test
   matrices) and leaving the next slow command (image-pulling Reload, a
   wedged Stop on a stuck container) to re-litigate the same design. All of
   the cost, a fraction of the benefit. Rejected.
2. **Full per-command concurrency with per-container lock (RECOMMENDED).**
   One uniform dispatch path; every invariant handled once; Status latency
   decoupled from Provision by construction; mirrors the concurrency shape the
   Server side already ships (router sendMu + pendingCall join).
3. **Do nothing now; gate entirely on T9 (SEA-1328).** T9 is Backlog and
   genuinely not started (no owner, no assignee, no branch). Gating leaves the
   agent-facing HOL blocking — which the spawn/despawn record ships
   agent-triggerable in a loop — in place indefinitely, guarded only by the
   relay deadline (which bounds one caller's wait, not the queue). Rejected;
   kept as OQ-1 option B for Matt.
4. **One coarse host-wide lifecycle mutex** (a single mutex serializing all
   lifecycle ops across every container, deferring per-container granularity to
   T9). Simpler than the per-container map, and Status stays responsive (still
   lock-free). Rejected because it reproduces the exact failure this record
   exists to remove: a minutes-long Provision holding the one mutex
   head-of-line-blocks a concurrent Stop/despawn of a DIFFERENT container — the
   "Stop, Status, despawn, a second spawn" exposure from the Problem section.
   Per-container granularity is the point, not an over-engineering.

## Plan

Tasks are ordered red-first; each is independently reviewable. All in package
`go/internal/runner` unless noted. Gate: `go test -race ./go/internal/runner/...`
(CGO_ENABLED=1).

### T1 — Red-first concurrency tests (dispatch + host fake)

Write the failing tests before any production change, over the existing
fake-SessionHost harness (`dispatch.go:27` — "a test drives a fake"). All
event-gated on channels, no sleeps (repo test convention, e.g.
`gateway/socket_test.go:15-17`). Red today because dispatch is serial and the
host has no container lock:

1. **Slow Provision does not block a concurrent Stop**: fake host whose
   Provision parks on a channel; push Provision then Stop; assert the Stop
   result frame arrives while Provision is still parked. (Red: serial loop
   never reaches Stop.)
2. **Same-id concurrent retry executes the host exactly once**: fake host
   counts Provision invocations and parks; push the same request id twice;
   release; assert one invocation, two identical result frames (or one — the
   duplicate Send is allowed, the invariant is invocation count == 1).
3. **Concurrent ops on ONE container serialize**: fake host records
   entry/exit interleaving for two ops against one container name; assert no
   overlap; two ops on DIFFERENT containers DO overlap (proves the lock is
   per-container, not global).
4. **Leak-free shutdown joins all in-flight goroutines**: park a command in
   the fake host, cancel ctx, assert RunSessions returns only after the
   command goroutine observed ctx.Done and exited (goroutine-count or
   done-channel assertion), under `-race`.
5. **A ConfigVersion Reload does not self-deadlock**: drive a ConfigVersion
   signal that moves a container's version so the config worker's fan-out calls
   `Reload` on a container whose lock it took; assert the Reload completes and
   a subsequent dispatch-driven Stop/Remove of that container also completes
   (Red: with a single non-reentrant lock taken by both the fan-out leg and
   `Reload`, the worker wedges and the later Stop wedges behind it — the test
   times out). This pins the `reloadLocked` split (T4).
6. **A broken Send surfaces as a non-nil loop error**: force `send` to fail
   for one command; assert `RunSessions` returns a non-nil error (not nil via
   a mis-classified clean-cancel), while a normal external ctx-cancel still
   returns nil. This pins the `WithCancelCause` classification (Approach (a),
   T3).

Interfaces: test-only; drives `(*ServerLink).RunSessions(ctx context.Context,
host SessionHost, log *slog.Logger) error` (`dispatch.go:164`) over the
in-memory wire the existing dispatch tests use, plus a fake `SessionHost`
(`dispatch.go:28-66`). Test 3 additionally drives `agentHost` via
`NewSessionHost` (`host.go:108`) or asserts through the dispatcher against a
lock-aware fake — implementer's choice, noted in the test file header.

### T2 — In-flight-sentinel idempotency map

Convert `dispatcher.handled` to the two-state entry; rewrite `handle`'s
check/record around it (greens test 2).

Interfaces:

- `dispatcher.handled` field: `map[string]*compassv1internal.SessionsRequest`
  → `map[string]*inflightResult` (`dispatch.go:88`).
- New type: `type inflightResult struct { done chan struct{}; result *compassv1internal.SessionsRequest }`.
- `func (d *dispatcher) handle(ctx context.Context, cmd *compassv1internal.SessionsResponse) *compassv1internal.SessionsRequest`
  (`dispatch.go:211`) — signature unchanged; a joiner returns nil on
  ctx.Done (no frame sent, matching the signal-only nil contract at
  `dispatch.go:198-201`).
- `newDispatcher` map literal updated (`dispatch.go:113`).
- Update the `handled` doc comment (`dispatch.go:80-87`): the
  single-delivery-per-id reliance is gone; bounded eviction remains deferred
  to T9.

### T3 — Per-command goroutine dispatch + sendMu + shutdown join

Spawn a goroutine per command from the Receive loop; serialize Send; join on
every return path (greens tests 1 and 4; test 2's two-frame variant needs this
too).

Interfaces:

- `dispatcher` gains: `wg sync.WaitGroup`, `sendMu sync.Mutex`, and
  `send func(*compassv1internal.SessionsRequest) error` (set by RunSessions;
  the closure takes `sendMu` around `stream.Send`, mirroring
  `router.go:143-145`).
- `RunSessions` (`dispatch.go:164-207`): loop body replaced per Approach (a);
  the local ctx becomes `ctx, cancelCause := context.WithCancelCause(ctx)`
  (replacing the plain `context.WithCancel` at `dispatch.go:167-168`); a failed
  off-loop `send` calls `cancelCause(<send-failure error>)`; the Receive-error
  classification (`dispatch.go:194-197`) checks `context.Cause(ctx)` — a
  `context.Canceled` cause returns nil (clean shutdown), a send-failure cause
  is returned as the loop error; the deferred join becomes
  `cancelCause(nil); d.wg.Wait(); <-d.configWorkerDone`. Greens test 6.
- No `SessionHost` change; no proto change.
- Signal-only arms stay inline on the receive loop (Approach (a)).

### T4 — Per-container transition lock in agentHost

The scoped T9 lock, pulled forward (greens tests 3 and 5; closes the Start
TOCTOU).

Interfaces:

- `agentHost` gains: `containerLocks map[string]*sync.Mutex` (guarded by
  `h.mu` for get-or-create, never deleted — see Approach (d) shape),
  initialized in `NewSessionHost` (`host.go:115-129`).
- New helper: `func (h *agentHost) lockContainer(name string) (unlock func())`.
- `Reload` splits: public `Reload(ctx, sessionID)` (resolves session→container
  under `h.mu`, takes the container lock, re-checks, calls `reloadLocked`) +
  unexported `func (h *agentHost) reloadLocked(ctx context.Context, ...)`
  assuming the container lock is held. This is what lets `RefreshConfig`'s leg
  hold the lock across `MountLabel`+`Materialize`+`reloadLocked` without
  re-entering the non-reentrant mutex (Approach (d) self-deadlock fix).
- Acquisition added to: `Provision(ctx, req)` (`host.go:143`),
  `Start(ctx, req, resumeBody)` (`host.go:227`) — lock taken after the
  container name is read from the request, before the registry resolve;
  `Stop(ctx, sessionID)` (`host.go:363`) and `Reload` (the public wrapper) —
  resolve session→container under `h.mu`, release, take container lock,
  re-check session; `Remove(ctx, containerName)` (`host.go:394`); and the
  per-container leg of `RefreshConfig` (`host.go:629`), which takes the lock
  itself and calls `reloadLocked` (NOT `Reload`, to avoid the self-deadlock).
- `Status` deliberately does NOT acquire container locks.
- Rewrite the TOCTOU comment (`host.go:244-252`) and the dispatch idempotency
  comment (`dispatch.go:83-87`): the serialization now exists; what remains
  for T9 is in-process reattach + bounded eviction, consuming this lock
  (Global Constraints, forward dependency).

### T5 — Changelog + record cross-references

Changelog entry; add the forward-dependency note where T9's future implementer
will find it (this record's Global Constraints already carries it; T4's
comment rewrite points here). No other docs change.

Interfaces: none (docs only).

## Tasks

- [ ] T1: red-first concurrency tests — slow-Provision/Stop isolation,
      same-id single execution, per-container serialization, leak-free
      shutdown join, ConfigVersion-Reload no-self-deadlock (test 5),
      broken-Send-surfaces-non-nil (test 6); all red, all under `-race`
- [ ] T2: in-flight-sentinel `handled` map (`inflightResult` with done
      channel); doc comment updated
- [ ] T3: per-command goroutine spawn + `sendMu` Send closure +
      `WithCancelCause` send-failure classification + `wg.Wait()` shutdown
      join in RunSessions
- [ ] T4: per-container transition lock in `agentHost`
      (`containerLocks` + `lockContainer`); `Reload`→`reloadLocked` split;
      acquisition in Provision/Start/Stop/Remove/Reload + RefreshConfig leg
      (leg calls `reloadLocked`); Status lock-free; TOCTOU + T9-deferral
      comments rewritten
- [ ] T-cap: Provision-arm concurrency semaphore (small tunable cap, e.g.
      8–16) acquired only on the Provision arm before the heavy podman work +
      its test (fan-out beyond the cap queues the overflow; a concurrent Stop
      still returns immediately) — lands between T3 and T4 (Matt ruled OQ-4 =
      (i))
- [ ] T5: changelog + forward-dependency cross-references
- [ ] Gate: `CGO_ENABLED=1 go test -race ./go/internal/runner/...` green

## Open Questions

### OQ-1 (LOAD-BEARING — blocks merge until Matt rules): pull T9's transition lock forward, or gate on T9?

Concurrent dispatch makes concurrent `SessionHost` callers reachable before
the per-session transition lock exists — `host.go:250-252` defers that lock to
T9 ("A per-session transition lock is deferred to T9, where in-process
reattach against a persistent host first makes concurrent callers reachable").
SEA-1575 and T9 (SEA-1328) share the same locking machinery: T9's issue body
documents the per-session transition lock + in-flight sentinel as its own
items and marks the concurrent-host-caller races "verified unreachable in the
single-Runner MVP ... reachable only when T9 builds in-process reattach
against a persistent host". This record makes them reachable first. Options:

- **(A) Start T9's transition lock now, scoped to per-container lifecycle
  serialization (RECOMMENDED — the record as written, T4).** SEA-1575 is
  self-contained; the lock is small, local to `agentHost`, and is T9's OWN
  design built early — not a throwaway. Cost: if T9's eventual persistent-host
  redesign reshapes the host, the lock may need rework; mitigated by the
  forward-dependency note (T9 consumes/extends this lock, never re-introduces
  it) carried in Global Constraints and the T4 comment rewrite.
- **(B) Gate SEA-1575 entirely on full T9.** No duplicated machinery ever —
  but T9 is genuinely NOT started (Backlog, no owner, no assignee, no branch),
  so this gates the fix for an already-shipped, agent-triggerable HOL exposure
  on an unscheduled item of much larger scope (persistent host, in-process
  reattach, bounded eviction). The exposure's sole current guard is the relay
  deadline, which bounds one caller's wait, not the queue behind it.

**Resolved (Matt, 2026-08-09): (A)** — build T9's per-container transition
lock now, scoped to the lifecycle paths concurrent dispatch reaches (T4), with
the forward-dependency note so T9 (SEA-1328) consumes/extends it. The Approach,
Global Constraints, and T4 are written to this ruling.

### OQ-2 (not load-bearing — default chosen; flag only if Matt objects): worker pool vs goroutine-per-command

Default: goroutine-per-command, no fixed-size pool (Approach (e)). A size-k
pool reintroduces the exact HOL blocking this record removes — k slow
Provisions block the (k+1)th command (a Stop) behind the pool queue — so a
pool is the wrong shape for a latency-isolation change. The goroutine count
itself is cheap and not the concern; the concern is concurrent podman WORK,
which is a distinct and load-bearing question raised as OQ-4 (a Provision-arm
cap is NOT a worker pool — it gates only the heavy arm, leaving Stop/Status
un-queued). This OQ is only "pool vs no pool"; OQ-4 owns the Provision-cap
decision.

### OQ-3 (not load-bearing — flagging an interaction, not a fork): duplicate result frames from idempotent joiners

A same-id joiner re-Sends the recorded result (Approach (b)); the router
treats a completed/unknown id as "ignored, the original contract"
(`router.go:241-242`), so this is benign today. If the router ever grows
strict duplicate detection, the joiner should skip the Send instead (a
one-line change in `handle`'s join arm). Recorded so the interaction is
visible, not decided by accident.

### OQ-4 (LOAD-BEARING — blocks merge until Matt rules): Provision concurrency before SEA-1574 caps exist

Today's serial dispatch loop is an accidental **concurrency-1 throttle** on
agent-triggered Provisions: one agent looping spawns gets at most one podman
build/pull at a time, because the loop can't start the next command until the
current one returns. This record removes that throttle before any explicit cap
exists — spawn caps were explicitly deferred to **SEA-1574** (Problem section;
the relay deadline is named there as the *sole* MVP guard). So N distinct-id
Provisions become N concurrent podman launches, each up to
`defaultCommandTimeout` (120s, `podman.go:317-320`) of CPU/disk/network — a
Runner-host resource-exhaustion surface that did not exist under the serial
loop. The exposure is real because Provision is agent-triggerable in a loop
(spawn/despawn record) and the frozen trust model has the Runner trust the
Server, so a misbehaving Server or a reconnect storm widens the same surface;
`waitCall` even leaves a timed-out call in flight Runner-side
(`router.go:226-231`), so a caller giving up does not stop the work. The
"bounded upstream" intuition is false here: the Server-side per-call deadline
bounds WAITERS, not Runner-side concurrent WORK. Options:

- **(i) Land a cheap Provision-arm concurrency cap IN this change (RECOMMENDED).**
  A small counting semaphore (e.g. cap 8–16, a single tunable const) acquired
  in the spawned goroutine ONLY on the Provision arm, before the heavy podman
  work — Stop/Status/Reload/Remove never touch it, so every latency-isolation
  property this record buys is preserved. ~One field + a few lines; adds a T
  (T-cap) and a test (a slow-Provision fan-out beyond the cap queues the
  overflow but a concurrent Stop still returns immediately). Restores a real,
  intentional throttle instead of silently deleting the accidental one.
- **(ii) Order SEA-1574 before SEA-1575.** No cap logic in this change, but it
  blocks a shipped-exposure fix behind an unscheduled cap design, and leaves
  the window fully open until SEA-1574 lands.
- **(iii) Accept the widened window explicitly for the single-Runner dogfood.**
  Cheapest now; correct only if the dogfood Runner is never driven by an
  adversarial/looping agent before SEA-1574. Makes the accidental-throttle
  removal a conscious, recorded decision rather than an inherited one.

**Resolved (Matt, 2026-08-09): (i)** — land the Provision-arm concurrency cap
(small tunable semaphore, cap 8–16) in this change, on the Provision arm only.
The Plan carries it as **T-cap** between T3 and T4.
