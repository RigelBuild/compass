# Compass sidebar pins — unreachable-pin amendment

Status: Active

Tracker: RIG-1645.

Amends: `compass-sidebar-pins` (RIG-1632, DL-096) §T2/§T3.

Ledger: this record's PR appends DL-098 to
`docs/designs/product/DECISIONS.md` in the same diff (see §Ledger delta).
DL-096 stays Active — DL-098 amends its unresolvable-pin sub-behavior, it
does not reverse the pinning layer (§Ledger delta states the call) — so the
ledger gate's touch-coupling leg is satisfied directly; no `Ledger-impact:`
escape hatch is needed in the PR body.

> **Amends `compass-sidebar-pins` (frozen).** This record is a sibling
> amendment to `docs/designs/product/compass-sidebar-pins/design.md` (RIG-1632,
> DL-096). The merged record is frozen; per sealed convention a later change
> ADDS a record. This amendment supersedes the frozen record's §T2
> unresolvable-pin filtering and §T3 fluctuation-coercion clauses per Matt's
> ruling of 2026-08-02 (PR #98's review medium). All file+line grounding below
> was verified against the working trees this run: the frozen record in the
> sealed repo, the shipped implementation in the compass repo
> (`apps/ui/src/…`, post-merge #98).

## Problem / Intent

The frozen sidebar-pins record rules that an unresolvable pin (its agent
dead, despawned, or filtered out of the visible set) silently vanishes: §T2
filters it from the activity bar ("a retained-but-unresolvable pin produces
no activity-bar item and no pane while unresolvable", frozen design.md:314-315)
and §T3 auto-switches an active agent tab to `status` when the agent's
visibility fluctuates away (frozen design.md:358-363). Matt ruled on
2026-08-02 (on PR #98's review medium) that this behavior is wrong, on two
grounds:

1. **UX — silent disappearance is disorienting.** An agent vanishing from the
   sidebar mid-view yanks the surface out from under the user. The user, not
   a visibility fluctuation, decides when a pin is dropped.
2. **Implementability — the shipped guard cannot deliver the frozen contract
   under live agents.** PR #98's review flagged (medium) that the synchronous
   set-time resolvability guard —

   ```ts
   // compass apps/ui/src/store.ts:679-683
   const setActiveRightTab = (tab: RightSidebarTab) => {
     if (tab.startsWith("agent:") && !agentIsVisible(tab.slice(6)))
       setActiveRightTabRaw("status");
     else setActiveRightTabRaw(tab);
   };
   ```

   — only fires when a tab is *set*. Once the live agent stream replaces the
   static `STUB_AGENTS` seed (`store.ts:602-603`, `const agents =
   STUB_AGENTS;`), an already-active `agent:X` whose agent vanishes is never
   re-set, so §T3's fluctuation-coercion never runs and the pane strands
   blank: the RightSidebar pane `<Switch>` (`RightSidebar.tsx:565-602`) has no
   default arm, and `activeFleetItem()` returns `undefined` for an
   unresolvable active tab (`RightSidebar.tsx:549-554`).

This amendment captures the ruling — the pin stays visible with an "agent
unreachable" pane and a manual unpin — and thereby dissolves the review
medium: no fluctuation-watcher is needed at all.

## Approach

Matt's ruling, verbatim:

> When a pinned agent becomes unresolvable (dead / despawned / filtered out),
> DO NOT auto-hide the pin and DO NOT auto-switch the active tab to status.
> KEEP the pin visible, render its pane with an "agent unreachable"
> (dead/unreachable) message, and let the user unpin deliberately when they
> choose. Rationale: an agent silently vanishing from the sidebar mid-view is
> disorienting; the user decides when to drop it.

### What this amendment asserts

1. **Every pin keeps its activity-bar item.** `rightTabGroups()` emits a fleet
   item for EVERY pinned id, resolvable or not; unresolvable items are marked
   so the render distinguishes them. The §T2 filter clause is superseded.
2. **An unresolvable pin's pane is the "agent unreachable" state.** The
   RightSidebar pane `<Switch>`'s `agent:` arm fires for ANY pinned-agent tab
   and renders either the live `FleetPane` (agent resolvable) or an "agent
   unreachable" pane (unresolvable). There is no fall-through-to-status —
   which also closes the Switch's no-default-arm gap defensively: no
   `agent:`-prefixed active tab can ever strand a blank pane.
3. **Removal is a manual unpin, only.** No auto-hide. The pin (and its bar
   item, and its unreachable pane) persists until the user unpins.
4. **A visibility fluctuation to unresolvable does NOT auto-switch
   active→status.** The §T3 fluctuation-coercion clause is superseded; the
   active tab stays put and shows the unreachable pane. Consequently the
   set-time resolvability guard (`store.ts:679-683`) is retired — selecting
   or keeping an unresolvable agent tab is now valid.
5. **Unpin-active→status IS retained.** Unpinning the active tab is a user
   gesture that removes the tab, so the existing fallback stands
   (`store.ts:1692`: `` if (activeRightTab() === `agent:${accountId}`)
   setActiveRightTab("status"); ``).

### Why this dissolves PR #98's review medium

The review medium said the synchronous set-time guard cannot implement §T3's
fluctuation-coercion once live agents replace the static seed — an active
`agent:X` whose agent vanishes would strand a blank pane. The ruling removes
the coerce-to-status requirement entirely: no `createEffect`
fluctuation-watcher is needed, and the blank-pane failure mode is replaced by
an intentional unreachable-state pane. The behavior is also testable NOW,
with no live-agent source: a ghost pin — an id that resolves to no fixture
agent, e.g. `"acc-ghost"`, already used exactly this way in the shipped suite
(`store.test.ts:521-522`: "Both resolve to visible agents (so they surface in
rightTabGroups()); \"acc-ghost\" resolves to none") — exercises every arm of
the new contract.

## What this amendment supersedes

Both superseded clauses live in the frozen
`docs/designs/product/compass-sidebar-pins/design.md` (Status: Active,
Tracker RIG-1632, ledger DL-096). Everything else in that record — the
configurable pin layer, Supervisor/Warden removal, empty default set,
per-workspace persistence, boot-on-first-resolvable-pin, the tree affordance
— stands unchanged.

### §T2 — the unresolvable-pin filter (frozen design.md:311-315)

Old (quoted):

> The derivation is the
> layer that filters unresolvable pins: `rightTabGroups()` emits a
> fleet item (`group: "fleet"`, `agentId` set, `id` the
> `agent:`-prefixed tab id) only for pins that resolve to a visible
> agent; a retained-but-unresolvable pin produces no activity-bar item
> and no pane while unresolvable (T3 keeps it in the persisted set).

New: `rightTabGroups()` emits a fleet item for EVERY pin, in pin order;
items whose pin does not resolve to a visible agent are marked unreachable so
the activity bar and the pane render the unreachable state. T3's
retained-in-the-persisted-set behavior is unchanged.

### §T2 — the pane-arm resolvability gate (frozen design.md:320-330)

Old (quoted, the gating clause):

> the pane
> `<Switch>`'s hardcoded `"supervisor"`/`"warden"` `<Match>` arms are
> replaced by a single arm whose `when` is a *resolvability* test on
> `activeRightTab()`: the active tab id is an `agent:`-prefixed pin **and**
> it resolves to a visible agent. […]
> Gating on resolvability (not the bare `agent:`-prefix) makes the "no
> pane while unresolvable" invariant hold for the *active* tab too: if the
> active tab's agent is unresolvable the arm does not fire and the pane
> falls through to `status`.

New: the arm gates on the bare `agent:` prefix. For a resolvable agent it
renders `FleetPane`; for an unresolvable one it renders the "agent
unreachable" pane. No fall-through to `status`; the Switch's no-default-arm
gap is closed defensively.

### §T3 — the fluctuation-coercion clause (frozen design.md:354-363)

Old (quoted):

> A pinned
> id that resolves to no visible agent is retained in the persisted set
> (visibility can fluctuate; the pin survives the agent coming back)
> while the T2 derivation filters it out — no activity-bar item, no pane.
> Unpinning the active tab falls back to `setActiveRightTab("status")`.
> The same fallback covers the symmetric transition the T2 arm gates on:
> when the *active* agent tab's pin becomes unresolvable through a
> visibility fluctuation (not an explicit unpin), the shell moves
> `activeRightTab` to `status` (`setActiveRightTab("status")`) — a
> live-active tab whose agent vanished never strands a pane, and the
> activity-bar selection agrees with the T2 Switch arm's resolvability gate.

New: the retained-in-the-persisted-set sentence stands (minus "filters it
out" — the derivation now emits a marked item). Unpin-active→status stands.
The fluctuation clause ("the same fallback covers the symmetric transition…")
is REMOVED: a fluctuation to unresolvable changes nothing about the active
tab — the pane shows the unreachable message and the pin stays until manually
unpinned.

## Global Constraints

Every Plan task below inherits these:

- No `any` / `as any`; use `Set`/`Map` for runtime collections.
- Red-first tests: each behavioral task lands its failing test before the
  implementation that turns it green.
- `moon run compass-ui:ci` and biome green at every task boundary.
- Every unreachable-pin STATE is testable NOW via a ghost pin (an id
  resolving to no fixture agent, e.g. `"acc-ghost"` per
  `store.test.ts:521-525`): the marked bar item, the unreachable pane, the
  retired coerce, and the unpin fallback all assert against a static pin set.
  The live→unreachable TRANSITION (a resolvable pin becoming unreachable
  mid-session) is NOT exercisable until the live-agents migration replaces
  the static `agents` const (`store.ts:602-603`) — no test can mutate the
  agent set today. State coverage is sufficient for this slice's contract;
  the transition path lands its test with the live migration.
- No AI tool, agent-product, or persona names in code or comments; describe
  behavior directly.
- Code comments AND interface docs citing the superseded contract are updated
  to cite this amendment where the behavior changed: the inline comments
  ("Record A §T2/§T3", e.g. `store.ts:662-663`, `store.ts:673-678`,
  `store.ts:1694-1697`, `RightSidebar.tsx:544-548`) AND the shipped `AppStore`
  interface docs that still assert the OLD contract — `store.ts:290-293`
  ("filtered out of `rightTabGroups()`"), `store.ts:301-304` ("A
  retained-but-unresolvable pin produces no item (and no pane)"), and
  `constants.ts:90-95` (`fleetItemForAgent`'s "Only called for pins that
  resolve to a visible agent, so `agentId` always badges a real StateDot" —
  no longer an `ActivityBarItem`-wide invariant once unreachable items carry
  an `agentId` with no agent).

## Plan

### P0 — pin set persists `{ id, handle }`, not bare ids (OQ-2)

The persisted pin set carries the handle so an unreachable pin can render the
human name the user pinned. Shipped shape is `readonly string[]` throughout
(`store.ts:659`, `:294`, `:570-585`, `:1673-1692`, `:1702`).

- Interfaces:
  - A `PinnedAgent` shape — `{ id: string; handle: string }` — replaces the
    bare `string` element. `pinnedAgentIds: Accessor<readonly string[]>`
    (`store.ts:294`) stays id-valued for its existing consumers (rename is
    out of scope); a sibling accessor `pinnedAgents: Accessor<readonly
    PinnedAgent[]>` exposes the pairs, and the internal signal
    (`store.ts:659-660`) holds `PinnedAgent[]` with `pinnedAgentIds` derived
    as `.map((p) => p.id)`.
  - `pinAgent(accountId: string)` (`store.ts:1677-1683`) resolves the handle
    at pin time via the P5 seam (`agentById(accountId)?.account.handle`,
    falling back to `accountId` if somehow unresolvable at pin time) and
    appends the `{ id, handle }` pair. `unpinAgent`/`isPinned`
    (`store.ts:1673`, `:1686-1693`) match on `id`.
  - `loadPinnedAgentIds`/`savePinnedAgentIds` (`store.ts:570-585`) become
    `loadPinnedAgents`/`savePinnedAgents` over `PinnedAgent[]`. **Legacy
    hydration:** a stored bare-`string[]` payload (pre-migration, or a
    hand-edited key) hydrates as `{ id, handle: id }` — no version flag, no
    migration write needed. It self-heals: a resolvable pin always renders its
    LIVE handle via `fleetItemForAgent` (P1), so the `handle: id` fallback only
    ever surfaces for a pin that is *already* unreachable — exactly today's
    degraded state, no regression. The parser discriminates per element: a
    `string` element hydrates to `{ id, handle: id }` (legacy), an object with
    `id`/`handle` hydrates as-is, anything else is dropped — and a non-array
    (`store.ts:577`) or unparseable payload yields the empty set, the element
    filter (`store.ts:578`) dropping any non-string legacy entry.
  - The `AppStore` interface doc for the pin accessors (`store.ts:290-294`) is
    rewritten to the new shape.

  This task lands first: P1's `unreachableFleetItem` and P5's seam both consume
  it.

### P1 — `rightTabGroups()` emits an item for every pin, marking unresolvable ones

The derivation stops filtering. Shipped code
(`compass/apps/ui/src/store.ts:1701-1705`):

```ts
const fleetItems: ActivityBarItem[] = [];
for (const id of pinnedAgentIds()) {
  const agent = agents.find((a) => a.account.id === id);
  if (agent) fleetItems.push(fleetItemForAgent(agent));
}
```

New: every pin contributes an item; an unresolvable pin's item is marked.

- Interfaces:
  - `compass/apps/ui/src/constants.ts:51-61` — `ActivityBarItem` gains an
    optional flag: `unreachable?: boolean` (absent/false = live). `id`,
    `icon`, `title`, `group`, `agentId` unchanged.
  - `compass/apps/ui/src/constants.ts:96-104` — `fleetItemForAgent(agent:
    Agent): ActivityBarItem` unchanged (resolvable pins). New sibling builder
    `unreachableFleetItem(pin: PinnedAgent)` (the `{ id, handle }` shape from
    P0) returns:

    ```ts
    unreachableFleetItem(pin: PinnedAgent): ActivityBarItem {
      return {
        id: `agent:${pin.id}`,
        icon: (pin.handle.at(0) ?? "?").toUpperCase(),
        title: pin.handle,
        group: "fleet",
        agentId: pin.id,
        unreachable: true,
      };
    }
    ```

    The cached handle (OQ-2, ruled: cache-at-pin-time) is the degraded label,
    so an unreachable pin shows the human name the user pinned, not an opaque
    id. A legacy `{ id, handle: id }` fallback pin (P0) degrades to the id,
    matching pre-migration behavior.
  - `compass/apps/ui/src/store.ts:1698-1711` — the loop iterates the
    `PinnedAgent` pairs (P0): resolvable → `fleetItemForAgent(agent)`, else →
    `unreachableFleetItem(pin)`. Resolution routes through the P5 seam
    (`agentById(pin.id)`), not an inline `agents.find`.
  - The doc comment `store.ts:1694-1697` ("An unresolvable pin contributes no
    item — no activity-bar entry, no pane.") is rewritten to this contract.

### P2 — the RightSidebar unreachable-pane arm

The pane `<Switch>`'s fleet arm fires for any `agent:` tab. Shipped code
(`compass/apps/ui/src/components/RightSidebar.tsx:549-554`):

```ts
const activeFleetItem = (): ActivityBarItem | undefined => {
  const active = store.activeRightTab();
  if (!active.startsWith("agent:")) return undefined;
  const agent = STUB_AGENTS.find((a) => a.account.id === active.slice(6));
  return agent ? fleetItemForAgent(agent) : undefined;
};
```

The fold reads the active item out of the P1 memo instead of rebuilding it —
reconciling the builder signature and collapsing the two construction paths
onto P1's single site:

```ts
const activeFleetItem = (): ActivityBarItem | undefined => {
  const active = store.activeRightTab();
  if (!active.startsWith("agent:")) return undefined;
  // The P1 memo already emits an item for every pin (marked or not) with the
  // cached-handle title; read it rather than resolving/rebuilding a second time.
  return store
    .rightTabGroups()
    .flatMap((g) => g.items)
    .find((i) => i.id === active);
};
```

- Interfaces:
  - `activeFleetItem(): ActivityBarItem | undefined` — for an
    `agent:`-prefixed active tab it returns the matching item from the P1
    `rightTabGroups()` memo, which already carries the `unreachable` flag and
    cached-handle title (resolvable or not). This is the SINGLE
    item-construction site: no second `fleetItemForAgent`/`unreachableFleetItem`
    call and no duplicated resolvable-vs-unreachable branch at the pane. Never
    `undefined` for a pinned `agent:` tab (that was the blank-pane gap);
    `undefined` only for a non-`agent:` tab or an `agent:` tab with no matching
    pin (which falls through to `status`).
  - The `<Match when={activeFleetItem()}>` arm
    (`RightSidebar.tsx:566-568`) renders `FleetPane` when
    `!item().unreachable`, else a presentational `AgentUnreachable` block
    styled like the existing empty-state copy (`term-empty`, cf.
    `RightSidebar.tsx:591`): a message ("this pinned agent is unreachable —
    dead, despawned, or filtered out") AND a WORKING unpin control — a button
    calling `store.unpinAgent(item().agentId!)`. The unpin control MUST live
    in this pane: the only shipped unpin affordance is the left-tree pin
    toggle (`AgentLeaf`, `LeftSidebar.tsx:57-79` → `store.unpinAgent`), and
    the tree renders from the VISIBLE set (`agentTree(STUB_AGENTS)`,
    `LeftSidebar.tsx:349`), so an unreachable pin has NO tree row and no
    reachable toggle. Without a pane control the ruling's sole exit ("removal
    is a manual unpin") does not exist in the UI — the pin would be permanent
    until the agent returns. `unpinAgent` (`store.ts:1686-1693`) already
    handles the active-tab→status fallback.
  - `FleetPane`'s existing unresolved-`agentId` fallback
    (`RightSidebar.tsx:427-431` docblock, "Fallback covers an unresolved
    agentId") becomes DEAD for the `agent:` arm — the arm now resolves
    reachability before choosing `FleetPane` vs the unreachable block, so
    `FleetPane` only ever renders a resolvable agent. Remove or absorb that
    fallback rather than leaving two unreachable-render paths.
  - The activity bar renders marked items dimmed/badged so the bar itself
    distinguishes an unreachable pin (visual treatment is the implementer's
    choice within the existing `StateDot`/dimming vocabulary).

### P3 — retire the set-time coerce-to-status; keep unpin-active→status

- Interfaces:
  - `compass/apps/ui/src/store.ts:679-683` — the guard body is removed;
    `setActiveRightTab(tab: RightSidebarTab)` becomes a plain
    `setActiveRightTabRaw(tab)` pass-through (keep the wrapper as the single
    public set seam; its doc comment `store.ts:673-678` is rewritten: an
    `agent:` tab no longer requires a visible agent — it may render the
    unreachable pane).
  - `compass/apps/ui/src/store.ts:1686-1693` — `unpinAgent` unchanged: line
    1692 `` if (activeRightTab() === `agent:${accountId}`)
    setActiveRightTab("status"); `` stays (the retained user-gesture
    fallback).
  - No `createEffect` fluctuation-watcher is added anywhere (per the ruling
    there is nothing to watch for).

### P4 — boot default: reconsidered, kept unchanged

Shipped code (`compass/apps/ui/src/store.ts:669-672`):

```ts
const firstResolvablePin = pinnedAgentIds().find(agentIsVisible);
const [activeRightTab, setActiveRightTabRaw] = createSignal<RightSidebarTab>(
  firstResolvablePin ? `agent:${firstResolvablePin}` : "status",
);
```

Decision: this stays UNCHANGED — boot prefers the first *resolvable* pin,
else `status`. The ruling's rationale is mid-view disorientation (an agent
vanishing out from under the user); at boot there is no mid-view state to
preserve, so landing on a live pane beats landing on an unreachable message.
An unreachable pin still shows its bar item at boot (P1) and can be selected
(P3). RULED by Matt (OQ-1, kept unchanged) — see §Resolved questions; no
DL-096 flip is owed.

- Interfaces: none (no code change); the doc comment `store.ts:666-668`
  drops its "matching the T2 derivation" clause (the derivation no longer
  filters).

### P5 — centralize agent-tab→agent resolution on one store seam (PR #98 reviewer low #3)

The `accountId → Agent` lookup is currently duplicated: `store.ts:664-665`
(`agentIsVisible` — `agents.some((a) => a.account.id === accountId)`),
`store.ts:1703` (`agents.find((a) => a.account.id === id)`),
`RightSidebar.tsx:552` (`STUB_AGENTS.find((a) => a.account.id ===
active.slice(6))`), and `RightSidebar.tsx:422-425` (`agentFor` —
`STUB_AGENTS.find((a) => a.account.id === item.agentId)`). The future
live-agents migration must flip ONE source.

- Interfaces:
  - New store accessor on `AppStore`: `agentById(accountId: string): Agent |
    undefined` — the single resolution seam, backed today by the store's
    `agents` seed (`store.ts:602-603`) and later by the live stream. It MUST
    be a REACTIVE read: the amendment's headline behavior is an item flipping
    live→unreachable when the agent set changes, and both `rightTabGroups`
    (`createMemo`, `store.ts:1698`) and `activeFleetItem` (render-time
    accessor) must re-run on that change. `agents` is a plain `const` today
    (`store.ts:602-603`), so a closure over it satisfies every P6 state test
    yet cannot flip — the live migration owes converting `agents` to a
    signal/store read that flows through this one seam (not a non-reactive
    snapshot).
  - `agentIsVisible` becomes `agentById(accountId) !== undefined`
    (or is inlined away); the P1 `rightTabGroups` loop is now the SOLE
    item-construction consumer of `agentById` — P2's `activeFleetItem` reads
    that memo's already-built items, inheriting the seam transitively rather
    than resolving again; `agentFor` (`RightSidebar.tsx:419-425`) delegates to
    `store.agentById(item.agentId)` (it moves into component scope or takes
    the store — it must stop importing `STUB_AGENTS` directly).
  - After this task, `STUB_AGENTS` has no remaining consumer in
    `RightSidebar.tsx`.

### P6 — red-first tests (ghost pin)

- Interfaces (test anchors; fixture ghost id `"acc-ghost"` per
  `store.test.ts:521-525`):
  - `compass/apps/ui/src/store.test.ts` — extend the `"agent pins (Record A
    §T2/T3/T5)"` suite (`store.test.ts:520`):
    1. a ghost pin KEEPS its bar item: `rightTabGroups()` fleet items include
       `agent:acc-ghost` with `unreachable === true`, in pin order, its
       `title` the cached handle (P0);
    2. `setActiveRightTab("agent:acc-ghost")` is NOT coerced —
       `activeRightTab()` stays `"agent:acc-ghost"`;
    3. `unpinAgent("acc-ghost")` removes the item AND falls active→status
       when the ghost tab was active;
    4. the pin set round-trips as `{ id, handle }` (P0): pinning a resolvable
       agent then reloading (`loadPinnedAgents`) preserves its handle, and a
       legacy bare-`string[]` payload hydrates as `{ id, handle: id }`.
  - `compass/apps/ui/src/components/RightSidebar.test.ts` — in the
    `rightTabGroups()` derivation suite, the old-contract test
    `an unresolvable pin yields no fleet item` (`RightSidebar.test.ts:175-187`),
    which asserts the ghost id is absent and the fleet ids are exactly the
    resolvable pin + `status`, flips: an unresolvable pin now surfaces a marked
    item. The sibling invariant `fleet pin items carry a resolving agentId`
    (`RightSidebar.test.ts:192-232`) also relaxes — an unreachable item carries
    an `agentId` that resolves NO stub, so its "every fleet-with-agentId item
    resolves a real stub" assertion must exclude marked items.
  - A render test (alongside `RightSidebar.fleetpane.test.tsx`) asserting the
    pane for an active ghost pin renders the "agent unreachable" message, not
    `FleetPane` and not `StatusPane`.
  - The shipped tests asserting the OLD contract flip to the new contract in
    the same slice: the coerce test "an unresolvable active tab falls back to
    status" (`store.test.ts:643-650`) is REWRITTEN to assert the tab stays
    `"agent:acc-ghost"` (P6 test 2), and any "no item for unresolvable pin"
    derivation assertions flip to expect a marked item.

## Tasks

- [ ] P0 — pin set persists `{ id, handle }` (OQ-2); legacy `string[]`
  hydrates as `{ id, handle: id }`; `pinAgent` resolves handle via `agentById`
- [ ] P1 — `rightTabGroups()` emits every pin; `unreachable` flag +
  `unreachableFleetItem` builder (cached-handle label)
- [ ] P2 — RightSidebar `agent:` arm renders FleetPane or the unreachable
  pane (message + working in-pane unpin control); no fall-through, no
  default-arm gap; remove FleetPane's now-dead unresolved-agentId fallback
- [ ] P3 — retire the `setActiveRightTab` resolvability guard; keep
  unpin-active→status; no fluctuation-watcher
- [ ] P4 — boot default confirmed unchanged (first-resolvable-else-status);
  comment updated
- [ ] P5 — `agentById` store seam; all four duplicated lookups route through
  it
- [ ] P6 — red-first ghost-pin tests: bar item kept, pane message + unpin
  control, no coerce (flip `store.test.ts:643-650`), unpin fallback

## Ledger delta

### DL-098 (append — new row, "UI shell" section, after DL-097)

`DL-09x` occupancy verified this run by grepping DECISIONS.md: DL-090..097
are taken (DL-096 at DECISIONS.md:152, DL-097 at :153), nothing holds DL-098
— it is the next free id. Proposed row (Decision cell immutable once
appended):

| ID | Decision | Status | Record |
| --- | --- | --- | --- |
| DL-098 | An unresolvable pinned agent does not vanish: the pin keeps its activity-bar item, its pane renders an "agent unreachable" state, and removal is a manual unpin only — a visibility fluctuation to unresolvable never auto-switches the active tab to `status` (the unpin-active→status fallback is retained, and the pane arm closes the Switch's no-default-arm gap) — amending DL-096's unresolvable-pin handling (the frozen record's §T2 filter and §T3 fluctuation-coercion clauses are superseded; DL-096's pinning-layer core stands) | Active (Matt, 2026-08-02) | [unreachable-pin amendment §Approach](compass-sidebar-pins-unreachable-amendment/design.md#approach) |

### DL-096 — stays Active

DL-096's core — pinning as a configurable presentation layer, Supervisor/
Warden removed, empty default, per-user `localStorage` persistence, boot onto
the first resolvable pin — is not reversed; only the unresolvable-pin
*handling* changes. Per the ledger's Conventions (DECISIONS.md:22-31: "A new
ruling is a new row plus a `Superseded` flip on the old" — a flip is owed
only when the new row supersedes the decision the old row states), and
mirroring the partial-amendment grammar of DL-072 ("amending RT-3's
'per-session' wording", DECISIONS.md:125) and DL-092 (amends #995's plan
without flipping DL-048/DL-049), **DL-096 stays `Active`** and DL-098's
Decision cell names what it amends.

### The frozen record's `Status:` header — stays as-is, untouched

Precedent: `compass-server-ownership-layer/design.md:3` still reads a bare
`Status: Active` with no amended-by note now that its sibling amendment
exists (verified this run — grepping that record for "amend" finds nothing);
the amendment's own header blockquote and the ledger row carry the
cross-reference. Mirroring that, `compass-sidebar-pins/design.md:3` stays
`Status: Active` and the frozen record is not edited at all. Discoverability
is the ledger's job: DL-096's row is one hop from DL-098's, and this record's
header names exactly which §§ it supersedes.

## Resolved questions (Matt, 2026-08-02)

Both were surfaced to Matt as load-bearing and ruled before this record
froze:

- **OQ-1 — boot default: KEEP UNCHANGED.** Boot prefers the first *resolvable*
  pin, else `status` (`store.ts:669-672`), unchanged — see P4. The rationale
  held: the ruling's basis is mid-view disorientation, and boot has no mid-view
  state to preserve. **Ledger consequence:** because the answer preserves what
  DL-096's immutable Decision cell states ("the shell boots onto the first pin
  that resolves to a visible agent, else Status", `DECISIONS.md:152`), no
  `Superseded` flip on DL-096 is owed — DL-098 stays a clean partial amendment
  (see §Ledger delta). (The opposite ruling would have contradicted DL-096's
  stated boot rule and owed a flip per the Conventions, `DECISIONS.md:22-31`;
  it does not.)
- **OQ-2 — unreachable pin's label: CACHE THE HANDLE AT PIN TIME.** The pin
  set persists `{ id, handle }` pairs, not bare ids, so an unreachable pin
  renders the human handle the user pinned rather than an opaque account id.
  This serves the ruling's rationale (the user must recognize which pin to
  drop) directly; the raw-id fallback would have rendered every unreachable
  pin as the identical glyph. This changes the persisted pin-set shape — its
  own plan task (P0) and interface deltas across P1/P5 below.
