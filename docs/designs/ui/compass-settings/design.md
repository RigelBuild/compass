# Compass Settings page

Builds on: brand parity (`compass-brand-parity/design.md`, open PR #1915:
square corners, weights, surfaces, `--cx-border-field`, text-entry
primitives), [UX foundation](../compass-ux-foundation/design.md)
Refs: RIG-4775 (this record); RIG-3121 (Providers, task E5 of the
[gateway OAuth enrollment record](../../server/compass-gateway-oauth-enrollment/design.md));
ledger DL-431, DL-433, DL-434; DL-432, DL-435, and DL-436 are held for
Open Questions 1, 2, and 3
Depends on: brand-parity T9 (S1); enrollment E3 (S7)

## Problem / Intent

Matt's ask: "Settings styling and layout are bad, and we likely need many
more settings. Write a full settings design (sections, which settings
exist, layout), then implement."

Today Settings is one page that edits the tracker config. Defects (from
`SettingsView.tsx`, the `.settings-*` rules in `app.css`, and
`settings.png`):

1. **Bespoke boxes.** `.settings-input`/`.settings-select` and
   `.settings-btn*` copy `.cx-input` and `.cx-btn`.
2. **Raw sizes and bold.** `14px`/`12px` heads at weight `600`, `11px`
   labels, a `10px` gap; none is a token.
3. **Rounded cards.** `.settings-section` has `--cx-radius-md`.
4. **Fixed widths and wrapping.** `100px` and `140px` status columns clip
   long names; the `flex-wrap` fields row breaks into ragged lines.
5. **One page, no sections.** Editor and model registry share one scroll
   and one route.
6. **The one edit does little.** The store always uses the fixture seam,
   which ignores its `_config`; `setTrackerConfig` rebuilds it, but only
   the handle reaches the fixture queue. The kind and the status map
   change nothing, and the server maps tracker status itself (DL-129).
   Nothing shows the server, account, motion, or providers.

Brand-parity T2a and T9 fix defects 2 and 3 (they sweep every radius and
`font-weight: 600` in `app.css`). This record fixes the rest: one view
with a section nav, a route per section, one row anatomy on the shared
primitives, and a save model per section.

## Approach

### A1 — Sections and settings

Sections in nav order. Tracker is OQ1; Usage data's place is OQ2.

- **General** (`general`): Server URL (`connection.baseUrl`, the
  transport's URL in both hosts); Mode (`shellMode()`: none → Browser,
  `"embedded"` → Embedded server, `"client"`/`"setup"`/`"reopen"` →
  Remote server, since setup reaches the app only by connecting and
  reopen never mounts it); Server version (`version`, `api_version`, and
  `rev` from `GetServerInfo`; "Not connected" when `store.daemon().live`
  is false); Signed in as (`store.caller()` handle and display name); a
  Keyboard shortcuts button that opens the overlay; Usage data (OQ2). New.
- **Appearance** (`appearance`): Reduce motion, Follow system or Always,
  per device in localStorage `compass.settings.reduceMotion`. Always sets
  `:root[data-reduce="on"]`, which the CSS honors and nothing sets. New.
- **Tracker** (`tracker`): exists; Open Question 1.
- **Models** (`models`): the model registry, read-only. Exists.
- **Providers** (`providers`): enrollment E5 (A5). Needs E3
  (`ProviderEnrollmentService`), not yet in the proto, Go, or the UI.

Not in this record: server URL edits (deferred, native server URL record
OQ-5); browser sign-out (no signed-out boot path); a day theme (D2,
"later"); key remap (deferred; a Keyboard section comes with it);
notifications (none exist); Secrets (`SecretsService` overlaps the
Providers key entry; its own design after E5); Replay tour (a General row
once the tour UI merges); `GetAgentConfigInfo` (fleet member names, not a
preference; a later Fleet section); pins and window layout (set in place).

### A2 — Layout

`.settings-view` is a grid of two columns: the section nav and the body.

- **Nav.** `<nav aria-label="Settings sections"
  class="cx-tabs settings-nav" data-orientation="v">` of `a.cx-tab` links;
  the current link has `data-selected` (the `.cx-tabs` selection rule)
  and `aria-current="page"`. Links, because each section is a route.
- **Narrow pane.** Under `@container view-panel (width < 560px)` (as in
  `app.css`), `settings.css` lays `.settings-nav` out as a row above the
  body and restates the `data-orientation="h"` border and bottom accent,
  since CSS cannot change the attribute; rows stack.
- **Body.** One section: a head (`--cx-text-sm`, `--cx-text-bright`,
  uppercase, letter-spaced, no weight, per brand-parity A9) and one line
  of help in `--cx-text-dim`; capped at `72ch`.
