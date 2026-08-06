# Compass v0.8 — Threaded replies + first-party typed session renderer

Status: Historical

## Problem / Intent

Two UI gaps Matt called out after the v0.7 channel workspace merged
(PRs #810/#816/#821/#822):

1. **Replies can't be created.** The threading model is fully built and tested
   — messages carry `parentMessageId`, `threadsOf()` groups them, `ThreadView`
   renders them — but there is no interaction to *create* a reply: the composer
   only posts flat top-level messages and no message carries a reply
   affordance.
2. **The session trace is ugly and opaque.** The agent observation panel
   renders coarse opaque frames (a kind tag + one preformatted line) under the
   frozen v0.6 decision that the trace is OMP-native and rendered by OMP's own
   renderer. Matt has now ruled the opposite: Compass builds a **first-party
   typed session renderer**, with session events crossing a **typed gRPC
   stream** — not opaque bytes, and explicitly **not ACP**.

This record designs both: (1) a Slack/Discord-style **reply panel /
side-thread** for creating replies, and (2) the typed session-event contract +
Compass-side per-kind renderer, superseding the opaque-trace decisions of
v0.6 and v0.7 by citation.

## Approach

### Matt's rulings (verbatim)

On the reply UX:

> "i think discord also uses a reply panel like slack? at least from my quick
> test"

On the session renderer:

> "terminal surface for just the session stream seems ugly. maybe we should
> just build out a renderer, and accept we need to implement all the various
> tool calls, edits, etc? would need to investigate the best way to pass all of
> those events out of the Compass agent though. I'm hesitant about tying
> ourselves to ACP because we're doing a first party agent intentionally to
> have a better integrated experience. can likely do a typed gRPC stream?"

And the trace surface stays observation-only: "just without an input box" —
the Stop control stays, no composer.

### Superseded frozen decisions

Superseded by citation — the frozen records themselves are never rewritten
(sealed `AGENTS.md`).

This record supersedes the *opaque-trace* arm of two frozen records and the
proto shape that encodes it:

- **compass-0.6, round-two fork (e)** — "**the trace is a dedicated OMP-native
  session stream** (`SubscribeAgentSession`), not typed `SubscribeEvents`
  variants" (`docs/designs/product/compass-0.6/design.md:139-143`); "streamed
  from the **dedicated OMP-native session-tail stream** (opaque frames rendered
  by OMP's own renderer …)" (`compass-0.6/design.md:261-264`); and the T5/T7
  plan item "carrying the agent's OMP-native session events as an **opaque**
  envelope (bytes/JSON) … consumed by OMP's own renderer"
  (`compass-0.6/design.md:521-528`). The *dedicated session stream* part
  survives — only the *opaque, OMP-rendered* payload is superseded: the same
  stream now carries typed events Compass renders itself.
- **compass-0.7 D2's opacity premise** — "The raw agent output (OMP-native
  `SessionFrame` stream, opaque to Compass …) is a *companion* you glance at"
  (`docs/designs/product/compass-0.7-channel-workspace/design.md:151-159`);
  "**the OMP session trace** (ephemeral, **opaque**) … Compass does not
  interpret it; it hands frames to OMP's own renderer"
  (`compass-0.7-channel-workspace/design.md:185-191`); and the D2 alternative
  rationale "different data source (opaque OMP frames vs user-openable
  resources)" (`compass-0.7-channel-workspace/design.md:235-239`). D2's
  *panel shape* (fixed minimizable side panel, not a tab/split leaf) is NOT
  superseded — only what renders inside it.
- **The proto shape encoding the opacity** — the internal
  `SessionFrame { bytes event = 1; AgentSessionState state = 2 }`
  (`proto/compass/v1/agent.proto:68-71`), doc'd "under the
  first-party OMP agent the trace is not re-typed by Compass — OMP's own
  renderer inflates it" (`agent.proto:48-53`). The `bytes event` field is
  superseded by a typed event oneof (T-P1 below); the `state` lifecycle arm
  and the AgentFrame conversation variants are untouched.

**Why the v0.6 typed-variant removal doesn't block this.** v0.6 removed the
three typed variants `AgentMessageChunk`/`AgentToolCall`/`AgentPlan` from
`SubscribeEvents` as "neither the native render format nor needed"
(`compass-0.6/design.md:531-535`). That removal was about the **board stream**
— `SubscribeEvents` keeps Compass's own projections (liveness, lifecycle,
board), and it still does under this record. This record reintroduces typed
session events on the **dedicated observation-trace stream** (v0.6 fork (e)'s
own stream), richer than the removed set (thought, tool_call + update, diff,
plan), because Matt now wants Compass-side rendering. The messages still exist
in the tree today (`compass.proto:169-203` defines
`AgentMessageChunk`/`AgentToolCall`/`AgentPlan`/`AgentPlanEntry`) and inform,
but do not constrain, the new event shapes.

### Change 1 — Reply panel / side-thread

**What exists (all tested, frozen v0.6 comms — reused, never forked):**

- The carrier: `Message.parentMessageId?`
  (`apps/ui/src/comms-stub.ts:163-165` — "SEAM (channel-model
  amendment): the message this one replies to, forming a thread; absent for a
  top-level message").
- The grouping: `threadsOf(messages, channelId): Thread[]`
  (`comms.ts:195-219`), over
  `Thread { root: Message; replies: Message[] }` (`comms.ts:153-156`), one
  level deep with orphan + cycle guards; exercised by
  `comms.test.ts:80-221` (ordering, orphan, cross-channel, cycle).
