# Compass Settings page

Builds on: brand parity (`compass-brand-parity/design.md`, open PR #1915:
square corners, weights, surfaces, `--cx-border-field`, text-entry
primitives), [UX foundation](../compass-ux-foundation/design.md)
Refs: RIG-4775 (this record); RIG-3121 (Providers, task E5 of the
[gateway OAuth enrollment record](../../server/compass-gateway-oauth-enrollment/design.md));
ledger DL-431, DL-433, DL-434 (DL-432 is held for Open Questions 1 and 2)
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
6. **The one edit does little.** The store always uses
   `createFixtureTrackerSeam(DEFAULT_TRACKER_CONFIG)`, which ignores its
   `_config`; only the handle reaches the fixture queue. The kind and the
   status map change nothing, and the server maps tracker status itself
   (DL-129). Nothing shows the server, account, motion, or providers.

Brand-parity T2a and T9 fix defects 2 and 3 (they sweep every radius and
`font-weight: 600` in `app.css`). This record fixes the rest: one view
with a section nav, a route per section, one row anatomy on the shared
primitives, and a save model per section.

## Approach

### A1 — Sections and settings

Sections in nav order, with the recommended Open Question answers:

- **General** (`general`)
  - Server URL: read-only `connection.baseUrl`, the URL the transport uses
    in both hosts. New.
  - Mode: read-only `shellMode()`: none → Browser, `"embedded"` → Embedded
    server, `"client"`/`"setup"`/`"reopen"` → Remote server (setup reaches
    the app only by connecting; reopen never mounts it). New.
  - Server version: read-only `version`, `api_version`, and `rev` from
    `GetServerInfo` (the UI reads the first two today). "Not connected"
    when `store.daemon().live` is false. New.
  - Signed in as: read-only handle and display name, `store.caller()`. New.
  - Keyboard shortcuts: a button that opens the shortcuts overlay. New.
  - Usage data: Open Question 2. New.
- **Appearance** (`appearance`): Reduce motion, Follow system or Always,
  per device in localStorage `compass.settings.reduceMotion`. Always sets
  `:root[data-reduce="on"]`, which the CSS already honors and nothing sets
  today. New.
- **Tracker** (`tracker`): exists; its future is Open Question 1.
- **Models** (`models`): the model registry, read-only. Exists.
- **Providers** (`providers`): enrollment E5 (A5). Needs E3
  (`ProviderEnrollmentService`), not yet in the proto, Go, or the UI.

Not in this record: editing the server URL (deferred, native server URL
record OQ-5); browser sign-out (no signed-out boot path); a day theme
(ux-foundation D2, "later"); key remap (deferred; a Keyboard section comes
with it); notifications (none exist); Secrets (`SecretsService` overlaps
the Providers API-key entry, so it gets its own design after E5); Replay
tour (a General row once the tour UI merges); the fleet config bundle
(`GetAgentConfigInfo`, read-only member names: fleet information, not a
preference; a later Fleet section can show it beside Models); pinned
agents and window layout (set in place, they stay there).

### A2 — Layout

`.settings-view` is a grid of two columns: the section nav and the body.

- **Nav.** `<nav aria-label="Settings sections">` of links in the
  vertical `.cx-tabs` look; the current one has `aria-current="page"`.
  Links, because each section is a route that opens, copies, and goes back.
- **Narrow pane.** Under `@container view-panel (inline-size < 560px)` the
  nav is a horizontal `.cx-tabs` strip above the body, and rows stack.
- **Body.** One section. A head (`--cx-text-sm`, `--cx-text-bright`,
  uppercase, letter-spaced, no weight, per brand-parity A9) and one line
  of help in `--cx-text-dim`. Capped at `72ch`.
- **Row.** A grid, `minmax(16ch, 1fr) minmax(0, 2fr)`: the label (help
  under it in `--cx-text-dim`, `--cx-text-xs`, via `aria-describedby`),
  then the control. A `1px solid var(--cx-border)` line between rows; no
  card, radius, or fill.
- **Controls**, all primitives, no box styles: `input.cx-input`,
  `select.cx-select`, plain text for read-only values, `.cx-btn` actions
  (`primary` commits, `danger` destroys). One of two or three is `.cx-btn`
  buttons with `aria-pressed` (the pressed one also `data-selected`) in a
  `role="group"` named by the row label; each is a tab stop.