- **Row.** A grid, `minmax(16ch, 1fr) minmax(0, 2fr)`: the label (help
  under it in `--cx-text-dim`, `--cx-text-xs`, via `aria-describedby`),
  then the control; a `1px solid var(--cx-border)` line between rows; no
  card, radius, or fill.
- **Controls**, all primitives: `input.cx-input`, `select.cx-select`,
  plain text when read-only, `.cx-btn` actions (`primary` commits,
  `danger` destroys). Two or three choices are `.cx-btn` buttons with
  `aria-pressed` (the pressed one also `data-selected`) in a
  `role="group"` named by the row label. Row status (pending, done,
  error) sits under the row in a `role="status"`, errors in `--cx-error`.

### A3 — Route

`/settings/:section`. `parseRoute` maps bare `/settings` to the first
section and an unknown id to the Bridge. `routePath` always prints
`/settings/<section>`, so bare `/settings` is not canonical.

The catch-all `RedirectHome` (sends its view to `/`) becomes
`RedirectCanonical` (sends it to `routePath(view.route())`), so
`/settings`, old links, and restored layouts land on a section. A
behavior change: a path with extra segments now lands on the view it
parses to, not `/` (`/backlog/foo` → `/backlog`, `/agent/x/extra` →
`/agent/x`, `/channel/x/topic` → `/channel/x`). Unknown heads still land
on `/`.

Every opener (`showSettings` behind `Mod+,`, `G S`, the palette, and the
destinations list; and the sidebar link) targets `store.settingsPath()`,
the last section shown in this window or the first. Whether it navigates
the focused view or opens a tab is Open Question 3. A detached window
(DL-160) opens on its section. The palette gets "Go to Settings: Label"
per section; the title is "Settings · Label".

### A4 — Save model, per section

- **Immediate** for a device preference (Reduce motion, Usage data):
  applied and stored on click.
- **Per action** for Providers: each Connect, code, key, and Disconnect is
  one RPC with its own pending and error state on its row.
- **None** for read-only rows. No page-level Save; Tracker follows Open
  Question 1.

### A5 — Providers (E5)

S7 implements E5 as written (policy filter, connected state, Connect and
the paste-code dialog with `instructions`, expiry countdown and re-begin,
API-key entry, confirmed Disconnect, E5's store accessors, no token
rendered or stored), with two changes: "beside the tracker-config editor"
becomes one A2 row per provider in Providers, and the "draft/commit
pattern" becomes A4's per-action model (each action is one RPC).

## Alternatives considered

- **One long page with anchored headings.** The hash router owns `#`, so
  a heading cannot be linked; every section shares one scroll.
- **A modal dialog.** DL-160 makes Settings a window-scoped view.
- **Horizontal tabs only.** Four or five sections fit today, but more are
  named (Keyboard, Secrets, Fleet), and a strip under the window's tab
  strip (DL-390) reads as nested tabs. It is the narrow fallback.
- **A switch, or a roving radiogroup.** A new primitive for two rows, or
  arrow-key code built for dispatcher ids; `aria-pressed` needs neither.
- **One Save bar.** Device preferences and RPCs cannot wait for it.

## Global Constraints

- **After brand-parity T9.** T2a and T9 rewrite the `.settings-*` radii
  and weights and recapture every shot, so S1 starts after T9 merges. T7
  only changes the `.cx-input` border; tasks use `.cx-input` either way.
- **Overlap with brand parity.** T5 (`/agents`) edits `RouteMatch`,
  `parseRoute`, `routePath`, `appRoutes`, `route-title.ts`, `spine.ts`,
  `store.ts`, and `LeftSidebar.tsx`, as S1 does. T10 leaves
  `view-route.ts` and `ViewHost.tsx` alone but edits the `/backlog` and
  `/done` entries in `routes.tsx`, `LeftSidebar.tsx` and its test (S1),
  and `surfaces.md` § Backlog / Done / Settings (S4). The edits do not
  overlap in meaning; whichever lands second rebases.
- **Tokens only**, the D7 stylelint guard, and the T2a and T9 rules. Each
  component imports the primitive CSS it renders (brand-parity A7); no
  new primitive and no `.settings-*` box style (layout in `settings.css`).
- **Every entry keeps working:** `Mod+,`, `G S`, palette `view.settings`,
  the sidebar link, and `/#/settings`.
- **Device preferences** use localStorage keys `compass.settings.<name>`
  through `safeLocalStorage()`; an invalid value reads as the default.
