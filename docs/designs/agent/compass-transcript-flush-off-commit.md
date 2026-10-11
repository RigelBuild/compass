# Transcript flush off the commit path

> Freezes on merge; later changes supersede by citation, never rewrite.
> Linear: RIG-5158 (root cause of the RIG-5157 CI failure). Parent:
> `docs/designs/agent/compass-agent-session-persistence/design.md` § *T4*.
> Refines DL-093's flush timing. This record adds to the parent's two-tier
> store; it does not rewrite it.

## Problem / Intent

`Store.AppendTranscriptEntry` (`go/internal/store/agent_transcripts.go`)
inserts the row, then flushes inline:

```go
if checkpoint {
	return s.primaryFlush(ctx, sessionID, entrySeq)
}
return s.maybeSafetyValve(ctx, sessionID)
```

Both reach `flushUpto`, which calls
`if err := s.objectStore.PutSegment(ctx, key, body); err != nil {`
(`S3ObjectStore.PutSegment` in `go/internal/store/objectstore.go` has no
deadline of its own). This runs inside `Hub.CommitConversationFrame`
(`go/internal/runnerhub/relay_comms.go`), on the request ctx:
`if err := h.commitFrame(ctx, transcripts, sessionID, req.GetFrame(), req.GetIdempotencyKey()); err != nil {`.
So with the object store hung:

1. The commit waits on the PUT. The agent's unary attempt times out after
   `const DURABLE_CALL_TIMEOUT_MS = 5000;`
   (`packages/compass-agent/src/transport/frame-sink.ts`).
2. A flush error maps to `CodeInternal`. The agent retries the same key on
   `DURABLE_RETRY_BACKOFF_MS`. The retry dedups (`rowsAffected == 0`), skips
   the flush, and succeeds, because the first attempt committed the row.
3. The tee sends frames one at a time:
   `const run = this.#emitChain.then(() => this.#deliver(frame));`
   (`packages/compass-agent/src/session-tee.ts`). So each blocked flush stalls
   the transcript lane about 5s. Three of them push the e2e
   `TestLegS3DurabilityValves/s3_outage` past its budget,
   `transcriptPollBudget   = 15 * time.Second` (`go/e2e/agent_ops.go`).

Matt ruled option A: a commit returns once its row commits, and the flush
runs afterward, per session, on a worker the Store owns.

## Approach

**The store owns the worker and its lifecycle.** Only the store knows a flush
is owed, so the hub's `TranscriptStore` seam does not change. `Open` starts
the worker and `Close` stops it, so no binary or harness can forget it.

- **Commit path.** After a fresh insert (`rowsAffected > 0`),
  `AppendTranscriptEntry` calls `s.requestFlush(ctx, sessionID)` and returns
  nil. With no object store, and on a deduped retry, it requests nothing.
  `primaryFlush` is deleted. The pass takes the commit ctx's scope, never
  its deadline.
- **Flush pass.** `flushPass` works on the rows in PG at pass time: the
  primary step (`latestCheckpointSeq`, then `flushUpto(0, cp-1, superseded,
  prune, remark cp)` when cp > 0), then `maybeSafetyValve`. A pass does not
  depend on its trigger, so a failed pass heals at the next commit of either
  kind. A session that never commits again keeps its rows in PG; resume
  reads them, and session-end archival (`archiveSessionEnd`,
  `go/server/session_end_archive.go`) archives them.
- **Single flight per session.** `transcriptFlusher` holds one slot per
  session with a pass owed or running. `requestFlush` never blocks: it makes
  a slot and wakes the run loop, or sets `again` on a running slot. The loop
  starts one goroutine (`sync.WaitGroup.Go`) per slot not yet running. It
  runs a pass, then one more if `again` was set, else deletes the slot. So
  one session's passes never overlap in one process; triggers coalesce.
- **Concurrency cap.** A semaphore of `maxConcurrentFlushes = 4` gates each
  pass before it reads any rows.
- **Tenant.** The Runner request starts without a tenant. Before the append,
  `Hub.CommitConversationFrame` calls
  `ctx, _, ok := h.runnerSessionAccount(ctx, runnerID, sessionID)`, and
  `runnerSessionCtx` attaches the session binding's tenant
  (`return store.WithTenant(ctx, tenant), true`). So `resolveTenant(ctx)` at
  the append returns the session's tenant; bootstrap is only the fallback
  when no tenant is attached. The slot keeps `resolveTenant(ctx)` and
  `isSystemRole(ctx)` from that scoped ctx. A pass runs on
  `WithTenant(runCtx, slot.tenant)`, plus `WithSystemRole` when set.
