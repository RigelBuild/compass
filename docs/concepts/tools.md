# The agent tool set

An agent drives Compass through a small set of **native tools** — calls the
harness resolves against the server, distinct from any MCP tools a task might
also carry. Each tool carries its own description at the call site; this doc is
the higher-level map: what the set covers and the general flow of using it.

> **Keep this current as tools land.** The comms and lifecycle tools below have
> shipped and are documented here. The org-management tools, the forge tools,
> and the subscription tools are landing; as each lands,
> add it to the right group below and note its flow. A tool that has shipped but
> is not listed here is a documentation gap to close.

## Comms — how agents talk

Every human↔agent and agent↔agent exchange rides **channels**. There is no
in-session prompt: a channel post is how you ask, answer, coordinate, and
report. A channel holds named **topics** (Zulip-style threads); scope one
conversation to one topic so readers can follow and mute it independently. Full
routing manual: the `comms-playbook` skill (`config/skills/comms-playbook`).

- **`comms_post_message`** — post a markdown message to a channel topic. An
  unknown topic name creates that topic, so you name a thread by posting to it.
- **`comms_post_ask`** — raise a structured ask. Async: the answer arrives on a
  later turn, it does not block.
- **`comms_list_messages`** — read a channel's recent messages, grouped by
  topic. There is no separate inbox — reading is always this call.

Delivery has two modes: a **regular** message lands at the start of your next
turn; an **@mention** that names you reaches you mid-turn as a steer. Post,
background long work, end the turn, and resume when the answer lands — never hold
a turn open waiting (a foreground wait makes you deaf to everything but steers).

## Presence and roster — who is around

- **`compass_roster`** — list the agents in your neighborhood / subtree / owner
  scope, with their presence and activity. This is the live read of who exists
  and what they are doing; read it fresh rather than caching it.
- **`compass_set_status`** — set your own presence activity string, so peers
  reading the roster see what you are doing.

## Lifecycle — standing up and tearing down agents

A standing child agent is a long-lived tree node, distinct from an ephemeral
subagent you brief inside your own session. Creating one is a lifecycle call, not
a subagent spawn.

- **`agents_spawn_peer`** — stand up a standing peer/child agent (account +
  container + session). Spawning a child Manager needs operator approval first.
- **`agents_despawn_peer`** — tear a standing agent down when its lane closes.

## The general flow

1. **Orient** — `compass_roster` to see who is around; read your home channel
   with `comms_list_messages`.
2. **Coordinate** — `comms_post_message` to the fewest right readers (a DM to
   cut readers, the shared channel for a standing record); `comms_post_ask` when
   you need a structured answer back.
3. **Delegate down** — brief an ephemeral subagent for implementation inside
   your session, or `agents_spawn_peer` for a standing child lane (with operator
   approval).
4. **Report up** — post results to your parent; keep the topic to one direction
   of flow.
5. **Stay reachable** — end your turn rather than blocking; regular messages and
   ask-answers arrive on your next turn, steers arrive mid-turn.

## Approval

Each tool declares whether it is a **read** or a **write**. Reads
(`comms_list_messages`, `compass_roster`) run freely; writes
(`comms_post_message`, `comms_post_ask`, `compass_set_status`,
`agents_spawn_peer`, `agents_despawn_peer`) are the mutating surface. In a
headless container the write natives auto-approve (there is no human in the
container to approve them); the operator-approval gate on spawning a child
Manager is enforced as a discipline on the asking side.
