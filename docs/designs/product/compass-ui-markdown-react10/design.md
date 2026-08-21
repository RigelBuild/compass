# Adopt react-markdown-10 rewrite (solid-markdown #44 + #45) into @rigelbuild/solid-markdown

Status: Draft

Supersedes: DL-208's `solid-markdown` clause (Matt's ruling, 2026-08-19).
DL-207, DL-209, and DL-208's `@tanstack/solid-virtual` clause are unaffected.
Parent record: [compass-ui-solid-v2](../compass-ui-solid-v2/design.md)
(RIG-2187), whose Resolved decision 2 this record partially supersedes.

## Problem / Intent

Matt ruled (2026-08-19) that the owned `RigelBuild/solid-markdown` fork adopts
BOTH unmerged upstream PRs — `andi23rosca/solid-markdown` #44 (the
react-markdown-10 API rewrite, a draft) and #45 (the bundle-deps jsdom fix) —
NOW, before the fork tags and publishes, re-ported to Solid 2, with the compass
`apps/ui` consumer rewritten to match. This overrides the active DL-208 ledger
decision, which had bound the fork to "the `solid-js` 2.x peer bump + Solid-2
codemods (imports + effect lifecycle, no algorithmic rewrite, no in-repo AST
renderer)" — #44 is precisely that algorithmic renderer rewrite. Per the
immutable-ledger-cell convention this record supersedes DL-208's solid-markdown
clause with a new ledger row (DL-218) and a clause-scoped Status flip, rather
than rewriting DL-208's decision text.

The consumer rewrite is forced because #44 removes the `text` component
override compass uses to inject @mention chips — a core product feature
(mention-first composition, DL-041→DL-098 lineage) — and removes the `inline`
prop the `code` override reads.

## Approach

Adopt #44 + #45 onto a new fork branch based on fork `main` (`66c00a5`, ==
upstream), replacing the current `solid-2-support` port (`8977c9e4`), then
re-port the result to Solid 2. Publish as `@rigelbuild/solid-markdown@3.0.0`.
The compass consumer moves mention injection into a consumer-side rehype
plugin over the existing `mentionRuns` splitter, and derives inline-vs-block
code from the `pre`/`code` override split instead of the removed `inline` prop.

### A1 — What #44 and #45 actually change (grounded)

**#44** deletes `src/renderer.tsx`, `src/rehype-filter.ts`, `src/utils.ts` and
rewrites `src/index.tsx`/`src/types.ts` to the react-markdown-10 model: a
default `Markdown` export plus `MarkdownAsync`/`MarkdownResource`, with
rendering delegated to `hast-util-to-jsx-runtime` over a JSX-runtime triple
imported from the third-party `solid-jsx` package (`pr44.diff:5094,5107`):

```ts
import { toJsxRuntime } from "hast-util-to-jsx-runtime";
import { Fragment, jsx, jsxs } from "solid-jsx/jsx-runtime";
```

Legacy props now throw at runtime — `checkOptions` calls `unreachable` for
every entry in the `deprecations` table (`source`, `renderers`, `class`,
`transformLinkUri`, … — `pr44.diff:5208-5242,5515-5533`). The `Components`
type is HTML-tags-only — no `text` key (`pr44.diff:6070-6074`):

```ts
export type Components = {
  [Key in keyof JSX.IntrinsicElements]?:
    | ((props: JSX.IntrinsicElements[Key] & ExtraProps) => JSX.Element)
    | keyof JSX.IntrinsicElements;
};
```

