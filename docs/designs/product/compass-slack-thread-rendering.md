# Compass Slack-model thread rendering (RIG-1352)

Status: Active

Tracking: RIG-1352. Parent (frozen):
`docs/designs/product/compass-0.8-threading-and-session-renderer/design.md`.
This record **supersedes by citation** that record's dual-render decision (see
Problem); the frozen record itself is never rewritten.

## Problem

The threading UI merged in #841 renders a thread's reply bodies in **two places
at once** — inline-indented in the main channel stream AND in the side
`ThreadPanel` — which matches neither Slack nor Discord. Matt ruled: use the
**Slack model** — the main stream shows only the root message plus a compact
thread-summary affordance; reply bodies live exclusively in the panel.

Where the duplication lives today (all cites re-verified against `main`):

- `ThreadView` (`apps/ui/src/components/ChannelView.tsx:204-241`)
  renders the root, a bare `reply` button, and then all reply bodies inline.
  The inline block, `ChannelView.tsx:226-238`:

  ```tsx
  <Show when={props.thread.replies.length > 0}>
      <div class="thread-replies">
          <Index each={props.thread.replies}>
              {(reply) => (
                  <MessageRow
                      msg={reply()}
                      byId={props.byId}
                      byHandle={props.byHandle}
                  />
              )}
          </Index>
      </div>
  </Show>
  ```