- **Row status.** Pending, done, or error text in a `role="status"` under
  the row; errors in `--cx-error`.

### A3 — Route

`/settings/:section`. `parseRoute` maps bare `/settings` to the first
section and an unknown section id to the Bridge (as `/agent` with no id
does). `routePath` always prints `/settings/<section>`, so bare
`/settings` is not canonical.

A non-canonical path renders the catch-all, `RedirectHome`, which sends
its own view to `/`. It becomes `RedirectCanonical`, which sends its view
to `routePath(view.route())`: `/` for every path that goes home today,
`/settings/<first>` for `/settings`, so old links and restored layouts
land on a section.

Every opener (`showSettings`, behind `Mod+,`, `G S`, the palette, and the
destinations list; and the sidebar link) targets `store.settingsPath()`,
the last section shown in this window or the first. The layout's `open`
dedupe matches exact paths, so an open Settings tab on that section is
focused, not duplicated. A detached window (DL-160) opens on its section.
The palette gets "Go to Settings: Label" per section; the window title is
"Settings · Label".

### A4 — Save model, per section

- **Immediate** for a device preference (Reduce motion, Usage data): the
  change applies and is stored on click.
- **Per action** for Providers: each Connect, code, key, and Disconnect is
  one RPC with its own pending and error state on its row. Fields keep
  their text until submitted.
- **None** for read-only rows. No page-level Save; Tracker follows Open
  Question 1.

### A5 — Providers (E5)

S7 implements E5 as written: the `ListProviders` policy filter, connected
state, Connect then the paste-code dialog with `instructions`, the
attempt-expiry countdown and re-begin, API-key entry, Disconnect with a
confirm, E5's store accessors, and no token value rendered or stored. It
changes E5 in two points only: E5's "beside the tracker-config editor"
becomes the Providers section, one A2 row per provider; and E5's
"draft/commit pattern" becomes A4's per-action model, because each action
is its own RPC and the tracker editor may go (Open Question 1).

## Alternatives considered

- **One long page with anchored headings.** The hash router owns `#`, so
  a heading cannot be linked, and every section shares one scroll.
- **A modal dialog.** DL-160 makes Settings a window-scoped view.
- **Horizontal tabs only.** Four or five sections fit a strip today, but
  the out-of-scope list names more (Keyboard, Secrets, Fleet, a
  server-backed Tracker), and a strip under the window's tab strip
  (DL-390) reads as nested tabs. It stays as the narrow fallback.
- **A switch primitive, or a roving radiogroup.** A switch is a new
  primitive for two rows. A radiogroup needs one tab stop and arrow keys;
  `createRovingGroup` is built around dispatcher command ids.
  `aria-pressed` buttons need neither.
- **One Save bar for the whole page.** Device preferences and RPC actions
  cannot wait for a page-level Save.

## Global Constraints

- **After brand-parity T9.** T2a and T9 rewrite the `.settings-*` radii
  and weights and recapture every shot; S1 starts after T9 merges, so it
  does not rebase on the same lines or recapture twice. T7 only changes
  the `.cx-input` border; tasks use `.cx-input` before or after it.
- **Route conflicts.** Brand-parity T5 (`/agents`) and T10 (`/backlog`
  and `/done` render the Bridge) edit `RouteMatch`, `parseRoute`,
  `ROUTE_PATTERN`, and `appRoutes`. The cases do not overlap; whichever
  lands second rebases.
- **Tokens only**, the D7 stylelint guard, and the T2a and T9 rules.
- **Each component imports the primitive CSS it renders** (brand-parity
  A7). No new primitive and no `.settings-*` box style; layout classes in
  `settings.css` only.
- **Every entry keeps working:** `Mod+,`, `G S`, palette `view.settings`,
  the sidebar link, and `/#/settings`.
- **Device preferences** use localStorage keys `compass.settings.<name>`
  through `safeLocalStorage()`; an invalid value reads as the default.
- **Copy.** Sentence-case labels; one-sentence help.
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
    `settingsPath(): string`; `showSettings` navigates to
    `settingsPath()`; the `LeftSidebar` link uses
    `() => store.settingsPath()`.
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
  - Tests that expect bare `/settings` move to `/settings/<id>`
    (`view-route.test.ts`, `window-history.test.tsx`,
    `tab-keep-alive.test.tsx`); `settings-mapping.test.ts` changes its
    import; `SettingsView.model-registry.test.tsx` →
    `settings/ModelsSection.test.tsx`.
