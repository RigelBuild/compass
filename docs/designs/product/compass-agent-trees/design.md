# Compass agent trees

Status: Active

Tracker: SEA (the spawning agent fills the issue id when the PR opens).

Ledger: this record's PR appends DL-095 to
`docs/designs/product/DECISIONS.md` in the same diff (see §Ledger delta) and
supersedes no existing row (§Ledger delta states the call), so it satisfies
the ledger gate's touch-coupling leg directly — no `Ledger-impact:` escape
hatch is needed in the PR body.

**This is a foundational, organizing record.** It does not add a feature; it
names the primitive the product is built around and states how every existing
and future surface hangs off it. The code delta it mandates is deliberately
small (one proto field plus deriving what is hand-authored today); the weight
is in the framing — how the UI is structured, how users are instructed, and
what instructions agents receive all flow from the agent tree.

## Problem / Intent

Agents do not scale flat. A working fleet is already hierarchical in
practice — a coordinating agent spawns worker agents that report back up to
it, and delegation flows down — but Compass models none of that structure:
`AgentAccount` carries an owner and a home channel and no relationship to any
other agent (`compass/proto/compass/v1/comms.proto:130-142`), and the only
tree the UI shows is a hand-authored, user-defined folder organization
(`compass/apps/ui/src/stub-data.ts:1246-1306`) that has nothing to do with
how the fleet actually reports.

Matt ruled (2026-08-01): "on agent trees — I think we emphasize more that the
whole structure should be built around this. Compass is a way to build and
manage these agent trees, and all of our design decisions flow off of that.
It doesn't change a huge amount of the code we already wrote but informs how
we build the UI, how we instruct people to use it, what instructions we give
to agents."

So the intent is a reframe, held in deliberate tension with a small
mechanism: **Compass is a tool to build and manage agent trees.** The agent
tree is the organizing primitive; the board, the workspaces sidebar,
channels, roles, user guidance, and agent instructions all flow from it. The
existing product framing — "Compass is an Agentic Development Environment
(ADE) — a persistent local **server** (`compass-server`, Go) fronting a web
**UI**" (`docs/specs/product/compass.md:17-18`) — is not overturned; it is
given its spine. Compass remains an ADE; *what the ADE is for* is building
and managing the tree of agents that does the development.

## Approach

### The tree is the primitive; the model change is one field

The mechanism is a single self-referential parent id on the agent account:

```proto
message AgentAccount {
  string owner_user_id = 1;
  reserved 2;
  reserved "harness";
  string home_channel_id = 3;
  // The parent agent's account id; empty for a root agent (the top of a
  // tree). Set at creation: the spawning agent for agent-spawned accounts,
  // empty (or a user-chosen parent) for user-created accounts.
  string parent_agent_id = 4;
}
```

Grounding, verified this run:

- `AgentAccount` today is `owner_user_id = 1`, `reserved 2` +
  `reserved "harness"`, `home_channel_id = 3`
  (`compass/proto/compass/v1/comms.proto:130-142`:
  `string owner_user_id = 1;` … `reserved 2;` … `string home_channel_id = 3;`)
  — so **field 4 is the next free number**.
