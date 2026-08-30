# Design: Server runnerhub — per-router bounded send queue (non-blocking dispatch)

Status: Draft

Tracker: RIG-1611 — PR #75 review LOW #3, parked at review time ("no change for
MVP, multi-Runner only"). Provenance: the parent product record
`docs/designs/product/compass-notification-delivery/design.md` (RIG-1569 T3)
ships the fan-out consumer whose head-of-line blocking motivates this record;
that record is frozen (`Status: Active`) and is cited, never edited. This is
the **SERVER-side counterpart** to the already-**MERGED** RUNNER-side work
`compass-runner-concurrent-dispatch` (RIG-1575, commit 2ebdf09d on main —
"feat(runner): concurrent per-command Sessions dispatch"; its record's
`Status: Draft` header is stale, out of scope here): that change decoupled the
Runner's serial Sessions *command-execution* loop
(`go/internal/runner/dispatch.go`); this one decouples the Server's per-Runner
*stream-send* path (`go/internal/runnerhub/router.go`). Different subject, same
discipline (leak-free join, red-first `-race` tests, no wire change).

An earlier draft of this record designed the fix as delivery-side per-session
dispatch workers. Matt ruled the placement fork (formerly OQ-0) to **(B): a
runnerhub-internal per-router send queue** — see Resolved decisions. The
delivery-side worker design is recorded under Alternatives considered as the
weighed-and-rejected (A).

## Problem / Intent

The delivery consumer's `Run` is one goroutine doing everything. Its loop
(`go/internal/delivery/consumer.go:290-296`) reads the bus tail, drains the
settle/start queues, and handles each event inline:

> ```go
> for {
>   select {
>   case <-ctx.Done():
>     return nil
>   case <-c.notify:
>     c.drainSettles(ctx)
>     c.drainStarts(ctx)
>   case event, ok := <-sub.Live:
> ```

with `c.handleEvent(ctx, event.Payload)` called inline at `consumer.go:351`.
Every dispatch that loop performs funnels through the per-session gate into a
**synchronous, flow-control-blocking stream Send**:

1. `gatedDispatch` (`go/internal/delivery/dispatch.go:268-279`) takes the
   recipient session's gate and calls the hub synchronously:
   `gate.Lock(); defer gate.Unlock(); if err := c.dispatch.DispatchControl(ctx, sessionID, op); err != nil {`.
2. `Hub.DispatchControl` (`go/internal/runnerhub/dispatch_control.go:38-53`)
   resolves the session's router and ends in `return router.send1(cmd)` —
   send-only, no result wait.
