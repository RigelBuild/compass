# Reload replay barrier

Status: Superseded by ../reload-control-restart/design.md

Tracker: RIG-3854. Freezes on merge.

## Problem / Intent

`agentHost.Start` sends `ReplayComplete` on every served start, but `reloadLocked` relaunches the agent inside the same session without sending it. The replacement agent process starts with its barrier closed (`#replayComplete = false` in `packages/compass-agent/src/agent.ts`), and the Runner's control retention does not re-deliver the original signal: `controlProducer.serve` derives each subscription's high-water mark with `sent := s.cursor` (“Start from the acked cursor”; `serve` in `go/internal/runner/gateway/control.go`), and `AckControl` prunes at-or-below-cursor ops from retention (`AckControl` in `go/internal/runner/gateway/control.go`). No retention-side change could have re-delivered it.

The barrier therefore stays closed for the life of the reloaded session, and the agent refuses every live control class behind it: **deliver** first — the case the fresh-start lift exists for, per DL-189 ("so the first case-2 deliver is not refused-and-stranded") — plus live prompt, steer, and forge notification, guarded in `agent.ts`'s `#applyControl` and at decode in `control-source.ts`.

The production trigger is config refresh: `reloadLocked`'s callers are `Reload` and `refreshOneContainer`, the per-container leg of `RefreshConfig`, which calls `reloadLocked` directly because the lock is non-reentrant. That is the path DL-081 froze as the MVP config-update mechanism (re-materialize + in-place agent Reload). Make reload match start without changing the replay protocol or the persisted transcript contract.

## Approach

Factor `Start`'s replay-complete send into one host-local helper and call it from `reloadLocked` after the replacement stream is installed and the session is marked ready. After `s.stream.Stop()` succeeds and before `h.link.StartAgent`, displace the prior Control subscription through a `controlProducer` takeover method: under the session lock it bumps `sub`, closes `wake`, and clears `live` without changing retention or the ack cursor. This follows `controlProducer.serve` in `go/internal/runner/gateway/control.go`, which bumps `s.sub`, closes the predecessor's wake channel (“Retire the predecessor”), and sets `s.live = true`; its deferred cleanup only clears liveness when `s.sub == mine` (“Only the CURRENT subscription clears liveness”). Doing this before StartAgent is required because the replacement may subscribe before StartAgent returns; a later takeover would displace the replacement. The old drainer exits with a clean stream end. The replacement receives the lift. The helper resolves `h.sockets[containerName]` under `h.mu`, releases the lock, then calls `listener.SendControl`, matching `Deliver`'s resolve-then-send protocol; the producer has its own locking. This is runner-local and applies to every backend. It does not stop a survivor's in-progress turn. The session is already bound from `Start`, so reload never rebinds and the session ID, container identity, and durable transcript are untouched. Error policy is unchanged: a send failure is logged and does not fail an otherwise successful reload.

“Match start” holds for the barrier lift, but not for first-op position. `Start` binds a fresh session whose control state is empty, so its replay-complete is genuinely the agent's first op. Reload keeps the session bound, so retention, cursor, and sequence survive and the new lift is appended last. The takeover runs after stopping the old stream and before launching the replacement, preventing either the survivor or an early replacement subscription from acking the lift; the survivor's ongoing turn is not cancelled.

## Global Constraints

- The wire operation remains `compass.v1.AgentControl.replay_complete`; no proto field or enum changes.
- The signal is emitted only when the container has a served listener; an unserved container keeps current behavior.
- Reload must keep the same session ID, container identity, and persisted transcript, and must not rebind or retire control state.
- After `s.stream.Stop()` and before `h.link.StartAgent`, reload displaces the prior subscription. This ordering ensures that even if StartAgent establishes the replacement subscription before returning, the takeover cannot end the replacement stream. The send happens after the `h.mu` section so the session's recorded state is coherent. Delivery does not require a live subscription: `controlProducer.Send` retains above the ack cursor, and `serve` derives a new subscription's high-water mark from the cursor. A replacement subscription drains the lift.
- Reload does not reorder retained pre-reload live ops behind the lift, and this change does not recover them. On every backend, any op received but not acked remains retained above the cursor; the new replay-complete carries the highest seq and `collectBatch` walks ascending, so stale ops drain ahead of the lift and the replacement refuses and acks them. That pre-existing hazard is out of scope; `HoldForReplay` has no production caller today. Recovery remains tracked as RIG-3923.
- A survivor's in-progress turn may continue after its Control stream ends. Making `Stop` kill the in-container agent on CLI backends, with a real-podman smoke test, is a separate runtime follow-up: RIG-4181.
- The send resolves the listener under `h.mu` and sends outside it, never holding `h.mu` across `SendControl`.
- A listener-send failure is logged and does not turn a completed relaunch into a reload error.
- No database, token, container cleanup, or live-environment mutation is part of this change.

