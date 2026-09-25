# Reload replay barrier

Tracker: RIG-3854. Freezes on merge.

## Problem / Intent

`agentHost.Start` sends `ReplayComplete` on every served start, but `reloadLocked` relaunches the agent inside the same session without sending it. The replacement agent process starts with its barrier closed (`#replayComplete = false` in `packages/compass-agent/src/agent.ts`), and the Runner's control retention does not re-deliver the original signal: `controlProducer.serve` derives each subscription's high-water mark with `sent := s.cursor`, commented "Start from the acked cursor", and `AckControl` prunes at-or-below-cursor ops from retention outright — so the original op is not merely un-resent, it no longer exists. No retention-side change could have re-delivered it.

The barrier therefore stays closed for the life of the reloaded session, and the agent refuses every live control class behind it: **deliver** first — the case the fresh-start lift exists for, per DL-189 ("so the first case-2 deliver is not refused-and-stranded") — plus live prompt, steer, and forge notification, guarded in `agent.ts`'s `#applyControl` and at decode in `control-source.ts`.

The production trigger is config refresh: `reloadLocked`'s callers are `Reload` and `refreshOneContainer`, the per-container leg of `RefreshConfig`, which calls `reloadLocked` directly because the lock is non-reentrant. That is the path DL-081 froze as the MVP config-update mechanism (re-materialize + in-place agent Reload). Make reload match start without changing the replay protocol or the persisted transcript contract.

## Approach

Factor `Start`'s replay-complete send into one host-local helper and call it from `reloadLocked` after the replacement stream is installed and the session is marked ready. Placing the call inside `reloadLocked` covers both callers structurally, rather than at either call site. The helper resolves `h.sockets[containerName]` under `h.mu`, releases the lock, then calls `listener.SendControl`, matching `Deliver`'s resolve-then-send protocol; the producer has its own locking. The session is already bound from `Start`, so reload never rebinds and the session ID, container identity, and durable transcript are untouched. Error policy is unchanged: a send failure is logged and does not fail an otherwise successful reload, because `StartAgent` already replaced the process and there is nothing to roll back to.

"Match start" holds for the barrier lift, but **not** for first-op position, and the difference is not cosmetic — see the retained-op constraint below. `Start` binds a fresh session whose control state is empty, so its replay-complete is genuinely the agent's first op (pinned by `TestFreshStartSendsReplayCompleteFirst`). Reload keeps the session bound, so retention, cursor, and sequence survive and the new lift is appended last.

## Global Constraints

- The wire operation remains `compass.v1.AgentControl.replay_complete`; no proto field or enum changes.
- The signal is emitted only when the container has a served listener; an unserved container keeps current behavior.
- Reload must keep the same session ID, container identity, and persisted transcript, and must not rebind or retire control state.
- The send happens after the `h.mu` section purely so the session's recorded state is coherent. Delivery does **not** depend on that ordering, and does not depend on a live subscription: `controlProducer.Send` retains above the ack cursor ("It succeeds whether or not a subscription is live: retention is what makes 'queued until acked' true"), and `serve` re-derives each new subscription's high-water mark from the cursor, so the op drains whenever the replacement process subscribes. The send path is `SocketListener.SendControl`, which delegates to `Send`. **`SendIfLive` must not be used** — it fails rather than queues when no subscription is bound, which is exactly the normal case at the moment `reloadLocked` finishes, and would silently drop the lift.
- **Reload does not reorder retained pre-reload live ops behind the barrier lift, and this change does not recover them.** `s.stream.Stop()` SIGKILLs the pre-reload agent, so any op it received but never acked stays retained above the cursor; the new replay-complete carries the highest seq and `collectBatch` walks ascending, so those stale ops drain *ahead* of the lift and the replacement agent refuses-and-acks each one (`control-source.ts` calls `acks.markApplied(seq)` on the refusal path), dropping them permanently. The hazard predates this change and is out of scope here; this record lifts the barrier, it does not close that gap.
- The send resolves the listener under `h.mu` and sends outside it, never holding `h.mu` across `SendControl`.
- A listener-send failure is logged and does not turn a completed relaunch into a reload error.
- No database, token, container cleanup, or live-environment mutation is part of this change.

