# Compass message surface — virtualized thread list + streaming markdown (RIG-1332)

Status: Active

Docs-only record; implementation is a separate follow-on lane that rebases onto
franklin's RIG-1337 ChannelView restructure (seam stated in §Approach).

## Problem / Intent

The channel conversation in the Compass UI (`apps/ui`, SolidJS
`^1.9.13`) renders every thread unconditionally and treats message text as
plain text:

1. **No virtualization, no scroll contract.** `ChannelView` renders a flat
   `<Index each={threads()}>` inside `.conv-stream`
   (`apps/ui/src/components/ChannelView.tsx:343-351`) and the file
   contains **zero** scroll management — no ref, no `onScroll`, no
   stick-to-bottom (a search for `scroll`/`ref=`/`onScroll` across the file
   returns nothing, verified this session). A long channel renders every DOM
   node; a fast-streaming agent turn re-renders them all.
2. **Message text is not rendered as markdown.** The comms model documents a
   text block as "settled markdown, may carry @-mentions"
   (`comms-stub.ts:143-150`), but the render path is `Block` →
   `MentionText` — plain runs + mention chips only, no markdown, no code
   highlighting (`ChannelView.tsx:158-162`:
   `fallback={<MentionText text={blockText(props.block)} byHandle={props.byHandle} />}`).

This record adopts two libraries to close both gaps: `@tanstack/solid-virtual`
for the thread list (with the full chat scroll contract designed fresh) and
`solid-markdown` + `shiki` for message rendering (composing with — never
replacing — the existing `MentionText` mention chips), correct mid-stream
while a text block's string grows across renders.

**Boundary with compass-0.8.** The sibling record
`compass-0.8-threading-and-session-renderer/design.md` designs the
**session renderer** — the typed agent trace/observation pane ("Per-kind
renderer components (`SessionEventRow` dispatching to …)",
`compass-0.8-threading-and-session-renderer/design.md:230-237`). That is a
DIFFERENT surface: this record covers only the durable channel-message
markdown in the conversation stream. The `Block` doc comment states the same
split: rich agent blocks "are not in the channel; they live in the session
observation panel" (`ChannelView.tsx:150-152`).

