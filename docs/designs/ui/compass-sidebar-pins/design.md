# Compass configurable sidebar pins

Status: Active

Tracker: RIG-1632.

Ledger: this record's PR appends DL-096 to
`docs/designs/product/DECISIONS.md` in the same diff (see §Ledger delta) and
supersedes no existing row (§Ledger delta states the call), so it satisfies
the ledger gate's touch-coupling leg directly — no `Ledger-impact:` escape
hatch is needed in the PR body.

**Merge order:** this record's PR lands **after** PR #1058 (the agent-trees
record, DL-095) merges — rebase before freeze — so the ledger gate's DL-095
reference and this record's citations into `compass-agent-trees/design.md`
resolve at merge time.

**This is a de-special-casing record.** It designs the presentation layer the
agent-trees record names and defers: DL-095 froze that the agent tree is
Compass's organizing primitive, that no agent is special-cased, and that
"pinning is layered on top, never a hole in the structure"
(`compass-agent-trees/design.md` §The tree replaces the user-defined folder
organization: "Whether an agent is *also* surfaced in a pinned pane or a
fleet tab is a separate presentation choice (configurable pins, its own
record), not a reason to drop it from the tree"). This record is that
separate presentation choice: sidebar pinning becomes a **user-configurable
presentation layer over the agent set** — any agent can be pinned as a
right-sidebar tab, and the built-in privileged Supervisor/Warden pair is
removed.

## Problem / Intent

The right sidebar hardcodes two named agents as always-on privileged
surfaces. `RIGHT_SIDEBAR_TAB_BY_ID` bakes the pair in with fixed account ids
(`compass/apps/ui/src/constants.ts:72-93`):

```ts
supervisor: {
  id: "supervisor",
  icon: "◆",
  title: "Supervisor",
  group: "fleet",
  agentId: "acc-supervisor",
},
warden: {
  id: "warden",
  icon: "🛡",
  title: "Warden",
  group: "fleet",
  agentId: "acc-warden",
},
status: { id: "status", icon: "▦", title: "Fleet status", group: "fleet" },
```

The special-casing reaches into the type system: the tab union names the two
agents as string literals (`compass/apps/ui/src/store.ts:82-84`: `export
type FleetTab = "supervisor" | "warden" | "status";` / `export type IssueTab
= "files" | "vcs" | "pr";` / `export type RightSidebarTab = FleetTab |
IssueTab;`), and the shell boots onto one of them
(`compass/apps/ui/src/store.ts:578-581`: "Boots onto the Supervisor
conversation (D6)" / `createSignal<RightSidebarTab>("supervisor")`).

Matt ruled (2026-08-01): make sidebar pins configurable, **so we can
de-special-case the supervisor and warden**. Under DL-095's frozen frame —
no agent is special-cased; every agent lives in the derived tree; pinning is
a separate presentation layer, never a hole in the tree — a hardcoded
two-agent fleet group is exactly the special-casing being retired. The
intent: the fleet group of the right-sidebar activity bar becomes a
user-configured **pin set** — any agent the user pins gets a full-height
conversation tab there, defaulting to none, with no baked-in pair.

## Approach

### Pins are presentation over the tree, never structure

The frozen premise (DL-095, cited above) does the framing work: the agent
tree carries *every* agent; a pin never removes an agent from the tree and
never grants it model-level privilege. A pin is one thing only — a
user-chosen shortcut that surfaces an agent's conversation as an always-visible
right-sidebar tab, so the user can watch the board while talking to the
agents *they* care about, whichever agents those are. The agent-trees record
already names the natural gesture ("the tree makes 'pin this subtree's lead'
the natural gesture — pin the parent, and its reporting line is one expand
away", `compass-agent-trees/design.md` §How each surface flows from the
tree); this record gives that gesture its mechanism.

What DL-034/DL-036 built stays: one right sidebar, one activity bar, a fleet
group above a divider and an issue group below
(`compass/apps/ui/src/constants.ts:43-45`: "Activity-bar group: fleet tabs
render above the divider, issue below" / `export type RightTabGroup = "fleet"
| "issue";`). What changes is the fleet group's *membership*: from a
hardcoded `Supervisor · Warden · Status` to `⟨the user's pinned agents⟩ ·
Status`. The dock-in-sidebar record's D2 rationale ("the fleet agents are
always-on and are the new primary use") is refined rather than discarded —
"always-on" was never a property of two particular agents; it is a property
the *user* assigns by pinning.

### The tab model: pins are data, not union members

A configurable pin set cannot live in a closed string union of two agent
names. The union breaks open along its existing fleet/issue seam:

- **Issue tabs stay a closed union.** `IssueTab = "files" | "vcs" | "pr"`
  (`store.ts:83`) is genuinely closed — those are Compass surfaces, not
  agents — and keeps its mapped-object entries in
  `RIGHT_SIDEBAR_TAB_BY_ID`-style constants.
- **`status` stays a fixed tab.** It is the one fleet tab that is not an
  agent (`constants.ts:89`: `status: { id: "status", icon: "▦", title:
  "Fleet status", group: "fleet" }`) — a fleet-metrics pane with no
  `agentId` — so it is not a pin and cannot be unpinned in the MVP.
- **Agent tabs become derived data.** `FleetTab`'s two agent literals are
  deleted. The replacement tab id is derived from the pinned account id
  (shape: `` `agent:${accountId}` ``), and the activity-bar items for the
  fleet group are computed from the pin set: for each pinned account id, an
  `ActivityBarItem` with `group: "fleet"` and `agentId` set. The
  `ActivityBarItem` interface already carries the needed optional field
  (`constants.ts:51-61`: `agentId?: string;` — "Fleet agent tabs: the agent
  whose `StateDot` badges the tab icon"), so the item shape is unchanged;
  only its provenance moves from a hand-written constant to a derivation.

Consequences, stated so downstream tasks inherit them:

- `RIGHT_SIDEBAR_TAB_BY_ID`'s exhaustiveness trick ("Keyed on the full
  `RightSidebarTab` union in a mapped object, so TypeScript rejects the
  module unless EVERY tab has an activity-bar entry",
  `constants.ts:63-71`) survives only for the *static* tabs
  (`status` + issue tabs). Dynamic agent tabs cannot be statically
  enumerated; their items are derived at runtime from the pin set, and
  `RIGHT_SIDEBAR_TAB_GROUPS` (`constants.ts:100-108`, which today derives
  the two groups from the static table by `group` filter) becomes a store
  derivation: fleet group = derived pin items + the static `status` item,
  issue group = the static issue items.
- The chrome-hiding predicate keeps working unchanged in kind: it keys off
  the active tab's `group` (fleet tabs hide the card-scoped chrome), and
  derived pin items carry `group: "fleet"` like the hardcoded pair did.
- The active-tab signal (`store.ts:277-280`: `activeRightTab:
  Accessor<RightSidebarTab>` + `setActiveRightTab`) keeps its shape; its
  type widens to the opened union. Unpinning the active agent tab falls
  back to `status`.
- Pinned-tab icons: the hand-picked `◆`/`🛡` glyphs were part of the
  hardcoding. A derived agent tab renders a generic agent glyph plus the
  agent's `StateDot` badge (the `agentId` mechanism that already exists);
  per-pin custom icons are out of scope.

### Where pins live: a per-user UI preference, not fleet config

The pin set is **per-user presentation state, owned by the UI**, not
server-distributed agent configuration:

- **Not DL-078's bundle store.** DL-078 is explicitly fleet-wide agent
  config — "a Server-side FLEET-WIDE bundle store (one bundle all agents
  get)" (`DECISIONS.md` DL-078) — distribution of skills/extensions/MCP
  config *to agents*. A pin set is neither fleet-wide (it is one user's
  view preference) nor agent-facing (no agent behaves differently for being
  pinned). Putting per-user UI preference in the one-bundle-for-all store
  would be a category error on both axes.
- **MVP home: the client-local UI store,** exactly where every existing UI
  preference lives today — collapse state
  (`store.ts:273-274`: `isFolderCollapsed: (folderId: string) => boolean;`
  / `toggleFolder`), section collapse (`store.ts:570-572`:
  `const [sectionCollapsed, setSectionCollapsed] =
  createSignal<ReadonlySet<string>>(new Set());`), and the active tab
  itself (`store.ts:580-581`). The pin set is one more signal of this kind:
  an ordered list of pinned account ids (append-on-pin) with pin/unpin
  setters; reorder is deferred (Open Question 1).
  To make pins survive a reload — table stakes for a preference the user
  curates — the signal is backed by `localStorage` (the UI's first
  persisted preference; a deliberate, tiny mechanism justified by the fact
  that a pin set that resets every session is not "configurable" in any
  useful sense). Existing ephemeral prefs are not migrated to it.
- **Named post-MVP seam:** a server-side per-user preferences surface
  (roaming pins across devices). When such a surface exists, the pin set is
  its obvious first tenant; nothing in the MVP shape blocks the move (the
  store signal becomes server-hydrated instead of localStorage-hydrated).

### Defaults: empty pin set, Status landing tab

- **The default pin set is empty.** Seeding it with supervisor/warden would
  re-encode the special-casing one layer up. A fresh workspace shows the
  fleet group as just `Status`; the user (or first-run guidance) pins from
  there. Pinning is discoverable from the tree: each agent row in the
  workspaces sidebar offers pin/unpin.
- **The shell boots onto `status`.** Today it boots onto the Supervisor
  conversation (`store.ts:578-581`, quoted in §Problem / Intent) — a
  default that presumes a privileged agent exists. With no baked-in pair,
  `status` is the only fleet tab guaranteed to exist and meaningful with
  zero selection, so it is the boot default. When the user has pins, the
  shell boots onto the **first pin that resolves to a visible agent**
  instead (an unresolvable hydrated pin is skipped, never a blank boot
  pane) — preserving the
  dock-in-sidebar D6 experience (board + a conversation side by side) with
  the user's chosen agent in the seat the Supervisor used to be hardcoded
  into.

### Relation to the left sidebar's view pins

The left sidebar already has a pin concept — for views, not agents: "the
Workspace header and Bridge/Backlog/Done/Settings nav links pinned at the
top" (`compass/apps/ui/src/components/LeftSidebar.tsx:332-333`). These are
**two different surfaces and stay two**: the left nav pins are fixed
navigation chrome (not user-configurable, not agent-bearing), while agent
pins are right-sidebar conversation tabs (user-configurable, agent-bearing).
The MVP does not unify them — a single "pin anything anywhere" framework is
speculative machinery with no current consumer. The shared *word* is fine;
the shared *mechanism* is not built. If view pins ever become configurable,
that is its own record.

### Scope fences

- **Presentation only.** Pins do not touch the agent tree, do not touch
  `parent_agent_id`, and are never a hole in the tree — a pinned agent is
  also in the tree (DL-095, frozen). No server model change ships in this
  record.
- **Roles are RIG-1623.** The `supervisor`/`warden` *role* vocabulary
  survives this record untouched — e.g. the tree row's role pip
  (`LeftSidebar.tsx:52-55`: `<Show when={a().role !== "worker"}>` …
  `{a().role === "supervisor" ? "◆" : "🛡"}`) still keys glyphs off the
  role. That is a role-vocabulary echo, flagged here as downstream cleanup
  for RIG-1623, not redesigned: this record removes the pin *hardcoding*,
  not the role concept.
- **Board and tree surfaces** are Record C's and its downstream tasks';
  nothing here reorders lanes or filters trees.

## Alternatives considered

### Keep a hardcoded fleet pair, add extra pins around it

Leave `supervisor`/`warden` baked in and let users pin more agents beside
them. Rejected: it keeps the privileged pair — the exact special-casing
DL-095 retires — and makes the type model worse (a closed union *and* a
dynamic set for the same group). The whole point is that no agent is
built-in.

### Fleet-wide pins via the DL-078 bundle store

Persist the pin set server-side in the existing fleet-wide config bundle so
every user sees the same pins. Rejected: DL-078 is one-bundle-for-all
*agent* config; pins are per-user *presentation* state (§Approach). A
fleet-wide pin set would also quietly rebuild the privileged-pair pattern —
an operator-blessed set of special agents every user gets.

### Unify agent pins with the left sidebar's view pins

One pin framework spanning the left nav links and right-sidebar agent tabs.
Rejected for the MVP: the two surfaces share a word, not a behavior — the
nav links are fixed chrome with no configuration surface, and forcing a
common abstraction now is speculative generality with one real consumer.

## Plan

### Global Constraints

- **No built-in privileged agent** (Matt, 2026-08-01; frame frozen by
  DL-095): no agent account id, agent name, or role appears as a constant,
  union literal, or default in the pin/tab model. The default pin set is
  empty.
- **Pins are per-user presentation state**: client-local UI store signal,
  `localStorage`-backed; never DL-078's fleet-wide bundle; server-side
  per-user preferences are a named post-MVP seam.
- **The activity-bar structure stands** (DL-034/DL-036): one right sidebar,
  one activity bar, fleet group above the divider, issue group below;
  `status` and the issue tabs remain fixed tabs.
- **Presentation only**: no proto/server change; the agent tree and
  `parent_agent_id` are untouched (Record C owns them); roles are RIG-1623.
- **Sequencing after Record C**: the code tasks (T2-T5) depend on Record
  C's every-agent tree derivation. Today's `STUB_TREE` excludes exactly
  the pair being de-special-cased ("The moat agents are not tree leaves:
  the supervisor is baked into its own pinned pane, and the warden lives
  in the right sidebar's fleet tabs",
  `compass/apps/ui/src/stub-data.ts:1242-1245`), so landing T2-T5 first
  would leave those agents with no hardcoded tab and no tree row to pin
  from — a de-facto hole the frozen premise forbids. This record's code
  lands after Record C's tree-derivation task, which puts every agent
  (supervisor and warden included) in the tree the pin affordance offers.
- **Vocabulary**: *pin / unpin*, *pin set*, *pinned agent tab*. "Fleet
  tabs" survives only as the group name for ⟨pins⟩ + Status.

### T1 — this record + ledger row (this PR, docs-only)

Freeze this record at `docs/designs/ui/compass-sidebar-pins/design.md`
and append DL-096 to `docs/designs/product/DECISIONS.md` in the same diff
(§Ledger delta). No code changes.

- Interfaces: `docs/designs/product/DECISIONS.md` (append one row under
  §UI shell); this file.

### T2 — open the tab union and derive the fleet group (downstream)

Break `FleetTab`'s agent literals out of the type system and derive the
fleet group from the pin set.

- Interfaces:
  - `compass/apps/ui/src/store.ts:82-84` — the closed union
    (`FleetTab = "supervisor" | "warden" | "status"`) is replaced:

    ```ts
    type PinnedAgentTab = `agent:${string}`;
    type RightSidebarTab = PinnedAgentTab | "status" | IssueTab;
    ```

    (`IssueTab` unchanged.)
  - `compass/apps/ui/src/constants.ts:72-93` — the `supervisor` and
    `warden` entries are deleted from `RIGHT_SIDEBAR_TAB_BY_ID`; the
    constant shrinks to the static tabs (`status`, `files`, `vcs`, `pr`)
    and its mapped-object key narrows accordingly.
  - `compass/apps/ui/src/constants.ts:100-108` —
    `RIGHT_SIDEBAR_TAB_GROUPS` is replaced by a store-level derivation:
    `rightTabGroups(): readonly { group: RightTabGroup; items: readonly
    ActivityBarItem[] }[]`, fleet = derived pin items + the static
    `status` item; issue = the static issue items. The derivation is the
    layer that filters unresolvable pins: `rightTabGroups()` emits a
    fleet item (`group: "fleet"`, `agentId` set, `id` the
    `agent:`-prefixed tab id) only for pins that resolve to a visible
    agent; a retained-but-unresolvable pin produces no activity-bar item
    and no pane while unresolvable (T3 keeps it in the persisted set).
    `ActivityBarItem` (`constants.ts:51-61`) is unchanged.
  - The chrome-hiding predicate and the activity-bar render consume
    `rightTabGroups()` instead of the static table; behavior keys off
    `group` exactly as today.
  - `compass/apps/ui/src/components/RightSidebar.tsx:553-565` — the pane
    `<Switch>`'s hardcoded `"supervisor"`/`"warden"` `<Match>` arms are
    replaced by a single arm whose `when` is a *resolvability* test on
    `activeRightTab()`: the active tab id is an `agent:`-prefixed pin **and**
    it resolves to a visible agent. The arm then resolves the account id
    from the tab id and passes the derived `ActivityBarItem` to `FleetPane`
    — so `FleetPane` renders for any pinned agent, not two named ones.
    Gating on resolvability (not the bare `agent:`-prefix) makes the "no
    pane while unresolvable" invariant hold for the *active* tab too: if the
    active tab's agent is unresolvable the arm does not fire and the pane
    falls through to `status`. The `status` and issue arms stay as-is.
  - In-slice test fallout — three test files hardcode the deleted
    literals and are updated in this slice:
    `compass/apps/ui/src/components/RightSidebar.fleetpane.test.tsx:52-57`
    (reads `RIGHT_SIDEBAR_TAB_BY_ID.supervisor`/`.warden` through
    `agentIdForTab`), `compass/apps/ui/src/store.test.ts:513-514`
    (`setActiveRightTab("warden")` round-trip), and
    `compass/apps/ui/src/components/RightSidebar.test.ts:123-179`
    (asserts `RIGHT_SIDEBAR_TAB_GROUPS` partitions the union against the
    hardcoded id list).

### T3 — the pin-set signal + persistence (downstream)

The pin set as a store signal with pin/unpin (ordered, append-on-pin),
`localStorage`-backed.

- Interfaces: `compass/apps/ui/src/store.ts` — alongside the existing UI
  prefs (`isFolderCollapsed`/`toggleFolder`, `store.ts:273-274`):
  `pinnedAgentIds: Accessor<readonly string[]>` (ordered, append-on-pin;
  reorder is deferred, Open Question 1), `pinAgent(accountId: string)`,
  `unpinAgent(accountId: string)`, `isPinned(accountId: string): boolean`.
  Hydrated from and written through a `localStorage` key **namespaced per
  server/workspace** (keyed by the connection's workspace identity), so
  one deployment's account ids never hydrate as pins on another. A pinned
  id that resolves to no visible agent is retained in the persisted set
  (visibility can fluctuate; the pin survives the agent coming back)
  while the T2 derivation filters it out — no activity-bar item, no pane.
  Unpinning the active tab falls back to `setActiveRightTab("status")`.
  The same fallback covers the symmetric transition the T2 arm gates on:
  when the *active* agent tab's pin becomes unresolvable through a
  visibility fluctuation (not an explicit unpin), the shell moves
  `activeRightTab` to `status` (`setActiveRightTab("status")`) — a
  live-active tab whose agent vanished never strands a pane, and the
  activity-bar selection agrees with the T2 Switch arm's resolvability gate.

### T4 — pin/unpin affordance in the workspaces tree (downstream)

The gesture lives where the agents live: each agent row in the left
sidebar's tree offers pin/unpin.

- Interfaces: `compass/apps/ui/src/components/LeftSidebar.tsx` — the agent
  row component (`AgentLeaf`, `LeftSidebar.tsx:34-61`) gains a pin toggle
  (hover affordance calling `pinAgent`/`unpinAgent`, state from
  `isPinned`). The role pip in the same row (`LeftSidebar.tsx:52-55`) is
  explicitly left alone (RIG-1623).
- Ordering: Record C's tree rebuild rewrites this same `AgentLeaf`
  component — C's tree derivation lands first, and this task adds the pin
  toggle onto the derived row (see §Global Constraints).

### T5 — boot default + landing behavior (downstream)

- Interfaces: `compass/apps/ui/src/store.ts:578-581` — the boot value of
  `activeRightTab` changes from `"supervisor"` to: the first hydrated pin
  that resolves to a visible agent (unresolvable pins are skipped,
  matching the T2 derivation), else `"status"`. The D6 no-auto-switch
  rule (card selection never steals the active tab) is unchanged.

## Tasks

- [ ] **T1** — this record + the DL-096 ledger row, same diff (this PR,
  docs-only).
- [ ] **T2** — open `RightSidebarTab`; delete the hardcoded
  supervisor/warden entries and pane `<Match>` arms; derive the fleet
  group from the pin set; update the three hardcoding tests.
- [ ] **T3** — `pinnedAgentIds` signal + pin/unpin +
  workspace-namespaced `localStorage` persistence.
- [ ] **T4** — pin/unpin affordance on tree agent rows.
- [ ] **T5** — boot default: first resolvable pin, else Status.

## Ledger delta

Appended to `docs/designs/product/DECISIONS.md` in the same PR that freezes
this record (touch-coupling), under **UI shell** — this is a shell
presentation decision, not a product thesis:

> | DL-096 | Sidebar pinning is a user-configurable presentation layer over the agent set (the layer DL-095 names and defers): any agent can be pinned as a right-sidebar fleet tab, the hardcoded always-on Supervisor/Warden tabs and their `FleetTab` union literals are removed (no built-in privileged agent), the pin set is a per-user client-local UI preference (`localStorage`-backed; never DL-078's fleet-wide bundle; server per-user prefs a post-MVP seam), the default pin set is EMPTY, and the shell boots onto the first pin that resolves to a visible agent, else Status, instead of a special-cased Supervisor conversation | Active (Matt, 2026-08-01) | [sidebar pins §Approach](compass-sidebar-pins/design.md#approach) |

**No existing row is superseded — the call, stated:** the hardcoded
Supervisor/Warden fleet tabs were never a ledgered decision. A grep of
`DECISIONS.md` for supervisor/warden/pin/sidebar rows finds no Active row
ratifying the pair as always-on pinned surfaces: DL-034 ("The right sidebar
mirrors Orca; fleet + workstream conversations live there") ratifies the
*sidebar structure* — which this record keeps — not any particular agent's
presence in it; DL-036 ("The bottom dock is folded into the right sidebar:
one activity bar, one signal, the dock removed") ratifies the fold, which
likewise stands. The specific `Supervisor · Warden · Status` membership and
the Supervisor boot default were record-level details of the dock-in-sidebar
record (its D2/D3/D6), never promoted to rows — and that record's framing
premise ("the fleet agents are always-on") was already retired by DL-095's
no-special-casing ruling. DL-035 is already Superseded (by DL-036) and DL-010
(Supervisor + Bridge orchestration in the MVP) is a topology/orchestration
ruling, not a pinning one — a supervisor may well exist and be pinned by its
user; it is just not *built in*. Nothing to flip.

## Open Questions

None is load-bearing for T1 (this docs-only PR); each carries a
recommendation for the downstream task that hits it.

1. **Pin reordering surface (for T3/T4).** The pin set is ordered (tab
   order in the activity bar). Is reorder drag-and-drop on the activity
   bar, or ordered-by-pin-time only for the MVP?
   **Recommendation:** pin-time order for the MVP (append on pin); drag
   reorder is a small later add that touches only the signal's setter.
2. **Pinning views or subtrees, not just agents (post-MVP).** Record C
   names "pin this subtree's lead" as the natural gesture — satisfied by
   pinning the parent agent. Pinning a whole *subtree* (one tab cycling a
   reporting line) or arbitrary *views* is out of scope here.
   **Recommendation:** agents only in the MVP; revisit subtree pins
   alongside the board's subtree filtering once both surfaces exist.
3. **First-run guidance (for T5 + docs).** With an empty default pin set, a
   fresh workspace boots onto Status beside the board. Does first-run
   guidance prompt the user to pin a root agent?
   **Recommendation:** yes — one-line hint on the empty fleet group
   pointing at the tree's pin affordance; folds into Record C's T6
   onboarding narrative (tree-building as the workflow).