## Plan

1. Extract `Start`'s replay-complete construction and send into a host-local helper that takes the session ID and container name.
2. Call the helper from `Start` in place of the inline block, preserving current ordering and log fields.
3. Call the helper at the end of `reloadLocked`, after the `h.mu` section that swaps `s.stream` and sets `AGENT_SESSION_STATE_READY`.
4. Add Runner tests for reload delivery, config-refresh delivery, unserved-container behavior, and the non-fatal send-failure policy.
5. Run `gofmt` on changed files and the focused `go/internal/runner` tests.

## Tasks

- [ ] **Send replay completion after reload**  
  **Interfaces:** consumes `agentHost.reloadLocked(ctx, sessionID string) error`, `h.sessions`, `h.sockets`, and `AgentControl_ReplayComplete`; sends via `SocketListener.SendControl` (delegating to `controlProducer.Send`, never `SendIfLive`); produces a single replay-complete send per reload on the replacement served listener, covering both `Reload` and `refreshOneContainer`. One send per reload is the emission contract; it is not a claim about how many the replacement subscription drains, which depends on the session's ack cursor.
- [ ] **Test reload barrier behavior**  
  **Interfaces:** consumes the existing host test helpers, fake `StartAgent` link, and `newConfigRefreshFixture` in `config_refresh_test.go`; produces four assertions — (1) after a reload the replacement subscription receives a replay-complete op for the same session ID; (2) a config-version bump driving `RefreshConfig` delivers it on the refreshed container's listener; (3) an unserved container sends none; (4) a send error leaves `reloadLocked` returning nil with the session `READY`. **Assertion (1) must not be written as "exactly one".** `AckControl` is driven by the real agent over the wire and has no in-process caller, so a Go fixture never advances the cursor: `Start`'s original op stays retained above it and a post-reload subscription legitimately drains *two* replay-completes. Assert that a replay-complete arrives and that the last op is one — never a count. Cases 3 and 4 are **white-box-only states with no production path**: reach (3) by deleting the container's entry from `h.sockets` under `h.mu`, and (4) by calling the exported `SocketListener.RetireSession(sessionID)` before the reload so `controlProducer.send` returns `errNoBoundSession`. Every other `SendControl` failure mode is unreachable in production — a served listener always has a wired producer, `ReplayComplete` is representable, and the retention cap exempts replay-path ops.
- [ ] **Run focused verification**  
  **Interfaces:** consumes changed `go/internal/runner` sources and tests; produces formatter output plus focused `go test` evidence.

## Open Questions

No load-bearing questions. Three resolved during grounding, one deferral recorded:

- **Why retention does not already cover this:** `controlProducer.serve` sets `sent := s.cursor` ("Start from the acked cursor"), and `AckControl` prunes at-or-below-cursor ops from retention entirely. A reload needs a fresh send; no retention-side change could have re-delivered the original.
- **Whether a duplicate lift is harmful:** `agent.ts`'s `#applyControl` sets `#replayComplete = true` when the op is **applied** (not on receipt — `control-source.ts` is apply-then-ack, and keeps its own decode-time `replayComplete` flag, so both layers open independently). A second lift only re-sets a boolean and cannot regress an open barrier.
- **Whether placing the send in `reloadLocked` covers `RefreshConfig`:** yes — `refreshOneContainer` calls `reloadLocked` directly, so one call site serves both callers.
- **Deferred, non-load-bearing:** recovering the pre-reload retained ops that drain ahead of the lift. `controlProducer.HoldForReplay` exists for exactly this ("Raised by the lifecycle when it restarts an agent into an existing session") and has no production caller today; pairing it with the `ReplayCompleteAck` release would close the gap. Out of scope for this record; tracked as RIG-3923.