- **Copy.** Sentence-case labels; help is one sentence.
- **Baselines and stacking.** Recapture under the pinned dev shell and
  commit per DL-399; one linear line, red-first tests, lane `implement-ts`.
- **Public repo.** Cite only this repo and `docs/specs/brand/`.

## Plan

S1 → S2 → S3 → S4 → S5; S6 and S7 stack on the top when their gate opens.

### S1 — Shell and section route

- **Gate:** brand-parity T9 merged.
- **Do:** A2, A3, with today's two parts as the first sections.
- **Interfaces:**
  - `apps/ui/src/view-route.ts`:
    `export const SETTINGS_SECTIONS = ["tracker", "models"] as const;`,
    `export type SettingsSection = (typeof SETTINGS_SECTIONS)[number];`,
    `export const SETTINGS_SECTION_LABEL: Record<SettingsSection, string>`.
    The settings member of `RouteMatch` becomes
    `{ view: "settings"; section: SettingsSection }`. `parseRoute`: no
    `param` → `SETTINGS_SECTIONS[0]`; a known id → that id; another →
    `{ view: "bridge" }`. `routePath` → `` `/settings/${section}` ``.
  - `ViewHost.tsx`: `ROUTE_PATTERN.settings = "/settings/:section"`.
    `routes.tsx`: `{ path: "/settings/:section", component: SettingsView }`;
    `RedirectHome` → `RedirectCanonical`, which calls
    `view.navigate(routePath(view.route()))`.
  - Store: `settingsSection(): SettingsSection`,
    `setSettingsSection(section: SettingsSection): void`, and
    `settingsPath(): string`. The `LeftSidebar` link passes
    `() => store.settingsPath()` to `openLink`; its plain click calls
    `showSettings`. `showSettings` runs `hideShortcuts()`, then per Open
    Question 3: (a) `navigateTo(settingsPath())`; (b)
    `dispatchLayout({ kind: "open", path: settingsPath() })`.
  - `components/SettingsView.tsx` moves to `components/settings/`:
    `SettingsView.tsx` (the shell: reads the section from its view scope,
    calls `setSettingsSection`, renders nav and body, imports `tabs.css`,
    `button.css`, `input.css`, and `./settings.css`); `TrackerSection.tsx`
    and `ModelsSection.tsx` (today's two blocks, unchanged);
    `SettingsRow.tsx` (`export function SettingsRow(props: { label: string;
    help?: string; for?: string; children: JSX.Element }): JSX.Element`);
    `sections.tsx` (`export const SECTION_VIEW: Record<SettingsSection, ()
    => JSX.Element>`); `settings.css` (shell, nav, fallback, rows).
  - `spine.ts`: a `view.settings.<id>` per section, "Go to Settings:
    Label", shaped like `view.settings`; `route-title.ts`: "Settings ·
    Label".
  - `app.css`: take `.settings-view` out of the shared list rule; delete
    `.settings-head` and `.settings-section*`.
  - Tests that expect bare `/settings` move to `/settings/<id>`:
    `view-route.test.ts`, `window-history.test.tsx`,
    `tab-keep-alive.test.tsx`, and `components/LeftSidebar.test.tsx` ("a
    view link opens its view in a new tab…" expects `["/",
    "/settings/tracker"]`). `settings-mapping.test.ts` changes its import;
    `SettingsView.model-registry.test.tsx` →
    `settings/ModelsSection.test.tsx`.
- **Test (red first):**
  - `view-route.test.ts`: `/settings/models` round-trips; `/settings`
    parses to the first section; `/settings/nope` parses to the Bridge.
  - `routing.test.tsx` (`RedirectCanonical`): `mountApp("/backlog/foo")`
    ends on `/backlog`; `mountApp("/no-such-surface")` still ends on `/`.
  - `settings/SettingsView.test.tsx` with `mountApp`: `/settings` ends on
    `/settings/tracker`, its link has `data-selected` and
    `aria-current="page"`; clicking Models moves to `/settings/models` and
    shows the registry, not the editor.
  - `store.test.ts` (`showSettings`, per Open Question 3), after the
    view moves to `/settings/models` and then to `/`: (a) one tab, its
    view on `/settings/models`; (b) from `/` it opens
    `/settings/models` in a second, active tab, and a repeat call from
    `/` focuses that tab (still two).
- **Baselines:** recapture `settings.png`; add `settings-narrow.png` (480px).

### S2 — General section

- **Interfaces:** `SETTINGS_SECTIONS` gains `"general"` first, with
  `settings/GeneralSection.tsx` (rows except Usage data, S6). `DaemonInfo`
  gains `rev: string` (`probeServer` reads it; `STUB_DAEMON.rev` is
  `""`). `AppStoreOptions` gains `serverUrl?: string`, set by `main` in
  `index.tsx` to `connection.baseUrl`; the store exposes `serverUrl():
  string | undefined`. `GeneralSection.tsx` exports `modeLabel(mode:
  ShellMode | undefined): "Browser" | "Embedded server" | "Remote
  server"`, an exhaustive switch.
- **Test (red first):** `settings/GeneralSection.test.tsx` with
  `createRouterTransport` serving `getServerInfo` (`1.2.3`, an API
  version, `abc123`): the three show; offline shows "Not connected"; the
  caller's handle shows; the shortcuts button opens the overlay;
  `modeLabel` maps all five inputs. **Baselines:** recapture `settings.png`.

### S3 — Appearance section

- **Interfaces:**
  - New `apps/ui/src/preferences.ts`:

    ```ts
    export type ReduceMotion = "system" | "on";
    export const REDUCE_MOTION_KEY = "compass.settings.reduceMotion";
    export function loadReduceMotion(storage: Storage | undefined): ReduceMotion;
    export function saveReduceMotion(storage: Storage | undefined, value: ReduceMotion): void;
    export function applyReduceMotion(root: HTMLElement, value: ReduceMotion): void;
    ```

  - Store: `reduceMotion(): ReduceMotion` and `setReduceMotion(value:
    ReduceMotion): void` (saves and applies); `mount.tsx` applies the
    stored value before the first render. `SETTINGS_SECTIONS` gains
    `"appearance"` after `"general"`; `settings/AppearanceSection.tsx`.
- **Test (red first):** `preferences.test.ts`: none or junk → `"system"`;
  `"on"` → `"on"`; apply `"on"` sets `data-reduce`, `"system"` removes it.
  `AppearanceSection.test.tsx`: Always sets `aria-pressed`, the root
  attribute, and the stored value.

### S4 — Tracker, per Open Question 1

- **Gate:** Open Question 1. Interfaces are for the recommended (c).
- **Interfaces (c):**
  - Delete `settings/TrackerSection.tsx`, `mergeFromTracker`,
    `settings-mapping.test.ts`, `"tracker"` from `SETTINGS_SECTIONS` and
    the `view.settings` keywords, and every remaining `.settings-*` rule.
  - `AppStore` drops `setTrackerConfig` and its two `store.test.ts` cases;
    `trackerConfig` stays at `DEFAULT_TRACKER_CONFIG` for the fixture queue.
  - `surfaces.md` § Backlog / Done / Settings: replace the status-mapping
    sentences (composition, empty state, flip item 2) with the A2 rows.
  - File a follow-up: a server-backed Tracker section with the tracker
    contract.
- **Interfaces (a):** the editor on A2 rows; the status map a `<table>` of
  free-text `input.cx-input` per state, as today (no source lists a
  tracker's statuses); Reset and Save in a sticky foot.
  `settings/tracker-draft.ts`: `createTrackerDraft(store: AppStore):
  TrackerDraft` (`draft()`, `dirty()`, `set(next)`, `reset()`, `save()`),
  held by the shell so a draft survives a section switch, with a nav dot
  when dirty; `surfaces.md`'s `.cx-tree-row` table becomes that table.
  **(b):** the same rows, read-only, under "Preview, not connected".
- **Test (red first):** (c): no Tracker link; `/settings/tracker` ends on
  `/`. (a): `tracker-draft.test.ts` (clean at start; `set` dirties;
  `reset` restores; `save` commits and cleans); an edit survives a switch
  to Models and back. **Baselines:** `settings.png`; (a) and (b) add
  `settings-tracker.png`.

### S5 — Tracker persistence

- **Gate:** Open Question 1 is (a); otherwise dropped.
- **Interfaces:** `preferences.ts` adds `loadTrackerConfig(storage:
  Storage | undefined, workspace: string): TrackerConfig | undefined` and
  `saveTrackerConfig(storage: Storage | undefined, workspace: string,
  config: TrackerConfig): void`, key `compass.settings.tracker.${workspace}`
  with the pins' `workspaceKey`, loaded with the pins and saved by
  `setTrackerConfig`. **Test (red first):** a new store on the same
  storage and workspace reads it; another workspace or bad JSON gives the
  default.

### S6 — Usage data

- **Gate:** Open Question 2. Interfaces are for the recommended (b).
- **Interfaces:** `Analytics` gains `readonly enabled: boolean`,
  `optOut()`, `optIn()`, and `optedOut(): boolean` (posthog-js
  `opt_out_capturing()`, `opt_in_capturing()`,
  `has_opted_out_capturing()`); `NoopAnalytics` has `enabled` false and
  `optedOut()` true. `AppStoreOptions` gains `analytics?: Analytics` and
  the store exposes `analytics(): Analytics | undefined` (reused if the
  tour analytics branch lands it first). A Usage data row in
  `GeneralSection.tsx`: On and Off `aria-pressed` buttons, or "Off. This
  build sends no usage data." when disabled.
- **Test (red first):** `analytics.test.ts`: `optOut` calls
  `opt_out_capturing`. `GeneralSection.test.tsx` with a fake `Analytics`:
  Off calls `optOut`, On calls `optIn`, the pressed button follows
  `optedOut()`, and a disabled build shows the text and no buttons.

### S7 — Providers section (E5)

- **Gate:** enrollment E3 merged. **Do:** A5; E5's Interfaces and test
  cycle are the contract, plus: `SETTINGS_SECTIONS` gains `"providers"`
  after `"models"` (`settings/ProvidersSection.tsx`); `openExternal`
  moves from `MarkdownText.tsx` to `apps/ui/src/open-external.ts`
  (`export function openExternal(url: string): void`) for `auth_url` in
  both hosts; a test that only `ListProviders`' providers render.
  **Baselines:** add `settings-providers.png`.

## Tasks

- [ ] S1 — Shell and route (after brand-parity T9; opener per OQ3)
- [ ] S2 — General section (after S1)
- [ ] S3 — Appearance section (after S2)
- [ ] S4 — Tracker per Open Question 1 (after S3 and Open Question 1)
- [ ] S5 — Tracker persistence (only if Open Question 1 is (a); after S4)
- [ ] S6 — Usage data (after S2 and Open Question 2; top of the line)
- [ ] S7 — Providers, E5 (after S1 and enrollment E3; top of the line)

## Open Questions

1. **The tracker editor.** It edits a config only the fixture seam reads,
   and only its handle; the kind and the status map change nothing.
   DL-129 puts the mapping on the server (the reverse
   `TrackerStatusMapping`).
   - (a) Keep it, rebuild it on A2 (S4), persist it per `workspaceKey`
     (S5). Small, but it saves a value nothing uses
     (`rule://no-inert-gating`).
   - (b) Show it read-only as "Preview, not connected"; drop S5. Honest,
     but it shows a status map the server does not use.
   - (c) Delete the editor and `setTrackerConfig`; a server-backed Tracker
     section returns with the tracker contract. Loses the one editable
     setting and its tests.
   - **Recommendation:** (c). Nothing it writes reaches the server, and a
     preview of an unused value misleads more than it shows.
2. **A usage-data control, and where it goes.** No record decides
   analytics consent. The observability record sends managed builds to
   Rigel's PostHog; self-hosted builds are off or use the deployer's key,
   and most have no key.
   - (a) A per-device On or Off in its own Privacy section, stored by
     posthog-js (`opt_out_capturing()`). With no key, the section is one
     sentence.
   - (b) The same control as a row in General: four sections, or five if
     Open Question 1 keeps Tracker. With no key, one read-only line.
   - (c) No control; the deployer decides at build time. A managed-build
     user cannot opt out in the app.
   - (d) Per-account consent stored on the server. Follows the account;
     needs a proto field and a server task.
   - **Recommendation:** (b): user control now, no server work, no
     near-empty section; (d) can replace its storage later.
3. **How a Settings opener behaves.** DL-390: "links navigate the focused
   view in place and `Mod`+click or middle-click opens a tab"; only that
   tab path dedupes an open path. Today `showSettings` follows it
   (`navigateFocused`), like `showBacklog` and `showDone`.
   - (a) Keep DL-390. `Mod+,`, `G S`, the palette, and the sidebar link
     navigate the focused view to the last section; bare `/settings`
     still redirects to the first. Dedupe stays with Mod-click and
     middle-click. One rule for every view, no ledger change; but `Mod+,`
     replaces the focused view (Back returns), and with Settings in a
     background tab it makes a second Settings view.
   - (b) Settings openers open or focus a Settings tab, amending DL-390
     for Settings only, as DL-409 did for New tab. Keeps the user's place
     and never duplicates Settings, like an editor's settings tab. Costs a
     second DL-390 exception, Settings unlike the other `show*` openers,
     and the 10-tab cap can refuse `Mod+,`.
   - **Recommendation:** (a). DL-390 is uniform, and Mod-click and
     middle-click already open a deduped tab. Revisit as (b) if dogfood
     shows duplicate Settings views.