- The self-referential parent-id shape has an in-file precedent:
  `ChannelGroup.parent_group_id` (`comms.proto:155-169`: "`// Parent group;
  empty for a top-level group.`" / `string parent_group_id = 3;`). The agent
  tree adopts exactly that convention — empty means root.
- **Set at creation.** An agent account is minted through one production path
  today (`CreateAgent`), with a second designed but not yet built (the
  agent-facing spawn path); both are parent-assignment sites:
  - The public `CreateAgent` RPC (`comms.proto:41-42`: "`// is the caller,
    not a request field — a user creates agents they own.`" /
    `rpc CreateAgent(CreateAgentRequest) returns (CreateAgentResponse);`),
    whose request is `{handle, display_name}` (`comms.proto:474-482`). A
    user-created agent is a **root** (empty parent) unless the user chooses a
    parent.
  - The agent-facing spawn path: `SpawnPeerRequest` on the `AgentGateway`
    lifecycle family, orchestrated server-side through `LifecycleCaller` →
    `store.CreateAgent`
    (`docs/designs/product/compass-agent-spawn-despawn/design.md:659-662`:
    `SpawnAsAccount(ctx context.Context, caller store.AccountID, req
    *compassv1internal.SpawnPeerRequest)`). A spawned agent's parent **is
    the spawning agent** — the server resolves the caller's account id at
    the same edge where it already resolves the caller's owner.
- The spawn/despawn record explicitly notes today's gap this field closes:
  "Under same-owner ownership there is no parent edge at all — despawning
  the supervisor leaves its spawned peers running"
  (`compass-agent-spawn-despawn/design.md:859-860`). `parent_agent_id` is
  that edge, made first-class. Despawn's handling of a dead parent's
  children (orphan, no cascade) stays exactly as the spawn/despawn
  record has it; this record does not redesign despawn. What this record
  *does* decide is the user-initiated edge move — re-parenting — designed
  below (§The tree is editable).

The motivating reality is how a multi-agent wave already runs: one
coordinating agent at the top spawns workers, each worker reports its results
up to the agent that spawned it, and delegation flows down the same edges.
The tree is not an aspirational org chart — it is the reporting structure the
fleet already has, captured in the model instead of living only in prompts.

### The tree replaces the user-defined folder organization

**Matt ruled: replace, not coexist.** Today the workspaces sidebar renders a
hand-authored folder tree:

- `Folder` + `TreeNode` (`compass/apps/ui/src/stub-data.ts:356-370`: "`/** A
  user-defined folder grouping agents; folders nest arbitrarily. */`" —
  `Folder {id, name, color, icon, children}` and `TreeNode = {kind:"folder"}
  | {kind:"agent"}`).
- The fixture `STUB_TREE` (`stub-data.ts:1246-1306`), which today excludes the
  moat agents by convention (`stub-data.ts:1241-1245`: "`The moat agents are
  not tree leaves: the supervisor is baked into its own pinned pane, and the
  warden lives in the right sidebar's fleet tabs`") — a special-case this
  record retires (see below).
- `AgentsSection` in the left sidebar renders it directly
  (`compass/apps/ui/src/components/LeftSidebar.tsx:305-330`: "`the existing
  user-organized folder tree of agents`" / `<For each={STUB_TREE}>{(node) =>
  <Node node={node} />}</For>`).

After this record: **the workspaces tree IS the agent tree, derived from
`parent_agent_id`.** The manual `Folder`/`TreeNode` grouping is removed as a
source of truth — the sidebar just follows the tree. The recursive render
machinery (`FolderRow`/`Node`/`AgentLeaf`, `LeftSidebar.tsx:33-114`) is
reusable as-is in shape — a parent agent's row renders where a folder row
renders, its children nested under it — but its *input* becomes the derived
agent tree, never user folders.

**No agent is special-cased (Matt, 2026-08-01).** The moat convention that
kept the supervisor and warden out of the tree is retired: *every* agent
appears in the derived tree under its parent, supervisor and warden included.
There is no built-in exclusion and no privileged node. The user and their
agents build the tree however the work wants it — a single root with
everything beneath it, or several independent roots — purely by how
`parent_agent_id` is set at creation. Whether an agent is *also* surfaced in a
pinned pane or a fleet tab is a separate presentation choice (configurable
pins, its own record), not a reason to drop it from the tree. This is the
direct extension of de-special-casing the supervisor/warden: the tree carries
all agents; pinning is layered on top, never a hole in the structure.

The loss is stated honestly: users lose arbitrary manual grouping (folders
by project, by color, by whim). They gain a sidebar that matches how the
fleet actually reports — the structure they *manage* is the structure they
*see*. If manual grouping is wanted later it is a future overlay on top of
the tree, not designed here — Matt chose replace.

### The tree is editable: re-parenting is first-class

**Matt ruled (2026-08-01): re-parenting is a first-class operation, not a
set-at-creation-only frozen edge.** The hierarchy is not a one-shot decision.
A user iterates on structure as the work teaches them which reporting lines
fit, and the shape drifts as a project evolves. So `parent_agent_id` is
mutable after creation, not fixed at mint.

The gesture: move an agent under a new parent, or promote it to a root
(empty parent). The agent's whole subtree moves with it — only the one edge
changes, because every descendant still points at its own unchanged parent.

Why first-class rather than teardown-and-rebuild: agent accounts are
permanent (`DECISIONS.md` DL-077: "teardown is compute-only" — the account
outlives its running session) and a running agent carries grounded working
context. Tearing a subtree down to re-pin it elsewhere discards that context
and re-pays the grounding cost from scratch; re-parenting moves the edge and
keeps the agents running. Forcing a rebuild for every hierarchy change is
exactly the cost this avoids.

Mechanism (contract in T3): a `ReparentAgent` mutation, modeled on the
existing `UpdateChannelMembers` shape (`comms.proto:62-65`: "caller-authorized
… Emits ChannelChanged. One RPC covers join, subscribe-toggle, …"). The
server validates the move — caller authority, same-owner, and no cycle
(§below) — and emits `AccountChanged` (`comms.proto:427-430`: "An account
was created or changed." / `AccountChanged {Account account = 1}`), the same
event every surface already re-derives the tree from. So re-parenting needs
no surface-specific plumbing: the sidebar and board reflect the move by
re-deriving from the changed `parent_agent_id`, exactly as they do for any
account change. The client applies `AccountChanged` as an **in-place,
id-keyed update** of the agent in its list — the account's position in the
input array is unchanged — so the moved agent keeps its original insertion
position among its new siblings, and the derivation's stable-input-order
tie-break (§T4) stays deterministic across the move rather than reshuffling
visibly. Collapse/expand state is keyed on agent id (T4), so a moved subtree
keeps its expand state across the move.

A running agent's standing instruction ("report results up to your parent")
reads the parent fact lazily: the agent re-reads its parent off its own
account (`ListAccounts`, `comms.proto:44-45`) at each consult, so it picks up
a re-parent on its next read. Brief staleness between the move and that read
is accepted for the MVP; pushing a re-parent to a running agent mid-turn is a
named post-MVP seam, not built here.

### How each surface flows from the tree

This is the record's spine — the "all of our design decisions flow off of
that" part. Per surface:

- **Workspaces sidebar (`LeftSidebar.tsx`).** Renders the agent tree
  directly (previous section). Expanding/collapsing a row
  expands/collapses a subtree; an agent's children nest under it; a root
  agent sits at the top level.
- **Board views (`board.ts`, Bridge).** Swimlanes key on `assignee` today
  (`compass/apps/ui/src/board.ts:45-66`: `boardAgents` filters
  `agents.filter((a) => all.some((w) => w.assignee === a.account.id …))`,
  `cellItems` narrows by `w.assignee === agentId`) — a flat agent list. The
  tree gives the board two new capabilities, stated here as the contract a
  downstream board record consumes:
  1. **Tree-ordered/grouped lanes** — swimlanes ordered by tree position (a
     parent's lane followed by its children's, depth-first), so the board
     reads as the org reads.
  2. **Subtree filtering** — scope the board to one subtree (one root's
     whole reporting line), e.g. "show only this tree's work".

  The board Issues/PRs remodel (a separate downstream record) consumes this
  ordering/filtering contract for its grouping and filter UI; this record
  establishes the contract and stops there.
- **Pins (separate downstream record).** Pinning relates to tree position:
  the tree makes "pin this subtree's lead" the natural gesture — pin the
  parent, and its reporting line is one expand away. Pins are not designed
  here.
- **User guidance (docs/onboarding).** See §How this informs usage below.
- **Agent instructions.** See §How this informs agent instructions below.
- **Channels (SEA-1622).** Channels currently form their *own* tree via
  `ChannelGroup.parent_group_id` (`comms.proto:155-169`) — a parallel
  namespace hierarchy. Unifying them under the agent tree (an agent's
  subtree implying its channel scope) is **SEA-1622, post-MVP**: this record
  makes the agent tree the primitive SEA-1622 will fold channels into, and
  deliberately does **not** do that folding — the two trees coexist until
  SEA-1622 lands.
- **Roles (SEA-1623).** Roles compose onto the tree: a role applied to an
  agent can scope to its subtree (everything under this parent inherits the
  posture). Referenced, not designed here.

### How this informs usage

Compass is used by **building a tree**. The onboarding narrative flows
directly from the primitive: you create a root agent; you give it children
(or it spawns its own); you watch the tree work in the sidebar and the
board; you scope your attention by subtree. Documentation and first-run
guidance teach tree-building as *the* workflow — not "create some agents and
organize them into folders", but "grow the reporting structure your work
needs".

### How this informs agent instructions

Every agent knows its place in the tree, and its standing instructions
derive from it: an agent is told who its parent is and reports results up to
that parent; delegation flows down — a parent decomposes work and spawns or
tasks its children. This is exactly how a running wave already behaves
(hierarchical report-to-parent, held today entirely in prompt text); the
tree turns "who do I report to" from per-prompt convention into a model fact
the instruction layer can state mechanically.

## Alternatives considered

### Coexist: keep user folders alongside the agent tree

Keep `Folder`/`TreeNode` as a user-curated view and add the agent tree as a
second sidebar mode or an overlay. Rejected — **Matt ruled replace**. Two
trees over the same leaves means two sources of truth for "where is this
agent", a mode toggle nobody maintains, and a sidebar that can contradict
the reporting structure. The folder tree's one real value (arbitrary manual
grouping) is deferred to a possible future overlay on the tree.

### A separate organization tree divorced from the reporting structure

Give agents a `group_id` into a free-standing org hierarchy (folders as
first-class server objects), independent of who spawned or supervises whom.
Rejected: it recreates the folder tree with extra steps and misses the
point — the structure worth showing and managing is the one the fleet
*actually runs on* (spawn/report/delegate edges), not a second, manually
curated taxonomy that drifts from it.

## Plan

### Global Constraints

- **Replace, not coexist** (Matt, 2026-08-01): the agent tree is the only
  workspaces-sidebar organization; no folder mode, toggle, or overlay ships
  in this arc.
- **Derived, never hand-organized**: every tree surface renders from
  `parent_agent_id`; no surface persists its own copy of the hierarchy.
- **Low code churn**: one proto field, set-at-creation wiring, and derive
  what is hand-authored today. No migration machinery — no wire build has
  shipped a populated `AgentAccount` tree, so the field is authored
  directly.
- **Field number 4** on `AgentAccount` (2 is reserved with name `harness`,
  `comms.proto:134-137`); empty string = root, mirroring
  `ChannelGroup.parent_group_id` (`comms.proto:160-161`).
- **Scope fence**: channels stay on their own `ChannelGroup` tree until
  SEA-1622; roles-on-subtrees is SEA-1623; board grouping/filter UI and pins
  are separate downstream records. This record establishes the primitive
  and its contracts, nothing more.
- **Re-parenting is first-class** (Matt, 2026-08-01): `parent_agent_id` is
  mutable post-creation via a `ReparentAgent` mutation, so users iterate
  hierarchy without teardown. The server rejects a cross-owner or
  cycle-forming re-parent (§T3).
- **Vocabulary**: *agent tree*, *root agent* (empty `parent_agent_id`),
  *subtree*, *parent/child*. No "folder" vocabulary survives on the agent
  surfaces.

### T1 — this record + ledger row (this PR, docs-only)

Freeze this record at `docs/designs/product/compass-agent-trees/design.md`
and append DL-095 to `docs/designs/product/DECISIONS.md` in the same diff
(§Ledger delta). No code changes.

- Interfaces: `docs/designs/product/DECISIONS.md` (append one row under
  §Strategy & positioning); this file.

### T2 — `parent_agent_id` on `AgentAccount` (downstream)

Add the field and regenerate.

- Interfaces: `compass/proto/compass/v1/comms.proto:130-142` — add
  `string parent_agent_id = 4;` with the root-is-empty comment, mirroring
  `ChannelGroup.parent_group_id` (`comms.proto:160-161`). Consumers: the
  generated `@compass/client` types and the Go store row for agent
  accounts.

### T3 — parent wiring: set at creation and re-parenting (downstream)

Populate the parent at both mint sites, and make the edge editable.

- Interfaces:
  - `rpc CreateAgent(CreateAgentRequest)` (`comms.proto:41-42`), request
    `{handle = 1, display_name = 2}` (`comms.proto:474-482`) — gains an
    optional `parent_agent_id` the server validates (§Server validation
    below — same resolved owner, must exist; empty = root).
  - The spawn path: `LifecycleCaller.SpawnAsAccount(ctx, caller
    store.AccountID, req *compassv1internal.SpawnPeerRequest)`
    (`compass-agent-spawn-despawn/design.md:659-662`) — the server sets the
    spawned account's `parent_agent_id` to the **calling agent's** account
    id at the same edge where it resolves the caller's owner
    (`design.md:295-296`: "`callerOwnerUserID, …` — the F2 ruling realized
    in one argument").
  - **Re-parenting** — a new `rpc ReparentAgent(ReparentAgentRequest)
    returns (ReparentAgentResponse)`, modeled on the existing
    `UpdateChannelMembers` mutation (`comms.proto:62-65`: "caller-authorized
    against channel visibility. Emits ChannelChanged."). Request
    `{string agent_account_id = 1; string new_parent_agent_id = 2;}` — an
    empty `new_parent_agent_id` promotes the agent to a root. Response
    carries the mutated account — `ReparentAgentResponse {Account account =
    1;}` — matching `UpdateChannelMembersResponse {Channel channel = 1;}`
    (`comms.proto:558-560`). The server emits `AccountChanged`
    (`comms.proto:428-430`); surfaces re-derive from the changed
    `parent_agent_id` with no surface-specific plumbing.
  - **Server validation** (applied on both creation and re-parenting):
    (0) **caller authority** — the caller must be the owner of
    `agent_account_id`, or an agent of that owner; this is **unconditional**,
    whether the new parent is empty or not, so promote-to-root is not an
    unauthorized back door (mirroring DL-075's same-owner-scoped despawn
    authority: any of an owner's agents/user may move any of that owner's
    agents — the accepted MVP authz scope). (1) **same-owner** — a non-empty
    parent's `owner_user_id` must equal the moved agent's resolved owner
    (`AgentAccount.owner_user_id`, `comms.proto:130-133`; the caller's
    resolved owner is `callerOwnerUserID` for an agent caller, the user id
    for a user). (2) **no cycle** — the new parent must be neither the agent
    itself nor any of its transitive descendants (walk the parent chain from
    the proposed parent, reject if the agent is reached; the walk carries a
    visited-set / depth bound so a pre-existing bad cycle cannot spin it).
    The validate-and-write runs as **one serialized unit** — a single
    Postgres transaction (the store of record, DL-019/DL-020) taking a
    per-owner-tree lock (row `FOR UPDATE` / advisory lock) so two concurrent
    individually-acyclic re-parents cannot interleave into a persisted cycle.
    (3) **existence** — a non-empty `new_parent_agent_id` must resolve to an
    existing agent account under the same resolved owner (clause 1).
    Set-at-creation cannot form a cycle (a new account has no descendants);
    re-parenting can, which is why the cycle check and its serialization are
    required now that the edge is mutable.
    Rejections map to distinct gRPC codes so the client can tell them apart:
    `PERMISSION_DENIED` for a caller-authority or cross-owner failure
    (clauses 0/1), `FAILED_PRECONDITION` for a cycle (clause 2),
    `NOT_FOUND` for a non-existent parent (clause 3).

### T4 — derive the workspaces tree (downstream)

The sidebar renders the derived agent tree; the manual folder model goes.

- Interfaces:
  - Remove `Folder`/`TreeNode` (`compass/apps/ui/src/stub-data.ts:356-370`)
    and `STUB_TREE` (`stub-data.ts:1246-1306`) as sources of truth; the
    fixture instead carries `parent_agent_id` on its stub agent accounts.
  - `AgentsSection` (`compass/apps/ui/src/components/LeftSidebar.tsx:305-330`)
    consumes a derived tree — `agentTree(agents: readonly Agent[]):
    AgentTreeNode[]` where `AgentTreeNode = {agent: Agent; children:
    AgentTreeNode[]}`, roots = accounts with empty `parent_agent_id`,
    children nested under their parent. **Ordering:** roots, and the children
    within each parent, preserve the stable input order of the `agents` array
    — depth-first alone is not a total order, so this sibling/root tie-break
    is what makes the derivation deterministic for a fixed input (the one
    contract the board's `treeOrder` consumes). A **dangling** `parent_agent_id`
    (referencing an account not in `agents` — e.g. filtered by visibility, or
    an unresolved id) is treated as a root: the derivation promotes such a
    child to top-level rather than dropping it, so no agent is ever
    unreachable from the tree. (Account deletion is out of scope — accounts
    are permanent, ratified `DECISIONS.md` DL-077 — so a dangling
    parent arises only from visibility filtering, not deletion.)
  - The recursive render machinery (`FolderRow`/`Node`/`AgentLeaf`,
    `LeftSidebar.tsx:33-114`) is reused with the derived node type as input:
    a parent agent's row carries the expand/collapse + descendant count a
    folder row carries today (`countAgents`, badge at
    `LeftSidebar.tsx:86`).
  - The collapse state moves from folder ids to agent ids: `store.ts`'s
    `isFolderCollapsed`/`toggleFolder` (`store.ts:273-274` interface,
    `:1541-1547` impl, exported `:1598-1599`) are keyed by folder id today;
    they re-key to parent-agent id (the collapse target is now an agent row),
    and their tests (`store.test.ts:527-546,683-695`) flip with them. Naming
    can follow (`isAgentCollapsed`/`toggleAgent`) or stay generic on a node
    id — a downstream-task call, not a contract point.

### T5 — the board tree contract (downstream)

Establish tree ordering and subtree filtering as pure helpers the board
record consumes.

- Interfaces: `compass/apps/ui/src/board.ts` — alongside
  `boardAgents(agents, all): Agent[]` (`board.ts:45-52`) and
  `cellItems(all, agentId, state): Issue[]` (`board.ts:57-66`), add:
  - `treeOrder(agents: readonly Agent[]): Agent[]` — depth-first order over
    the derived tree (parent's lane, then its children's), siblings and roots
    in the stable input order `agentTree` fixes (§T4), the swimlane
    ordering. It composes with the board's existing swimlane filter, which
    runs *after* ordering: the board takes `treeOrder`'s sequence and then
    keeps only `boardAgents` rows (agents with ≥1 active issue,
    `board.ts:45-52`). An issueless parent with issue-holding children drops
    out of the swimlanes while its children keep their relative depth-first
    order — a filtered-out ancestor never reorders the survivors. `treeOrder`
    orders the full agent set; filtering stays the board's concern.
  - `subtreeAgentIds(agents: readonly Agent[], rootAgentId: string):
    ReadonlySet<string>` — the subtree membership set, the board filter
    predicate. The set **includes `rootAgentId`** and every transitive
    descendant (a subtree includes its own root), so scoping the board to a
    subtree keeps the root's own lane.
  - Retire the stale rationale in `boardAgents`' doc comment
    (`board.ts:41-44`: "Moat agents (supervisor/warden) own no board lanes")
    — the exclusion is by *no active issue*, not by role, so the behavior is
    unchanged, but the comment narrates the retired moat convention on a
    surface this task lands in and must be rewritten to the issue-based
    reason. (The `AgentRole` vocabulary at `stub-data.ts:58` is roles, not
    exclusion — SEA-1623, left alone.)

  The board Issues/PRs remodel record consumes these for its grouping and
  filter UI; no board UI changes in this task.

### T6 — usage + agent-instruction narrative (downstream)

Fold the tree framing into user guidance and the agent instruction layer.

- Interfaces: the product docs' onboarding narrative (tree-building as the
  workflow, §How this informs usage) and the agent instruction layer, which
  splits by what is fleet-uniform vs. per-agent:
  - The **generic contract** — "report results up to your parent, delegate
    down to your children, in terms of `parent_agent_id`" — is the same for
    every agent, so it rides the fleet-wide agent config bundle (DL-078's
    Server-side bundle store, "one bundle all agents get").
  - The **concrete parent fact** — "your parent is agent X" — is per-agent
    data and cannot ride a one-bundle-for-all store; an agent reads it off
    its own account (`ListAccounts`, `comms.proto:44-45` "List the accounts
    visible to the caller") once T2's field lands (§How this informs agent
    instructions).

## Tasks

- [ ] **T1** — this record + the DL-095 ledger row, same diff (this PR,
  docs-only).
- [ ] **T2** — `string parent_agent_id = 4;` on `AgentAccount` + regen.
- [ ] **T3** — parent wiring: `CreateAgent`/spawn set the parent at
  creation; a `ReparentAgent` mutation makes the edge editable; server
  validates same-owner + no-cycle on both paths.
- [ ] **T4** — derive the workspaces tree from `parent_agent_id`; remove
  `Folder`/`TreeNode`/`STUB_TREE` as sources of truth; `AgentsSection`
  renders the derived tree.
- [ ] **T5** — `treeOrder` + `subtreeAgentIds` board helpers (the contract
  the board remodel consumes).
- [ ] **T6** — usage narrative + agent-instruction contract updates.

## Ledger delta

Appended to `docs/designs/product/DECISIONS.md` in the same PR that freezes
this record (touch-coupling), under **Strategy & positioning** — this is a
product-thesis decision, not a UI-shell detail:

> | DL-095 | The agent tree is Compass's organizing primitive — Compass is a tool to build and manage agent trees: `AgentAccount` carries `parent_agent_id` (field 4; empty = root, set at creation — the spawning agent, or user choice — and editable thereafter via a `ReparentAgent` mutation, so users iterate hierarchy without teardown), the workspaces sidebar and board views derive from and filter by the tree, REPLACING the user-defined folder organization (replace, not coexist); channels (SEA-1622) and roles (SEA-1623) compose onto the tree later | Active (Matt, 2026-08-01) | [agent trees §Approach](compass-agent-trees/design.md#approach) |

**No existing row is superseded — the call, stated:** the user-defined
folder organization this record replaces was never a ledgered decision. A
grep of `DECISIONS.md` for the workspaces/folder tree finds no row ratifying
it; the folder tree entered as fixture-level convention in the 0.7 shell
record ("the existing folder tree, `STUB_TREE` + `AgentLeaf`",
`compass-0.7-channel-workspace/design.md:587`), which is **Historical**
(version-narrative chain) and holds no Active folder-organization ruling to
flip. Adjacent rows stand untouched: DL-031 (board-primary shell) and
DL-034 (right sidebar) govern surfaces this record does not move; DL-001's
ADE framing is deepened, not overturned — Compass remains an ADE, and the
tree is what the ADE builds and manages. DL-001's topology leg ("named
workstream agents supervised by a Dispatcher, gated by a Warden") is
likewise generalized, not contradicted: the Dispatcher-supervised fleet is
the canonical single-root tree instance, and the no-special-casing ruling
(a user may build several independent roots with no supervisor anywhere)
makes that one shape among many the tree allows — DL-001 states the default
topology, not a mandated one, so no supersession is warranted.

## Open Questions

None. The two questions this record carried are now decided:

- **Re-parenting semantics** — decided (Matt, 2026-08-01): `parent_agent_id`
  is mutable post-creation via `ReparentAgent`, first-class in the MVP
  (§The tree is editable, T3). Despawn's handling of a dead parent's
  children is unchanged from the spawn/despawn record — orphan (no
  cascade): the account persists (`DECISIONS.md` DL-077, "teardown is
  compute-only"), so the child's `parent_agent_id` stays valid and children
  keep running nested under the now-inactive parent — no edge is cleared on
  despawn (`compass-agent-spawn-despawn/design.md:862`, "orphan (no
  cascade)").
  Re-parenting is a *user* gesture, not a despawn side effect.
- **Cycle/ownership validation** — decided: the server enforces same-owner
  and rejects cycles on both creation and re-parenting (§T3). Now that the
  edge is mutable a cycle is reachable, so the check is required, not
  deferred.
