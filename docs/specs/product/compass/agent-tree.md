# The agent tree

The agent tree is Compass's organizing primitive. Every agent account carries a
`parent_agent_id` (empty means the agent is a **root**), so the accounts
themselves capture the reporting structure a fleet already runs: a coordinating
agent spawns workers that report up to it, and delegation flows back down the
same edges. The tree is not a separate index laid over the accounts — it *is*
the accounts, one edge per agent.

`parent_agent_id` is set at creation. On the spawn path the parent is the
spawning agent's account; on `CreateAgent` it is an optional user-supplied
parent (empty creates a root). It is editable afterward via `ReparentAgent`,
which changes that one edge — so moving an agent moves its whole subtree with
it, since every descendant still points at its unchanged parent. Because
accounts are permanent, re-parenting a running agent keeps its grounded context
rather than tearing it down and re-spawning.

The workspaces sidebar derives its tree from `parent_agent_id` rather than a
user-defined folder organization, and an agent reads its own parent off its
account through `ListAccounts`. Every agent created through spawn or
`CreateAgent` carries a role from the closed Manager taxonomy (`supervisor`,
`owner`, `manager`) and an optional persona, both set at creation (below). Rows
created before the role check may hold an empty or unknown role; either falls
back to the default block-0. Composing channels or subtree-applied roles onto
the tree is named for later (RIG-1622, RIG-1623) and is not yet built.

## Requirement: An agent account carries a parent, forming the agent tree

Every `AgentAccount` SHALL carry a `parent_agent_id`; an empty `parent_agent_id`
SHALL mean the agent is a root. The parent SHALL be set at creation: on the
spawn path it SHALL be the spawning agent's account id, and on `CreateAgent` it
SHALL be the optional user-supplied `parent_agent_id` (empty creating a root).
When a non-empty parent is supplied at creation the server SHALL validate it —
the parent MUST exist and MUST have the same owner as the agent being created —
and SHALL reject a supplied parent that fails either check. Every creation SHALL
carry a `role` of `supervisor`, `owner`, or `manager`; the server SHALL reject
any other value, including empty, with `INVALID_ARGUMENT` before writing.

### Scenario: A spawned agent's parent is the spawning agent

- **Given** an agent that spawns a worker account
- **When** the worker is created on the spawn path
- **Then** the worker's `parent_agent_id` is the spawning agent's account id,
  and the worker appears under it in the derived tree.

### Scenario: An agent is created as a root

- **Given** a `CreateAgent` call with an empty `parent_agent_id` and a valid
  `role`
- **When** the account is created
- **Then** it carries an empty `parent_agent_id` and that `role`, and is a root
  of the agent tree.

### Scenario: A creation without a taxonomy role is rejected

- **Given** a `CreateAgent` call whose `role` is empty or outside the taxonomy
- **When** the server handles it
- **Then** it returns `INVALID_ARGUMENT` and creates no account.

## Requirement: An agent's role and persona are set at creation

`CreateAgent` SHALL require a `role` and accept an optional `persona`. The
`role` MUST be one of the closed set `supervisor`, `owner`, or `manager`; the
server SHALL reject any other value, including empty, with `INVALID_ARGUMENT`
before writing anything. A `persona` longer
than 64 KiB (`MaxPersonaBytes`) SHALL be rejected with `INVALID_ARGUMENT`. The
spawn path SHALL require a role from the same set and apply the same persona
cap. The server SHALL store both on the account; provisioning SHALL read them
from the stored account, never from a provision request. Re-spawning an
existing agent SHALL keep its stored role and persona.

### Scenario: An off-taxonomy role is rejected

- **Given** a `CreateAgent` call with `role` set to `director`
- **When** the server handles it
- **Then** it returns `INVALID_ARGUMENT` and creates no account.

### Scenario: A re-spawn keeps the stored role and persona

- **Given** an agent created with role `owner` and a persona
- **When** the operator spawns it again, passing a different `--role`
- **Then** the agent provisions with its stored role and persona, and the CLI
  reports that the flags were not applied.

## Requirement: Re-parenting moves an agent's subtree, validated against cycles and ownership

`ReparentAgent(agent_account_id, new_parent_agent_id)` SHALL re-parent the named
agent by changing its single `parent_agent_id` edge, which moves the agent
together with its whole subtree. An empty `new_parent_agent_id` SHALL promote the
agent to a root. The server SHALL authorize the caller — the caller MUST be the
owner of the moved agent or an agent of that owner, unconditionally, including a
promote-to-root — and SHALL validate the move atomically, so no interleaving of
concurrent valid re-parents can persist a cycle: a non-empty new parent MUST
exist, MUST have the same owner as the moved agent, and MUST be neither the agent
itself nor any transitive descendant of it.
On success the server SHALL return the mutated `Account` and publish an
`AccountChanged` event so surfaces re-derive the tree. Rejections SHALL map to
the enforced gRPC codes: `PERMISSION_DENIED` for a caller lacking authority or a
cross-owner parent, `FAILED_PRECONDITION` for a cycle-forming move, and
`NOT_FOUND` for a non-existent parent.

### Scenario: A re-parent moves the subtree and emits `AccountChanged`

- **Given** an agent with a subtree, owned by the caller
- **When** the caller calls `ReparentAgent` naming a same-owner agent as
  `new_parent_agent_id`
- **Then** the server returns the mutated account under its new parent, the
  subtree moves with it, and an `AccountChanged` event is published so
  subscribers re-derive the tree.

### Scenario: A cross-owner parent is rejected

- **Given** a `ReparentAgent` whose `new_parent_agent_id` is an agent of a
  different owner
- **When** the caller invokes it
- **Then** the server rejects it with `PERMISSION_DENIED` and mutates nothing.

### Scenario: A cycle-forming move is rejected

- **Given** a `ReparentAgent` whose `new_parent_agent_id` is the agent itself or
  one of its transitive descendants
- **When** the caller invokes it
- **Then** the server rejects it with `FAILED_PRECONDITION`, and the edge is
  unchanged.