`renderingStrategy` survives on the sync `Markdown` only, deprecated
(`pr44.diff:5143-5148`: "`@deprecated` Solid-specific compatibility prop. It
will be removed in the next major release."). The pipeline keeps
`allowDangerousHtml: true` unconditionally (`pr44.diff:5136-5138`:
`const emptyRemarkRehypeOptions … = { allowDangerousHtml: true }`), and `post()`
retypes surviving `raw` nodes to `text` (or splices them under `skipHtml`)
before `toJsxRuntime` (`pr44.diff:5465-5473`).

**#45** is `package.json`-only: it moves the 9 runtime deps
(`comma-separated-tokens`, `property-information`, `remark-parse`,
`remark-rehype`, `space-separated-tokens`, `style-to-object`, `unified`,
`unist-util-visit`, `vfile`) from `dependencies` to `devDependencies`
(`pr45.diff:9-47`) so tsup bundles them (tsup externalizes `dependencies` +
`peerDependencies` only), fixing the Vitest+jsdom `util.deprecate` crash
(upstream issue #27).

### A2 — Solid-1 → Solid-2 re-port of #44 (the viability crux): VERDICT — works, via a fork-local jsx-runtime

PR #44 targets Solid 1 (`"solid-js": "^1.8.0"` peer, `pr44.diff:469-470`) and gets
its `jsx`/`jsxs`/`Fragment` triple from `solid-jsx@1.1.4`. Three facts, all
verified this session against the fork checkout's installed Solid 2 RC:

1. **`@solidjs/web@2.0.0-rc.0` ships no runtime JSX triple.** Its export map
   has a `./jsx-runtime` entry, but it resolves to the main bundle with
   types-only JSX (`node_modules/@solidjs/web/package.json`, `exports["./jsx-runtime"].import` =
   `{ "types": "./types/jsx.d.ts", "default": "./dist/web.js" }`), and
   `dist/web.js` exports no `jsx`/`jsxs`/`Fragment` (grep over both export
   statements: zero occurrences; the exports are `Dynamic`, `Portal`,
   `delegateEvents`, … — `web.js:1858`). So there is no first-party runtime to
   point `hast-util-to-jsx-runtime` at.
2. **`solid-jsx@1.1.4` does not run on Solid 2 as-is.** Its peer range
   (`solid-js: '>=1.4.0'`, `pr44.diff:2330-2331`) nominally admits 2.x, but its
   source (`high1/solid-jsx` `src/jsx-runtime.ts`, read this session) imports
   `mergeProps` from `solid-js` and `Dynamic` from `solid-js/web`:

   ```ts
   import { createComponent, createContext, mergeProps, … } from 'solid-js';
   import { Dynamic } from 'solid-js/web';
   ```

   Both break on Solid 2: `mergeProps` is renamed `merge` (Solid 2's
   `dist/solid.js` exports `merge`, `createComponent`, `createContext` — and
   zero `mergeProps` hits; the fork's own Solid-2 port already imports `merge`
   from `solid-js`, `solid-markdown/src/index.tsx:7`), and the `solid-js/web`
   subpath no longer exists (Solid 2's export map is only `.`,
   `./package.json`, `./refresh`, `./types/*`; the DOM runtime moved to the
   separate `@solidjs/web` package — the fork's renderer already imports
   `Dynamic` from there, `solid-markdown/src/renderer.tsx:5`).
3. **The triple is small and framework-shaped, so the fork owns it.**
   `solid-jsx`'s entire runtime is ~100 lines: `Fragment` returns
   `props.children`; `jsx(type, props)` calls `type(props)` for function
   components and `createComponent(Dynamic, mergeProps(props, { component:
   type }))` for tag strings; `jsxs = jsx` (source read this session). Every
   primitive it needs exists in Solid 2 under a new name/home.

**Re-port design:** drop the `solid-jsx` dependency entirely and add a
fork-local `src/jsx-runtime.ts` exporting the `jsx`/`jsxs`/`Fragment` triple
over Solid-2 primitives (`createComponent`/`merge` from `solid-js`, `Dynamic`
from `@solidjs/web`), minus solid-jsx's MDX-only surface (`MDXProvider`,
`useMDXComponents`, the `mjx-` dash-to-underscore compat cache — MDX machinery
`hast-util-to-jsx-runtime` never exercises). `hast-util-to-jsx-runtime` is
framework-agnostic — it calls whatever `jsx`/`jsxs`/`Fragment` it is handed
(`pr44.diff:5447-5458` passes them in with `elementAttributeNameCase: "html"`,
`stylePropertyNameCase: "css"`); `passKeys` keys arrive as a third argument the
Solid runtime legitimately ignores (Solid has no keyed VDOM diff). THREE
further Solid-2 subpath deltas live in #44's own code — all mechanical, all
caught by F2's `lint:types`/`test:ssr`, named here so the implementer does not
read the compile failures as surprises: (1) its single-arg
`createRenderEffect(() => { … setReconciledTree(reconcile(…)) })`
(`pr44.diff:5269-5273`) → Solid 2's two-arg (compute, effect) form, as the
current port already did (`solid-markdown/src/index.tsx:93-100`); (2)
`import { createStore, reconcile } from "solid-js/store"` (`pr44.diff:5104`) →
core `solid-js` (Solid 2 dropped the `./store` subpath; the current port
already imports them from `solid-js`, `src/index.tsx:3-9`); (3)
`renderToString`/`renderToStream` from `solid-js/web` in #44's SSR plugin +
test (`pr44.diff:6549,6635`) → `@solidjs/web` (`server.js:905,973`).
Additionally, the fork-local `jsx`/`jsxs` wraps function-component calls in
`createComponent` (= `untrack(() => Comp(props))`, `solid.js:966-968`) rather
than calling them bare as `solid-jsx` did, so a component override cannot
accidentally track the parent render scope.

This is a genuinely new (fork-local) module, justified explicitly: no existing
seam can carry it — the first-party runtime doesn't exist (fact 1) and the
third-party one is Solid-1-bound (fact 2). It lives in the fork we own, not in
compass, and keeps the published package a clean generic Solid-2 port.

### A3 — @mention chip injection: a consumer-side rehype plugin (option A)

Today compass injects chips via the `text` component override
(`apps/ui/src/components/MarkdownText.tsx:407-411`):

```tsx
const components: SolidMarkdownComponents = {
  // Prose text → mention chips (the composition seam).
  text: (p: { node: HastText }) => (
    <MentionRuns text={p.node.value} byHandle={props.byHandle} />
  ),
```

Under #44 that key no longer exists in `Components` (A1) and hast text nodes
render as raw strings inside `toJsxRuntime`, never routed through
`components` — the override would silently never fire. **Chosen approach:** a
new consumer rehype plugin (`apps/ui/src/markdown/rehype-mention-chips.ts`)
that walks the hast tree after `rehypeInertRawAndBreaks`, and for every text
node NOT inside a `code`/`pre` subtree runs the existing pure splitter
`mentionRuns(text, byHandle)` (`apps/ui/src/markdown/mention-runs.ts:20-41`)
and replaces the text node with an interleaved sequence of text nodes and
`span` elements:

```ts
{ type: "element", tagName: "span",
  properties: { className: ["mention-chip", /* "reserved" | "unknown" */] },
  children: [{ type: "text", value: run.text }] }
```

mirroring exactly the classes the `MentionRuns` component emits today
(`MarkdownText.tsx:285-293`: `class="mention-chip"` with `reserved` /
`unknown` modifiers). Chips become real hast elements before `toJsxRuntime`,
so they render with zero component overrides. The invariants hold:

- **Code stays verbatim.** The plugin inherits an `inCode` flag down
  `code`/`pre` subtrees and skips their text nodes — the same inherited-flag
  shape `rehypeInertRawAndBreaks` already uses (`MarkdownText.tsx:128-129`:
  "`inCode` is INHERITED: a code subtree is verbatim all the way down").
- **Link labels never render chips.** The `a` override renders its label from
  `rawText(p.node)`, which flattens element children back to their text
  (`MarkdownText.tsx:258-270` — `child.type === "element"` recurses), so even
  a chip span injected inside a label flattens back to the literal `@handle`;
  the label path renders no children (`MarkdownText.tsx:424-450`).
- **Raw-HTML text still chips.** Ordering the plugin AFTER
  `rehypeInertRawAndBreaks` in `rehypePlugins` preserves today's behavior:
  raws are retyped to `text` first (`MarkdownText.tsx:151-152`) and then chip
  like any prose text — under the old renderer those retyped nodes hit the
  `text` override the same way.

`byHandle` is passed as a plugin option. Reactivity note: `#44`'s processor is
a memo over `options` (`pr44.diff:5259`), so the consumer wraps the
`rehypePlugins` array in a `createMemo` keyed on `props.byHandle` — a new map
identity re-creates the processor and re-parses, refreshing chip
known/reserved state. This is coarser than today's per-chip reactive
resolution (`MentionRuns`'s `runs()` re-ran live on `byHandle` changes,
`MarkdownText.tsx:279`), an accepted tradeoff: account-map changes are rare
and a full re-parse of one message is what every streaming tick already costs.
The `MentionRuns` component is deleted; the `mentionRuns` splitter and its
tests are untouched.

### A4 — `code` inline derivation: the `pre` override owns blocks

react-markdown 9+ (and #44) passes no `inline` prop — the old fork's
`CodeComponent` typed it explicitly (`solid-markdown/src/types.ts:58-60`:
`JSX.IntrinsicElements["code"] & SolidMarkdownProps & { inline?: boolean }`)
and the consumer reads it (`MarkdownText.tsx:313-316,375`). Under #44,
block-vs-inline is structural: block code is always `<pre><code
class="language-…">`. The consumer therefore splits the current `CodeBlock`:

- `pre` override → the block path: finds the `code` element child on
  `p.node`, reads its `language-…` class and raw text, and renders the
  existing debounced-Shiki block body (`MarkdownText.tsx:318-399` — the
  debounce, `createResource` highlight, and plain-`<pre>` fallback move over
  unchanged, minus every `props.inline` guard). It renders from raw text and
  never renders `p.children`, so the inner `code` component's (discarded)
  output is never mounted and no `<pre>` nests (the current pass-through `pre`
  override, `MarkdownText.tsx:419`, is replaced by this).
- `code` override → the inline path only: `<code class={p.class}>{rawText(p.node)}</code>`
  — no language, no highlight, exactly today's inline branch
  (`MarkdownText.tsx:376`).

### A5 — `renderingStrategy="reconcile"`: kept and un-deprecated; teardown mitigated by R1

Compass streaming requires reconcile (`MarkdownText.tsx:204-207`: "`reconcile`
keeps the same DOM nodes across ticks (so selection and scroll survive)";
passed at `MarkdownText.tsx:465`). #44 keeps the prop on the sync `Markdown`
but deprecates it with a one-shot console warning (`pr44.diff:5535-5550`).
Since we own the fork and are its only consumer, the fork **un-deprecates** it:
drop the `@deprecated` JSDoc and delete `warnForRenderingStrategy` — no
warning to silence, no dead code.

However, #44's reconcile is weaker than the old renderer's. The old pipeline
rendered the reconciled store through a fine-grained reactive walker
(`For`/`Switch`/`Dynamic` — `solid-markdown/src/renderer.tsx:19-45`), so an
unchanged subtree kept its DOM nodes. #44 instead re-runs `post()` +
`toJsxRuntime` over a fresh clone of the reconciled tree inside a memo on
every change (`pr44.diff:5275-5282`):

```ts
const rendered = createMemo(() => {
  const nextTree =
    options.renderingStrategy === "reconcile"
      ? cloneReconciledTree(reconciledTree as unknown as Node)
      : cloneTree(tree());
  return post(nextTree, options);
});
```

`toJsxRuntime` builds real DOM eagerly (Solid has no VDOM diff), so every
growth tick rebuilds the whole message subtree — reconcile still suppresses
no-op ticks (an unchanged parse produces an unchanged store and the memo does
not re-fire), but a changed tree loses DOM identity. The behavior contract in
`MarkdownText.tsx:221-257` (mid-stream growth) checks structure and
content, not node identity, so it stays green through both the highlight flash
and selection loss this rebuild causes. The mitigation is decided in **R1**
(Resolved decisions): adopt #44's renderer, kill the highlight flash with a
synchronous consumer-side `(lang, code)` highlight cache read at initial render
(Arm 3), accept the marginal selection loss, and keep the fine-grained-walker
re-port (Arm 2) as the escalation if C2's smoke shows selection loss actually
bites.

### A6 — Version, provenance, unused surface

- **Version: `3.0.0`** (no `-rigel.N` prerelease suffix — Matt, 2026-08-19: we
  OWN this fork, its versions are ours to set, not a suffix tracking an
  upstream line). #44 is a semver-major break — `SolidMarkdown` is removed and
  legacy props throw (A1) — so a clean `3.0.0` is the honest major. The fork's
  current `package.json` sits at `2.1.1-rigel.1` (`package.json:3` on the
  `solid-2-support` branch; `main` is unscoped upstream `2.1.1`),
  unpublished — nothing pins a 2.x line, so
  jumping to `3.0.0` strands no consumer. This is the fork's own versioning; it
  does not track `andi23rosca/solid-markdown`'s numbering.
- **Provenance (owned-fork vendoring, not an upstream contribution):** #44 is
  an unmerged draft by a third party (`keithce`) on
  `andi23rosca/solid-markdown`, and upstream is effectively inactive on it —
  the maintainer left one comment 2026-03-09 ("Thanks for this!" + "bump to
  3.0.0") then went silent for 5 months; #45 has had zero maintainer
  engagement since 2026-06 (checked at source this session). We are NOT
  upstreaming and do not depend on upstream merging anything (Matt,
  2026-08-19: we own this fork). We vendor the draft's code into the owned fork
  at a pinned base (`main` = `66c00a5` == upstream) and record the diff files
  (`/home/mattw/notes/solid-markdown-react10/pr44.diff`, `pr45.diff`) as the
  adopted snapshot. Post-adoption the fork's renderer is ours; upstream's
  numbering and any future upstream #44 shape are irrelevant to it.
- **The benefit adopting #44 buys (why we take it on merits):** it retires the
  hand-maintained in-repo AST renderer (`solid-markdown/src/renderer.tsx`'s
  `For`/`Switch`/`Dynamic` walk, deleted by #44) in favor of the maintained
  standard `hast-util-to-jsx-runtime` — the same rendering engine react-markdown
  10 uses — so the fork we own long-term stands on a maintained engine plus a
  thin fork-local jsx-runtime (A2) rather than a bespoke walker. It also brings
  the current react-markdown idiom and built-in `urlTransform` URL sanitization
  (compass hand-rolls `safeHref` today). The async exports are inert (next
  bullet). This is the whole upside; it is weighed against the streaming
  teardown cost in R1.
- **`MarkdownAsync`/`MarkdownResource`:** compass uses only the sync
  `Markdown` (streaming string). The async exports are carried as unused
  surface to keep the fork's adopted-snapshot diff minimal — they are pure
  additions (`pr44.diff:5288-5365`) with no cost to the sync path.
- **`rehypeInertRawAndBreaks` compatibility (verified):** the consumer plugin
  runs inside the processor (`rehypePlugins`), BEFORE #44's `post()` raw-node
  arm. It retypes every `raw` child to `text` and splits newlines
  (`MarkdownText.tsx:151-152,164-180`), so `post()`'s `isRaw` arm
  (`pr44.diff:5465-5473`) sees no raws, and `toJsxRuntime` (which throws on
  unknown node types) receives none. `allowDangerousHtml: true` still holds
  under #44 (A1), so raws are still produced upstream of the plugin exactly as
  the plugin's contract assumes (`MarkdownText.tsx:113-115`). The raw-node
  tests in `MarkdownText.test.tsx` ("content preservation",
  `MarkdownText.test.tsx:634-637`) remain the contract and must stay green
  unmodified.

## Alternatives considered

### Mention injection

- **(A) Consumer-side rehype plugin — CHOSEN (A3).** The react-markdown-10
  idiom; composes with the existing consumer plugin
  (`rehypeInertRawAndBreaks`); keeps the published fork a clean generic port
  with no Compass-specific seam.
- **(B) Fork-local `text` component seam.** Re-add a `text` key to the fork's
  `Components` and route text nodes through it inside a wrapped
  `toJsxRuntime`. Rejected: `hast-util-to-jsx-runtime` renders text nodes
  internally with no interception point, so this means patching or wrapping
  the vendored utility — a permanent divergence from the upstream model #44
  exists to align us with, re-created on every future upstream sync.
- **(C) Remark (mdast) plugin.** Split mdast text nodes pre-rehype. Rejected:
  the code-verbatim invariant is structural in hast (`pre`/`code` ancestry)
  but positional in mdast (`inlineCode`/`code` are separate node types, but
  GFM autolink/footnote interplay re-parents text later), and
  `rehypeInertRawAndBreaks` retypes raw→text only at the hast stage — a
  remark plugin would never see raw-HTML prose text, silently un-chipping it.
  Splitting at the same stage the invariants live in is strictly simpler.

### Reconcile / DOM stability

- **Adopt #44's memo-over-reconciled-store + consumer highlight cache —
  CHOSEN (R1, Arm 3).** Minimal fork divergence; suppresses no-op ticks; the
  per-tick DOM/instance rebuild's user-visible casualty (the highlight flash)
  is killed by a consumer-side `(lang, code)` cache, leaving only the marginal
  selection-during-stream loss.
- **Re-port the old fine-grained walker under #44's API (R1 Arm 2 — the
  escalation).** Keep `renderer.tsx`'s `For`/`Switch`/`Dynamic` walk over the
  reconciled store, fed by #44's new option surface. Preserves full DOM +
  instance identity but re-creates the in-repo AST renderer #44 deletes — most
  of the rewrite's simplification evaporates. Held as the fallback if C2 shows
  selection loss actually bites, not the default.

### Version

- **`3.0.0` — CHOSEN (A6/R3).** A clean semver-major for the breaking API,
  with no `-rigel.N` prerelease suffix — the fork is ours and sets its own
  versions.
- **`2.1.1-rigel.2`.** Continuity with the unpublished `2.1.1-rigel.1`
  placeholder only; mislabels a breaking API as a patch prerelease. Rejected.

## Global Constraints

- **Solid 2 target:** `solid-js 2.0.0-rc.0` + `@solidjs/web 2.0.0-rc.0` peers
  (the Solid-2 peer set from the current `solid-2-support` port,
  `package.json:134-137` on that branch; `main` still carries upstream's
  `solid-js ^1.6.0`, re-pinned to Solid 2 in F2), per DL-207. No Solid-1 code
  paths ship in the fork.
- **Fork base:** both PRs apply onto fork `main` (`66c00a5`, == upstream);
  the current `solid-2-support` branch (`8977c9e4`) is superseded by the new
  branch, not merged into it. `main` carries UPSTREAM packaging (unscoped
  `name: solid-markdown`, no `publishConfig`, a semantic-release `publish.yml`,
  and `repository`/`homepage`/`bugs` URLs pointing at `andi23rosca`), so the
  fork's RigelBuild packaging — which lives only on the discarded
  `solid-2-support` branch — is re-applied explicitly in F1, not inherited from
  the base. (The `exports` map is byte-identical on `main` and `solid-2-support`
  — relative `./dist/…` paths unaffected by the package scope — so it carries no
  delta and needs no re-application.)
- **Toolchain floors (carried from the current port):** TypeScript `^5.9.3`,
  tsup `^8.5.1`, vite `^7.2.4`, `vite-plugin-solid 3.0.0-next.27`,
  `babel-preset-solid 2.0.0-rc.0` (pnpm-overridden), vitest `^3.2.7` — NOT
  #44's vitest `^4.0.18` bump unless the suite needs it
  (`solid-markdown/package.json:138-162`).
- **Every slice green:** each slice ends with the fork's `lint:types` + test
  suite (or the compass `apps/ui` suite for consumer slices) passing before
  the next begins.
- **No second convention:** the consumer keeps ONE mention-render convention
  (the rehype plugin); `MentionRuns`-the-component is deleted in the same
  slice that lands the plugin. The fork keeps ONE renderer (#44's);
  `renderer.tsx`/`rehype-filter.ts`/`utils.ts` are deleted, not shadowed.
- **Behavior contract:** `apps/ui/src/components/MarkdownText.test.tsx` passes
  with test *expectations* unmodified (mention chips, code-verbatim,
  mid-stream stability, highlight fallback, link safety, content
  preservation); only render-plumbing internals in test setup may change.
- **Ledger coupling:** the design PR that freezes this record MUST, in the
  same diff, land the DECISIONS.md delta (Plan F4) — the
  `tools/design-ledger-gate` touch-coupling check enforces it.
- **Scope boundary:** no bun migration (deferred), no S4/S5/S7 work from
  RIG-2187 — this record replaces only S6's solid-markdown half.

## Plan

Fork slices (F*) land in `RigelBuild/solid-markdown`; consumer slices (C*) in
compass `apps/ui`; the ledger slice rides the design PR itself.

- **F1. Adopt #45 (bundle-deps) + establish RigelBuild packaging on the
  main-based branch.** Cut the new fork branch from `main` (`66c00a5`, ==
  upstream); apply `pr45.diff` (move the 9 runtime deps to `devDependencies`);
  and re-apply the RigelBuild packaging that today lives ONLY on the discarded
  `solid-2-support` branch, since `main` carries upstream's unscoped packaging
  (verified `git show main:package.json`: `name: solid-markdown`, no
  `publishConfig`, peer `solid-js ^1.6.0`, `repository`/`homepage`/`bugs`
  pointing at `andi23rosca/solid-markdown`, and a `publish.yml` running
  `pnpm lint → pnpm build → npx semantic-release`): set
  `name: @rigelbuild/solid-markdown`, `publishConfig: { access: "public" }`,
  the `repository`/`homepage`/`bugs` URLs at `RigelBuild/solid-markdown`, and
  replace upstream's semantic-release `publish.yml` with the fork's tag-driven
  `lint:types → test:client → build → pnpm publish --no-git-checks --access
  public` workflow. (The `exports` map is byte-identical on `main` and
  `solid-2-support` — relative `./dist/…` paths, scope-independent — so it
  carries no delta and needs no re-application. These packaging fields all exist
  on `solid-2-support`, but that branch is discarded and F2 re-ports #44 fresh
  off `main`, so they must be carried over explicitly, not inherited.)
  - Interfaces: consumes `pr45.diff`; produces a `package.json` with
    `name: @rigelbuild/solid-markdown`, `publishConfig.access = "public"`,
    `repository`/`homepage`/`bugs` at `RigelBuild/solid-markdown`, the upstream
    `exports` map unchanged, `dependencies` = `{}` (pre-#44) and the 9 packages
    in `devDependencies` at the pinned ranges from `pr45.diff:27-47`, plus the
    tag-driven publish workflow.
  - Verify: `pnpm install && pnpm run lint:types && pnpm run test:client`
    green; `pnpm run build` output contains no `require("unified")`-style
    externals for the 9 (tsup bundles them); `package.json` name is scoped,
    `repository` points at `RigelBuild`, and `publish.yml` runs `pnpm publish`
    (not `semantic-release`).
- **F2. Adopt #44 re-ported to Solid 2.** Apply `pr44.diff` onto F1, then
  re-port: delete the `solid-jsx` dep; add fork-local `src/jsx-runtime.ts`
  (A2) exporting `jsx`/`jsxs`/`Fragment` over `createComponent`/`merge`
  (`solid-js`) + `Dynamic` (`@solidjs/web`); switch `src/index.tsx`'s runtime
  import to it (wrapping function-component calls in `createComponent`/`untrack`
  so an override cannot track the parent render scope, A2); convert the THREE
  Solid-2 subpath deltas in #44's own code (A2): single-arg `createRenderEffect`
  → two-arg form (pattern: `solid-markdown/src/index.tsx:93-100`), `solid-js/store`
  imports (`pr44.diff:5104`) → core `solid-js`, and `solid-js/web`
  `renderToString`/`renderToStream` (`pr44.diff:6549,6635`) → `@solidjs/web`;
  un-deprecate `renderingStrategy` (drop the JSDoc `@deprecated` + delete
  `warnForRenderingStrategy`, `pr44.diff:5535-5550`); add #44's new runtime
  deps (`devlop`, `hast-util-to-jsx-runtime@^2.3.6`,
  `html-url-attributes@^3.0.1`, `unist-util-visit@^5`, `@types/hast@^3`) as
  devDependencies per the #45 bundling convention; keep peers at Solid 2
  (Global Constraints), NOT #44's `solid-js ^1.8.0`.
  - Interfaces: produces the new public API — default `Markdown(options:
    Options & { renderingStrategy?: "memo" | "reconcile" }): JSX.Element`,
    `MarkdownAsync`, `MarkdownResource`, `defaultUrlTransform`, and types
    `Components` / `Options` / `ExtraProps` / `UrlTransform` / `AllowElement`
    (shapes per `pr44.diff:5945-6106`); produces `src/jsx-runtime.ts` with
    `jsx(type: string | Component, props: ParentProps): JSX.Element`,
    `jsxs = jsx`, `Fragment(props: ParentProps): JSX.Element`.
  - Verify: fork suite green under jsdom (`pnpm run test:client`) and SSR
    (`pnpm run test:ssr`, including ONE case rendering GFM + inline HTML
    through the fork-local runtime's string-tag path under `@solidjs/web`'s
    server bundle — the hand-rolled `jsx`/`Dynamic` path is untested SSR
    territory otherwise); `lint:types` green against Solid-2
    types; a dev smoke rendering GFM + inline HTML through `Markdown` with
    `renderingStrategy="reconcile"`; and a deterministic node-identity
    characterization test (render, hold a reference to a rendered `<p>`/`<pre>`
    element, grow the source signal one tick, assert `isSameNode` — this
    PROVES the DOM-rebuild mechanism R1 turns on, pre-publish, so the C2
    smoke carries only the acceptability judgment not the mechanism question).
- **F3. Publish `@rigelbuild/solid-markdown@3.0.0` (only AFTER C2 confirms R1 —
  if selection loss forces the Arm-2 escalation it reopens the fork, and an npm
  publish cannot be unpublished after 72h).** Bump
  `package.json` version; tag `v3.0.0`; the tag-driven publish workflow F1
  established (`lint:types → test:client → build → pnpm publish --no-git-checks
  --access public`) runs on the tag.
  - Interfaces: consumes F2's green branch; produces the published npm
    artifact compass pins.
  - Verify: CI publish job green; `npm view @rigelbuild/solid-markdown@3.0.0`
    resolves; a scratch install renders under Vitest+jsdom without the
    `util.deprecate` crash (the #45 fix, upstream issue #27).
- **C1. Consumer rehype-mention plugin + MarkdownText rewrite (develops against
  F2's branch, NOT the published package).** In `apps/ui`: add
  `src/markdown/rehype-mention-chips.ts` (A3); rewrite `MarkdownText.tsx` to
  import default `Markdown` from `@rigelbuild/solid-markdown` pinned to F2's
  branch via a git-ref dep (`github:RigelBuild/solid-markdown#<F2-branch>` — the
  exact link mechanism DL-208 already blesses for `@tanstack/solid-virtual`,
  `github:RigelBuild/virtual`, `DECISIONS.md:200`), replacing `solid-markdown
  2.1.1` (`apps/ui/package.json:25`); pass
  `rehypePlugins={[rehypeInertRawAndBreaks, rehypeMentionChips(byHandle)]}`
  memoized on `props.byHandle`, split `CodeBlock` into the `pre`-owned block
  path + inline-only `code` override (A4), delete `MentionRuns` and the `text`
  override, keep the `a`/`img` overrides as-is (their props shape —
  `node`/`href`/`alt` — survives under #44's `passNode: true`,
  `pr44.diff:5455`); and add the R1 highlight cache — a **synchronous**
  `(lang, code)` → HTML cache (module-level `Map`) that `CodeBlock` reads at
  its initial render (R1): a cache hit seeds `html` and bypasses the 150ms
  debounce + async `createResource` (`MarkdownText.tsx:342-367`), so a block
  rebuilt by #44's per-tick teardown paints highlighted markup on the first
  frame; a miss runs today's debounce → `highlightToHtml` path
  (`highlighter.ts:73-82`) and populates the cache on resolution. Developing
  against the branch (not F3's npm artifact) keeps the one irreversible step —
  the npm publish (F3) — AFTER C2 confirms R1 holds.
  - Interfaces: consumes `mentionRuns(text: string, byHandle: Map<string,
    Account>): MentionRun[]` (`mention-runs.ts:20-23`, unchanged) and F2's
    branch build; produces `rehypeMentionChips(options: { byHandle:
    Map<string, Account> }): (tree: HastRoot) => void` and the rewritten
    `MarkdownText: Component<{ text: string; byHandle: Map<string, Account> }>`
    (external contract unchanged). The mention plugin's visitor MUST NOT
    descend into the chip spans it injects (it would re-chip `@handle` inside
    its own output) — build a fresh children array and skip inserted nodes, the
    hand-rolled pattern `rehypeInertRawAndBreaks` already uses
    (`MarkdownText.tsx:164-186`); and the inline-only `code` override MUST stay
    computation-free (no effect/resource/timer) — A4's discard-safety for the
    block path's inner `code` element depends on it.
  - Verify: full `MarkdownText.test.tsx` suite green with expectations
    unmodified (Global Constraints); `tsc` green; app boots and renders a
    streamed message with chips + a highlighted fence in the dev shell; a
    consumer test asserts a cache-hit rebuild (grow the source one tick over an
    already-settled fence) paints highlighted output with no plain-`<pre>`
    fallback frame (the R1 cache working).
- **C2. Streaming smoke — confirms R1's highlight cache + measures selection
  residual.** In the dev/packaged shell, stream a long message whose fixture
  MUST contain an early tagged code fence (e.g. a `ts` block) that settles and
  Shiki-highlights, THEN ≥1s of continued prose streaming below it (without the
  settled-fence-then-grow shape the smoke cannot see the flash R1 must
  suppress). Verify: no prose flip; scroll position survives
  growth ticks; the settled highlighted block does NOT flash back to the plain
  `<pre>` fallback on subsequent ticks (R1's `(lang, code)` cache working); and
  RECORD whether text selection inside the growing message survives (R1's
  accepted residual — if it materially bites, this is the signal to escalate to
  Arm 2 before F3 publishes).
  - Interfaces: consumes C1; produces a recorded observation on the PR (R1
    confirmation: no flash; selection residual characterized).
  - Verify: the observation is recorded; mid-stream test
    (`MarkdownText.test.tsx:221-257`) green.
- **C3. Pin-flip (after F3 publishes).** Flip `apps/ui`'s
  `@rigelbuild/solid-markdown` dep from C1's git-ref (`github:RigelBuild/solid-markdown#<F2-branch>`)
  to the published `3.0.0`; refresh the lockfile.
  - Interfaces: consumes F3's npm artifact; produces `apps/ui/package.json`
    pinned to `@rigelbuild/solid-markdown@3.0.0`.
  - Verify: `pnpm install`/lockfile clean; `MarkdownText.test.tsx` suite still
    green against the published package; `tsc` green.
- **F4. Ledger delta (APPLIED in this design PR alongside this record — the
  design-ledger-gate couples the two).**
  - Flipped DL-208's Status cell (`DECISIONS.md:200`) to
    `Active (Matt, 2026-08-18) / solid-markdown clause superseded by DL-218
    (Matt, 2026-08-19)` — the clause-scoped grammar the ledger already uses for
    a partial supersede (DL-196's cell supersedes "only DL-097's flat-list
    clause"; DL-164's cell reads `Active (start-half …) / add-half Superseded
    by DL-185`); DL-208's `@tanstack/solid-virtual` clause stays Active.
  - Appended **DL-218** to the **UI shell** table (`DECISIONS.md:202`): the
    fork adopts #44 + #45 re-ported to Solid 2 via the fork-local jsx-runtime
    triple, published `@rigelbuild/solid-markdown@3.0.0`; the consumer injects
    @mention chips via the rehype plugin over `mentionRuns`, derives inline
    code via the `pre`/`code` split, keeps un-deprecated
    `renderingStrategy="reconcile"`, and adds the R1 `(lang, code)` highlight
    cache; supersedes DL-208's solid-markdown clause only.
  - Annotated the parent record's affected clause: `compass-ui-solid-v2/design.md`
    (still Draft) carries a ledger note under Resolved decisions that decision
    2's solid-markdown clause is superseded by DL-218 — a header/ledger note,
    never a decision-text rewrite.
  - Swept `DL-208` citations across the compass tree: the only hits are the
    ledger itself and this record; no code comment cites DL-208, so no code
    sweep is owed.
  - Verify: `tools/design-ledger-gate` passes on the design PR.

Dependency order: F1 → F2 → C1 (against F2's branch; includes the R1 highlight
cache) → C2 (confirms R1: no flash, characterizes selection residual) → F3
(publish, gated on C2 confirming R1) → C3 (pin-flip to the published version).
F4 rides the design PR and freezes before any of them execute.

## Tasks

- [ ] F1 — cut fork branch from `main`; adopt #45 (deps → devDependencies);
      re-apply RigelBuild packaging (scoped name, publishConfig, repo/homepage/bugs
      URLs, tag-driven publish.yml; exports map unchanged from upstream) onto the
      main-based branch; fork suite + build green
- [ ] F2 — fork adopts #44 re-ported to Solid 2 (fork-local jsx-runtime over
      createComponent/merge + @solidjs/web Dynamic; three Solid-2 subpath
      deltas; un-deprecate renderingStrategy); test:client + test:ssr +
      lint:types + node-identity characterization test green
- [ ] C1 — consumer: rehype-mention-chips plugin (visitor skips its own
      injected spans), `pre`/`code` split (inline `code` stays
      computation-free), R1 synchronous `(lang, code)` highlight cache read at
      `CodeBlock` initial render (bypasses the debounce on a hit),
      `Markdown` default import via git-ref to F2's branch, `MentionRuns`
      deleted; `MarkdownText.test.tsx` green with expectations unmodified
- [ ] C2 — streaming smoke in the shell (fixture: early settled code fence +
      ≥1s continued streaming); confirm R1 kills the flash + record selection
      residual
- [ ] F3 — tag + publish `@rigelbuild/solid-markdown@3.0.0` (after C2 confirms
      R1)
- [ ] C3 — pin-flip `apps/ui` to published `@rigelbuild/solid-markdown@3.0.0`;
      suite green against the published package
- [x] F4 — ledger delta APPLIED in this design PR: DL-208 clause-scoped Status
      flip + DL-218 row + parent-record ledger note + DL-208 citation sweep
      (no code cites it); design-ledger-gate green on the PR

## Resolved decisions

All three load-bearing forks were decided by Matt (2026-08-19); folded here so
the frozen record carries the outcome, not the question.

### R1 — Streaming teardown mitigation: highlight cache (Arm 3)

PR #44's renderer rebuilds the whole message subtree (DOM + component-override
instances) on every changed tick under `renderingStrategy="reconcile"`: the
`rendered` memo reads every reconciled-store leaf via `cloneReconciledTree` =
`JSON.parse(JSON.stringify(tree))` (`pr44.diff:5556-5558`), so any changed tick
re-fires it and re-creates instances — where the old fine-grained walker
(`solid-markdown/src/renderer.tsx:19-45`) kept unchanged subtrees' nodes AND
instances alive. Two casualties, both invisible to the current suite
(`MarkdownText.test.tsx:221-257` checks structure + textContent):

1. **Highlighted-code flash (the user-visible one).** `CodeBlock`'s `settled()`
   signal + `createResource` + 150ms debounce are per-instance state
   (`MarkdownText.tsx:333-367`), and `highlighter.ts` has no per-`(lang, code)`
   output cache (`highlighter.ts:73-82` re-runs `codeToHtml` every call). So a
   settled Shiki block is torn down + rebuilt on every subsequent growth tick —
   even ticks that only append prose below it — flashing back to the plain
   `<pre>` fallback until the ≥150ms debounce re-fires.
2. **Within-message selection.** DOM identity is lost per changed tick, so
   text selection inside an actively-streaming message may not survive; scroll
   likely does (the scrolling ancestor is outside the Markdown root).

**Decision — Arm 3 (consumer-side highlight cache).** The flash is NOT killed
by memoizing inside the async `highlightToHtml` alone: a rebuilt `CodeBlock`
starts with `settled()` = `null` (`MarkdownText.tsx:333`) so it paints the
plain `<pre>` fallback on mount, then gates highlighting behind a 150ms
debounce (`MarkdownText.tsx:342-348`) and an async `createResource`
(`MarkdownText.tsx:350-367`) that never resolves synchronously even for an
already-resolved promise — so a memo inside `highlightToHtml`
(`highlighter.ts:73-82`) removes the re-tokenize COST but leaves the ≥150ms
fallback frame. C1 therefore adds a **synchronous** `(lang, code)` → HTML cache
(a module-level `Map` keyed on the code text + language) that `CodeBlock` reads
AT INITIAL RENDER: on a cache hit it seeds `html` directly and bypasses the
debounce/`createResource` cycle, so a rebuilt instance of an already-highlighted
block paints highlighted markup on the first frame; on a miss it runs today's
debounce → highlight path and writes the resolved string into the cache. The
teardown mechanism is proven (`pr44.diff:5275-5282,5556-5558`) and pinned by
F2's deterministic node-identity characterization test; C1 adds a consumer test
asserting a cache-hit rebuild paints highlighted output with no fallback frame,
and C2's shell smoke (fixture: early settled code fence + ≥1s continued
streaming) confirms the flash is gone and measures the residual selection loss.
Selection-during-active-stream loss is accepted as a marginal case (selecting
inside a token-by-token-growing message is a moving target). **Escalation, not
part of the frozen default:** if C2 shows selection loss actually bites in
practice, Arm 2 (re-port the fine-grained walker under the new API —
Alternatives § Reconcile, full identity, at the cost of re-owning the deleted
AST renderer) is the fallback. Arm 1 (accept the flash) was rejected — the
flash is the more user-visible casualty and Arm 3 kills it.

### R2 — @mention injection: consumer-side rehype plugin (option A)

Matt confirmed option A (A3): a new `apps/ui` rehype plugin
(`rehype-mention-chips.ts`) splits non-code text nodes into chip spans via the
existing pure `mentionRuns` splitter, ordered after `rehypeInertRawAndBreaks`.
The invariants are verified in A3 (code-verbatim via inherited `inCode`; link
labels flatten chips away via `rawText`; raw-HTML text still chips). Options B
(fork-local text seam) and C (remark plugin) are weighed and rejected in
Alternatives.

### R3 — Published version: `3.0.0`

Matt ruled `3.0.0` — a clean major, no `-rigel.N` prerelease suffix, because we
own the fork and set its versions (A6). #44 is a semver-major break
(`SolidMarkdown` removed, legacy props throw); nothing is published on a 2.x
line to strand.

## Deferred (non-load-bearing)

- **`MarkdownAsync`/`MarkdownResource` unused surface:** carried to keep the
  adopted-snapshot diff minimal (A6); pruning them is an optional later fork
  cleanup with no consumer impact.
- **Bun migration of the fork's tooling:** Matt deferred (2026-08-19) to its
  own low-priority follow-up issue — the fork stays on pnpm/tsup/vitest for
  this change.
