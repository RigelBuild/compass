# Design: Order held delivery by turn sequence (RIG-4033)

Ledger-impact: appends DL-383 for Matt's option-3 ruling (design-ledger-gate)

## Problem / Intent

The delivery consumer holds agent-authored messages while the author's session is live, then fires held messages on `OnSessionSettled`. Because the settle edge is queued and drained asynchronously, a turn N+1 post can be held before turn N's edge drains; firing it with partial blocks and deduping by `message_id` can discard its later settled delivery. This record implements Matt's RIG-4033 option 3: order holds and settle edges by the agent-reported per-session turn sequence, not wall time.

## Approach

The agent owns a monotonically increasing `uint64` turn sequence for the lifetime of a session identity. It increments on each `agent_start` and attaches the current sequence to each `MessagePosted` it emits and to the lifecycle `AgentSessionFrame` that settles the turn. The Runner relays both values unchanged; the server passes the sequence from the session frame through `Hub.deliverSession` and `SettleSink.OnSessionSettled` to the delivery consumer. The consumer fires only holds whose sequence is <= the settle sequence. A hold made after its own settle edge was observed is immediately eligible through the existing settle/recheck path, but never by comparing timestamps.

Session identity scopes the sequence. A session resume that preserves the session identity must preserve and continue its counter; a new session identity starts at 1. Runner process restart and agent reconnect do not reset it when resuming the same session. If the agent cannot recover the counter for an existing session, it must start a new session identity rather than reuse a sequence. This prevents ambiguous ordering without server persistence or wall-clock assumptions.

### Proto and compatibility

- `proto/compass/v1/comms.proto`: add `uint64 turn_sequence = 2` to `MessagePosted` (field 1 remains `Message message`). The comms `MessagePosted` is the agent-to-Runner committed post frame (`CommitAgentPost` in `go/internal/comms/agent_caller.go`); `comms.publishMessagePosted` constructs the server's bus event in `go/internal/comms/mapping.go` and must preserve the incoming sequence when relaying it. Human/API-authored posts have no agent turn and use zero.
- `proto/compass/v1/compass.proto`: add `uint64 turn_sequence = 4` to `AgentSessionFrame` (fields 1–3 remain `session_id`, `event`, `state`). The agent sets it on the `agent_end` lifecycle frame produced by the mapper (`packages/compass-agent/src/mapping.ts`); non-settle trace frames need not carry it. Runner passes it to the server's settle sink.
- Zero/missing sequence from an older peer follows the pre-decided compatibility rule: retain today's fire-everything settle behavior. Log that fallback once per session, not once per message or edge. Do not infer a turn from `at_unix_ms`.
- Roles: the **agent** creates, persists/restores, increments and stamps both values; the **Runner** transports values and preserves them over reconnect; the **server** validates/consumes values, orders holds and bounds each settle fire. No server-side counter assignment.

The existing producer/consumer seams establish those owners: `AgentSessionFrame` is currently `session_id/event/state` (`proto/compass/v1/compass.proto`); `Hub.deliverSession` relays the frame then calls `SettleSink.OnSessionSettled` (`go/internal/runnerhub/hub.go`); `delivery.Consumer.OnSessionSettled` queues a settle with `upTo: math.MaxInt64` (`go/internal/delivery/settle.go`); `onMessagePosted` calls `hold` for a live author (`go/internal/delivery/dispatch.go`). The agent mapper maps `agent_end` to READY (`packages/compass-agent/src/mapping.ts`), and agent-authored posts are wrapped as `MessagePosted` by `postedFrame` (`go/internal/comms/agent_conversation_pgtest_test.go`) and committed via `CommitAgentPost` (`go/internal/comms/agent_caller.go`).

## Alternatives considered

- **Bind holds to settle wall time (option 1).** Rejected by Matt in favor of an explicit turn identity. Millisecond event and enqueue times describe when observations reach components, not which turn authored a message; delayed frames can cross the settle boundary.
- **Server- or Runner-assigned turn counter.** Rejected: only the agent observes `agent_start`, which defines turn boundaries. Assigning at relay/receipt would make delivery order depend on transport scheduling and reconnect gaps.

