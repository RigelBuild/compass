# Compass ADE — Ask-in-channel (first-responder-wins)

Status: Active

Sibling to the frozen 0.7 record
(`../compass-0.7-channel-workspace/design.md`, merged) and the pending
`../compass-threading-ui/design.md`. Scope: the compass-ui stub +
store seam only — the "dumb multi-user ask" Matt ruled for the MVP.

## Problem / Intent

The standalone channel surface renders EVERY ask read-only —
`App.tsx:116` mounts `<ChannelView readonlyAsks />` — so a question an
agent posts to a channel cannot be answered where it was asked; members
are bounced to the owning agent's workspace via an "answer in @X's
workspace" hint. Matt's ruling (2026-07-20): asks should be answerable
**wherever they are** — any channel, a group DM, the agent workspace, the
board fleet pane — with no rerouting. First-responder-wins is the sole
settlement: the first answer locks the ask for everyone.

First-responder-wins is the backend contract that landed with PR #810
(RIG-1243, merged 2026-07-20): it reshapes
`Ask` into per-question `Ask{AskID, Questions[]}`, makes `RespondToAsk`
take `[]AskQuestionAnswer` keyed by `question_id` (every question covered exactly
once), and adds a PERSISTED `Ask.Answered` flag — settlement is
deliberately NOT derived from chosen options, so even a fully-skipped ask
rejects a re-answer with `CodeAlreadyExists` under a `FOR UPDATE` row
lock. This record delivers the fixture-era analog in the UI: asks answer
in place; a settled ask locks to every viewer.

## Global Constraints

- Package `apps/ui` — SolidJS + TS, Vite, Bun test, Biome.
  No new dependencies.
- Fixture→daemon seam: UI reads ONLY AppStore accessors/actions; asks
  are answered exclusively through `store.answerAsk` (the fixture-era
  analog of the daemon's `AnswerAsk`/`RespondToAsk` —
  `store.ts:313` "Becomes a RespondToAsk call when the stream lands").
  Never mutate fixtures directly.
- Red-first: each task's tests land and fail before the implementation.
- Backend contract pinned to #810 (merged): first-wins at the ASK
  level via a persisted `Answered` flag; no responder-identity field is
  persisted, so the UI can show the winning option but never "answered by
  @X". A #810 revision is an explicit invalidation trigger for this record.

## Approach

### What exists today

