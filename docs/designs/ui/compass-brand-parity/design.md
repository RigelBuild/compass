# Design: Compass Brand Parity with rigel.build

Builds on: [UX foundation](../compass-ux-foundation/design.md) (DL-148, DL-150, DL-184), [glyph primitives](../compass-glyph-primitives/design.md) (DL-367), [channels in the agent tree](../compass-channels-in-agent-tree/design.md)
Refs: RIG-4774 (parent RIG-4770); RIG-1622 (agent tree); ledger DL-418 to
DL-421, DL-423, DL-424, DL-430 (DL-422 and DL-425 are held for Open
Questions 2 and 4)
Depends on: #1811 (RIG-4771 chrome cleanup), #1814 (RIG-4772 session
stream), #1644 (DL-399 local baselines)
Siblings: RIG-4771 (chrome cleanup, pane edges), RIG-4772 (Session Log
stream), RIG-4773 (Bridge toggle), RIG-4775 (Settings), RIG-4776
(Backlog/Done). This record does not redo their work.

## Problem / Intent

Matt's dogfood review: the Compass UI does not look like
[rigel.build](https://rigel.build). The colors look bad, the agent status
icons do not show, messages are plain text, the composer does not read as a
composer, and the top search, new-topic name, and first-message fields look
unstyled. This record finds the concrete causes in `apps/ui/src/` and plans
ten small PRs that bring Compass to the marketing site's look, with the
agent tree as the highlight.

Out of scope: Settings (RIG-4775), the Backlog/Done views (RIG-4776), usage
display (RIG-4771 deletes `UsageBar`), and the pane-edge rules themselves.
PR #1811 (RIG-4771) puts the `.right` and `.log-panel` edges on
`--cx-border-strong` and hands the token values back to this record; T2b
sets them.

## Approach

The reference is the public site: [rigel.build](https://rigel.build) and
[rigel.build/compass](https://rigel.build/compass), whose shipped CSS uses
the same `--rigel-*` primitive values as `apps/ui/src/design/tokens.css`
(night `#011627`, panel `#0e2a45`, night-2 `#0b2942`, fog `#d6deeb`, haze
`#89a4bb`, mute `#5f7e97`, blue `#82aaff`). The primitives already match.
The gaps are in five places: fonts that never load, a surface and line model
that hides edges, semantic aliases and weights that differ from the site,
component CSS that is never imported, and an agent-state data path that
drops most states.

### Gap audit

| Site element (rigel.build) | Compass today | Gap | Task |
| --- | --- | --- | --- |
| Brand faces load from `@font-face` (`/fonts/SpaceMono-Regular.ttf`, `/fonts/DepartureMono-Regular.otf`) | No `@font-face` anywhere under `apps/ui`; `--cx-font-ui: var(--rigel-mono)` names Space Mono, but nothing loads it, so the host falls back to `ui-monospace` | The whole UI renders in the wrong face outside CI (CI pins the faces through fontconfig, so baselines hide it) | T1 |
| Display face for headings (`.bridge-heading` in Departure Mono 22px) | `--cx-font-display` is used by one rule in `app.css`; the face never loads | Fixed when the face loads | T1 |
| Square corners everywhere: cards, inputs, `.seg`, and square status pips (`.dot` 10×10, `.ci-badge` 7×7) | `--cx-radius-sm/md/lg` are 3/6/10px, used by 57 declarations; 26 more literal radii (`.tree-agent` 4px, `.bridge-link .count` 10px, round pips at 50%, 999px pills) | Rounded corners and round pips are off-brand | T2a |
| Night-2 hairlines drawn on the night page (`html,body{background:var(--rigel-night)}`, `.topbar{border-bottom:1px solid var(--rigel-night-2)}`); raised is kept for cards, menus, the root card, and the human message | Topbar, `.left`, `.right`, and `.log-panel` sit on `--cx-bg-raised`; `--cx-border` is `color-mix(in srgb, var(--rigel-panel) 70%, transparent)`; night-2 and raised are the same hex (`#0b2942`) | Pane edges barely show; the site's own line color would vanish on raised chrome | T2b (Open Question 1) |
| Form input: raised ground, mute border, 11×14 padding, blue border on focus (site `input` rule) | Text boxes have no visible edge | No field border token | T7 |
| CI success badge in green, approved review in cyan (selectors in A9) | `--cx-ci-pass` and `--cx-review-approved` both resolve to `--cx-ok`, the syntax-tier `--rigel-success` (`#22da6e`) | Wrong greens on every board card | T9 |
| Hierarchy by color, case, and letter-spacing; no `font-weight` outside `@font-face` | 48 `font-weight: 600/700/bold` rules in `app.css`; Space Mono ships 400 and 700 only and `base.css` sets `font-synthesis: none`, so every 600 renders bold | Chrome reads heavy and loud | T9 |
| Agent state glyph: 9×9 cells drawn at 16px (`.ac-glyph svg{width:16px;height:16px}`), state-colored, state label beside it; alive glyphs pulse 0.7→1 (`ac-pulse`) | `StateDot` draws 9×9 CSS px; live data reaches only 4 of 8 states; most rows show a grey hollow square; `cx-state-dot-pulse` dips to 0.4 | Icons "do not show" (root cause below) | T3, T4 |
| Manager tree: depth-indented agent cards with spine elbows, glyph, name, colored state label, issue chip; root card raised with a 2px blue left rule; a blue pip runs down live spines (`spine-flow 1.4s` on `.tree-spine[data-flow="1"]`) | Left-sidebar text rows (`.tree-agent`, 12px name, 9px dot); `.tree-children` is a static 1px rule; no overview | No agent overview, no tree motion | T5 (Open Question 2) |
| Thread message: author in fog 12px, kind tag (`MANAGER`, `HUMAN`), `ASK` flag in blue and `STEER` flag in cyan (`.msg-flag.ask`, `.msg-flag.steer`), time right-aligned in mute, body in haze 13px; human messages on raised with a 2px blue left rule | `.msg-role` colors authors blue (`--cx-accent`), bright green (`--cx-ok`), or magenta (`--cx-author-system`), weight 600; `.msg-at` sits beside the name; no tag or flag | Messages read as plain text with loud names | T6 |
| (no composer on the site; the site `input` rule is the reference) | `Composer` is a one-line `<input class="field">`; its border is `--cx-border` on `--cx-bg-raised` | Composer does not read as a composer | T7 |
| Text inputs | `.new-topic-name`/`.new-topic-message` carry only flex sizing (browser-default box); `.topbar-search-input` has no `font: inherit` and a faint border | Fields look unstyled | T7 |
| (site CSS is all loaded) | Six primitives are never imported: `ask`, `badge`, `input`, `loader`, `panel`, `tree`. Two have users today: `ShortcutsOverlay` renders `cx-search`, and `Palette` renders `.cx-loader[data-topology="bar"]`; both are unstyled | Users of an unimported primitive fail silently | T7 |
| Compass needle mark (`/compass-c-needle.svg`, purple north half on the navy tile) | Top bar shows the 11×11 `logo` `Glyph` in `--cx-accent` blue | Product mark missing | T8 (Open Question 4) |

### Why the status icons do not show

The SVG itself is fine: `StateDot` (`apps/ui/src/components/StateDot.tsx`)
emits a `viewBox="0 0 9 9"` `crispEdges` SVG of `rect` cells filled with
`currentColor`, and `state-dot.css` colors it per `data-state`. The cause is
upstream and in sizing:

1. **The live set is coarse.** Every agent row renders
   `<StateDot state={a().lifecycle ?? "idle"} />` (`AgentLeaf` in
   `LeftSidebar.tsx`). `lifecycle` comes from `joinAgents`
   (`apps/ui/src/roster.ts`): `lifecycle: info ? info.lifecycle : "stopped"`,
   where `info` is the comms presence entry. Presence is a 4-value enum
   (`AgentPresence` in `proto/compass/v1/comms.proto`: IDLE, WORKING,
   WAITING, OFFLINE), and `presenceLifecycle` (`apps/ui/src/live/adapt.ts`)
   maps `OFFLINE → "stopped"`. The server's `presenceFor`
   (`go/internal/presence/presence.go`) folds STOPPED, ERRORED,
   DISCONNECTED, and "no session" into OFFLINE. So live Compass can only
   show working, idle, waiting, and stopped. Done, paused, error, and
   disconnected never render.
2. **The full session state is dropped.** `AgentSessionStatus` (with its
   `state`) does arrive on `SubscribeEvents`, but on main `applyPayload` in
   `runEventStream` (`apps/ui/src/live/events.ts`) keeps only
   `runtime.set(account, adaptRuntimeMarker(payload.value))`.
   `adaptRuntimeMarker` documents that "the identity/lifecycle fields the
   status also carries are the presence path's job". #1814 (RIG-4772) keeps
   the state for the Session Log, but nothing feeds it to the dot.
   `agentDotState` (`apps/ui/src/agent-state.ts`), the 8-state projection
   built for this, has no production caller.
3. **Most rows are grey.** Any agent without a live session is "stopped": a
   hollow outline in `--cx-st-stopped` (`--rigel-mute`). Idle is a mute
   block too. In a fleet that is mostly idle or offline, the colored glyphs
   (green `»`, amber `?`, cyan tick, red `!`) almost never appear.
4. **The glyph is small.** `.cx-state-dot` is `width: 9px; height: 9px`
   (`state-dot.css`). The brand spec names "the 12px row-dot size"
   (`docs/specs/brand/state-icons.md`), and the site draws the tree glyph at
   16px.
5. **Geometry drifted.** The site's cells for `working`, `waiting`, and
   `done` differ from `STATE_CELLS` (A4 lists them). `idle` matches.

### A1 — Ship the brand faces

Load Space Mono (regular, bold), Departure Mono, and IBM Plex Mono with
`@font-face` in `tokens.css`, from font files under
`apps/ui/src/design/fonts/`. The files are the OFL faces already committed
in `apps/eng-docs/public/fonts/`. Vite bundles them as assets for both hosts.
`font-display: swap`, as on the site.

CI pins the faces through fontconfig (`fontDirs` in
`tools/toolchain/chromium-e2e-env.nix`: nixpkgs `google-fonts` Space Mono and
`departure-mono`). The `SpaceMono-Regular.ttf`, `SpaceMono-Bold.ttf`, and
`DepartureMono-Regular.otf` files in `apps/eng-docs/public/fonts/` have the
same SHA-256 as those nixpkgs files, so the glyph outlines in CI do not
change. Shots can still differ if Chromium rasterizes a webfont differently
from a system face; T1 treats any diff as expected and recaptures.

### A2a — Square corners

The site draws no rounded corner except one footer tile, and its status pips
are square (`.dot` 10×10, `.ci-badge` 7×7). `docs/specs/brand/surfaces.md`
asks for "flat square cards". Compass has 57 `var(--cx-radius-*)`
declarations and 26 literal radii in CSS, including `.tree-agent` 4px, the
round pips (`border-radius: 50%`), and 999px pills. `boot-styles.ts` sets one
more, `border-radius:4px`, in a TypeScript style string.

Delete `--cx-radius-sm`, `--cx-radius-md`, and `--cx-radius-lg` from
`tokens.css`, and delete every `border-radius` declaration under
`apps/ui/src`. A stylelint rule keeps the CSS that way; it does not read
`boot-styles.ts`, so T2a edits that file by hand. Three tokens all set to 0
would be dead weight, and would still leave the 26 literal radii. This
supersedes the radius line of ux-foundation D1 ("Radius:
`--cx-radius-sm|md|lg` (3/6/10)").

### A2b — Surfaces and lines (Open Question 1)

The site draws night-2 lines on night ground: `html,body` are night,
`.topbar` has `border-bottom:1px solid var(--rigel-night-2)`, and its product
mock puts thread and lane heads on panel. Raised is kept for cards, menus,
the root agent card, and the human message. Compass puts all chrome on
`--cx-bg-raised`: `.topbar`, `.left`, `.right`, and `.log-panel`. Night-2 and
raised are the same hex (`#0b2942`), so the site's line color is invisible
on Compass chrome. `--cx-border` (panel at 70%) and `--cx-border-strong`
(panel) are within a few levels of raised, which is why the pane edges in
the ask do not show. Note that `apps/ui/src/design/surfaces.md` already
puts the topbar on `--cx-bg`, so `app.css` has drifted from its own spec.

The surface model is Open Question 1. Settled under every option: a new
`--cx-border-field: var(--rigel-mute)`, the site's input edge, used only by
`input.css`.

### A3 — Agent lifecycle is a client-side join over session status

T3 builds on #1814 (RIG-4772). That PR keeps one `AccountSession
{ sessionId, state }` per agent account from `AgentSessionStatus` events in
`runEventStream`, emits it through `onSessions`, clears it on resync, and
stores it as `accountSessions`. T3 adds no second map. It projects that map
through `agentDotState` in `joinAgents`:

| Inputs for an account | `lifecycle` |
| --- | --- |
| session known | `agentDotState(session.state, { awaitingInput, turnDoneUnopened })` |
| no session, presence entry | `presenceLifecycle(presence)` (today) |
| neither | `"stopped"` (today) |

- `awaitingInput` is `presence?.lifecycle === "waiting"`. The server sends
  presence WAITING for STARTING or READY with an open ask (`presenceFor`).
  Today `agentDotState` reads `awaitingInput` only for STARTING and WORKING;
  READY returns `turnDoneUnopened ? "done" : "idle"`. T3 changes the READY
  arm so the order is **waiting > done > idle**. Without that change the
  join would turn every waiting agent grey.
- `turnDoneUnopened` is `turnEndedAtUnixMs > lastOpenedAt(account)`.
  - `AccountSession` gains `turnEndedAtUnixMs?: number`. `applyPayload` sets
    it to the event time (`SubscribeEventsResponse.at_unix_ms`) when the
    account's previous state in the map is WORKING and the new state is
    READY. Other events for the same `sessionId` carry the previous value.
  - `lastOpenedAt` is a per-agent time persisted in `localStorage` under
    `compass.lastOpened.${workspace}`, the same shape as the pin store
    (`loadPinnedAgents`/`savePinnedAgents`). One store effect writes it:
    while the focused view's route is `view === "agent"`, it records
    `max(Date.now(), turnEndedAtUnixMs)` for that agent, and again on each
    new turn end. The `max` stops a client clock behind the server from
    leaving the tick on. The effect keys on the route, not on `openAgent`,
    so every way in counts: `openAgent`, a Mod-click or middle-click
    (`dispatchLayout` open), a deep link, a tab focus, back/forward, and an
    agent DM. A background tab open does not focus the view, so it does not
    count.
  - Because both sides are times, a cold-start ring replay and a page reload
    give the same answer as the live stream. A resync clears the map; the
    replay re-observes the edge with its original time. The residual: an
    agent whose WORKING event fell out of the 1024-event ring shows `idle`,
    not `done`.

No proto change. Done, error, and disconnected become reachable, and
waiting is kept. Whether to seed at cold start is Open Question 3; the plan
assumes no seed.

### A4 — Site glyph cells and an integer 2× scale

Replace the `working`, `waiting`, and `done` entries of `STATE_CELLS` with
the site's cells. Site `working` is two 5-cell chevrons
(`1,2 4,2 2,3 5,3 3,4 6,4 2,5 5,5 1,6 4,6`); Compass draws two 7-cell
chevrons over rows 1-7. Site `done` is a 7-cell tick
(`1,4 2,5 3,6 4,5 5,4 6,3 7,2`); Compass draws 9 cells. Site `waiting` is
`3,1 4,1 5,1 2,2 6,2 6,3 4,4 5,4 4,5 4,7`. The other five glyphs have no
site render and stay. `StateDot` gains `scale?: 1 | 2`; `2` sets
`data-scale="2"` and an 18px box. Integer scale keeps every cell a whole
2×2 block; the site's 16px is a non-integer 1.78×. The sidebar tree and the
agent tree (A5) use 2×; dense spots (right-sidebar tab badge, Bridge cards,
agent header) keep 1×.

The site's working glyph breathes from 0.7 to 1 (`ac-pulse`). Compass
`cx-state-dot-pulse` dips to 0.4, which reads as blinking. Set the low
point to 0.7.

### A5 — The agent tree, the highlight (Open Question 2)

The anatomy is settled; where it lives is Open Question 2. The plan assumes
the recommended option: a new main view at `/agents`. The view draws the
site's Manager tree from `agentTree(store.agents())` (`stub-data.ts`):

- Row: `padding-left: depth × 26px`, with a 1px elbow spine in
  `--cx-border` to the parent (site `.tree-row`, `.tree-spine`).
- Card: grid `20px 1fr auto`; 2× `StateDot`; handle; meta line with
  `AGENT_STATE_LABEL[state]` in the state color; issue chip on the right. No
  child count: the site's `N workers` counts dispatched workers, Compass has
  no live worker source, and the spine already shows child agents.
- Root cards: raised ground and a 2px `--cx-accent` left rule.
- Issue chip (`agentIssueChip`): the agent's active issues
  (`assignee === account.id && isActiveState(state)`); none → no chip; one →
  `issueKey(issue, multiForge)`; more → `N open`.
- Spine flow: a spine to a `working` child carries `data-flow="1"`, and a
  1×6px `--cx-accent` pip runs down it (site `spine-flow`, 1.4s). This is the
  chase-light pip that ux-foundation D6 names in the Manager-tree reference.
  It is the view's one unbounded motion; the state-dot column stays the
  sanctioned exception (`apps/ui/src/design/motion.md`, Budget). The period
  is a new token zeroed under reduced motion, like `--cx-pulse-period`.
- Click or Enter on a card calls `store.openAgent(account.id)`.

The sidebar tree stays the compact navigator and gets the 2× glyph (A4).

### A6 — Message anatomy

`MessageRow` (`ChannelView.tsx`) is the only emitter of `.msg`, and the agent
view's chat goes through the same `ChannelView`, so the rules restyle `.msg`
directly with no extra scope. The row emits the site anatomy:

- `.msg-author`: handle, `--cx-text`, `--cx-text-sm`.
- `.msg-tag[data-kind]`: `AGENT`, `HUMAN`, or `SYSTEM` from the author's
  `kind`; 1px border, `--cx-text-xs`, letter-spaced. The site says
  `MANAGER`; Compass uses its own domain word.
- `.msg-flag`: `ASK` (`--cx-accent`) when the message has an ask block, and
  `STEER` (new `--cx-msg-steer`, cyan) when a text block @-mentions an agent
  account. The `Composer` doc defines an @-mention of an agent as a steer.
- `.msg-at`: pushed right (`margin-left: auto`), `--cx-text-faint`.
- Body: `--cx-text-dim` at `--cx-text-md`. A human message gets
  `--cx-bg-raised` and a 2px `--cx-accent` left rule, with body in
  `--cx-text`.

Delete the `.msg-role` color rules and `--cx-author-system`; nothing else
uses them.

### A7 — Text entry uses the primitives

Each component imports the primitive CSS it uses, as `RuntimeMarker.tsx`
imports `runtime-marker.css` and `LayoutNotice.tsx` imports `button.css` and
`toast.css`. Six primitives are never imported today. This wires the two
that have or gain a user (`input`, `loader`) and adds a `button.css` import
where `ChannelView` starts to render `cx-btn`. The four with no user (`ask`,
`badge`, `panel`, `tree`) stay unimported until a component first renders
them; importing unused CSS adds bytes and no fix.

- `TopBarSearch` imports `input.css`; its input → `cx-search`.
- `ShortcutsOverlay` imports `input.css`; its `cx-search` field starts
  rendering as designed.
- `Palette` imports `loader.css`; its `.cx-loader[data-topology="bar"]`
  starts rendering.
- `ChannelView` imports `input.css` and `button.css`.
  - `NewTopic` name and first-message inputs → `cx-input`; start button →
    `cx-btn`.
  - `Composer` → `<textarea class="cx-composer">`. A textarea does not grow
    by itself, so on each input the composer sets its height to
    `scrollHeight`, capped by the primitive's `max-height: 40vh`. This works
    in both hosts; `field-sizing: content` is not assumed. Enter sends,
    Shift-Enter inserts a newline, and Enter during IME composition
    (`isComposing`) does nothing. Send → `cx-btn data-variant="primary"`.
- `input.css` border → `--cx-border-field`.
- Delete the ad-hoc box rules (`.topbar-search-input` box, `.conv-composer
  .field`, `.conv-composer .send`); keep the layout rules.

### A8 — The Compass mark in the top bar (Open Question 4)

Replace `<Glyph name="logo" />` in `App.tsx` with an `<img>` of the needle
mark, copied byte-for-byte from the public
[compass-c-needle.svg](https://rigel.build/compass-c-needle.svg). Purple lives
only in that static asset; no `--cx-*` token aliases purple (the rule in
`tokens.css`). Remove the `logo` entry from `GlyphName` and its cells, since
`App.tsx` is its only user.

Draw it at 24px. The SVG is a 12×12 grid of 8-unit cells in a 96 viewBox,
so 24px gives each cell exactly 2px, and 24px is the topbar mark floor in
`apps/ui/src/design/surfaces.md` § Mark placement. That paragraph also says
the topbar mark is the sigil wordmark, standing alone, with "no
icon-beside-wordmark lockup". The needle beside the "Compass" title breaks
that rule, as today's `logo` glyph beside the title already does. Open
Question 4 carries this; under the recommended option T8 rewrites the
paragraph to name the needle at 24px beside the plain-text product name.

### A9 — Site greens and hierarchy by color

The site paints a passing CI badge green
(`.ci-badge[data-status=success]{background:var(--rigel-green)}`) and an
approved review cyan
(`.review-badge[data-verdict=approved]{background:var(--rigel-cyan)}`).
Compass maps both `--cx-ci-pass` and `--cx-review-approved` to `--cx-ok`,
the syntax-tier `--rigel-success` (`#22da6e`). Point `--cx-ci-pass` at
`--rigel-green` (`#addb67`) and `--cx-review-approved` at `--rigel-cyan`;
`--cx-ok` and its other users stay. `docs/specs/brand/` is not edited; it
mirrors the company brand system.

The site CSS sets `font-weight` only inside `@font-face`. Its hierarchy is
color (fog, haze, mute), uppercase, and letter-spacing. `app.css` has 48
`font-weight: 600/700/bold` rules (`.msg-role`, `.bridge-link`,
`.conv-name`, `.topic-name`, column heads, and more). Space Mono ships 400
and 700 only and `base.css` sets `font-synthesis: none`, so every 600 renders
as bold. Remove the weight from chrome rules and carry the step with
`--cx-text` against `--cx-text-dim`, or uppercase plus letter-spacing for
labels. Markdown headings keep their weight and `strong` keeps the browser
default: that is author emphasis, not chrome. A stylelint rule stops new
weights; `tokens.css` is exempt for its `@font-face` descriptors.

## Alternatives considered

### Server-side: widen `AgentPresence` to eight states

Add ERRORED, DISCONNECTED, and so on to the proto enum and `presenceFor`.
Rejected: a proto change across server and UI for data the UI already
receives on `SubscribeEvents`, and `agentDotState` already encodes the UI-only
refinements (design D9).

### Edge-detect done-unopened in memory

Set `turnDoneUnopened` on a WORKING → READY edge seen by the store and clear
it when the agent's view gains focus. Rejected: `runEventStream` hands
consumers whole-map snapshots, not transitions; a resync clears the map and
loses the edge; a cold-start replay re-fires old edges; a reload clears
every tick. Comparing two persisted times (A3) gives one answer on every
path.

### Draw the glyph at 16px like the site

Rejected: 16/9 is not an integer, so cells become uneven 1-2px blocks at
DPR 1. 18px keeps the 1-bit grid exact (`docs/specs/brand/spine.md`
rendering rules).

## Global Constraints

- **Tokens only.** Component and app CSS consume `--cx-*` tokens. No raw hex,
  no `--rigel-*` reference, no literal duration or easing outside
  `design/tokens.css` (the D7 guard in `apps/ui/.stylelintrc.cjs`).
- **Purple is mark-only.** It appears only in the needle SVG asset. Never
  alias a purple primitive into `--cx-*`.
- **Departure Mono only at even 11px multiples** (22, 44): use
  `--cx-display-sm` or `--cx-display-lg`, never another size
  (`docs/specs/brand/type.md`).
- **Glyphs are 1-bit and integer-scaled.** `crispEdges`, 9×9 grid, scale 1×
  or 2× only.
- **Motion.** Of the eight state glyphs only `working` animates
  (`docs/specs/brand/state-icons.md`). The tree's spine flow is the one other
  unbounded motion. Every period token is zeroed under reduced motion.
- **Square corners.** No `border-radius` under `apps/ui/src` after T2a.
- **Visual baselines.** Each task recaptures its affected
  `apps/ui/e2e/__screens__/` shots locally under the pinned dev shell and
  reviews the diff. Committing those local captures follows DL-399 (#1644): a
  branch may commit them when `visual-gate` is green on the PR head and the
  PR body lists the PNGs. Until #1644 merges, T-tasks that change baselines
  wait for it.
- **Stacking.** Tasks stack in linear lines (below), never a tree.
  Red-green tests first in every task.
- **Lane:** `implement-ts` for every task.
- **Public repo.** Cite the public site and `docs/specs/brand/` only.

## Plan

Ten tasks in two linear lines. Lines keep logic rebases away from baseline
churn, and the tasks that change every shot (T1, T2a, T9) go first so later
tasks recapture a stable base.

- **Look line.** Base: the current top of the in-window tabs line, which is
  #1811 (RIG-4771) today. T2b and T8 need #1811's chrome; T5 needs that
  line's `RouteMatch` shape. Order: T1 → T2a → T9 → T4 → T6 → T7 → T8, then
  T2b and T5 in the order their open questions are answered.
- **State line.** Base: #1814 (RIG-4772). T3 only. It changes no baseline:
  `visual-smoke.spec.ts` renders stub data with no daemon.

Every task that changes baselines also waits for #1644 (DL-399).

### T1 — Ship the brand faces

- **Do:** add the font files and four `@font-face` rules (A1).
- **Interfaces:** new `apps/ui/src/design/fonts/{SpaceMono-Regular.ttf,
  SpaceMono-Bold.ttf,DepartureMono-Regular.otf,IBMPlexMono-Regular.ttf}`;
  `apps/ui/src/design/tokens.css` (`@font-face` block above the primitives;
  `--rigel-mono`, `--rigel-display` unchanged);
  `apps/ui/e2e/visual-smoke.spec.ts`.
- **Test (red first):** in `visual-smoke.spec.ts`,
  `document.fonts.load('22px "Departure Mono"')` and
  `document.fonts.load('12px "Space Mono"')` each resolve to one `FontFace`.
  `load` fetches the face itself, so the result does not depend on which page
  is open. Red today: no `FontFace` exists and both resolve to `[]`.
- **Baselines:** recapture all; review any diff (A1).

### T2a — Square corners

- **Do:** A2a.
- **Interfaces:** `apps/ui/src/design/tokens.css` (delete `--cx-radius-sm`,
  `--cx-radius-md`, `--cx-radius-lg`); every `border-radius` declaration in
  `apps/ui/src/app.css`, `apps/ui/src/design/base.css`, and
  `apps/ui/src/design/components/{ask,badge,button,card,input,loader,menu,
  palette,panel,tab-strip,toast,tooltip}.css`; the `border-radius:4px` entry
  in `apps/ui/src/boot-styles.ts`; the radius lines in
  `apps/ui/src/design/components.md`; `apps/ui/.stylelintrc.cjs` adds
  `"property-disallowed-list": ["/radius/"]`.
- **Test (red first):** add the stylelint rule; `moon run compass-ui:stylelint`
  fails on 83 declarations, then passes after the sweep.
- **Baselines:** recapture all.

### T9 — Site greens and hierarchy by color

- **Do:** A9.
- **Interfaces:** `apps/ui/src/design/tokens.css` (`--cx-ci-pass:
  var(--rigel-green)`, `--cx-review-approved: var(--rigel-cyan)`); the 48
  `font-weight: 600/700/bold` rules in `apps/ui/src/app.css` and the one in
  `apps/ui/src/design/components/runtime-marker.css`;
  `apps/ui/.stylelintrc.cjs` adds `"declaration-property-value-allowed-list":
  { "font-weight": ["400", "normal", "inherit"] }` and sets it to `null` in
  the existing `**/design/tokens.css` override (the `@font-face` rules from
  T1 declare weight 700). The `.markdown-content h1`–`h6` rule keeps its
  weight (author emphasis) with a `stylelint-disable-next-line` and the
  reason; `strong` keeps the browser default.
- **Test (red first):** the stylelint rule fails on the weight rules, then
  passes after the sweep. A token test is not added: the token is a value.
- **Baselines:** recapture all; `bridge-card.png` shows the CI badge.

### T4 — Site glyph cells and 2× scale

- **Do:** A4.
- **Interfaces:** `apps/ui/src/components/StateDot.tsx` (`STATE_CELLS`
  working/waiting/done; `StateDot: Component<{ state: AgentState; scale?:
  1 | 2 }>`); `apps/ui/src/design/components/state-dot.css`
  (`.cx-state-dot[data-scale="2"] { width: 18px; height: 18px }`, SVG fills
  the box; `cx-state-dot-pulse` 50% opacity `0.7`); ASCII grids in
  `apps/ui/src/design/components.md`; `AgentLeaf` in `LeftSidebar.tsx`
  passes `scale={2}`.
- **Test (red first):** `StateDot.test.tsx`: `scale={2}` sets
  `data-scale="2"`; the `rect` cells for working, waiting, and done equal
  the site cells listed in A4 and the root-cause section.
- **Baselines:** `state-dot.png` and the shots with the left sidebar.

### T6 — Message anatomy

- **Do:** A6.
- **Interfaces:**
  - `apps/ui/src/comms.ts`: `export function messageFlags(msg: Message,
    byHandle: Map<string, Account>): readonly ("ask" | "steer")[]`. `ask`
    when a block has `kind === "ask"`; `steer` when `parseMentions` over a
    text block's `blockText` finds a non-reserved handle whose account has
    `kind === "agent"`.
  - `MessageRow` in `apps/ui/src/components/ChannelView.tsx`: `data-kind` on
    `.msg`; `.msg-author`; `.msg-tag[data-kind]`;
    `.msg-flag[data-flag]` per `messageFlags`.
  - `apps/ui/src/design/tokens.css`: add `--cx-msg-steer:
    var(--rigel-cyan)`; delete `--cx-author-system`.
  - `apps/ui/src/app.css`: `.msg*` rules; delete the `.msg-role` color
    rules.
- **Test (red first):** `comms.test.ts`: `messageFlags` returns `ask` for an
  ask block, `steer` for an @-mention of an agent, nothing for an @-mention
  of a user or a reserved handle. `ChannelView.test.tsx`: an agent row shows
  tag `AGENT` and a human row `HUMAN`.
- **Baselines:** `topic-markdown.png`, `agent.png`.

### T7 — Text entry and composer

- **Do:** A7.
- **Interfaces:**
  - `apps/ui/src/design/tokens.css`: add `--cx-border-field:
    var(--rigel-mute)`; document it in `apps/ui/src/design/components.md`.
  - `apps/ui/src/design/components/input.css`: border →
    `--cx-border-field`.
  - `TopBarSearch.tsx` and `ShortcutsOverlay.tsx` import
    `../design/components/input.css`; `TopBarSearch` input → `cx-search`.
  - `Palette.tsx` imports `../design/components/loader.css`.
  - `ChannelView.tsx` imports `input.css` and `button.css`; `NewTopic`
    fields → `cx-input`, start → `cx-btn`; `Composer` → `textarea.cx-composer`
    with an `onInput` autosize (`height = "auto"`, then
    `height = scrollHeight + "px"`), Enter/Shift-Enter/`isComposing`
    handling, send → `cx-btn data-variant="primary"`.
  - `apps/ui/src/app.css`: delete the replaced box rules.
- **Test (red first):** `ChannelView.composer.test.tsx`: the selector moves
  from `input.field` to `textarea.cx-composer`; Shift-Enter inserts a
  newline and does not send; Enter sends; Enter with `isComposing: true`
  does not send; with `scrollHeight` stubbed to 120, input sets
  `style.height` to `120px`.
- **Baselines:** the shots with the top bar or a channel.

### T8 — Compass mark

- **Do:** A8, after Open Question 4.
- **Interfaces:** new `apps/ui/src/assets/compass-needle.svg`; the
  `.brand` block in `App.tsx` as #1811 leaves it (`span.logo` holding
  `<Glyph name="logo" />`) becomes `<img class="logo" src={needle} alt=""
  width="24" height="24">`; `.brand .logo` in `app.css`; `GlyphName` and the
  `logo` cells in `Glyph.tsx`; the `logo` grid in `components.md`; the topbar
  Mark placement paragraph in `apps/ui/src/design/surfaces.md` (A8).
- **Test:** `tsc` proves no `logo` user is left; `App.test.tsx` asserts the
  brand `img` source.
- **Baselines:** recapture all.

### T2b — Surfaces and lines

- **Do:** A2b, after Open Question 1. The interfaces below are for the
  recommended option (c); options (a) and (b) touch the same files.
- **Interfaces:** `apps/ui/src/design/tokens.css` (`--cx-border:
  var(--rigel-night-2)`, `--cx-border-strong: var(--rigel-selection)`);
  `apps/ui/src/app.css` backgrounds (`.topbar`, `.left`, `.right`,
  `.log-panel` → `--cx-bg`; `.conv-head`, `.av-pane-head` →
  `--cx-bg-panel`); `apps/ui/src/design/surfaces.md` composition paragraph.
- **Test:** stylelint green. The change is token and background values
  only, so the shots are the test: recapture all and check that each pane
  edge is visible.

### T3 — Agent lifecycle from session status

- **Do:** A3, stacked on #1814.
- **Interfaces:**
  - `apps/ui/src/agent-state.ts`: `agentDotState` READY arm returns
    `awaitingInput ? "waiting" : turnDoneUnopened ? "done" : "idle"`;
    update its doc table and the `awaitingInput` doc.
  - `apps/ui/src/live/events.ts` (#1814 shape): `AccountSession` gains
    `readonly turnEndedAtUnixMs?: number`; `applyPayload(payload, atUnixMs:
    number)`, called with `Number(resp.atUnixMs)`.
  - `apps/ui/src/roster.ts`: `joinAgents(accounts, presence, runtime,
    sessions: ReadonlyMap<string, AccountSession>, lastOpened:
    ReadonlyMap<string, number>): Agent[]`.
  - `apps/ui/src/store.ts`: `loadLastOpened(workspace)` and
    `saveLastOpened(workspace, map)` beside `loadPinnedAgents`, key
    `compass.lastOpened.${workspace}`; a `lastOpened` signal; one effect
    that, while `focusedView().route().view === "agent"`, records the time
    for that `agentId` and again on each new `turnEndedAtUnixMs`; the
    `agents` memo passes `accountSessions()` and `lastOpened()`.
- **Test (red first):**
  - `agent-state.test.ts`: READY + `awaitingInput` → `"waiting"`, also with
    `turnDoneUnopened`; READY + `turnDoneUnopened` → `"done"`. This replaces
    the existing test "does NOT change READY (stays idle)", which pins the
    old READY arm; its red is the intended change, not a regression.
  - `roster.test.ts`: ERRORED → `"error"`; DISCONNECTED →
    `"disconnected"`; READY + presence waiting → `"waiting"`; READY with
    turn end after last-opened → `"done"`, at or before → `"idle"`; no
    session + OFFLINE → `"stopped"`.
  - `live/events.test.ts`: WORKING then READY sets `turnEndedAtUnixMs` to
    the READY event's time; READY then READY keeps it; a resync clears the
    map and a replay restores the original time.
  - Store tests: `openAgent` writes the storage key;
    `dispatchLayout({ kind: "open", path: "/agent/<id>" })` writes it too;
    the same open with `background: true` does not; a new store over the
    same storage keeps the agent idle; a turn end while the agent's view is
    focused keeps it idle; a turn end after focus leaves shows `"done"`.

### T5 — The agent tree view

- **Do:** A5, after Open Question 2 and after T4. Interfaces are for the
  recommended option (a), in the #1811 shapes.
- **Interfaces:**
  - `apps/ui/src/view-route.ts`: `RouteMatch` adds `{ view: "agents" }`;
    `parseRoute` and `routePath` gain the `"agents"` case.
  - `apps/ui/src/routes.tsx`: `appRoutes` adds
    `{ path: "/agents", component: AgentsView }`.
  - `apps/ui/src/route-title.ts`: `routeTitle` case `"agents"` →
    `"Agents"`.
  - `apps/ui/src/store.ts`: `View` adds `"agents"`; `showAgents(): void`
    runs `hideShortcuts(); navigateTo("/agents")`, like `showBacklog`.
  - `apps/ui/src/keyboard/destinations.ts`: a static `{ id: "agents",
    title: "Agents" }` destination that calls `store.showAgents()`;
    `apps/ui/src/keyboard/spine.ts`: a `showAgents` dep and a `view.agents`
    command "Go to Agents", like `viewBacklog`.
  - `apps/ui/src/board.ts`: `export function agentIssueChip(agentId:
    string, issues: readonly Issue[], multiForge: boolean): string | null`.
  - New `apps/ui/src/components/AgentsView.tsx` (`AgentsView: Component`)
    and `apps/ui/src/design/components/agent-card.css`, imported by the
    component.
  - `apps/ui/src/design/tokens.css`: `--cx-spine-flow-period: 1.4s`, zeroed
    in both reduced-motion blocks beside `--cx-motion-fast`.
  - `LeftSidebar.tsx`: an "Agents" link above the Bridge link, calling
    `store.showAgents()`.
- **Test (red first):** `agentIssueChip` cases (0, 1, many, inactive issues
  ignored); `view-route` round trip for `/agents`; `AgentsView.test.tsx`:
  clicking a card sets `view() === "agent"` and `selectedAgentId()`, Enter
  on a focused card does the same, a spine to a `working` child has
  `data-flow="1"` and one to an idle child does not.
- **Baselines:** new `agents.png`; the shots with the left sidebar.

## Tasks

Look line, on the in-window tabs line top (#1811):

- [ ] T1 — Ship the brand faces
- [ ] T2a — Square corners
- [ ] T9 — Site greens and hierarchy by color
- [ ] T4 — Site glyph cells and 2× scale
- [ ] T6 — Message anatomy
- [ ] T7 — Text entry and composer
- [ ] T8 — Compass mark (after Open Question 4)
- [ ] T2b — Surfaces and lines (after Open Question 1)
- [ ] T5 — The agent tree view (after Open Question 2)

State line, on #1814:

- [ ] T3 — Agent lifecycle from session status

## Open Questions

1. **Surface and line model (decides the pane-border ask).** The site draws
   night-2 lines on night. Compass chrome is raised, which is the same hex
   as night-2, so the site's line vanishes there. Today's pane edge is panel
   on raised, 1.02:1. Every option changes every baseline.
   - (a) Site-literal: chrome (`.topbar`, `.left`, `.right`, `.log-panel`)
     moves to `--cx-bg`, heads to panel, and every line is night-2. Edge
     contrast 1.23:1, as faint as the site. Sidebars and main become one
     color split only by that line. Matches `surfaces.md` for the topbar
     only; that spec keeps the sidebars raised.
   - (b) Keep raised chrome. `--cx-border` becomes selection (1.28:1 on
     raised) and `--cx-border-strong` becomes mute (3.49:1). Smallest
     change; visible edges; not the site's look.
   - (c) Hybrid: chrome on `--cx-bg` and heads on panel as in (a);
     `--cx-border` is night-2 for in-pane lines; `--cx-border-strong`, which
     #1811 already uses for pane edges, becomes selection (1.57:1 on
     night). No new token.
   - Recommendation: (c). It is the site's surface model with pane edges
     one step stronger than the site, which is what the ask needs.
2. **Where the agent tree lives.** ux-foundation D6 named the site's
   Manager tree as the sidebar's starting point; the Bridge already draws
   agent lanes in tree order; `/` is the Bridge (DL-031).
   - (a) A main view at `/agents`, first in the left sidebar and in the
     palette. Deep-linkable and a top-level window per DL-160. Costs an
     eighth `View` kind on the tabs line.
   - (b) No new view: the sidebar rows take the site anatomy in compact
     form (spine, 2× glyph, colored state word, issue chip on hover). Fits
     D6; a 244px sidebar cannot hold full cards.
   - (c) A `Tree` segment on the Bridge toolbar (`Tree | Issues | PRs`), as
     the site's `.seg`. On the landing surface with no new route, but not a
     window of its own, and it shares the control RIG-4773 is fixing.
   - Recommendation: (a), with the sidebar keeping its 2× glyph (T4).
3. **Cold-start seed.** `GetAgentStatus` is admin-only (`classifyProcedure`
   in `go/internal/auth/admin_gate.go`), and its all-sessions snapshot drops
   STOPPED and ERRORED (`isTerminal` in `go/internal/board/projection.go`).
   The cold-start ring replay already carries recent statuses.
   - (a) No seed (the plan). An agent whose last status left the event ring
     (`RingCapacity`, 1024, in `go/events/events.go`) shows its
     presence state.
   - (b) A best-effort seed for admins only; PermissionDenied is silent. It
     adds DISCONNECTED and live states, never `error`.
   - (c) A server change: an account-scoped status read open to the owner,
     or ERRORED kept in the snapshot. A proto and server change outside
     this record.
   - Recommendation: (a). File (c) only if dogfood shows a missing `error`.
4. **Placeholder mark in the topbar.** `docs/specs/brand/compass-mark.md`
   marks the needle "Placeholder", not locked; the site ships it in the
   Compass hero and footer. `apps/ui/src/design/surfaces.md` § Mark
   placement wants the sigil wordmark alone in the topbar, at 24px or more,
   with no icon beside a wordmark. Today's 11×11 `logo` glyph beside the
   "Compass" title already breaks that rule.
   - (a) Ship the needle now at 24px beside the plain-text name (T8). T8
     rewrites the Mark placement paragraph to match. The needle is the
     product mark the site pairs with Compass, and swapping one SVG later is
     cheap.
   - (b) Follow `surfaces.md` as written: the sigil wordmark alone, no
     needle. This needs a wordmark asset Compass does not have, and the R
     wordmark names the company, not the product.
   - (c) Hold T8 until the mark locks; the `logo` glyph stays.
   - Recommendation: (a).
