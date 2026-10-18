# Comms stream, threads, and asks

## Requirement: The `SubscribeComms` fan-out is visibility-scoped

The comms bus fans every event to every subscriber, so the server SHALL filter
each event by the subscriber's visible set before delivering it, scoping each
event variant by the **same** predicate its corresponding read RPC uses (the
store is the one D9 source of truth, so the stream filter and the `List*` read
SHALL NOT diverge): a `MessagePosted`/`MessageUpdated` by channel membership (as
`ListMessages`); a `ChannelChanged` by channel visibility — member **or** a
shared-grouped channel — (as `ListChannels`), not bare membership, so a shared
channel's change still reaches a non-member who may see it; a
`ChannelGroupChanged` by group visibility (as `ListChannelGroups`); an
`AccountChanged` by account visibility (as `ListAccounts`); an
`AgentWorkspaceChanged` by the agent's home-channel membership; an
`AgentBoardChanged` by board visibility (as `ListAgentBoards`). A
`CommsResyncRequired` is a control frame and is always delivered. A non-visible
event is silently skipped, not a resync; a failure to resolve visibility ends
the stream rather than delivering an unfiltered event.

`AgentBoardChanged` follows the same visibility split as `ListAgentBoards`. Its
board contains authored fields only, with no asks or derived refs; clients
re-read the list RPC for those values.

Because a subscriber's live tail is filtered, it observes gaps in the bus `seq`
sequence (an event dropped for a channel it cannot see). A gap is normal: the
client SHALL treat it as such and resume from its last-seen `seq`, never as a
buffer underflow that forces a resync.

A member removed from a channel is the one exception: it SHALL receive the single
`ChannelChanged` that records its own removal (that event names the removed
accounts), after which the channel goes silent to it as a non-member. This is the
one event a departing member could not otherwise observe, since it no longer
matches the channel's post-change membership. The departing member's copy SHALL
carry only the channel id and the removed accounts, never the post-change roster,
name, or policy, so an account added in the same change is not revealed to it.

### Scenario: A departing member does not see a same-change addition

- **Given** a channel with members Owner and Y
- **When** Owner adds X and removes Y in one membership change
- **Then** Y's stream receives one final `ChannelChanged` naming Y as removed,
  whose channel carries no members, and Owner's stream receives the change with
  X in the roster.

### Scenario: A non-member does not receive a private channel's events

- **Given** two subscribers, one a member of a private channel and one not
- **When** a caller posts a message to that channel
- **Then** the member's stream receives the `MessagePosted` and the non-member's
  does not.

### Scenario: A removed member receives its removal, then silence

- **Given** a member of a channel, subscribed to `SubscribeComms`
- **When** another member removes it via `UpdateChannelMembers`
- **Then** it receives exactly one `ChannelChanged` naming it among the removed
  accounts, and no further events for that channel.

## Requirement: A mutation writes the store, then publishes its event

Each mutating RPC SHALL commit to the Postgres store first, then publish the
corresponding event (`MessagePosted`, `MessageUpdated`, `ChannelChanged`, …) on
the comms bus. A subscriber on `SubscribeComms` SHALL observe the event, and the
committed row SHALL be readable through the read RPCs — both, for one mutation.

An idempotent retry SHALL NOT re-publish. When `PostMessage` carries a
`client_request_id` that matches an already-stored message, the server returns
the stored message and publishes **no** `MessagePosted` — nothing was committed,
so no live event fires. A subscriber therefore sees one `MessagePosted` per
distinct post, never a duplicate under a client retry.

### Scenario: A post reaches a subscriber and the store

- **Given** a client subscribed to `SubscribeComms`
- **When** another caller `PostMessage`s to a channel both can see
- **Then** the subscriber receives a `MessagePosted` for it, and `ListMessages`
  returns the same message.

### Scenario: A retried post republishes nothing

- **Given** a `PostMessage` already stored under a `client_request_id`
- **When** the same caller retries `PostMessage` with that same id
- **Then** the server returns the already-stored message and publishes no second
  `MessagePosted` — the subscriber sees the post exactly once.

## Requirement: A message may thread under a parent

`PostMessage` MAY carry a `parent_message_id` naming the message this one replies
to; the server persists it and echoes it back on the stored `Message`. A root
message leaves it unset. The parent SHALL be an existing message **in the same
channel** — a parent that names no message, or one that lives in a different
channel, is rejected as an invalid argument and indistinguishably so (the
rejection never reveals whether the id exists in another channel) — so a reply
never dangles off a nonexistent id and never references a message across a
channel boundary the author may not see.

### Scenario: A reply threads under its parent

- **Given** a stored root message in a channel the caller is a member of
- **When** the caller `PostMessage`s naming that message as `parent_message_id`
- **Then** the stored reply carries that `parent_message_id`, and `ListMessages`
  returns it on the reply; a `parent_message_id` naming no message, or a message
  in a different channel, is rejected as an invalid argument

## Requirement: `SubscribeComms` mirrors the event stream's replay contract

`SubscribeComms` SHALL follow the same `seq`/`instance_epoch` replay model as
`SubscribeEvents` (see [The event stream](event-stream.md)) on its own,
independent bus instance: its own `seq` space and its own per-boot
`instance_epoch`. A cursor the ring cannot serve, or a stale `instance_epoch`
from a prior instance, SHALL yield a terminal `CommsResyncRequired`; the client
re-snapshots from the Postgres read RPCs, deduplicating by message id.

### Scenario: A client reconnects across a server restart

- **Given** a client that subscribed before the server restarted, so the running
  instance's `instance_epoch` differs from the one it holds
- **When** it resubscribes with `since_seq > 0` and the stale epoch
- **Then** the server answers `CommsResyncRequired`, and the client re-snapshots
  the messages via `ListMessages`, seeing each exactly once.

## Requirement: Answering an ask is visibility-scoped and serialized

`RespondToAsk` SHALL record the answer only when the caller is a member of the
channel carrying the ask; an ask the caller cannot see is indistinguishable from
a nonexistent one (both not-found), so ask existence never leaks across a
visibility boundary. The server SHALL validate the answer against the ask's
offered options and its single- or multi-select arity, rejecting an unoffered
option, an over-arity answer, or an empty answer. Concurrent answers to distinct
asks on one message SHALL each be recorded — neither lost — by serializing the
read-modify-write on the message row.

### Scenario: A non-member answers an ask it cannot see

- **Given** an ask in a channel the caller is not a member of
- **When** the caller calls `RespondToAsk` for it
- **Then** the server returns not-found — the same result as a nonexistent ask,
  never a distinct not-authorized error.

## Requirement: An ask may name the issue or pull request it concerns

An `Ask` MAY carry `ref`, one issue or pull request reference (`RIG-12` or
`owner/repo#12`). The server SHALL store it in canonical form (a forge ref
lowercased) and SHALL reject a `ref` that is not exactly one reference with
invalid-argument, storing nothing. The server SHALL return the stored `ref` on
every read of the ask, and `RespondToAsk` SHALL keep it on the answered ask and
on the answer's ask snapshot. The server SHALL NOT fill a default `ref`.

### Scenario: An ask ref survives the answer

- **Given** an ask posted with `ref` `RigelBuild/Compass#12`
- **When** a member answers it with `RespondToAsk`
- **Then** `ListMessages` returns the ask and the answer snapshot, each with
  `ref` `rigelbuild/compass#12`.

### Scenario: A malformed ask ref is refused

- **Given** an ask whose `ref` is `see RIG-12`
- **When** the caller posts it with `PostMessage`
- **Then** the server returns invalid-argument and the channel has no new
  message.