- The indent styling is `.thread-replies` (`app.css:2958-2967`): "A thread:
  root message + indented replies under it." with `margin-left: 20px;
  padding-left: 12px; border-left: 2px solid var(--border)`.
- `ChannelView` also mounts the panel (`ChannelView.tsx:371-375`:
  `<ThreadPanel channel={chan()} byId={byId()} byHandle={byHandle()} />`),
  which renders the SAME replies again — `ThreadPanel.tsx:82-88` reuses
  `MessageRow` under its own `.thread-replies` div, and its doc comment
  (`ThreadPanel.tsx:46-48`) states the duplication as intended: "reuses
  MessageRow and `.thread-replies` so a posted reply appears in both the panel
  and the main stream".

The dual render was **specified deliberately** in the frozen RIG-1337 record:
`design.md:110-112` ("`ThreadView` … renders root + replies under
`.thread-replies`") and `design.md:372-374` ("composer posts a reply that
appears in-panel AND indented in the stream"). This record supersedes exactly
that decision; everything else in the frozen record stands.

## Approach

**Slack model.** Per thread, the main stream (`.conv-stream`) renders the root
message plus ONE compact affordance; reply bodies render only inside
`ThreadPanel`.

### The affordance shape

`ThreadView` (`ChannelView.tsx:204-241`) changes as follows:

- **Removed:** the inline `<Show>`/`.thread-replies` block
  (`ChannelView.tsx:226-238`). Reply `MessageRow`s never render in the main
  stream again.
- **Added:** a `.thread-summary` button, rendered only when
  `props.thread.replies.length > 0`, sitting as a direct child of `.thread`
  immediately after `.thread-root` (exactly where the inline block sat — fork
  3, a clear call: the summary reads as "this root has a conversation under
  it", so it belongs under the root row inside the thread group). It shows,
  Slack-style:
  - **reply count** — `"1 reply"` / `"N replies"`;
  - **participants** — the distinct reply authors as placeholder-initial
    avatar badges, in first-reply order: each badge shows the handle's first
    character uppercased (`handleOf` (`comms.ts:24-29`) — the codebase's one
    resolution convention, so the initial can't drift from `MessageRow`'s
    `.msg-role`), carrying the full `@handle` in `title`/`aria-label`. The
    pile is capped at 5 badges; a `+N` overflow node follows when there are
    more than 5 participants (Slack's faces-pile shape). Real photo faces stay
    deferred until an avatar pipeline exists — `.thread-summary-people` is the
    drop-in seam;
  - **last-reply time** — `hhmm(lastReplyAtUnixMs)` via the existing
    deterministic-UTC formatter (`ChannelView.tsx:26-31`), rendered as
    `"last HH:MM"`.
- **Click:** `store.openThread(props.thread.root.id)` — the identical call the
  current bare `reply` button makes (`ChannelView.tsx:218-224`), driving the
  unchanged T-T1 store API (`store.ts:325-331`: `openThreadRootId` /
  `openThread` / `closeThread`; `postReply` below them).
- **Zero-reply roots** (fork 2, a clear call): the existing `.thread-reply`
  "reply" button (`ChannelView.tsx:218-224`) is KEPT but gated to
  `replies.length === 0`. A thread must stay *startable* — dropping the button
  outright would regress thread creation, and rendering it alongside the
  summary would be redundant (both call `openThread` on the same root). So
  each root row carries exactly one affordance: `reply` when the thread is
  empty, `.thread-summary` once it has replies.

The count/participants/last-time derivation is extracted into a pure, unit-
tested helper `threadSummary` in `comms.ts` beside `Thread`/`threadsOf`
(`comms.ts:153-156`, `comms.ts:195-219`) — see Plan T-1. It derives from the
existing `Message` fields only (`comms-stub.ts:154-168`: `id`, `channelId`,
`authorAccountId`, `atUnixMs`, `parentMessageId?`, `blocks`); no model change.

### CSS

- New `.thread-summary` rules in `app.css` (compact pill/row: small font,
  `var(--text-faint)` base, accent on hover — visually descended from the
  existing `.thread-reply` rule at `app.css:3020-3033`).
- `.thread-replies` (`app.css:2958-2967`) **stays** — `ThreadPanel.tsx:83`
  still uses it inside the panel, where the indent is correct.
- `.thread-reply` (`app.css:3020-3033`) **stays** — still used by the
  zero-reply start-a-thread button.

### What is explicitly untouched

`ThreadPanel.tsx` (component, composer, close, membership gating), the T-T1
store API, `Thread`/`threadsOf`, `Message`/fixtures, and all of `compass.v1` /
transport / daemon. UI-only, fixture-backed walking skeleton.

## Alternatives considered

- **Discord sub-channel model** (a thread as a navigable sub-channel with its
  own view) — rejected: Matt ruled Slack model for RIG-1352. Also heavier: it
  needs routing/navigation state the walking skeleton doesn't have, vs. reusing
  the already-shipped `ThreadPanel` + `openThread` store seam unchanged.
- **Keep the inline dual render** (status quo per frozen `design.md:372-374`) —
  rejected: it double-prints every reply, scales the main stream with reply
  volume (defeating the point of threading), and matches no reference product.
  The panel already renders replies correctly.
- **Minimal summary ("N replies" only)** — rejected as the resting state:
  participants + last-reply time are derived from fields already in hand
  (`authorAccountId`, `atUnixMs`) at negligible render cost, and the richer
  form is the actual Slack shape Matt pointed at. The helper returns all three
  regardless, so thinning the display later is a markup-only change.
- **Real photo avatar faces for participants** — deferred: the skeleton has no
  avatar asset pipeline. Matt ruled placeholder-initial badges (the handle
  initial) as the resting state now; real photo faces become a drop-in inside
  `.thread-summary-people` when an avatar pipeline exists.
- **Keep the bare `reply` button alongside the summary at N>0** — rejected:
  two adjacent controls invoking the same `openThread(root.id)` is redundant
  chrome; the summary itself opens the panel whose composer replies.
- **Relative last-reply time** ("last reply 2h ago", Slack's actual copy) —
  rejected: relative time needs a wall-clock `now`, which the
  Deterministic-rendering constraint forbids for a fixture-pinned skeleton.
  Absolute `hhmm` is the forced, correct choice; a relative form would need an
  injected clock the skeleton doesn't have.
- **Unread / new-reply styling on the summary** — deferred, not designed: the
  skeleton has no read-state model (no `lastReadAt` on `Channel`/`Message`,
  `comms-stub.ts:154-168`), so there is no substrate. It lands later as an
  additive `classList` on the same `.thread-summary` node once a read model
  exists — no rework.

## Global Constraints

- **UI-only.** No `compass.v1` contract, transport, or daemon change; no
  `parentMessageId`/`Thread` model change (`comms.ts:153-156`,
  `comms-stub.ts:154-168` stay as-is). Fixture-backed walking skeleton.
- **`ThreadPanel` intact** (one comment exception). `components/ThreadPanel.tsx`
  and its `.thread-replies` usage (`ThreadPanel.tsx:83`) are untouched, EXCEPT
  the doc comment at `ThreadPanel.tsx:46-48` ("a posted reply appears in both
  the panel and the main stream") — which this change falsifies, so T-2 updates
  that one comment line to describe panel-only replies. Only the main-stream
  duplication is removed; the `.thread-replies` CSS rule stays.
- **Store API unchanged.** `openThread` / `openThreadRootId` / `closeThread` /
  `postReply` (`store.ts:325-335`) are consumed, never modified.
- **Red-first** (rule://red-green-testing): new assertions written and observed
  red before the component change.
- **Existing thread tests stay green** except the two `ThreadPanel.test.tsx`
  legs this design deliberately changes (audited in T-2 below); those are
  updated in the same red→green cycle, never deleted.
- TypeScript strict; `direnv exec ~/agents/workspaces/<codename>/compass moon run
  compass-ui:typecheck compass-ui:test` green; biome-clean.
- SolidJS conventions: `ThreadView` stays a `Component`; derived values are
  accessors, not precomputed constants.
- Deterministic rendering: times via the existing UTC `hhmm`
  (`ChannelView.tsx:26-31`); no locale- or wall-clock-dependent output.

## Plan

### T-1 — Pure thread-summary model helper

Extract the summary derivation into `comms.ts`, beside `Thread` / `threadsOf`
(`comms.ts:153-156`, `195-219`), so the numbers are unit-testable without a
DOM and the component stays markup-only.

Interfaces:

```ts
// comms.ts — new, exported

