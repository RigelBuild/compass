# Compass ask round-trip over the comms transport (RIG-1509 redesign)

Status: Draft

Tracking: RIG-1509 (redesign after PR #390 closed as wrong transport). Entangled: RIG-1310 (ask correlation key, co-owned by compass-agent).

## Problem / Intent

**The Compass agent has NO promptable session.** There is no composer and no
input box: the session log is operator-visible for **monitoring + stop only** —
watching what the agent does moment to moment, and interrupting it. There is
**no reply path through the session**. ALL operator↔agent communication flows
through comms **channels + threads** (the agent's home channel + topics). This
is the deliberate, distinct break from the traditional agentic session loop.

An `ask` is therefore a **typed comms message** posted onto a channel/topic and
answered **async**: the agent posts its questions and continues; the operator
answers whenever they get to it; the answer returns to the agent as a typed
control op on a later turn. This record wires the agent to that already-frozen
transport — the agent-side **raise** (a Compass tool emitting
`PostMessage(MessageBlock{ask})`) and the agent-side **answer-consume**
(`AskAnswerControl` over the control lane). PR #390, which wired RIG-1509
through the SDK session-UI `askDialog` seam, was closed as wrong transport;
this record replaces it.

## Approach

The transport is **already frozen in proto** — this record invents nothing on
the wire; it wires the agent to both ends of it.

### The raise lane (agent → comms)

An ask is a message-block variant, not a new frame. `MessageBlock`
(`proto/compass/v1/comms.proto:343-350`):

> ```proto
> message MessageBlock {
>   oneof block {
>     // Settled user-facing / assistant text (markdown).
>     string text = 1;
>     // A structured question awaiting an answer.
>     Ask ask = 2;
>   }
> }
> ```

`Ask` carries a server-minted correlation id and the question set
(`comms.proto:361-368`):

> ```proto
> message Ask {
>   // Correlation id echoed by RespondToAsk; server-assigned and globally
>   // unique, resolved within the caller's authorized channels. One id per Ask
>   // (per native ask tool call), NOT per question — questions are keyed inside
>   // the Ask by AskQuestion.question_id.
>   string ask_id = 1;
>   // The questions, in the order the agent asked them. At least one.
>   repeated AskQuestion questions = 2;
> ```

and `answered` is server-owned (`comms.proto:376-378`: "Server-owned: set only
by the server, on RespondToAsk. A value supplied on an inbound Ask (via
representable: `AskQuestion` (`comms.proto:383-402`) carries `question_id`,
`question`, `header`, `repeated AskOption options`, `allow_multiple`,
`optional int32 recommended`; `AskOption` (`comms.proto:419-427`) carries
`id`, `label`, `description`, `preview`. The SDK schema it mirrors
(`~/.bun/install/cache/@oh-my-pi/pi-coding-agent@17.3.1@@@1/src/tools/ask.ts:57-80`)
is `QuestionItem { id, question, header?, options[], multi?, recommended? }`,
`OptionItem { label, description?, preview? }` — a 1:1 field map with one
derived field: native `OptionItem` has **no id**, but `AskOption.id` is
required (it is the referent `AskQuestionAnswer.chosen_option_ids` echoes back,
`comms.proto:837`), so the mapper **mints it** as the option's zero-based index
rendered as a decimal string (T2) — the option's position in `options[]` is the
stable key, exactly as the superseded derivation table specified before DL-213
moved the raise onto `comms_post_ask`.

The async contract is stated on the proto itself (`comms.proto:358-360`):

> "The ask is a normal async channel message; the agent chooses at the turn
> level whether to wait for the answer, which arrives via RespondToAsk."

The raise rides the live `PostMessage` RPC (`comms.proto:85`:
`rpc PostMessage(PostMessageRequest) returns (PostMessageResponse);`). In
`PostMessageRequest` (`comms.proto:771-794`) an unset `container` oneof routes
to the home channel — the server fills it
(`go/internal/comms/agent_caller.go:355-358`: "defaultChannel returns
req with an empty channel_id filled from the account's home channel"), resolved
from `AgentAccount.home_channel_id` (`comms.proto:172-175`: "The agent's home
channel, minted at CreateAgent. The agent is always subscribed to it … Server-set").
The **topic**, by contrast, is **not** server-defaulted: the store hard-rejects
an unset topic (`go/internal/comms/store/messages.go:36-37`:
`if (topic.ID == "") == (topic.Name == "") { return … "exactly one of topic id or name is required" }`),
and the comms package says so in its own words
(`agent_caller.go:45-46`: "The landed store has no home-topic default, so an
append must name a topic"). The proto's "unset posts to the channel's home
topic" comment (`comms.proto:783-786`) is stale/aspirational — every in-repo
caller that wants home-topic behavior names a topic explicitly
(`CommitAgentPost` sets `TopicName "general"`, `agent_caller.go:233`). So
`comms_post_ask` names a topic (a `topic` parameter, `topic_name` default
`"general"`); the topic is get-or-created inside the append transaction
(`store/messages.go:14`: "a TopicRef.Name is get-or-created on
(channel_id, lower(name))"), so no separate CreateTopic step is needed.

**The tool surface**: per Matt's ruling, the SDK `ask` tool is NOT overridden
(it awaits a dialog result synchronously; the Compass flow is async, and the
ask format may be extended later). Instead the agent raises an ask through a
new dedicated native tool, **`comms_post_ask`** (Decision 1; a
`comms_post_message` ask-variant was weighed and rejected — see Alternatives).
It follows the
existing tool conventions in `packages/compass-agent/src/comms.ts` (arktype
parameters, `CommsBroker.call` with a `CommsCallRequest{post}`, home-channel
default via `case: undefined`, idempotency key via `broker.idempotencyKey`).
The tool is **non-blocking**: it posts `MessageBlock{ask}` with all
questions/options, returns the server-minted `ask_id` + a "the answer will
arrive on a later turn" note to the model, and the turn continues. The
structural guard stays intact — the agent can never *answer* an ask
(`comms.ts:37-41`: "NEVER AN ASK-ANSWERING TOOL. … The prohibition is
structural rather than a convention to uphold — the request oneof cannot
express RespondToAsk").

### The answer lane (human → server → agent)

Already shipped server-side; this record consumes it, it does not rebuild it.
`RespondToAsk` (`comms.proto:98`) takes `RespondToAskRequest { ask_id,
answers[] }` (`comms.proto:825-831`); the server records the answer once
(`comms.proto:369-371`: "It flips exactly once, on the first RespondToAsk, and
a second one is rejected") and wakes the asking agent
(`go/internal/comms/comms.go:387-405`: `store.AnswerAsk(…)` then
`c.askWaker.WakeAskAnswer(ctx, msg.AuthorAccountID, req.Msg.GetAskId(),
req.Msg.GetAnswers())`, best-effort and nil-safe).

The answer reaches the agent as the frozen 6th `AgentControl` variant
(`proto/compass/v1/agent.proto:164`: `AskAnswerControl ask_answer = 4;`),
shaped (`agent.proto:180-191`):

> ```proto
> message AskAnswerControl {
>   // Correlates to the in-flight ask (the ask_id the agent emitted).
>   string ask_id = 1;
>   …
>   repeated AskQuestionAnswer answers = 2;
> }
> ```

Agent-side, the decode already exists:
`packages/compass-agent/src/transport/control-source.ts:346-353` decodes the
wire op into the domain union (`op: { kind: "askAnswer", askId: v.askId,
answers: v.answers }`), typed at `control.ts:47-51`. The **apply arm is
parked**: `agent.ts:519-529` surfaces it as a counted unmapped op —
`"ask_answer delivery staged — awaiting SEA-1310 ask correlation key"` — never
dropped. This record's consume task un-parks that arm: on a post-barrier
`askAnswer`, format the answers into a prompt injection delivered on the
agent's next turn, following the established turn-end-delivery pattern the
deliver arm already uses (`agent.ts:83-85`: "A delivered channel message is
coalesced to a turn-end prompt: mid-turn delivers queue and flush as ONE prompt
when the turn settles; an idle deliver starts a turn at once"). Correlation is
by `ask_id`: the raise tool records the pending `ask_id`s it minted (in-memory,
container-scoped) so the apply arm can render the answer against the questions
the model asked — the exact correlation contract is the RIG-1310-entangled
piece, co-ratified as the server-minted `ask_id` (Decision 3).

**The answer push is single-shot today, keyed on the session id — the recovery
model (Matt-ruled) re-keys it on the agent handle.** The wake fires inline at
`RespondToAsk` (`comms.go:404`) to whatever session is bound at that instant,
and there is no durable redelivery behind it:

- If the agent is offline (or its session binding was dropped) at answer time,
  `WakeAskAnswer` is a silent no-op (`go/internal/runnerhub/ask_waker.go:34-36`:
  `sessionID, ok := h.SessionForAccount(agent); if !ok { return }` — "no live
  session to wake"); a Runner reconnect drops all bindings
  (`hub.go:305-309`).
- The "normal delivery path" the interface comment leans on does **not**
  redeliver it. `RespondToAsk` emits a `MessageUpdated` (`comms.go:396`), and a
  `MessageUpdated` is explicitly **not** a delivery trigger
  (`delivery/consumer.go:274-277`). The ask message is **agent-authored**, and
  delivery excludes the author (`delivery/dispatch.go:15`), so neither the ask
  nor its answered update fans out to the asking agent through the
  message-delivery lane.
- Runner-side control retention does not survive a restart: retention is
  per-session (`runner/gateway/control.go:119-125`), a resumed container is
  promoted onto a **new** session id (`runnerhub/resume_start.go:41`), and
  retired sessions drop retained ops (`control.go:252-253`).

**The ruling (Matt).** The session-id keying is a wart: an owed answer belongs
to the **agent handle** (account), not to a session id that a relaunch throws
away. So (a) the owed answer is keyed on the agent account and delivered to
whatever session is next live for that handle — a delivery-layer fix in the
Go runner/hub, external to the `packages/compass-agent` package this record
owns; and (b) when the operator answers an ask whose agent is **not currently
live**, recovery is a **human decision at answer time** — relaunch that agent
(it then receives the owed answer on its next live session) or route the answer
to a new agent. There is deliberately **no agent-side boot poll** of the
channel: the agent does not go looking for stale answers; the delivery layer
owes the answer to the handle, and a human decides liveness. This record's
agent half therefore consumes `AskAnswerControl` by `ask_id` on whatever session
is live and correlates purely on `ask_id` (a handle-scoped registry, not
session-scoped); the durable owed-to-handle delivery + the session-id→handle
re-key is a **filed runner/hub dependency** (T7, per Decision 3), not agent-side
scope. The `PendingAsks` registry stays in-memory within a live session; an
`ask_id` with no live registry entry is surfaced-not-dropped (the handle-keyed
delivery layer, once landed, is what makes the answer survive a relaunch — not
an agent poll).

### End-to-end shape

```mermaid
sequenceDiagram
    participant M as Model (OMP turn)
    participant T as comms_post_ask tool
    participant S as Server (comms)
    participant O as Operator (channel UI)
    participant C as Control lane
    M->>T: questions[] (SDK-ask-shaped)
    T->>S: PostMessage(MessageBlock{ask})
    S-->>T: Message{ask_id}
    T-->>M: "Ask a-1 posted; answer arrives on a later turn"
    Note over M: turn continues / ends — never blocks
    O->>S: RespondToAsk(ask_id, answers)
    S->>C: WakeAskAnswer → AskAnswerControl{ask_id, answers}
    C->>M: answer injected as prompt on a later turn
```

### The AgentFrame reconciliation (DL-043)

The outbound `AgentFrame` oneof carries only `session` / `replay_complete_ack`
/ `control_ack` / `delivery_ack` / `transcript_entry` (`agent.proto:46-85`) —
there is **no Ask or conversation-block variant**; fields 1-2
(`conversation_posted`/`conversation_updated`) are "REMOVED, not reserved (F9):
the streaming conversation write-through is gone" (`agent.proto:47-49`). So the
path `compass-ask-typed-derivation.md` (DL-043) specified — deriving an
outbound Ask conversation frame from the OMP `ask` tool-call via the
`mapping.ts` `#deriveAsk` helper — is **dead**: SEA-1708 removed agent
conversation-frame
production, and `mapping.ts` has no ask arm today (grep for `deriveAsk`/`ask`
in `packages/compass-agent/src/mapping.ts` matches only todo-plan extraction
code, `mapping.ts:429-450`). PR #390 drew the wrong conclusion from this
deadness (→ use the session-UI seam); the right conclusion is that ask-raising
moves onto the **live comms `PostMessage` lane**, a separate transport from
AgentFrame.

**Ruling this record makes**: DL-043's derivation **mechanism** (the
`#deriveAsk` → AgentFrame conversation-frame path) is **superseded** by this
record; DL-043's `Ask = repeated AskQuestion` **shape** stays **live** — it is
exactly the shape frozen in `comms.proto:361-427` that both lanes of this
record ride. The ledger delta (below) records this as a partial supersede.

### Non-goals

- **The UI answering surface** — owned by `compass-ask-in-channel/design.md`
  (DL-037/first-responder-wins; Status: Active). That is the ANSWERING lane;
  this record is the RAISE + CONSUME lane.
- **Any session-UI mechanism** — askDialog / `hasUI` / `PI_NO_PTY`. That was
  PR #390; rejected (see Alternatives).
- **Answering an ask from the agent** — structurally impossible and kept so
  (DL-028; `comms.ts:37-41`).
- **The respond-side server/store work** — `RespondToAsk` (`comms.go:387-405`),
  `store.AnswerAsk`, and `askWaker.WakeAskAnswer` already exist in the tree;
  consumed here, not rebuilt. The wake has no durable redelivery today; the
  owed-to-handle fix is a filed runner/hub dependency (T7, per Decision 3).

## Alternatives considered

### Rejected: the SDK session-UI `askDialog` seam (= PR #390)

PR #390 satisfied RIG-1509 by giving the agent a session UI context whose
`askDialog` resolves the native `ask` tool's awaited promise. Rejected because
it presumes a promptable session: the Compass session log is **observe + stop
only** — there is no composer, no dialog, no reply path through the session.
An `askDialog` answer would have to arrive through a surface that structurally
does not accept input. It also makes the ask **turn-blocking** (the native tool
awaits a single result covering every question), where the frozen comms
contract is explicitly async (`comms.proto:358-360`). The session-UI seam is
the wrong transport, not a smaller version of the right one.

### Rejected: overriding the SDK `ask` tool to post to comms

Ruled out by Matt: the SDK ask tool is not set up for the async flow (its
contract is await-one-result), and the ask format may be extended later —
extension is cleaner on a Compass-owned tool than inside an overridden SDK
seam. DL-139's role delta already tells the agent "async comms / no `ask`",
so a Compass-native surface is consistent with frozen intent.

### Weighed: three tool surfaces for raising an ask

- **A `questions?` field alongside `text` in `comms_post_message` (additive,
  not a mode-fork)**: the proto `Message` carries `repeated MessageBlock`
  (`comms.proto:340,349`), so one post can legally mix a `text` block and an
  `ask` block — an additive optional `questions` field on the existing post
  tool is expressible and, being an arktype union member rather than a
  `.narrow` predicate, **is** visible as an `anyOf` in the JSON Schema. It even
  matches how a human posts "context + question" as one message. Rejected
  anyway: it couples the async answer-arrives-later contract onto a tool whose
  description and single-line result text are about plain posting, and grows
  that tool every time the ask format is extended (Matt's stated expectation).
- **A `text`-XOR-`questions[]` mode-fork inside `comms_post_message`**: keeps
  DL-028's "two native tools" count nominally intact, but forces one tool to
  carry two unrelated contracts in a single description. Rejected: same
  coupling as above without even the mixed-blocks ergonomics.
- **Dedicated `comms_post_ask` tool (RECOMMENDED)**: one tool, one contract,
  its own description carrying the async semantics ("you will not receive the
  answer this turn"), its own schema mirroring the SDK QuestionItem shape 1:1,
  and clean extension room for the anticipated ask-format growth. The "extra
  tool" cost against DL-028 is smaller than it looks: the shipped toolset is
  already **four** tools (`comms.ts:43`: "Four tools ship: post, list, roster,
  and set_status; search is deferred"), so DL-028's "two tools" count line is
  already stale in code and owes a ledger refresh regardless. What DL-028
  load-bears — **no ask-answering capability** — is untouched by all three.
  (Note: the schema-invisibility hazard of `.narrow` predicates,
  `comms.ts:112-118`, is a real constraint on *field-level* rules but does not
  by itself decide this fork — a union serializes visibly; the deciding factor
  is contract-per-description + extension room, not schema visibility.)

Recommendation, ruled by Decision 1: **dedicated `comms_post_ask`**.

## Plan

### Global Constraints

- **No proto changes.** The ask/answer transport is frozen:
  `MessageBlock.ask` (`comms.proto:343-350`), `Ask`/`AskQuestion`/`AskOption`
  (`comms.proto:361-427`), `PostMessage` (`comms.proto:85`), `RespondToAsk`
  (`comms.proto:98`), `AskAnswerControl` (`agent.proto:164,180-191`). This
  record wires to them.
- **Async, non-blocking end to end.** The raise tool returns after the post;
  the answer arrives as a control op on a subsequent turn. Never a Promise the
  turn awaits, never a session-UI dialog.
- **Apply-then-ack is at-least-once; a drop must throw, not return.** The
  control source acks an op only when the consumer returns for the next one
  (`control-source.ts:16-18`), so an arm that returns normally is acked and
  retired from Runner retention (`control.go:318`) — permanently. An
  `askAnswer` the agent cannot yet apply (pre-`ReplayComplete`) MUST throw so
  the Runner redelivers it (`control-source.ts:568-570`); it must never be a
  counted-return "refusal", which silently drops it. (`HoldForReplay` is not a
  fallback — it has no production caller, `control.go:429-431`.)
- **The structural no-answer guard stays.** The `CommsCallRequest` oneof must
  never grow a RespondToAsk arm (`comms.ts:37-41`, DL-028).
- **Server-owned fields are never set client-side**: `ask_id`, `answered`, and
  the AskQuestion answer fields are server-minted/ignored-on-inbound
  (`comms.proto:362-366,376-378,403-406`).
- **Follow existing comms-tool conventions** (`comms.ts`): arktype parameters
  with `.narrow` + description-carried constraints, `CommsBroker` delegation,
  home-channel via `container: { case: undefined }`, idempotency via
  `broker.idempotencyKey(toolCallId)`, single-text-block tool results,
  `attr()` for server-value interpolation.
- **Red → Green** (rule://red-green-testing): each task lands its failing
  tests first; the existing staged-arm pins in `agent.test.ts:341-397` are the
  RED baseline the consume task flips.
- **Ledger coupling**: this is a product record — the same PR carries the
  `docs/designs/product/DECISIONS.md` delta (new row for this record; DL-043
  partial-supersede flip; DL-028 count refresh) and a `Ledger-impact:` PR
  line. The driver performs the ledger edit + PR; T5 states the exact delta.
- **Naming**: the tool name is `comms_post_ask` (prefix-consistent with the
  shipped `comms_*` family), settled by Decision 1.

### T1 — RED: raise-tool contract tests

Failing tests in `packages/compass-agent/src/comms.test.ts` pinning the
`comms_post_ask` contract before it exists: (a) a well-formed multi-question
call produces one `CommsCallRequest{post}` whose single block is
`{ case: "ask" }` with N `AskQuestion`s mapping every SDK axis (id, question,
header, options{label,description,preview}, multi→allow_multiple,
recommended); (b) the container oneof is `case: undefined` when `channel_id`
is omitted (home-channel default); (c) `clientRequestId` is broker-scoped;
(d) the tool result names the server-minted `ask_id` and states the answer
arrives on a later turn; (e) duplicate/empty question ids are rejected at the
parameter gate (mirroring the enforcement note on `comms.proto:383-389`);
(f) the tool never blocks (returns after the post resolves).

Interfaces:

- Consumes: `postAskParameters` (new arktype export, shape below), the
  `CommsBroker` fake pattern already used by `comms.test.ts`.
- Produces: RED tests only; no src change.

### T2 — GREEN: `comms_post_ask` raise tool

Implement the tool in `packages/compass-agent/src/comms.ts` beside the four
shipped tools, turning T1 green. Non-blocking post of
`PostMessage(MessageBlock{ask})`; result text:
`Posted ask <ask_id> to topic <topic_id>. The operator's answer will arrive in a later turn — do not wait for it; continue.`
Extract the server-minted `ask_id` from the returned `Message.blocks[0].ask`.
Register the pending ask with the correlation registry (T3's consumer): the
tool hands `{ askId, questions }` to a `PendingAsks` recorder passed into
redden: `comms.test.ts:273` ("exposes exactly the four comms tools and never
an ask-answering one" — becomes five, still never an ask-answering one) and
the three `cli.test.ts` count pins — `:1842` (`natives` 6 → 7), `:1892` and
`:1924` (`customTools` 6 → 7).

Interfaces:

- Consumes: `CommsBroker.call(CommsCallRequest{ case: "post" })`;
  `PostMessageRequestSchema` / `MessageBlockSchema` / `AskSchema` /
  `AskQuestionSchema` / `AskOptionSchema` from `gen/compass/v1/comms_pb`.
- Produces:
  - `export const postAskParameters = type({ questions: …, "topic?": …, "channel_id?": … })`
    where each question is
    `{ id: string, question: string, "header?": string, options: {label: string, "description?": string, "preview?": string}[], "multi?": boolean, "recommended?": number }`
    (1:1 with SDK `ask.ts:57-69`).
  - `createCommsTools(broker: CommsBroker, pendingAsks?: PendingAsks): AgentTool[]`
    (signature widened; existing callers in `cli.ts` updated).
  - `export interface PendingAsks { record(askId: string, questions: AskQuestion[]): void; take(askId: string): AskQuestion[] | undefined }`.
  - The block builder that turns each parsed `postAskParameters` question into
    an `AskQuestion`: it **mints `AskOption.id`** as the option's zero-based
    index rendered as a decimal string (native `OptionItem` carries no id,
    `ask.ts:57-61`; the position is the stable key the returning
    `chosen_option_ids` echoes, `comms.proto:837`). T1 pins this: a
    multi-option question round-trips option ids `"0"`, `"1"`, … in
    `options[]` order.
- Docs: top-of-module contract line added to the tool's doc comment (see T6).

### T3 — RED: answer-consume tests

Flip the staged-arm pins: extend
`packages/compass-agent/src/agent.test.ts:341-397` ("CompassAgent —
ask_answer is staged, never delivered to the SDK (SEA-1310)") so the
post-barrier case asserts DELIVERY instead of staging: a post-`replayComplete`
`askAnswer` whose `askId` is registered in `PendingAsks` is formatted and
delivered to the SDK as a prompt (idle → starts a turn; mid-turn → coalesces
to the turn-end flush, reusing the deliver-arm machinery pinned at
`agent.test.ts:682+`). **Change the pre-barrier arm, do not keep it as a
counted-unmapped refusal**: refusal is apply-then-ack (`control-source.ts:16-18`:
"the consumer returning for the next op is proof the previous one applied"), so
a refused pre-barrier `askAnswer` returns normally, gets acked, and the Runner
retires it from retention (`control.go:318`) — a permanent drop, not a hold.
The `HoldForReplay` seam that would otherwise buffer it has **no production
caller** (`control.go:429-431`: "no production caller exists yet … exercised
only by tests"), so the barrier is not a safety net here. Pin instead that a
pre-barrier `askAnswer` **throws** (an apply that throws is not acked —
`control-source.ts:568-570`: "an apply that failed must not be acked — the
Runner redelivers the op to the next session"), converting the drop into an
at-least-once redelivery post-barrier. Add: an `askAnswer` with an UNKNOWN
`askId` (no registry entry — e.g. a container-restart wiped the in-memory
registry) surfaces a counted unmapped op (never a fabricated prompt), and a
duplicate redelivered `askAnswer` is not double-injected.

Interfaces:

- Consumes: `AskQuestionAnswerSchema`, the `runWith`/`startDeliverAgent`
  harnesses already in `agent.test.ts`.
- Produces: RED tests replacing the "staged" post-barrier pin.

### T4 — GREEN: wire the askAnswer apply arm (RIG-1310-entangled)

Replace the staged arm at `agent.ts:506-530`: on a post-barrier `askAnswer`,
look up `pendingAsks.take(askId)`; render the answers against the recorded
questions (question text + chosen option labels + `custom_text`, one section
per question, following `formatDeliversForPrompt`'s pure-exported-formatter
pattern, `agent.ts:586-592`); enqueue through the same turn-end coalescing
path deliver uses. A **pre-barrier** `askAnswer` **throws** (not acked → Runner
redelivers post-barrier, per T3). A post-barrier `askAnswer` with an unknown
`askId` → counted unmapped op (reason names the missing correlation). Update
the stale doc comments in `control.ts:41-46` and `control-source.ts:13-16` that
describe the arm as staged.

**Correlation contract (co-ratified with RIG-1310)**: the key is the
server-minted `ask_id`, echoed verbatim from `Ask.ask_id`
(`comms.proto:362-366`) through `RespondToAskRequest.ask_id`
(`comms.proto:825-826`) to `AskAnswerControl.ask_id` (`agent.proto:181-182`:
"Correlates to the in-flight ask (the ask_id the agent emitted)"). The
agent-side registry is in-memory and container-scoped. **The cross-restart
recovery gap is ruled by Decision 3, not left open**: an `AskAnswerControl`
that lands while the agent is offline — or after a restart mints a new session
id — is not redelivered by any lane today, so the answer's injection is lost
even though the answer is durably recorded on the ask block. Per Decision 3 the
durable fix is the owed-to-handle delivery re-key in the runner/hub answer lane,
a filed dependency external to this package (T7); there is **no agent-side boot
re-hydration sweep**. The unknown-`askId` arm is the *within-session* safety net
(surface, never fabricate) and is a permanent contract, not a transitional one;
cross-restart answer survival is what T7 delivers.

Interfaces:

- Consumes: `AgentControl` union arm `{ kind: "askAnswer", askId, answers }`
  (`control.ts:47-51`); `PendingAsks` (T2); the `#turnActive`/turn-end flush
  machinery (`agent.ts:83-85,380-383`).
- Produces:
  - `export function formatAskAnswerForPrompt(questions: readonly AskQuestion[], answers: readonly AskQuestionAnswer[]): string`
    (pure, exported, unit-tested like `formatDeliversForPrompt`).
  - The live `#applyControl` askAnswer arm; T3 goes GREEN.

### T5 — Ledger delta (shipped in this PR)

This is a product record, so its `docs/designs/product/DECISIONS.md` delta ships
in the **same PR** (the `design-ledger-gate` touch-coupling check enforces it).
The status enum is binary (`Active` / `Superseded by DL-<n>`), and both affected
rows keep a **live half**, so the partial supersede is encoded the house way —
new citing rows, old rows stay `Active` (the DL-183 / DL-147 refine-by-citation
pattern) — not a blanket `Superseded` flip that would falsely kill the live half:

- **DL-211** (new, this record): the agent ask round-trip rides the comms
  transport — raise via `comms_post_ask` → `PostMessage(MessageBlock{ask})` into
  a topic, consume via `AskAnswerControl` keyed on `ask_id`; session observe+stop
  only; the owed answer keys on the agent handle (Decision 3).
- **DL-212** (new): the comms native toolset is five tools; refreshes DL-028's
  stale "two tools" count. **DL-028 stays `Active`** — its load-bearing
  no-ask-answering clause is still current truth, carried forward verbatim.
- **DL-213** (new): the outbound ask realization moves off the dead
  `#deriveAsk` → AgentFrame path onto the live comms `PostMessage` lane. **DL-043
  stays `Active`** — its `Ask = repeated AskQuestion` SHAPE is still live (both
  lanes ride it); only the derivation MECHANISM is dead, recorded by citation.
- **`compass-ask-typed-derivation.md` `Status:` stays `Active`** — record-level
  Status is lifecycle, per-decision truth lives only in the ledger, and that
  record still holds a live decision (DL-043's shape). The mechanism death is a
  ledger citation (DL-213), not a record-level supersede.
- **DL-139** corroborates ("async comms / no `ask`") — cited, no flip.
- **Citation sweep**: `grep` for `DL-043`/`DL-028` in code found no `per DL-043`
  code citations and only still-valid DL-028 references (its no-answering
  clause) — no sweep edits owed.

Interfaces:

- Consumes: `DECISIONS.md` "Comms & tools" + "Ask contract" sections.
- Produces: DL-211/DL-212/DL-213 rows, same PR as this record; `Ledger-impact:`
  PR line naming them.

### T6 — Invariant documentation

Plaster the "no promptable session" contract as a first-class documented
invariant, on exactly these surfaces:

- **This record's headline** (done — Problem/Intent opening line).
- **NEW `packages/compass-agent/AGENTS.md`** (neither AGENTS.md nor README
  exists in the package today — glob confirms only `tsconfig.json`,
  `package.json`, `moon.yml`, `src/`, `scripts/`): a short package contract
  whose first section states the invariant — no promptable session; session
  log = operator observe+stop only; all operator↔agent comms via channels +
  topics; asks are raised via `comms_post_ask` and answered async via
  `AskAnswerControl`; the agent never answers an ask.
- **Top-of-module doc of the raise tool's file** (`comms.ts` file header):
  extend the existing "NEVER AN ASK-ANSWERING TOOL" block (`comms.ts:37-41`)
  with the raise-side contract: an ask is an async channel message, never a
  session dialog; the answer arrives over the control lane on a later turn.
- **Cross-check, not duplicate**: the agent role-delta (DL-139,
  `config/agents/*`) already carries "async comms / no `ask`" — cite it from
  AGENTS.md rather than restating it.
- **The temporary answer-liveness limit** (Decision 3): AGENTS.md states, as an
  operator-visible contract, that until the runner/hub owed-to-handle delivery
  lands (T7), an answer delivered while the asking agent is not live is missed
  and the operator relaunches that agent (or routes to a new one) to retry —
  never a silent drop.

Interfaces:

- Consumes: `comms.ts:37-43` header block; DL-139 row (`DECISIONS.md:155`).
- Produces: `packages/compass-agent/AGENTS.md` (new); amended `comms.ts`
  header; amended raise-tool doc comment.

### T7 — Filed dependency: owed-to-handle answer delivery (runner/hub, driver-filed)

**External to this package; the driver files it, this record depends on it**
(Decision 3). The runner/hub answer lane keys the wake on the session id bound
at `RespondToAsk` time (`runnerhub/ask_waker.go:34-36` no-ops when no live
session; retention is per-session, `runner/gateway/control.go:119-125`; a
relaunch mints a new session id, `runnerhub/resume_start.go:41`). The fix: an
owed `AskAnswerControl` is keyed on the **agent account/handle** and delivered
to whatever session is next live for that handle, surviving a relaunch. This is
Go runner/hub work, entangled with RIG-1310; the driver files it as its own
issue (Owner: whoever owns runnerhub; compass-agent co-ratifies the correlation
key = `ask_id`). This record's agent half (T2–T4) is correct and shippable
against the current lane — it just misses an answer delivered to a dead session
until T7 lands (the T6-documented temporary limit). No code in this package.

## Tasks

- [ ] T1 — RED: `comms_post_ask` contract tests in `comms.test.ts` (block
  shape, SDK-axis 1:1 map, home-channel default, idempotency key, ask_id in
  result, duplicate/empty question-id rejection, non-blocking)
- [ ] T2 — GREEN: `comms_post_ask` tool + `PendingAsks` registry +
  `createCommsTools` signature widening + `cli.ts` call-site update
- [ ] T3 — RED: answer-consume tests — flip the post-barrier staged pin
  (`agent.test.ts:341-397`) to assert delivery; pre-barrier throws (redelivers,
  not dropped); unknown-askId surfaced; no double-injection
- [ ] T4 — GREEN: live `#applyControl` askAnswer arm +
  `formatAskAnswerForPrompt` + pre-barrier throw + stale-doc-comment sweep
  (`control.ts:41-46`, `control-source.ts:13-16`) — RIG-1310 correlation
  contract co-ratified
- [ ] T5 — Ledger delta (driver): new row, DL-043 partial-supersede flip +
  citation sweep, DL-028 count refresh, DL-139 corroboration cite;
  `Ledger-impact:` PR line
- [ ] T6 — Invariant documentation: new `packages/compass-agent/AGENTS.md`,
  `comms.ts` header + raise-tool doc-comment amendments
- [ ] T7 — Filed dependency (driver): runner/hub owed-to-handle answer delivery
  (key the wake on agent handle, not session id) — a runner/hub issue entangled
  with RIG-1310; no code in this package

## Open Questions

None. All three load-bearing forks — tool surface, ledger encoding, answer
recovery — were put to Matt and ruled (see Decisions). The only work outside
this package's control is the runner/hub owed-to-handle answer delivery, tracked
as the filed dependency T7, not an open design question.

## Decisions (ratified by Matt — this record is post-fork)

All three load-bearing forks were put to Matt and ruled; the record is written
to these answers, not proposing them.

1. **Tool surface: dedicated `comms_post_ask`, posting into a thread/topic like
   messages.** Ruled: a dedicated tool (not a variant of `comms_post_message`),
   because it carries one contract in its own description (including the async
   "you will not receive the answer this turn" semantics) and gives future
   ask-format extension a clean home. It **posts into a topic/thread exactly
   like `comms_post_message`** — a named `topic` parameter (default `"general"`,
   get-or-created in the append transaction), never a channel-level post. (Both
   a `text`-XOR-`questions` mode-fork and an additive `questions?` field were
   weighed and rejected — see Alternatives.) DL-028's "two tools" count is stale
   (four ship today, `comms.ts:43`); the count refresh is owed regardless, and
   the load-bearing "no ask-answering" clause carries forward untouched.
2. **Ledger: partial-supersede DL-043 + count-refresh DL-028, both by
   citation.** DL-043's derivation MECHANISM is dead (no AgentFrame conversation
   variant, `agent.proto:46-85`; `mapping.ts` has no ask arm), while the `Ask =
   repeated AskQuestion` SHAPE stays live (`comms.proto:361-427`). Because the
   ledger status enum is binary and each row keeps a live half, this is encoded
   the house way (DL-183/DL-147 refine-by-citation), not a blanket flip: DL-043
   and DL-028 **stay `Active`**, and new rows DL-211/DL-212/DL-213 carry the
   deltas by citation (DL-213 records the mechanism death; DL-212 the count
   refresh). `compass-ask-typed-derivation.md`'s record-level `Status:` **stays
   `Active`** — it still holds a live decision (DL-043's shape); record-level
   status is lifecycle, per-decision truth lives only in the ledger. No code
   citation sweep owed. DL-139 cited as corroboration, no flip. (Executed in T5.)
3. **Answer recovery: key the owed answer on the agent HANDLE, not the session
   id; a human decides liveness at answer time.** Ruled: the session-id keying
   is a wart. An owed answer belongs to the agent account/handle, delivered to
   whatever session is next live for that handle. When the operator answers an
   ask whose agent is **not currently live**, recovery is a human decision —
   relaunch that agent (it receives the owed answer on its next live session) or
   route the answer to a new agent. There is deliberately **no agent-side boot
   poll** of the channel for stale answers. Consequences for this record:
   - The agent half correlates purely on `ask_id` (handle-scoped `PendingAsks`,
     in-memory within a live session), consuming `AskAnswerControl` on whatever
     session is live. No agent-side boot re-hydration task is planned.
   - The durable **owed-to-handle delivery** and the **session-id → handle
     re-key** in the runner/hub answer lane (`ask_waker.go`/`resume_start.go`/
     control retention) are a **filed dependency external to this package** —
     the driver files it as a runner/hub issue (entangled with RIG-1310) and
     this record depends on it for cross-relaunch answer survival. Until it
     lands, an answer delivered to a no-longer-live session is missed (the
     operator relaunches to retry); this is a stated, temporary limit, recorded
     in AGENTS.md (T6), not a silent drop.
4. **`comms_post_ask` topic parameter — settled.** The store has no home-topic
   default and hard-rejects an unset topic (`store/messages.go:36-37`;
   `agent_caller.go:45-46`); the named-`topic` shape (default `"general"`,
   get-or-created) is the only shape the store accepts — which is also exactly
   Decision 1's "post into a thread like messages".
