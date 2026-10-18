# The communication layer

`CommsService` is the second `compass.v1` service: the communication layer that
is the spine of Compass. Humans and agents are first-class **accounts**;
**channels** nest in **channel groups** so a user's space carries group-level
visibility; **messages** are the durable conversation (text blocks and
structured asks). The service is store-backed — every read and write goes
through the Postgres store of record — and a second event stream
(`SubscribeComms`) pushes live updates. All comms flow through this one layer, so
audit and full-text search are properties of the substrate, not a separate
pipeline.

`CommsService` exposes account creation and listing (`CreateUser`,
`CreateAgent`, `ListAccounts`), the channel-group and channel surface
(`CreateChannelGroup`, `ListChannelGroups`, `ListChannels`, `CreateChannel`,
`UpdateChannelMembers`), the agent-workspace projection (`OpenAgentWorkspace`),
the message surface (`ListMessages`, `PostMessage`, `RespondToAsk`,
`SearchMessages`), roster reads (`GetRoster`, `ListAgentBoards`), and the comms
event stream (`SubscribeComms`). Agents write their own board through the
agent-gateway `update_board` relay arm, not a CommsService RPC.

## Requirement: An agent writes its own board with a presence-based patch

An agent writes only its own board, through the agent-gateway `update_board`
arm; a non-agent caller gets `permission_denied`. In each `upsert_tasks`
entry an unset field keeps the stored value and a set field replaces it; `""`
clears `note` or `primary_ref`, but a task's `title` is never empty. A present
`refs` or `waiting` op replaces the list, and an empty op clears it. A new task
needs a `title`, and its `state` defaults to `ACTIVE`.
`remove_task_keys` drops tasks (removing the focus task clears the focus), and
`halt` and `clear_halt` are mutually exclusive. An `automatic` patch never
refreshes the board's written turn or time; an empty one returns the board
unchanged and emits no event. A committed write emits `AgentBoardChanged` with
the authored fields only.

## Requirement: `ListAgentBoards` returns visible boards and derived refs

`ListAgentBoards` SHALL return each matching agent's whole board when the
caller can see that agent, with the matching task keys and derived refs. Each
board SHALL carry that agent's asks the caller can see: every pending ask and
asks answered in the last 24 hours, at most 20, pending asks first and each
group newest first, with each ask's forge mirror state and URL. The request's
optional handle, text, ref, waiting, state, and halt filters SHALL be combined
with AND; the default board limit is 20 and the maximum is 100.

### Scenario: A peer lists a matching board

- **Given** an agent board visible to the caller and a task matching the query
- **When** the caller lists boards with that query
- **Then** the response contains the whole board and identifies the matching
  task key.

### Scenario: A peer cannot see an ask in an invisible channel

- **Given** a caller who can see an agent but cannot see one of its ask channels
- **When** the caller lists boards
- **Then** the response contains the agent's board without that ask.

## Requirement: The caller is the connection's account, never a request field

Every `CommsService` RPC SHALL authorize against the account authenticated on
the connection, never an identity carried in the request (which would be
spoofable). On the local socket the caller is attributed to the bootstrap admin;
the network door's token interceptor sets the real caller. No request message
carries a caller identity.

## Requirement: Reads and writes are scoped to the caller's visible set

The server SHALL scope every listing, read, and search to the channels, groups,
and accounts the caller may see, enforced in the store (SQL), not at the RPC
edge. An attached channel is visible to its members and to the anchor agent's
owner set. That visibility does not grant history access: message reads, search,
topic reads, and writes require channel participation. TREE participation is the
anchor agent, its owner, and the agents in the anchor's subtree. A caller who
does not participate SHALL receive no channel history by naming its id.

### Scenario: A non-member lists a private channel's messages

- **Given** a channel the caller is not a member of
- **When** the caller calls `ListMessages` (or `SearchMessages`) naming it
- **Then** the result is empty, indistinguishable from an empty channel, rather
  than leaking the channel's messages.

