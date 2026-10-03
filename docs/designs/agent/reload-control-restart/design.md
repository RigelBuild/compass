# Reload control restart

Tracker: RIG-4195. Freezes on merge. Supersedes the reload barrier-lift in [the reload replay barrier record](../reload-replay-barrier/design.md) (DL-372).

## Problem / Intent

DL-372 froze a reload design: displace the old Control subscription, then append `replay_complete` after the relaunch. Main shipped a different mechanism first, `controlProducer.Restart`. It also covers retained ops the old process never acked, which DL-372 left open as a hazard. This record states what reload actually does, so the ledger matches the code.

## Approach

`agentHost.reloadLocked` in `go/internal/runner/host.go` stops the old stream. Then, before `h.link.StartAgent`, it calls `SocketListener.RestartSession(sessionID, replayCompleteOp())`. On an ERRORED session, Bind recreates the state that was retired at exit. `controlProducer.Restart` in `go/internal/runner/gateway/control.go` does all of this under the session lock:

- It advances the epoch and the subscription generation, closes the wake channel and clears liveness. The old stream ends, and acks from the old process are fenced off by epoch.
- It stamps `replay_complete` as seq 1.
- It renumbers every retained op after the lift, in order, and drops any earlier lift.
- It resets the ack cursor to 0 and clears the hold.

So the lift is the replacement process's first op. Ops the old process received but never acked are redelivered behind the lift, at least once, as on a reconnect. A concurrent `Send` cannot land ahead of the lift.

### Alternatives considered

- **DL-372 as frozen (displace, then append the lift after StartAgent).** Rejected. The lift gets the highest seq, so stale retained ops drain ahead of it, and the replacement refuses and acks each one, so they are lost.
- **Hold for replay, release on the lift ack (proposed DL-373 and DL-374).** Rejected. `Restart` already orders the lift first, so a hold adds a second mechanism for the same ordering.

## Global Constraints

- The wire op stays `compass.v1.AgentControl.replay_complete`; no proto change.
- Reload keeps the session ID, container identity and persisted transcript.
- An unserved container skips the restart and keeps its current behavior.

## Plan

No code change. Main already implements this. Coverage on main is `TestControlRestartQueuesReplayCompleteThenUnackedOps` and `TestControlRestartFencesReplacedProcessAcks` in `go/internal/runner/gateway/control_test.go`. A reload-level test that acks the start lift first, and fails without the reload lift, is tracked separately.

## Tasks

- [x] Ledger: add DL-398, flip DL-372 to superseded, and mark the reload replay barrier record superseded.