## Plan

1. Extract `Start`'s replay-complete construction and send into a host-local helper that takes the session ID and container name.
2. Call the helper from `Start` in place of the inline block, preserving current ordering and log fields.
3. In `reloadLocked`, after `s.stream.Stop()` succeeds, displace the old Control subscription before calling `h.link.StartAgent`; after StartAgent returns, swap the stream and set READY, then send replay-complete.
4. Add Runner tests for takeover stream end and retained state, reload delivery, config-refresh delivery, unserved-container behavior, and non-fatal send failure. The reload test must establish the replacement subscription inside `StartAgent` before it returns, then assert that subscription receives the lift.
5. Run `gofmt` on changed files and focused `go/internal/runner` tests.

## Tasks

- [ ] **Send replay completion after reload**  
  **Interfaces:** consumes `agentHost.reloadLocked(ctx, sessionID string) error`, `h.sessions`, `h.sockets`, and `AgentControl_ReplayComplete`; sends via `SocketListener.SendControl` (delegating to `controlProducer.Send`, never `SendIfLive`); produces a single replay-complete send per reload on the replacement served listener, covering both `Reload` and `refreshOneContainer`. One send per reload is the emission contract; it is not a claim about how many the replacement subscription drains, which depends on the session's ack cursor. The prior subscription is displaced after Stop and before StartAgent, so an early replacement subscription is not displaced; retention and cursor are unchanged.
- [ ] **Test reload barrier behavior**  
  **Interfaces:** consumes the existing host test helpers, fake `StartAgent` link, and `newConfigRefreshFixture` in `config_refresh_test.go`; produces six assertions — (1) takeover makes the old Control stream end cleanly; (2) takeover preserves retained ops and ack cursor; (3) a reload test establishes the replacement subscription before fake StartAgent returns and that subscription receives replay-complete for the same session ID; (4) a config-version bump driving `RefreshConfig` delivers it on the refreshed container's listener; (5) an unserved container sends none; (6) a send error leaves `reloadLocked` returning nil with the session `READY`. Reload assertions pin the lift by seq, never by kind or count.
- [ ] **Run focused verification**  
  **Interfaces:** consumes changed `go/internal/runner` sources and tests; produces formatter output plus focused `go test` evidence.

## Resolved decisions

Matt ruled RIG-4029 Option 3: after stopping the old stream and before calling StartAgent, reload displaces the old Control subscription. The takeover advances the subscription generation, closes its wake channel, and clears liveness while keeping retention and the ack cursor. The old stream ends cleanly. A replacement may subscribe before StartAgent returns; because takeover already ran, it remains live and drains the lift. This is runner-local and applies to all backends. It does not stop an in-progress turn. Making `Stop` kill the in-container agent on CLI backends is a separate runtime follow-up, RIG-4181.

- **Why retention does not cover the original lift:** `controlProducer.serve` in `go/internal/runner/gateway/control.go` sets `sent := s.cursor` (“Start from the acked cursor”), and `AckControl` in the same file prunes at-or-below-cursor ops from retention. Reload must send a fresh lift.
- **Why takeover works:** `controlProducer.serve` in `go/internal/runner/gateway/control.go` increments `s.sub`, closes the predecessor's `wake`, then marks the new subscription live. Its deferred cleanup clears liveness only if its generation is still current. Reload performs the takeover after stopping the old stream but before StartAgent can establish the replacement subscription, and leaves retained ops intact.
- **Duplicate lifts are harmless:** `agent.ts`'s `#applyControl` sets `#replayComplete = true` when applied; a second lift only re-sets a boolean.
- **RefreshConfig coverage:** `refreshOneContainer` calls `reloadLocked` directly, so one call site serves both callers.
- **Deferred, non-load-bearing:** recovering pre-reload retained ops that drain ahead of the lift remains out of scope; tracked as RIG-3923.
