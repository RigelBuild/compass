# Always-explicit ask submission + free-text (`custom_text`) answers (RIG-1330)

Status: Active
Tracker: RIG-1330

Ledger placeholder: the decisions below are marked
`DL-TBD-1`…`DL-TBD-5` — real row ids are allocated when this record's PR
flips `docs/designs/DECISIONS.md`, never by this draft.

## Problem / Intent

Three problems, one send model.

**Free-text questions are unanswerable.** An `AskQuestion` whose `options`
list is empty is a free-text question — the wire says so: "Free-text answer
(\"Other\"); a question with no options is answered by custom_text alone"
(`proto/compass/v1/comms.proto`, `AskQuestionAnswer.custom_text`, `:909-911`).
The UI renders such a question as an empty `.ask-options` container with no
input (`AskBlock`, `apps/ui/src/components/ChannelView.tsx:99-123` — the only
per-question content is `<For each={q().options}>`), so a free-text question
cannot be answered. This record designs the free-text answer path end to end:
domain shape, store surface, outbound wiring, renderer, and completion
accounting.

**Asks auto-send, and they must not (Matt's ruling).** Today the option click
that completes an ask ships the one unrepeatable `RespondToAsk` with no
confirmation (`answerAsk`'s tail, `store.ts:1719-1720`); a single-question ask
sends on its only click. Matt's ruling on this record: **an ask is never
auto-sent — submitting a response is always an explicit user gesture.** That
changes **already-shipped behaviour**, not just the new free-text path: the
completing-click send and the single-click single-question send are removed,
and the submit control becomes the sole, unconditional send path (D0). The
existing gate comment argued only against a *per-click* respond ("don't send
early"); it never argued "send without confirmation" — auto-send was simply
where the gate landed once partial sends were ruled out. Meanwhile the wire is
one-shot and unrepeatable (`applyAskAnswer`'s answer-once guard in
`go/internal/store/messages.go` returns `ErrConflict` on a second respond, and
is the sole single-fire mechanism), a first toggle on a multi-select question
can already complete an ask before the user's second pick, and free text
widens that sharp edge from a rare shape to a common one (typed text counting
toward completeness). Every other destructive one-shot in this UI has an
explicit control; the ask now does too.

**And the shipped UI is a strict subset of the contract (Matt's directive:
full parity).** The wire already carries every presentation axis the native
tool has — `header`, `recommended`, `AskOption.preview` — and free text on
every question; the UI renders none of them and offers the input nowhere.
The contract work is done (the merged derivation record cited in Approach);
the parity gap is entirely in the UI layer, and this record closes it: D1
widens the domain type, D4 puts the free-text input on every question, D6
renders the presentation axes.

Non-goals: proto or server changes, and timeout auto-selection (`timed_out`,
RIG-1310 — the one wire field that stays dropped). One deliberate divergence
from the native tool, and the only one: a single-question single-select ask
auto-submits natively ("A single-select question still submits immediately
when it is the only question", `omp://tools/ask.md` § Modes / Variants);
here it never does — Matt's ruling that an ask is never auto-sent (D0)
removes exactly that behaviour. Do not "fix" this later: the non-parity is
intentional, and this line is its record.

## Approach

Eight decisions. The through-lines: **nothing sends but an explicit submit**
(D0); the typed text is a **local pick with exactly the lifecycle of
`chosenOptionIds`** (D1-D5), so it lives in the same place and rides the
same machinery — the local ask copy in the store's message state — rather
than a parallel keyed map; and the UI renders **every wire axis except
`timed_out`** (D1, D6), full parity with the native tool.

"Explicit" means a deliberate confirmation, **not a mouse**: the intended
flow is answer every question, tab to submit, press Enter. That keyboard path
is a first-class requirement (D5), not an accessibility footnote — it comes
free from using a native `<button>`, and D5 names the three things that would
silently break it. What is being removed is only the *implicit* send — a
radio click that both answers and ships.

The ruling *simplifies* the prior draft: there is no "did text or the click
complete the ask" discrimination, one send path instead of two, and the submit
control is unconditional rather than a skip affordance that appears only on a
partially-answered ask. It also deletes machinery: `isAskComplete` loses its
only caller, `sendAsk`'s rollback parameter loses both of its, and
`sameAnswers` loses its one call site (D0).

**Checked against the tool this proto mirrors.** `comms.proto`'s ask
messages are written as a wire form of the agent harness's own `ask` tool.
The derivation is its own merged record —
`docs/designs/agent/compass-ask-typed-derivation.md` § "Axis carriers
(native → reshaped)" — which this record defers to for everything
contract-level; the tool's documented behaviour (`omp://tools/ask.md`) is
the reference for what "an ask" means here, not an analogy. What that
reference genuinely confirms about the central ruling: the tool's rich
dialog is a **form** the user fills and submits, and its plain fallback
treats a pick as navigation between questions, preserving prior answers.
What it does NOT confirm: the single-question single-select case, which
natively "still submits immediately when it is the only question"
(§ Modes / Variants) — precisely the auto-send Matt's ruling removes (L2
inverts the shipped test that pinned it). That is this record's **single
intentional divergence** from the native tool, named in Non-goals so nobody
later restores it as a parity fix. Everything else reaches parity here:
free text on every question (D4) and the presentation axes the wire already
carries — `header`, `recommended`, `preview` (D1, D6). The two native
affordances with no wire carrier at all — the per-answer note and the "Chat
about this" redirect — were dropped deliberately at the contract layer, not
here (Open Questions, item 3). Timeout auto-selection is the `timed_out`
field, owned by RIG-1310 and out of scope.

### D0 — sending is always an explicit gesture (DL-TBD-3)

An ask is sent **only** by the user clicking the submit control. No recording
path — option click, keystroke — ever triggers the wire. Concretely, in
`apps/ui/src/store.ts`:

- **`answerAsk`'s send tail is removed.** Today it ends
  `if (!answered || !isAskComplete(answered)) return;
  sendAsk(messageId, answered, before);` (`store.ts:1719-1720`). Both lines
  go; `answerAsk` becomes record-only, exactly like the new `answerAskText`
  (D2). The `before` capture (`store.ts:1673-1676`, the rollback target) goes
  with it; the `answered` local survives only to drive `clearAskError`.
- **The gate comment is rewritten.** `store.ts:1714-1718` currently reads
  "THE GATE (Matt's ruling): the click stays LOCAL until the ask is COMPLETE.
  … A single-question ask completes on its only click, so it still sends
  there." That ruling is superseded: clicks stay local, full stop. The
  replacement, where the tail used to be:

  ```typescript
  // Recording is LOCAL, always: the server takes exactly one respond per
  // ask, forever, and only the explicit submit (`submitAsk`) ever sends one.
  ```

- **`submitAsk` is the sole send path.** Its body barely moves: the
  `isAskSubmitted` / `ask.answered` gates stay (`store.ts:1729`, `:1737`), and
  the wholly-unanswered guard
  `if (ask.questions.every((q) => q.chosenOptionIds.length === 0)) return;`
  (`store.ts:1741`) becomes
  `if (!ask.questions.some(isQuestionAnswered)) return;` (D5) — a wholly
  blank respond says nothing, so it stays inert. Its header comment
  (`store.ts:1722-1727`, "The skip affordance. …") is rewritten — it is no
  longer the affordance for the skip case but the send path for every case:

  ```typescript
  // The ONE send path. Answers accumulate locally — clicks and typed text
  // alike — and this explicit gesture ships them atomically, with an empty
  // answer for each skipped question. Inert on a submitted, CLOSED, unknown,
  // or wholly unanswered ask.
  ```

- **`isAskComplete` is deleted.** Its only caller was the removed send tail
  (`store.ts:1420-1421`, `:1719`). Completeness survives solely as UI copy:
  `AskBlock` computes `answeredCount() < ask().questions.length` inline (from
  the shared `isQuestionAnswered`, D5) to switch the submit label — no store
  predicate needed.
- **The `AppStore` interface doc comments follow.** `answerAsk`'s
  (`store.ts:448-459`) promises "the completing click … a single-question ask
  completes on its only click" and "a REFUSED respond rolls the local answer
  back" — both now false; it shrinks to the record-only contract (local
  recording, first-responder-wins, the no-op gates). `submitAsk`'s
  (`store.ts:466-471`, "Submit an INCOMPLETE ask — the skip affordance")
  becomes the sole-send-path contract, matching its body comment above.
- **`ChannelView.tsx`'s component account is rewritten.** The `AskBlock` doc
  comment (`ChannelView.tsx:35-45`) says "clicks accumulate locally and the
  completing click ships them all at once. A question the user means to SKIP
  … grows a `submit` control" — now wrong twice over. Replacement:

  ```tsx
  /** An inline async ask (comms.proto Ask): questions with selectable
   *  options and an always-available free-text input, answerable in place —
   *  never a blocking modal. Every answer stays LOCAL until submit: the
   *  server accepts exactly ONE RespondToAsk per ask, so the submit control
   *  is the only send path, and unanswered questions ship blank (its copy
   *  says so). Once the ask is SETTLED — our respond issued, or the server's
   *  `answered` flag set by whoever answered first — every control locks. */
  ```

**The refusal path inverts from rollback to restage.** Today `sendAsk` takes a
`rollback` ask (the pre-click state) and conditionally restores it on refusal,
guarded by `sameAnswers(current, ask)` (`store.ts:1629`, `:1645-1652`). Both
callers of that machinery die: `answerAsk` no longer sends, and `submitAsk`
never passed a rollback ("nothing was recorded by this call",
`store.ts:1742-1744`). With submit recording nothing, a refusal has nothing to
roll *back* — the staged local state is already honest and retryable. What a
refusal can now cost is the **staged work itself**, via the in-flight-push
window (the decided contract below). So:

- `sendAsk`'s signature simplifies to `sendAsk(messageId, ask)` — the
  `rollback` parameter is deleted.
- `sameAnswers` (`store.ts:1467-1477`) is deleted: its one caller was the
  rollback guard (`store.ts:1649`). The prior draft widened it with a
  `customText` compare; that widening dies with it.
- The catch block's conditional restore becomes a conditional **restage** of
  the shipped ask (see next paragraph).

**Decided contract — the submitted-in-flight loss window is closed by
restaging.** The window: submit issues the respond and marks the ask
submitted; `preserveLocalAsks` deliberately skips submitted asks
(`store.ts:1539`), so a stream push restating the ask blank replaces the local
copy — clicks and typed text both; then the respond is refused and
`unmarkAskSubmitted` reopens the ask over that blank state. Today the same
window loses clicks; with free text it loses typed prose, which is costlier.
The shipped `ask` object in `sendAsk`'s closure **still holds every answer**
(it is the exact object `respondToAsk` serialized), so the catch restages it:

```typescript
.catch((error) => {
  unmarkAskSubmitted(ask.askId);
  const current = findAsk(messageId, ask.askId);
  // A refusal must not cost staged work: a blank push adopted while the
  // respond was in flight took the local answers, and the shipped ask still
  // holds them. Declines when the ask CLOSED meanwhile (the
  // accepted-then-lost-reply race) or its shape moved.
  if (
    current &&
    current !== ask &&
    !current.answered &&
    sameQuestions(current, ask)
  ) {
    putAsk(messageId, ask);
  }
  // … askErrors + onCommsError, unchanged …
});
```

Why restage is safe and rollback's guards are not needed: while the ask is
marked submitted, both recorders are gated off (`isAskSubmitted` at entry,
`store.ts:1672`, `:1729`), so no local edit can land between submit and
refusal — the only way `current` differs from `ask` is a stream push. An
*unanswered* push is always blank (the proto: answer values on an inbound Ask
are ignored; the server records chosen ids and `custom_text` and flips
`answered` in one function — `applyAskAnswer` sets `q.CustomText` then
`ask.Answered = true`, persisted by the single `updateMessageBlocksExec` in
the same tx), so restoring the shipped
answers over it is exactly what `preserveLocalAsks` would have done had the
ask not been submitted. An *answered* push is authoritative and the
`!current.answered` clause declines, preserving the accepted-then-lost-reply
behaviour the race suite pins. The `current !== ask` reference check keeps the
no-push case a no-op.

One adjacent note, not a regression: `preserveLocalAsks`'s `local.size === 0`
fast path (`store.ts:1547`) stops short-circuiting once the channel holds a
*closed* free-text-answered ask, because the widened raw scan (D2) sees its
recorded `customText` as a local pick worth collecting. That is exact parity
with today's closed *option*-answered asks, whose recorded `chosenOptionIds`
already defeat the same fast path; the per-ask `serverHasNoAnswer` guard
(`store.ts:1556`) still drops them from the actual replacement.

### D1 — the domain type reaches the full wire shape (DL-TBD-1)

`adaptAskQuestion` (`apps/ui/src/live/adapt.ts:239-251`) today drops four
wire fields under the comment "The wire's presentation/audit extras (header,
recommended, customText, timedOut) are not in the domain contract and are
deliberately dropped here rather than carried half-rendered"
(`adapt.ts:234-238`), and its option map drops `AskOption.preview` the same
way. The comment's own criterion — a field is carryable exactly when the
design renders it — was right, and this record now satisfies it four times
over: D4 binds `customText`, and D6 renders `header`, `recommended`, and
`preview`. Only `timedOut` still fails the criterion (RIG-1310, Non-goals);
it stays dropped, and the adapt comment is rewritten to name only it.

Each field's contract-level intent is already ruled in the merged derivation
record (`docs/designs/agent/compass-ask-typed-derivation.md` § "Axis
carriers (native → reshaped)") and is consumed here, not re-derived:
`header` carries native `QuestionItem.header` (empty = absent),
`recommended` carries the native zero-based option index (proto3 `optional`,
unset = absent), `AskOption.preview` carries native `OptionItem.preview`,
and `custom_text` carries the always-available free-text answer.

`customText` is the one that is **answer state**, not a presentation extra —
it sits under the proto's answer banner beside `chosen_option_ids`:

> `// ── Answer state, empty/unset while pending (kept for audit) ──`
> `// The chosen option ids once answered.`
> `repeated string chosen_option_ids = 7;`
> `// The participant's free-text answer, …`
> `string custom_text = 8;`

— `proto/compass/v1/comms.proto:435-443`. The domain already carries the
first answer-state field: `AskQuestion.chosenOptionIds` ("The chosen option
ids once answered; empty while pending (kept for audit)",
`apps/ui/src/comms-stub.ts:167-168`). `customText` follows that precedent
exactly, and for the same two reasons:

1. **Local staging.** `answerAsk` records an option click by rewriting the
   local ask copy (`answerQuestion` returns `{ ...q, chosenOptionIds: chosen
   }`, `store.ts:1377`), and every downstream mechanism — the stream-push
   carry (`preserveLocalAsks`, `store.ts:1520-1567`), the refusal restage
   (D0), the send payload (`sendAsk`, `store.ts:1637-1640`) — operates on
   that ask object. A parallel store map keyed by `(askId, questionId)` would
   need each mechanism re-implemented against a second state shape, and any
   one missed diverges silently (a push-carry that preserves clicks but
   discards text). On the object, the text inherits them.
2. **Audit rendering.** On a closed ask the wire value is the server's
   recorded answer; mapping it lets the locked free-text input display what
   was answered, symmetric with the `chosen` styling `chosenOptionIds`
   already drives.

The "half-rendered" hazard the adapt comment warns about no longer applies
to any of the four: each is fully rendered or bound by this design, which is
the comment's own criterion for carrying a field. The server-owned asymmetry
on `customText` is the same one `chosenOptionIds` already lives with: on a
*pending* ask the wire value is always empty (the proto's `:436-438` "Values
supplied on an inbound Ask (via PostMessage) are IGNORED" means the server
never publishes a non-empty one before answering), so local typing edits a
field the stream will not fight over — and `preserveLocalAsks` guards the
push-race window exactly as it does for clicks.

Concretely (`apps/ui/src/comms-stub.ts`):

```typescript
// AskQuestion, after `question`:
/** Optional short display chip above the question (comms.proto
 *  AskQuestion.header). "" = absent, the wire's own convention. */
header: string;

// AskQuestion, after `allowMultiple`:
/** Zero-based index into `options` of the agent-recommended option; absent
 *  when nothing was recommended (comms.proto AskQuestion.recommended,
 *  proto3 optional). A UI hint ONLY: it never pre-selects, and an
 *  out-of-range value is ignored (D6). */
recommended?: number;

// AskQuestion, after `chosenOptionIds` — the draft/audit discriminant:
/** Free-text answer (comms.proto AskQuestion.custom_text): the locally
 *  staged DRAFT while the ask is neither answered nor submitted; the
 *  server's recorded audit value once it is. "" when absent either way. */
customText: string;

// AskOption, after `description`:
/** Optional rich preview content (comms.proto AskOption.preview). */
preview?: string;
```

`adaptAskQuestion` maps `header` and `customText` verbatim and
`recommended: w.recommended` (the generated field is already
`number | undefined` — `comms_pb.ts`, `AskQuestion.recommended`); the option
map gains `preview: o.preview || undefined`, the same empty-string→absent
convention `description` uses on the line above (`adapt.ts:246`).

A free-text-**only** question still needs **no new discriminant field**: it
is precisely `q.options.length === 0`, which the proto blesses ("Selectable
options; MAY be empty (a free-text-only question …)", `comms.proto:426-429`)
and the store's own comment already names ("a free-text question, which
carries no options to choose", `store.ts:1487-1488`). Free text itself is on
every question (D4); the option-less shape is only the case where it is the
sole answer path.

### D2 — store surface: a sibling `answerAskText`, recording locally (DL-TBD-2)

Widening `answerAsk(messageId, askId, questionId, optionId)`
(`store.ts:460-465`) is rejected: an option click and a keystroke are
different inputs, and overloading one entry point with a
`string`-that-means-two-things parameter hides that. The new sibling:

```typescript
/** Record the free-text answer to a question, LOCALLY — like every
 *  recorder, it never sends; re-typing replaces the draft until the
 *  explicit submit. No-op on a single-select question already settled by a
 *  chosen option (exclusivity, D4), an unknown message/ask/question, a
 *  submitted ask, and a CLOSED (`answered`) ask. */
answerAskText: (
  messageId: string,
  askId: string,
  questionId: string,
  text: string,
) => void;
```

Under D0 the two recorders are **symmetric: neither sends.** (The prior draft
justified the asymmetry — "a click is a discrete gesture, a keystroke is not"
— which the ruling refutes; there is no asymmetry left to justify.)

Semantics, point by point against the existing machinery:

- **Editable until submit — on every question.** The single-select
  first-responder-wins early return ("a single-select question settles on
  its first answer", `store.ts:1369-1371`) reaches text only through
  exclusivity: the reducer sibling `answerQuestionText(q, text)` returns the
  same reference (the file's established no-op protocol,
  `store.ts:1360-1363`) when the question is a settled single-select
  (`!q.allowMultiple && q.chosenOptionIds.length > 0` — the pick already
  answered it, D4) or `text === q.customText` (nothing changed); otherwise
  `{ ...q, customText: text }`. Re-typing replaces.
- **The click side of exclusivity.** `answerQuestion` (the option reducer,
  `store.ts:1377`) widens: recording a pick on a single-select also clears
  the draft — `{ ...q, chosenOptionIds: chosen, customText: "" }` when
  `!q.allowMultiple` — so the staged state never holds the option-plus-text
  pair the server rejects (D4). On `allowMultiple` the text survives the
  toggle.
- **Gates.** Same as clicks: `isAskSubmitted(askId)` guard at entry
  (`store.ts:1672`), `ask.answered` guard inside the block map
  (`store.ts:1697`), `clearAskError(askId)` when a record lands
  (`store.ts:1713`).
- **`locked`/`closed`.** In `AskBlock`, `locked(q) = closed() ||
  (!q.allowMultiple && q.chosenOptionIds.length > 0)`
  (`ChannelView.tsx:72-73`). The input's disabled state is exactly
  `locked(q())` — the same accessor the option buttons use, no new lock
  concept — and on a single-select this is the exclusivity surface: a
  chosen option disables the input, so type-after-pick is structurally
  impossible (D4). On `allowMultiple`, and on any question without a pick,
  text stays editable until submit; a closed ask locks the input and shows
  the recorded `customText` (D1's audit payoff).

One existing store mechanism widens to see text as a pick (the D1 tradeoff
made explicit and paid once, here):

- **`preserveLocalAsks`** — the unshipped-edit scan `if
  (b.ask.questions.every((q) => q.chosenOptionIds.length === 0)) continue;`
  (`store.ts:1540-1541`) becomes `q.chosenOptionIds.length === 0 &&
  q.customText === ""` — a typed draft is an unshipped edit a stream push
  must not discard. Raw compare, not trimmed: any typed character is worth
  carrying. (The `local.size === 0` fast-path consequence is in D0.)

The prior draft's other two widenings are gone: `sameAnswers` is deleted
outright (D0), and `isAskComplete` is deleted with its caller (D0);
`submitAsk`'s guard moves to the shared predicate (D5).

The shared predicates live beside the `Ask` types in `comms-stub.ts` as
exported pure functions, so store and renderer read one definition:

```typescript
/** An option-less question — free text is the ONLY way to answer it
 *  (comms.proto AskQuestion.options MAY be empty). Free text itself is
 *  available on EVERY question (D4); this predicate only picks the
 *  option-less copy (hint line, placeholder). */
export function isFreeTextQuestion(q: AskQuestion): boolean;
/** Whether this question holds an answer — a chosen option, or a
 *  non-whitespace draft/recorded text. */
export function isQuestionAnswered(q: AskQuestion): boolean;
```

with `isQuestionAnswered(q) = q.chosenOptionIds.length > 0 ||
q.customText.trim() !== ""`. Whitespace-only text does not count as
answered: the wire reads empty `custom_text` + no chosen ids as an accepted
skip (`store.ts:1484-1486`), and a whitespace "answer" is a skip to every
reader; counting it answered would enable the submit control over a
question the wire will record as skipped.

### D3 — the outbound `RespondToAsk` carries `customText` per question

`sendAsk` builds the answers array today as

```typescript
answers: ask.questions.map((q) => ({
  questionId: q.questionId,
  chosenOptionIds: [...q.chosenOptionIds],
})),
```

(`store.ts:1637-1640`). It becomes:

```typescript
answers: ask.questions.map((q) => ({
  questionId: q.questionId,
  chosenOptionIds: [...q.chosenOptionIds],
  customText:
    !q.allowMultiple && q.chosenOptionIds.length > 0
      ? ""
      : q.customText.trim(),
})),
```

- **Coverage contract preserved** — still exactly one entry per question, in
  ask order ("Exactly one answer per AskQuestion in the Ask, keyed by
  question_id", `comms.proto:898-900`); nothing about the map's shape
  changes.
- **Accepted-skip shape preserved** — a skipped question still ships
  `chosenOptionIds: []` and `customText: ""`, the exact shape the store
  documents as "an ACCEPTED skip that satisfies the wire's
  coverage-of-every-question contract" (`store.ts:1484-1486`). A skipped
  *free-text* question (never typed, or whitespace only) ships the same
  shape: `""` after trim.
- **Trim at the send seam, once** — the staged draft stays verbatim (what the
  user sees in the input is what they typed); the trim happens only where the
  value becomes the permanent audit record, keeping D2's answered-predicate
  (`trim() !== ""`) and the shipped payload consistent by construction.
- **The exclusivity rule is enforced structurally at the seam** — a
  single-select holding a chosen option ships `customText: ""`,
  unconditionally. The recorders already keep the staged state exclusive
  (D2), so this guard should be redundant; it stays because the cost of
  needing it once is the whole ask: `validateQuestionAnswer` in
  `go/internal/store/messages.go` rejects one chosen option plus non-empty
  custom text on a non-`allow_multiple` question with `ErrInvalidArgument`,
  and the respond is one-shot. On `allowMultiple` both ship together, which
  the server accepts (the proto: custom text is typed "instead of (or, for
  allow_multiple, alongside) picking options").

`comms-fake.ts`'s `respondToAsk` recorder (`live/comms-fake.ts:263-266`,
`:301-307`) widens its request type and its `askResponses` copy to carry
`customText`, so renderer/store tests can assert the wire payload — today it
records only `questionId` + `chosenOptionIds`. Any test asserting a
`customText` wire shape sequences after this widening (see D7/T2).

### D4 — renderer: a free-text input on every question (DL-TBD-4)

Free text renders on **every** question — matching the native tool's
unconditional `Other (type your own)` and the proto's "free-text answering
is always available" (`comms.proto:426-429`) — not only on the option-less
ones. Matt's directive: full parity. The prior draft's
`options.length === 0` gate was this record's one deliberate scope cut, and
it is gone.

**Shape: an always-visible input under the option row, not an "Other"
chip.** Native's `Other` is an entry in its option list, so a chip inside
`.ask-options` would match native's shape — but it would be a fake
`.ask-option` (no option id, excluded from the answer map and from the
`chosen` styling) plus a revealed/hidden flag the store does not hold, and
surviving pushes and re-renders for free is exactly why D1 put the draft on
the ask copy rather than in component state. An always-visible single-line
input directly under `.ask-options` is one DOM element with no new state,
and it keeps the parity point visible instead of re-hiding free text behind
a discovery step — a discoverability gap is what this decision removes. The
cost is one thin input row per question.

In `AskBlock` (`ChannelView.tsx:99-123`), the per-question body becomes the
existing `.ask-options` `<For>` (rendered only when options exist) followed
unconditionally by the input:

```tsx
<Show when={q().options.length > 0}>
  <div class="ask-options">{/* the existing <For> */}</div>
</Show>
<input
  type="text"
  class="ask-text"
  value={q().customText}
  disabled={locked(q())}
  aria-label={q().question}
  placeholder={
    isFreeTextQuestion(q()) ? "type your answer" : "other — type your own"
  }
  onInput={(e) =>
    store.answerAskText(
      props.messageId,
      ask().askId,
      q().questionId,
      e.currentTarget.value,
    )
  }
/>
```

- **Single-line, not multi-line.** The proto frames the field as the
  "Other"-chip answer ("Free-text answer (\"Other\")", `comms.proto:909`;
  "mirroring the native tool's always-offered 'Other (type your own)'",
  `comms.proto:427-428`) — a short typed alternative to picking an option,
  not a prose surface. The ask block is an inline channel element; long-form
  prose already has a home one gesture away (a channel reply). A single-line
  input accepts arbitrary length, just no line breaks. Multi-line growth is a
  named Open Question, non-load-bearing.
- **Binding and commit.** Controlled input: `value` reads the store's staged
  draft, `onInput` records per keystroke through `answerAskText`. One source
  of truth — the same local ask copy `preserveLocalAsks` protects — so a
  stream push landing mid-typing cannot eat the draft (the DOM value and the
  store never disagree). The per-keystroke `setComms` map over messages is
  the price; it is the same O(messages) write a click already performs
  (`store.ts:1678-1710`) at human typing rate.
- **Exclusivity on a single-select is a correctness rule, not styling.**
  `validateQuestionAnswer` (`go/internal/store/messages.go`) rejects one
  chosen option plus non-empty custom text on a single-select with
  `ErrInvalidArgument`, and the ask has ONE unrepeatable respond — a UI
  that lets both stand burns it on a refusal. The resolution, per gesture:
  **choosing an option clears the staged draft** (`answerQuestion` records
  `customText: ""` on a single-select pick, D2) and settles the question
  under the shipped first-responder-wins rule, which disables the input via
  `locked(q())` — so **typing after a pick never arises**; it is
  structurally impossible, not merely cleared. The destructive direction is
  named honestly: a pick discards typed text. That is the right cost order,
  because the draft is provisional by design (editable until submit) while
  the pick is the settling gesture the shipped single-select model already
  treats as final. On `allowMultiple` nothing clears and nothing locks:
  options and text coexist and ship together (D3), which the server
  accepts.
- **Enter in the *input* does not send; Enter on the *submit control* does.**
  These are different keys in different places, and only the first is
  declined. Enter while typing in one question's field would ship the whole
  ask — blanks included for every question the user had not reached — from a
  gesture that in every other text field means "commit this field", not
  "commit the form". So the input has no `onKeyDown`. Enter (and Space) on
  the focused submit control is the *primary* keyboard path and must work:
  see D5's keyboard contract. A user answers every question, tabs to
  submit, and presses Enter.
- **Disabled/locked.** `disabled={locked(q())}` — identical accessor to the
  option buttons (`ChannelView.tsx:105`). After submit or on a closed ask
  the input is disabled and displays the recorded `customText` from the
  wire.
- **Accessibility.** The option buttons carry `aria-pressed`
  (`ChannelView.tsx:114`); a text input instead needs a label association.
  The question text lives in a bare `div.ask-question` with no id, so
  `aria-label={q().question}` provides the accessible name without minting
  id plumbing. `aria-pressed` is not applicable to a text input.
- **Hint line, three states.** The `choose any`/`choose one` hint
  (`ChannelView.tsx:95-98`) must say that text is now an answer path
  everywhere: option-less — "type your answer · async — answer when ready";
  single-select with options — "choose one, or type your own · async —
  answer when ready"; `allowMultiple` — "choose any, or type · async —
  answer when ready".
- **CSS.** `.ask-text` joins the ask family in `apps/ui/src/app.css` beside
  `.ask-option` (`app.css:1161`), using the existing tokens
  (`--cx-text-dim`, `--cx-border-focus` family) — sizing to match the option
  row so a mixed ask reads as one block.

### D5 — the submit control: unconditional, with honest copy

This supersedes the prior draft's `canSubmit`. The control is no longer a
skip affordance that appears only in the partially-answered window
(`ChannelView.tsx:79-82`, `canSubmit = !closed() && answeredCount() > 0 &&
answeredCount() < ask().questions.length`) — it is the **only** way any ask
ever sends, so it renders on every live ask from first paint (a send path the
user must discover mid-flow is the empty-options bug reborn one layer up).

- **Visible:** whenever the ask is live — `!closed()`. On a settled ask
  (submitted, or server-`answered`) it disappears with the rest of the
  interactive surface, exactly as today.
- **Enabled:** `answeredCount() > 0` — mirrors `submitAsk`'s
  wholly-unanswered guard (D0), so the control never offers a gesture the
  store would drop. Disabled on an untouched ask, it still *announces* that
  an explicit submit is coming.
- **Copy, switched on `answeredCount() < ask().questions.length`:** a
  partially-answered ask reads **"submit — skip the rest"** (the user
  shipping blanks must know the rest goes blank; the `title` keeps the
  once-only warning); a fully-answered ask — and the disabled untouched
  state — reads plain **"submit"**.
- **Keyboard-operable — this is the primary path, not an accessibility
  afterthought.** The intended flow is: answer the questions, move focus to
  submit, press Enter. It comes free from the platform — a native `<button>`
  activates on Enter and Space and fires `onClick` with no key handling of
  our own — so the work is not to build it but to **not break it**. Two
  things that would silently defeat it are absent and must stay absent: there
  is no `<form>` anywhere in `apps/ui/src` (so no implicit submission and no
  stray `type="submit"` default — keep `type="button"`), and the only `Enter`
  interception in the file is the composer's own `onKeyDown`
  (`ChannelView.tsx:316-321`), a different component that never sees ask
  focus. Options are `<button>`s too, so Tab already reaches submit in DOM
  order with no `tabindex` needed. **A disabled button is not focusable**
  (measured), so on an untouched ask the control announces itself visually
  but cannot be tabbed to — acceptable, since an untouched ask has nothing
  to send.
- **What the test suite can and cannot prove here.** happy-dom does not
  implement the button's implicit Enter-to-click default action: a
  `keyDown` Enter fires `onClick` zero times under `bun test` (measured
  against a click positive control). So R6 pins the *preconditions* —
  `BUTTON`, `type="button"`, no ancestor `<form>`, focusable, Enter left
  uncancelled — and the keystroke itself is verified manually in the running
  app. Do not let a green suite be read as proof the key works.

The predicates, replacing the old `answeredCount`/`hasStagedText`/`canSubmit`
trio:

```typescript
// ChannelView.tsx — AskBlock:
const answeredCount = () => ask().questions.filter(isQuestionAnswered).length;
const canSubmit = () => answeredCount() > 0; // drives disabled, not <Show>
// visibility: <Show when={!closed()}> around the submit row
```

**`hasStagedText` collapses away** — it never lands. Its whole purpose in the
prior draft was surfacing the control when typed text completed an ask (the
one state where auto-send could not fire and the skip-window predicate hid
the only send path). With the control unconditional on a live ask, that state
needs no detector.

`answerAsk`'s send tail is gone (D0), so nothing in the store consumes
completeness; the label switch above is the only completeness reader left.

### D6 — the presentation axes render: `header`, `recommended`, `preview` (DL-TBD-5)

Parity is not only the answer path: the wire carries three presentation
fields the native rich dialog renders, and D1 maps a field only when the
design renders it — this decision is what makes D1's widening legal. It is
a separate decision, not folded into D4, because D4 is about answering and
this is about display; nothing here records or ships state.

- **`header` — a chip above the question.** Native calls it a "short
  display chip" (`omp://tools/ask.md`, `Question.header`). Rendered as
  `<div class="ask-header">` immediately above `.ask-question`, inside
  `<Show when={q().header}>` so an empty header (the wire's absent) renders
  no node. `.ask-header` joins the family in `apps/ui/src/app.css` above
  `.ask-question` (`app.css:1143`): small, dim, chip-shaped — visually
  subordinate to the question text it labels.
- **`recommended` — a visual mark; never a label edit, never a
  pre-selection.** The option at a valid `recommended` index gains a
  `recommended` class on its `.ask-option` button and a small
  `<span class="ask-option-rec">recommended</span>` badge beside the label.
  Visual marking rather than native's fallback `(Recommended)` label
  suffix: the label is the agent's text — it is the button's accessible
  name and what tests select on — and native itself has to strip the suffix
  back out of its own results (`omp://tools/ask.md` § Notes), a wart a UI
  with a real style layer does not need. **Invalid index: ignored**, as
  native ignores it ("Invalid indexes are ignored for selection"). The
  renderer marks only when `Number.isInteger(r) && r >= 0 &&
  r < q.options.length`; any other value renders exactly as if
  `recommended` were unset — nothing throws, nothing indexes out of range.
  **It never pre-selects.** Native treats it as "only a UI/default hint"
  (`omp://tools/ask.md` § Notes), and pre-selecting would stage an answer
  the user never gave — under D5 an enabled submit would then ship it,
  manufacturing an answer in direct collision with D0's explicit-gesture
  ruling. `chosenOptionIds` stays empty until the user acts. (Native's one
  use of the index as a *default* is timeout auto-selection — the
  `timed_out` path, RIG-1310's, out of scope.)
- **`preview` — rendered, minimally, capped.** "Rich preview content (e.g.
  a rendered snippet)" (`AskOption.preview`, the generated `comms_pb.ts`
  comment). Rendered inside the option button under the description as
  `<code class="ask-option-preview">`: monospace, `white-space: pre-wrap`,
  with a `max-height` cap (roughly eight lines) and `overflow: hidden` so a
  pathological preview cannot blow up the channel column. Rendered rather
  than deferred, deliberately: deferring the render would forbid the D1
  mapping (the drop comment's criterion) and leave the axis dropped, short
  of the directive. Plain text only — no markdown or HTML interpretation; a
  richer rendering is a future widening contained to this element. The cap
  is honest lossiness in an inline block: preview is a presentation aid,
  not answer state.
- **CSS.** `.ask-header`, `.ask-option-rec`, and `.ask-option-preview` join
  the `.ask-*` family beside `.ask-question` (`app.css:1143`),
  `.ask-option` (`app.css:1161`), and `.ask-option-desc` (`app.css:1176`),
  on the existing tokens (`--cx-text-dim`, `--cx-text-faint`,
  `--cx-accent`).

### D7 — test plan

Two kinds of work: **new tests** (send-model contracts + the free-text path)
and **inversions of shipped tests** that pin the auto-send this record
removes. A reviewer must be able to tell an intended inversion from a broken
test, so the inversions are enumerated per file at the end.

#### New tests

**`apps/ui/src/store.live.test.ts`** — the store write-path charter:

1. **L1 (mandatory — the design's most load-bearing safety claim was
   previously untested).** All but one question of a mixed ask answered
   by clicks; type into the remaining free-text question (completing the ask
   under `isQuestionAnswered`); assert `fake.askResponses` **stays empty**.
   Then `submitAsk`: exactly one respond, the typed question carrying the
   trimmed text. RED today twice over: `answerAskText` does not exist, and
   the wire-payload half needs the D3 recorder widening — so the payload
   assertion lands with T2, after the fake widening it depends on.
2. **L2 (mandatory — the shipped-behaviour change).** A single-question ask's
   only click sends **nothing**; `submitAsk` then ships it. RED today: the
   click auto-sends (`store.ts:1719-1720`), so `askResponses` is non-empty
   before any submit. Belongs here because the suite already owns the
   single-question send contract it inverts (`:373`).
3. **L3.** Whitespace-only typed text: submit on an otherwise-answered mixed
   ask ships that question as `customText: ""` (the accepted-skip shape).
   RED: no text path exists. Here because the trim seam is a store/send
   contract, not a DOM one.
4. **L4 (mandatory — the exclusivity rule; a violation burns the one
   respond).** On a single-select question: type a draft, then click an
   option — the staged `customText` clears, and a subsequent `submitAsk`
   ships that question as the chosen id with `customText: ""`. Then the
   other direction: with the option chosen, `answerAskText` is a no-op (the
   draft stays empty). RED: neither the clearing nor the gate exists.
5. **L5 — `allowMultiple` carries both.** Toggle an option and type on the
   same multi-select question; submit ships both the chosen id and the
   trimmed text in one answer entry. RED: no text path exists.

**`apps/ui/src/store.ask-race.test.ts`** — the `adoptComms`-vs-local-pick
charter ("this suite's subject is narrowly `adoptComms` — what a stream push
does to an in-progress ask", `:33-34`), using its `askMessage` builder:

1. **S1 — a stream push does not discard a typed draft.** Record text via
   `answerAskText`, emit a `messageUpdated` push carrying the same-shape
   unanswered ask, assert the local `customText` survives (the
   `preserveLocalAsks` widening, D2). RED: the unshipped-edit scan sees only
   chosen ids, so the push discards the draft.
2. **S2 — a push carrying a closed free-text ask wins over the draft.** Same,
   but the push has `answered: true`: the local draft is dropped and the
   server's recorded `customText` shows. Extends the suite's existing
   custom_text-only-closure case (`:296-302`) from "not restored" to "not
   restored *and* the draft yields". RED for the draft half.
3. **S3 (mandatory — the D0 restage contract). Split across two slices,
   because the mechanism lands before the field does.** Hold the respond in
   flight (`fake.holdNextAskResponse`), `submitAsk`, then emit a blank
   unanswered restatement of the ask (adopted, because the submitted ask is
   skipped by the preserve), then reject the held respond. Assert the staged
   answer is back and the ask is retryable (`isAskSubmitted` false). RED
   against the pre-change tree and against a change that omits the restage:
   the state after refusal is the adopted blank ask. Belongs in this suite:
   it is precisely an `adoptComms`-vs-local interaction, beside the other
   held-in-flight case (`:601`).
   - **S3a, in T0 — clicks only.** The restage mechanism (`sameQuestions`
     plus `putAsk` in `sendAsk`'s catch) is field-agnostic and ships in T0,
     so its test must be too: click on a mixed ask, then the sequence
     above, asserting `chosenIn` is restored. Compiles at the end of T0,
     which a typed-text assertion would not — `answerAskText` is a T2
     export and `customText` a T1 field.
   - **S3b, in T2 — the typed half.** Same sequence with a typed draft
     alongside the click; asserts `customText` is restored too. Sequences
     after T1/T2 for the symbols it needs, beside S1/S2 which need the same
     ones.

**`apps/ui/src/components/ChannelView.ask.test.tsx`** — the render surface
over the live fake, whose `freeText` option already builds option-less wire
questions (`comms-fake.ts:453-455`, `:499-503`), same `mountAsk`/`settled`
harness (`:62-87`):

1. **R1 — every question renders an answerable input.** Snapshot with
   `freeText: ["q-2"]`: `.ask-text` exists for **both** questions — for q-2
   (option-less) it is the only per-question content, zero `.ask-option`
   beside it; for q-1 it renders under the option row. Both enabled. RED:
   no input renders today.
2. **R2 — typed text reaches `custom_text` on the outbound respond.** Type
   into q-2's input (`fireEvent.input`), click a q-1 option, click the
   submit control, assert `fake.askResponses` equals one respond whose q-2
   entry carries `customText: "<typed>"` and whose q-1 entry carries the
   chosen id with `customText: ""` (the D3 shape, via the widened
   recorder). RED: no store path records or ships text.
3. **R3 — the draft is editable until submit.** Type, re-type, assert the
   input value and the shipped `customText` reflect the second typing; after
   submit the input is disabled. RED.
4. **R4 (mandatory — the inverted old draft's R4, and the central D0
   contract for every ask shape).** Type into q-2, then click a q-1 option —
   the ask is now complete — and assert **nothing sends**
   (`fake.askResponses` empty). Only the submit control's click ships. RED
   today: the completing click auto-sends.
5. **R5 — the submit control is unconditional on a live ask.** From first
   render: control present and disabled; after one answer: enabled,
   "submit — skip the rest"; after all answers: enabled, "submit"; after
   submit: gone. RED: today the control renders only in the
   partially-answered window (`ChannelView.tsx:79-82`).
6. **R6 (mandatory — the keyboard path is a decided contract, D5) — but it
   asserts *preconditions*, not an Enter keystroke.** Measured in this
   harness: happy-dom does **not** implement a button's implicit
   Enter-to-click default action, so `fireEvent.keyDown(btn, {key:"Enter"})`
   fires `onClick` **zero** times while a click fires once. A test that
   dispatches Enter and then asserts one respond therefore proves nothing —
   it would pass only if a click ran too, which is R2. So R6 asserts the
   three structural facts the native behaviour depends on, each verified
   assertable: the submit control is a `BUTTON` with `type="button"`, has no
   ancestor `<form>` (`closest("form") === null`), and is focusable when
   enabled (`document.activeElement` after `.focus()`). Plus the interceptor
   probe, which *is* observable: dispatch a bubbling cancelable `keydown`
   Enter at the control and assert `ev.defaultPrevented === false`, catching
   an ancestor handler that would swallow Enter (control: with such a handler
   the flag reads `true`). These fail independently of R2's click. GREEN the
   moment D5's control renders, so R6 is a **regression guard**, not a
   red/green driver.
   *Real Enter-key activation is a browser behaviour this harness cannot
   exercise — it belongs to manual/e2e verification, and the design must not
   claim unit coverage of it.*
7. **R7 — single-select exclusivity in the DOM.** On a question with
   options: type into its input, then click an option — the input's value
   clears and the input disables (the pick locks the question); submit
   ships that question as the option id with `customText: ""`. The
   invariant under test is the server rule: no sequence of clicks and
   typing on a single-select may produce a respond carrying both an option
   id and non-empty text. RED: no input exists.
8. **R8 — `header` renders as a chip only when present.** A question with
   `header` set shows `.ask-header` with its text above `.ask-question`; a
   question without one renders no `.ask-header` node. RED.
9. **R9 — `recommended` marks, never selects, never crashes.** A valid
   index adds the `recommended` class and the `.ask-option-rec` badge to
   exactly that option, and nothing is pre-selected (no `chosen` class, the
   submit control still disabled on the untouched ask). An out-of-range
   index (e.g. `7` on a two-option question) renders no marking and throws
   nothing — the block still mounts. RED for the valid half; the invalid
   half is a regression guard.

**`apps/ui/src/live/adapt.test.ts`** — the wire→domain mapping:

1. **A1 — `adaptAskQuestion` maps the widened shape.** `customText` and
   `header` verbatim (empty and non-empty); `recommended` present and unset
   (`undefined` survives the map); `AskOption.preview` present, and `""` →
   `undefined` per the `description` convention. RED: all are dropped
   today. (The prior draft also asserted "still drops
   `header`/`recommended`/`timedOut`" — dropped; only `timedOut` remains
   unmapped, and under TS strict a domain field that does not exist cannot
   be asserted absent except by a near-tautological runtime probe.)

Store-level `answerAskText` no-op gates (settled single-select, submitted
ask, closed ask) belong beside the existing `answerAsk` gate coverage in
`store.live.test.ts`; they are RED trivially (the method does not exist).

#### Inversions and edits to shipped tests, by file

**`apps/ui/src/store.live.test.ts`:**

- `answerAsk sends nothing until the ask is complete, then one full
  RespondToAsk` (`:336`) — leg (b) asserts the completing click ships the
  respond (`:354-365`). **Inverts:** the completing click now also asserts
  `askResponses` empty; an appended `submitAsk` carries the send assertion.
  Rename accordingly (`answerAsk never sends; submitAsk ships the full
  respond`).
- `a single-question ask still sends on its only click` (`:373`) —
  **inverts wholesale** (this is L2's subject): the click sends nothing;
  submit sends. Its comment ("the gate must not regress the ONE-question
  ask") pins exactly the behaviour Matt removed.
- `submitAsk sends an incomplete ask with the skipped question empty`
  (`:401`) — **survives unchanged**; its trigger is already the explicit
  path.
- `a completed ask takes no further answer and issues no second respond`
  (`:435`) — the send trigger is two completing clicks (`:445-448`).
  **Edit:** insert a `submitAsk` to issue the one respond; the
  further-answer/further-submit inertness legs survive.
- `a refused RespondToAsk rolls the local answer back and stays retryable`
  (`:467`) — **inverts in meaning:** the send becomes click + `submitAsk`,
  and the "local record does NOT show the refused answer" leg (`:488`)
  inverts — submit records nothing, so a refusal leaves the *staged answer
  in place* (still shown, still retryable). The re-answer/re-send tail
  becomes a plain re-submit.
- `a refused respond does not clobber an ask the stream moved meanwhile`
  (`:517`) — send gesture becomes submit; the contract **survives** (the
  D0 restage declines: the mid-flight push carries an accepted — answered —
  value, and `!current.answered` blocks the restage exactly as it blocked
  the rollback).
- `a rejected single-select answer does not reach the wire` (`:581`) — send
  trigger (the completing q-2 click, `:593`) becomes click + submit; the
  first-responder-wins wire assertion survives.
- `an unknown ask coordinate sends no RespondToAsk` (`:605`) — survives
  textually, but its send-side assertion becomes vacuous (no click ever
  sends now); keep it as records-nothing coverage, with the comment updated.

**`apps/ui/src/store.ask-race.test.ts`** (the suite carrying 8 `askResponses`
assertions):

- `an unsubmitted local answer survives a stream push` (`:131`) — the "still
  completable" leg (`:163-173`) sends via the completing click. **Edit:**
  click then `submitAsk`; the survives-the-push assertions stand.
- `an authoritative server answer beats an unsubmitted local one` (`:186`) —
  no send trigger; **survives**.
- `a shipped ask takes the pushed server value` (`:225`) — ships via a
  single-question ask's only click (`:233-236`). **Edit:** click + submit.
- `a fully-skipped answered ask beats an unsubmitted local one` (`:263`) —
  **survives** (asserts `askResponses` empty after a first click, which the
  new model satisfies a fortiori).
- `a custom-text-only answered ask beats an unsubmitted local one` (`:302`)
  — **survives**; S2 extends it.
- `an unanswered pushed ask still preserves the local answer` (`:343`) — the
  "real state, not a ghost" leg (`:369-380`) sends via the completing click.
  **Edit:** click + submit.
- `a click on a server-closed ask ships nothing` (`:394`) — **survives**;
  its comment (the click "would ship the doomed respond") updates — the
  guarded contract is now the *record* refusal (`chosenIn` stays empty), the
  wire leg being vacuous.
- `a submit on a server-closed ask ships nothing` (`:434`) — **survives
  unchanged**, and gains weight: submit is now the only send path this gate
  defends.
- `a pushed ask that grew a question` (`:475`), `…renamed a question`
  (`:512`), `a push over an untouched ask is adopted by reference` (`:551`),
  `…revised its options` (`:670`) — no send trigger; **survive**.
- `a refusal after the server accepted does not reopen the closed ask`
  (`:601`) — the held-in-flight send is a single-question click (`:612`).
  **Edit:** click + submit; the declined-restage behaviour is identical to
  the declined rollback it pins (guarded by `answered` either way).

**`apps/ui/src/components/ChannelView.ask.test.tsx`:**

- `the first click sends nothing; the completing click sends one full
  respond` (`:103`) — second leg **inverts** (R4's subject): the completing
  click asserts empty; the submit control's click carries the send
  assertion.
- `the submit control appears only on a partially answered ask` (`:138`) —
  **inverts wholesale** (R5's subject): the control now renders on every
  live ask; the visibility legs become the disabled/enabled/copy legs.
- `submitting a partially answered ask ships the skipped question empty`
  (`:162`) — **survives**; already submit-triggered.
- `a refused respond renders in the ask block and clears on the next answer`
  (`:199`) — the send is two completing clicks (`:206-207`). **Edit:**
  clicks + submit-control click; the rollback-shaped leg (`:211-213`, "q-2
  can be answered again") relaxes — with no rollback, the staged q-2 answer
  survives the refusal, and the clears-on-next-answer leg re-answers or
  re-submits.
- `a server-closed ask renders locked with no submit control` (`:237`) —
  **survives**: the no-control assertion now rides the `!closed()`
  visibility gate rather than the partial-answer window, same observable.

**`apps/ui/src/store.test.ts`** — the `answerAsk` describe (`:1750-1841`)
runs against the *offline* store (`withStore`, no comms client) and asserts
only local recording (first-responder-wins, unknown-coordinate no-ops).
**Survives unchanged** — nothing in it observes a send.

**`apps/ui/src/comms.test.ts`** — references `answerAsk` only in a fixture
comment ("the answerAsk MUTATION path lives in store.test.ts and is not
restated here", `:804-806`). **Unaffected.**

**`apps/ui/src/components/RightSidebar.fleetpane.test.tsx`** — references
`answerAsk` only in the suite's header comment (`:26-27`); the ask test
drills into a topic and asserts routing, not sends. **Unaffected** (comment
mention only).

**`apps/ui/src/components/ChannelView.test.tsx`** — mounts the ask surface
(`.ask-option` queries, `store.answerAsk`, the `locked`/`chosen` render)
against the *offline* store (`createAppStore({ initialComms:
STUB_COMMS_STATE })`, `:109-110`, `:359-360`), so it observes local
recording only and never a send. **Survives D0 unchanged** — it cannot pin
auto-send. T3 adds DOM beneath these mounts (the input on every question,
the new hint copy, the unconditional submit control) without changing the
`.ask-option` queries or first-responder-wins, so no edit is expected;
listed because silence here would leave an implementer unsure whether a
break in it is intended.

## Plan

### Global Constraints

- TypeScript strict: no `any`, no `as`, no `@ts-ignore` (the one sanctioned
  `as unknown as CommsClient` seam in `comms-fake.ts:316` stays as-is).
- SolidJS v2 idioms only — no `produce`, `createResource`, `batch`,
  `solid-js/store`.
- No changes under `proto/`; the wire contract is consumed, not amended.
- The one-respond-per-ask invariant is untouchable — and under D0, stronger:
  no code path issues a `respondToAsk` except `submitAsk`.
- Comments: 1-2 lines, WHY not WHAT; do not imitate the long surrounding
  blocks; no planning metadata in source.
- Every new test must be RED before its slice lands and assert
  consumer-observable behaviour (DOM, `fake.askResponses`, store accessors) —
  never mock echoes.
- A respond never carries an option id and non-empty `customText` for a
  single-select question — the staged state keeps them exclusive (D2/D4)
  and the send seam enforces it structurally (D3).

### T0 — the send model (prerequisite slice; touches shipped behaviour)

`apps/ui/src/store.ts`:

- Remove `answerAsk`'s send tail and `before` capture; write the D0
  replacement comment.
- Delete `isAskComplete` and `sameAnswers`.
- `sendAsk(messageId, ask)`: drop the `rollback` parameter; replace the catch
  rollback with the D0 restage (`current !== ask && !current.answered &&
  sameQuestions(current, ask)` → `putAsk(messageId, ask)`); rewrite the
  header comment block accordingly.
- Rewrite `submitAsk`'s header comment (D0 text); gates unchanged this slice
  (the guard's predicate swap lands in T2 with `isQuestionAnswered`).

`apps/ui/src/components/ChannelView.tsx`:

- Rewrite the `AskBlock` doc comment (D0 text).
- Submit control: `<Show when={!closed()}>`, `disabled` on
  `answeredCount() === 0`, label switch per D5 (this slice still on the
  chosen-ids `answeredCount`; T3 swaps in `isQuestionAnswered`). Keep
  `type="button"`, add no `onKeyDown`, and introduce no wrapping `<form>` —
  D5's keyboard contract depends on the native button activation this
  preserves.

Tests: L2 and R6 (new), S3a (the clicks-only restage guard — the mechanism
ships here, so its field-agnostic half is testable here), plus every
inversion/edit in D7's enumeration for `store.live.test.ts`,
`store.ask-race.test.ts`, and `ChannelView.ask.test.tsx` (`:103`, `:138`,
`:199`, `:435`-analogue edits).

Interfaces:

- Consumes: nothing new.
- Produces: `submitAsk` as the sole caller of `sendAsk`; the restage refusal
  contract; the unconditional submit control's DOM (`.ask-submit` present on
  every live ask).

### T1 — domain shape + adapt mapping

`apps/ui/src/comms-stub.ts`: widen `AskQuestion` with `header: string`,
`recommended?: number`, and `customText: string` (after `chosenOptionIds`,
with the D1 draft/audit discriminant comment) and `AskOption` with
`preview?: string`; add the two exported predicates `isFreeTextQuestion` /
`isQuestionAnswered` (D2 bodies, verbatim). `apps/ui/src/live/adapt.ts`: map
`header`/`customText` verbatim, `recommended: w.recommended`, and
`preview: o.preview || undefined` in `adaptAskQuestion`; rewrite the drop
comment to name only `timedOut`. Fix every construction-site compile break
the widened interfaces cause (fixture/stub ask builders gain `header: ""`,
`customText: ""`).

Interfaces:

- Consumes: `WireAskQuestion.header`/`.recommended`/`.customText` and the
  wire option's `preview` (generated, already present).
- Produces: `AskQuestion.header: string`; `AskQuestion.recommended?: number`;
  `AskQuestion.customText: string`; `AskOption.preview?: string`;
  `isFreeTextQuestion(q: AskQuestion): boolean`;
  `isQuestionAnswered(q: AskQuestion): boolean`.

Tests: D7 case A1 in `live/adapt.test.ts` (the widened shape); predicate
unit cases (option answered, text answered on an option question,
whitespace text, option-less question) beside the other comms-stub
consumers in `store.test.ts` or a small direct describe — boundary-owning:
the `trim()` and the `options.length === 0` discriminant each have a case
that reddens if removed.

### T2 — store: record, preserve, ship

`apps/ui/src/store.ts`:

- `answerQuestionText(q, text)` reducer + `answerAskText` action (D2
  signature and gates, verbatim — no-op on a settled single-select),
  exported on `AppStore` beside `answerAsk`.
- `answerQuestion` clears `customText` on a single-select pick (D2's click
  half of the exclusivity rule).
- `submitAsk` guard → `!ask.questions.some(isQuestionAnswered)`.
- `preserveLocalAsks` unshipped-edit scan → `q.chosenOptionIds.length === 0
  && q.customText === ""`.
- `sendAsk` answers map gains the D3 `customText` line — `""` on a
  single-select holding a chosen option, `q.customText.trim()` otherwise
  (structural exclusivity at the send seam).

`apps/ui/src/live/comms-fake.ts`: widen `respondToAsk`'s request type and the
`askResponses` copy with `customText: string` — the prerequisite for every
wire-payload assertion on text (L1's payload half, R2, L3).

Interfaces:

- Consumes: T1's field + predicates; T0's single send path.
- Produces: `AppStore.answerAskText(messageId: string, askId: string,
  questionId: string, text: string): void`; `fake.askResponses` entries carry
  `customText: string`.

Tests: L1, L3, L4, and L5 in `store.live.test.ts` (plus the `answerAskText`
gate no-ops); S1, S2, and S3b in `store.ask-race.test.ts`.

### T3 — renderer + completion accounting

`apps/ui/src/components/ChannelView.tsx` (`AskBlock`): the unconditional
free-text input on every question (D4 binding, `aria-label`,
`disabled={locked(q())}`, per-shape placeholder), the three-state hint
line, the `header` chip, the `recommended` marking with its bounds guard,
and the `preview` block (D6); `answeredCount` moves to `isQuestionAnswered`
so the submit control (T0) counts text. `apps/ui/src/app.css`: `.ask-text`,
`.ask-header`, `.ask-option-rec`, and `.ask-option-preview` beside the
`.ask-option` family.

Interfaces:

- Consumes: T1's predicates, T2's `answerAskText`, T0's submit control.
- Produces: the `.ask-text` / `.ask-header` / `.ask-option-rec` /
  `.ask-option-preview` DOM contract the tests select on; no exported API.

Tests: D7 cases R1-R9 in `ChannelView.ask.test.tsx` (R4/R5 land their final
form here; their inversion halves are already green from T0). R6 — the
keyboard submit guard — lands with T0's control and stays green from there.

Task order is T0 → T1 → T2 → T3 (each consumes the previous slice's
exports); T0's inversions and T1's adapt/predicate tests are green at the end
of their slices, the rest stay RED until theirs.

## Tasks

- [ ] T0: remove auto-send; `submitAsk` sole path; restage refusal; delete
  `isAskComplete`/`sameAnswers`; unconditional submit control; comment
  rewrites — `store.ts`, `components/ChannelView.tsx`; inversions in
  `store.live.test.ts`, `store.ask-race.test.ts`, `ChannelView.ask.test.tsx`
- [ ] T1: widen `AskQuestion` (`header`, `recommended`, `customText`) +
  `AskOption.preview` + predicates + adapt mapping — `comms-stub.ts`,
  `live/adapt.ts`, tests in `live/adapt.test.ts`
- [ ] T2: `answerAskText` + exclusivity recorders + widened
  preserve/guard/ship + fake recorder — `store.ts`, `live/comms-fake.ts`,
  tests in `store.live.test.ts` + `store.ask-race.test.ts`
- [ ] T3: `AskBlock` free-text input on every question +
  header/recommended/preview rendering + text-aware `answeredCount` + CSS —
  `components/ChannelView.tsx`, `app.css`, tests in
  `ChannelView.ask.test.tsx`

## Open Questions

1. **Enter-in-the-input-field.** (Not load-bearing — and no longer about
   whether the keyboard can submit at all: Enter on the focused submit
   control is a **decided contract**, D5's keyboard bullet, with a test.
   What stays declined is only the *shortcut* of Enter while typing in a
   question's field, which would ship the whole ask from a key that means
   "commit this field" everywhere else.) Revisit only if users report the
   tab-to-submit trip is friction.
2. **Multi-line growth.** (Not load-bearing.) Single-line per D4's reading of
   the proto's "Other" framing. If agents start asking prose-shaped free-text
   questions, a `textarea` swap is contained to the D4 branch and its CSS.
3. **Per-answer note + "Chat about this" — settled at the contract layer,
   not open here.** The native tool's remaining two affordances have no
   wire carrier, and that is a decided contract ruling, not a gap this
   record found: the merged derivation record
   (`docs/designs/agent/compass-ask-typed-derivation.md` § "Axis carriers
   (native → reshaped)") drops both deliberately — `QuestionResult.note` is
   "omitted now; `string note = 4` on `AskQuestionAnswer` is a non-breaking
   addition if RIG-1310 finds it needed", and `chatRedirect` / the "Chat
   about this" label is "moot in Compass, which has first-class chat; not
   modeled". In Compass a note is a channel reply, and "chat about this" is
   the channel itself. Nothing for this record to decide; recorded so
   nobody re-discovers the absence as a UI bug.