- The rendering: `ThreadView` (`components/ChannelView.tsx:203-228`) renders
  root + replies under `.thread-replies` (`app.css:2956-2959` —
  `margin-left: 20px; … border-left: 2px solid var(--border)`).

**What's missing:** the CREATE interaction. `Composer`
(`ChannelView.tsx:262-291`) posts only flat messages (and is itself a
documented no-op stub — the send button has no onClick); there is no reply
affordance, no reply-target state, and no `postMessage`/`postReply` store
action (`store.ts` has `answerAsk` at `store.ts:316` but no post action).

**Chosen shape (Matt's ruling): a reply panel / side-thread**, Slack/Discord
style. A per-thread "reply" affordance on the thread's ROOT row (the interface
adds only `onOpenThread(rootMessageId)`, matching the one-level-deep model)
opens a dedicated thread panel: the root message, its replies, and a
thread-scoped composer. No inline reply-under-message affordance.

Design decisions:

- **Store state, not component state.** The open thread is cross-component
  state (the stream opens it, the panel closes it), so it lives in `AppStore`
  as `openThreadRootId: Accessor<string | null>` + `openThread(rootMessageId)`
  / `closeThread()`, per the v0.7 Global Constraint "all cross-component state
  lives in the one `AppStore`"
  (`compass-0.7-channel-workspace/design.md:249-251`).
- **The panel renders one `Thread`** — resolved by `threadsOf()` output for
  the current channel, filtered to the open root id. No parallel derivation.
- **Split beside the stream, not overlay** (recommended; OQ-1). The
  conversation `<section class="conversation">` gains a sibling
  `<aside class="thread-panel">` inside a flex row, mirroring how `LogPanel`
  is an `<aside>` companion. An overlay would hide the stream the user is
  replying *from*.
- **Both mounts** (recommended; OQ-1): `ChannelView` hosts the panel, so it
  appears in both the agent-workspace chat pane and the standalone channel
  view — the panel is a function of the channel, not the shell.
- **Composer parity with the main composer** (recommended; OQ-1): the thread
  composer obeys the same rule as the main composer — enabled in the
  single-player workspace DM, and in standalone channels exactly as the main
  composer is (rendered enabled over a documented no-op stub per v0.7's
  "Stop/Send render enabled over documented no-op RPC stubs",
  `compass-0.7-channel-workspace/design.md:267-270`). No composer is read-only
  in a joined standalone channel: text posting was never gated, and asks are
  answerable everywhere with first-responder-wins (the merged ask-in-channel
  record, `compass-ask-in-channel/design.md:271-278`, Matt 2026-07-20, which
  superseded v0.7 D3's read-only-ask deferral). A thread reply is text posting,
  so the thread composer mirrors the main composer's enablement.
- **Posting stays a documented no-op** in the walking skeleton: a
  `postReply(channelId, parentMessageId, text)` store action appends to the
  in-memory fixture state (so the interaction is explorable and testable) and
  is doc'd as the `PostMessage` seam.

### Change 2 — First-party typed session renderer

**The pipe today (grounded end to end).** The transport already exists; only
the payload is opaque:

1. The OMP agent writes newline-delimited protojson `AgentFrame` lines on
   stdout. `AgentFrame` is a
   `oneof frame { conversation_posted | conversation_updated | session }`
   (`agent.proto:40-55`).
2. The Runner relays: "relay reads newline-delimited protojson AgentFrame
   lines off the agent's stdout and sends each up the PublishEvents stream,
   Runner-sequenced" (`go/internal/runner/relay.go:138-141`; decode + send at
   `relay.go:151-162`).
3. RunnerHub classifies by the set oneof field and hands the session variant
   to the tail sink:
   `RelaySessionFrame(sessionID string, frame *compassv1internal.SessionFrame)`
   (`go/internal/runnerhub/helpers_test.go:136-140` shows the sink contract).
4. The public repackaging — v0.6 fork (e)'s
   `rpc SubscribeAgentSession(...) returns (stream AgentSessionFrame)`
   (`compass-0.6/design.md:521-528`) — is designed but **not yet in
   `compass.proto`** (no `SubscribeAgentSession`/`AgentSessionFrame` symbol
   exists in `proto/compass/v1` today; verified by search this
   session). This record defines it typed from the start, so no opaque public
   surface ever ships.

**The gen-fence (SEA-1267) is a hard constraint.** *What it is for:* the compass
proto serves two audiences from one source tree — a **public client SDK** (the
gen trees a downstream consumer imports: `compass-client/src/gen`, `go/gen`) and
the **internal transport envelopes** the Server/Runner/agent speak among
themselves (`packages/compass-agent/src/gen`, `go/internal/gen`). The transport
types (`AgentFrame`/`AgentControl`/`SessionFrame`, `RunnerService`/`RunnerError`)
are implementation detail and MUST NOT leak into the public SDK, or external
consumers couple to the wire framing. `buf.gen.yaml`'s `exclude_paths` keeps them
out; the fence is the *check that the exclusion held*, and it is distinct from
`drift` precisely because a forgotten `exclude_paths` entry leaks silently — once
the leaked symbol is committed, regen reproduces it and `drift` passes green. So
the fence greps the two public gen trees for any internal symbol
(`AgentFrame|AgentControl|SessionFrame|RunnerService|RunnerError|compassv1internal`)
and fails CI on a hit (`proto/moon.yml:111-123`).