Non-goals (ticket): no chat-in-a-box mega-component; no Kobalte/corvu a11y
primitives (deferred); no change to the `AppStore` contract or component tree
(#783's frozen surface); no Go.

Spec-impact: none. Net-new UI rendering entirely below the store seam
(`store.ts:10-13`: "the accessors below stay and their bodies swap the fixture
for the generated @compass/client — the AppStore contract is the seam");
`docs/specs/product/compass.md` describes the `compass.v1` contract and server
surface only (a search for markdown/render/virtual/scroll in the spec returns
nothing, verified this session) — no living spec changes.

## Approach

The change is two orthogonal adoptions inside the `Block`/`ThreadView`/
`ChannelView` render path, resolved through four design forks below. Nothing
above the store seam changes: `threadsOf()` (`comms.ts:195-219`) stays the one
grouping derivation, `MentionText`/`AskBlock` behavior is preserved, and the
`AppStore` contract is untouched.

### Fork (a) — Virtualization + the scroll contract (designed fresh)

**Decision: `createVirtualizer` from `@tanstack/solid-virtual` in chat mode
(`anchorTo: 'end'` + `followOnAppend`), variable-size over the THREAD list,
with the scroll container being the post-RIG-1337 `.conv-stream`.**

*What is virtualized.* The list unit is a **thread**, not a message: the
stream today is `<Index each={threads()}>` → `ThreadView` (`ChannelView.tsx:343-351`),
where a `Thread` is `{ root: Message; replies: Message[] }` (`comms.ts:153-156`)
— one root plus N replies, one level deep ("Replies are one level deep (a
reply to a reply still attaches to the top-level parent)", `comms.ts:150-152`).
Thread heights are therefore intrinsically variable (reply count, block count,
markdown length), so the virtualizer runs **variable-size**: `estimateSize`
returns a cheap per-thread estimate (base row height + replies × row height)
and every rendered thread element registers `ref={virtualizer.measureElement}`
with `data-index` so real heights correct the estimate. Keys are the thread
ROOT message ids via `getItemKey: (i) => threads()[i].root.id` — never the
index — so prepend anchoring can re-find items (TanStack's chat guidance:
"Do not use index keys for chat history", tanstack.com/virtual/latest/docs/chat).

*The scroll contract — designed from zero.* ChannelView has NO scroll
management today (verified: no `scroll`/`ref=`/`onScroll` match in the file),
so this record owns the whole contract. TanStack Virtual ≥3.13's chat mode
provides it natively and we adopt it rather than hand-rolling:

- **Stick-to-bottom only when already at bottom.** `anchorTo: 'end'` +
  `followOnAppend: true`: an append scrolls to end ONLY if the viewport was
  already at the end before the append ("The follow only happens if the
  viewport was already at the end before the append; users who have scrolled
  up to read history are not pulled down",
  tanstack.com/virtual/latest/docs/api/virtualizer). The **at-bottom
  predicate** is the library's `isAtEnd()`, governed by `scrollEndThreshold`
  ("the pixel threshold used by `isAtEnd()` and `followOnAppend` to decide
  whether the viewport is close enough to the end to count as pinned"). We set
  `scrollEndThreshold: 80` (≈ one message row of slack — a product-feel
  number; OQ-3). No hand-written `scrollTop + clientHeight >= scrollHeight - ε`
  predicate: the library owns it, our tests pin the *behavior*.
- **Prepend history without jump.** `anchorTo: 'end'` gives prepend stability
  via keyed re-anchoring: the virtualizer captures the visible keyed item
  before the data change and adjusts scroll offset so it stays put — no manual
  `scrollTop += delta` compensation, no `column-reverse` inversion. This is
  why root-id keys are load-bearing.
- **Mid-stream growth of the last thread.** When the pinned last item grows
  (a streaming turn re-measuring taller), end-anchored mode applies the size
  delta and keeps the end pinned — which is exactly the fork-(c) growing-text
  case arriving through `measureElement`.
- **Initial mount AND channel switch.** `scrollToEnd()` once the scroll
  element is mounted, so a channel opens at latest. Switching the selected
  channel does NOT remount `.conv-stream` (`ChannelView`'s `Show` stays
  truthy — `ChannelView.tsx:327-332`), so a mount-only effect would leave the
  new channel at the previous channel's scroll offset. The contract is a
  `createEffect` keyed on the selected channel id that resets the virtualizer
  to the end on every change (initial mount is the first fire), so each
  channel opens at its latest message. A "jump to latest" affordance driven by
  `!isAtEnd()` is deliberately OUT of scope (non-goal creep; OQ-3 notes it as
  the natural follow-on).

*Where it sits — the RIG-1337 seam (pending #841 merge).* RIG-1337
restructures the `ChannelView` wrapper into a flex-row; the scroller is
designed against franklin's POST-restructure shape (his PR #841, live but
unmerged — line numbers below are its head and shift on rebase):

```text
<section class="conversation">
  <ChannelHeader/>                 // full-width top
  <div class="conv-body-row">      // NEW flex-row
    <div class="conv-main">        // NEW flex-column
      <div class="conv-stream"> …unchanged Index of ThreadView… </div>
      <Composer/>
    </div>
    <ThreadPanel/>                 // NEW aside, right, OUTSIDE the scroll region
  </div>
</section>
```

PR #841 head line map: `.conv-body-row` at ChannelView.tsx L345 (new
flex-row), `.conv-main` at L346 (new flex-column: stream + composer),
`.conv-stream` at L347 with **unchanged internals** — the threads
`Show`/`Index`/`ThreadView` and empty fallbacks at L348-367, the
`<Index each={threads()}>` the virtualizer wraps at L358 — and
`<ThreadPanel>` at L371 as a sibling of `.conv-main`. Only `.conv-stream`'s
parent chain changes; its CSS keeps `flex: 1; overflow-y: auto` inside
`.conv-main` (the new `.conv-body-row`/`.conv-main`/`.thread-panel` rules land
in app.css after `.thread-replies`, ~L2966+). So `.conv-stream` IS the scroll
element (`getScrollElement` returns its ref), the virtualizer's inner sizer
div and absolutely-positioned thread items replace the bare `Index` inside
it, and `ThreadPanel` sits outside the scroll region and cannot interfere
with measurement. **This record does not hard-code the current single-column
`.conversation` shape** (`ChannelView.tsx:324-358` on main today); the impl
rebases onto franklin's landed restructure and takes the wrapper as found.

**The one structural invariant this record needs from #841** is that the
thread `Index` lives inside a single dedicated `overflow-y: auto` element
(`.conv-stream`) that is the scroll container, with `ThreadPanel` outside it.
The line map above is illustration: if #841 is reshaped in review, only that
invariant must survive — the scroller binds to whatever element carries the
thread list's scroll, `ThreadPanel` excluded from measurement.

*Solid specifics.* `createVirtualizer` (the Solid adapter) returns a
reactive virtualizer; `getVirtualItems()` is consumed inside a `<For>` over
virtual items, each row absolutely positioned via
`transform: translateY(${item.start}px)` on the standard two-div pattern
(outer scroll container → inner relative sizer of `getTotalSize()` height).
`@tanstack/solid-virtual` 3.13.34 declares peer `solid-js ^1.3.0` —
compatible with the app's `^1.9.13` (`package.json:10`). The chat-mode
surface this fork leans on (`anchorTo`, `followOnAppend`, `scrollEndThreshold`,
`isAtEnd()`, `scrollToEnd()`, `shouldAdjustScrollPositionOnItemSizeChange`)
lives in `@tanstack/virtual-core` — solid-virtual 3.13.34's core dep is
`virtual-core` 3.17.5, whose shipped `dist/esm/index.d.ts` declares all of
them (verified against the published package this session). If T1's installed
core resolves below the chat-mode floor, T1 pins `@tanstack/virtual-core` to
`^3.17.5` explicitly — the options are the contract, not the adapter version.

*Rejected alternative:* keep the flat `Index` and add only a hand-rolled
stick-to-bottom `createEffect` on `scrollHeight`. Cheaper today, but it leaves
the long-channel DOM cost unsolved (the ticket's first acceptance criterion),
and the hand-rolled prepend anchoring we'd inevitably need later is exactly
the hard part TanStack's chat mode already solves. Also rejected:
`column-reverse` CSS inversion (breaks keyboard/screen-reader order, fights
the virtualizer) and virtualizing at message granularity (would split a
thread's root from its replies across virtual items, forcing the thread
border/indent structure — `.thread-replies`, `ChannelView.tsx:214-226` — to be
faked per-row).

*Bounded risk — thread granularity.* One virtual item is a whole thread
(root + all replies, `comms.ts:153-156`), so a single hot thread with hundreds
of replies collapses into one large DOM row that `measureElement` re-measures
per tick — the cost virtualization avoids, reintroduced inside one row.
Fixture threads are small today; if hot threads emerge, the follow-on is
replies-collapse-after-N (or promoting replies to their own virtual items),
not a change to this record. The trade is conscious, not unexamined.

### Fork (b) — Markdown renderer + MentionText composition (load-bearing)

**Decision: `solid-markdown` as the renderer; mentions render INSIDE the
markdown tree by post-processing its TEXT nodes ("markdown-first"), via a
custom text-node renderer that reuses `parseMentions` — not by segmenting on
mentions first.**

*The constraint.* Mentions are NOT markdown. They are a Compass span syntax —
`@` + handle run, `MENTION_RE = /@([a-z0-9][a-z0-9._-]*)/gi` (`comms.ts:237`)
— parsed by `parseMentions(text)` (`comms.ts:244`), which returns
`{ handle, reserved, start, end }` spans (`comms.ts:227-232`). Today
`MentionText` (`ChannelView.tsx:35-87`) slices the raw string on those spans
into alternating plain runs and `<span class="mention-chip">` chips. The
markdown renderer must compose with this, not replace it (ticket requirement).

*The two candidate compositions:*

1. **Markdown-first, mention-post-process (CHOSEN).** Parse the whole block
   text as markdown; override the renderer's **text-node** component so every
   literal text run in the resulting tree is rendered through the
   mention-chipping logic (the exact `parseMentions` → alternating runs/chips
   loop `MentionText` uses today, extracted so both call sites share it).
   The mention override fires on prose text runs only, via `solid-markdown`'s
   dedicated **text-node** override — verified present in the pinned version:
   `solid-markdown@2.1.1` `dist/index.d.ts:71-74` declares
   `text?: Component<{ node: Text }>` on its `Components` map (the `Text` type
   imported from `hast`, `:4`), so the override receives the HAST text node
   directly — it is NOT limited to element tags. **Note on code:** in hast a
   code span/block's text IS a child text node of the `code` element — so a
   bare text-node override WOULD chip `@handle` inside backticks. Non-chipping
   in code is therefore an explicit contract of the `code` override (fork (d)):
   both the inline and block branches render from the hast node's raw text
   value (`node.children[0].value`), never from the mapped `children`, so the
   mention override never runs on code content. Link labels are the symmetric
   case, handled at their own seam rather than by text-node ancestry: the `a`
   component override (§Link safety below — the same override that routes opens
   through the Tauri opener) renders its label from the link node's raw text
   value, so the mention override never descends into link text. This covers
   every link label regardless of inline nesting (`[**@cook**](url)` too);
   solid-markdown's text hook receives only `{ node: Text }` (no ancestor
   pointer), so a text-node ancestry check could not. An autolinked email
   therefore never re-chips. Offsets are trivially
   correct: each prose text node is parsed independently, so `parseMentions`
   runs on exactly the string it chips — no cross-node offset math.
2. **Mention-first, markdown-per-run (REJECTED).** Segment the raw string on
   `parseMentions` spans, then render each plain run as markdown. This loses
   twice: (i) a mention inside a code span (`` `@cook` ``) gets chipped
   BEFORE markdown can see the backticks, so code spans grow mention chips —
   wrong; (ii) markdown constructs spanning a mention break — `**hey
   @cook!**` splits into the fragments `**hey`, `@cook`, and `!**`, and no
   fragment is
   valid emphasis, so the bold silently degrades to literal asterisks. Any
   fix requires re-joining runs with placeholder tokens and mapping offsets
   back through the markdown parse — the offset-drift trap. Not two-way;
   rejected outright.

*Renderer choice: `solid-markdown` 2.1.1 over a hand-rolled path.*
`solid-markdown` is Solid-native (a SolidJS port of react-markdown: "Replacing
React specific component creation with SolidJS components", its README), MIT,
peer `solid-js ^1.6.0` (compatible), remark-based (safe: renders an AST, never
`innerHTML`), and — decisive for us — supports **per-element component
overrides** via its `components` map, which is precisely the seam forks (b)
and (d) need: a text-node override for mention chips and a `code` override
for Shiki. It also exposes `renderingStrategy: "reconcile"`, which diffs
successive markdown ASTs via a Solid store + `reconcile` and re-renders only
changed parts — load-bearing for fork (c)'s growing string (default `"memo"`
rebuilds the full DOM per parse). A hand-rolled path (e.g. `marked` +
manual JSX) would re-implement the component-override seam and the streaming
reconcile for no size win worth the maintenance; rejected.

*GFM is opt-in.* `solid-markdown` bundles only CommonMark
(remark-parse/remark-rehype); `remark-gfm` is a devDependency, so tables,
strikethrough, task lists, and bare-URL autolinks are OFF by default. For an
agent-authored chat surface this is a product fork, surfaced as OQ-6
(recommendation: ship `remarkPlugins: [remark-gfm]`, ratified OQ-6). GFM
autolink-literal interacts with mentions — `user@host.com` becomes an `<a>`
whose label text still contains `@host.com`, which the raw `MENTION_RE` would
match (`comms.ts:237`). **Mechanism:** the `a` component override renders
its label from the link node's raw text value, never through the mention
override, so autolinked emails and every link label stay verbatim;
`parseMentions` runs only on non-link prose runs — no text-node ancestry
check (solid-markdown's text hook has no ancestor pointer). T4 case (9) pins that `user@host.com` renders a plain
autolink with no mention chip.

*Chip-parity fine print.* The `text` override receives the DECODED string, so
entity/escape-decoded `@` sequences (`&commat;cook`, a `\@`-adjacent run)
newly chip where today's raw-string `MentionText` (`ChannelView.tsx:41-64`)
does not. Cosmetic delta; accepted.

*Integration point.* Exactly one: the `Block` text arm
(`ChannelView.tsx:158-162`) swaps `MentionText` for the new
`MarkdownText` component (markdown tree + mention-chipping text renderer +
Shiki code renderer). `MentionText`'s chip logic is extracted to a shared
helper; the component itself remains for any plain-text call sites (the
composer preview, if any) or is deleted if `Block` was its only consumer —
executor's call at impl time. `AskBlock` and the ask contract are untouched
(`ChannelView.test.tsx`'s ask suite must stay green).

*Link + image safety (Tauri webview).* This record newly renders
message-authored `<a href>` / `<img src>` inside the Tauri shell, where a bare
anchor NAVIGATES THE APP and an `<img>` fetches an arbitrary remote URL
(tracking-pixel / IP-leak class). `solid-markdown` exposes the JS-side seams
(`linkTarget`, `transformLinkUri`, `transformImageUri`, `disallowedElements`).
Contract: links open externally via the Tauri opener (never in-app
navigation) through the `a` component override; images are disallowed by
default (or `src`-transformed to an allowlist) — a product fork surfaced as
OQ-7 (recommendation: ship images-disallowed, revisit on evidence).
**Dependency + seam.** The external-open call needs `@tauri-apps/plugin-opener`
(`openUrl` from `@tauri-apps/plugin-opener` `^2`; the app today carries only
`@tauri-apps/api`, verified) added to T1, and the `a` component override in
T5 that intercepts anchor activation and routes to `openUrl` — the same
override that renders link labels verbatim, so mentions never chip in link
text. The
**native side** (the Rust `tauri-plugin-opener` registration + the opener
capability/permission in the Tauri shell's `capabilities`) lands with the
compass Tauri shell lane (RIG-1022; no `src-tauri` crate exists on main yet,
verified) — this lane's T1/T5 own the JS dep + interception and the impl notes
the native capability as a cross-lane dependency, so the no-navigation
behavior is not falsely claimed deployable before the shell wires the
permission. T4 pins that a message link does not navigate the app (the
interception handler fires); the end-to-end external open is a shell-lane
integration check. Until RIG-1022 wires the native opener capability, this
lane's merge delivers only the interception + no-navigation guarantee: an
activated link neither navigates the app nor opens externally (`openUrl`
rejects with no registered capability). T6 acceptance and the PR body must
state this, so link-open is not marked done on a green CI that never
exercises the native path.

### Fork (c) — Mid-stream partial markdown (a growing string, not a token stream)

**Decision: re-parse the full (growing) string each render with
`renderingStrategy: "reconcile"` for DOM stability, plus a tiny
pre-parse *fence-closing normalizer* so an unterminated construct renders as
its best valid partial — never a broken-markdown flash.**

*The real observable — stated as a seam.* The ticket says the agent turn
"streams token-by-token", but that describes the future live path. Today
there is NO token-append API: a text block is a plain string
(`ConvBlock = { kind: "text"; text: string } | { kind: "ask"; ask: Ask }`,
`comms-stub.ts:148-150`), store reactivity is at the `store.messages()`
accessor granularity, and the store is the documented fixture seam
("This is a dev mockup: the store reads the in-memory fixture … the accessors
below stay and their bodies swap the fixture for the generated
@compass/client — the AppStore contract is the seam", `store.ts:10-13`). A
search for `delta`/`append`/`streaming` methods in `store.ts` returns nothing
(verified this session). **The mid-stream requirement is therefore: a text
block whose `.text` is a LONGER string on each successive render must render
a valid partial tree every time.** The renderer is designed against that
growing-string contract, so when the live path later re-emits
`store.messages()` per chunk, nothing in the renderer changes.

*Why naive re-parse alone is not enough.* CommonMark parses any string to
*some* tree, so there is no "invalid markdown" crash — the failure mode is a
**semantic flash**: an unterminated ``` fence mid-stream makes everything
after it a code block for one render, then snaps back to prose when the
closing fence arrives; a half-typed `**bold` renders literal asterisks that
later become emphasis. The asterisk case is acceptable churn (plain text →
styled text, no layout explosion). The fence case is NOT (prose ↔ full-block
code flip-flop, large layout shift inside the pinned last thread).

*The strategy, in order of leverage:*

1. **Fence-closing normalizer** (pure function, unit-testable):
   `closeOpenFence(text: string): string`. A naive odd-count scan is wrong (a
   closing fence must match the opener's CHAR and be at least its LENGTH; a
   `~~~` line inside an open ` ``` ` fence is content, not a fence; a run
   indented ≥4 spaces is indented code; an info string marks an opener only).
   So it is a small CommonMark line-scanner tracking
   `(open?, fenceChar, fenceLen, containerPrefix)`: when closed, an unindented
   run of ≥3 identical `` ` ``/`~` opens a fence (record char + length); when
   open, a matching-or-longer run of the same char closes it. If still open at
   EOF, append `\n` + `containerPrefix` + `fenceChar` repeated `fenceLen`
   times. Result: the in-progress code block renders AS a code block growing
   line by line (the correct partial), never flipping. **Scope:** a fence
   opened inside a blockquote/list closes implicitly at container end, so the
   residual flash there is bounded to that container, not the document —
   accepted; the normalizer targets top-level fences (the catastrophic
   full-document flip). Other partials (emphasis, links) degrade as literal
   text and need no normalization.
2. **`renderingStrategy: "reconcile"`** on `SolidMarkdown`: successive parses
   diff via a Solid store + `reconcile`, so the stable prefix of the tree
   keeps its DOM nodes and only the tail re-renders — no full-subtree
   teardown per growth tick (solid-markdown README: "diff the previous and
   next markdown ASTs and only trigger re-renders for the parts that have
   changed").
3. **Highlight-async-with-plain-fallback** for code (fork (d)): the code
   renderer shows the plain `<pre><code>` text immediately and swaps in
   Shiki's highlighted output when the (async) highlighter resolves — growth
   never blocks on highlighting. Two interactions the swap must handle: (i)
   **stale resolutions** — a streaming fence re-renders per growth tick, each
   kicking a fresh async highlight, so resolution is guarded last-write-wins
   keyed by the code text identity; a resolution whose source text is no
   longer current is dropped. (ii) **height invariance** — the plain fallback
   pins `white-space: pre` and the same font metrics as the highlighted
   `<pre>` so the swap is height-identical at a given width; where a
   historical row's highlight resolves off-screen and does shift height, that
   is a mid-list resize handled by the virtualizer's
   `shouldAdjustScrollPositionOnItemSizeChange` (fork (a)), not a silent yank.

*Rejected alternative:* an incremental/streaming markdown parser (e.g. a
marked-lexer hand-feed or a streaming-markdown package) that consumes only
the appended suffix. Rejected because (i) the observable is a whole string,
not a delta — we would diff strings to synthesize deltas the store doesn't
emit; (ii) message bodies are chat-sized (KBs, not MBs), so full re-parse per
tick is well inside frame budget; (iii) it forfeits solid-markdown's
component-override seam that forks (b)/(d) depend on.

### Fork (d) — Shiki bundling (Tauri app size)

**Decision: fine-grained bundle — `createHighlighterCore` from `shiki/core`
with the JavaScript regex engine (`shiki/engine/javascript`) and an explicit
initial set of languages/themes. Never the full bundle.**

Shiki 4.3.1's pre-composed bundles are disqualifying for a desktop app
payload: the full bundle is 6.4 MB minified / 1.2 MB gzip, the web bundle
3.8 MB / 695 KB (shiki.style/guide/bundles), versus ~12 KB core plus only the
grammars/themes actually imported. The Compass UI ships inside the Tauri
shell; multiplying the app bundle several-fold for unhighlighted languages is
not on the table. Concretely:

- `createHighlighterCore` from `shiki/core`, langs/themes as **dynamic
  imports** from `@shikijs/langs/*` / `@shikijs/themes/*` so Vite splits them
  into async chunks resolved on first use.
- **Engine: `createJavaScriptRegexEngine()`** — no WASM asset to ship or load
  in the Tauri webview, smaller and faster to start than the Oniguruma+WASM
  engine. The JS engine is expected-compatible with the 13 grammars below
  (Shiki publishes a per-grammar JS-engine compatibility table; a mismatch
  surfaces as mis-tokenization, not an error, and T4 case (6) plus the escape
  hatch cover a miss). Escape hatch: if a grammar misbehaves, swapping to
  `createOnigurumaEngine(import('shiki/wasm'))` is a one-line change at the
  highlighter singleton.
- **Initial language set** (the code this UI actually shows — Compass/seal
  stack): `typescript`, `tsx`, `javascript`, `json`, `bash`, `go`, `rust`,
  `python`, `yaml`, `toml`, `sql`, `diff`, `markdown`. Unknown/absent lang
  tags fall back to plain `<pre><code>` (no highlight, no error).
- **Themes:** one dark + one light (`github-dark-default` /
  `github-light-default`), matching the UI's existing dark-leaning palette;
  exact pick is a non-load-bearing OQ (OQ-4).
- **Singleton:** one lazily-created highlighter instance module-wide
  (`highlighter.ts`), never per-component — a highlighter loads grammars +
  themes and holds an engine, so Shiki's guidance is to create one and reuse
  it; the async-create races are contained in one place. The `code` component
  override in fork (b)'s `components` map calls it with the
  plain-text-fallback contract from fork (c).

*Rejected alternative:* full `shiki` bundle import — 1.2 MB gzip for mostly
unused grammars; and `shiki/bundle/web` — still 695 KB gzip, still mostly
unused, and it forecloses the engine choice.

### Rider — stale ACP comment cleanup

The `Block` doc comment still describes the block split in ACP vocabulary:
"The rich ACP blocks (thought/tool_call/plan/diff) are not in the channel;
they live in the session observation panel" (`ChannelView.tsx:150-152`).
Compass-0.8 ruled the trace "not opaque bytes, and explicitly **not ACP**"
(`compass-0.8-threading-and-session-renderer/design.md:19-20`; also "typed
event shape — first-party, ACP-informed, not ACP-bound", `:209`) — the
comment's vintage is stale. Since this change rewrites the
`Block` text arm anyway, the impl rewrites the comment to OMP-native wording
(e.g. "The rich session blocks (thought/tool_call/plan/diff) are not in the
channel; they render in the session observation panel"). Guard-safe: the
design-citations gate forbids only `/channel-(first|primary)/i` and
`/design\s+compass-0\.6\b/i` (`design-citations.test.ts:38-48`) — neither
idiom appears in the rewrite. Whether this rider instead defers to franklin's
comment sweep is OQ-2.

## Alternatives considered

Library-level rejections (per the ticket's non-goals), then the per-fork
alternatives already argued inline in §Approach.

- **Chat-in-a-box mega-components (Deep Chat, Loquix, and kin):** rejected —
  ticket non-goal. They own the whole surface (composer, list, bubbles),
  which collides with the frozen #783 component tree, the `AskBlock` contract
  (first-responder-wins, settled-lock — defended by `ChannelView.test.tsx`),
  and the Compass-specific mention syntax. We need two narrow capabilities,
  not a chat product.
- **Kobalte / corvu a11y primitives:** deferred — ticket non-goal. Nothing in
  this change needs a11y primitives; adopting them is its own decision with
  its own record when interaction surfaces (menus, dialogs) demand it.
- **React virtualization/markdown kits (react-window, react-virtuoso,
  react-markdown, react-shiki):** out — the app is SolidJS `^1.9.13`
  (`package.json:10`); a React compatibility layer for two leaf concerns is
  strictly worse than the Solid-native equivalents chosen. Both picks are MIT
  and Solid-native/compatible (`@tanstack/solid-virtual` peer
  `solid-js ^1.3.0`; `solid-markdown` peer `solid-js ^1.6.0`).
- **Per-fork rejected alternatives** (argued where the context lives, in
  §Approach): flat list + hand-rolled stick-to-bottom, `column-reverse`
  inversion, and message-granularity virtualization (fork a);
  mention-first-then-markdown-per-run composition and a hand-rolled
  marked-based renderer (fork b); an incremental/delta-feeding markdown
  parser (fork c); the full or web Shiki bundle and the Oniguruma/WASM
  engine as the default (fork d).

## Plan

### Global Constraints

Every task below inherits these; task briefs do not restate them.

- **Version floors:** `solid-js ^1.9.13` (existing); new deps
  `@tanstack/solid-virtual` `3.13.34`, `solid-markdown` `2.1.1`, `shiki`
  `4.3.1` (registry-latest verified 2026-07-21), plus `remark-gfm` (OQ-6
  ratified — see Open Questions). They go **directly in
  `apps/ui/package.json` dependencies**, NOT the root catalog —
  the catalog carries only shared toolchain deps (root `package.json:26-31`);
  app runtime deps are pinned in the app (`@tauri-apps/api: "^2"`,
  `solid-js: "^1.9.13"`, app `package.json:8-10`).
- **No `AppStore` contract or component-tree change** (#783 frozen).
  `MentionText` chip semantics and the whole `AskBlock` ask contract are
  preserved; the existing `ChannelView.test.tsx` suite stays green.
- **Guard-clean:** no new source line matches
  `/channel-(first|primary)/i` or `/design\s+compass-0\.6\b/i`
  (`design-citations.test.ts:38-48`).
- **Rebase-onto-franklin:** implementation branches from franklin's landed
  RIG-1337 ChannelView restructure (PR #841) and takes the
  `.conv-body-row`/`.conv-main` wrapper as found; no task edits the wrapper.
- **Tests: `moon run compass-ui:ci`** = typecheck + build + test
  (`moon.yml:32-35`); test = `bun test --conditions browser
  --pass-with-no-tests` (`moon.yml:29`) — the browser condition is
  load-bearing (Bun's default `node` condition pulls solid-js's SSR build,
  `moon.yml:25-28`). Red→green per `rule://red-green-testing`: tests first,
  watch them fail, then implement.
- **No planning metadata in source** — RIG-1332 appears in commit subjects /
  PR body only, never in code comments.

### T1 — Dependencies

Add the deps to `apps/ui/package.json` `dependencies`
(`@tanstack/solid-virtual`, `solid-markdown`, `shiki`, `remark-gfm`,
`@tauri-apps/plugin-opener` for external link opening — §fork (b) link safety)
and refresh the lockfile; `moon run compass-ui:ci` green (build proves Vite
resolves them, including shiki's dynamic-import chunks). If the installed
`@tanstack/virtual-core` resolves below `3.17.5` (the chat-mode floor,
§fork (a)), pin it explicitly.

Interfaces: consumes registry packages `@tanstack/solid-virtual@3.13.34`
(exports `createVirtualizer`), `solid-markdown@2.1.1` (exports
`SolidMarkdown: Component<SolidMarkdownOptions>` with `components`,
`renderingStrategy` props), `shiki@4.3.1` (subpath exports `shiki/core` →
`createHighlighterCore`, `shiki/engine/javascript` →
`createJavaScriptRegexEngine`, `@shikijs/langs/*`, `@shikijs/themes/*`).
Produces: the resolvable module graph every later task imports.

### T2 — Red tests: the scroll contract

Write the failing behavioral suite for fork (a) before any scroller code,
mounting `ChannelView` via `@solidjs/testing-library` under happy-dom (the
existing pattern, `ChannelView.test.tsx`). Cases: (1) opening a channel lands
at latest (end-anchored mount); (2) append while at bottom → view follows;
(3) append while scrolled up past `scrollEndThreshold` → view does NOT move;
(4) prepending older messages preserves the visible thread's position
(root-id keyed anchor); (5) only a bounded window of thread nodes is in the
DOM for a long channel (virtualization actually engaged); (6) switching the
selected channel resets the view to that channel's latest (the channel-id
`createEffect`, not a remount — §fork (a)). happy-dom has no
real layout, so tests drive the virtualizer through injected element-size
mocks/`measureElement` stubs — the suite pins the contract, not pixel math.

Add case (7): the virtualizer is CONSTRUCTED with the chat-mode config —
`anchorTo: 'end'`, `getItemKey` returning the thread root id, and
`measureElement` registered on rendered rows — asserted directly, so a config
regression is caught even where happy-dom's absent layout hides the math. What
happy-dom cannot exercise (`ResizeObserver` from layout, real
`scrollHeight`/`scrollTop` coupling, post-#841 scroll-element identity) is
named as a manual-QA / vitest-browser-mode follow-up in T6.

Interfaces: consumes `threadsOf(messages, channelId): Thread[]`
(`comms.ts:195-219`), `Thread { root: Message; replies: Message[] }`
(`comms.ts:153-156`), fixture builders from `comms-stub.ts`. Produces:
`ChannelView.scroll.test.tsx` (red), the executable statement of fork (a).

### T3 — Virtualized thread list (turns T2 green)

Replace the bare `<Index each={threads()}>` inside `.conv-stream` (post-#841
L358) with the virtualizer structure: `.conv-stream` ref as scroll element,
inner relative sizer div at `getTotalSize()` height, `<For
each={virtualizer.getVirtualItems()}>` rendering `ThreadView` rows absolutely
positioned by `item.start`, each registering `measureElement`. Add the
channel-id `createEffect` that resets the virtualizer to the end on selected-
channel change (§fork (a) — initial mount + switch). Empty/join
fallbacks unchanged.

Interfaces: consumes `createVirtualizer({ count, getScrollElement,
estimateSize: (i) => number, getItemKey: (i) => threads()[i].root.id,
anchorTo: 'end', followOnAppend: true, scrollEndThreshold: 80, overscan })`
from `@tanstack/solid-virtual`; `ThreadView: Component<{ thread: Thread;
byId: Map<string, Account>; byHandle: Map<string, Account> }>`
(`ChannelView.tsx:203-207`, unchanged). Produces: the virtualized
`.conv-stream` internals; T2 suite green; existing `ChannelView.test.tsx`
still green.

### T4 — Red tests: markdown + mentions + mid-stream

The failing suite for forks (b)/(c)/(d)'s composition rules: (1) a text block
with `**bold**`, lists, links renders semantic HTML; (2) `@cook` in prose
chips (class `mention-chip`, known/reserved/unknown variants exactly as
`MentionText` does today, `ChannelView.tsx:70-82`); (3) `` `@cook` `` inside
a code span does NOT chip; (4) `**hey @cook!**` renders bold AND chips the
mention inside it; (5) a growing string ending in an unterminated ``` fence
renders a code block at every growth step (never a prose-flip); (6) a fenced
block with a known lang eventually carries highlighted tokens, and an
unknown lang stays plain `<pre><code>`; (7) plain-fallback shows immediately
before async highlight resolves. Plus a unit suite for the pure
`closeOpenFence` normalizer (odd/even fences, `~~~`, fence-in-fence, inline
backticks ignored).
Additionally: (8) a markdown link in a message routes through the `a`
component override and does NOT navigate the app (per fork (b) link safety);
(9) a bare `user@host.com` under GFM autolink renders a plain link with no
mention chip, and (9b) a link label with inline markup (`[**@cook**](url)`)
renders as plain label text with no chip — the `a` override renders labels
from raw text value, so emphasis inside a label is intentionally flattened
(accepted tradeoff, §fork (b)) and the mention never chips. The `closeOpenFence` units
add fence char/length mismatch (a triple-backtick close does not terminate an
open `~~~` fence) and a tilde-fence-nested-in-backtick-fence case.

Interfaces: consumes `parseMentions(text): Mention[]` (`comms.ts:244`),
`Mention { handle; reserved; start; end }` (`comms.ts:227-232`),
`blockText(block: ConvBlock): string` (`comms.ts:263-265`). Produces:
`MarkdownText.test.tsx` + `markdown-stream.test.ts` (red).

### T5 — Markdown/mention/code renderer at the Block text arm (turns T4 green)

Build the renderer stack and swap it into `Block`'s fallback arm
(`ChannelView.tsx:158-162`, post-#841 position). New modules under
`src/components/` (+ `src/highlighter.ts`): the shared mention-run splitter
extracted from `MentionText`, the `MarkdownText` component
(`SolidMarkdown` with `renderingStrategy: "reconcile"`, text-node override →
mention chips, `code` override → Shiki with plain fallback, input passed
through `closeOpenFence`), and the module-singleton highlighter (fine-grained
core + JS regex engine + the §fork-(d) lang/theme set). Rewrite the stale ACP
doc comment at the `Block` site to OMP-native wording (per §Rider, unless
OQ-2 defers it). Delete `MentionText` if `Block` was its only consumer, else
repoint it at the shared splitter.

Also in T5: **code override contract** — both the inline (`inline: true`) and
block branches render from the hast node's raw text value
(`node.children[0].value`), never the mapped `children`, so mentions never
chip inside code (the mechanism §fork (b) relies on). **Markdown-content CSS**
(app.css, after `.thread-replies` at L2956; today `.msg-text` at L2968 is
`white-space: pre-wrap` only): heading demotion inside `.msg`, `<p>` margins,
`<pre>`/code `overflow-x` for unbounded code lines inside measured rows, and
chip styles surviving inside `em`/`strong`. No test sees CSS, so it is named
scope here rather than discovered mid-impl. **Link/image safety** per fork
(b): the `a` component override routes anchor activation to `openUrl` from
`@tauri-apps/plugin-opener` (never in-app navigation) and renders link labels
from raw text value, so mentions never chip inside a link (no text-node
ancestry check — the text hook has no ancestor pointer); images
disallowed-or-transformed. Native opener capability is the RIG-1022 shell
lane's (cross-lane dependency, not owned here); until it lands, an activated
link neither navigates nor opens (`openUrl` rejects) — this lane ships the
interception guarantee only.

Interfaces: consumes `SolidMarkdown` (`components` map keyed by hast tag
names — `code`, `a`, and the text override; `renderingStrategy: "reconcile"`),
`createHighlighterCore({ themes, langs, engine }) →
Promise<HighlighterCore>` with `codeToHtml(code, { lang, theme })`;
the shared splitter `mentionRuns(text: string, byHandle: Map<string,
Account>): { text: string; mention?: { handle: string; known: boolean;
reserved: boolean } }[]` (the exact shape `MentionText` builds today,
`ChannelView.tsx:41-64`); `closeOpenFence(text: string): string`. Produces:
`MarkdownText: Component<{ text: string; byHandle: Map<string, Account> }>`
(drop-in for `MentionText`'s props, `ChannelView.tsx:35-38`), wired at the
`Block` text arm; T4 green.

### T6 — CI + guard sweep

Full `moon run compass-ui:ci` green: typecheck, Vite build (verifying shiki
chunk splitting and bundle deltas), the whole test suite including
`design-citations.test.ts` and the pre-existing `ChannelView.test.tsx` ask
contract. Record the built `dist/` size delta in the PR body (fork (d)'s
acceptance evidence).

Acceptance also names a manual-QA pass (or a vitest browser-mode follow-up)
for the real-webview scroll behaviors happy-dom cannot exercise —
follow-at-bottom, no-yank-when-scrolled-up, prepend-no-jump, and that
`.conv-stream` stays the actual scroll element post-#841.

**Link-open completion gate (cross-lane).** External link-opening is not
"done" on this lane's merge. This lane delivers only the interception +
no-navigation guarantee — `openUrl` rejects until the RIG-1022 shell lane
registers `tauri_plugin_opener::init()` and grants the opener capability, so
every rendered link is inert (intercepted, neither navigating nor opening)
on an otherwise-green CI. The link-open feature is marked complete only when
an RIG-1022-owned end-to-end integration check (real webview: activate a
message link → external open, no in-app navigation) passes. That check is a
required gate on the RIG-1022 shell lane and a named prerequisite dependency
of this record's link contract, tracked as a blocking cross-lane edge — not a
box this lane's green CI can tick. The PR body states this explicitly so
link-open is never reported done on the strength of this PR alone.

Interfaces: consumes the `ci` task graph (`moon.yml:32-35`). Produces: the
merge-ready implementation PR (RIG-1332 in subject/body only).

## Tasks

Execution order is T1 → (T2, T4 in either order; both red before their green
task) → T3 → T5 → T6. T3 and T5 are independent once red suites exist and may
land as stacked PRs.

- [ ] T1 — add `@tanstack/solid-virtual` 3.13.34, `solid-markdown` 2.1.1,
  `shiki` 4.3.1, `remark-gfm`, `@tauri-apps/plugin-opener` to the app
  `package.json`; lockfile; ci green.
- [ ] T2 — red scroll-contract suite (`ChannelView.scroll.test.tsx`): mount
  at end, follow-at-bottom, no-yank-when-scrolled-up, prepend-no-jump,
  bounded DOM window, channel-switch-resets-to-latest, and chat-mode wiring
  (anchorTo / getItemKey / measureElement).
- [ ] T3 — virtualized thread list inside post-#841 `.conv-stream`
  (end-anchored, root-id keyed, measured, channel-id reset effect); T2 green,
  existing suite green.
- [ ] T4 — red markdown suite (`MarkdownText.test.tsx`,
  `markdown-stream.test.ts`): semantics, mention chips in prose, no chips in
  code (inline + block), mention-inside-emphasis, fence-stable growth,
  highlight + fallback, link-does-not-navigate, GFM autolink vs mention,
  `closeOpenFence` units (char/length mismatch, `~~~`, fence-in-fence).
- [ ] T5 — `MarkdownText` (solid-markdown reconcile + mention text override +
  code override rendering from raw node value + `closeOpenFence` + link/image
  safety) at the `Block` text arm; shared mention splitter; highlighter
  singleton; markdown-content CSS; stale ACP comment rewritten (OQ-2
  permitting); T4 green.
- [ ] T6 — `moon run compass-ui:ci` green end to end; guard clean; bundle
  delta recorded in PR body; manual-QA / browser-mode note for real-webview
  scroll.

## Open Questions

Batched for Matt (this record's author cannot prompt him); each carries a
recommendation and proceeds on it unless overruled. **OQ-1 and OQ-6 (the two
load-bearing ones) were ratified by Matt on 2026-07-21, both as recommended**
— folded into the record above; the other four proceed on their defaults.

- **OQ-1 (load-bearing; RATIFIED — Matt, 2026-07-21: code is verbatim, never
  chip).** Fork
  (b) chooses markdown-first with a text-node mention override, which means
  `@handle` inside code spans/blocks does NOT chip (code is verbatim), while
  today's `MentionText` chips it anywhere in the string
  (`ChannelView.tsx:41-64` slices the raw text with no code awareness). This
  is a forward-looking behavior choice with an **empty blast radius on
  existing content** — zero fixture message texts contain a backtick
  (`comms-stub.ts`, verified) — not a change to any shipped message. The
  non-chipping is delivered by the `code` override rendering from the node's
  raw text (§fork (b) mechanism), not by the library. Recommendation: accept
  — chipping inside code is wrong once code rendering exists; the alternative
  (mention-first) breaks markdown across mention boundaries (§fork (b)). Needs
  Matt's ratification because it sets observable rendering policy for mentions
  in code.
- **OQ-2 (non-load-bearing; deferred-decision, not blocking impl order) —
  who owns the stale ACP comment rewrite.** The `Block` doc comment
  (`ChannelView.tsx:150-152`) is stale ACP vocabulary; franklin's RIG-1337
  lane may run its own comment sweep. Recommendation: this lane rewrites it —
  T5 rewrites the surrounding code anyway, and a one-comment dependency on
  another lane's sweep is coordination overhead for nothing. If franklin's
  sweep lands first, T5 simply finds it already fixed. Rationale for
  non-load-bearing: either owner produces the same end state.
- **OQ-3 (non-load-bearing; product-feel, defaulted) — the at-bottom
  threshold.** `scrollEndThreshold: 80` px (≈ one message row) decides when
  the view counts as "at latest" for follow-on-append. Pure feel; any value
  30-150 is defensible. Recommendation: ship 80, tune by hand once the
  virtualized surface is usable. Also parked here: a "jump to latest" pill
  driven by `!isAtEnd()` is the natural follow-on affordance but is scope
  creep for RIG-1332 — recommend a follow-up ticket, not this lane.
- **OQ-4 (non-load-bearing; cosmetic, defaulted) — Shiki theme pair.**
  Recommendation: `github-dark-default` + `github-light-default` as the
  initial pair (neutral, maintained, matches the UI's palette direction);
  swapping themes later is a two-line change at the highlighter singleton.
- **OQ-5 (load-bearing only if answered "yes") — initial language set
  breadth.** Fork (d) ships 13 grammars (§fork (d) list). If Matt wants
  arbitrary-language highlighting from day one, the fine-grained choice
  stands but the set grows (or lazy-loads on unknown tags via a dynamic
  `import()` map). Recommendation: ship the 13; unknown langs render plain —
  correct, just uncolored — and grammars are added by evidence of use.
- **OQ-6 (load-bearing; RATIFIED — Matt, 2026-07-21: ship `remark-gfm`).**
  `solid-markdown` ships CommonMark only; GFM (tables, strikethrough, task
  lists, bare-URL autolinks) is opt-in via `remarkPlugins: [remark-gfm]`
  (`remark-gfm` is a `solid-markdown` devDependency, so it becomes an added
  app dep). Agent-authored chat routinely uses tables and bare URLs, so
  CommonMark-only renders literal pipes and unlinked URLs. Recommendation:
  ship `remark-gfm`. Load-bearing because it changes what renders AND
  interacts with mentions (GFM autolink-literal turns `user@host.com` into a
  link, altering the text nodes the mention override sees — T4 pins it). If
  Matt prefers CommonMark-only, the mention/autolink interaction disappears
  and T4's autolink case is dropped.
- **OQ-7 (non-load-bearing; product, defaulted) — do agent-authored `<img>`
  render at all.** The link+image safety contract (§fork (b)) disables image
  rendering by default (or `src`-transforms to an allowlist). This is a
  product fork of the same class as OQ-6: agents legitimately emit
  screenshots/diagrams, so "images never render" has real product weight.
  Recommendation: ship images-disallowed, revisit on evidence — the default is
  defensible and reversible (a one-line `disallowedElements` change). Surfaced
  as an OQ so the fork is on the frozen record rather than defaulted in prose.
