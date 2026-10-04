# Design: Order held delivery by turn sequence (RIG-4033)

Ledger-impact: appends DL-382 for Matt's option-3 ruling (design-ledger-gate)

> Post-freeze note (annotate, don't rewrite): `Message.turn_sequence` is field 8, because field 6 is `author_handle` and field 7 was the removed `parent_message_id`. The agent→Runner carrier is the internal `SessionFrame` (`turn_sequence = 3`), so the agent stamps that frame, not the public `AgentSessionFrame`; `toPublicFrame` copies it across.

## Problem / Intent

The delivery consumer holds agent-authored messages while the author's session is live, then fires held messages on `OnSessionSettled`. Because the settle edge is queued and drained asynchronously, a turn N+1 post can be held before turn N's edge drains; firing it with partial blocks and deduping by `message_id` can discard its later settled delivery. This record implements Matt's RIG-4033 option 3: order holds and settle edges by the agent-reported per-session turn sequence, not wall time.

## Approach

The agent owns a monotonically increasing `uint64` turn sequence for the lifetime of a session identity. A session-lifetime counter in compass-agent increments on each `agent_start`; the comms tool path (`packages/compass-agent/src/comms.ts`) stamps its current value on `PostMessageRequest`, and `EventMapper` (`packages/compass-agent/src/mapping.ts`) stamps it on the lifecycle `AgentSessionFrame` that settles the turn. Values must remain <= `math.MaxInt64` for the Postgres BIGINT carrier. The Runner relays both values unchanged; the server passes the frame value through `Hub.deliverSession` and `SettleSink.OnSessionSettled` to delivery. The consumer fires only holds whose sequence is <= the settle sequence. A hold made after its own settle edge was observed is immediately eligible through the existing settle/recheck path, but never by comparing timestamps.

Session identity scopes the sequence. A session resume that preserves the session identity must preserve and continue its counter; a new session identity starts at 1. Runner process restart and agent reconnect do not reset it when resuming the same session. If the agent cannot recover the counter for an existing session, it must start a new session identity rather than reuse a sequence. This prevents ambiguous ordering without server persistence or wall-clock assumptions.

### Proto and compatibility

- `proto/compass/v1/comms.proto`: add `uint64 turn_sequence = 7` to `PostMessageRequest` (fields 1–6 remain unchanged), `uint64 turn_sequence = 2` to `MessagePosted` (field 1 remains `Message message`), and `uint64 turn_sequence = 6` to `Message` (fields 1–5 remain unchanged). Agent tools construct the request in `packages/compass-agent/src/comms.ts`; production calls travel in `CommsCallRequest.post` (`proto/compass/v1/agent_gateway.proto`) through `Hub.executeCall` (`go/internal/runnerhub/relay_comms.go`) to `PostAsAccountByName` and `PostMessage` (`go/internal/comms/agent_caller.go`, `comms.go`). Persist the value with the message: add non-null `turn_sequence BIGINT DEFAULT 0` to `messages`, constrained nonnegative and <= signed BIGINT maximum. `PostMessage` writes it; `MessageByID` returns it to the fabric re-read; `MessagePosted` wraps that wire `Message`. The durable field is required because the cutover re-reads Postgres by `EventRef.RowID`. Human/API posts use zero.
- `proto/compass/v1/compass.proto`: add `uint64 turn_sequence = 4` to `AgentSessionFrame` (fields 1–3 remain `session_id`, `event`, `state`). `EventMapper` (`packages/compass-agent/src/mapping.ts`) stamps it on the `agent_end` lifecycle frame; non-settle trace frames need not carry it. Runner passes it to the server's settle sink.
- Zero/missing sequence from an older peer follows the pre-decided compatibility rule: retain today's fire-everything settle behavior. Log that fallback once per session, not once per message or edge. Do not infer a turn from `at_unix_ms`.
- Roles: the **agent** creates, persists/restores, increments and stamps both values; the **Runner** transports values and preserves them over reconnect; the **server** validates/consumes values, orders holds and bounds each settle fire. No server-side counter assignment.

The existing production seams establish these owners: `AgentSessionFrame` is currently `session_id/event/state` (`proto/compass/v1/compass.proto`); `Hub.deliverSession` relays the frame then calls `SettleSink.OnSessionSettled` (`go/internal/runnerhub/hub.go`); `delivery.Consumer.OnSessionSettled` queues a settle with `upTo: math.MaxInt64` (`go/internal/delivery/settle.go`); `onMessagePosted` calls `hold` for a live author (`go/internal/delivery/dispatch.go`). Agent tools construct their post request in `packages/compass-agent/src/comms.ts`; calls arrive through `Hub.executeCall` (`go/internal/runnerhub/relay_comms.go`) and use `PostAsAccountByName` → `PostMessage` (`go/internal/comms/agent_caller.go`, `comms.go`); `publishMessagePosted` currently rebuilds the event from the stored row (`go/internal/comms/mapping.go`).

## Alternatives considered

- **Bind holds to settle wall time (option 1).** Rejected by Matt in favor of an explicit turn identity. Millisecond event and enqueue times describe when observations reach components, not which turn authored a message; delayed frames can cross the settle boundary.
- **Server- or Runner-assigned turn counter.** Rejected: only the agent observes `agent_start`, which defines turn boundaries. Assigning at relay/receipt would make delivery order depend on transport scheduling and reconnect gaps.

## Global Constraints

- Per-session turn sequence is unsigned 64-bit; zero means absent/legacy, positive values are agent-reported turns. Never derive it from timestamps or message ids.
- A numbered settle at N fires only held entries with sequence <= N. A later turn's hold remains held, even when its post races ahead of N's queued drain.
- Missing/zero sequence retains legacy fire-everything behavior and emits one fallback log per session.
- Reconnect/resume of the same session preserves its sequence; a new session identity starts at 1. A peer unable to restore sequence must not resume under the old identity.
- The `messages.turn_sequence BIGINT NOT NULL DEFAULT 0` column arrives in a new append-only migration file (never an edit to an applied one) and lands before server code reads or writes the column. Existing rows and old peers are zero and retain fire-everything semantics. Roll out the schema first, then server and agent/Runner support; rollback remains safe because zero is legacy-compatible.
- Protobuf field additions are additive. Generated Go and TypeScript bindings are regenerated from proto; no hand edits to generated output.
- Keep message-id dedup and existing recipient re-resolution, settle eligibility states, durable delivery cursor, and non-terminal DISCONNECTED semantics unchanged.
- The delivery code being designed against is the cutover at bookmark `compass-managed/rig-3107-t6-rulings`; `main` does not yet include that code.

## Rollout

- **Agent:** compass-agent owns the session-lifetime counter, mapper, comms-tool stamping, and persistence/restoration of the counter.
- **Runner:** compass-runner owns relay; it transports both sequence values unchanged.
- **Server:** compass-managed owns the delivery consumer and settle handling.
- T1's schema and carrier propagation must land before T2's bounded consumer logic can have effect. A mixed-version fleet stays on the legacy fire-everything path when either sequence field is absent.

## Plan

### T1 — Define and propagate the sequence fields

Add fields to `PostMessageRequest`, `MessagePosted`, and `AgentSessionFrame`; persist the post sequence in the message row and thread it through the agent call, Runner relay, and server delivery event/re-read and settle sink.

### T2 — Bound held delivery by settle sequence

Store each hold's turn sequence; record the settle sequence per session; during drain select only holds at or below the settle boundary. Preserve a later-turn hold through an earlier settle and allow a subsequent settle to fire it. Preserve the legacy path and log it once per session when sequence is absent.

### T3 — Prove the N/N+1 race end to end at the consumer seam

Add a deterministic red→green delivery-consumer regression: hold a turn-N message and a turn-N+1 message before draining N's queued settle; drain N and prove only N fires with the settled blocks; then drain N+1 and prove its message fires once with settled blocks. First run the test against the old fire-everything behavior and record its named failing assertion; implement T2 and rerun to green.

## Tasks

### T1 — Proto and sequence propagation

**Interfaces:** `PostMessageRequest.turn_sequence: uint64 = 7`, `MessagePosted.turn_sequence: uint64 = 2`, and `Message.turn_sequence: uint64 = 6` in `proto/compass/v1/comms.proto`; `AgentSessionFrame.turn_sequence: uint64 = 4` in `proto/compass/v1/compass.proto`; `messages.turn_sequence BIGINT NOT NULL DEFAULT 0` with a nonnegative check. Agent session counter increments at start and agent comms tools stamp it; Runner relays `CommsCallRequest.post`; server `PostMessage` stores it, and `MessageByID` supplies it on the returned `Message`. Sequence values must be <= `math.MaxInt64`. Session persistence/restoration retains the counter across same-identity resume.

**Acceptance:** generated Go/TypeScript APIs expose all four proto fields; migration tests prove existing rows default to zero and new writes round-trip values through `PostMessage` and `MessageByID`; agent tests prove successive turn stamps and resume restoration; Runner tests prove unchanged relay; and the matching settle frame carries the sequence. A new session identity starts at 1.

### T2 — Sequence-bounded holds and legacy compatibility

**Interfaces:** held entry gains `turnSequence uint64`; settle queue event gains `turnSequence uint64`; `OnSessionSettled` accepts the sequence. `onMessagePosted(ctx, msg *compassv1.Message)` remains unchanged: it reads `turn_sequence` from the `Message` carried by `MessagePosted` after Postgres re-read. Positive settle sequence drains only entries `<= N`; zero uses legacy fire-everything and a per-session one-time log.

**Acceptance:** focused tests prove N settle leaves N+1 held, N+1 settle fires it, repeated edges do not double-fire, and an absent sequence fires all held entries with one fallback log per session. Existing recipient and dedup semantics stay intact.

### T3 — Race regression

**Interfaces:** delivery consumer test seam controls message reads/blocks, hold delivery, and queued settle drain without sleeps. Reuse the consumer fakes and enqueue/drain methods in `go/internal/delivery`.

**Acceptance:** before implementation, the deterministic N/N+1 test fails because N's settle fires N+1's partial blocks; after implementation it passes, demonstrating only N fires at the first drain and N+1 only at its own settle. Record exact failing and passing assertion lines in the implementation PR.