The typed event message therefore lives on the **public** side of that line:
`SessionEvent` is defined **once as a public message in `compass.proto`** (it is
the render contract the public client needs), and the internal `SessionFrame` is
retyped to *reference* it. The internal→public import direction already exists
(`agent.proto:24-25`), so this needs zero mirror duplication and the fence stays
green — a public symbol in the internal tree is fine; the fence only rejects
internal symbols in public trees. See T-P1 for the exact proto and OQ-2 for the
ratified ruling.

**The typed event shape — first-party, ACP-informed, not ACP-bound.** The OMP
fork's ACP surface (`oss/forks/oh-my-pi/packages/coding-agent/src/modes/acp/acp-event-mapper.ts`)
is the reference for which kinds exist: `agent_message_chunk` /
`agent_thought_chunk` (`acp-event-mapper.ts:271-293`), `tool_call`
(`:445-447`), `tool_call_update` (`:203-206, 227-231`), `plan` (`:253-256,
388-389`), and `diff` as tool-call content (`:645-673`). Compass defines its
**own** proto for these kinds — no ACP import, no ACP JSON on the wire —
because the first-party agent exists for tighter integration than ACP's
generic surface (Matt's ruling above). OMP's own renderer is a terminal TUI
(`oss/forks/oh-my-pi/packages/tui`, ANSI); there is no reusable web renderer,
which is exactly why Compass builds one.

**UI side.** Today `LogPanel` → `TracePane` → `FrameRow` print the opaque stub:
"prints each opaque frame's preformatted line"
(`components/LogPanel.tsx:18-23`), over
`SessionFrame { id; kind; at; text; link? }` (`session-stub.ts:33-42`, doc'd
"NOT a compass block kind: Compass does not interpret these",
`session-stub.ts:18-21`). This record replaces that stack with:

- A typed fixture module (`session-events-stub.ts`) mirroring the new proto's
  event union.
- Per-kind renderer components (`SessionEventRow` dispatching to
  `AssistantTextEvent` / `ThinkingEvent` / `ToolCallEvent` / `DiffEvent` /
  `PlanEvent` / `NoticeEvent`).
- A rebuilt `TracePane` consuming `store.agentSession()` in its new typed
  shape. The panel stays D2's minimizable observation companion —
  read-only, **no input box**, Stop control stays
  (`LogPanel.tsx:103-113` — "The one non-observational control: stop the
  running turn").

**Cheap win — the block CSS already exists.** `app.css:996-1134` still carries
the rich block styling from the deleted `AcpConversation`: `.block-thinking`
(`:996-1003`), `.block-tool` + `.tool-status[data-status]` (`:1005-1032`),
`.block-plan` + `.plan-step[data-status]` (`:1053-1089`), `.block-diff` +
`.diff-line[data-kind]` (`:1092-1134`). The new renderer components reuse and
adapt these classes; only the components are new.

**Scope of this increment (recommended; OQ-3).** The UI is a no-daemon walking
skeleton, so this record lands in two stackable halves:

- **Now (fixture-backed):** the typed event model in TS, the fixture reshape,
  the renderer components, the `LogPanel` rebuild — fully testable via
  `moon run compass-ui:test`.
- **Stacked (real wiring):** the proto files, gen, Runner/Server plumbing, and
  the public `SubscribeAgentSession` — designed here with exact shapes so the
  executor needs no rediscovery, landed as a stacked PR (or a separate lane if
  Matt prefers; OQ-3).

### Alternatives considered

- **Inline reply affordance (reply composer under each message):** rejected —
  Matt ruled the Slack/Discord reply-panel shape.
- **ACP `SessionUpdate` JSON over the existing `bytes event`:** rejected —
  keeps the opaque wire, ties Compass to ACP's union evolution, and Matt
  explicitly declined ACP binding.
- **Reusing OMP's renderer:** impossible for the web UI — OMP renders via its
  terminal TUI (`packages/tui`); no web renderer exists to embed.
- **Typed events on `SubscribeEvents`:** rejected — re-opens the v0.6 removal
  (`compass-0.6/design.md:531-535`); the board stream stays Compass
  projections only. The dedicated session stream is the right carrier.

## Global Constraints

Every task below inherits these; task briefs do not restate them.

- **Stack: SolidJS + Vite**, UI at `apps/ui/src/`. No new framework
  or state library; all cross-component state lives in the one `AppStore`
  (`store.ts`) read through context (`context.ts`)
  (`compass-0.7-channel-workspace/design.md:249-251`).
- **Walking skeleton, no daemon.** Every surface renders from in-memory
  fixtures; the store accessors are the seam that later swaps to the generated
  `@compass/client` (`compass-0.7-channel-workspace/design.md:252-255`). No
  component reads a fixture module directly when a store accessor exists.
- **The comms model is v0.6's, frozen.** `comms.ts` / `comms-stub.ts` shapes
  are reused, never re-derived or forked; the seam annotations
  (`parentMessageId`, channel-only container) stay.
- **Tests: `moon run compass-ui:test` = `bun test --conditions browser`** —
  the browser condition is load-bearing (Bun's default `node` condition pulls
  solid-js's SSR build; `compass-0.7-channel-workspace/design.md:260-262`).
  Red→green per `rule://red-green-testing`: tests first, watch them fail, then
  implement.
- **Go/proto side: `moon run` tasks** for compass Go + proto gen; the SEA-1267
  gen-fence (`proto/moon.yml:111-123`) MUST stay green — no
  internal symbol (`AgentFrame`/`AgentControl`/`SessionFrame`/
  `compassv1internal`) in a public gen tree
  (`packages/compass-client/src/gen`, `go/gen`).
- **Posting is a documented no-op** until `PostMessage` lands: interaction
  affordances render enabled and mutate fixture state, doc'd as the RPC seam
  (v0.7's "Stop/Send render enabled over documented no-op RPC stubs",
  `compass-0.7-channel-workspace/design.md:267-270`).
- **The trace surface is observation-only**: no input box; the Stop control
  stays (`LogPanel.tsx:103-113`).
- **Lint/format:** biome (TS), markdownlint-clean records, gofmt/golangci (Go).
- **Frozen-record convention:** this record freezes on merge; later changes
  supersede by citation, never rewrite.

## Plan

Three clusters. The threading cluster (T-T*) and the renderer UI cluster
(T-U*) are independent and fixture-backed — they land in this increment. The
proto/transport cluster (T-P*) stacks on the record (design-exact here;
recommended as a stacked PR or separate lane — OQ-3).

### Cluster T — Threaded replies

#### T-T1 — Store: open-thread state + `postReply`

Add the cross-component thread state and the reply action to `AppStore`
(`store.ts`). Red-first: store tests for open/close, root-only invariant, and
`postReply` appending a fixture message with `parentMessageId` set.

- `openThreadRootId` starts `null`; `closeThread()` returns it to `null`.
- `openThread(rootMessageId)` sets it. Callers pass a ROOT id (the affordance
  lives on `ThreadView`, which knows its `thread.root.id`); the store guards
  by resolving through `threadsOf(messages(), channelId)` and no-ops on an
  unknown root.
- Selecting a different channel or agent closes any open thread (the root id
  is channel-scoped state).
- `postReply` appends an in-memory `Message` (id `msg-local-<n>`,
  `authorAccountId` = caller, `atUnixMs` = now, one `text` block,
  `parentMessageId` set), doc'd as the `PostMessage` seam.

Interfaces:

```ts
// store.ts (AppStore additions)
openThreadRootId: Accessor<string | null>;
openThread: (rootMessageId: string) => void;
closeThread: () => void;
postReply: (channelId: string, parentMessageId: string, text: string) => void;
```

Test cycle: `moon run compass-ui:test` — new `store.thread.test.ts(x)` cases
red, then green.

#### T-T2 — `ThreadPanel` component + reply affordance

New `components/ThreadPanel.tsx`: renders the open `Thread` (root via
`MessageRow`, replies via the existing indent pattern), a close control, and a
thread-scoped composer wired to `store.postReply`. `ChannelView`
(`ChannelView.tsx:306-360`) hosts it: the `<section class="conversation">`
gains a flex-row wrapper so the panel splits beside the stream (OQ-1
recommendation); `ThreadView` gets a per-thread "reply" affordance calling
`store.openThread(thread.root.id)`. Reuses `MessageRow`, `threadsOf`,
`.thread-replies` CSS; new `.thread-panel` CSS only.
The flex row wraps the existing `{stream + composer}` column and the new
`ThreadPanel` as siblings; the conversation header spans the full width above
them, and `.conversation { flex: 1 }` (the #827 composer-stick fix,
`app.css:2870-2882`) stays so the composer keeps pinning to the bottom.

Interfaces:

```ts
// components/ThreadPanel.tsx
export const ThreadPanel: Component<{
  channel: Channel;
  byId: Map<string, Account>;
  byHandle: Map<string, Account>;
}>;
// ChannelView.tsx — ThreadView gains:
//   onOpenThread: (rootMessageId: string) => void
```

Test cycle: component render tests (panel opens on affordance click, shows
root + replies, composer posts a reply that appears in-panel AND indented in
the stream, close hides). Red → green via `moon run compass-ui:test`.

### Cluster U — Typed session renderer (fixture-backed)

#### T-U1 — Typed session-event model + fixture reshape

Replace the opaque stub model. `session-stub.ts` (opaque
`SessionFrame { id; kind; at; text; link? }`, `session-stub.ts:33-42`) is
superseded by a typed module `session-events.ts` (pure types + fold helper) +
`session-events-stub.ts` (fixtures). The TS union mirrors the proto shape in
T-P1, so the later client swap is **mechanical via a thin mapper at the store
seam** (`store.agentSession()`, `store.ts:580-583`) — not type-identity
substitution: the generated `@compass/client` (protobuf-es) represents
`int64 at_unix_ms` as `bigint` and `AgentToolCallStatus`/`AgentPlanEntryStatus`
as numeric proto enums (e.g. `helpers_test.go:222`
`compassv1.AgentSessionState_AGENT_SESSION_STATE_UNSPECIFIED`), which the mapper
narrows to `atUnixMs: number` and the string-literal unions below.
`store.agentSession()` re-points to the new fixtures with the new shape;
`AgentSession.running` semantics unchanged.

Interfaces:

```ts
// session-events.ts
export type ToolCallStatus = "pending" | "in_progress" | "completed" | "failed";
export type PlanEntryStatus = "pending" | "in_progress" | "completed";
export interface PlanEntry { content: string; status: PlanEntryStatus; }
export interface FileDiff { path: string; oldText: string | null; newText: string; }
export type SessionEvent = { id: string; atUnixMs: number } & (
  | { kind: "assistant_text"; messageId: string; text: string }
  | { kind: "thinking"; messageId: string; text: string }
  | { kind: "tool_call"; toolCallId: string; title: string; status: ToolCallStatus }
  | { kind: "tool_call_update"; toolCallId: string; status: ToolCallStatus;
      output?: string; diffs?: FileDiff[] }
  | { kind: "plan"; entries: PlanEntry[] }
  | { kind: "notice"; text: string; link?: string }
);
export interface AgentSession {
  agentAccountId: string;
  running: boolean;
  events: SessionEvent[];
}
/** Fold updates into their tool_call by toolCallId; latest plan wins a
 *  coalesced trailing position; coalesce consecutive `assistant_text`/`thinking`
 *  sharing a `messageId` into one rendered block carrying the concatenated text
 *  (ratified, OQ-2.1); notice passes through in order. An orphan
 *  `tool_call_update` (no preceding `tool_call` for its `toolCallId`) becomes
 *  its own tool item keyed by `toolCallId` with no `call`. */
export function foldSession(events: readonly SessionEvent[]): TraceItem[];
export type TraceItem =
  | { kind: "text" | "thinking"; messageId: string; text: string }
  | { kind: "notice"; event: SessionEvent }
  | { kind: "tool"; toolCallId: string; call?: Extract<SessionEvent, { kind: "tool_call" }>;
      status: ToolCallStatus; output?: string; diffs?: FileDiff[] }
  | { kind: "plan"; entries: PlanEntry[] };
```

Test cycle: pure unit tests on `foldSession` (update folds into call by
`toolCallId`; orphan update becomes its own tool item with no `call`; latest
plan wins; ordering stable; consecutive `assistant_text`/`thinking` sharing a
`messageId` coalesce into one item carrying the concatenated text, differing
`messageId`s stay separate, and an interleaved `tool_call` does not merge across
two same-`messageId` deltas) — red → green. A whole-turn fixture (many one-word
deltas sharing a `messageId`) in `session-events-stub.ts` exercises the
streaming shape, not just whole-string fixtures.

#### T-U2 — Per-kind renderer components + `TracePane` rebuild

Replace `FrameRow` (`LogPanel.tsx:23-44`) and rebuild `TracePane`
(`LogPanel.tsx:48-72`) over `foldSession(store.agentSession().events)`. New
`components/SessionTrace.tsx` with one component per `TraceItem` kind, reusing
the existing block CSS (`app.css:996-1134`): `.block-thinking`, `.block-tool`
with `.tool-status[data-status]`, `.block-plan` and `.plan-step[data-status]`,
`.block-diff` + `.diff-line[data-kind]`. `LogPanel`'s shell (header, running
dot, Stop, minimize — `LogPanel.tsx:80-132`) is unchanged. Delete
`session-stub.ts` and the `FRAME_TAG` map once nothing imports them.

**Resolve the `tool-status` value-space mismatch.** The existing
`.tool-status[data-status="running"|"ok"|"error"]` selectors
(`app.css:1024-1031`) key a DIFFERENT vocabulary than the model's
`ToolCallStatus` (`pending|in_progress|completed|failed`) — emitting
`data-status="in_progress"` matches no selector, so the dot renders unstyled
while a presence-only test still passes. (`.plan-step[data-status]` already
aligns: `in_progress`/`completed`, `app.css:1079-1084`.) Migrate the four
tool-status selectors to the proto value space
(`pending`/`in_progress`/`completed`/`failed`) so one value vocabulary runs end
to end.

Interfaces:

```ts
// components/SessionTrace.tsx
export const SessionTrace: Component<{ items: TraceItem[] }>;
// internal: TextItemRow, ThinkingRow, ToolRow (status dot + title + output
//   disclosure + DiffBlock list; title falls back to `toolCallId` when `call`
//   is absent — an orphan update), PlanBlock, NoticeRow
const DiffBlock: Component<{ diff: FileDiff }>; // .block-diff/.diff-line render
```

Test cycle: component render tests per kind (thinking renders italic-dim
block; tool row carries a `data-status` whose value is in the migrated proto
space and matches a styled selector — asserting the value, not mere attribute
presence, so red-green catches the vocabulary gap; diff renders add/del lines;
plan steps carry `data-status`; no input box present in the panel) — red →
green.

### Cluster P — Typed proto + transport (stacked / real wiring)

#### T-P1 — Typed `SessionEvent` proto + `SessionFrame` retyping

Define `SessionEvent` ONCE as a **public** message in `compass.proto` (it is
the render contract the UI client needs), and retype the internal
`SessionFrame` to carry it. This works within the gen-fence with zero mirror
duplication because the import direction is already internal→public:
`agent.proto` imports `compass/v1/compass.proto` today (`agent.proto:24-25`)
for `AgentSessionState`. The fence (`moon.yml:123`) only greps for internal
symbols in public trees; a public symbol in the internal gen tree is fine.

Interfaces (exact proto — OQ-2 asks Matt to ratify):

```proto
// compass.proto (public) — the typed observation-trace event.
message SessionEvent {
  string event_id = 1;
  int64 at_unix_ms = 2;
  oneof event {
    SessionAssistantText assistant_text = 3;
    SessionThinking thinking = 4;
    SessionToolCall tool_call = 5;
    SessionToolCallUpdate tool_call_update = 6;
    SessionPlan plan = 7;
    SessionNotice notice = 8;
  }
}
message SessionAssistantText { string text = 1; string message_id = 2; }
message SessionThinking { string text = 1; string message_id = 2; }
message SessionToolCall {
  string tool_call_id = 1;
  string title = 2;
  AgentToolCallStatus status = 3; // reuse compass.proto:190-196
}
message SessionToolCallUpdate {
  string tool_call_id = 1;
  AgentToolCallStatus status = 2;
  string output = 3;
  repeated SessionFileDiff diffs = 4;
}
message SessionFileDiff {
  string path = 1;
  optional string old_text = 2; // absent = file creation
  string new_text = 3;
}
message SessionPlan { repeated AgentPlanEntry entries = 1; } // compass.proto:206-209
message SessionNotice { string text = 1; optional string link = 2; }

// agent.proto (internal) — SessionFrame retyped:
message SessionFrame {
  reserved 1; reserved "event"; // was: bytes event
  AgentSessionState state = 2;
  SessionEvent typed_event = 3;
}
```

Notes: reuses the surviving `AgentToolCallStatus` (`compass.proto:190-196`)
and `AgentPlanEntry` (`compass.proto:206-209`) rather than minting parallel
enums. The `bytes event` removal is buf-breaking on an INTERNAL message —
permitted under the same rationale as v0.6's removal ("the Server on `main`
is ephemeral (no live client)", `compass-0.6/design.md:534-535`). For the
`SessionEvent` family the fence stays green as-is: none of the six fence
patterns (`AgentFrame|AgentControl|SessionFrame|RunnerService|RunnerError|compassv1internal`,
`moon.yml:123`) matches `SessionEvent` or its `Session*` sub-messages, and it
is defined public (emitted only into the public gen trees). T-P2's
`AgentSessionFrame` is the collision case — see T-P2 and OQ-2.4.

Test cycle: `moon run compass-proto:lint compass-proto:breaking(--against
exception noted in PR) compass-proto:drift compass-proto:gen-fence` green;
regen both lanes.

#### T-P2 — Public `SubscribeAgentSession` stream

Land v0.6 fork (e)'s session-tail RPC (designed at
`compass-0.6/design.md:521-528`, not yet in the tree — no
`SubscribeAgentSession` symbol exists in `proto/compass/v1` today)
directly in its typed form.

Interfaces:

```proto
// compass.proto (public)
rpc SubscribeAgentSession(SubscribeAgentSessionRequest)
    returns (stream AgentSessionFrame);
message SubscribeAgentSessionRequest { string session_id = 1; }
message AgentSessionFrame {
  string session_id = 1;
  SessionEvent event = 2;              // unset for a pure lifecycle frame
  AgentSessionState state = 3;         // UNSPECIFIED = no transition
}
```

**Fence collision — T-P2 MUST amend the fence regex.** The public name
`AgentSessionFrame` (frozen in v0.6, `compass-0.6/design.md:521-528`) contains
`SessionFrame` as a substring, and the SEA-1267 fence greps unanchored
(`grep -rlE "…|SessionFrame|…"`, `moon.yml:123`). So the moment T-P2 generates
`AgentSessionFrame` into `packages/compass-client/src/gen`, the fence matches
the substring and fails with a false SEA-1267 violation — the stacked PR trips
its own CI. T-P2 MUST amend the fence to word-bounded / per-symbol matching
(e.g. `\bSessionFrame\b`) in the same change, validated against the GENERATED
tree (TS gen can emit `SessionFrame` inside larger identifiers), not the proto
alone. Ratified under OQ-2.4.
Access control per v0.6: scoped to the caller's channel membership
(`compass-0.6/design.md:266-267` — "Access is a projection of the agent's
home-channel membership"). The handler MUST resolve `session_id` to its owning
agent's home channel FIRST, then authorize the caller's membership against THAT
channel — never a bare session-existence or any-membership check, or a caller
could stream another channel's session (its thinking, tool output, file diffs)
by holding a foreign `session_id`; an unknown `session_id` is rejected, not
leaked.

**Ownership record (the resolution needs one — it does not exist today).** No
`session_id → agent` mapping is persisted anywhere in the model, and the start
request cannot supply one directly: `StartAgentSessionRequest` carries only
`container_name` + `initial_prompt` and the Server mints `session_id` in the
*response* (`compass.proto:254-266`) — no `agent_account_id` on it. The agent
identity is known one hop earlier: `ProvisionAgentWorkspaceRequest` carries
`agent_account_id` and returns the `container_name`
(`compass.proto:221-250`), and `AgentAccount` carries `home_channel_id`
(`comms.proto:141`). So the resolution chain is
`session_id → container_name → agent_account_id → home_channel_id`, built in
two hops: the Server persists `container_name → agent_account_id` at
**ProvisionAgentWorkspace** (where the agent id is known) and binds
`session_id → container_name` at **StartAgentSession** (where both are known,
response-side). At subscribe it walks the chain to the home channel and checks
the caller's membership. Each mapping is a **durable store row** (Matt,
2026-07-21), surviving Server restart — not an in-memory-only registry. The
contract requirement (record ownership at provision + start, resolve-then-check
at subscribe, reject unknown `session_id`) is fixed here; the durable-store
schema + placement is the T-P\* owner's to implement.
Test cycle: proto ci + Server-side stream tests (subscribe → relayed
typed frame arrives; non-member on the resolved home channel denied; unknown
`session_id` denied).

#### T-P3 — Runner/Server plumbing for the typed frame

The Runner relay is already payload-agnostic — it decodes whole `AgentFrame`
lines and forwards them (`relay.go:142-167`), so it needs only regen. The
RunnerHub tail sink keeps its signature
(`RelaySessionFrame(sessionID string, frame *compassv1internal.SessionFrame)`,
`helpers_test.go:136-140`); the Server-side repackaging maps internal
`SessionFrame.typed_event`/`state` → public `AgentSessionFrame` field-for-field.
The OMP fork's compass mode gains the emitter: map its native session events
(the same union `acp-event-mapper.ts` maps to ACP) to `SessionEvent` protojson
on stdout — a sibling mapper, not an ACP dependency.

**Go test migration (mechanical, no design change).** `reserved 1` deletes
`SessionFrame.Event`, so the Go sites that construct or assert it break and
migrate to the typed field in the same change: `relay_test.go:44`
(`SessionFrame{Event: []byte(event)}`) and its `GetSession().GetEvent()`
assertions at `relay_test.go:80, 114-116, 121-122, 148, 221`;
`runnerhub/hub_test.go:76` (`frames[0].frame.GetEvent()`); and
`runnerhub/helpers_test.go:219-223` (`sessionTraceFrame` builds
`SessionFrame{Event: …}`). This mirrors v0.6's own removal precedent, which
enumerated its one affected fixture in the same change
(`compass-0.6/design.md:534-541`).

**Wire-compat note.** After the retype, an old agent binary still emitting
protojson with an `"event"` field fails `protojson.Unmarshal` as an unknown
field and is skipped-with-log (`relay.go:152-156`) — acceptable under the
ephemeral-Server rationale (agent + Runner ship together), but previously
unstated.

**Dumb emitter.** The OMP-fork emitter sets `message_id` per streamed message
(OQ-2.1, ratified) and does NOT itself buffer or coalesce — coalescing is
`foldSession`'s job.

Interfaces:

```go
// go/internal/<server>/sessiontail: internal→public repackaging
func toPublicFrame(sessionID string, f *compassv1internal.SessionFrame) *compassv1.AgentSessionFrame
```

```ts
// forks/oh-my-pi packages/coding-agent compass mode (sibling of acp-event-mapper.ts)
export function mapAgentSessionEventToCompassSessionEvents(
  event: AgentSessionEvent, sessionId: string): SessionEvent[];
```

Test cycle: Go unit tests on the repackaging (event-only, state-only, both;
unknown oneof logged not dropped) + a fork-side mapper test mirroring the
acp-event-mapper suite. gofmt/golangci green.

## Tasks

This increment (fixture-backed, land as a stack of small PRs or one PR with
red→green commits per task):

- [ ] T-T1 — Store: `openThreadRootId` / `openThread` / `closeThread` /
  `postReply` + red-first store tests.
- [ ] T-T2 — `ThreadPanel` component, reply affordance on `ThreadView`,
  `ChannelView` split hosting, `.thread-panel` CSS + render tests.
- [ ] T-U1 — `session-events.ts` typed model + `foldSession` + fixture
  reshape (`session-events-stub.ts`), re-point `store.agentSession()`; unit
  tests on the fold.
- [ ] T-U2 — `SessionTrace` per-kind renderers reusing `app.css` block CSS,
  `TracePane` rebuild, delete `session-stub.ts`/`FrameRow`; render tests
  (incl. the no-input-box invariant).

Stacked real wiring (pending OQ-3):

- [ ] T-P1 — Public `SessionEvent` proto family in `compass.proto`; internal
  `SessionFrame` retyped (`reserved 1`); regen both lanes; proto ci +
  gen-fence green.
- [ ] T-P2 — `SubscribeAgentSession` RPC + `AgentSessionFrame`; Server stream
  and membership-scoped access tests.
- [ ] T-P3 — Runner regen, Server internal→public repackaging, OMP-fork
  compass-mode `SessionEvent` emitter; Go + fork mapper tests.

## Open Questions

Batched for Matt (this record's author is headless; franklin relayed in one
`ask`). Matt ruled all three (2026-07-21); each OQ records its **Ruling**
below. The design above already assumed the recommendations, so the rulings
confirm it — except OQ-3's ownership split (T-P\* is a separate lane/owner).

### OQ-1 — Thread panel placement + mounts + composer rule

1. **Split beside the stream vs overlay over it?** Recommendation: **split**
   (a right-side panel inside the conversation section, like Slack's thread
   pane) — an overlay hides the stream the user is replying from, and the
   workspace already establishes the aside-companion pattern (`LogPanel`).
   In the agent workspace, where `LogPanel` already occupies the right edge,
   the thread panel splits *within* the chat pane (inside
   `.conversation`), so the two asides don't fight.
2. **Both mounts or workspace-only?** Recommendation: **both** — the panel is
   hosted by `ChannelView`, which serves both the workspace chat pane and the
   standalone channel view (`ChannelView.tsx:293-305`), so threading works
   wherever conversations render at no extra cost.
3. **Is the thread composer read-only in standalone multiplayer channels?**
   Recommendation: **no** — nothing gates a composer in a joined standalone
   channel. Asks are answerable everywhere with first-responder-wins (the merged
   ask-in-channel record, `compass-ask-in-channel/design.md:271-278`, Matt
   2026-07-20, which superseded v0.7 D3's read-only-ask deferral), and text
   posting was never gated; the main composer already renders enabled in joined
   standalone channels (`ChannelView.tsx:262-291` disables only on
   `membership === "none"`). A thread reply is text posting, so the thread
   composer mirrors the main composer's enablement exactly.

**Ruling (Matt, 2026-07-21):** accept all three — split beside the stream (not
overlay), both mounts, thread composer enabled (mirrors the main composer).

### OQ-2 — Typed session-event proto shape (LOAD-BEARING)

The exact variant set and fence placement in T-P1/T-P2 need ratification
before an executor touches the proto:

1. **Variant set:** `assistant_text · thinking · tool_call ·
   tool_call_update (output + diffs) · plan · notice` — ACP-informed
   (`acp-event-mapper.ts` variants) but first-party. Diffs ride
   `tool_call_update` (matching how OMP produces them,
   `acp-event-mapper.ts:645-673`) rather than as a top-level event.
   Recommendation: ratify this set; additions are non-breaking oneof appends.
   - **Streaming-delta correlation (missing) — new fork.**
     `assistant_text`/`thinking` model whole strings, but at the real source
     they are STREAMING DELTAS: `acp-event-mapper.ts:283-285` emits one
     `agent_message_chunk` per `text_delta`, `:293-294` one
     `agent_thought_chunk` per thinking delta, and ACP correlates them via a
     `messageId` (`:280`, `:328-331`). The proposed `SessionEvent` has an
     `event_id` but no message/turn correlation and no append-vs-replace
     semantic, so one turn would paint hundreds of one-word rows once T-P3 maps
     the real union (the whole-text fixtures hide this). Recommendation: add
     `string message_id` to `SessionAssistantText`/`SessionThinking` with
     documented append-per-`message_id` semantics; `foldSession` coalesces by it
     (cheap, additive, mirrors what ACP already proved necessary, keeps the
     emitter dumb — T-P3). Rejected alternatives: emitter-side per-turn
     buffering (changes emitter latency/semantics), or `foldSession` coalescing
     consecutive same-kind runs (fragile across interleaved tool calls).
2. **Fence split:** define `SessionEvent` as a PUBLIC message in
   `compass.proto` and have the internal `SessionFrame` reference it
   (internal→public import already exists: `agent.proto:24-25`), instead of
   defining it internally and mirroring it publicly. Recommendation: the
   public-definition approach — zero duplication, and for the `SessionEvent`
   family the fence stays green as-is (no fence pattern matches `SessionEvent`
   or its `Session*` sub-messages; `moon.yml:123` greps only for internal
   symbols in public trees). The `AgentSessionFrame` substring collision is a
   SEPARATE fork — see OQ-2.4.
3. **`SessionFrame.bytes event` removal:** retype in place (`reserved 1`),
   buf-breaking on an internal-only message, same ephemeral-Server rationale
   as v0.6 (`compass-0.6/design.md:534-535`). Recommendation: remove now —
   never ship a dual opaque+typed wire.
4. **Fence-name collision on `AgentSessionFrame` (T-P2) — new fork.** The
   v0.6-frozen public name `AgentSessionFrame` (`compass-0.6/design.md:521-528`)
   contains `SessionFrame` as a substring, so the unanchored fence grep
   (`moon.yml:123`) false-fires the moment T-P2 generates it into the public
   tree. Recommendation: amend the fence regex to word-bounded / per-symbol
   matching (`\bSessionFrame\b`, validated against the generated tree) in the
   same change and KEEP the frozen `AgentSessionFrame` name. Rejected
   alternative: rename the public message — it would supersede a frozen v0.6
   decision for no benefit, and every plausible name that keeps
   "Session"+"Frame" collides the same way.
5. **Session-ownership resolution for `SubscribeAgentSession` (T-P2) —
   security, surfaced post-ruling by review.** The access check ("scoped to the
   caller's home-channel membership", `compass-0.6/design.md:266-267`) needs to
   resolve `session_id → agent → home_channel`, but no such mapping is persisted
   today and the start request cannot supply one: `StartAgentSessionRequest` is
   `container_name` + `initial_prompt` with `session_id` minted in the response
   (`compass.proto:254-266`); `agent_account_id` is on
   `ProvisionAgentWorkspaceRequest` (which returns `container_name`,
   `compass.proto:221-250`); `AgentAccount.home_channel_id` at `comms.proto:141`;
   no session registry in `go/internal/**`. Without a persisted chain a naive
   handler checks only session-existence and a caller streams another channel's
   session by holding a foreign `session_id`. Recommendation: persist
   `container_name → agent_account_id` at ProvisionAgentWorkspace and
   `session_id → container_name` at StartAgentSession, resolve the full chain to
   the home channel and check membership at subscribe (reject unknown
   `session_id`); the contract requirement is fixed in T-P2. Store-choice
   sub-fork (Matt ruled): a **durable store row** surviving Server restart, over
   an in-memory-only registry — the T-P\* owner implements the schema +
   placement.

**Ruling (Matt, 2026-07-21):** accept sub-decisions 1–4 — the variant set;
`message_id` + append-per-`message_id` correlation on
`SessionAssistantText`/`SessionThinking` with `foldSession` coalescing;
public-definition fence split; `reserved 1` in-place removal of the bytes event;
and the word-bounded fence-regex amendment keeping the frozen `AgentSessionFrame`
name. Sub-decisions 1–4 are the ratified proto contract the stacked T-P\* PR
builds to. **Sub-item 5 (session-ownership resolution) was surfaced by review
after this ruling and Matt ruled it (2026-07-21): the contract requirement is
applied in T-P2 and the ownership mapping(s) are durable store rows, implemented
by the T-P\* owner.**

### OQ-3 — Scope of this increment vs deferred real wiring

The UI is a no-daemon walking skeleton. How much lands now?
Recommendation: **T-T1/T-T2/T-U1/T-U2 land now** (fixture-backed, fully
testable, deliver both visible changes); **T-P1–T-P3 are frozen here as the
contract but land as a stacked PR** once OQ-2 is ratified — either the next
PR in this stack or a separate lane if another agent owns the Go side. The TS
event model (T-U1) mirrors the proto shape, so the swap is mechanical via the
thin `store.agentSession()` mapper (T-U1) either way.

**Ruling (Matt, 2026-07-21):** accept the split — **T-T1/T-T2/T-U1/T-U2 land now**
(this lane, franklin), **T-P1–T-P3 are a separate stacked lane owned by another
agent**, not franklin. The UI lane implements against this frozen record; the
T-P\* owner builds to the ratified OQ-2 contract. franklin coordinates the
contract handoff (the T-U1 TS event model ↔ T-P1 proto shape) but does not
implement T-P\*.
