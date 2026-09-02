# Pre-stream error/abort content surfacing (mapper)

Status: Active

Refs: RIG-2616 (parent RIG-974). Surface: compass-agent event mapper +
`compass.v1` session-trace contract.

## Problem / Intent

An inner-stream failure or a deliberate abort reaches the compass-agent event
mapper as a `message_update` whose inner `AssistantMessageEvent` is
`type: "error"` — carrying the failure text on `inner.error.errorMessage`. The
mapper surfaces the *fact* of the failure but never its *content*: a
`reason: "error"` produces a bare `ERRORED` lifecycle frame
(`packages/compass-agent/src/mapping.ts:309-311`) and a `reason: "aborted"`
produces a counted `UnmappedEvent` stamped `"abort-surfacing staged"`
(`mapping.ts:312-319`) — a deliberately-staged placeholder. Neither carries the
error message, the failure class, or the HTTP status to a subscriber tailing the
session, so a UI session pane cannot show *what* went wrong. This record freezes
the payload shape that surfaces that content as typed session-trace content.

## Global Constraints

- **Additive-only wire.** The `compass.v1.SessionEvent.event` oneof is extended
  by one appended field; the proto comment already guarantees this is
  non-breaking (`proto/compass/v1/compass.proto:448`, "Additions to the `event`
  oneof are non-breaking appends"). No existing `SessionEvent` case, no
  `AgentSessionState` enum value, and no lifecycle contract changes shape.
- **`abort ≠ crash`.** `reason: "aborted"` is a deliberate steer/user cancel, not
  an engine failure. The spec scopes `ERRORED` to an *unexpected* agent exit —
  crash / OOM / engine restart, no auto-reconnect
  (`docs/specs/product/compass.md:390-398`) — and the mapper already documents
  this asymmetry (`mapping.ts:298-308`). An abort MUST NOT ride `ERRORED`.
- **Session surface only.** The streaming conversation write-through to comms was
  removed by RIG-1708 (`mapping.ts:23-26`): assistant text rides the session
  trace, not comms `Message` blocks. Error/abort content surfaces on the session
  trace (`SessionFrame.typed_event`), NOT as a comms `Message` block; do not
  revive a comms write-through.
- **Frame-sink lane split.** The socket sink routes a `session` frame by
  durability (`packages/compass-agent/src/transport/frame-sink.ts:269-272`): a
  lifecycle transition (`state` set) OR a `SessionInjection` trace frame rides
  the never-drop PRIORITY lane (`spine.enqueuePriority`), everything else — every
  other trace frame, `state` UNSPECIFIED — rides the bounded, drop-oldest TRACE
  queue (`spine.enqueueTrace`). The `SessionInjection` carve-out
  (`frame-sink.ts:99-110`, `isInjection`) is the precedent: an observation-
  critical trace frame a busy stream must not drop is lifted to the priority lane
  even though its board state is UNSPECIFIED. Error/abort content is
  observation-critical in exactly this sense (see the routing decision in
  Approach).
- **No consumer depends on the current contentless behavior** (RIG-2616): the
  `ERRORED` lifecycle frame is preserved (board/presence/delivery key off it),
  but the counted-`UnmappedEvent` abort staging carries no contract — it was
  explicitly a placeholder.
- **pi-ai pin.** `packages/compass-agent/package.json` specs `@oh-my-pi/pi-ai:
  ^16.4.8`, which `bun.lock` resolves to `16.5.2`; the
  `AssistantMessageEvent` error variant shape below is cited against that
  resolved `16.5.2` source. The field shape is identical across the `^16.4.8`
  range, so no version-spec change is part of this contract.
- **markdownlint:** dash bullets, blank lines around headings/lists/fences/
  tables, language on every fence, leading+trailing table pipes. MD013 disabled.

## Approach

Add one dedicated `SessionError` message to the `SessionEvent` oneof (next free
field, `10`), discriminated by a `SessionErrorKind` enum (`ERROR` | `ABORTED`),
carrying the failure `message` and an optional HTTP `status`. The mapper's
`case "error":` arm emits it through the existing `#sessionEvent` construction
seam, so it inherits the uniform `event_id`/`at_unix_ms` stamping every trace
frame gets.

### The inner event (pi-ai, pinned)

The failure arrives as a `message_update` whose inner
`AssistantMessageEvent` is the error variant
(`@oh-my-pi/pi-ai/src/types.ts:905-910`):

```ts
| {
      type: "error";
      contentIndex?: undefined;
      reason: Extract<StopReason, "aborted" | "error">;
      error: AssistantMessage;
  };
```

`inner.error` is a full `AssistantMessage` (`types.ts:707-747`); the fields this
contract reads:

- `errorMessage?: string` (`types.ts:727`) — the user-facing failure text.
- `errorStatus?: number` (`types.ts:731`) — HTTP status the provider surfaced.

`inner` is narrowed to the error variant by the `switch (inner.type)` on the
discriminant, so `inner.error.errorMessage` / `inner.error.errorStatus` /
`inner.reason` are SDK-typed reads — not the `any`-typed tool `args`/`result`
that the `isRecord` readers guard, so no runtime narrowing and no inline cast is
involved (`rule://ts-no-inline-cast-access` satisfied).

### The new payload (proto)

Appended to `proto/compass/v1/compass.proto`:

```proto
// A turn-ending failure surfaced as session-trace content: an inner/provider
// stream error, or a deliberate abort (steer/user cancel). `kind` discriminates
// the two — an abort is NOT a crash and does NOT ride the ERRORED lifecycle
// state. `message` is the human-readable failure text (AssistantMessage
// errorMessage); `status` is the provider HTTP status when one was surfaced.
message SessionError {
  SessionErrorKind kind = 1;
  string message = 2;
  optional int32 status = 3;
}

// The class of a SessionError. ERROR pairs with the ERRORED lifecycle
// transition (an unexpected inner/provider failure); ABORTED is a deliberate
// cancel and carries no lifecycle transition.
enum SessionErrorKind {
  SESSION_ERROR_KIND_UNSPECIFIED = 0;
  SESSION_ERROR_KIND_ERROR = 1;
  SESSION_ERROR_KIND_ABORTED = 2;
}
```

and the oneof gains one arm (`compass.proto:454-462`):

```proto
    SessionError session_error = 10;
```

### The emit rule (mapper)

Rewrite the `case "error":` arm (`mapping.ts:297-320`) and add a `#sessionError`
private that routes through `#sessionEvent`:

- `reason === "error"` → emit **two** frames, content first: the
  `SessionError(kind = ERROR)` trace frame, then the existing
  `#sessionState(AgentSessionState.ERRORED)` lifecycle frame. The `ERRORED`
  transition is **preserved** because board projection, presence, and held-
  delivery all key off it (`go/internal/board/projection.go:148-151`,
  `go/internal/presence/presence.go:175-188`,
  `go/internal/delivery/settle.go:75-82`). The content frame is purely additive.
- `reason === "aborted"` → emit **one** frame: the `SessionError(kind = ABORTED)`
  trace frame. **No lifecycle transition** (abort ≠ crash), and the prior counted
  `UnmappedEvent` staging is **removed** — the event is now mapped.

`#sessionError` reads `inner.error.errorMessage ?? ""` for `message` and sets
`status` only when `inner.error.errorStatus !== undefined`.

### Frame-sink routing (never-drop the content)

A `SessionError` is a trace frame (board state UNSPECIFIED), so by the default
split (`frame-sink.ts:269-272`) it would ride the bounded, drop-oldest TRACE
queue — meaning a busy trace backlog could drop the error/abort content, which
is the exact loss RIG-2616 sets out to fix. For `reason = aborted` the content
frame is the ONLY signal (no lifecycle frame accompanies it), so a drop reverts
precisely to today's contentless behavior; for `reason = error` the paired
`ERRORED` lifecycle frame survives (priority lane), but the *content* — the whole
deliverable — could still vanish. **Decision (Matt ruled 2026-09-02): lift
`SessionError` to the never-drop PRIORITY lane**, matching the `SessionInjection`
F3 carve-out precedent (`frame-sink.ts:99-110`): add an `isSessionError`
classifier and extend the priority predicate at `frame-sink.ts:269` to
`isLifecycle(frame) || isInjection(frame) || isSessionError(frame)`. This makes
the surfaced failure content as durable-on-the-spine as the lifecycle transition
it reports.

## Alternatives considered

- **Reuse `SessionNotice` (`compass.proto:513-516`).** No proto change, but a
  notice is a free-standing advisory/status line — conflating a turn failure with
  it erases the render distinction (a subscriber cannot tell an error card from a
  status line) and there is no field on it to carry the `kind` class or the HTTP
  `status`. Rejected: the loss of the failure-vs-advisory distinction is the whole
  point of surfacing the content.
- **Two messages — `SessionError` (error) + `SessionAbort` (aborted).** Two oneof
  appends (fields 10 and 11). Rejected: error and abort carry the *same* payload
  shape (a failure `message` + optional `status`); the only difference is the
  class, which a `kind` enum on one message models exactly. Two messages
  duplicate the field set and spend twice the wire surface for no gain.
- **Surface the partial `AssistantMessage.content[]` blocks.** A pre-stream
  failure by definition produced no content, and a mid-stream failure's blocks
  already streamed as `text_delta`/`thinking_delta` session chunks
  (`mapping.ts:277-296`). Re-emitting them on the error frame would double-render.
  Rejected: `errorMessage` is the surfacing target, not the (already-streamed or
  empty) content blocks.

## Plan

Five right-sized tasks; T2/T3 depend on T1 (the regen must land first so the
`SessionError`/`SessionErrorKind` symbols exist).

### T1 — proto append + regen

- **Interfaces produced:** `compass.v1.SessionError` message, `SessionErrorKind`
  enum, `SessionEvent.session_error = 10` oneof arm.
- Edit `proto/compass/v1/compass.proto`: add the `SessionError` message +
  `SessionErrorKind` enum (beside `SessionNotice`, `compass.proto:511-516`) and
  the `session_error = 10` oneof arm (`compass.proto:454-462`).
- Regen: `moon run proto:gen` (runs the public lane `buf generate`, the agent-ts
  lane `buf generate --template buf.gen.agent-ts.yaml --include-imports`, and the
  internal-go lane). Checked-in outputs updated: `go/gen/compass/v1/compass.pb.go`,
  `packages/compass-client/src/gen/compass/v1/compass_pb.ts`,
  `packages/compass-agent/src/gen/compass/v1/compass_pb.ts` (via `--include-imports`).
- **Acceptance:** `moon run proto:drift` clean (no drift); the three gen trees
  carry `SessionError`/`SessionErrorKind`.

### T2 — mapper error/abort arms + frame-sink routing

One slice: the mapper change and the sink routing ship together — the mapper
alone would emit a droppable content frame, so the never-drop routing is part of
the same contract.

- **Interfaces consumed:** `SessionError`, `SessionErrorSchema`,
  `SessionErrorKind` from `./compassv1`; `inner.error.errorMessage`,
  `inner.error.errorStatus`, `inner.reason` (pi-ai, SDK-typed).
- **Interfaces produced:** `EventMapper.#sessionError(kind, error)` private;
  rewritten `case "error":` arm in `EventMapper.#onMessageUpdate`
  (`packages/compass-agent/src/mapping.ts:297-320`); `isSessionError(frame)`
  classifier + extended priority predicate in
  `packages/compass-agent/src/transport/frame-sink.ts` (add beside `isInjection`
  at `:99-110`; extend the predicate at `:269`).
- `#sessionError` builds `SessionError` (`message: errorMessage ?? ""`, `status`
  set only when `errorStatus !== undefined`) and routes through `#sessionEvent`
  (`mapping.ts:239-247`). The arm returns `[content, ERRORED]` for `error` and
  `[content]` for `aborted`; the `UnmappedEvent` return is deleted. Update the
  arm's comment to state the surfaced-content contract (drop "abort-surfacing
  staged").
- `isSessionError` returns true when `frame.kind === "session"` and
  `frame.value.typedEvent?.event.case === "sessionError"`; the predicate at
  `frame-sink.ts:269` becomes
  `isLifecycle(frame) || isInjection(frame) || isSessionError(frame)`.
- **Acceptance:** `bunx tsc --noEmit` clean; `bunx biome check` clean on both
  files.

### T3 — mapper unit tests

- **Interfaces consumed:** `mapping.test.ts` helpers `upd()`, `typedEvents()`,
  `soleTyped()`, `FIXED_NOW`; `SessionErrorKind` / `SessionError`.
- **Rewrite the existing describe block**, do not merely add fixtures: the block
  `EventMapper — inner error: crash → session ERRORED vs abort → counted
  unmapped` (`packages/compass-agent/src/mapping.test.ts:358-382`) encodes the
  RETIRED contract and reddens under the new emit rule — its `reason "error"`
  test asserts `soleSessionState(...)` (exactly one frame) but the arm now emits
  two, and its `reason "aborted"` test asserts `frame.kind === "unmapped"` but
  the arm now emits a `sessionError` frame. Replace both tests and update the
  block comment (`:359-363`) to the new contract:
  - error-reason `message_update` → two frames: a `sessionError` typed event
    (`kind === SESSION_ERROR_KIND_ERROR`, `message` = the errorMessage) then an
    `ERRORED` lifecycle frame.
  - aborted-reason `message_update` → one `sessionError` typed event
    (`kind === SESSION_ERROR_KIND_ABORTED`), no lifecycle frame, no `UnmappedEvent`.
  - `status` present when `errorStatus` is set; absent when it is not.
- Red-check: reverting the arm to the pre-change behavior (ERRORED-only / counted
  UnmappedEvent) reddens the rewritten tests.
- **Acceptance:** `bun test src/mapping.test.ts` green; red-check confirmed.

### T4 — frame-sink routing test

- **Interfaces consumed:** `packages/compass-agent/src/transport/frame-sink.test.ts`
  harness (the existing `isInjection` priority-lane test at
  `frame-sink.test.ts:697-736` is the pattern — a `sessionInjection` frame lands
  on the priority lane, never the trace lane).
- Add a test mirroring it for a `sessionError` frame: it rides the never-drop
  priority lane and never the drop-oldest trace lane. Cover both `kind`s.
- Red-check: dropping `isSessionError` from the `frame-sink.ts:269` predicate
  reddens it (the frame falls to the trace lane).
- **Acceptance:** `bun test src/transport/frame-sink.test.ts` green; red-checked.

### T5 — ledger delta

- Append to `docs/designs/DECISIONS.md` (current max `DL-317`), under the
  Observability & tracing section (where `DL-303`/`DL-310`
  `SessionInjection`/session-trace rows live):
  - **DL-318** — "Pre-stream/inner error and abort content surface on the session
    trace as a dedicated `SessionError` message (`compass.v1.SessionEvent` oneof
    field 10) with a `SessionErrorKind` discriminator (ERROR | ABORTED), a failure
    `message`, and an optional HTTP `status` — not `SessionNotice`, not a comms
    `Message` block." Status `Active (Matt, 2026-09-02)`, link to this record.
  - **DL-319** — "Emit rule: `reason=error` emits `SessionError(ERROR)` AND the
    existing `ERRORED` lifecycle transition (additive — board/presence/delivery key
    off `ERRORED`); `reason=aborted` emits `SessionError(ABORTED)` with NO lifecycle
    transition (abort ≠ crash) and replaces the prior counted-`UnmappedEvent`
    staging." Status `Active (Matt, 2026-09-02)`, link to this record.
  - **DL-320** — "The `SessionError` trace frame rides the FrameSink never-drop
    PRIORITY lane (not the bounded drop-oldest trace queue), via an
    `isSessionError` classifier extending the `frame-sink.ts` priority predicate
    — matching the `SessionInjection` F3 never-drop carve-out, so surfaced failure
    content is as durable-on-the-spine as the lifecycle transition it reports."
    Status `Active (Matt, 2026-09-02)`, link to this record.
- PR body carries `Ledger-impact:` naming the three appended rows (not `none`).
- **Acceptance:** `tools/design-ledger-gate` passes (record touched ↔ ledger
  touched).

## Tasks

- [ ] T1 — append `SessionError` + `SessionErrorKind` + `session_error = 10` to
  `compass.proto`; regen (`moon run proto:gen`); drift-check clean.
- [ ] T2 — rewrite the mapper `case "error":` arm + add `#sessionError`
  (`mapping.ts`) + `isSessionError` never-drop routing (`frame-sink.ts`); tsc +
  biome clean.
- [ ] T3 — mapper unit-test fixtures for both reasons + status presence
  (`mapping.test.ts`); red-checked.
- [ ] T4 — frame-sink priority-lane routing test for `sessionError`
  (`frame-sink.test.ts`); red-checked.
- [ ] T5 — ledger delta DL-318 + DL-319 + DL-320 (`DECISIONS.md`);
  `Ledger-impact:` line; gate passes.

## Open Questions

- **(RESOLVED — Matt ruled 2026-09-02)** Never-drop lane for `SessionError`:
  ruled **never-drop PRIORITY lane** (folded into Approach → Frame-sink routing,
  and DL-320). The alternative (accept trace-lane drop for a low-frequency
  terminal frame) was rejected because a `reason = aborted` content frame is the
  sole signal and a drop silently reverts to the contentless behavior RIG-2616
  fixes.

- **(Non-load-bearing, deferred)** Surface `AssistantMessage.errorId`
  (`types.ts:733`, a bit-packed machine-readable classifier) as a future
  `SessionError` field. Omitted from the frozen contract: a subscriber cannot
  interpret the bit layout without pi-ai's error-id helpers (`error/flags.ts`;
  the SDK's own `types.ts:732` doc-comment still points at the pre-move
  `utils/error-id.ts`), and the oneof/
  message grows additively if a consumer ever needs it. The design is correct
  without it.
- **(Non-load-bearing, deferred)** Surface `AssistantMessage.stopDetails`
  (`types.ts:726`, provider-specific terminal classification, e.g. an Anthropic
  refusal). Omitted: provider-specific and not needed to render *what* failed;
  additive later if a refusal-specific render is wanted.