## Requirement: A mutation is authorized against the caller's membership

Every mutating `CommsService` RPC SHALL authorize the caller in the store (SQL),
in the same transaction that performs the mutation, against the same visible set
that scopes a read — a caller who cannot see a channel SHALL NOT be able to
mutate it. Refusal SHALL be the not-found result, indistinguishable from a
nonexistent target, so a write probe never reveals that a private channel or
group exists. `PostMessage`, `RespondToAsk`, and `UpdateChannelMembers` SHALL
require the caller to be a member of the target channel; `OpenAgentWorkspace`
SHALL require the caller to be a member of the agent's home channel.

### Scenario: A non-member posts to a private channel

- **Given** a channel the caller is not a member of
- **When** the caller calls `PostMessage` (or `UpdateChannelMembers`) naming it
- **Then** the server returns not-found and persists nothing — the same result
  as a nonexistent channel, never a distinct not-authorized error.

## Requirement: Creating a channel is authorized against its parent group

`CreateChannel` into a group SHALL be authorized against that parent group: the
caller SHALL own the group, or be an agent whose owning user owns the group, or
the group SHALL be visible to everyone (a shared group). A group the caller
neither owns nor may see — and an unknown group — SHALL both return not-found, so
a non-owner cannot probe which group ids exist. An ungrouped channel (a DM or
group-DM, or a top-level channel) has no parent group to authorize against; the
caller is a founding member by construction.

### Scenario: A non-owner creates a channel in a private group

- **Given** an owner-visibility group the caller neither owns nor belongs to
- **When** the caller calls `CreateChannel` naming that group
- **Then** the server returns not-found, indistinguishable from a nonexistent
  group.

## Requirement: Agent tools name a channel group by leaf name or path

Agent tools SHALL never pass group ids. `create_channel` and
`create_channel_group` SHALL name the parent group as a leaf name, or as a slash
path from the root when the name contains `/`; a group name SHALL NOT contain
`/`. A leading `/` SHALL anchor the path at the top level (`/infra`), and a
leading `/~<handle>/` SHALL keep only top-level groups in that user's namespace
(`/~matt/eng`). Resolution SHALL use the caller's visible groups; unknown or
invisible groups SHALL return not-found. A ref that names more than one visible
group SHALL return invalid-argument, never pick one, and the error SHALL name
the anchored or owner-qualified ref for each match. The human RPC SHALL take ids
and reject the name fields.

A top-level group name SHALL be unique within one user's namespace. An agent's
groups belong to its owning user's namespace, so a user and that user's agents
cannot create same-named top-level groups. A nested group name SHALL be unique
under its parent, whoever creates it: a shared group has no per-user
namespaces, so a second child of the same name is a conflict.

### Scenario: An ambiguous leaf name is rejected

- **Given** two visible groups with the same leaf name
- **When** an agent tool names that group by its leaf name
- **Then** the call returns invalid-argument naming a ref for each group

### Scenario: A slash path selects one of two same-named groups

- **Given** two visible groups named `svc` under different parent groups
- **When** an agent tool names one with its root slash path
- **Then** the call resolves to the group at that path

### Scenario: An owner qualifier selects between two users' top-level groups

- **Given** the caller's own top-level group `eng` and another user's shared
  top-level group `eng`
- **When** an agent tool names `/~<other-handle>/eng`
- **Then** the call resolves to the other user's group

### Scenario: An anchor selects the top-level group over a nested one

- **Given** a top-level group `infra` and a group `infra` nested under `eng`
- **When** an agent tool names `/infra`
- **Then** the call resolves to the top-level group, and `eng/infra` resolves to
  the nested one

### Scenario: A shared group holds one child of each name

- **Given** a shared group `pub` with a child `infra` made by one user
- **When** another user creates `infra` under `pub`
- **Then** the call returns a conflict, and `pub/infra` resolves to the one
  child for both users
