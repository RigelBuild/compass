# Compass v0.7 — comms-in-workspace: channel chat inside the board-primary shell

Status: Historical

> Internal design record — July 2026. Supersedes exactly **one decision** of
> the frozen v0.6 record (`../compass-0.6/design.md`, merged): the **UI-shell
> layout of its §T7** ("UI: the communication layer as the primary surface").
> Unchanged — the comms contract (accounts, channels, membership, threads,
> asks, mentions), the agent-account model (`AgentAccount` as an owned subtype +
> `home_channel_id`, RT-2), and the T8 board projection
> `../compass-0.6/design.md:1585-1601`). A second, non-decision cleanup rides
> along — dropping the two vestigial `harness` fields (`AgentAccount.harness`
> and `CreateAgentRequest.harness`, T0), which merely executes v0.6's
> already-frozen no-BYOA/single-agent stance
> (`../compass-0.6/design.md:676-680`) to the wire. Shell/state lineage:
> `../compass-ade-shell/design.md` and `../compass-dock-in-sidebar/design.md`
> (both merged). This record is the contract PR #783 (branch
> `franklin-compass-channel-first-ui`) is reshaped against after this record's
> PR merges. Frozen records are superseded by citation only, never rewritten.

## Problem / Intent

PR #783 implemented v0.6 §T7 faithfully; 0.7 supersedes §T7's UI-shell layout
— the one 0.6 decision this record changes. §T7 is titled "UI: the
communication layer as the primary surface" and rules "the **channel is the
primary human↔agent surface** and the workspace is the **observation pane** …
observation-only, with a **stop** control and no message-composer"
(`../compass-0.6/design.md:1544,1546-1547,1557`). #783 realized that shell-level
decision concretely: the board became one half of a `Channels|Board` top-bar
swap (`apps/ui/src/App.tsx:16` — "The communication layer (channels
\+ DMs) is the primary surface"; `app.css:2648` — "the board is demoted to a
secondary projection") and the workspace's tab/split-pane/terminal machinery
was replaced by a fixed observation set (`store.ts:93` —
`export type ObservationPaneKind = "trace" | "terminal" | "file";`, doc'd "The
set is FIXED — no split-tree, no tabs", `store.ts:90-92`).

0.7 **supersedes §T7's UI-shell layout** — the one 0.6 decision this record
changes — replacing those swap/deletion mechanics as a consequence. It has two
facets:

1. **Board-primary shell.** The board returns as the top-level UI (topbar,
   left sidebar, `view()`-routed center, right sidebar); the `Channels|Board`
   swap is gone. §T7 gave *shell-level* primacy to the channel; 0.7 gives it
   back to the board. §T7's channel-primacy is not discarded — it is
   **re-homed one level down**: the board is the shell, and the channel is
   the primary surface *once you are inside an agent workspace*. The two
   compose at different levels of the hierarchy.
2. **Channel chat inside the workspace.** §T7 kept the channel and the
   workspace as separate surfaces (channel = conversation, workspace =
   observation-only, no composer). 0.7 merges them: the workspace's PRIMARY
   pane becomes the agent's channel chat (composer + threads + ask — replacing
   the pre-branch ACP pane), the execution trace/log is demoted to a
   minimizable observation companion beside it, and the pre-branch
   tab/split-pane/terminal machinery is restored around it.

Standalone channels (not tied to an agent workspace) move into a collapsible
**Channels** section of the left sidebar, above a collapsible **Agent
workspaces** section. The comms *model* v0.6 froze is untouched — the branch's
comms layer (`comms.ts`, `comms-stub.ts`, `ChannelView.tsx`) carries it
faithfully and is re-homed, not reverted. What changes is where that layer
mounts: inside the board-primary shell, not instead of it.

### Superseded decisions (by citation)

Modeled on 0.6's own section (`../compass-0.6/design.md:60`). Exactly one:

1. **0.6 §T7's UI-shell layout** (`../compass-0.6/design.md:1544-1567`):
   "channel is the primary human↔agent surface", workspace = observation pane
   with "no message-composer". Superseded by: the board-primary shell with
   the channel chat AS the workspace's primary pane (composer included) and
   the trace as a minimizable log companion. **Not superseded**: T7's comms
   consumption contract (SubscribeComms rendering, resync/dedup, async ask +
   RespondToAsk, steer via @-mention, OMP's renderer over the opaque session
   stream, stop control, home-channel-membership ACL) — all carried forward
   into the new mount points.

## Approach

### The shape, in one pass

Restore the `origin/main` board-primary shell and workspace machinery, then
re-home the branch's comms layer inside it:

- **Shell**: back to `origin/main:App.tsx`'s layout — topbar, left sidebar,
  `view()`-routed center (`<Switch fallback={<AgentView />}>` over
  `bridge`/`backlog`/`done`/`settings`), right sidebar, usage bar. The
  `Channels|Board` top-bar swap and the full-width `ChannelSidebar` rail are
  removed. One new view is added: `channel` — a standalone channel opened from
  the sidebar renders `ChannelView` in the center, board chrome intact.
- **Agent workspace**: the pre-branch `AgentTab`/`SplitNode` machinery is
  restored verbatim from `origin/main:store.ts` (types at lines 56-171, actions
  at 548-622: `openTab`, `closeTab`, `splitActivePane`, `setFocusedPane`,
  `closePane`). The only semantic change: the permanent first pane's kind is
  renamed `session` → `chat`, and its body renders the agent's home DM channel
  through `ChannelView` (threads + ask + composer) instead of
  `AcpConversation` (`origin/main:components/AgentView.tsx:198` — `export const
  AcpConversation: Component<{ agent: Agent }>`; `:318-320` — `<Match
  when={props.pane.kind === "session"}><AcpConversation agent={props.agent}
  />`).