- **Test (red first):** `view-route.test.ts`: `/settings/models`
  round-trips; `/settings` parses to the first section; `/settings/nope`
  parses to the Bridge. `settings/SettingsView.test.tsx` with `mountApp`:
  `/settings` ends on `/settings/tracker` with that link
  `aria-current="page"`; clicking Models moves to `/settings/models` and
  shows the registry, not the editor; after that, `store.showSettings()`
  returns to `/settings/models`; `/nope` still ends on `/`.
- **Baselines:** recapture `settings.png`; add `settings-narrow.png` (480px).

### S2 — General section

- **Do:** A1 General, without Usage data (S6).
- **Interfaces:**
  - `SETTINGS_SECTIONS` gains `"general"` first;
    `settings/GeneralSection.tsx` in `SECTION_VIEW`.
  - `DaemonInfo` gains `rev: string`; `probeServer` reads `rev`;
    `STUB_DAEMON.rev` is `""`.
  - `AppStoreOptions` gains `serverUrl?: string`, which `main` in
    `index.tsx` sets to `connection.baseUrl`; the store exposes
    `serverUrl(): string | undefined`.
  - `GeneralSection.tsx` exports `modeLabel(mode: ShellMode | undefined):
    "Browser" | "Embedded server" | "Remote server"`, an exhaustive switch.
- **Test (red first):** `settings/GeneralSection.test.tsx` with
  `createRouterTransport` serving `getServerInfo` (`1.2.3`, an API
  version, `abc123`): the three show; offline shows "Not connected"; the
  caller's handle shows; the shortcuts button opens the overlay;
  `modeLabel` maps all five inputs.
- **Baselines:** recapture `settings.png` (now General).

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

- **Gate:** Open Question 1. Interfaces are for the recommended (e).
- **Interfaces (e):**
  - Delete `settings/TrackerSection.tsx`, `mergeFromTracker`,
    `settings-mapping.test.ts`, `"tracker"` from `SETTINGS_SECTIONS`, the
    `"tracker"` keyword of `view.settings`, and every remaining
    `.settings-*` selector in `app.css`.
  - `AppStore` drops `setTrackerConfig` and its two `store.test.ts` cases;
    `trackerConfig` stays at `DEFAULT_TRACKER_CONFIG` for the fixture
    queue.
  - `apps/ui/src/design/surfaces.md` § Backlog / Done / Settings: replace
    the status-mapping editor sentences (composition, empty state, flip
    item 2) with the A2 row anatomy.
  - File a follow-up: a server-backed Tracker section once the tracker
    contract lands.