- **Failure.** Each failed pass logs `"transcript flush failed"` at ERROR
  with `session_id`, adds 1 to `compass.store.transcript_flush_failures`,
  and is dropped. The next commit covers the rows; nothing retries in a loop.
- **Pass deadline.** A pass that holds the semaphore runs under
  `context.WithTimeout(runCtx, s.flushPassTimeout)`, default
  `defaultFlushPassTimeout = 60 * time.Second`. `flushUpto` gets no deadline,
  so session end keeps its own bounds. The largest normal segment, the
  primary one, is about one valve cap (64 MiB); 60s moves it at ~9 Mbit/s.
- **Shutdown.** `Open` starts the loop on
  `context.WithCancel(context.WithoutCancel(ctx))` and keeps only the cancel
  func (`containedctx` allows it). `Close` cancels, waits for the loop and
  every pass, then closes the pool. An in-flight PUT or tx fails on the
  cancelled ctx with PG untouched. `Serve` (`go/server/serve.go`) is as-is.
- **Commit contract.** `CommitConversationFrame` returns `committed=true`
  once the row commits. Object-store errors no longer reach it.
  `CodeInternal` now means a Postgres fault on the commit path (the
  `SessionBase` read or the insert).

**Resume stays complete.** `SessionResumeSnapshot` reads the PG tail from the
latest checkpoint and the `safety_valve` manifest in one REPEATABLE READ
snapshot. A deferred or failed flush only leaves more rows in PG. A prune
commits in its manifest row's tx, after the PUT, and only if it deleted
exactly the archived rows. Commits now land during a pass, so two races open:

