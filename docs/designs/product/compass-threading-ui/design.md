# Compass Threading UI — Slack-style threads in the channel surface

Status: Active

> Internal design record — July 2026. Builds on the frozen 0.7 record
> (`../compass-0.7-channel-workspace/design.md`, merged): board-primary shell,
> comms as a surface within it (standalone `channel` view + the agent
> workspace's chat pane). This record changes only how threads *present and
> grow* in `ChannelView`; the comms model (accounts, channels, membership,
> asks, mentions, `parentMessageId` threading) is untouched.

## Problem / Intent

The thread *model* exists — `comms.ts` groups a channel into `Thread[]` — but
the UI renders every thread fully expanded inline (root + indented replies),
which reads as one noisy flat log and offers no way to reply into a thread at
all. This record designs a Slack-style threading UX: a quiet main stream
of root messages with per-thread summary affordances, a thread panel for
focused reading and replying, and the store action that makes replying real
against the fixture (seam-compatible with the eventual daemon `PostMessage`).

## Global Constraints

- **Stack**: SolidJS + TypeScript, Vite, Bun test (`@solidjs/testing-library`,
  `--conditions browser`), Biome. No new dependencies.
- **Fixture→daemon seam**: components read ONLY `AppStore` accessors/actions
  (`store.ts:10-13` — "the store reads the in-memory fixture (stub-data.ts).
  When the daemon grows the real streams, the accessors below stay and their
  bodies swap the fixture for the generated @compass/client"). No component
  imports `STUB_MESSAGES` directly; no daemon calls anywhere in this work.
- **Board-primary framing** (frozen, 0.7 record §39-52): the board is the
  shell; the channel is a surface WITHIN it — the standalone `channel` view
  and the agent workspace's chat pane. Both mounts render the SAME
  `ChannelView`, so every threading affordance here must behave in both.
- **Red-first** (rule://red-green-testing): every task lands its failing
  BDD/unit tests before its implementation.
- **Source hygiene**: no superseded-design citations in new code
  (`design-citations.test.ts:1-3` — "Source-hygiene gate: no superseded
  compass-0.6 / channel-first design citations survive in shipped UI source").
  New code cites THIS record.
- **Deterministic renders**: timestamps format via the existing UTC `hhmm`
  (`ChannelView.tsx:15-17` — "deterministic, locale-independent (the fixture
  pins atUnixMs to a fixed clock)"); tests never assert wall-clock values.

## Approach

### Grounding: what exists today

- **Model** — one-level-flat threads, the flat shape Slack threads use
  (`comms.ts:150-156`):

  ```ts
  /** A message plus its direct replies — one thread in the conversation. Replies
   *  are one level deep (a reply to a reply still attaches to the top-level
   *  parent), matching a flat Discord-style thread under a parent. */
  export interface Thread {
      root: Message;
      replies: Message[];
  }
  ```

  carried by `Message.parentMessageId` (`comms-stub.ts:152-154` — "SEAM
  (channel-model amendment): the message this one replies to, forming a
  thread; absent for a top-level message.", `parentMessageId?: string`), and
  grouped by `threadsOf(messages, channelId): Thread[]` (`comms.ts:195-198`)
  via `rootIdOf(msg, byId, bound)` (`comms.ts:176-180`).
- **Render** — the flat inline expansion this record replaces
  (`ChannelView.tsx:207-216`): `ThreadView` renders the root `MessageRow` then
  `<Show when={props.thread.replies.length > 0}><div class="thread-replies">`
  with every reply always expanded, indented by CSS only
  (`app.css:2938-2941` — `.thread-replies { margin-left: 20px; …
  border-left: 2px solid var(--border);`).
- **No way to reply**: the composer is a targetless no-op
  (`ChannelView.tsx:260` — "The composer (non-functional in the mockup —
  posting lands with PostMessage)") and no store action posts a message; the
  one message mutation today is `answerAsk` (`store.ts:668-688`,
  `setMessages((ms) => ms.map(...))` over the fixture signal
  `store.ts:463` — `const [messages, setMessages] =
  createSignal<Message[]>(STUB_MESSAGES);`).
- **Fixture threading exists**: `#svc.compass` carries a real thread —
  `msg-c1` root, `msg-c2`/`msg-c3` with `parentMessageId: "msg-c1"`
  (`comms-stub.ts:307-345`) — so every affordance below is demonstrable
  against the stub with no fixture surgery.

### Slack's thread model, and the faithful Compass analog

The designed shape — flat one-level replies, an in-stream summary affordance
on the parent, a side panel docked beside the channel, reply-to-any-message
— is **Slack's thread model**: the main stream stays quiet (replies live in
the thread, not interleaved in the channel), the parent message carries a
summary affordance ("N replies" + participants), and the thread opens in a
panel beside the channel rather than pulling you away from it. Matt's brief
said "similar to Discord's" — Discord's actual model (threads as first-class
sub-channels with their own stream, name, and archival) is the richer
daemon-era variant deferred below and in the thread-rail open question. The
Slack shape fits the existing `parentMessageId` carrier with no model
change.

The Compass analog, scoped to the stub-era MVP:

1. **Main stream = roots + summary chips.** The channel stream renders one
   `MessageRow` per thread ROOT. A thread with replies shows a **thread
   summary chip** under its root — reply count, distinct participant handles,
   last-reply time, and a pending-ask badge when a buried reply carries an
   unanswered ask (T3) — instead of the always-expanded `.thread-replies`
   block. This is the core Slack-style change: replies leave the main
   stream.
2. **Thread panel, docked in the conversation.** Clicking the chip (or the
   reply affordance) opens a **thread panel**: a column docked at the right
   edge of the `.conversation` section showing the root, all replies
   (chronological, flat — the model's one-level guarantee), and a thread
   composer. Docking INSIDE `ChannelView` (not the app-level right sidebar)
   keeps the affordance identical in both mounts — the standalone `channel`
   center and the workspace chat pane — without touching the board shell,
   matching Slack's panel-beside-the-channel behavior.
3. **Starting/continuing a thread = the reply affordance.** Hovering any
   message (root in the stream, or any message in the panel) shows a
   **Reply** button. Replying to a root starts/continues its thread; a
   reply-to-a-reply GROUPS under the thread root either way (the model
   flattens to one level, `comms.ts:150-152`). The STORED `parentMessageId`
   is the TRUE parent (the message actually replied to); read-time
   resolution flattens it under the thread root (Matt's ruling on the former
   Open Questions #2 — see Resolved decisions).
4. **Flat stays flat.** No nesting change: one-level-flat replies are both
   the current model and Slack's own thread shape. No model migration.
5. **Asks in threads** follow the mount's existing `readonlyAsks` discipline
   (`ChannelView.tsx:308-310` — `readonlyAsks?: boolean` prop): the panel
   passes the prop through, so a standalone-channel thread renders asks
   read-only and the workspace thread stays answerable. No new rule.
6. **Membership gates replying** exactly as the main composer gates posting
   (`ChannelView.tsx:279` — `disabled={props.channel.membership === "none"}`).

**Deliberately NOT adopted from Discord** (deferred, non-load-bearing):
Discord-style threads as first-class sub-channels in the channel rail,
auto-archival, thread naming/creation-without-a-message, per-thread
membership/notification settings. All are daemon-era contract surface; the
stub has no carrier for them and the MVP loses nothing without them.

### The `rootIdOf` parent-cycle contract (fixed here, not deferred)

Today the cycle guard walks exactly `bound` steps and returns wherever it
lands (`comms.ts:182-188` — `for (let i = 0; i < bound; i++) { … }` then
`return cur.id; // cycle guard: treat the last seen as root`), with
`bound = inChannel.length + 1` (`comms.ts:201`). For a parent cycle the
landing spot depends on the walk length's PARITY, so there are two failure
modes, not one:

- **Odd bound** (a 2-cycle A→B→A alone in the channel, bound = 3): each
  member resolves the OTHER, so each files as a *reply* in `threadsOf`'s
  else-arm (`comms.ts:209-213`), no root is pushed, and the cycle is
  silently OMITTED from the render — violating `threadsOf`'s own "nothing
  is dropped" doc (`comms.ts:193-194`).
- **Even bound** (the same 2-cycle plus one unrelated message, bound = 4):
  each member resolves ITSELF, both push as roots (`comms.ts:206-208`), and
  the cycle renders as two reply-less threads — nothing dropped,
  differently wrong.

Structurally unreachable from the fixture, but real once the daemon streams
arbitrary data — and this record makes root resolution load-bearing for the
reply action, so we fix it now with a **deterministic root election**: when
the walk detects a revisit, elect one root among the CYCLE'S MEMBERS ONLY,
identical for every message whose parent chain reaches that cycle. T1
specifies the election (and weighs the two candidate orders). Pure-function
change, fully unit-testable.

### Alternatives considered

- **Keep inline expansion, add collapse.** A disclosure toggle on
  `.thread-replies` is smaller, but it keeps replies in the main stream (the
  thing Slack-style threading removes) and gives no home for a thread
  composer. Rejected: it's a collapse, not threading.
- **Thread panel in the app-level right sidebar.** Reuses the dock, but the
  right sidebar is a board/workspace surface (`store.ts:71-73` —
  `RightSidebarTab`), absent/different across the two ChannelView mounts, and
  wiring a comms panel into it would couple the board shell to comms state.
  Rejected: dock inside `.conversation` instead, identical in both mounts.
- **Thread-open state in the store.** A global `openThreadRootId` signal would
  let the two mounts share one open thread — which is exactly the D3-style
  bleed 0.7 engineered away (the workspace pane must not follow standalone
  selection, `ChannelView.tsx:303-307`). Rejected: thread-open state is
  component-local per `ChannelView` instance.

## Plan

Four tasks, each red-first with its own test cycle. T1/T2 are pure
model/store work (no render change); T3/T4 are the UX. T2 depends on T1
(a reply must GROUP under the correct root — via T1's fixed resolution);
T3 depends on nothing but the model;
T4 depends on T2+T3. **Hard constraint (promoted from review — formerly
OQ5): T3 and T4 land in ONE PR.** T3 alone removes inline reply expansion,
leaving replies reachable only through a chip whose panel doesn't exist
yet.

### T1 — `rootIdOf` deterministic cycle election (comms.ts)

Fix the parent-cycle misresolution: replace the bounded-loop guard with a
visited-set walk (the `bound` parameter goes away). The visited set is the
cycle DETECTOR only, never the election pool: it holds the walk's non-cycle
PREFIX as well as the cycle, so electing over it would re-introduce
entry-point dependence — walking C→A into the cycle A→B→A visits
{C, A, B} before revisiting A (a min over that set can crown C, not a
cycle member at all), while walking from B visits only {B, A}. The election
pool is the CYCLE MEMBERS ONLY: on the first re-visit, the cycle is the
suffix of the walk from the re-visited node onward — collect exactly those
nodes and elect among them, so every entry point sees the same pool and
every message reaching the cycle resolves the same root. Update the
`rootIdOf` and `threadsOf` doc comments so "nothing is dropped"
(`comms.ts:193-194`) is true again.

**Election order (weighed sub-choice)**: two deterministic candidates —
smallest id (lexicographic; simplest) vs chronologically first (min
`atUnixMs`, id tiebreak — the exact total order `channelMessages` already
sorts by, `comms.ts:166-169`). Smallest-id can crown a late-posted message
as the "starter"; chronologically-first reads naturally as the thread
starter and reuses an ordering the module already owns.
**Recommendation: chronologically-first.**

`Interfaces:`

- Changes (in `comms.ts`; `threadsOf` signature untouched; `rootIdOf`
  stays module-private — the store-true-parent ruling means T2 does no
  root resolution, so nothing outside `comms.ts` consumes it):

  ```ts
  function rootIdOf(msg: Message, byId: Map<string, Message>): string;
  // - no parent            → msg.id (unchanged)
  // - unresolved parent    → the orphan's own id (unchanged, comms.ts:185)
  // - parent cycle reached → the elected root: the chronologically-first
  //   member (min atUnixMs, id tiebreak) among the CYCLE'S MEMBERS ONLY,
  //   identical for every message whose parent chain reaches that cycle
  export function threadsOf(messages: readonly Message[], channelId: string): Thread[];
  // unchanged signature; new guarantee: every in-channel message appears in
  // exactly one Thread (as root or reply) — cycles included
  ```

- Consumes: `Message.parentMessageId` (`comms-stub.ts:154`).

Test cycle (red first, `comms.test.ts`): a 2-cycle A→B→A yields one thread
(root = the elected member, the other a reply); a 3-cycle likewise; a chain
hanging off a cycle (C→A with cycle A→B→A) files C under the elected root —
this test MUST be id- AND timestamp-adversarial: give C an id (and
`atUnixMs`) that sorts BELOW both cycle members, so an implementation that
elects over the visited set would crown C and fail; the even-bound
dual-root regression — a 2-cycle plus one unrelated channel message, the
shape where today's `bound = inChannel.length + 1 = 4` walk
(`comms.ts:201`) resolves each cycle member to ITSELF and pushes both as
roots (`comms.ts:206-208`) — asserts the cycle yields ONE thread, not two
reply-less ones; a self-parent (A→A) roots itself; existing acyclic/orphan
cases unchanged; total message count across `threadsOf` output equals the
channel's message count for a cyclic fixture (the "nothing dropped"
invariant).

### T2 — Store: `postReply` action (the fixture-era PostMessage seam)

Add the one mutation the thread composer needs, following the `answerAsk`
pattern (`store.ts:668-688` — a `setMessages((ms) => …)` functional update
over the fixture signal). The reply is stamped with the caller, the target
channel, the clicked message's id as `parentMessageId` (store-true-parent,
per Matt's ruling — see below), and a **strictly monotonic** timestamp (`Date.now()`, or
max(existing `atUnixMs`)+1 if larger — keeps chronological order stable
against the fixture's pinned clock, `comms-stub.ts:277`). Strict
monotonicity is a load-bearing invariant, not a nicety: equal timestamps
must be IMPOSSIBLE for minted replies, so the `channelMessages` id tiebreak
(`comms.ts:168`) never fires for them — `msg-reply-<n>` counter ids sort
lexicographically (`"msg-reply-10" < "msg-reply-2"`) and would misorder
under equal timestamps. Guards: no-op when the channel is unknown, when the
caller's membership is `"none"`, when the parent message doesn't exist in
that channel, or when the text is blank after trim.

The stored `parentMessageId` is the TRUE parent — the clicked message's id,
stored directly (Matt's ruling on the former Open Questions #2). `postReply`
does NO root resolution; `threadsOf` already files any chain member under the
resolved root (`comms.ts:203-213`), so the rendered grouping is identical and
`rootIdOf` stays private to `comms.ts`.

`Interfaces:`

- Adds (on `AppStore`, `store.ts:216`, in the "Comms" section beside
  `answerAsk`, `store.ts:311-314`):

  ```ts
  /** Post a reply into a thread: appends a caller-authored text message
   *  with `parentMessageId` = the clicked message's id (store-true-parent).
   *  No-op for an unknown channel/parent, an unjoined channel, or blank
   *  text. Becomes a PostMessage call when the daemon stream lands. */
  postReply: (channelId: string, parentMessageId: string, text: string) => void;
  ```

- Consumes: `CALLER_ID` (`store.ts:49`), `messages`/`setMessages`
  (`store.ts:463`), `channels` (`store.ts:462`). No root resolution — the
  clicked id is stored verbatim, so `rootIdOf` is not consumed here.
- Id minting: `msg-reply-<n>` off a counter minted PER STORE (inside
  `createAppStore`), never module-local: a module-local counter is shared
  across store instances, so `store.test.ts` assertions against literal
  ids would depend on suite execution order. Fixture-era only, replaced by
  the server-assigned id (`comms-stub.ts:144` — "Server-assigned stable
  id") at the daemon swap.

Test cycle (red first, `store.test.ts`): a reply to a root appears in
`messages()` with the right channel/parent/author; a reply to a REPLY lands
in that thread's `replies` via `threadsOf` (asserting the RENDERED
grouping); blank text / unknown
parent / membership `"none"` are no-ops; two replies keep chronological
order; the strict-monotonicity invariant — a reply minted against the
pinned fixture clock carries an `atUnixMs` strictly greater than every
existing message's in the channel, so no minted reply ever ties a
timestamp; assertions use a fresh store per test (or assert id shape, not
literal counter values).

### T3 — Stream reshape: roots + thread summary chips (ChannelView.tsx)

Replace the always-expanded `.thread-replies` block (`ChannelView.tsx:215-227`)
in the MAIN stream: `ThreadView` renders the root `MessageRow`, then — when
`replies.length > 0` — a **summary chip** (button): reply count ("N replies"),
up to 3 distinct participant handles (from `replies[].authorAccountId` via the
existing `byId` map, `ChannelView.tsx:171`), and the last reply's `hhmm` time.
When any reply carries a PENDING ask — an `ask` block whose
`chosenOptionIds` is empty ("empty while pending", `comms-stub.ts:128-129`)
— the chip also shows a one-glyph **pending-ask badge**: asks are the
load-bearing interaction (`AskBlock`, `ChannelView.tsx:85`), and without a
badge a question posted as a thread reply is invisible in the main stream.
A root with no replies shows no chip. Hovering a root shows a **Reply**
affordance. Both chip-click and reply-click call an `onOpenThread` callback
with the root id (T4 owns what opens). The chip is a real `<button>`
(keyboard-reachable); the reply affordance likewise.

`Interfaces:`

- Changes (in `ChannelView.tsx`; `ThreadView` stays module-private):

  ```ts
  const ThreadView: Component<{
      thread: Thread;
      byId: Map<string, Account>;
      byHandle: Map<string, Account>;
      readonlyAsks?: boolean;
      onOpenThread: (rootId: string) => void;   // chip / Reply click
  }>;
  const ThreadSummaryChip: Component<{ thread: Thread; byId: Map<string, Account>; onOpen: () => void }>;
  // hasPendingAsk is derived inside the chip from thread.replies: any
  // reply block with kind === "ask" and chosenOptionIds.length === 0
  // lights the badge
  ```

- CSS: `.thread-chip` (+ participant/count/time and pending-ask badge
  spans) added beside the existing `.thread` rules (`app.css:2937-2941`);
  `.thread-replies` retired
  from the main stream (it survives only if T4 reuses the class in-panel —
  prefer new `.tp-*` classes to keep the panel independently styleable).

Test cycle (red first, `ChannelView.test.tsx`): the fixture thread
(`msg-c1` + 2 replies, `comms-stub.ts:307-345`) renders ONE `.msg` in the
stream for the thread plus a chip reading "2 replies"; reply bodies
(`msg-c2` text) are NOT in the stream DOM; a reply-less root renders no
chip; chip click fires `onOpenThread` with `"msg-c1"`; a thread whose reply
carries an unanswered ask shows the pending-ask badge, and the badge clears
once the ask is answered; identical in both mounts (standalone +
workspace-style `channel` prop).

### T4 — Thread panel: focused read + thread composer (ChannelView.tsx)

The docked panel. `ChannelView` holds component-local state
`openThreadRootId: string | null` (a `createSignal`, NOT store state — see
Alternatives). When set and the root still exists in the current channel's
threads, `.conversation` renders a two-column body: the stream (T3) plus a
`ThreadPanel` column at the right edge — header ("Thread" + close button),
the root `MessageRow`, all replies chronological and flat, and a thread
composer. The composer posts via `store.postReply(channel.id, rootId, text)`
and clears; it is disabled when `channel.membership === "none"` with the
main composer's join placeholder convention (`ChannelView.tsx:267` —
`if (props.channel.membership === "none") return "Join to post…";`).
Channel switch resets `openThreadRootId` to null — keyed on the LOGICAL id
(`channel()?.id`, `ChannelView.tsx:316-317`), NEVER on object identity:
`setMembership` mints a fresh object for the mutated channel
(`{ ...c, membership: next(c.membership) }`, `store.ts:662-666`) and both
channel sources re-`find` over the fresh list (`selectedChannel`,
`store.ts:556-558`; `workspaceChannel`, `store.ts:566-569`), so joining or
toggling subscribe on the CURRENTLY OPEN channel emits a new object for the
same logical channel — an identity-keyed reset would spuriously close the
panel mid-read. `readonlyAsks` passes through to the panel's `MessageRow`s
unchanged.

**Narrow-pane behavior: explicitly deferred for the MVP.** The workspace
chat pane lives inside `AgentView`'s split-pane tree (`ChannelView` mounts
in a pane body, `AgentView.tsx:111-115`), so `.conversation` can be a
fraction of the window and a docked second column may leave both columns
cramped. A responsive fallback (a min-width below which the panel overlays
the stream instead of docking) is deliberately out of scope: deferring
keeps T4 a pure docked-column change. Revisit if the workspace pane proves
too narrow in practice.

`Interfaces:`

- Adds (in `ChannelView.tsx`, module-private):

  ```ts
  const ThreadPanel: Component<{
      thread: Thread;                       // resolved by root id each render
      channel: Channel;                     // membership gate + composer target
      byId: Map<string, Account>;
      byHandle: Map<string, Account>;
      readonlyAsks?: boolean;
      onClose: () => void;
  }>;
  ```

- Consumes: T2 `postReply`; T3 `onOpenThread` (wired to
  `setOpenThreadRootId`); `threadsOf` output already computed in `threads()`
  (`ChannelView.tsx:321-324`) — the panel's thread is
  `threads().find(t => t.root.id === openThreadRootId())`, so a new reply
  reactively appears with no extra plumbing.
- CSS: `.conv-body` (row flex wrapping stream + panel), `.thread-panel`,
  `.tp-head`, `.tp-composer` — new rules in `app.css` beside the existing
  conversation block.

Test cycle (red first, `ChannelView.test.tsx`): chip click opens the panel
showing root + both replies; typing + send calls through to the store and
the new reply renders in the panel AND bumps the stream chip to "3 replies";
close button unmounts the panel; switching channels closes it (the reset
keys on `channel().id`); toggling subscribe on the OPEN channel does NOT
close it (the `setMembership` identity-mint case above); in an
unjoined channel the thread composer is disabled; with `readonlyAsks` an ask
inside the panel renders inert; two `ChannelView` mounts hold independent
open-thread state.

## Tasks

- [ ] **T1 — `rootIdOf` cycle election**: visited-set walk with
      cycle-members-only election (chronologically-first: min `atUnixMs`,
      id tiebreak), `threadsOf` "nothing dropped" guarantee restored, doc
      comments updated; cycle unit tests (id-adversarial chain-into-cycle +
      even-bound dual-root regression) red→green in `comms.test.ts`.
- [ ] **T2 — `postReply` store action**: `AppStore.postReply(channelId,
      parentMessageId, text)`, true parent stored verbatim (store-true-parent),
      strictly monotonic timestamps, per-store id
      counter, membership/blank/unknown guards, `answerAsk`-pattern
      functional update; red→green in `store.test.ts`.
- [ ] **T3 — Stream reshape**: roots-only stream, `ThreadSummaryChip`
      (count + participants + last-reply time + pending-ask badge), hover
      Reply affordance, `onOpenThread` callback; `.thread-replies` retired
      from the stream; red→green in `ChannelView.test.tsx`.
- [ ] **T4 — Thread panel**: component-local `openThreadRootId`, docked
      `ThreadPanel` with thread composer wired to `postReply`,
      channel-switch reset keyed on `channel().id`, `readonlyAsks`
      pass-through, membership gate; red→green in `ChannelView.test.tsx`;
      full battery + Biome clean. Ships in the SAME PR as T3 (Plan
      constraint).

## Open Questions

None — all four were ruled by Matt on 2026-07-20 (see Resolved decisions).

## Resolved decisions

Settled during the design-critique fold; recorded for audit, no longer
open:

- **Reply affordance scope** — panel-only replying: the main composer stays
  thread-agnostic (top-level posts only, still the documented no-op) until
  PostMessage lands. A Slack-style "replying to…" pill on the main composer
  doubles composer states for no MVP gain.
- **T3/T4 transition** — promoted to a hard Plan constraint (see Plan): T3
  and T4 ship in one PR, because T3 alone leaves replies unreachable behind
  a chip with no panel.
- **Cycle contract destination (former OQ1)** — **Matt: yes.** The daemon
  enforces parent-chain acyclicity at write time (rejects a PostMessage
  whose parent chain would cycle); the UI's `rootIdOf` election is
  defense-in-depth / render-safety net. Out of this record's scope to
  implement — flagged so the channel-model contract amendment picks it up.
- **Reply storage (former OQ2)** — **Matt: store the true parent.**
  `postReply` stores the clicked message's id verbatim; read-time
  resolution flattens under the thread root. No write-time re-rooting, so
  the reply target is preserved for a future daemon-era "in reply to @x"
  affordance and the PostMessage seam never discards it. `rootIdOf` stays
  module-private (T2 does no root resolution). T1/T2 above are written to
  this branch.
- **Root-message deletion display (former OQ3)** — **Matt: defer.** The
  stub has no delete; `threadsOf` re-roots orphans so replies survive.
  Revisit with message deletion in the daemon era.
- **Thread rail entries (former OQ4)** — **Matt: defer.** The summary chip
  is the MVP discovery surface; per-thread rail entries need thread naming +
  activity state the stub lacks.
