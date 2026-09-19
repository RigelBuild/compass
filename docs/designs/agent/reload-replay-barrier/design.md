# Reload replay barrier

Tracker: RIG-3854. Freezes on merge.

## Problem / Intent

`agentHost.Start` sends `ReplayComplete` on every served start, but `reloadLocked` relaunches the agent inside the same session without sending it. The replacement agent process starts with its barrier closed (`#replayComplete = false` in `packages/compass-agent/src/agent.ts`), and the Runner's control retention does not re-deliver the original signal: each subscription's high-water mark starts at the session's ack cursor, so an op the pre-reload process already acked is never resent. The barrier therefore stays closed for the life of the reloaded session and live prompt/steer are refused indefinitely. Make reload match start without changing the replay protocol or the persisted transcript contract.

## Approach

Factor `Start`'s replay-complete send into one host-local helper and call it from `reloadLocked` after the replacement stream is installed and the session is marked ready. The helper resolves `h.sockets[containerName]` under `h.mu`, releases the lock, then calls `listener.SendControl`, matching `Deliver`'s resolve-then-send protocol; the producer has its own locking. The session is already bound from `Start`, so reload never rebinds and the session ID, container identity, and durable transcript are untouched. Error policy is unchanged: a send failure is logged and does not fail an otherwise successful reload, because `StartAgent` already replaced the process and there is nothing to roll back to.

## Global Constraints

- The wire operation remains `compass.v1.AgentControl.replay_complete`; no proto field or enum changes.
- The signal is emitted only when the container has a served listener; an unserved container keeps current behavior.
- Reload must keep the same session ID, container identity, and persisted transcript, and must not rebind or retire control state.
- The replacement stream must be installed and the session marked ready before the send, so the op reaches the new process.
- The send resolves the listener under `h.mu` and sends outside it, never holding `h.mu` across `SendControl`.
- A listener-send failure is logged and does not turn a completed relaunch into a reload error.
- No database, token, container cleanup, or live-environment mutation is part of this change.

## Plan

1. Extract `Start`'s replay-complete construction and send into a host-local helper that takes the session ID and container name.
2. Call the helper from `Start` in place of the inline block, preserving current ordering and log fields.
3. Call the helper at the end of `reloadLocked`, after the `h.mu` section that swaps `s.stream` and sets `AGENT_SESSION_STATE_READY`.
4. Add Runner tests for reload delivery, unserved-container behavior, and the non-fatal send-failure policy.
5. Run `gofmt` on changed files and the focused `go/internal/runner` tests.

## Tasks

- [ ] **Send replay completion after reload**  
  **Interfaces:** consumes `agentHost.reloadLocked(ctx, sessionID string) error`, `h.sessions`, `h.sockets`, and `AgentControl_ReplayComplete`; produces one replay-complete operation on the replacement served listener.
- [ ] **Test reload barrier behavior**  
  **Interfaces:** consumes the existing host test helpers, fake `StartAgent` link, and served-listener seams; produces assertions that reload delivers exactly one replay-complete op for the same session ID, that an unserved container sends none, and that a send error leaves `reloadLocked` returning nil with the session `READY`.
- [ ] **Run focused verification**  
  **Interfaces:** consumes changed `go/internal/runner` sources and tests; produces formatter output plus focused `go test` evidence.

## Open Questions

No load-bearing questions. Two resolved during grounding:

- **Why retention does not already cover this:** `controlProducer.serve` starts each subscription's high-water mark at the session's ack cursor, so an op the pre-reload process acked is not re-sent to the replacement subscription. A reload therefore needs a fresh send, not a retention change.
- **Whether a duplicate lift is harmful:** the agent sets `#replayComplete = true` on receipt, so a second replay-complete is idempotent; the reload send cannot regress an already-open barrier.