**The stub models the PRE-#810 flat, single-question ask** — no answered
flag, no responder field; settlement is derived from `chosenOptionIds`
(`comms-stub.ts:121-130`). #810's persisted `Answered` flag and the widened
`[]AskQuestionAnswer` transport stay deferred to the `@compass/client` swap; the
per-question structural shape, by contrast, this record absorbs into the stub
now (OQ4=B, #821). Today's flat shape:

```ts
export interface Ask {
  /** Correlation id echoed by RespondToAsk (comms.proto Ask.ask_id). */
  askId: string;
  question: string;
  options: AskOption[];
  /** Whether more than one option may be chosen. */
  allowMultiple: boolean;
  /** The chosen option ids once answered; empty while pending (kept for audit). */
  chosenOptionIds: string[];
}
```

**AskBlock already locks a settled single-select** — the render lock
needs no work (`ChannelView.tsx:94`, applied at `:118`):

```ts
const locked = () => !ask().allowMultiple && ask().chosenOptionIds.length > 0;
```

```tsx
disabled={props.disabled || locked()}
```

`locked()` renders a settled single-select's options disabled to EVERY
viewer — exactly the first-wins render once the store makes the settled
state stick (T1). A multi-select ask stays toggleable (the predicate
excludes `allowMultiple`).

**The read-only gate is the `readonlyAsks` prop** — the thing this record
removes. It threads `ChannelView → ThreadView → MessageRow → Block →
AskBlock` (`ChannelView.tsx:310,206,173,149`, drilled at
`:352,213,223,191,161`), lands as AskBlock's `disabled` prop
(`ChannelView.tsx:161`), and renders the ask fully inert with a routing
hint (`ChannelView.tsx:133-137`):

```tsx
<Show when={props.disabled}>
  <div class="ask-readonly-hint">
    answer in @{authorHandle()}'s workspace
  </div>
</Show>
```

**`answerAsk` is the store seam** (`store.ts:668-688`): a functional
`setMessages` update; single-select sets `chosenOptionIds = [optionId]`,
multi-select toggles (`store.ts:678-682`):

```ts
const chosen = ask.allowMultiple
  ? ask.chosenOptionIds.includes(optionId)
    ? ask.chosenOptionIds.filter((id) => id !== optionId)
    : [...ask.chosenOptionIds, optionId]
  : [optionId];
```

Note: today a second single-select answer REPLACES the first —
`store.test.ts:1342-1360` pins it ("a second answer REPLACES it rather
than appending"). That is the pre-first-wins contract this design
changes (see T1).

**Where `readonlyAsks` is passed today** — the standalone channel surface
mounts it blanket-read-only (`App.tsx:115-117`):

```tsx
<Match when={store.view() === "channel"}>
  <ChannelView readonlyAsks />
</Match>
```

The agent-workspace chat pane passes no prop and is interactive
(`AgentView.tsx:114`, `<ChannelView channel={store.workspaceChannel()} />`).
In-flight PR #813 adds a THIRD mount — the board fleet pane —
`<ChannelView channel={homeDm()} readonlyAsks />` (`RightSidebar.tsx:401`);
under the kill-the-gate ruling #813 drops that `readonlyAsks` (its home DMs
are text-only in the fixture, so the fleet pane asks-answerable path is
tested by a fixture ask added there — see #813's M2).

### The designed change

Two moves — the render lock (`locked()`) is untouched and becomes the
first-wins render surface:

1. **Remove the read-only gate.** Delete the `readonlyAsks` prop and its
   entire drill chain: the prop declarations
   (`ChannelView.tsx:310,206,173,149`), the drill sites
   (`:352,213,223,191`), AskBlock's `disabled` prop and its two uses
   (`:161` pass-through, `:118` `disabled={props.disabled || locked()}` →
   `disabled={locked()}`, `:120` `if (props.disabled) return;` removed),
   and the readonly hint block (`:133-137`, plus the now-unused
   `authorHandle()` `:96` if nothing else references it) and its
   `.ask-readonly-hint` CSS. Delete the `readonlyAsks` usage at
   `App.tsx:116` (`<ChannelView />`). Every `ChannelView` mount then
   renders asks live; there is no routing and no read-only mount. #813's
   fleet-pane `readonlyAsks` (`RightSidebar.tsx:401`) is dropped in that
   PR (its M2 asserts the home-DM ask renders answerable).
2. **First-wins guard (store seam)** — the SOLE settlement. `answerAsk`
   gains a settled-check: a single-select ask with
   `chosenOptionIds.length > 0` is already settled, so a further answer is
   a no-op. The guard lands INSIDE the block mapper (a `return b;` beside
   the existing option-miss guard at `store.ts:677`, NOT at function top —
   the settled-check needs the ask in scope). It pins the SEAM contract the
   `@compass/client` swap depends on: the seam must already treat a second
   single-select answer as a no-op so UI behavior matches
   `CodeAlreadyExists` without a rewrite. (The stub has no cross-client
   state — the only race it can exhibit in one client is a programmatic
   double-call or a stale-render double-click; the two-member race is the
   POST-SWAP scenario the seam is being shaped for.) Fidelity gap: the
   no-op models the client outcome (losing clicker re-renders the winner),
   but the backend returns `CodeAlreadyExists`; at the swap the seam may
   want a `boolean`/`Result` return so a caller can surface a "someone
   answered first" signal — parked for the swap. The
   `store.test.ts:1342-1360` replacement contract is deliberately inverted
   (red-first, T1). Multi-select behavior is a resolved decision (OQ2).

Settled state for a viewer who didn't answer: the existing `answered`
class + `chosen` highlight (`ChannelView.tsx:105,117`) show the winning
option; no attributor is possible (no responder field, OQ3).

### Alternatives considered

- **Keep a gate, refine it to route DM asks to the workspace** (the prior
  draft of this record) — rejected by Matt's ruling: asks answer wherever
  they are, no rerouting. Removing the gate is strictly simpler than any
  refinement of it (deletes a prop + drill chain rather than adding a
  predicate).
- **Membership-gated answerability** (answerable only where the caller is a
  member; observation-only elsewhere) — rejected for the MVP stub: channel
  membership is a backend gate (`messages.go:286-291`: `JOIN
  channel_members … account_id = $1`; zero rows → `ErrNotFound`), the stub
  has no membership check in `answerAsk`, and it is moot in the current
  fixture (every channel a member can see, they are a party to). Net-new
  stub scope with no fixture that exercises it. The daemon enforces it at
  the swap.
- **A responder-identity (`answeredBy`) field on Ask** — rejected: no such
  field exists in the stub or in #810's reshape. Net-new backend scope,
  out of MVP. (#810 DOES add a persisted `Answered` flag precisely because
  settlement is NOT derivable from chosen options — an answered-by-skip ask
  leaves no per-question trace yet must still reject a re-answer. The stub's
  derived-settlement model cannot represent skip; that gap closes at the
  client swap — OQ2.)
- **Skip the store guard, rely on `locked()`** — rejected: the lock is
  a per-client render guard, not a seam contract. When the generated
  `@compass/client` replaces the fixture, the seam must already treat
  a second single-select answer as a no-op so the UI behavior matches
  `CodeAlreadyExists` handling without a rewrite.

## Plan

### T1 — first-wins guard in `answerAsk` (red-first)

Invert the second-answer contract in `store.test.ts` (currently
`store.test.ts:1342-1360`: second single-select answer replaces), then
implement.

- Red: rewrite the "REPLACES" test to assert a second single-select
  answer is a no-op (winner's `chosenOptionIds` unchanged); keep the
  no-op tests for unknown message/ask/option ids green.
- Green: settled-check in `answerAsk`'s block mapper (a `return b;` beside
  the option-miss guard at `store.ts:677`, NOT function top — the ask must
  be in scope) when `!ask.allowMultiple && ask.chosenOptionIds.length > 0`.
- Interfaces: `answerAsk(messageId: string, askId: string, optionId:
  string): void` — signature unchanged; behavior contract narrows.
- Multi-select: unchanged (still toggles) — OQ2 resolves multi-select as a
  documented divergence for the MVP.

### T2 — remove the read-only gate (red-first)

- Red: extend `ChannelView.test.tsx` — the standalone channel mount with a
  channel carrying an ask: options enabled, a click records via
  `answerAsk`, NO readonly hint anywhere. Reuse the existing harness
  (`ChannelView.test.tsx:105-107` spread-cast mount). Add a leg asserting
  a settled single-select renders every option disabled (`locked()`) with
  the winning option `chosen`.
- Green: delete the `readonlyAsks` prop and drill chain
  (`ChannelView.tsx:310,206,173,149,352,213,223,191,161`), AskBlock's
  `disabled` prop and its uses (`:118` → `disabled={locked()}`, drop the
  `:120` early-return), the hint block (`:133-137`) + `.ask-readonly-hint`
  CSS, and the `App.tsx:116` usage (`<ChannelView />`). Remove
  `authorHandle()` (`:96`) if unreferenced after the hint goes.
- Interfaces: `ChannelView` (and `ThreadView`/`MessageRow`/`Block`/
  `AskBlock`) lose the `readonlyAsks?: boolean` prop entirely; `AskBlock`
  loses `disabled`.
- Cross-PR: #813 (`RightSidebar.tsx:401`) drops its `readonlyAsks` and its
  M2 asserts the fleet-pane home-DM ask renders answerable. If #813 merges
  first, this T2 rebases onto a tree where the fleet mount already passes no
  prop; if this merges first, #813's mount is already prop-free. Either
  order is clean — both just stop passing a prop that no longer exists.

### T3 — settled-state render + doc sweep

- Red (render half): a settled single-select ask on the standalone surface
  renders all options disabled (`locked()`), winning option `chosen`.
- Red (interaction half — the loop the store guard exists for): a click on
  a locked option does NOT call `answerAsk` and does not mutate
  `chosenOptionIds` (the store no-op from T1 backs the render lock).
- Green: expected to pass from T1+T2 alone (the lock already exists); any
  red here is a real bug.
- Sweep the blanket-read-only prose now stale: `App.test.tsx:19` comment
  block, `ChannelView.test.tsx:14-18` header prose, AND
  `ChannelView.test.tsx:27-33` fixture ground-truth prose (pins
  `ask-s4-integration` as read-only-hinted — now answerable). If #813
  merges first, `RightSidebar.fleetpane.test.tsx`'s inertness framing joins
  the sweep. Gate: `bunx biome check` + `bun test` on the package.

## Tasks

- [ ] T1: red — invert second-single-select-answer test to no-op
- [ ] T1: green — first-wins early return in `answerAsk`
- [ ] T2: red — asks answerable on the standalone mount, no hint
- [ ] T2: green — delete `readonlyAsks` prop + drill + hint + `App.tsx` usage
- [ ] T3: settled-state render test + stale-comment sweep + full gate
      (`bun test`, `bunx biome check`)

## Open Questions

### OQ1 (RESOLVED — Matt, 2026-07-20): where are asks answerable?

**Everywhere; no rerouting.** Matt ruled asks answer wherever they are —
any channel, group DM, workspace, fleet pane — with first-responder-wins
as settlement. This supersedes the prior draft's DM-vs-channel gate
boundary entirely: there is no gate, so the boundary question is moot.
Membership-based answerability is a backend concern (out of stub scope;
moot in the fixture, see Alternatives).

### OQ2 (RESOLVED): multi-select under first-wins

The prior draft's option C (route multi-select asks to the workspace)
depended on the gate this record removes, so it is no longer available.
The remaining options are: **A** — single-select gets first-wins,
multi-select keeps today's toggle behavior in the stub (a "dumb"
multi-user toggle: any member can toggle any member's choices, no lock);
**B** — lock multi-select on first answer too, which needs a commit-step
UX the current click-mutates-immediately UI lacks.

**Resolved: A**, forced by the kill-the-gate ruling and matching Matt's
"dumb multi-user ask" MVP framing. Single-select first-wins ships; the
multi-select toggle is a documented divergence from the backend
(ask-level answer-once) that closes at the `@compass/client` swap. B is a
real UX addition deferred to a follow-up. (Flagged in the PR body for
confirmation; A is the MVP-simplest and downstream-forced, not a fresh
design fork.)

### OQ3 (non-load-bearing, park): settled state with no attributor

No responder identity is persisted (#810 gates visibility on the
answering account but stores no field), so the settled ask reads
"answered: winning option highlighted" with no "answered by @X".
Acceptable for MVP; responder identity is net-new backend scope — park as
a follow-up, does not block T1-T3.

### OQ4 (RESOLVED — Matt, 2026-07-20): absorb #810's per-question Ask shape now, or at the swap?

PR #810 reshapes `Ask` from the stub's flat single-question shape into
per-question `Ask{AskID, Questions[]}` / `AskQuestion{QuestionID, Options[],
AllowMultiple}`, with `RespondToAsk` taking `[]AskQuestionAnswer` keyed by
`question_id`. The stub's `answerAsk(messageId, askId, optionId)` seam cannot
carry a per-question `[]AskQuestionAnswer` — so at the `@compass/client` swap the seam
SIGNATURE widens, not just its transport.

- **A — keep the flat single-question stub now; migrate the seam at the
  swap.** Smaller diff today; the stub stays faithful to the pre-#810 UI.
  Cost: a known SECOND migration of this exact seam (signature + every
  caller) when the client lands.
- **B — absorb the per-question shape into the stub `Ask` type now.** One
  migration instead of two; the stub leads the backend. Cost: net-new stub
  scope (per-question rendering, `[]AskQuestionAnswer` accumulation) the current
  single-question fixtures don't exercise — arguably the over-scoping this
  record avoids.

**Ruling: B** (Matt, 2026-07-20) — absorb the per-question shape into the stub
`Ask` type now. One migration instead of two; the stub leads the backend. This
reverses the record's prior draft recommendation (A). The structural
per-question decomposition lands in #821 as a behavior-preserving refactor,
ahead of T1-T3; the #810 affordances
that ride on top — free-text/`custom_text`, timeout, preview, and the atomic
`[]AskQuestionAnswer` transport — stay Non-goals, deferred to the `@compass/client` swap
where the seam signature widens (tracked in RIG-1330). The stub carries only
single-question fixtures today, so per-question rendering is exercised but
multi-question accumulation is not until the swap.

## Non-goals

- Polls, aggregation, vote-counting — deferred per Matt's MVP ruling.
- Responder identity ("answered by @X") — no backend field exists.
- Channel-membership answer gating — a backend concern; out of the stub.
- Any proto/Go change — first-wins landed with #810 (merged); this
  record is UI-only.
- Daemon wiring — this is the stub UX + store seam only. The generated
  `@compass/client` swap is out of scope, and it will WIDEN the `answerAsk`
  seam signature to per-question `[]AskQuestionAnswer` (OQ4), not merely swap the
  transport behind today's signature.