- **Interfaces (a):** the editor on A2 rows; the status map is a `<table>`
  with one free-text `input.cx-input` per state, as today (no source lists
  a tracker's statuses); Reset and Save in a sticky section foot.
  `settings/tracker-draft.ts` exports `createTrackerDraft(store: AppStore):
  TrackerDraft` (`draft()`, `dirty()`, `set(next)`, `reset()`, `save()`),
  held by the shell so a draft survives a section switch, with a nav dot
  when dirty. `surfaces.md`'s "two-column `.cx-tree-row` table" becomes
  that table. **(d):** the same rows, read-only, under "Preview, not
  connected".
- **Test (red first):** (e): no Tracker link, and `/settings/tracker` ends
  on `/`. (a): `tracker-draft.test.ts` (clean at start; `set` dirties;
  `reset` restores; `save` commits and cleans), and an edit survives a
  switch to Models and back.
- **Baselines:** recapture `settings.png`; (a) and (d) add
  `settings-tracker.png`.

### S5 — Tracker persistence

- **Gate:** Open Question 1 is (a); otherwise dropped.
- **Interfaces:** `preferences.ts` adds `loadTrackerConfig(storage:
  Storage | undefined, workspace: string): TrackerConfig | undefined` and
  `saveTrackerConfig(storage: Storage | undefined, workspace: string,
  config: TrackerConfig): void`, key `compass.settings.tracker.${workspace}`
  with the pins' `workspaceKey`. The store loads it with the pins;
  `setTrackerConfig` saves it.
- **Test (red first):** `store.test.ts`: a new store on the same storage
  and workspace reads the saved config; another workspace and malformed
  JSON give the default.

### S6 — Usage data

- **Gate:** Open Question 2. Interfaces are for the recommended (b).
- **Interfaces:**
  - `Analytics` gains `readonly enabled: boolean`, `optOut()`, `optIn()`,
    and `optedOut(): boolean` (posthog-js `opt_out_capturing()`,
    `opt_in_capturing()`, `has_opted_out_capturing()`); `NoopAnalytics`
    has `enabled` false and `optedOut()` true.
  - `AppStoreOptions` gains `analytics?: Analytics`; the store exposes
    `analytics(): Analytics | undefined` (reused if the tour analytics
    branch lands it first).
  - A Usage data row in `GeneralSection.tsx`: On and Off `aria-pressed`
    buttons, or "Off. This build sends no usage data." when disabled.
- **Test (red first):** `analytics.test.ts`: `optOut` calls
  `opt_out_capturing`. `GeneralSection.test.tsx` with a fake `Analytics`:
  Off calls `optOut`, On calls `optIn`, the pressed button follows
  `optedOut()`, and a disabled build shows the text and no buttons.

### S7 — Providers section (E5)

- **Gate:** enrollment E3 merged. **Do:** A5; E5's Interfaces and test
  cycle are the contract.
- **Interfaces:** E5's, plus: `SETTINGS_SECTIONS` gains `"providers"`
  after `"models"`; `settings/ProvidersSection.tsx`; `openExternal` moves
  from `MarkdownText.tsx` to `apps/ui/src/open-external.ts` (`export
  function openExternal(url: string): void`) so Connect opens `auth_url`
  in both hosts.
- **Test (red first):** E5's cycle, plus: only the providers
  `ListProviders` returns are rendered. **Baselines:** add
  `settings-providers.png`.

## Tasks

- [ ] S1 — Shell and section route (after brand-parity T9)
- [ ] S2 — General section (after S1)
- [ ] S3 — Appearance section (after S2)
- [ ] S4 — Tracker per Open Question 1 (after S3 and Open Question 1)
- [ ] S5 — Tracker persistence (only if Open Question 1 is (a); after S4)
- [ ] S6 — Usage data (after S2 and Open Question 2; top of the line)
- [ ] S7 — Providers, E5 (after S1 and enrollment E3; top of the line)

## Open Questions

1. **The tracker editor.** It edits a config that only the fixture seam
   reads, and only its handle; the kind and the status map change nothing.
   DL-129 puts the mapping on the server, which ingests tracker status
   through the reverse `TrackerStatusMapping`.
   - (a) Keep the editor, rebuild it on A2 (S4), and persist it in
     localStorage keyed by `workspaceKey` (S5). Small, but it saves a
     value nothing uses: `rule://no-inert-gating` argues against it.
   - (d) Show the config read-only as "Preview, not connected", and drop
     S5. Honest about the state, but it shows a status map the server
     does not use, which can mislead.
   - (e) Delete the client-side editor and `setTrackerConfig`; the
     mapping stays on the server. A Tracker section returns, server-backed,
     when the tracker contract lands. Loses the one editable setting
     today, and its tests.
   - **Recommendation:** (e). Nothing the editor writes reaches the
     server, and a read-only preview of a value the server does not use
     misleads more than it shows.
2. **A usage-data control, and where it goes.** No record decides
   analytics consent. The observability record sends managed builds to
   Rigel's PostHog and keeps self-hosted builds off by default or on the
   deployer's own key; most self-hosted builds have no key.
   - (a) A per-device On or Off in its own Privacy section, stored by
     posthog-js (`opt_out_capturing()`). In a build with no key, the
     section is one sentence.
   - (b) The same control as a Usage data row in General, giving four
     sections, or five if Open Question 1 keeps Tracker. A build with no
     key shows one read-only line.
   - (c) No user control; the deployer decides at build time. No work, but
     a user of a managed build cannot opt out in the app.
   - (d) A per-account consent stored on the server and read at boot.
     Follows the account, but needs a proto field and a server task.
   - **Recommendation:** (b). It gives a user control now with no server
     work and no near-empty section; (d) can replace its storage later.
