---
name: management-trees
description: Example agent-tree shapes for a Compass software factory, the always-a-root-Supervisor invariant, the name-by-function tenet, and the delegation mechanics a Manager runs daily.
---

# Management trees

Use this skill whenever you stand up a new subtree, propose a spawn, or scope a
delegation. Compass is an **agentic software factory**: a single operator runs
what would otherwise take a whole engineering org by managing a tree of Manager
agents that build software. A tree is not a one-off for a single task — it is
the standing org chart of the operator's software company.

## Invariant — there is always a root Supervisor

Every Compass tree has one root-level **Supervisor** — a node whose ROLE is
`supervisor` (see the closed role set below). It persists; the operator grows a
subtree per project, repo, or department beneath it. A lone Manager with no tree
above it is not the Compass shape — that is what a plain OMP session is for.

The root Supervisor:

- Is the operator's first point of contact and posts to the top-level channels
  (`#announcements`, `#incidents`).
- Carries top-down broadcasts for the whole tree — a posture like "I'm going to
  bed, will respond in the morning" goes to the Supervisor, which relays it
  down. Any Manager still talks to the operator directly for its own lane; the
  Supervisor is specifically the top-down / first-contact node.
- Spawns and owns the project subtrees.

## The contract — report to your parent, delegate to your children

Every agent's place in the tree is a **model fact, not a prompt convention**.
Each agent account carries a `parent_agent_id`: empty for a root agent,
otherwise the account id of the agent it reports to. It is set at creation — on
a spawn the spawning agent becomes the parent; a user-created agent may be given
an explicit parent (or none, making it a new root) — and stays editable
afterward via the `ReparentAgent` mutation, so a subtree can be reshaped without
teardown (the server rejects a cross-owner or cycle-forming move).

From that one field every agent derives its standing instructions:

- **Report up.** Results, PRs, and status flow to your parent — the agent named
  by your `parent_agent_id`, or the operator if you are a root. You surface
  state to the node directly above you; you do not report sideways or skip
  levels.
- **Delegate down.** A parent decomposes work and pushes it to its children,
  spawning or `task`-ing them (see *Delegation mechanics* below). Work flows
  toward the leaves; results flow back toward the root.

This is exactly how a running wave already behaves — hierarchical
report-to-parent, today held entirely in prompt text. The tree turns "who do I
report to" from per-prompt convention into a fact the instruction layer states
mechanically: read your `parent_agent_id`, report there. (Reading *your own*
current parent fresh — re-read because `ReparentAgent` can change it — is a
deferred affordance; see *Deferred affordances* below.)

## Tenet — name a Manager for what it DOES, not the tool it uses

Name each node the way you would name a **team or department in a company** —
a **CI Manager**, an **Observability Manager**, a **Payments Manager**. The
function is stable; the tools it reaches for are an implementation detail that
can change without renaming the node. When proposing a spawn, ask "what
team/department is this?"

**Counter-example (the anti-pattern):** do NOT name nodes for their tools — an
`aws` agent, a `grafana` agent, a `stripe` agent. A hard lesson from running the
wave: tool-named agents bind a node to a tool instead of a responsibility. The
`aws` agent should have been an **Observability Manager** or a **Platform
Manager**; the tool is what it reaches for, not what it is.

### Roles compose with names

A node's **role** and its **name** are two different things and both matter. The
role sets capability, model, and tools; the name states the function. The role
is one of a closed set of three:

- **`supervisor`** — owns the whole tree (the root; intake, incidents,
  operator first-contact, grows the project subtrees).
- **`owner`** — owns one product, service, or domain end to end (decomposes it
  into lanes, delegates to child managers, grows its own subtree).
- **`manager`** — owns one lane and drives it to done (the leaf).

So a single node is both a role and a function: a `supervisor` at the root, a
**Payments** `owner` below it, a **CI** `manager` under that. Pick the role for
the scope, name it for the function. The full role contract is in
`docs/concepts/agent-roles.md`.

## Example tree shapes

Every example names nodes by **function**, per the tenet above.

### Single product / service

Supervisor [`supervisor`] -> a **Product Manager** [`owner`] for that service ->
function Managers beneath it (**CI Manager**, **Observability Manager**,
**Frontend Manager**) [each `manager`] -> ephemeral workers [subagents].

- **When to use:** one shippable product or service with a few distinct
  concerns. The most common starting shape.
