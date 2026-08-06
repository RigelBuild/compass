# Compass ask typed derivation — the superseding ask contract (Option A)

Status: Active

Tracking: SEA-1243 (Go-port wave follow-up flagged at PR #771 merge).

**This record supersedes design compass-0.5 D5's `Ask` shape by citation**
(`docs/designs/product/compass-0.5/design.md:288`, "D5 — The UI pivots around
the communication layer"; the frozen single-question `compass.v1.Ask` is D5's
contract child). Per the AGENTS.md freeze rule a frozen record is never
rewritten — a change ADDS a superseding record, and this is that record. Matt
ruled the contract fork: **Option A — reshape `compass.v1.Ask` to
`repeated AskQuestion` (buf-breaking)**, with atomic multi-question UX
REQUIRED and free-text answering (`customInput`) REQUIRED to survive. The
fork is decided; B/C/D are recorded below as considered-and-declined.

## Problem / Intent

The compass-agent's `ask` event mapping is a staged stub: `#deriveAsk`
(`packages/compass-agent/src/mapping.ts:256-281`) emits a counted
`UnmappedEvent` on `tool_execution_start` ("ask conversation-block derivation
staged — see deriveAsk", mapping.ts:262) and on a failed end
(`tool_execution_end && isError === true`, mapping.ts:270-279), so the typed
ask structure the agent produced is logged + counted but never rendered. MVP
needs Compass to surface the agent's `ask` as a real structured question — a
typed `compass.v1.Ask` conversation block a participant can answer via
`RespondToAsk`.

The crux is a cardinality mismatch between the two shapes on either side of
the mapping. Native OMP `ask` is **multi-question**
(`oss/forks/oh-my-pi/packages/coding-agent/src/tools/ask.ts:78-80`):

```ts
const askSchema = arkType({
  questions: QuestionItem.array().atLeastLength(1).describe("questions to ask"),
});
```

while the frozen `compass.v1.Ask` is **single-question**
(`proto/compass/v1/comms.proto:277-288`):

```proto
message Ask {
  string ask_id = 1;
  // The question the agent is asking.
  string question = 2;
  repeated AskOption options = 3;
  // Whether more than one option may be chosen.
  bool allow_multiple = 4;
  // The chosen option ids once answered; empty while pending (kept for audit).
  repeated string chosen_option_ids = 5;
}
```

Matt ruled the mismatch resolves on the contract side: one atomic
multi-question form, one `RespondToAsk` round-trip (the native tool awaits ONE
`QuestionResult[]` covering all questions, ask.ts:84-96), and the native
free-text channel (the always-offered `"Other (type your own)"`, ask.ts:48 →
`QuestionResult.customInput`, ask.ts:91) must have a contract carrier. This
record therefore does two things: (1) fully specifies the reshaped
`compass.v1` ask contract, and (2) specifies the typed `#deriveAsk` mapping
onto it.

## Approach — Option A: reshape `Ask` → `repeated AskQuestion` (buf-breaking)

Reshape the frozen contract so one `Ask` carries the native ask's full
question list, each question carrying every native axis, and one
`RespondToAsk` answers all of them atomically. This is a **deliberate
buf-breaking change**: `buf.yaml` gates the schema at
`breaking: use: [FILE]` (buf.yaml:19-21), under which `FIELD_NO_DELETE` fires
on the removed `Ask`/`RespondToAskRequest` fields. `buf` has no per-field
granularity, so reserving numbers does NOT satisfy the FILE gate. The gate is
satisfied instead by the **scoped, file-level `FIELD_NO_DELETE` exemption**
that PR #803 (T0) adds to `buf.yaml`
(`breaking.ignore_only.FIELD_NO_DELETE: [proto/compass/v1/comms.proto]`,
verified on `compass-sea-1243-t0-harness-drop` this run). That exemption is
file-scoped, so once A stacks on T0 its ask-field removals inherit it and
`buf breaking` PASSES — the removals are ratified by THIS record, and A's
proto task (T2) EXTENDS the `buf.yaml` exemption comment to cite this record
as their authorization (the comment enumerates each removal-set by record;
`buf` itself has no field granularity, so the comment is the only per-set
attribution). Matt ruled the break knowing the additive no-buf-break
alternative (Option D) existed and chose the clean reshape. The `reserved`
statements below are additionally wire/JSON hygiene (a stale writer's old
field bytes can never misparse as new fields); they are orthogonal to the
gate — the T0 exemption, not the reservations, is what makes it green.

No data migration accompanies the break: Compass is pre-production, the only
ask consumers are fixture-driven (`apps/ui/src/stub-data.ts`) or
freshly-seeded dev stores, and the store's JSONB block shape
(`go/internal/store/blocks.go:55-72`, `marshalBlocks` →
`storedBlock`) changes with the Go mirror types — dev databases reset.

### The reshaped contract

Message names: **`AskQuestion`** and **`AskQuestionAnswer`**. Verified against
every message in the `compass.v1` package this run (`^message` grep over
`comms.proto`, `compass.proto`, `agent.proto`): no `Question`, `AskQuestion`,
`Answer`, or `AskQuestionAnswer` exists, so either name is collision-free. The
`Ask`-prefixed forms win because they extend the existing `Ask`/`AskOption`
family naming (comms.proto:277, 291) and keep the generic identifiers
`Question`/`Answer` free in a package that already spans three protos.

```proto
// A structured question set the agent asks: one atomic form of one or more
// questions (mirrors the native OMP ask tool, which is multi-question by
// schema). A participant answers ALL questions in one RespondToAsk — the
// native tool awaits a single result covering every question. Not a
// permission prompt — permission gating is a separate, deferred concern.
message Ask {
  // Correlation id echoed by RespondToAsk; server-assigned and globally
  // unique, resolved within the caller's authorized workspaces. One id per
  // Ask (per native ask tool call), NOT per question — questions are keyed
  // inside the Ask by AskQuestion.question_id.
  string ask_id = 1;
  // The questions, in the order the agent asked them. At least one.
  repeated AskQuestion questions = 6;
  // Superseded single-question shape (compass-0.5 D5); see
  // docs/designs/product/compass-ask-typed-derivation.md.
  reserved 2 to 5;
  reserved "question", "options", "allow_multiple", "chosen_option_ids";
}

// One question within an Ask, carrying its own options and answer state.
message AskQuestion {
  // The agent-supplied question id (unique AND non-empty within the Ask); the
  // key an AskQuestionAnswer addresses. Native `QuestionItem.id` is bare
  // agent-controlled text (ask.ts:64, no uniqueness/non-empty constraint), so
  // uniqueness + non-emptiness is ENFORCED downstream (T5 mapper counts a
  // duplicate/empty id as malformed; the store append rejects it) — see Plan.
  string question_id = 1;
  // The question text.
  string question = 2;
  // Optional short display chip rendered above/beside the question.
  string header = 3;
  // Selectable options; MAY be empty (a free-text-only question — free-text
  // answering is always available, mirroring the native tool's always-offered
  // "Other (type your own)").
  repeated AskOption options = 4;
  // Whether more than one option may be chosen.
  bool allow_multiple = 5;
  // Zero-based index into options of the agent-recommended default; unset
  // when the agent recommended nothing.
  optional int32 recommended = 6;
  // ── Answer state, empty/unset while pending (kept for audit) ──
  // The chosen option ids once answered.
  repeated string chosen_option_ids = 7;
  // The participant's free-text answer, when they typed one instead of (or,
  // for allow_multiple, alongside) picking options.
  string custom_text = 8;
  // True when the answer was recorded by timeout auto-selection rather than a
  // participant (SEA-1310 owns whether/when the Compass answer path can time
  // out; the field is the audit carrier either way).
  bool timed_out = 9;
}

// One selectable answer to an AskQuestion.
message AskOption {
  string id = 1;
  string label = 2;
  // Optional explanatory text shown under the label.
  string description = 3;
  // Optional rich preview content (e.g. a rendered snippet) for interactive
  // ask dialogs.
  string preview = 4;
}
```

The answer RPC reshapes to per-question answers, one atomic request:

```proto
message RespondToAskRequest {
  string ask_id = 1;
  // Exactly one answer per AskQuestion in the Ask, keyed by question_id. The
  // server rejects a request that omits a question, repeats one, or names an
  // unknown one — answering is atomic, all questions in one round-trip.
  repeated AskQuestionAnswer answers = 3;
  // Superseded flat single-question answer shape.
  reserved 2;
  reserved "chosen_option_ids";
}

// The answer to one AskQuestion.
message AskQuestionAnswer {
  string question_id = 1;
  // The chosen option id(s); more than one only when the question allows it.
  repeated string chosen_option_ids = 2;
  // Free-text answer ("Other"); a question with no options is answered by
  // custom_text alone.
  string custom_text = 3;
}

message RespondToAskResponse {}
```

### Field-number discipline

- **Retained:** `Ask.ask_id = 1` and `RespondToAskRequest.ask_id = 1` keep
  their numbers and exact semantics (server-assigned correlation,
  comms.proto:278-280). `AskOption` fields 1-3 are untouched; `preview = 4`
  is a pure addition.
- **Reserved:** `Ask` fields 2-5 (`question`, `options`, `allow_multiple`,
  `chosen_option_ids`) and `RespondToAskRequest` field 2
  (`chosen_option_ids`), each with both number and name reserved. The new
  `questions` field takes 6 (not a reused 2) and `answers` takes 3, so no old
  wire tag is ever reinterpreted and no old JSON name ever re-binds.
- **New messages:** `AskQuestion`, `AskQuestionAnswer` — fresh names, no
  reservation interplay.
- **Intent:** this is a clean reshape, not wire-compatible evolution. Old
  writers/readers break by design (Matt's ruling); the reservations exist so
  the break is *loud* (unknown field / missing field) rather than a silent
  misparse, and so a future field can never collide with the dead numbers.

### Axis carriers (native → reshaped)

Every native *contract* axis has a carrier under this shape; two rich-dialog
answer affordances are deliberately dropped (noted after the table), so the
per-axis gap map in Alternatives shrinks to those two rather than to zero:

| native (`ask.ts`) | reshaped `compass.v1` carrier |
| --- | --- |
| `questions: QuestionItem[]` ≥1 (ask.ts:78-80) | `Ask.questions: repeated AskQuestion` |
| `QuestionItem.id` (ask.ts:64) | `AskQuestion.question_id` |
| `QuestionItem.question` (ask.ts:65) | `AskQuestion.question` |
| `QuestionItem.header?` (ask.ts:66) | `AskQuestion.header` (empty = absent) |
| `QuestionItem.multi?` (ask.ts:68) | `AskQuestion.allow_multiple` |
| `QuestionItem.recommended?` index (ask.ts:69) | `AskQuestion.recommended` (proto3 `optional`, unset = absent) |
| `OptionItem.label` / `.description?` (ask.ts:57-59) | `AskOption.label` / `.description` |
| `OptionItem.preview?` (ask.ts:60) | `AskOption.preview` |
| always-offered "Other (type your own)" → `QuestionResult.customInput` (ask.ts:48, 91) | free-text is always available at the contract level (no flag — mirrors the native tool, whose "Other" is unconditional, ask.ts:12 "Users will always be able to select \"Other\""); answer rides `AskQuestionAnswer.custom_text`, audit rides `AskQuestion.custom_text` |
| `QuestionResult.selectedOptions` labels (ask.ts:90) | `AskQuestionAnswer.chosen_option_ids` (ids; label reconstruction is SEA-1310's, see Mapping) |
| `QuestionResult.timedOut?` (ask.ts:94-95) | `AskQuestion.timed_out` (audit; SEA-1310 decides whether the Compass answer path times out at all) |

Deliberately dropped (no carrier), each an answer-side rich-dialog affordance
with no Compass analogue in v1:

- `QuestionResult.note` (ask.ts:92-93, "Optional note attached to the selected
  answer") — a free-form annotation beside the choice. Omitted now; `string
  note = 4` on `AskQuestionAnswer` is a non-breaking addition if SEA-1310 finds
  it needed.
- `AskToolDetails.chatRedirect` / the "Chat about this" reserved label
  (ask.ts:49, 110-112) — a native answer MODE that hands off to chat. Moot in
  Compass, which has first-class chat; not modeled.

Decisions folded into the shape (each resolvable at source, so resolved here
rather than asked — see Resolved decisions for the reasoning):

- **Correlation is per-Ask, not per-question.** One native `ask` tool call =
  one `Ask` block = one server-minted `ask_id`; the native tool awaits one
  `QuestionResult[]` for the whole call (the single results array is built and
  returned once per call, ask.ts:1073-1089; the `QuestionResult` interface is
  ask.ts:84-96), so SEA-1310 needs
  exactly one `toolCallId ↔ ask_id` key. Questions are addressed *within* the
  Ask by the agent-supplied `question_id` (native `QuestionItem.id`,
  ask.ts:64). The store's `mintAskIDs` (blocks.go:80-90) keeps its
  one-id-per-ask-block cardinality — no mint-per-question.
- **Free-text is flagless.** Native's "Other" is unconditional (ask.ts:12,
  48), so the contract makes free-text always available rather than carrying
  an `allow_custom_text` flag nobody can currently set to false. If a future
  non-OMP producer needs to disable it, an opt-out flag is a non-breaking
  addition then.
- **Zero-option questions are mapped, not bounced.** `QuestionItem.options`
  has no minimum (`OptionItem.array()`, ask.ts:67) and is natively answerable
  via "Other"; under this contract the same question is answerable via
  `custom_text`, so it maps as a free-text-only question. (This dissolves the
  prior record's OQ7.)
- **`timed_out` lives on `AskQuestion` as audit state**, beside
  `chosen_option_ids`/`custom_text` — same pattern the frozen shape used for
  chosen ids ("kept for audit", comms.proto:286-287). Whether the Compass
  answer path ever *sets* it (native auto-selects on timeout,
  `getAutoSelectionOnTimeout`, ask.ts:161-167) is SEA-1310's interception-seam
  call; the carrier exists either way so the audit trail can distinguish a
  timeout from a participant choice. Carrier-now over omit-now (a `bool
  timed_out = 9` addition is non-breaking later) is chosen so the store/UI
  mirror shape stays stable across SEA-1310's ruling rather than reshaping
  again when the answer path is designed.

### Server-side answer validation (contract semantics, not new code here)

`RespondToAsk` under the reshaped request is atomic. The server rejects
(InvalidArgument-class) a request whose `answers` do not cover exactly the
Ask's `question_id` set (every question has exactly one answer entry — the
atomic-round-trip requirement Matt ruled), an answer choosing >1 option where
`allow_multiple` is false, or an unknown option id. An *explicitly
empty* answer entry (no chosen ids AND empty `custom_text`) is an ACCEPTED
deliberate skip, not a rejection (Matt, this run — Resolved #9): the native
tool produces exactly this via forward-skip
(ask.ts:1073-1083 backfills an unanswered question as
`{selectedOptions: []}` with no `customInput`), so accepting it keeps
1:1 parity; mandatory-answer would tighten native semantics. Coverage
(every `question_id` answered exactly once) and per-answer
multiplicity/option-id checks still apply — an empty entry satisfies
coverage, it is not a missing answer.

The existing participation + unknown-actor guard order from the comms vertical
is unchanged by the reshape (PR #726 is closed-unmerged —
`go/internal/comms/` does not exist on baseline main, verified
this run — but the vertical is being re-cut as xenophon's live T2, on which
the handler half builds; see Plan T7).

### Mapping shape under A

`#deriveAsk` on `tool_execution_start` runtime-narrows `event.args` (the SDK
types `args` as `any`; mapping.ts:8-9 pins "every read of them is
runtime-narrowed (`typeof`/`in`), never an inline cast") against the native
`askSchema` shape and builds **ONE `Ask` block carrying N `AskQuestion`s** —
a 1:1 map of the native call, never decomposed — appended via the existing
`#appendBlock` seam (mapping.ts:238-245), which snapshots the block set into a
`MessageUpdated` conversation frame. Field mapping:

| native (`ask.ts:57-80`) | reshaped `compass.v1` |
| --- | --- |
| — (one tool call) | one `Ask`; `ask_id` left EMPTY — server-assigned (comms.proto:278-280; the Go store mints on append, `mintAskIDs`, blocks.go:80-90) |
| `questions[i].id` | `questions[i].question_id` (agent-supplied, passed through) |
| `questions[i].question` | `questions[i].question` |
| `questions[i].header?` | `questions[i].header` (absent → empty string) |
| `questions[i].multi?` | `questions[i].allow_multiple` (absent → false) |
| `questions[i].recommended?` | `questions[i].recommended` (absent → unset) |
| `questions[i].options[j].label` / `.description?` / `.preview?` | `options[j].label` / `.description` / `.preview` |
| — (options keyed by label, no id: ask.ts:57-61) | `options[j].id` — mapper-minted, deterministic: the zero-based option index as a decimal string (`"0"`, `"1"`, …) |
| — | `chosen_option_ids` / `custom_text` / `timed_out` — empty/unset while pending (answer-side, SEA-1310) |

Option-id minting: native options carry no id (ask.ts:57-61,
`OptionItem = { label, description?, preview? }`), so the mapper mints
index-string ids. Index ids are safer than label-derived ids (labels can
collide; the reserved-label `.narrow()` only excludes the three runtime
labels, ask.ts:70-76). SEA-1310 reconstructs the native label-keyed
`QuestionResult.selectedOptions` (ask.ts:90) from `chosen_option_ids` as
`questions[i].options[atoi(id)].label` — deterministic from state SEA-1310
already holds (the in-flight tool-call args plus the `toolCallId ↔ ask_id` key;
the answered `Ask` block also carries the full options list), so no separate
retention machinery is required. The mapper does not bounds-check the
agent-supplied `recommended` against `options.length`; the UI treats an
out-of-range index as "no highlight" (T6), so no mapper-side clamp is added.

Malformed `args` (fails narrowing: missing `questions`, empty array, wrong
member shapes, non-object, OR a `questions` list carrying a duplicate or empty
`question_id`) surface as one counted `UnmappedEvent` with a reason naming the
malformation — never a throw, never a silent `[]`. Because the map is 1:1 onto
ONE atomic `Ask`, a single malformed member (question `k` of N) bounces the
WHOLE ask as that one `UnmappedEvent` — there is no partial emission of the
valid questions, so no mixed-validity block ever reaches the store or SEA-1310.
A zero-option question is NOT malformed (schema-valid natively, ask.ts:67); it
maps as a free-text-only question (see Approach).

**The failed-end arm is kept verbatim.** A failed end
(`tool_execution_end && isError === true`) keeps today's counted surfacing
exactly (mapping.ts:270-279) — the PR #771 round-2 regression pin, tested at
mapping.test.ts:575-588 ("ask tool_execution_end with isError:true → one
counted UnmappedEvent (tool_execution_end:ask), failure never dropped").
Typed derivation replaces the START stub arm (mapping.ts:257-265) only. A
successful `update`/`end` still emits nothing — the block is defined by its
start args (pinned at mapping.test.ts:545-567).

The ask block appends into the block set of the assistant message that
requested the tool: `#blocks` resets only at `message_start` (mapping.ts:95)
and tool executions run after that message, so the ask sits mingled with that
message's settled text (ask is conversation continuity). Because the map is
1:1, message-id ambiguity is harmless to SEA-1310: even two `ask` toolCalls in
one assistant turn produce two distinct `Ask` blocks, each with its own
server-minted `ask_id`, and the one `toolCallId ↔ ask_id` key suffices — no
grouping/ordering/partial-completion problem (the problem Option B would have
created).

Answer-side wiring (native `toolCallId ↔ ask_id` correlation, blocking the
native tool on the Compass answer path instead of its own interactive prompt +
timeout auto-select, and reconstructing `QuestionResult[]` from an answered
`Ask`) is SEA-1310 scope — the `ask_answer` control arm is itself staged
awaiting that key (`packages/compass-agent/src/agent.ts:175-180`:
"ask_answer delivery staged — awaiting SEA-1310 ask correlation key") — and
stays out of this record, cited as the external dependency that makes the
rendered ask live rather than decorative.

## Alternatives considered

### The per-axis gap map (against the pre-reshape frozen contract)

Every axis on which native OMP `ask` and the pre-reshape frozen
`compass.v1.Ask` disagreed, verified at source this run. Under Option A every
row gains a carrier (see Axis carriers above); this map is what B/C would
have dropped and what D would have carried additively:

| axis | native OMP (`ask.ts`) | pre-reshape `compass.v1` (`comms.proto`) |
| --- | --- | --- |
| cardinality | `questions: QuestionItem[].atLeastLength(1)` (ask.ts:78-80) | single `string question = 2` (comms.proto:282) |
| option identity | by `label` only — `OptionItem = { label, description?, preview? }`, no id (ask.ts:57-61) | by option `id` — `AskOption { id, label, description }` (comms.proto:291-296); answers are `chosen_option_ids` |
| free-text answer | always-offered `"Other (type your own)"` → `customInput` (ask.ts:48, `QuestionResult.customInput` ask.ts:91) | absent — only option ids (comms.proto:287, RespondToAskRequest comms.proto:481-484) |
| recommended default | `recommended?: number` option index, auto-selected on timeout (ask.ts:14-15, 69) | absent |
| header / preview | `header?` display chip (ask.ts:66), per-option `preview?` (ask.ts:60) | absent |
| timeout auto-select | `timedOut?: boolean` on the result (ask.ts:94-95) | absent |
| answer shape | per-question `selectedOptions: string[]` (labels) (ask.ts:90) | flat `chosen_option_ids` (ids) (comms.proto:483-484) |

### Option B — decompose to N single-question Asks (declined)

Map native `questions[]` → N separate single-question `compass.v1.Ask` blocks
in the same message, contract unchanged. This was the near-term-cheap option
(zero buf-break, zero regen, zero server/UI change, ships on today's `main`)
and the prior version of this record recommended it. **Matt declined it**: it
has no atomic multi-question form — N independent asks and N `RespondToAsk`
round-trips, with partial answering able to strand the native tool call
(which awaits ONE `QuestionResult[]`, ask.ts:84-96) and no grouping key in
the contract for SEA-1310 to gather N asks back into one result — and it
drops the `customInput`/`recommended`/`header`/`preview`/`timedOut` axes
outright, where Matt ruled free-text MUST survive.

### Option C — single-question first, defer multi-question (declined)

Wire typed `#deriveAsk` for `questions.length === 1` only; bounce or truncate
multi-question asks. Strictly dominated by B (B's N=1 path is C's whole
scope; the loop costs nothing more), and multi-question is the normal native
shape, not an edge (the shipped gallery fixture is 2-question,
`oss/forks/oh-my-pi/packages/coding-agent/src/cli/gallery-fixtures/misc.ts:16-40`).
Declined with B, a fortiori.

### Option D — additive, non-breaking proto evolution (declined)

Reach the same parity by ADDING `repeated AskQuestion questions = 6` and the
axis carriers alongside the frozen fields 1-5 (deprecated-in-comment, never
removed), dual-writing `questions[0]` into the legacy single-question fields
during transition. Under `buf.yaml:19-21` (`breaking: use: [FILE]`) additions
are non-breaking, so D reaches full atomic parity with no buf-break and no
land-order hazard. **Matt saw D and declined it in favor of the clean
reshape**: Compass is pre-production with fixture-only ask consumers, so
dual-write transition machinery and permanently-dead legacy fields buy
nothing here — A pays one deliberate, well-ordered break for a contract with
no vestigial shape.

## Global Constraints

- Baseline: `main` @ `06e9a170` (post-#771 merge:
  `feat(compass): SEA-1243 T5 first-party agent package + internal proto gen
  lane`). Tracking issue: SEA-1243.
- **This record supersedes compass-0.5 D5's `Ask` shape by citation**
  (AGENTS.md freeze rule: a frozen record is never rewritten; a change ADDS a
  superseding record). D5's record file stays untouched; the reshaped contract
  in this record's Approach is now the authoritative `compass.v1` ask shape.
- **The buf-break is deliberate, and rides T0's exemption.** `buf breaking`
  gates at `breaking: use: [FILE]` (`buf.yaml:19-21`), under which
  `FIELD_NO_DELETE` fires on removed fields (reserving numbers does NOT satisfy
  the FILE gate). PR #803 (T0) adds a file-scoped
  `breaking.ignore_only.FIELD_NO_DELETE: [proto/compass/v1/comms.proto]`
  exemption (verified on `compass-sea-1243-t0-harness-drop` this run); stacked
  on T0, A's ask-field removals inherit it and `buf breaking` PASSES. A's proto
  task (T2) extends that exemption's comment to cite THIS record as the
  ask-removal-set's authorization. No additive/dual-write machinery is built
  (Option D declined).
- rule://planning-evidence: every claim about existing code in this record
  carries file+line and a snippet verified at source on this baseline, this
  run.
- **Failure-never-dropped invariant.** A failed ask end (`isError === true`)
  surfaces counted via `UnmappedEvent` (mapping.ts:270-279; pinned by
  mapping.test.ts:575-588, the PR #771 round-2 Greptile P1 regression). Typed
  derivation replaces the START stub arm only; the failed-end arm survives
  verbatim, RED-tested first (T1 before T5).
- `event.args` is SDK-typed `any`; every read MUST be runtime-narrowed
  (`typeof`/`in`), never an inline cast (mapping.ts:8-9). Malformed args
  surface as a counted `UnmappedEvent`, never a throw.
- `ask_id` is server-assigned, one per Ask (comms.proto:278-280); the agent
  NEVER mints one (matches `#appendBlock`'s no-server-id-to-mint stance,
  mapping.ts:233-236, and the Go store's mint-on-append, `mintAskIDs`,
  blocks.go:80-90 — cardinality unchanged by the reshape: one id per ask
  BLOCK, questions keyed by `question_id` within it).
- **Runner id-reconciliation constraint.** The store's UPDATE path REJECTS ask
  blocks with empty `ask_id` (the ask-id guard in `updateMessageBlocksExec`,
  `go/internal/store/messages.go`, pinned by
  `TestUpdateMessageBlocksRejectsEmptyAskID`), and
  the mapper re-emits the FULL block set on every settle (mapping.ts:238-245),
  so every post-ask `MessageUpdated` frame carries an id-less ask into that
  path. The eventual Runner MUST reconcile server-minted ids into subsequent
  update frames (runner owner's scope; a constraint this mapping shape
  creates regardless of the reshape).
- **SEA-1310 owns the native-ask resolution seam + answer wiring**: the
  `toolCallId ↔ ask_id` correlation, intercepting the native tool's own
  prompt/timeout resolution (`getAutoSelectionOnTimeout`, ask.ts:161-167) to
  block it on the Compass answer path, delivering `ask_answer`
  (agent.ts:175-180, staged), and reconstructing the label-keyed
  `QuestionResult[]` from an answered Ask. This record cites it as an
  external dependency, not an open question (Matt's ruling).
- **Land-order (proto stack).** A's proto + regen tasks (T2/T3) edit
  `comms.proto` and all three regen trees, which two in-flight lanes ALSO edit,
  so A stacks BEHIND both to avoid a generated-file merge fight:
  - **T0 / PR #803** (`compass-sea-1243-t0-harness-drop`, open) — drops the
    harness fields from `comms.proto`, regens all three trees, and establishes
    the file-scoped `FIELD_NO_DELETE` buf exemption A inherits.
  - **xenophon's T2 / comms vertical** (`xenophon-sea-1243-t4-comms-vertical`,
    live this run) — re-cuts the comms service from scratch AND narrows
    `comms.proto` (OQ-C + trace-variant/share-RPC/workspace_id removals,
    CreateChannel add), regenerating the same trees.
  - Pinned order: **T0 → xenophon-T2 → A**; A's T2/T3 rebase cleanly on their
    regen output instead of conflicting (compass owner pinned the T0→T2 base
    for the same reason).
- **Land-order (UI).** The UI half (T6) lands BEHIND PR #783 (open; it DELETES
  `ask-contract.test.ts` and rewrites `AgentView.tsx`, refolding ask invariants
  into `comms.test.ts` — landing a multi-question renderer first would collide
  with that rewrite).
- **Land-order (Go comms handler).** The `RespondToAsk` handler half (T7) is
  NOT dead work: PR #726 is closed-unmerged (`go/internal/comms/`
  absent on baseline main, verified this run), but that vertical is being
  actively RE-CUT as xenophon's T2 above. T7 builds on xenophon's landed T2
  CommsService, not on baseline. Until T2 lands, A's present-tense Go scope is
  proto + regen + store mirror only.
- Gates: biome; `moon run compass-agent:typecheck` + `compass-agent:test`
  (bun `mapping.test.ts` is the unit surface — mapping.ts:2-4: "the agent's
  own testable surface (design compass-0.6 §T5)"); `buf lint`; `buf breaking`
  (PASSES once stacked on T0's `FIELD_NO_DELETE` exemption — not an accepted
  red leg; the exemption + this record's ratification are what make it green);
  `go build/test ./...` for the store changes.

## Plan

Order: T1 (RED failure-surface pins) precedes T5 (the derivation) per
rule://red-green-testing. T2-T4 are the contract execution, stacked behind the
T0 (#803) → xenophon-T2 proto lanes. T6 (UI) is land-ordered behind #783; T7
(Go comms handler) is gated on xenophon's T2 comms vertical landing.

### T1 — failure-surface preservation tests, RED FIRST

Before touching `#deriveAsk`, extend
`packages/compass-agent/src/mapping.test.ts` so the failed-end
pin (mapping.test.ts:575-588) and the successful-update/end no-frame pins
(mapping.test.ts:545-567) are asserted to hold against the COMING typed
implementation: add the (initially RED against the stub, since the stub emits
`UnmappedEvent` not a conversation frame) assertion that a well-formed typed
start followed by a failed end yields both the conversation frame and the
counted failure, and that the `Object.hasOwn` prototype-chain suite
(mapping.test.ts:495-529) stays green. T1's assertions run BEFORE the regen
trees exist, so the RED pins are shape-agnostic — they assert `kind ===
"conversationUpdated"` plus the counted failure, deferring `AskQuestion`-shape
assertions to T5's derivation coverage.

Interfaces:

- Consumes: `EventMapper.map(event: AgentEvent): MapOutput[]` (mapping.ts:85);
  `UnmappedEvent { kind: "unmapped"; eventType: string; reason: string }`
  (mapping.ts:45-49).
- Produces: bun tests in
  `packages/compass-agent/src/mapping.test.ts` (the existing
  tool-execution describe block, mapping.test.ts:421). RED until T5 lands (the
  stub keeps emitting `UnmappedEvent` through T2/T3/T4; T5 replaces the start
  arm and turns these green).

### T2 — proto reshape + reserved fields (stacks on T0 + xenophon-T2)

Edit `proto/compass/v1/comms.proto`: replace `Ask`
(comms.proto:277-288) and `AskOption` (comms.proto:291-296) with the reshaped
`Ask` + new `AskQuestion` + extended `AskOption` from Approach, and replace
`RespondToAskRequest` (comms.proto:481-485) with the `repeated
AskQuestionAnswer answers` shape. Reserve old numbers + names exactly as
specified. Stack this branch on T0 (`compass-sea-1243-t0-harness-drop`) and,
once it lands, xenophon's comms-vertical T2 — both edit `comms.proto` + the
regen trees, so stacking avoids a generated-file conflict. EXTEND `buf.yaml`'s
`breaking.ignore_only.FIELD_NO_DELETE` comment (added by T0) to cite THIS
record as ratifying the ask-field removals (`Ask` 2-5, `RespondToAskRequest`
2) — additively, without repointing T0's existing harness/three-removal cites
(buf has no per-field granularity; the comment is the only per-set
attribution). `buf lint` clean; `buf breaking` PASSES on the inherited
exemption.

Interfaces:

- Consumes: current `comms.proto:277-296` (`Ask`/`AskOption`),
  `comms.proto:481-487` (`RespondToAskRequest`/`Response`).
- Produces: the reshaped messages in `comms.proto` — exact shapes as the
  proto block in Approach (`Ask{ask_id=1, questions=6, reserved 2-5}`,
  `AskQuestion{question_id=1..timed_out=9}`, `AskOption{preview=4}`,
  `RespondToAskRequest{ask_id=1, answers=3, reserved 2}`,
  `AskQuestionAnswer{question_id=1, chosen_option_ids=2, custom_text=3}`).

### T3 — regenerate the three gen trees

Run the buf codegen lane; commit the drift-gated outputs:
`packages/compass-client/src/gen/compass/v1/comms_pb.ts`,
`packages/compass-agent/src/gen/compass/v1/comms_pb.ts`
(current `AskSchema`/`AskOptionSchema` at comms_pb.ts:501-533 change; new
`AskQuestionSchema`/`AskQuestionAnswerSchema` appear), and the Go tree
`go/gen/compass/v1/comms.pb.go` (`type Ask struct` at
comms.pb.go:1001, `type RespondToAskRequest struct` at comms.pb.go:2779) +
`compassv1connect/comms.connect.go` (RespondToAsk handler plumbing at
comms.connect.go:138, 461-466 — regenerated, signatures type-change only).

Interfaces:

- Consumes: T2's `comms.proto`; the buf.gen.yaml lane (compass-agent's tree
  is the second `out:`, per `compassv1.ts:6-8` "generated into ./gen via a
  second `out:` on buf.gen.yaml — its own drift-gated tree").
- Produces: regenerated `Ask`, `AskQuestion`, `AskOption`,
  `RespondToAskRequest`, `AskQuestionAnswer` types + schemas in all three
  trees; drift gate green.

### T4 — Go store mirror types + JSONB shape

Update `go/internal/store/types.go:225-248`: `Ask` becomes
`{ AskID string; Questions []AskQuestion }`; new
`AskQuestion { QuestionID, Question, Header string; Options []AskOption;
AllowMultiple bool; Recommended *int32; ChosenOptionIDs []string; CustomText
string; TimedOut bool }`; `AskOption` gains `Preview string`. Update the
JSONB `storedBlock` marshal/unmarshal (blocks.go:55-72 `marshalBlocks`,
blocks.go:92+ `unmarshalBlocks`) for the nested questions. Add a totality
invariant both ways (same `ErrInvalidArgument` family as marshalBlocks'
exactly-one-of check, blocks.go:58-65): `marshalBlocks` rejects an ask block
with zero questions or with duplicate/empty `QuestionID`s across its questions
(the write-path half of the T5 mapper's malformed-id guard), and
`unmarshalBlocks` errors on a stored ask with zero questions — which also makes
any stale pre-reshape JSONB row (`{"question":…,"options":…}`, which decodes to
`Questions: nil` since Go ignores unknown keys) fail LOUD rather than decode to
an unanswerable ghost ask, honoring blocks.go's surfaced-not-silently-dropped
discipline (blocks.go:93-95). `mintAskIDs`
(blocks.go:80-90) is UNCHANGED in cardinality — still one id per ask block,
minted when `AskID == ""`. The store update path's empty-`AskID` rejection (the
ask-id guard in `updateMessageBlocksExec`,
`go/internal/store/messages.go`) is unchanged; its pin
(`TestUpdateMessageBlocksRejectsEmptyAskID`) and
the ask helpers (`askBlockID`) update to the new struct shape only.

Interfaces:

- Consumes: T3's regenerated Go types (the store mirrors the proto shape;
  types.go:225-228 cites comms.proto:277-296 — update the citation to this
  record).
- Produces: `store.Ask`, `store.AskQuestion`, `store.AskOption` (shapes
  above); `marshalBlocks([]MessageBlock) ([]byte, error)` /
  `unmarshalBlocks` round-tripping the nested shape; `go test ./internal/store`
  green. No data migration (pre-production; dev stores reset).

### T5 — typed `#deriveAsk`: 1:1 multi-question derivation

Replace the `tool_execution_start` stub arm
(`packages/compass-agent/src/mapping.ts:257-265`) with the typed
derivation from "Mapping shape under A": runtime-narrow `event.args`, build
ONE `Ask` with N `AskQuestion`s (index-string option ids, empty `askId`,
empty answer state), emit via `#appendBlock` (mapping.ts:238); on narrowing
failure — OR when the `questions` list carries a duplicate or empty
`question_id` (native `QuestionItem.id` is unconstrained, ask.ts:64) — emit one
counted `UnmappedEvent` naming the malformation. Keep the
failed-end arm (mapping.ts:270-279) byte-identical. Update the two
staged-stub doc comments: the `CONTENT_TOOLS` doc (mapping.ts:53-64, "staged
as a follow-up (see #deriveAsk)") and the `#deriveAsk` doc
(mapping.ts:247-255, "STAGED: …") to describe the live derivation. Then T1's
RED tests plus new derivation coverage go GREEN: well-formed 2-question
start (the gallery-fixture shape, misc.ts:16-40) → one `conversationUpdated`
frame whose last block is an ask with 2 questions
(`allowMultiple` false/true, `recommended` 0/unset, headers/previews mapped);
zero-option question maps as free-text-only; malformed-args matrix → counted
`UnmappedEvent`; ask coexists with previously settled text blocks.

Interfaces:

- Consumes: `AgentEvent` with `type: "tool_execution_start"`, `toolName:
  "ask"`, `args` narrowed to `{ questions: Array<{ id: string; question:
  string; header?: string; options: Array<{ label: string; description?:
  string; preview?: string }>; multi?: boolean; recommended?: number }> }`
  (ask.ts:57-80); `create`, `MessageBlockSchema`, `AskSchema`,
  `AskQuestionSchema`, `AskOptionSchema` from `./compassv1` (barrel:
  compassv1.ts:30-49 — add the new schema exports);
  `#appendBlock(block: MessageBlock): OutboundFrame` (mapping.ts:238).
- Produces: `#deriveAsk(event: AgentEvent): MapOutput[]` (signature
  unchanged) returning `[{ kind: "conversationUpdated", value:
  MessageUpdated }]` on a well-formed start (any N ≥ 1) or
  `[UnmappedEvent]` on malformed args; bun tests in `mapping.test.ts`;
  `moon run compass-agent:typecheck compass-agent:test` green.

### T6 — UI multi-question renderer (LAND-ORDERED BEHIND #783)

Blocked until PR #783 (channel-first UI reframe) merges: #783 deletes
`ask-contract.test.ts` and rewrites `AgentView.tsx` (verified in its file
list this run), so this task targets whatever renderer + `comms.test.ts`
invariant home #783 leaves. Reshape the ask rendering from the current
single-question arm (today `AgentView.tsx:138-171` reads `b().question`, one
`options` list, `allowMultiple`, `chosenOptionIds`; `askOptionsLocked` at
AgentView.tsx:53-58) into a multi-question form: per-question option groups,
header chips, recommended highlighting, preview affordance, a free-text
("Other") input per question, one atomic submit. Update the `.block-ask` CSS
family (`apps/ui/src/app.css:1093-1110+`), the fixture types
(`stub-data.ts:168-201` `AskOption`/ask `AcpBlock` — or their #783
successors), and refold the ask invariants (chosen ⊆ options, single-select
≤ 1 per question, unique option ids per question, answers cover all
questions) in the post-#783 test home.

Interfaces:

- Consumes: T3's regenerated client types (`compass-client` `comms_pb.ts`
  `Ask`/`AskQuestion`/`AskOption`); #783's landed renderer + fixture shape.
- Produces: multi-question ask renderer + CSS + fixtures + invariant tests;
  `moon run compass-ui:typecheck compass-ui:test` green.

### T7 — Go comms `RespondToAsk` handler (GATED on xenophon's T2 comms vertical)

`go/internal/comms/` does not exist on baseline main (PR #726, the
original comms vertical, is closed-unmerged — verified this run), but the
vertical is being re-cut from scratch as xenophon's T2
(`xenophon-sea-1243-t4-comms-vertical`, live this run) as a thin-shell
CommsService on the store. That T2 introduces a FLAT single-question answer
writer `store.AnswerAsk(ctx, actor AccountID, askID string, chosenOptionIDs
[]string) (Message, error)` (owner-blessed on `#svc.compass` this run:
visibility-scoped lookup collapsing not-found/not-visible, option-offered +
`AllowMultiple` arity validation, immutable-`ask_id` write path). When T2 lands,
T7 REWORKS both halves to the atomic multi-question contract from Approach: the
`RespondToAsk` handler AND `store.AnswerAsk`'s signature — from flat
`chosenOptionIDs []string` to per-question answers keyed by `question_id`, each
carrying `chosen_option_ids` + `custom_text`. Validate answers cover exactly the
Ask's `question_id` set and per-question multiplicity/option-id checks (an
empty entry — no chosen ids AND empty `custom_text` — is an ACCEPTED skip
per Matt's OQ2 ruling (Resolved #9), NOT rejected, and satisfies coverage;
T2's flat writer rejects empty, unaffected by OQ2 which governs only the
per-answer multi-question case), preserving T2's visibility-collapse +
authz-at-edge guard order. Until T2 lands, this record's Go scope is T3 + T4
only.

Interfaces:

- Consumes: T3's regenerated `RespondToAskRequest`/`AskQuestionAnswer`; the
  re-landed comms vertical's `service.go` + seam-contract test suite; T2's
  flat `store.AnswerAsk` (reworked here, not consumed as-is).
- Produces: atomic-answer `RespondToAsk(ctx, *connect.Request[v1.RespondToAskRequest])
  (*connect.Response[v1.RespondToAskResponse], error)` (connect surface per
  comms.connect.go:138), the per-question `store.AnswerAsk` rework, and
  seam-contract tests for the rejection matrix.

## Tasks

- [ ] T1 — failure-surface preservation tests, RED FIRST (precondition for
  T5 per rule://red-green-testing; the failed-end pin
  mapping.test.ts:575-588 must survive verbatim)
- [ ] T2 — proto reshape + reserved fields (`comms.proto`; deliberate
  buf-break riding T0's `FIELD_NO_DELETE` exemption; stacks on T0 → xenophon-T2)
- [ ] T3 — regen the three gen trees (compass-client + compass-agent
  `comms_pb.ts`, Go `comms.pb.go` + `compassv1connect`)
- [ ] T4 — Go store mirror types + JSONB + mint/update-path tests
  (cardinality unchanged: one ask_id per block; marshal/unmarshal reject zero
  questions or duplicate/empty question_ids)
- [ ] T5 — typed `#deriveAsk` 1:1 multi-question derivation + doc-comment
  updates + derivation coverage tests (duplicate/empty question_id → counted
  UnmappedEvent; turns T1 GREEN)
- [ ] T6 — UI multi-question renderer + CSS + fixtures + refolded invariants
  (GATED: land-ordered behind PR #783)
- [ ] T7 — Go comms `RespondToAsk` atomic-answer handler + per-question
  `store.AnswerAsk` rework (GATED on xenophon's T2 comms vertical landing;
  reworks T2's flat single-question answer writer, not baseline main)

## Resolved decisions

The load-bearing fork is RULED, not open. Recorded so no future reader
re-opens it:

1. **Contract fork → Option A** (Matt, via `ask`, this session):
   buf-breaking reshape to `repeated AskQuestion`. B/C/D considered and
   declined (see Alternatives).
2. **Atomic multi-question UX → REQUIRED** (Matt): one form, one
   `RespondToAsk` round-trip covering every question.
3. **Free-text `customInput` → MUST survive** (Matt): carried flagless
   (always available, mirroring native's unconditional "Other", ask.ts:12,
   48) via `AskQuestionAnswer.custom_text` + audit `AskQuestion.custom_text`.
4. **Native-ask resolution seam → SEA-1310** (Matt): interception of the
   native prompt/timeout path, `toolCallId ↔ ask_id` correlation, and
   `ask_answer` delivery are SEA-1310's scope; cited here as an external
   dependency.
5. **Correlation is per-Ask** (resolved at source): the native tool awaits
   one `QuestionResult[]` per call (results built/returned once per call,
   ask.ts:1073-1089), so one `toolCallId ↔ ask_id` key suffices; questions are
   addressed by agent-supplied `question_id` (ask.ts:64) inside the Ask.
   Because native `QuestionItem.id` carries no uniqueness/non-empty constraint,
   the contract's "unique within the Ask" is ENFORCED, not assumed: the T5
   mapper counts a duplicate/empty id as malformed and the store append rejects
   it (T4), so an unanswerable Ask (two questions sharing one key → no answer
   set can cover it) can never be persisted. `mintAskIDs` cardinality
   unchanged.
6. **Zero-option questions map** as free-text-only (schema-valid natively,
   ask.ts:67; answerable via `custom_text` exactly as natively via "Other").
7. **`timed_out` rides `AskQuestion`** as answer-side audit state; whether
   the Compass answer path ever sets it is SEA-1310's interception-seam
   call.
8. **Message names `AskQuestion`/`AskQuestionAnswer`** (resolved at source):
   collision-free across all three `compass.v1` protos this run; extends the
   existing `Ask*` family naming.
9. **Explicitly-empty answer → ACCEPTED skip, not rejected** (Matt, this
   run): an `AskQuestionAnswer` with no `chosen_option_ids` AND empty
   `custom_text` is a deliberate skip that SATISFIES coverage, not a
   validation error. Rejecting it would tighten native semantics — the
   native tool produces exactly this via forward-skip (`resultsByIndex`
   backfills an unanswered question as `{selectedOptions: []}` with no
   `customInput`, ask.ts:1073-1083). Accepting keeps 1:1 parity. Coverage
   (every `question_id` answered exactly once) and per-answer
   multiplicity/option-id checks still apply; an empty entry is a present
   answer, not a missing one. Sets the T7 rejection matrix and the
   answer-side contract SEA-1310 reconstructs against.

## Open Questions

1. **Does `RespondToAskRequest` need a per-request `timed_out` signal?** The
   contract carries `AskQuestion.timed_out` as audit state, but no caller can
   set it via the reshaped RPC — a timeout auto-answer would have to arrive
   through some other write path (server-internal, or a privileged field
   added later). Whether Compass-side ask timeouts exist AT ALL is
   SEA-1310's interception-seam design; if it rules "yes, and the runner
   records them via RespondToAsk", the RPC needs an additive
   `timed_out`/actor field then (non-breaking addition). Left open because
   it is decidable only inside SEA-1310's answer-path design, not at any
   source verifiable this run.