3. `send1` (`go/internal/runnerhub/router.go:231-233`) serializes into the
   stream: `r.sendMu.Lock(); err := send(cmd); r.sendMu.Unlock()`. `send` is
   the connect-go server-side `BidiStream.Send`
   (`router.go:71-75`: "sendMu serializes concurrent calls into the live
   stream's Send. connect's server-side BidiStream.Send is not safe for
   concurrent use"), which **blocks on HTTP/2 flow control** when the Runner
   stops reading.

The block domain is the **Runner stream, not the session**: `commandRouter` is
"One router per attached Runner" (`router.go:28-29`), so `sendMu` — and the
flow-control-blocked `Send` under it — is shared by every session on that
Runner. One Runner's wedged Sessions-stream `Send` therefore parks the single
consumer-loop goroutine inside `gatedDispatch`, and **head-of-line-blocks**:

- delivery to every other session — including sessions on *other, healthy
  Runners*, which share nothing with the wedged stream but the loop goroutine;
- settle/start draining (`consumer.go:294-296` — `drainSettles`/`drainStarts`
  only run when the loop is free), so held delivers don't fire and reconnect
  sweeps don't run;
- bus-lag observation (the `sub.Live` arm, `consumer.go:297-349`) — the loop
  can't even notice an overrun while parked in a Send.

**The block is a RUNNERHUB property, not a delivery one.** The wedge lives on
`commandRouter`: `sendMu` plus the flow-control-blocking `BidiStream.Send`
under it, one router per attached Runner (`router.go:28-29`). It is shared by
ALL THREE send paths, not just delivery's:

- `send1` — send-only DELIVERs from the delivery consumer
  (`dispatch_control.go:53`: `return router.send1(cmd)`);
- `dispatch` — blocking client-facing session COMMANDS (Start/Stop/
  Reload/Status; `go/internal/runnerhub/commands.go:195`:
  `result, err := router.dispatch(ctx, cmd)`, surfaced as `CodeUnavailable`
  on error);
- `push` — fire-and-forget SIGNALs
  (`go/internal/runnerhub/secrets_signal.go:79` and
  `config_signal.go:63`: `router.router.push(cmd)`).

All three take the same `sendMu` inline (`router.go:166-168`, `:194-196`,
`:231-233`), so a wedged Send HOL-blocks a client's Start RPC and a secrets
signal exactly as it blocks a deliver. The fix therefore belongs at the
router: put a bounded outbound queue in front of the stream, drained by one
sender goroutine per router, and make all three paths non-blocking enqueues.

Intent: a **semantics-preserving latency-isolation refactor inside
`go/internal/runnerhub/`**. The delivery consumer
(`go/internal/delivery/`) is **UNTOUCHED**: its loop stays single-threaded,
its gates, sweeps, and refusal handling are unchanged — `send1` simply returns
the instant the frame is enqueued (or refuses synchronously, an error class
the consumer already handles), so the loop is freed without any delivery-side
restructure. Simultaneously the client-facing command relay (`dispatch`) and
the signal relay (`push`) stop HOL-blocking on a wedged stream. No wire
change; per-session ordering is preserved trivially (the consumer loop is the
single enqueuer, in decision order, and the per-router FIFO preserves that
order to the wire).

## Global Constraints

- **Go; the merge gate is `CGO_ENABLED=1 go test -race ./go/internal/runnerhub/...`.**
  Every isolation/ordering invariant in the Plan lands as a test that reddens
  under `-race` if its serialization piece is dropped — the posture the merged
  runner-side sibling establishes
  (`go/internal/runnerhub/concurrent_dispatch_test.go` already exercises
  concurrent `router.dispatch` calls, `:80-84`).
- **No wire/proto change.** The fix is Server-internal goroutine structure in
  `go/internal/runnerhub/` only. `go/internal/delivery/` is untouched — no
  consumer, gate, sweep, or test change outside runnerhub.
- **Runnerhub contract preservation.** Preserved verbatim:
  - **Per-router FIFO send order.** The queue preserves the order callers
    enqueued; one sender goroutine drains it to the wire. Per-session
    ascending-seq delivery order (parent
    compass-notification-delivery design.md:212-218) is therefore preserved:
    the delivery consumer still decides and enqueues in bus order on its one
    goroutine, and the FIFO does not reorder.
  - **OQ6 idempotency.** `dispatch`'s pendingCall join by request id
    (`router.go:150-156`: "a retry with an id already in-flight ... joins the
    existing call and returns the same result") is unchanged; a full-queue
    failure deletes the registration exactly as today's push failure does
    (`router.go:169-175`: "The push failed; drop the registration so a later
    retry can re-issue").
  - **Async-refusal machinery.** `deliverRefusals` (the bounded LRU that makes
    a deliver refusal "OBSERVABLE (logged + counted) instead of silently
    dropped", `router.go:41-48`) and `complete` (`router.go:270-305`) are
    preserved; `complete` still delivers results to waiting calls and counts
    refusals, unchanged.
  - **Cursor-unadvanced-until-ack + D2 sweep backstop** for send1 delivers:
    "The cursor is never advanced on send — it advances only later on the
    recipient's delivery_ack" (`dispatch_control.go:30-32`), and the
    consumer's existing refusal handling — "A synchronous refusal (no live
    stream) is not fatal: the cursor was never advanced on send, so the D2
    sweep redelivers" (`go/internal/delivery/dispatch.go:239-240`) — is the
    backstop this design's overflow policy reuses.
- **Goroutine-safe seams; single-sender invariant.** The sender goroutine is
  the ONLY caller of the stream's `send` for a given attachment — this
  replaces `sendMu` as the serialization satisfying connect's
  "BidiStream.Send is not safe for concurrent use" (`router.go:71-75`). All
  queue/registration bookkeeping stays under the existing `mu`.
- **Leak-free goroutine lifecycle.** The sender goroutine is started on
  `attach` and joined on `detach` (and hence on stream teardown / shutdown,
  since the handler runs `router.attach(stream.Send)` /
  `defer router.detach(errStreamClosed)`,
  `go/internal/runnerhub/handler.go:107-108`) — mirroring the merged
  sibling's cancel-then-join discipline
  (`go/internal/runner/dispatch.go:243`: `ctx, cancelCause :=
  context.WithCancelCause(ctx)`; `:251-253`: `cancelCause(nil); d.wg.Wait()`).
- **AGENTS.md comment rules.** Code comments cite this record by path, never
  issue IDs; this record cites tracker IDs as bare plain text (RIG-1611,
  RIG-1569, RIG-1575, RIG-1610) per `docs/designs/CONTRIBUTING.md` rule 1.

## Approach

**A bounded per-router outbound queue drained by one sender goroutine.** The
three send paths (`send1`, `push`, `dispatch`) stop taking `sendMu` inline and
become non-blocking enqueues of a classed frame; one goroutine per attached
router drains the FIFO and performs the sole stream `Send`. Overflow policy is
per traffic class, because the three classes carry different droppability
contracts (deliver = refuse-to-sweep, signal = best-effort drop, command =
fail-fast). The queue and sender are per-ATTACHMENT: created in `attach`,
torn down and joined in `detach`.

### (a) The queue, the frame, and the sender goroutine

```go
// frameClass tags an outbound frame with its overflow/teardown contract.
// The three classes share one FIFO — they share the wire, and a command
// gains nothing by overtaking a deliver — but their full-queue and
// detach-time semantics differ (see (b)-(d)).
type frameClass int

const (
    frameDeliver frameClass = iota // send1: send-only, cursor-backstopped
    frameSignal                    // push: fire-and-forget, best-effort
    frameCommand                   // dispatch: pendingCall-correlated, blocking caller
)

// outFrame is one queued outbound Sessions frame.
type outFrame struct {
    cmd   *compassv1internal.SessionsResponse
    class frameClass
}

// sendQueueCap bounds the per-router outbound queue. See Open Questions for
// the value; 256 proposed — a frame is one small proto pointer, so memory is
// negligible; the bound exists to fail fast on a wedged stream, not to save
// memory.
const sendQueueCap = 256

// senderState is one attachment's queue + sender goroutine. A fresh
// senderState per attach means a re-attached stream never inherits stale
// frames from a previous attachment.
type senderState struct {
    queue chan outFrame
    done  chan struct{} // closed when the sender goroutine exits
}
```

`commandRouter` replaces its `send func(...)` + `sendMu` pair with
`sender *senderState` (guarded by `mu`, as `send` is today —
`router.go:31-33`: "Set when a Runner's Sessions stream is live; nil before it
opens. Guarded by mu"). The sender goroutine:

```go
// runSender drains the attachment's queue into the stream. It is the ONLY
// caller of send for this attachment — the single-sender invariant that
// replaces sendMu (connect's BidiStream.Send is not concurrent-safe,
// router.go:71-75). It exits when the queue is closed (detach) or when a
// Send fails (the stream is dead; detach follows via the handler's defer).
func (r *commandRouter) runSender(s *senderState, send func(*compassv1internal.SessionsResponse) error)
```

On a `Send` error the sender fails THAT frame per class — a command frame's
pendingCall is **completed with the error**: set `call.err`, `close(call.done)`,
and `delete` it from `inflight`, all under `mu` and presence-checked, exactly as
`detach` (`router.go:132-136`) and `complete` (`router.go:271-276`) complete a
waiting call — NOT a delete-only. The delete-only shape of today's synchronous
push-failure path (`router.go:169-175`) is the mirror for the registration
REMOVAL alone; it never closes `done` because its caller is still inline in
`dispatch`, whereas here the caller is parked in `waitCall` on `call.done`
(`router.go:176`) since the real `Send` now happens later in `runSender` — so a
delete-only would hang that client Start/Stop RPC until ctx timeout. The
presence check is also what keeps this completion from double-closing against
`detach`'s in-flight sweep under `-race`. A deliver frame's `deliverRefusals`
entry is removed and the failure warn-logged (mirroring `router.go:234-243`); a
signal frame is warn-logged — then exits:
a failed `Send` means the stream is dead, and the handler's
`defer router.detach(errStreamClosed)` (`handler.go:108`) tears down the rest.
Frames still queued at that point follow the detach semantics in (e).

**`sendMu` is REMOVED, not kept as defense-in-depth.** Its stated reason —
"multiple client RPCs dispatch onto the one shared stream at once"
(`router.go:71-75`) — no longer exists: after this change no path calls `send`
except the sender goroutine, and a leftover mutex would invite a future direct
`send` caller to think serialization-by-lock is still a supported pattern.
The single-sender invariant is documented on `runSender` and enforced by the
type: the raw `send` function is captured by `attach` into the sender
goroutine and stored nowhere else.

### (b) `send1` — deliver overflow = synchronous refusal, reusing the existing backstop

```go
// send1 registers the refusal-only entry and enqueues the deliver
// NON-BLOCKING. nil means "queued", no longer "pushed" — success still rides
// the later AgentFrame.delivery_ack exactly as before. A nil sender (no
// attached stream) or a FULL queue returns the "no live stream"-class
// refusal error; the caller (the delivery consumer) already treats any error
// as "no live session" and leaves the cursor unadvanced for the D2 sweep.
func (r *commandRouter) send1(cmd *compassv1internal.SessionsResponse) error
```

Body: under `mu` — nil `sender` returns the existing refusal
(`router.go:221-224`: `return fmt.Errorf("no live runner sessions stream for
deliver %q", id)`); otherwise `deliverRefusals.Add(id, ...)` as today
(`router.go:228`), then a non-blocking `select` enqueue. On a full queue:
`deliverRefusals.Remove(id)` (no frame will reach the Runner, so no refusal
can arrive — the same reasoning as today's push-failure cleanup,
`router.go:235-241`) and return
`fmt.Errorf("runner send queue full for deliver %q", id)`.

**This synchronous refusal is why (B) needs no delivery-side self-heal.** The
critic's overflow concern against the rejected (A) was real: a queue-overflow
DROP leaves the stream attached, so the detach → re-enroll → start-edge chain
never fires and nothing triggers the recovery sweep. Under (B) the overflow is
not a drop — it is a synchronous error return on the very call the consumer is
making, and the consumer's EXISTING refusal path handles it:
`gatedDispatch` warn-logs and leaves the message to the sweep
(`go/internal/delivery/dispatch.go:275-278`: "dispatch to session failed,
leaving to sweep"), the cursor was never advanced
(`dispatch_control.go:30-32`), and the D2 sweep redelivers on the recipient's
next start (`dispatch.go:239-240`). Zero new delivery-side machinery; the
error class already exists (`dispatch_control.go:26-28`: "The error return is
a SYNCHRONOUS refusal only: no Runner enrolled, or no live Sessions stream /
an immediate push failure").

### (c) `push` — signal overflow = best-effort drop

```go
// push enqueues the signal NON-BLOCKING. A nil sender remains a no-op nil
// (best-effort: "the Runner re-fetches its secret set on reconnect
// regardless", router.go:183-185); a full queue warn-logs and returns nil —
// the same best-effort contract, the signal is advisory.
func (r *commandRouter) push(cmd *compassv1internal.SessionsResponse) error
```

Today's contract already tolerates loss — a nil `send` is "a no-op success:
the signal is best-effort" (`router.go:182-185`, and both callers treat nil as
done: `secrets_signal.go:79`, `config_signal.go:63`). A full queue is the same
best-effort outcome with a warn log for observability. Caller-visible
consequence: `SignalSecretsVersion`/`SignalConfigVersion` now return nil for
BOTH enqueue-success and full-queue-drop, and a real `Send` error is handled
async in `runSender` — so a wedged stream no longer surfaces as an error to
those signal RPCs' callers (today `SignalSecretsVersion`'s loop can return a
push error). Acceptable because the signal is advisory and the Runner re-fetches
on reconnect (`router.go:182-185`); the multi-Runner `errors.Join` TODOs in
those callers are unaffected.

### (d) `dispatch` — command overflow = fail-fast, never a silent drop

```go
// dispatch registers the pendingCall, enqueues the command NON-BLOCKING, and
// waits on waitCall exactly as today. A full queue DELETES the registration
// and returns an error immediately: a blocking command must never be
// silently dropped under a waiting caller, and the caller-side contract
// (commands.go:195-198 surfaces the error as CodeUnavailable; a retry
// re-issues by id) already handles a prompt failure.
func (r *commandRouter) dispatch(ctx context.Context, cmd *compassv1internal.SessionsResponse) (*compassv1internal.SessionsRequest, error)
```

Body: the id check, idempotent join (`router.go:150-156`), nil-sender error
(`router.go:157-160`), and pendingCall registration (`router.go:161-164`) are
unchanged. The inline `sendMu`+`send` (`router.go:166-168`) becomes a
non-blocking enqueue; on a full queue, delete the registration and return an
error — **the exact shape of today's synchronous push-failure path**
(`router.go:169-175`: "The push failed; drop the registration so a later
retry can re-issue"), so OQ6 idempotent retry semantics are untouched: the
failed attempt leaves no registration, a retry with the same id re-issues
cleanly, and a retry racing a still-queued first attempt joins its live
pendingCall. On a successful enqueue, `waitCall(ctx, call)` as today
(`router.go:176`) — the caller's ctx still bounds the wait.

Fail-fast (vs block-with-deadline) is recorded as a decision, not a fork —
see Resolved decisions.

### (e) Sender lifecycle — attach starts, detach drains-fails-joins

- **`attach`** (`router.go:117-123` today: binds `r.send` under `mu`) now
  constructs a fresh `senderState` (queue + done), stores it under `mu`, and
  spawns `runSender` with the captured `stream.Send`. Called from the Sessions
  handler exactly as today (`handler.go:107`: `router.attach(stream.Send)`).
- **`detach`** (`router.go:125-137` today: nils `send`, fails every in-flight
  call) now additionally: takes the `senderState` and nils it under `mu`,
  closes the queue (safe: every enqueue is a non-blocking send performed
  under `mu` after a non-nil sender check, so no send can race the close),
  then waits on `done` outside `mu` — the join. The wait is bounded: `detach`
  runs in the handler's defer AFTER the stream is torn down
  (`handler.go:108`), so a sender parked in a wedged `Send` has already been
  unblocked by the teardown; it drains or error-exits promptly. Failing the
  in-flight calls stays in `detach` as today — which is exactly what handles
  a QUEUED-but-unsent command frame: its pendingCall is in `inflight` and
  detach fails it (`router.go:132-136`).
- **Queued-but-unsent frames at detach follow their class**: a command's
  pendingCall is failed by detach (above — the caller observes the detach
  cause, as today); a deliver's cursor was never advanced, and detach IS the
  causally-coupled recovery trigger — re-enroll → session re-promotion →
  start edge → D2 sweep (the existing refusal contract,
  `dispatch.go:239-240`); a signal is best-effort by contract
  (`router.go:182-185`). The sender simply discards frames it dequeues after
  the queue is closed-and-drained; no per-frame teardown work beyond the
  command-class pendingCall failure detach already performs.
- **Cross-class FIFO is deliberate.** The three classes share one wire, so a
  priority scheme buys nothing: a command behind a wedged deliver is blocked
  by the stream, not the queue, and under a healthy stream the queue depth is
  ~0. One FIFO keeps the ordering argument trivial (decision order = wire
  order) and per-class semantics fully independent.

### Alternatives considered

1. **(A) Delivery-side per-session dispatch workers — THE ORIGINAL DRAFT of
   this record, rejected by Matt's OQ-0 ruling (see Resolved decisions).**
   One worker goroutine per recipient session inside
   `go/internal/delivery/`, replacing the `gates` mutex; bounded queues with
   a drop + drain-empty self-heal sweep; `wg.Wait()` shutdown join; reap
   integration. Honest merits: blast radius confined to the delivery
   package; runnerhub — shared, higher-stakes code — untouched. Rejected
   because it fixes only delivery: `dispatch` (client Start/Stop RPCs) and
   `push` (signals) still HOL-block inline on the same `sendMu`
   (`router.go:166-168`, `:194-196`), the consumer grows worker-lifecycle /
   overflow / self-heal / join machinery that (B) makes unnecessary, and the
   overflow story needed NEW recovery machinery (the drain-empty self-heal)
   precisely because a delivery-side drop fires no start edge — whereas (B)'s
   overflow is a synchronous refusal the consumer already handles (see (b)).
2. **Timeout-bounded inline Send.** Caps rather than removes the HOL block
   (every wedged-stream dispatch still stalls the caller for the timeout,
   serially), and there is no ctx-aware Send to interrupt — `DispatchControl`
   accepts ctx but "it is not currently consulted by the non-blocking send
   path" (`dispatch_control.go:34-37`); making the Send interruptible is
   runnerhub surgery anyway, at which point it is dominated by the queue.
3. **Goroutine-per-send.** connect's server-side `BidiStream.Send` "is not
   safe for concurrent use" (`router.go:71-75`), so every spawned goroutine
   still serializes on `sendMu` — no concurrency gained, plus an unbounded
   population of goroutines parked on the mutex behind a wedged Send. The
   merged sibling's goroutine-per-command shape fits the Runner side because
   commands there are independent units of WORK; here the contended resource
   is one stream.

Honest tradeoff of the chosen (B): it modifies `go/internal/runnerhub/` —
shared, higher-blast-radius code carrying the client-facing command relay —
and it changes `send1`/`push`'s meaning of a nil return from "pushed" to
"queued". Both are accepted by the OQ-0 ruling; the contract-preservation
constraints above pin what must not move.

## Plan

Tasks are ordered red-first (the merged sibling's posture); each is
independently reviewable. All in package `go/internal/runnerhub`. Gate:
`CGO_ENABLED=1 go test -race ./go/internal/runnerhub/...`.

### T1 — Red-first isolation, overflow, ordering, and lifecycle tests

Write the failing tests before any production change, against
`newCommandRouter()` + `attach` with test send functions (the pattern the
package's tests already use, e.g. `router_test.go:91-92`:
`send := newRecordingSend(); r.attach(send.send)`). All event-gated on
channels, no sleeps. Red today because every path Sends inline under `sendMu`:

1. **A wedged Send does not block another router**: two routers; router A's
   send parks on a channel; assert router B's `send1` completes while A is
   parked. (Red: nothing blocks today across routers — this one is the
   baseline guard and should be green from the start; keep it as the
   regression fence.)
2. **A wedged Send does not block the caller**: router A's send parks;
   `send1` returns nil promptly (queued) instead of parking; `push` returns
   nil promptly; `dispatch` returns control to `waitCall` (observable via a
   ctx-cancel unblocking it) rather than parking inside the send. (Red: all
   three park inside `sendMu`+`send` today.)
3. **Full queue, deliver class**: park the send; enqueue past `sendQueueCap`;
   assert the overflowing `send1` returns a non-nil error promptly (the
   refusal class) and its `deliverRefusals` entry is removed (a later
   `complete` for that id is a no-op unknown, `router.go:286-288`).
4. **Full queue, command class**: park the send; fill the queue; assert
   `dispatch` returns a non-nil error promptly (not hanging to ctx timeout)
   and the id is NOT left registered — a subsequent retry with the same id
   re-issues rather than joining a phantom call.
5. **Full queue, signal class**: park the send; fill the queue; assert `push`
   returns nil (best-effort) and the process does not block.
6. **FIFO order preserved to the wire**: enqueue an interleaved sequence of
   delivers/commands/signals; unpark; assert the recording send observed
   exactly the enqueue order.
7. **Leak-free sender join on detach**: park the send mid-frame; tear down
   (unblock the send with an error, as a real stream teardown does) and
   `detach`; assert the sender goroutine exited (done-channel observable)
   and every queued command's pendingCall was failed with the detach cause
   (the existing detach contract, `router.go:132-136`), under `-race`.
8. **Queued deliver at detach leaves recovery to the sweep**: queue a
   deliver, detach before it sends; assert no panic, the frame never reaches
   the send, and `complete` for its id is a no-op — the cursor-unadvanced
   backstop (`dispatch.go:239-240`) is the recovery, exercised end-to-end by
   the existing delivery-side tests, unchanged.
9. **Send-failure per-class teardown**: send fails on the first frame; assert
   a command frame's parked `dispatch` caller UNBLOCKS with the error (its
   `waitCall` returns the Send error — not merely that the id left `inflight`),
   a deliver frame's refusal entry is removed, and the sender exits.

Interfaces: test-only; drives `newCommandRouter()`, `attach`, `detach`,
`send1`, `push`, `dispatch`, `complete` — the package's existing test surface.
Existing tests must be audited in T3 on TWO axes for the field/contract change:
(i) SYNCHRONY — tests that assert send1/push push completion (e.g.
`deliveryarm_test.go`, `secrets_test.go`, `config_signal_test.go` attach a
recording send and assert the frame arrived) now need an explicit drain barrier
(wait for the recording send to observe the frame) instead of assuming synchrony,
because the frame is queued-not-pushed; (ii) FIELD READERS — tests that read the
`send`/`sendMu` fields directly must switch to `sender`. A package grep found
exactly one: `helpers_test.go:57` (`attached := router.send != nil` in
`waitRouterAttached`) — a COMPILE break once `send` is removed, which fails the
`-race` gate before any test runs, so it is named in T2's touched set.

### T2 — senderState + runSender + attach/detach lifecycle (the structural change)

Introduce the queue and sender; rewire attach/detach. Greens tests 1-2, 6-9.

Interfaces (all in `go/internal/runnerhub/router.go` unless noted):

- `type frameClass int` (`frameDeliver`, `frameSignal`, `frameCommand`)
- `type outFrame struct { cmd *compassv1internal.SessionsResponse; class frameClass }`
- `const sendQueueCap = 256` (Open Questions rules the value)
- `type senderState struct { queue chan outFrame; done chan struct{} }`
- `commandRouter`: field `send func(...)` and `sendMu sync.Mutex` REPLACED by
  `sender *senderState` (guarded by `mu`); the `sendMu` doc comment's
  concurrency constraint (`router.go:71-75`) moves to `runSender` as the
  single-sender invariant.
- `func (r *commandRouter) attach(send func(*compassv1internal.SessionsResponse) error)`
  — signature unchanged (`handler.go:107` caller untouched); constructs the
  senderState, spawns `runSender`.
- `func (r *commandRouter) detach(cause error)` — signature unchanged
  (`handler.go:108` caller untouched); nils + closes the queue under `mu`,
  fails in-flight calls as today (`router.go:128-137`), joins on `done`
  outside `mu`.
- `func (r *commandRouter) runSender(s *senderState, send func(*compassv1internal.SessionsResponse) error)`
  — FIFO drain; sole `send` caller; per-class frame failure on Send error
  (command → complete the pendingCall with the error: set `call.err` +
  `close(call.done)` + `delete` under `mu`, presence-checked, per `detach`
  `router.go:132-136` / `complete` `router.go:271-276` — NOT delete-only;
  deliver → warn + `deliverRefusals.Remove`; signal → warn), then exit.
- `go/internal/runnerhub/helpers_test.go` — `waitRouterAttached` (`:57`) reads
  the removed `send` field (`router.send != nil`); switch its readiness probe
  to `router.sender != nil` (still under `router.mu`), or the test package fails
  to compile and the `-race` gate never runs.
- `func (r *commandRouter) enqueue(f outFrame) bool` — under `mu`,
  non-blocking `select` send; false on nil sender or full queue (callers
  distinguish the two under the same `mu` hold).

### T3 — Convert send1/push/dispatch to enqueue with the per-class overflow policy

Greens tests 3-5; audits/converts existing tests for the queued contract.

Interfaces (signatures all unchanged — callers `dispatch_control.go:53`,
`commands.go:195`, `secrets_signal.go:79`, `config_signal.go:63` untouched):

- `func (r *commandRouter) send1(cmd *compassv1internal.SessionsResponse) error`
  — refusal-entry Add, non-blocking enqueue; full queue → Remove entry +
  return the refusal-class error (Approach (b)). Doc comment updated: nil
  means queued; success still rides delivery_ack; the full-queue error is
  handled by the consumer's existing leave-to-sweep path.
- `func (r *commandRouter) push(cmd *compassv1internal.SessionsResponse) error`
  — non-blocking enqueue; nil sender → nil (unchanged); full queue →
  warn-log + nil (Approach (c)).
- `func (r *commandRouter) dispatch(ctx context.Context, cmd *compassv1internal.SessionsResponse) (*compassv1internal.SessionsRequest, error)`
  — register pendingCall, non-blocking enqueue, `waitCall` as today; full
  queue → delete registration + immediate error (Approach (d), mirroring
  `router.go:169-175`).
- Existing-test audit: any test asserting a frame is observable the moment
  send1/push returns gains an explicit recording-send drain barrier
  (channel-gated, no sleeps).

### T4 — Changelog + record cross-references

Changelog entry; cross-reference note (this record ↔ the merged
`compass-runner-concurrent-dispatch`: server/runner counterparts of the same
HOL-blocking theme, landed independently). No other docs change.

Interfaces: none (docs only).

## Tasks

- [ ] T1: red-first tests — cross-router isolation (1), non-blocking callers
      under a wedged Send (2), per-class full-queue policy (3-5), FIFO to the
      wire (6), leak-free detach join + queued-frame class semantics (7-8),
      Send-failure per-class teardown (9); all channel-gated, all under
      `-race`
- [ ] T2: `frameClass`/`outFrame`/`senderState` + `runSender` +
      `enqueue`; `send`+`sendMu` replaced by `sender`; attach spawns, detach
      closes-fails-joins; single-sender invariant documented
- [ ] T3: `send1`/`push`/`dispatch` converted to non-blocking enqueue with
      the deliver=refuse / signal=drop / command=fail-fast overflow policy;
      existing-test synchrony audit
- [ ] T4: changelog + sibling cross-references
- [ ] Gate: `CGO_ENABLED=1 go test -race ./go/internal/runnerhub/...` green

## Resolved decisions

- **OQ-0 — WHERE the fix lives: (B) runnerhub-internal per-router send queue.**
  Ruled by Matt, 2026-08-23. The delivery-side per-session-worker design
  (formerly this record's body) is recorded as rejected Alternative 1; the
  delivery consumer is untouched under (B).
- **OQ-1 — delivery-worker granularity: MOOT.** The fork sized delivery-side
  workers (per-session vs per-Runner vs per-dispatch); under (B) there are no
  delivery-side workers to size. The granularity is fixed by the ruling: one
  queue + one sender goroutine per router — exactly the `sendMu` block domain
  ("One router per attached Runner", `router.go:28-29`).
- **OQ-2 — overflow policy: decided per class by the ruling + the existing
  per-class contracts.** Deliver → synchronous refusal reusing the existing
  cursor/D2-sweep backstop (`dispatch.go:239-240`,
  `dispatch_control.go:26-32`); signal → best-effort drop (the contract
  `push` already carries for a nil sender, `router.go:182-185`); command →
  fail-fast with registration deletion, the exact shape of today's
  push-failure path (`router.go:169-175`), preserving OQ6 idempotent retry
  (`router.go:150-156`). Fail-fast over block-with-deadline for commands:
  the caller-side contract already handles a prompt error
  (`commands.go:195-198` surfaces `CodeUnavailable`; a retry re-issues by
  id), a deadline would need a value with no principled source, and a full
  queue means ≥`sendQueueCap` frames are already wedged ahead — a brief extra
  wait cannot help. Residual: the bound VALUE only (below).
- **OQ-3 — landing posture: land independently; the sibling is already
  MERGED.** The runner-side counterpart shipped as commit 2ebdf09d
  ("feat(runner): concurrent per-command Sessions dispatch (RIG-1575)") on
  main. Nothing to gate on. (Aside, out of scope here: that record's
  `Status: Draft` header is stale — code merged, doc never flipped.)
- **sendMu keep-vs-drop: DROP.** The single sender goroutine is the sole
  `send` caller per attachment, so the mutex's reason (`router.go:71-75`) is
  gone; keeping it as defense-in-depth would advertise a direct-send pattern
  this design forbids. Non-load-bearing — trivially reversible in review.

## Open Questions

### OQ-A (not load-bearing — default chosen; flag only if Matt objects): the queue bound value

`sendQueueCap = 256` proposed. A frame is one small proto pointer, so memory
is negligible at any plausible value; the bound's job is to convert an
unbounded pile-up behind a wedged stream into prompt per-class failures. 256
absorbs a burst of delivers across every session on one Runner plus command
and signal chatter without triggering false-positive refusals under a merely
SLOW (not wedged) stream; a wedged stream fails fast after one queue's worth.
A tunable const, not architecture. Default: 256.