1. **A checkpoint inside the valve's cut.** The valve reads the checkpoint
   seq, the byte total, and the row sizes in three statements. A checkpoint
   C2 that commits after the first read falls inside the evicted range
   `[C1+1..U]` and is pruned. Resume then takes C1 as the head, and
   `Hub.ReconstructSessionBody` (`go/internal/runnerhub/reconstruct.go`)
   splices C2's full body in as a delta. Guard: `HotTailBytes` and
   `HotTailSizes` derive the checkpoint inside the statement, and the cut
   stops before the first row whose `checkpoint` flag is set. Seqs commit in
   order per session (the tee's FIFO), so later rows land above the cut.
2. **A valve segment below a newer checkpoint.** A valve pass can commit its
   segment after C2 lands, before the next pass re-marks it, and resume
   would splice pre-C2 lines after the C2 body. Guard: the
   `SafetyValveSegments` query keeps only segments wholly above the latest
   checkpoint, so `SessionResumeSnapshot` and `Store.SafetyValveSegments`
   both get it. It is required, not hygiene: if the server stops first, the
   row stays `safety_valve` until the next checkpoint.

**Session end.** `Hub.archiveEnded` (detached, `sessionEndArchiveTimeout` =
2 minutes) and the inline, best-effort `archiveSessionEnd` in
`service.StopAgentSession` keep their bounds; neither causes this bug. An
overlap with a worker pass is safe: keys are deterministic, the manifest
insert is `ON CONFLICT DO NOTHING`, and `session_end` does not prune.

**Ledger.** DL-491 refines DL-093 (which stays Active):

> Transcript archive flushes (the compaction flush and the size-cap safety
> valve) run off the `CommitConversationFrame` commit path:
> `AppendTranscriptEntry` commits the row and returns, and a per-session
> single-flight flusher owned by the Store runs the flush afterward under a
> pass deadline, so an object-store fault never fails or stalls a commit.
> The database enforces that a pruning flush removes exactly the rows it
> archived. Refines DL-093; PUT-before-prune and the triggers are unchanged.

## Alternatives considered

- **Inline flush with a PUT deadline.** Above the agent's 5s attempt timeout
  it still stalls the lane and returns `CodeInternal`; below it, healthy
  large PUTs fail.
- **Wire the worker in `Serve`.** Harnesses that append through
  `newRunnerHub` without `Serve` (such as
  `go/server/service_resume_pgtest_test.go`) would run no flush and keep
  slots forever, silently. Store ownership costs one goroutine per `Open`.
- **Hub-owned, global, or detached workers.** The hub cannot tell a flush is
  owed without a `TranscriptStore` change. One global worker lets one hung
  PUT delay every session. A detached `go` per trigger (like `archiveEnded`)
  lets two valve passes for one session overlap, and shutdown cannot join it.
- **No concurrency cap.** `collectSegment` holds about three copies of a
  body while its PUT hangs, and the agent's 5s give-up no longer throttles
  that. A cap of 4 bounds it near 4 × 3 × 64 MiB.
- **A periodic sweeper as a fourth trigger.** It heals a session whose last
  pass failed and that never commits again. Resume does not need it,
  session-end archival already covers those rows, and it scans every tail.
- **Raise the e2e budget.** It hides a production stall.

## Global Constraints

- PUT-before-prune, deterministic segment keys, and the `ON CONFLICT`
  manifest insert are unchanged. No proto or agent change.
- The database, not the scheduler, owns disjointness. A pruning flush
  deletes exactly the rows it archived, or its tx rolls back. A losing
  pass's object has no manifest row, so no reader sees it.
- Thread the caller's ctx; no `context.Background()` outside `main` and
  tests. No ctx in a struct (`containedctx`); keep only the cancel func.
- No retry wrappers. Tests advance on channels; no `time.Sleep` (forbidigo).

## Plan

1. **One PR, two tasks.** T1 (store) and T2 (callers and tests outside the
   store) land together; T1 alone turns the synchronous server tests red.

## Tasks

- [ ] **T1 — Store-owned flusher behind the commit** (`implement-go`).
  Interfaces (new file `go/internal/store/transcript_flusher.go`):

  ```go
  const (
  	defaultFlushPassTimeout = 60 * time.Second
  	maxConcurrentFlushes    = 4
  )
  type flushSlot struct {
  	tenant                 TenantID
  	system, running, again bool
  	err                    error         // last pass result
  	idle                   chan struct{} // closed when the slot is deleted
  }
  type transcriptFlusher struct {
  	mu             sync.Mutex
  	slots          map[string]*flushSlot
  	wake, sem      chan struct{} // cap 1; cap maxConcurrentFlushes
  	wg             sync.WaitGroup
  	stop           context.CancelFunc
  	stopped        chan struct{}       // closed after the loop and passes end
  	failures       metric.Int64Counter // nil when the meter failed
  	passDone       func(sessionID string, err error) // test hook; nil in prod
  	afterValveRead func()                            // test hook; nil in prod
  }

  var errFlushRaced = errors.New("store: flush prune count mismatch")

  func (s *Store) startTranscriptFlusher(ctx context.Context)
  func (s *Store) requestFlush(ctx context.Context, sessionID string) *flushSlot
  func (s *Store) runTranscriptFlusher(ctx context.Context)
  func (s *Store) flushPass(ctx context.Context, sessionID string) error
  func (s *Store) FlushTranscript(ctx context.Context, sessionID string) error
  func valveCut(rows []db.HotTailSizesRow, capBytes int64) (from, upto uint64)
  ```

  - `FlushTranscript` is the synchronous barrier for tests. With no object
    store it returns the "object store not configured" error; else it calls
    `requestFlush`, waits for the slot's `idle`, `ctx.Done()`, or `stopped`,
    and returns the slot's last error.
  - `Open` sets `flushes` and `flushPassTimeout`, builds the counter as
    `NewComms` does (`go/internal/comms/comms.go`), and calls
    `startTranscriptFlusher(ctx)` after `BootstrapTenant`. `Close` calls
    `stop`, waits on `stopped`, then `pool.Close()`. New
    `instrumentationScope = "github.com/RigelBuild/compass/go/internal/store"`.
  - SQL (`go/internal/store/queries/agent_transcripts.sql`; regenerate sqlc):
    `HotTailBytes` and `HotTailSizes` take only `session_id` and filter
    `entry_seq > (SELECT COALESCE(MAX(entry_seq), 0) FROM agent_session_transcript_entries WHERE session_id = $1 AND checkpoint)`;
    `HotTailSizes` also returns `checkpoint`. `PruneTranscriptEntries`
    becomes `:execrows`. `SafetyValveSegments` adds `AND min_entry_seq >`
    the same subquery.
  - `maybeSafetyValve` drops its `latestCheckpointSeq` read, calls
    `afterValveRead` after `HotTailBytes`, and cuts with `valveCut`: evict
    the oldest rows until the rest fit the cap, never the newest row, and
    stop before the first checkpoint row. `flushUpto` with `prune` rolls back
    unless the delete count equals `len(seqs)`; the error wraps
    `errFlushRaced`.

  Tests (pgtest, package `store`; fakes block on `release` or `ctx.Done()`):
  - Non-blocking commit, for a checkpoint and a valve trigger: the append
    (10s ctx) returns nil while the PUT is blocked, the tail unpruned. After
    `release`, `FlushTranscript` returns nil; the segment is in the manifest
    and its rows are pruned.
  - Coalescing: with pass 1 blocked, append two checkpoints. After release,
    `passDone` fires exactly twice, no two PUTs overlap, and every row below
    the newest checkpoint is flushed.
  - Checkpoint inside the cut (cap 40): `afterValveRead` appends checkpoint
    C2 and a delta. After `FlushTranscript`, C2 is in PG, no segment covers
    it, and `SessionResumeSnapshot` heads with C2. The three-read valve fails
    this. A `valveCut` case with a mid-tail checkpoint cuts below it.
  - DB guard: Stores A (cap 40) and B (cap 80) share one DSN and a fake whose
    PUT waits for both passes, so their ranges overlap under different keys.
    A's append trips the valve; B calls `FlushTranscript`. Then one manifest
    row exists, each row is pruned once, and one pass reports `errFlushRaced`.
  - Deadline: with `flushPassTimeout = 50 * time.Millisecond` and a PUT that
    waits on `ctx.Done()`, `FlushTranscript` wraps `context.DeadlineExceeded`
    and PG is untouched.
  - Shutdown joins the pass: the fake PUT signals `cancelled` on
    `ctx.Done()`, then waits on a separate `release` channel. Start `Close`
    in a goroutine while the pass is blocked. After `cancelled` fires,
    `Close` has not returned; close `release`, and `passDone` fires before
    `Close` returns. A second Store on the DSN then sees the rows unpruned
    and no manifest row.
  - Tenant: an append under a second tenant's ctx yields a manifest row with
    that `tenant_id`. Filter: a `safety_valve` row below a later checkpoint
    is absent from both segment readers.
  - Rewrite the sync-flush tests in `agent_transcripts_test.go` to call
    `FlushTranscript` after the appends. In
    `TestAppendWithoutObjectStoreKeepsEntriesAndRetrySucceeds` the append
    returns nil and `FlushTranscript` returns the "not configured" error.

- [ ] **T2 — Callers and tests outside the store** (`implement-go`).
  - Hub pgtest (`go/internal/runnerhub`): with a real store and a blocking
    fake object store, `Hub.CommitConversationFrame` of a checkpoint frame
    (10s ctx) returns `Committed: true` while the PUT is blocked.
  - Harnesses: pgtests that append through `newRunnerHub` now run the
    worker. `TestStartAgentSessionResumeS3FallbackReconstructsEvictedEntries`
    (`go/server/service_resume_pgtest_test.go`) calls `FlushTranscript`
    before its segment checks. `go/server/service_sessionend_pgtest_test.go`
    stays under the default cap, so its passes write no segment. Confirm
    each harness closes its Store.
  - e2e `runS3Outage` (`go/e2e/leg_s3_durability_test.go`): keep
    `awaitTranscriptPersisted` as the gate on the committed row, budget
    unchanged. After CONT, a pass ends within its 60s deadline, inside the
    2-minute `settleTimeout` of `waitArchiveContains`. Fix stale comments.
  - Comment sweep: the docs on `AppendTranscriptEntry`, `FlushSuperseded`,
    `maybeSafetyValve`, `SafetyValveSegments` (its "By construction these are
    all post-latest-checkpoint" is now enforced by the query),
    `SessionResumeSnapshot`, `SetObjectStore`, and `Close`;
    `Hub.CommitConversationFrame`'s contract; the reconstructor's snapshot
    comment; the parent-record pointer in `TranscriptStore`.