## Global Constraints

- Per-session turn sequence is unsigned 64-bit; zero means absent/legacy, positive values are agent-reported turns. Never derive it from timestamps or message ids.
- A numbered settle at N fires only held entries with sequence <= N. A later turn's hold remains held, even when its post races ahead of N's queued drain.
- Missing/zero sequence retains legacy fire-everything behavior and emits one fallback log per session.
- Reconnect/resume of the same session preserves its sequence; a new session identity starts at 1. A peer unable to restore sequence must not resume under the old identity.
- Protobuf field additions are additive. Generated Go and TypeScript bindings are regenerated from proto; no hand edits to generated output.
- Keep message-id dedup and existing recipient re-resolution, settle eligibility states, durable delivery cursor, and non-terminal DISCONNECTED semantics unchanged.
- The delivery code being designed against is the cutover at bookmark `compass-managed/rig-3107-t6-rulings`; `main` does not yet include that code. The cutover record already names this amendment path under `## Post-freeze amendments`.

## Plan

### T1 — Define and propagate the sequence fields

Add the two proto fields and thread them from the agent's per-session counter through posted frames and the `agent_end` settle frame, through Runner relay, and into the server settle sink and committed `MessagePosted` event.

### T2 — Bound held delivery by settle sequence

Store each hold's turn sequence; record the settle sequence per session; during drain select only holds at or below the settle boundary. Preserve a later-turn hold through an earlier settle and allow a subsequent settle to fire it. Preserve the legacy path and log it once per session when sequence is absent.

### T3 — Prove the N/N+1 race end to end at the consumer seam

Add a deterministic red→green delivery-consumer regression: hold a turn-N message and a turn-N+1 message before draining N's queued settle; drain N and prove only N fires with the settled blocks; then drain N+1 and prove its message fires once with settled blocks. First run the test against the old fire-everything behavior and record its named failing assertion; implement T2 and rerun to green.

## Tasks

### T1 — Proto and sequence propagation

**Interfaces:** `MessagePosted.turn_sequence: uint64 = 2` in `proto/compass/v1/comms.proto`; `AgentSessionFrame.turn_sequence: uint64 = 4` in `proto/compass/v1/compass.proto`. Agent mapper owns sequence increment/stamping; `CommitAgentPost` preserves the post value; Runner `deliverSession` passes settle value to `SettleSink.OnSessionSettled(sessionID, state, turnSequence uint64)`; delivery receives posted sequence. Session persistence/restoration must retain the counter across same-identity resume.

**Acceptance:** generated Go/TypeScript APIs expose both fields; tests prove sequence increments at successive agent starts, posts carry their active turn, and the matching settle frame carries that turn. A resume/restart fixture preserves the value, while a new session identity starts at 1. Server and Runner round-trip the supplied sequence unchanged.

### T2 — Sequence-bounded holds and legacy compatibility

**Interfaces:** held entry gains `turnSequence uint64`; settle queue event gains `turnSequence uint64`; `OnSessionSettled` accepts the sequence. `MessagePosted.turn_sequence` is read at `onMessagePosted` and supplied to `hold`. Positive settle sequence drains only entries `<= N`; zero uses legacy fire-everything and a per-session one-time log.

**Acceptance:** focused tests prove N settle leaves N+1 held, N+1 settle fires it, repeated edges do not double-fire, and an absent sequence fires all held entries with one fallback log per session. Existing recipient and dedup semantics stay intact.

### T3 — Race regression

**Interfaces:** delivery consumer test seam controls message reads/blocks, hold delivery, and queued settle drain without sleeps. Reuse the consumer fakes and enqueue/drain methods in `go/internal/delivery`.

**Acceptance:** before implementation, the deterministic N/N+1 test fails because N's settle fires N+1's partial blocks; after implementation it passes, demonstrating only N fires at the first drain and N+1 only at its own settle. Record exact failing and passing assertion lines in the implementation PR.