- **Channels:** a home channel between the operator and the Product Manager; one
  coordination channel per function Manager for dispatching to its workers.
- **Issue flow:** the operator files product issues to the Product Manager, which
  decomposes them into per-function issues and delegates down; results and PRs
  flow back up to the Product Manager, which reports to the operator.

### Multi-service / monorepo (the current wave shape)

Supervisor [`supervisor`] -> a Manager per product area [each `manager`], each
owning a lane -> workers [subagents].

- **When to use:** several services or areas in one repo, worked in parallel.
  Mirrors today's merge wave, but named by area/function rather than by repo or
  tool.
- **Channels:** one coordination channel per area Manager; a shared channel for
  cross-area collision-avoidance when lanes touch the same files.
- **Issue flow:** issues are routed to the owning area Manager; each drives its
  lane end-to-end and reports PRs up. Cross-lane entanglements are surfaced on
  the shared channel, not resolved silently.

### Whole company from one operator

Supervisor [`supervisor`] -> department Managers [each `owner`] mirroring an org
chart (**Platform**, **Payments**, **Growth**, **Docs**), each growing its own
subtree.

- **When to use:** the "build a company with one person" shape — multiple
  products or business functions run concurrently.
- **Channels:** each department Manager owns a coordination channel for its
  subtree; the Supervisor holds the top-level channels for company-wide posture.
- **Issue flow:** the operator sets direction through the Supervisor or directly
  to a department Manager; each department decomposes within its subtree.
  Department Managers report status up to the Supervisor, which aggregates for
  the operator.

### Design-heavy / greenfield

Supervisor [`supervisor`] -> a **Design-Lead Manager** [`owner`] producing frozen
design records -> implementation Managers [each `manager`] executing them.

- **When to use:** greenfield or high-ambiguity work where the contract must be
  settled before code is written.
- **Channels:** a design channel where records are proposed and reviewed; a
  handoff channel where frozen records are picked up by implementation Managers.
- **Issue flow:** design issues land on the Design-Lead Manager, which ships a
  frozen record (its own PR, human-merged); implementation Managers then take
  execution issues that descend from the merged record and build against it.

## Delegation mechanics — what a Manager runs daily

A Manager is a **coordinator, not a typist**. Implementation is done by
in-process **subagents** you brief via the live `task` tool. Standing child
Managers are a different thing (below).

### Scope the subagent

Cut work into **one self-contained slice** per subagent: exact file targets, the
change, the acceptance criterion, and the non-goals. A slice too large to hold
in one review is two slices. Keep parallel subagents on non-overlapping file
zones so their diffs do not collide.

### Author its brief and initial prompt — the prompt IS the delivery surface

The subagent has **no shared chat history with you** — the brief is its entire
world. Everything it needs rides the initial prompt plus the shared context you
hand it: the file targets, the change, the frozen contract it descends from (a
merged design record when there is one), and the acceptance. A missing fact in
the brief is a defect the subagent cannot recover from. This is MP-5: the
subagent prompt is the delivery surface, so write it as the complete spec, not a
pointer to context the subagent cannot see.

Dispatch is non-blocking: brief the subagent, end your turn, and resume when it
reports back. Then **review the returned diff** — you are the judgment, the
subagent is the hands.

### Worker lifetime — hold until the PR merges

A worker scoped to a PR **stays warm until its PR merges or is dropped**, owning
the review and CI-fix loop. A PR's life does not end at first push: review
findings and CI failures arrive after. Reaping a worker at first push means every
bounce re-spawns a cold worker that re-acquires the whole context — the most
expensive shape for the most common loop. "Ephemeral" means *scoped and
disposable*, not *short-lived*: a worker is reaped when its scope closes, which
for a PR worker is merge, not push. (This mirrors the `hold-your-lane` rule.)

### Subagents vs standing Managers

- **Subagents** (in-process, via `task`): ephemeral implementation hands. No
  operator approval needed — spin them freely within your lane.
- **Standing child Managers** (via `agents_spawn_peer`, torn down with
  `agents_despawn_peer`): long-lived tree nodes. Spawning one **needs operator
  approval first** — propose it on your home channel, wait for a yes, then spawn.
  The approval gate governs tree *growth*, not day-to-day dispatch.

## Deferred affordances

These are referenced only; the tools are not live yet:

- Tree navigation / visualizing the tree — [TODO compass_tree].
- Fresh-read of the roster and your parent (re-parenting can change it) —
  [TODO RIG-1721].