- **Log panel**: the branch's OMP-native trace (`AgentView.tsx` `FrameRow` /
  `TracePane` over `SessionFrame`, `session-stub.ts:33-43`) is demoted from
  primary surface to a dedicated minimizable side panel docked at the
  workspace's right edge — outside the tab/split tree, with the running dot +
  Stop control in its header.
- **Left sidebar**: two collapsible sections — **Channels** (standalone
  channels: the non-DM group channels like announcements / coordination /
  svc.* / random, plus group DMs) above **Agent workspaces** (the agent
  roster/folder tree, restored from `origin/main:LeftSidebar.tsx`, which today
  renders `<div class="ws-section-label">Agent Workspaces</div>` +
  `STUB_TREE` non-collapsibly, branch `LeftSidebar.tsx:175-177`).
- **Identity**: one account-id space with separate co-addressed types (Decision
  D1 below) so a board agent always resolves its home channel off its account.

### Decisions (ruled by Matt — recorded, not reopened)

**D1 — Identity: separate, co-addressed UI types (one id space, composed at the
seam) — fixture conformance to 0.6's model, not a contract change.** The frozen
0.6 contract models an agent as one `AgentAccount` carrying an additive
`home_channel_id` ("the agent's named channel/DM, minted at `CreateAgent`",
`../compass-0.6/design.md:222-223,1760-1764`, RT-2 — ratified, not yet on the
wire), with its live lifecycle a projection of `AgentSessionStatus` over
`SubscribeEvents` (`../compass-0.6/design.md:1595-1596`). The UI's two disjoint
id spaces are a **fixture artifact** that predates that model: comms accounts
(`comms-stub.ts:192` `STUB_ACCOUNTS` — `acc-matt` + agent accounts
`acc-mercator/compass/cook/xenophon/franklin`) vs the board roster
(`stub-data.ts:303+` — ten `agent-*` ids: supervisor/warden/cook/livingstone/
cousteau/ross/shackleton/erikson/drake/magellan). Only `cook` overlaps by
handle, and the store itself documents the split (`store.ts:378-379` — "the
observed agent is the channel's other party, NOT the board's `selectedAgentId`,
which is a separate `agent-*` roster concept"). Under this design the
workspace's primary pane is the agent's channel chat, so every board agent MUST
resolve a home channel — today 0 of the 10 do (the only 1:1 DMs are compass's
and franklin's, `comms-stub.ts:307-321`, and neither is a board agent; `cook`
overlaps by handle but has only a group DM).

D1 collapses the two id spaces onto the ONE account id space (`acc-<handle>`)
but keeps **separate types** for what the Server streams separately (next
section): a durable `Account` (comms) and an ephemeral `AgentLifecycle` enum
(`SubscribeEvents`), composed at read by the store's `agentView(id)` into the
`Agent` view-model — the id is the join key, so no bridge field and no second id
space. The account arm carries handle, displayName, ownerUserId, and (fork 3)
an additive `homeChannelId` mirroring RT-2 — so the chat pane reads the home
channel O(1) off the account, not via a per-render `agentDmChannel` search. In
the fixture era T1 caches that id (equal to `agentDmChannel(...)`); when the
client consumes the real contract the field is fed by the account's
`home_channel_id` — same field, new source (the store seam; the proto landing of
`home_channel_id` is the comms-server lane, SEA-1195). Humans stay a `user`-kind
`Account`. Rationale: the fixtures conform to the contract they claim to mirror;
separate co-addressed types keep the durable/ephemeral split honest (a
created-but-unstarted agent has no lifecycle) without a bridge field the
contract never had.

**D2 — Log pane: a dedicated minimizable side panel, not a tab-pane.** The raw
agent output (OMP-native `SessionFrame` stream, opaque to Compass —
`session-stub.ts:14-16`: "SEAM: the exact session-frame envelope is OMP's, not
Compass's contract") is a *companion* you glance at while chatting, not a
destination you navigate to. Making it a tab would hide it behind a click;
making it a split-pane leaf would let users close it irrecoverably. A fixed
side panel with a minimize toggle keeps it one keystroke away and structurally
distinct from the user-managed tab/split tree, which stays reserved for
terminals and files.

**D3 — Ask: kept in the 1:1 agent-workspace chat; deferred only in standalone
multiplayer channels.** The workspace chat is single-player (the owner
answering their agent) — semantically the old ACP ask, so the branch's
`AskBlock` (`ChannelView.tsx:84-121`, `store.answerAsk`, `store.ts:491`)
renders live there. Standalone shared channels are multiplayer; who may answer
an ask there is an unsolved design (attribution, races, revocation), so v1
renders text + threads only — an ask block in a standalone channel shows
read-only (options disabled, pointing at the owning workspace).

### The three streams behind an agent, and the store seam

D1 keeps the types separate because the Server streams three distinct things
about an agent, from two services and different stores, on different
lifecycles — and only ONE of them is a field on the agent object:

1. **`Account`** (durable) — `CommsService.SubscribeComms`, Postgres store of
   record. Identity: handle, displayName, ownerUserId, and (agent kind) the
   `home_channel_id` (RT-2). Exists from `CreateAgent` onward, independent of
   any session.
2. **lifecycle `state`** (ephemeral) — `CompassService.SubscribeEvents`
   `AgentSessionStatus.state` (`compass.proto:126-129,135-142`), in-memory bus.
   The `STARTING/READY/WORKING/STOPPED/ERRORED` enum that drives `StateDot` and
   the board. Exists only while a session runs. **This is the only thing
   `SubscribeEvents` contributes to the agent object.**
3. **the OMP session trace** (ephemeral, **opaque**) — the session-tail stream
   of OMP-native `SessionFrame`s (`session-stub.ts:14-16`: "the exact
   session-frame envelope is OMP's, not Compass's contract"). Compass does not
   interpret it; it hands frames to OMP's own renderer. This is "the OMP session
   streaming to the OMP UI" — and it is **already a separate type today**
   (`AgentSession`, read via `store.agentSession()`, keyed by account id), never
   part of the agent object.

So there is no fat "session projection" to merge — the ephemeral arm is just a
lifecycle enum (source 2) plus an opaque trace that was never merged (source 3).
A single merged object would fuse the durable source 1 with the ephemeral source
2 and imply every agent always has a session. D1 keeps them separate:

- **`Account`** and **`AgentLifecycle`** are distinct, both keyed by the ONE
  account id (the id IS the join key — no bridge field). The store's
  `agentView(id)` composes them into the `Agent` view-model at read; `lifecycle`
  is optional, so a created-but-unstarted agent is representable.
- The `Agent` view-model is **assembled at the store seam — never a wire shape**
  (`store.ts:10-13`: "the accessors below … swap the fixture for the generated
  @compass/client — the AppStore contract is the seam").
- Today: `agentView` reads co-addressed fixture arrays. Later: it becomes a memo
  joining the `SubscribeComms` accounts with the `SubscribeEvents` lifecycle by
  account id — the pure `joinAgents(accounts, lifecycles)` function beside the
  store, unit-testable like `comms.ts`. The lifecycle's own wire key is
  `session_id`, not the account; `session_id → account` is resolved client-side
  via the `session_id ↔ container_name` binding from `StartAgentSession`
  (`compass.proto:197-209`) — the one open seam, not a new proto field.

**Dropped, no wire source (fork 2 + the v0.6 stream trim):** the old merged type
carried `harness` and a `feed: AgentEvent[]`. `harness` is gone (fork 2 — single
first-party agent). `feed` was "derived from `AgentMessageChunk`/`AgentToolCall`/
`AgentPlan`", but v0.6 **removed those three variants from `SubscribeEvents`**
(`../compass-0.6/design.md:531-535` — "neither the native render format nor
needed" under the first-party agent), so the RightSidebar activity feed has no
real-model source and is **dropped from v0.7** entirely. `role`/`model`/`cwd` stay
UI-only roster config (a later additive `SubscribeEvents` board variant, 0.6 T8);
`terminals` is fixture-only (no terminal stream in the MVP).

### Alternatives considered

- **A single merged `Agent` object** (one interface fusing the account + a
  materialized session projection): rejected. It implies every agent always has
  a session (false for a created-but-unstarted agent), and fuses a durable
  comms row with an ephemeral bus projection into one mutable shape. D1's
  separate co-addressed types keep the optionality honest.
- **A2 — two UI types + a bridge field** (`Agent.accountId` pointing into a
  *separate* `agent-*` roster id space): rejected by Matt. Separation is right;
  the bridge field is not. A2 keeps a fixture id space the contract doesn't
  have and a two-step lookup on every render path. D1 separates the types but
  co-addresses them on the ONE account id — no bridge field, no second id space.
- **Log as a pane kind in the split tree** (`PaneKind = "chat" | "log" |
  "terminal" | "file"`): rejected (D2). It would let the log be closed/split
  like any leaf; the log is a fixed companion with different lifecycle
  (minimize, never close) and different data source (opaque OMP frames vs
  user-openable resources).
- **Full revert of #783 then re-add comms**: rejected. The comms layer
  (`comms.ts` pure core + 774-line `comms.test.ts`, `comms-stub.ts`,
  `ChannelView.tsx`) is exactly what the corrected design needs; the reshape
  keeps it and moves its mount points.

## Global Constraints

Every task below inherits these; task briefs do not restate them.

- **Stack: SolidJS + Vite**, UI at `apps/ui/src/`. No new
  framework or state library; all cross-component state lives in the one
  `AppStore` (`store.ts`) read through context (`context.ts`).
- **Walking skeleton, no daemon.** Every surface renders from in-memory
  fixtures; the store accessors are the seam that later swaps to the
  generated `@compass/client` (`store.ts:10-13`). No task may read a fixture
  module directly from a component when a store accessor exists.
- **The comms model is 0.6's, frozen.** `comms.ts` / `comms-stub.ts` mirror
  the `compass.v1` comms contract (`comms-stub.ts:10-14`); tasks re-mount
  them, never re-derive or fork their shapes. The v0.6 seam annotations
  (membership carrier, `parentMessageId`, channel-only container) stay.
- **Tests: `moon run compass-ui:test` = `bun test --conditions browser`** —
  the browser condition is load-bearing (Bun's default `node` condition pulls
  solid-js's SSR build where `createMemo` is inert; `apps/ui/moon.yml:24-31`).
  Red→green per `rule://red-green-testing`: BDD/unit tests first, watch them
  fail, then implement.
- **Lint/format: biome** (repo-standard for TS). Markdown records are
  markdownlint-clean.
- **Stop/Send render enabled** over documented no-op RPC stubs (Matt's prior
  ruling; walking-skeleton fidelity — the control's enablement mirrors the
  real contract, the body is a stub until `StopAgentSession`/`PostMessage`
  land).
- **Frozen-record convention**: this record freezes on merge; later changes
  supersede by citation, never rewrite.

## Plan

Sequencing principle: **identity first** (T1 unblocks every workspace join),
then the store restoration (T2 — the machinery every surface mounts on), then
the surfaces (T3-T6), then the shell sweep + fixture/test reconciliation
(T7-T8). Each task carries its own test cycle and is a reviewable unit;
together they are the #783 reshape execution plan.

### T1 — Agent identity: separate co-addressed types (fixture conformance to 0.6)

Collapse the two id spaces onto one account-id space (`acc-<handle>`) and
reconcile the comms fixture onto it. **Surviving roster** = the ten board
agents (`stub-data.ts:303+`: supervisor/warden/cook/livingstone/cousteau/
ross/shackleton/erikson/drake/magellan), re-keyed to `acc-<handle>` and carrying
an agent-kind `Account`, plus `matt` (user). The four comms-only
agent accounts — `acc-mercator`, `acc-compass`, `acc-xenophon`, `acc-franklin`
(`comms-stub.ts:200-239`; `cook` already overlaps) — do NOT survive as
separate identities; the fixture content they author is re-homed onto the
surviving roster. Concretely T1 owns:

- **`STUB_AGENTS`** is the roster source of truth (the ten, re-keyed, each an
  `Agent` = `{account, lifecycle?, role, model, cwd, terminals}`). `STUB_ACCOUNTS`
  is derived — `[MATT_ACCOUNT, ...STUB_AGENTS.map((a) => a.account)]`.
- **Home DM per agent**: one `dm-<handle>` channel per board agent added to
  `STUB_CHANNELS`, its id cached on `account.homeChannelId` so it resolves for
  10/10 (today 0/10, above) without a per-render `agentDmChannel` search.
- **`STUB_SESSIONS` re-keyed** onto board agents (today keyed `acc-franklin`/
  `acc-compass`, `session-stub.ts:60,97` — neither survives, so without a
  re-key the T4 log panel would have nothing to show): the running trace → one
  board agent, the idle trace → another.
- **`STUB_MESSAGES` authors + `STUB_CHANNELS` memberships + the group-DM
  members** (`comms-stub.ts:341-461,282-329`) re-authored onto surviving ids,
  so no `acc-mercator/compass/xenophon/franklin` reference dangles.
- **The dead `AcpMessage`/`AcpBlock` conversation arm** (`stub-data.ts:179-210`)
  — both the `conversation` field AND its fixture data — is removed outright
  here (T8 keeps only the now-orphaned type-alias deletion + grep gate). The
  chat pane reads comms messages, not ACP fixtures; the branch already
  stripped `conversation` rendering from the board (`RightSidebar.tsx:371-373`
  — "The agent conversation moved to the channel surface").
- **The pre-existing 1:1 DMs `dm-compass`/`dm-franklin`**
  (`comms-stub.ts:307-321`, members `[matt, acc-compass]` / `[matt,
  acc-franklin]`) — whose non-`matt` party does not survive — are dropped;
  the surviving roster's 1:1 DMs are the new `dm-<handle>` set above.
- **Workstream `assignee`** values move to the new ids.

The surviving-roster composition is a walking-skeleton **fixture** choice
(which demo agents populate the board), not a contract matter — surfaced here
for the record review; D1's one-id-space model is what's ruled.

`Interfaces:`

- Produces (in `stub-data.ts`, replacing the current `Agent` at
  `stub-data.ts:221`):

  ```ts
  /** Durable comms identity (SubscribeComms · Postgres). The agent-kind arm
   *  gains an additive homeChannelId mirroring ratified 0.6 RT-2
   *  (`../compass-0.6/design.md:1760-1764`); the proto landing of
   *  `home_channel_id` on AgentAccount is the comms-server lane (SEA-1195). */
  export interface Account {
    id: string;                 // account id, e.g. "acc-cook" — the one id space
    handle: string;             // unique, e.g. "cook"
    displayName: string;
    kind: "user" | "agent";
    ownerUserId?: string;       // agent kind: owning user's account id
    homeChannelId?: string;     // agent kind: the agent's home DM (RT-2)
  }                             // NOTE: no `harness` — dropped (fork 2)

  /** The agent's ephemeral lifecycle — SubscribeEvents.AgentSessionStatus.state
   *  (`compass.proto:126-129`), keyed by account id. Absent = created but no
   *  session has run. This is the ONLY agent-object field SubscribeEvents feeds. */
  type AgentLifecycle = AgentState;

  /** The composed roster view-model the store assembles at the seam — NEVER a
   *  wire shape. `account` is durable; `lifecycle` is optional (honest for an
   *  unstarted agent); role/model/cwd are UI-only roster config (carried later
   *  by an additive SubscribeEvents board variant, 0.6 T8), terminals is pure
   *  fixture (no terminal stream in the MVP). The opaque OMP session trace is
   *  NOT here — it is a separate type (`AgentSession`, session-stub.ts) read by
   *  account id via `store.agentSession()`, handed to OMP's own renderer. */
  export interface Agent {
    account: Account;
    lifecycle?: AgentLifecycle;
    role: AgentRole;            // UI-only roster config
    model: string;             // UI-only (the model the OMP SDK is set with)
    cwd: string;               // UI-only
    terminals: Terminal[];     // fixture-only
  }
  export const STUB_AGENTS: Agent[];
  ```

- Produces (in `comms-stub.ts`): `STUB_ACCOUNTS` derived —
  `export const STUB_ACCOUNTS: Account[] = [MATT_ACCOUNT,
  ...STUB_AGENTS.map((a) => a.account)]`; one `dm-<handle>` channel per agent
  added to `STUB_CHANNELS`, its id stored back on the agent's
  `account.homeChannelId` so the chat pane reads it O(1) (no per-render
  `agentDmChannel` search). `agentView(id): Agent | undefined` on the store
  composes account + optional lifecycle by shared account id — the pure
  seam function (`joinAgents` in the real era) that stays a UI concern.
- Consumes: `Account` (`comms-stub.ts:41`), `agentDmChannel`
  (`comms.ts:126-133`) — unchanged.
- Callers to migrate: `store.ts` (`agents`, `selectedAgent`, `agentRepos`,
  `observedAgentId` unification, the `agentView(id)` composition); `constants.ts:87,94`
  (hard-coded `agent-supervisor`/`agent-warden` in `RIGHT_SIDEBAR_TAB_BY_ID` — else
  `RightSidebar.test.ts:173` "every fleet agentId resolves a real stub agent").
- Callers **deleted, not migrated** (fork 2, harness drop): `AgentKind` +
  `KIND_LABEL` (`constants.ts:5,42`) and every `harness`/`kind` render site —
  the `kind-tag` pip (`LeftSidebar.tsx:47`, import at `:3`), `Bridge.tsx:113`,
  `RightSidebar.tsx:383`. The `board.test.ts:59,64` `kind`/`conversation: []`
  fixture fields go with them (the latter under the ACP conversation-removal
  above). `LeftSidebar.tsx:39-46`'s `sharedWith` share pip is likewise removed
  (0.6 abolished the share model — `AgentWorkspace.participant_user_ids` +
  Share/Unshare RPCs removed, `../compass-0.6/design.md:1732-1734`; workspace
  access IS home-channel membership, RT-2). Also: `LeftSidebar.tsx` `STUB_TREE`
  agent ids; `session-stub.ts` `STUB_SESSIONS` keys (re-keyed onto board agents,
  above); board components reading `assignee`.

Test cycle (red first): unit tests asserting every agent's `account.homeChannelId`
resolves a real DM channel (and equals `agentDmChannel(...)` — the id is cached,
not stale); `agentView(id)` composes account + lifecycle for each of the ten and
returns undefined for an unknown id; a fixture agent with no `STUB_SESSIONS`
entry yields `lifecycle` present but `agentSession()` empty (honest optionality);
`STUB_ACCOUNTS` contains exactly one account per agent + the caller; no `agent-*`
id and no `harness`/`kind` field remains in any fixture; every `STUB_SESSIONS`
key resolves to a surviving agent id (the log panel has content) and no message
author / channel member / group-DM member references a dropped `acc-*` id; every
workstream `assignee` resolves to a surviving agent id.

### T2 — Restore the tab/split-pane store machinery, `session` → `chat`

Restore from `origin/main:store.ts` the pane/tab/split types (lines 56-171)
and actions (548-622) that #783 deleted, with one rename: the permanent
first pane kind is `chat` (was `session`), and `SESSION_TAB_ID` becomes
`CHAT_TAB_ID = "chat"`. Remove `ObservationPaneKind` /
`DEFAULT_OBSERVATION_PANE` / `activeObservationPane` /
`setActiveObservationPane` (`store.ts:93-97,206-210`). Add the log-panel
minimize state (D2) and the two sidebar-section collapse states (reusing the
existing `collapsed` set mechanism, `store.ts` `isFolderCollapsed`/
`toggleFolder`). `openAgent(agentId)` (account id now) resets tabs to the
chat tab, keyed on the workspace-init guard the branch already has
(`agentViewAgentId`, `store.ts:320-323`); it also selects the agent's home
DM channel so the chat pane and `selectedChannel` agree. In the fixture era
T1 guarantees a home DM for all 10 agents, so this resolves synchronously; for
the real-daemon era, when `agentDmChannel` finds no channel yet (account arm
ahead of its home-channel projection), `openAgent` sets `selectedChannelId`
to a connecting/empty state rather than leaving the prior selection stale —
the store seam owns this partial-join policy in one place (D1's composition
seam). `view()` keeps `"channel"` for standalone channels but the default view
becomes `"bridge"`.

`Interfaces:`

- Produces (restored verbatim from `origin/main:store.ts:56-171` modulo the
  rename):

  ```ts
  export type PaneKind = "chat" | "terminal" | "file";
  export interface Pane {
    id: string;
    kind: PaneKind;
    title: string;
    terminalId?: string;
    filePath?: string;
  }
  export type SplitNode =
    | { kind: "leaf"; pane: Pane }
    | { kind: "split"; direction: "row" | "column";
        left: SplitNode; right: SplitNode };
  export interface AgentTab {
    id: string;
    title: string;
    layout: SplitNode;
    focusedPaneId: string;
  }
  export const CHAT_TAB_ID = "chat";
  export function splitPaneIds(node: SplitNode): string[];
  export function splitPanes(node: SplitNode): Pane[];
  export function splitPaneOnce(
    node: SplitNode, targetPaneId: string, newPane: Pane,
    direction: "row" | "column",
  ): [SplitNode, boolean];
  ```

- Produces (on `AppStore`):

  ```ts
  // tabs + splits (restored, origin/main:store.ts:245-270)
  agentTabs: Accessor<AgentTab[]>;
  activeAgentTabId: Accessor<string | null>;
  activeAgentTab: Accessor<AgentTab | undefined>;
  setActiveAgentTab: (tabId: string) => void;
  openTab: (pane: Pane) => void;
  closeTab: (tabId: string) => void;
  splitActivePane: (pane: Pane, direction: "row" | "column") => void;
  setFocusedPane: (paneId: string) => void;
  closePane: (paneId: string) => void;
  // log panel (new, D2)
  logOpen: Accessor<boolean>;        // default true; per-workspace-entry reset
  toggleLog: () => void;
  // sidebar sections (new)
  isSectionCollapsed: (section: "channels" | "agents") => boolean;
  toggleSection: (section: "channels" | "agents") => void;
  ```

- Removes from `AppStore`: `activeObservationPane`,
  `setActiveObservationPane`, `observedAgentId` (unified with
  `selectedAgentId` — one id space after T1 makes the distinction moot;
  `agentSession` re-keys off `selectedAgentId`); `selectChannel`
  (`store.ts:445`) and `showChannel` (`store.ts:438`), whose routing is
  subsumed by T5's `openChannel`. `selectChannel`'s body resets the
  observation pane via `setActiveObservationPaneSignal(DEFAULT_OBSERVATION_PANE)`
  (`store.ts:453-455`); that reset is deleted alongside `DEFAULT_OBSERVATION_PANE`
  here, so no accessor references the removed symbol mid-plan. Sole non-test
  callers go dead by plan: `ChannelSidebar.tsx:45` (T5 deletes the file) and
  `App.tsx:58`'s `showChannel()` tab (T7 removes the `Channels|Board` swap).
- Consumes: T1's `Agent`/ids; `STUB_SESSIONS` (`session-stub.ts`) for
  `agentSession`; `agentDmChannel` for the home-channel select in
  `openAgent`.

Test cycle (red first): restore the deleted `origin/main:store.test.ts`
tab/split suites (openTab dedup, closeTab session-guard → chat-guard,
splitActivePane focus chaining, closePane collapse + focus fallback) against
the restored surface; **delete the branch's observation-pane + `selectChannel`
suites** (`store.test.ts:161-260` openAgent pane-reset asserts, `:644-728`
selectChannel, `:730-778` observation pane) and replace their coverage with
log-panel (`toggleLog`) + section-collapse tests, so T2 lands green rather
than leaving a red suite for T8; new tests — `openAgent` resets tabs to the
chat tab and selects the home DM; `toggleLog` flips; re-opening the same agent
preserves tabs (init-guard).

### T3 — Agent workspace: chat-primary pane + tab/split rendering

Rebuild `AgentView.tsx` on the restored machinery: the tab strip
(chat tab permanent, + terminal button), each tab rendering its `SplitNode`
tree recursively (restore `SplitView`/`PaneView` from
`origin/main:components/AgentView.tsx:339,256`), with the `chat` pane body
rendering the agent's home DM channel through `ChannelView` (composer +
threads + ask — D3: ask fully interactive here) instead of the deleted
`AcpConversation`. Terminal panes restore `TerminalBody` (fake scrollback
from `Agent.terminals`); file panes restore `FileViewer`. The `+` terminal
affordance restores `nextFreeTerminalPane`
(`origin/main:AgentView.tsx:376-379`).

`Interfaces:`

- Produces (components in `AgentView.tsx`):

  ```ts
  export const AgentView: Component;                  // the workspace shell
  const SplitView: Component<{ node: SplitNode; agent: Agent;
    focusedPaneId: string }>;
  const PaneView: Component<{ pane: Pane; agent: Agent; focused: boolean }>;
  const ChatPane: Component<{ agent: Agent }>;        // chat-kind pane body
  const nextFreeTerminalPane: (agent: Agent, tabs: AgentTab[])
    => Pane | undefined;
  ```

- `ChatPane` feed: `ChannelView` already renders from
  `store.selectedChannel()` + `threadsOf(store.messages(), channel.id)`
  (`ChannelView.tsx:275-278`); `openAgent` (T2) guarantees
  `selectedChannelId` is the agent's home DM, so `ChatPane` mounts
  `<ChannelView />` unmodified. Consumes `threadsOf(messages, channelId):
  Thread[]` (`comms.ts:182`), `agentDmChannel` (`comms.ts:126`),
  `store.answerAsk(messageId, askId, optionId)`.
- Consumes: T2 store surface (`agentTabs`, `activeAgentTab`, `openTab`,
  `splitActivePane`, `setFocusedPane`, `closePane`, `setActiveAgentTab`).

Test cycle (red first): restore + adapt the deleted
`origin/main:components/AgentView.test.ts` suite; assert the chat tab is
permanent (closeTab no-op), a terminal opens as a tab and as a split, and the
chat pane renders the home DM's threads (fixture message visible) with a
working ask (answer records via `answerAsk`).

### T4 — The log side panel (minimizable observation companion)

A new `LogPanel.tsx`: a fixed-width companion docked at the workspace's
right edge, outside the tab/split tree — header (agent handle, running dot,
Stop control, minimize toggle), body = the OMP-native trace (move
`FrameRow` and `TracePane` out of the branch's `AgentView.tsx:43-92`).
Minimized, it
collapses to a slim vertical rail with an expand affordance and the running
dot still visible (liveness at a glance). Stop stays enabled while
`running`, over the documented no-op `stopAgent` (`store.ts:214` — "A no-op
stub until the daemon's StopAgentSession lands").

`Interfaces:`

- Produces:

  ```ts
  export const LogPanel: Component<{ agent: Agent }>;
  const FrameRow: Component<{ frame: SessionFrame }>;   // moved, unchanged
  ```

- Consumes: `store.agentSession(): AgentSession | undefined`
  (`SessionFrame`/`AgentSession`, `session-stub.ts:33-56`), `store.logOpen()`
  / `store.toggleLog()` (T2), `store.stopAgent()`.
- Mounted by `AgentView` beside the tab area (CSS grid column; the split
  tree never contains it).

Test cycle (red first): renders frames for an agent with a session; empty
state for one without; minimize hides the body but keeps the running dot;
Stop disabled when idle, enabled when running.

### T5 — Left sidebar: two collapsible sections

Extend `LeftSidebar.tsx`: keep the Bridge/Backlog/Done/Settings links, then
a collapsible **Channels** section (standalone channels: non-DM group
channels + group DMs — the caller's member channels via
`railChannels`/`channelSections`, browse/join via `browsableChannels`,
moved from `ChannelSidebar.tsx`) ABOVE a collapsible **Agent workspaces**
section (the existing folder tree, `STUB_TREE` + `AgentLeaf`,
`LeftSidebar.tsx:16-52,175-177`). Clicking a channel calls a new
`openChannel(channelId)` → `view() === "channel"` with `ChannelView` in the
center (T6). Clicking an agent calls `openAgent` → the workspace. 1:1 agent
DMs do NOT list under Channels — the agent workspace is their surface; group
DMs do. `ChannelSidebar.tsx` is deleted.

`Interfaces:`

- Produces (in `LeftSidebar.tsx`):

  ```ts
  export const LeftSidebar: Component;
  const ChannelsSection: Component;      // rail rows + unread + browse/join
  const AgentsSection: Component;        // folder tree (existing Node/AgentLeaf)
  ```

- Produces (on `AppStore`): `openChannel: (channelId: string) => void` —
  selects the channel and sets `view` to `"channel"`; an agent-DM id routes
  to `openAgent` instead (one entry point, no dead-end DM view).
- Consumes: `channelSections(channels, groups): ChannelSection[]`,
  `railChannels`, `browsableChannels`, `dmChannels`, `isDm`, `totalUnread`
  (`comms.ts:42-133,211-213`); `store.joinChannel` / `store.toggleSubscribe`;
  T2's `isSectionCollapsed`/`toggleSection`.

Test cycle (red first): both sections collapse/expand independently; the
Channels section lists exactly the standalone set (no 1:1 agent DMs); a
channel click routes to `"channel"`, an agent-DM id routes to the workspace;
join/subscribe still mutate through the store.

### T6 — Standalone channel view (Slack/Discord style, no ask)

Mount `ChannelView` as the center surface for `view() === "channel"`, with
D3 enforced: in a standalone (non-agent-DM) channel, ask blocks render
**read-only** — options disabled, with a hint pointing at the owning agent's
workspace. `ChannelView` gains one prop; the interactive path (agent
workspace chat pane) passes nothing and behaves as today.

`Interfaces:`

- Changes:

  ```ts
  export const ChannelView: Component<{ readonlyAsks?: boolean }>;
  // AskBlock gains: disabled?: boolean — renders options inert + the
  // "answer in @<agent>'s workspace" hint when set.
  ```

- `App.tsx` mount: `<Match when={store.view() === "channel"}><main
  class="main"><ChannelView readonlyAsks /></main></Match>` inside the
  board shell (left sidebar + right sidebar stay).
- Consumes: T5's `openChannel` routing; `threadsOf`; `Composer` (unchanged —
  Send enabled over the documented no-op).

Test cycle (red first): an ask in a standalone channel renders disabled and
does not mutate on click; the same message in the workspace chat pane stays
answerable; threads render identically in both mounts.

### T7 — Shell restoration (board-primary App)

Restore `App.tsx` to the `origin/main` layout: single view-tab strip
(Bridge + the selected agent's tab, `origin/main:App.tsx` `view-tabs` nav),
left sidebar always available (toggle), center `<Switch
fallback={<AgentView />}>` over `bridge`/`backlog`/`done`/`settings` plus
the new `channel` match (T6), right sidebar, usage bar. Remove the
`Channels|Board` swap and `onChannelSurface`. Default view: `bridge`.
`RightSidebar`'s `FleetPane` placeholder ("conversation lives on the
Channels surface now", `RightSidebar.tsx:371-384`) re-points to the agent
workspace (opens the agent via `openAgent`).

`Interfaces:`

- `View` stays `"channel" | "agent" | "bridge" | "backlog" | "done" |
  "settings"` (`store.ts:56-62`); `createAppStore` boots with
  `createSignal<View>("bridge")`.
- Consumes: T2-T6 surfaces. Produces: the assembled shell.

Test cycle (red first): store-level routing tests — boot lands on `bridge`;
`openAgent` → `agent`; `openChannel` → `channel`; board views keep both
sidebars; snapshot-free (behavioral asserts only).

### T8 — Fixture/test reconciliation + cleanup sweep

Delete the dead branch surface: `ObservationPaneKind` remnants, the
`ChannelSidebar.tsx` file (T5), the orphaned `AcpMessage`/`AcpBlock` **type
aliases** in `stub-data.ts` (T1 already removed the `conversation` field + its
fixture data; only the bare types can remain — verify nothing consumes them
with a grep gate), and the branch's `observedAgentId` plumbing (T2). Reconcile
the wholesale-rewritten `app.css` (`app.css:2646+` "Channel-first surface … the
board is demoted to a secondary projection") back to the board-primary layout,
folding each surface task's CSS as it lands. Reconcile `store.test.ts` /
`comms.test.ts` suites to the final surface; run the full battery.

`Interfaces:`

- Consumes: everything T1-T7. Produces: a green `moon run compass-ui:test`,
  biome-clean tree, no orphaned exports (grep gate: `ObservationPaneKind`,
  `DEFAULT_OBSERVATION_PANE`, `AcpConversation`, `ChannelSidebar`,
  `selectChannel`, `showChannel`, `agent-supervisor`, `AgentKind`, `KIND_LABEL`,
  `harness`, `AgentEvent`, `kind-tag` → zero hits).

Test cycle: the full suite green under `--conditions browser`; the deleted
suites' coverage demonstrably re-homed (T2/T3 restored suites).

## Tasks

- [ ] **T0 — Proto: drop both `harness` fields (stacked PR, out of the UI
      reshape)**: remove `AgentAccount.harness` (field 2, `comms.proto:140-142`)
      AND `CreateAgentRequest.harness` (field 3, `comms.proto:377-379`), each
      with `reserved` on its number + name, then regenerate — buf-breaking,
      permitted pre-launch (Server on main is ephemeral, no live client).
      Commits the no-BYOA v0.6 stance to the wire (`../compass-0.6/design.md:676-680`):
      with both gone, nothing writes or reads `harness` on the wire, so the
      store's `agent_accounts.harness` column + `NewAgent`/`AgentAccount` harness
      types are fully orphaned (cleaned up in the SEA-1243 comms-server lane's
      store-harness task, not this record's T2). A small separate PR
      in the comms-server lane; the UI (T1) drops its `harness` consumers
      regardless of when T0 lands.
- [ ] **T1 — Agent identity (separate co-addressed types) + comms-fixture
      reconciliation**: one account-id space, `Account` (+`homeChannelId`) and
      `AgentLifecycle` composed by `agentView`, surviving roster = the ten board
      agents + matt, derived `STUB_ACCOUNTS`, home DM per agent (id cached),
      `STUB_SESSIONS`/messages/memberships re-homed, `assignee` migration,
      `harness`/`AgentKind`/`feed` dropped, ACP field + fixture data dropped.
- [ ] **T2 — Store machinery restored**: `PaneKind`/`Pane`/`SplitNode`/
      `AgentTab`/`CHAT_TAB_ID` + `openTab`/`closeTab`/`splitActivePane`/
      `setFocusedPane`/`closePane`/`agentTabs`/`activeAgentTab`; log +
      section state; observation-pane surface removed.
- [ ] **T3 — Workspace chat-primary**: tab strip + split rendering restored;
      `chat` pane = `ChannelView` on the home DM; terminals/files as panes.
- [ ] **T4 — Log panel**: minimizable companion with trace, running dot,
      Stop.
- [ ] **T5 — Left sidebar**: collapsible Channels above collapsible Agent
      workspaces; `openChannel`; `ChannelSidebar.tsx` deleted.
- [ ] **T6 — Standalone channel view**: `ChannelView` center mount,
      `readonlyAsks` in standalone channels.
- [ ] **T7 — Shell restoration**: board-primary `App.tsx`, default `bridge`,
      `Channels|Board` swap removed, `FleetPane` re-pointed.
- [ ] **T8 — Reconciliation sweep**: dead code deleted, suites reconciled,
      full battery green.

## Open Questions

The three Matt-ruled forks (D1 identity, D2 log panel, D3 ask scope) are
**Decisions above, not questions**. What remains:

1. **[RESOLVED — Matt + compass service-owner] Agent identity model.** D1 models
   identity as **separate, co-addressed types** — a durable `Account` (comms),
   an ephemeral `AgentLifecycle` enum (`AgentSessionStatus.state`), and the
   opaque OMP session trace (`AgentSession`, already separate) — composed at the
   store seam into the `Agent` view-model, not one merged fixture object. The
   compass service-owner's field-mapping review against `compass.proto` @ main
   still holds and is folded into D1: (a) of the ephemeral fields only `state`
   (`AgentSessionStatus.state`, `compass.proto:126-129`) streams as an
   agent-object field today; (b) the `session_id → account` attribution is a
   client-side binding via `container_name` from `StartAgentSession`
   (`compass.proto:197-209`), the one open seam, not a new proto surface; (c) no
   agent:session 1:1 contract is baked into any type. Separating the types makes
   the lifecycle's optionality (a created-but-unstarted agent has none) honest,
   which the merged shape could not. No T-task shape change beyond T1's type
   definitions.
2. **[non-load-bearing, deferred] Group-DM asks.** D3 defers asks in
   *standalone multiplayer channels*; a `group_dm` (e.g. `dm-cook-xenophon`,
   `comms-stub.ts:322-329` — its membership re-homes onto surviving ids in T1,
   but any group DM is multiplayer regardless), so T6 treats group DMs as
   standalone (read-only asks). If Matt intends group DMs to behave like the
   1:1 workspace chat, that is a one-line predicate change in T6.
   **Recommendation**: read-only asks in group DMs (consistent with the
   multiplayer rationale).
3. **[non-load-bearing, deferred] Log-panel state scope.** T2 models
   `logOpen` as a single global signal reset on workspace entry. Per-agent
   persistence (remembering each workspace's minimize state) is a trivial
   later upgrade (a `Set<string>` keyed by agent id).
   **Recommendation**: global signal for v1.
