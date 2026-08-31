# Agent roles: supervisor, owner, manager

Every standing node in a Compass tree is a **Manager agent**, and every one of
them carries a **role** that sets what it does. There are exactly three:

- **`supervisor`** — owns the whole tree. It is the operator's first point of
  contact, routes incoming issues down to the owning subtree rather than working
  them, is the default target for alerts and incidents, and carries top-down
  broadcasts for the whole tree. It grows and owns the project subtrees. There is
  always exactly one at the root (see [management trees](../../config/skills/management-trees/SKILL.md)).
- **`owner`** — owns one product, service, or domain end to end. It decomposes
  its area into per-function lanes, delegates each to a child `manager`,
  aggregates status and PRs back up, and grows its own subtree. The mid-tier.
- **`manager`** — owns one lane and drives it to done. The leaf: it is assigned
  issues, holds them end to end, ships stacked PRs through the review loop, and
  stops only when blocked on human input.

The roles form a hierarchy of scope — tree, domain, lane — but they are not a
chain of command distinct from the tree itself: an agent's parent and children
are a model fact (its `parent_agent_id`), and the role names what that node is
responsible for at its place in the tree.

## A role selects a block-0 prompt

A role is not a flag the agent reads and interprets — it selects the agent's
**system prompt**. Each role has a prompt at `prompts/<role>/SYSTEM.md`, and the
role a node is spawned with picks which one is injected as its block-0
`customSystemPrompt`, **replacing** the default. The prompt *is* the role's
capability and posture; there is no second place the role's behavior lives. This
is orthogonal to **persona**, which *appends* stable working context (the repos,
projects, and lanes a node owns) rather than replacing the block-0 posture — so a
node's identity is its role prompt (replace) plus its persona (append). See
[persona](persona.md).

## Role is not name

A role sets a node's capability, model, and tools; a node's **name** states its
**function** — what team or department it is. The two compose and do not
collide: an `owner` might be the *Payments* owner and a `manager` the *CI*
manager. Name a node for what it does, never for the tool it reaches for (an
`aws` or `stripe` node is the anti-pattern) — the function is stable, the tools
are an implementation detail. The naming tenet and example tree shapes are in
[management trees](../../config/skills/management-trees/SKILL.md).

## Workers are not a role — they are subagents

There is deliberately **no `worker` role**, and no tree node per implementation
hand. Implementation is done by **subagents**: in-process workers a Manager
briefs and dispatches inside its own session. Minting a tree node per worker
would cost a durable account handle and a per-agent container each — untenable
for workers that are numerous and short-scoped, whereas a subagent rides its
Manager's existing session and container at zero marginal cost. A subagent is
not a peer on the mesh, and this is **structural, not a prompting convention** —
the same way the [comms model](comms-model.md) keeps work and conversation on
separate surfaces by construction:

- A subagent has **no Compass handle or account** — it is not addressable, it
  cannot be spawned as or reparented into a tree node.
- A subagent holds **no Compass comms tools** — it cannot post to a channel, so
  it cannot reach the operator or another Manager. The operator has no channel to
  a worker and redirects one by pinging the Manager that owns it.
- A subagent has **neither comms surface**: no channel *and* no session log of
  its own on the mesh. Its work lives entirely inside its Manager's session log,
  nested under it.

Because a worker holds no comms tools and no handle, all user-facing and
cross-node traffic necessarily routes through Managers — comms centralization is
enforced by what a subagent structurally *is*, not by asking it to behave. That
is why "worker" is a lifecycle stage inside a Manager's session, not a rung in
the tree.