/** Derived stream-facing summary of a thread's replies. */
export interface ThreadSummary {
 /** replies.length. */
 replyCount: number;
 /** Distinct reply authorAccountIds, in first-reply order. */
 participantIds: string[];
 /** Max reply atUnixMs; 0 when replyCount === 0 (callers render the
  *  summary only when replyCount > 0). */
 lastReplyAtUnixMs: number;
}

export function threadSummary(thread: Thread): ThreadSummary;
```

Pure and allocation-light: one pass over `thread.replies` (a `Set` for
distinctness, running max for the time). No store, no component import.

Test cycle (red → green, in `comms.test.ts` beside the existing `threadsOf`
suite at `comms.test.ts:80-221`):

- zero replies → `{ replyCount: 0, participantIds: [], lastReplyAtUnixMs: 0 }`;
- N replies, duplicate author (fixture shape: `msg-c2` by `acc-livingstone`,
  `msg-c3` by `acc-cook`, root by `acc-cook`, `comms-stub.ts:320-356`) →
  `replyCount` correct, `participantIds` distinct in first-reply order,
  `lastReplyAtUnixMs` = the max reply `atUnixMs` (out-of-order reply times
  covered);
- root author appears in `participantIds` only if they also replied.

Run: `direnv exec ~/agents/workspaces/<codename>/compass moon run
compass-ui:test` (red first, then green), then `compass-ui:typecheck`.

### T-2 — `ThreadView` summary affordance + CSS + test migration

Rework `ThreadView` (`ChannelView.tsx:204-241`) to the Slack shape, add the
`.thread-summary` CSS, and migrate the two main-stream assertions in
`ThreadPanel.test.tsx` — one coherent red→green slice.

Interfaces:

- `ThreadView` props are **unchanged**
  (`{ thread: Thread; byId: Map<string, Account>; byHandle: Map<string, Account> }`,
  `ChannelView.tsx:204-208`); it keeps resolving the store via `useStore()`.
- Markup contract (what tests select):
  - `.thread` > `.thread-root` (root `MessageRow` — unchanged);
  - `button.thread-reply` ("reply") renders **only when**
    `thread.replies.length === 0`; click →
    `store.openThread(thread.root.id)` (unchanged handler,
    `ChannelView.tsx:221`);
  - `button.thread-summary` renders **only when**
    `thread.replies.length > 0`, as a direct child of `.thread` after
    `.thread-root`; click → `store.openThread(thread.root.id)`. Inside it:
    `.thread-summary-count` ("N replies", singular "1 reply"),
    `.thread-summary-people` (one `.thread-summary-avatar` badge per
    `participantIds` entry, in order: badge text is the handle's first char
    uppercased, resolved via `handleOf` (`comms.ts:24-29`) — the same helper
    `MessageRow` uses for `.msg-role` (`ChannelView.tsx:185-187`), so the
    initial can't drift from the codebase's one resolution convention — with
    the full `@handle` on each badge's `title` attribute (the `@` is
    prepended in `ThreadView` — `Account.handle` is stored bare, so
    `handleOf` returns e.g. `cook` and the title is `` `@${handle}` ``). The pile is capped
    at 5 badges from `participantIds.slice(0, 5)`; when
    `participantIds.length > 5` a trailing `.thread-summary-overflow` node
    renders `"+N"` (`N = participantIds.length - 5`). The cap is a view
    concern only — `threadSummary` still returns the full `participantIds`
    list, so no model change),
    `.thread-summary-time` (`last ${hhmm(lastReplyAtUnixMs)}`);
  - the inline `<Show>`/`.thread-replies` block (`ChannelView.tsx:226-238`) is
    **deleted**; no reply `MessageRow` ever renders under `.conv-stream`.
- CSS: new `.thread-summary` (+ `-count` / `-people` / `-time` / `-avatar` /
  `-overflow`) rules in `app.css` next to `.thread-reply`
  (`app.css:3020-3033`), same subtle-button vocabulary (`var(--text-faint)`,
  accent on hover, `font-size: 11px`, `align-self: flex-start`); the
  `.thread-summary-avatar` badge is a small square/circle initial chip and
  `.thread-summary-people` lays the badges in a row. `.thread-replies`
  (`app.css:2958-2967`) and `.thread-reply` rules untouched.

Existing-tests audit (verified in-repo). Enumerated by *rendering-surface
class* — every test selecting `.msg`, `.conv-stream`, or `.thread*`, not just
by thread-API name (which would miss reply-count assertions using generic
selectors) — so the affected set is `store.thread.test.ts`,
`ThreadPanel.test.tsx`, `ChannelView.test.tsx`,
`RightSidebar.fleetpane.test.tsx`, and `App.test.tsx`:

- `store.thread.test.ts` — **stays green** (all legs): pure store tests, no
  DOM (`store.thread.test.ts:64-128`).
- `ChannelView.test.tsx:221-255` ("…threads render identically") — **stays
  green**: it compares `.thread`/`.msg` counts between two mounts of the same
  channel (equality is render-shape-independent) and asserts
  `count(".msg") > 1`, which still holds — `ch-svc-compass` renders ≥ 2 root
  `MessageRow`s (`msg-c1` at `comms-stub.ts:320` and `msg-c4`, the zero-reply
  ask root, at `comms-stub.ts:358`).
- `RightSidebar.fleetpane.test.tsx:110-112` — **stays green**: it asserts
  `.conv-stream .msg` count === the channel's raw message count, the exact
  shape this change breaks for any channel with a threaded reply — but both
  fleet home-DMs are flat (the file notes this at :76-77; `msg-c2`/`msg-c3`
  are the fixture's only `parentMessageId`-bearing messages,
  `comms-stub.ts:336,349`), so the count is unchanged. Landmine only if a
  fixture DM is ever threaded.
- `App.test.tsx:146` — **stays green**: counts `.thread` (> 0); thread groups
  persist (only reply bodies leave the stream), so unaffected.
- `ThreadPanel.test.tsx` — six of eight legs **stay green**: "no thread panel
  by default" (:98), "panel shows root and replies" (:130), "close hides the
  panel" (:181), both composer-enablement legs (:199, :220), "switching
  channel removes the panel" (:265) — none touches the main-stream reply
  render. Two legs **change** (this design's intent, migrated red-first):
  - "reply affordance opens the panel" (:110-124) clicks `.thread-reply` on a
    root that HAS replies (`THREAD` requires `t.replies.length >= 1`,
    :47-64) — under this design that root carries `.thread-summary` instead.
    Migrate the selector to `.thread-summary`; same `openThreadRootId` +
    panel-appears assertions.
  - "panel composer posts a reply appearing in-panel and in the stream"
    (:146-176) asserts the posted text under `.conv-stream .thread-replies`
    (:170-175) — the exact behavior being removed. Migrate leg (b) to: the
    posted text does NOT appear under `.conv-stream`, and the root's
    `.thread-summary-count` reflects the incremented count. Rename the leg
    title too (e.g. "...appearing in-panel only, not in the stream") so the
    name matches the migrated assertion.

New red-first assertions (in `ThreadPanel.test.tsx` or a sibling
`ThreadView` describe block; written and observed red before the component
edit):

1. A thread with N > 0 replies renders `.thread-summary` inside its `.thread`,
   its `.thread-summary-count` text contains `"N replies"` (fixture: msg-c1's
   2 replies → `"2 replies"`), and **no** reply body renders in the stream:
   `container.querySelectorAll(".conv-stream .thread-replies").length === 0`
   and the reply fixture text (`THREAD.replyText`) is absent from
   `.conv-stream`'s textContent while present in the open panel.
2. Clicking `.thread-summary` sets `store.openThreadRootId()` to the root id
   and mounts `.thread-panel`.
3. A zero-reply root (fixture: `msg-c4`, no `parentMessageId` children) still
   renders `button.thread-reply`, and clicking it opens the panel — a thread
   remains startable.
4. A root with replies does NOT render `button.thread-reply` (exactly one
   affordance per root).
5. `.thread-summary-people` renders one `.thread-summary-avatar` badge per
   distinct reply author (badge text = handle initial, full `@handle` on
   `title`), in first-reply order; `.thread-summary-time` shows `hhmm` of the
   latest reply. A thread with > 5 distinct participants caps the pile at 5
   badges and renders a `.thread-summary-overflow` `"+N"` node — the
   2-participant stub never overflows, so the cap path needs a purpose-built
   fixture with ≥ 6 distinct reply authors.

Test cycle: write 1-5 + the two migrations → run
`direnv exec ~/agents/workspaces/<codename>/compass moon run compass-ui:test`
(observe the new legs red, the six untouched `ThreadPanel` legs green) →
implement `ThreadView` + CSS → all green → `moon run compass-ui:typecheck` →
`biome check` clean.

## Tasks

- [ ] T-1: `threadSummary(thread)` helper in `comms.ts` + unit tests in
  `comms.test.ts` (red → green).
- [ ] T-2: `ThreadView` Slack-model rework — delete the inline
  `.thread-replies` block, add gated `.thread-summary` / `.thread-reply`
  affordances (summary carries placeholder-initial `.thread-summary-avatar`
  badges capped at 5 + a `.thread-summary-overflow` `"+N"` node),
  `.thread-summary*` CSS, migrate the two `ThreadPanel.test.tsx` legs, add
  assertions 1-5 + the overflow-cap fixture (red → green); `moon run
  compass-ui:typecheck compass-ui:test` green, biome-clean.

## Resolved questions (Matt-ruled)

These were flagged open for Matt at design review; his rulings are folded into
the Approach and T-2 above and recorded here for provenance.

- **Summary richness → full Slack-shaped summary.** Matt ruled the full form
  (count + participants + last-reply `hhmm`), not the minimal "N replies". As
  designed; `threadSummary` already returns all three.
- **Participant rendering → placeholder-initial avatar badges now.** Matt ruled
  placeholder avatars over text `@handle` chips: each badge shows the handle's
  first character uppercased, full `@handle` on `title`. Real photo faces stay
  deferred until an avatar pipeline exists (`.thread-summary-people` is the
  seam).
- **Participant-chip overflow → `+N` cap at 5.** Matt ruled the Slack cap now:
  the pile shows at most 5 badges and a trailing `"+N"` overflow node. Kept a
  view concern — `threadSummary` returns the full `participantIds`, the cap is
  applied in `ThreadView`, so no model change.
