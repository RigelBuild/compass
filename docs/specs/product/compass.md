# Compass

Living source-of-truth for how **Compass** currently behaves and is built. The
point-in-time design rationale — the ADE vision (the Dispatcher, the Bridge,
per-agent containers, the three-tier Client→Server→Runner architecture) — lives
in the design corpus, indexed by the decision ledger
([`../../designs/product/DECISIONS.md`](../../designs/product/DECISIONS.md)) with
the surviving milestone rationale in the
[architecture lineage](../../designs/product/compass-architecture-lineage/design.md)
record; this spec describes only what the code exposes today.

Interface- and security-critical behavior is stated as `### Requirement:` +
`#### Scenario:` contracts (RFC 2119 SHALL/MUST, Given/When/Then). Prose frames
the model.

## Overview

Compass is an Agentic Development Environment (ADE) — a persistent local
**server** (`compass-server`, Go) fronting a web **UI**. The server owns the
backend; the UI is a client. The two meet at a single, owned contract:
**`compass.v1`**, a gRPC service the server serves over its local transport.

What is built today is the contract seam and the server's transport:
`compass-server` serves the `compass.v1` service over a Unix domain socket, and
a generated client is the only sanctioned way to reach it. The agent runtime
(a first-party in-container agent built on the Oh My Pi SDK, emitting
`compass.v1` natively, hosted on a per-workstream **Runner**), the Cotal
coordination substrate, and the Bridge UI are designed but not yet fully built;
the server
currently reports its liveness, provisions agent workstreams, and pushes a
server-status event stream, and the web UI is a walking skeleton. The sections
below describe the serving surface as it stands.

## The `compass.v1` contract

`compass.v1` is the owned door between any UI and the server: gRPC services
served over Connect so a native client (gRPC over the local transport) and a
browser (gRPC-Web) share them. It is versioned in the package name (`compass.v1`)
so a future schema generation doesn't retrofit a version segment. Two services
live in the package: **`CompassService`** (server liveness, the event stream, and
the agent-session lifecycle) and **`CommsService`** (the communication layer —
accounts, channels, messages, and their event stream), described in turn below.

`CompassService` exposes:

- **`GetServerInfo`** — a unary liveness probe returning the server's build and
  contract version. The first round-trip a UI makes after connecting.
- **`SubscribeEvents`** — a server-streaming event channel, the sole push path
  from the server to the UI (see [The event stream](#the-event-stream) for its
  ordering and resubscribe semantics).
- **`ProvisionAgentWorkspace`** — creates the isolated per-agent container for a
  workstream, routing Client → Server → RunnerHub → Runner (the server holds no
  container-engine code; see [Agent sessions](#agent-sessions)).
- **`StartAgentSession` / `StopAgentSession` / `ReloadAgentSession` /
  `GetAgentStatus`** — the agent-session lifecycle surface (see
  [Agent sessions](#agent-sessions)): bring the first-party in-container agent
  online, stop it, reload it in place, and query status.
- **`IssueToken`** — an admin-only RPC minting a bearer token for an existing
  account (the sole public-contract path to a non-bootstrap account's
  credential). Its admin-gated enforcement and token semantics are served by the
  network door (see [The network door](#the-network-door)).

The event payload is an extensible `oneof` — `ServerStatus` and `ResyncRequired`
plus the agent-session payloads (`AgentSessionStatus`, `AgentMessageChunk`,
`AgentToolCall`, `AgentPlan`); board and audit variants are added as backward-
compatible additions behind the breaking-change gate.

### Requirement: The server serves the `compass.v1` service surface

The server SHALL implement the `compass.v1` `CompassService` with exactly the
RPCs the schema declares. `GetServerInfo` SHALL return the server's semantic
`version` and the `api_version` string identifying the contract it serves
(`compass.v1`). `SubscribeEvents` SHALL be the only server-streaming RPC and the
only path by which the server pushes state to a UI.

#### Scenario: A UI probes a freshly connected server

- **Given** a running server reachable over its local transport
- **When** a client calls `GetServerInfo`
- **Then** the server returns its build `version` and `api_version = "compass.v1"`.

### Requirement: A generated client is the only door to the server

Client code SHALL reach the server only through the generated `compass.v1`
clients — the TypeScript client (`@compass/client`) for the UI, and the Go
client the backend generates — never a raw gRPC stub or hand-written socket
call. The frontend lint configuration SHALL fail a build that imports the raw
transport outside the generated-client package.

#### Scenario: UI code imports the raw transport directly

- **Given** UI code that imports the raw Connect transport instead of
  `@compass/client`
- **When** the lint gate runs
- **Then** it fails, naming the owned-door rule, so only the generated client
  reaches the server.

### Requirement: The checked-in clients cannot drift from the schema

The generated clients are checked in and CI-verified against the schema: the
build SHALL fail if regenerating from the `.proto` produces output that differs
from the committed generated code, if the schema fails its lint, or if a schema
edit is a breaking change to the published contract.

#### Scenario: A schema edit lands without regenerating the clients

- **Given** an edit to the `compass.v1` schema with the checked-in generated
  clients left stale
- **When** the contract-drift gate runs (regenerate, then diff against the
  committed clients)
- **Then** it fails on the difference, so the schema and its clients can never
  silently disagree.

## Local transport

Native clients — any native gRPC client, and the designed desktop shell's
webview — reach `compass-server` through the `compass.v1` gRPC service over a
**Unix domain socket**. The socket is the shipped door: it serves native gRPC
(HTTP/2) and gRPC-Web (HTTP/1.1) off one listener, so no localhost TCP port is
exposed in production on macOS or native Linux. A browser cannot dial a Unix
socket, so browser-based development instead uses the opt-in, off-by-default
loopback TCP endpoint ([Development endpoint](#development-endpoint)); the
designed desktop shell ([Not yet specified](#not-yet-specified)) will bridge its
webview to the socket over the shell's own IPC.

The socket path defaults to `$XDG_RUNTIME_DIR/compass/server.sock`, falling
back to `$HOME/.compass/server.sock`, and is overridable with `--socket`.

### Requirement: The Unix socket serves native gRPC and gRPC-Web on one listener

The server SHALL serve the `compass.v1` service over the Unix domain socket to
both native gRPC (HTTP/2) clients and gRPC-Web (HTTP/1.1) clients, from a single
listener, without an external proxy. Native gRPC requests SHALL be served
unchanged; only gRPC-Web requests are translated.

#### Scenario: A native gRPC client calls over the socket

- **Given** a running server bound to its Unix socket
- **When** a native gRPC (HTTP/2) client calls `GetServerInfo` over the socket
- **Then** the server returns its version and the `compass.v1` api-version.

#### Scenario: A gRPC-Web client calls over the same socket

- **Given** a running server bound to its Unix socket
- **When** a gRPC-Web (HTTP/1.1) client issues the same call over the socket
- **Then** the server answers it over gRPC-Web without a separate port or proxy.

### Requirement: The shipped path exposes no localhost TCP port

On macOS and native Linux the server SHALL NOT open any TCP listener as part of
its normal (shipped) operation; the Unix socket is the only transport. A TCP
listener is opened only when a dev endpoint is explicitly requested (below).

#### Scenario: Default startup opens no TCP port

- **Given** `compass-server` started without `--dev-http`
- **When** it reaches its serving state
- **Then** it listens only on the Unix domain socket and binds no TCP port.

### Requirement: The socket is owner-only

The server SHALL restrict the socket so only its owner can connect: any parent
directory the server creates for the socket SHALL be created mode `0700`, and the
socket file SHALL be set to mode `0600`. A pre-existing parent directory's mode
is left unchanged.

#### Scenario: The server creates the socket's parent directory

- **Given** a socket path whose parent directory does not yet exist
- **When** the server creates that directory and binds the socket
- **Then** the created directory is mode `0700` and the socket file is mode `0600`.

## Single-instance startup

### Requirement: The server refuses to displace a live server

On startup the server SHALL probe the socket path. It SHALL refuse to start
(returning an error) if a live server already answers there, and SHALL refuse a
path that exists but is not a socket. It SHALL clear only a genuinely stale
socket (one whose owner is gone) before binding.

#### Scenario: A second server starts on a live socket

- **Given** a server already serving on a socket path
- **When** a second `compass-server` is started on the same path
- **Then** the second refuses to start, names the already-running server, and
  leaves the live socket intact.

#### Scenario: A stale socket file remains from a dead server

- **Given** a socket file on disk whose server is no longer running
- **When** a new server starts on that path
- **Then** it clears the stale socket and binds successfully.

## Serving lifecycle

### Requirement: Clean shutdown drains streams and removes the socket

On its shutdown signal the server SHALL stop accepting connections, wake every
open `SubscribeEvents` stream so in-flight streams end and graceful drain
completes, and then remove the socket file — but only if the on-disk socket is
still the inode it bound (so it never deletes a successor server's socket).

#### Scenario: Shutdown with a subscriber stream held open

- **Given** a client holding a `SubscribeEvents` stream open
- **When** the server receives its shutdown signal
- **Then** `Serve` returns without error and removes the socket, rather than
  hanging on the held-open stream.

### Requirement: Startup failures surface promptly

The server SHALL bind its listeners before entering its serving state, so a bind
failure fails startup up front rather than after the server reports itself
serving. If a server task exits on its own (an error or panic) while serving, the
server SHALL tear down its peer server and propagate the failure rather than
leaving it unobserved until an external shutdown.

#### Scenario: The requested dev port is already in use

- **Given** `--dev-http` names a TCP address already bound by another listener
- **When** the server starts
- **Then** startup fails promptly with an error naming the dev-endpoint bind
  failure, and the server does not report itself serving.

## The event stream

`SubscribeEvents` is the server's one ordered push channel. Behind it is a
retained-event ring buffer plus a broadcast live-tail, so the stream's
gap-free-resubscribe contract has a single owner. Every event the server assigns
a **strictly monotonic `seq`** and an event time; the client passes the last
`seq` it saw back as `since_seq` to resume without gaps. The server first
publishes a `ServerStatus` when it binds, so a fresh subscriber immediately
learns the server is ready.

The ring retains a bounded window of recent events (1024). A subscriber that
falls further behind than that window — or reconnects with a cursor the ring can
no longer serve — recovers by re-snapshotting from `since_seq = 0`.

### Requirement: `since_seq` gives a gap-free resubscribe

Each `SubscribeEventsResponse` for a positioned event SHALL carry a `seq`
strictly greater than every prior positioned event's on the stream. A subscribe
with `since_seq = 0` SHALL first snapshot the currently retained events (oldest
first) and then tail live updates; a subscribe with `since_seq = N > 0` SHALL
replay only events with `seq > N` before tailing. The replay-to-live handoff
SHALL neither drop an event nor deliver one twice.

#### Scenario: A client resubscribes after a dropped connection

- **Given** a client that last received event `seq = N`, still within the
  retained window
- **When** it reconnects with `since_seq = N`
- **Then** the server replays every retained event with `seq > N` in order, then
  continues the live tail, with no gap and no duplicate at the boundary.

#### Scenario: A fresh client snapshots then tails

- **Given** a running server with retained events
- **When** a client subscribes with `since_seq = 0`
- **Then** it receives the retained events oldest-first, then live updates as
  they are published.

### Requirement: An unservable cursor yields `ResyncRequired`, not a gap

When a client's `since_seq` cannot be served by a gap-free replay — it predates
the oldest retained event, or it sits at or beyond the next `seq` the server
would assign (a stale cursor from a prior server instance) — the server SHALL
send a terminal `ResyncRequired` event and close the stream rather than return a
gRPC error or silently skip events. A subscriber that falls behind the live tail
by more than the retained window SHALL likewise receive `ResyncRequired`.
`ResyncRequired` SHALL carry `seq = 0` and MUST NOT be treated as a cursor; the
client discards its cursor and reconnects with `since_seq = 0`.

#### Scenario: A cursor predates the retained window

- **Given** a client reconnecting with a `since_seq` older than the oldest
  retained event
- **When** the server handles the subscribe
- **Then** it sends a single `ResyncRequired` (`seq = 0`) and closes the stream,
  and the client re-snapshots with `since_seq = 0`.

#### Scenario: A live subscriber falls too far behind

- **Given** a subscribed client that stops reading and lags past the retained
  window
- **When** the server can no longer serve its position from the ring
- **Then** the server emits `ResyncRequired` and ends that stream rather than
  delivering a gap.

### Requirement: A cursor from a prior server instance forces a resync

The server's `seq` space resets each boot, so a numeric cursor alone cannot prove
it belongs to the running instance — a stale cursor still within the fresh ring's
range would otherwise tail below the new stream. The server SHALL mint a per-boot
`instance_epoch` at startup that differs from prior instances' with negligible
collision odds, and SHALL stamp it on every `SubscribeEventsResponse`. On a
`SubscribeEvents` request with `since_seq > 0`, the server SHALL require the
request's `instance_epoch` to equal the running instance's; on any mismatch —
including an absent epoch (`0`), as sent by a client predating this field — it
SHALL answer with a terminal `ResyncRequired` rather than replaying. A
`since_seq = 0` snapshot request SHALL be served regardless of the epoch given.

#### Scenario: A client reconnects with a prior instance's cursor

- **Given** a server restarted since a client last subscribed, so its
  `instance_epoch` differs from the one the client holds
- **When** the client resubscribes with `since_seq > 0` and the stale epoch
- **Then** the server answers with `ResyncRequired` and the client re-snapshots
  at `since_seq = 0`, rather than tailing below the fresh stream.

## Agent sessions

An agent runs as a **first-party program built on the Oh My Pi SDK** inside its
own container (the per-agent isolation substrate), emitting `compass.v1` frames
natively — there is no [Agent Client Protocol](https://agentclientprotocol.com/)
anywhere in the system, and no translator. The agent is spawned on the container
host — the **Runner** — over a long-lived streaming exec, and its
newline-delimited `compass.v1` stream is relayed up to the server, which fans it
out to clients. One container holds one agent, driven by one session.

The topology is **Client → Server → Runner**: the server holds no
container-engine code; the Runner owns every container operation and dials out to
the server (a NAT-friendly outbound connection, like a CI runner). Container
lifecycle commands flow Client → Server → RunnerHub → Runner over the internal
Runner seam; agent frames flow Runner → Server → Client.

### Requirement: Provisioning a workstream creates its isolated container

The server SHALL expose `ProvisionAgentWorkspace` — agent ref + repo/workstream
spec in, a stable `container_name` out. It routes the request Client → Server →
RunnerHub → Runner, where the Runner drives the built lifecycle façade to create
the isolated per-agent container; the server itself runs no container-engine
code. Provisioning and starting are **separate** operations: a container can
exist idle before a session runs in it, so `ProvisionAgentWorkspace` and
`StartAgentSession` stay distinct RPCs. When the request carries a
`client_request_id`, it is an idempotency key: a timeout-retry with the same id
SHALL return the same `container_name` rather than creating a second container.

#### Scenario: A retried provision creates no duplicate container

- **Given** a provision request carrying a `client_request_id`
- **When** the client's first call times out and it retries with the same id
- **Then** the server returns the same `container_name` and the Runner has
  created exactly one container.

### Requirement: The lifecycle RPCs manage one agent session per container

The server SHALL expose `StartAgentSession`, `StopAgentSession`,
`ReloadAgentSession`, and `GetAgentStatus`, each routing Client → Server →
RunnerHub → Runner. `StartAgentSession` SHALL bring the first-party agent in a
provisioned container online — spawning it over the streaming-exec bridge — and
return a server-assigned session id. `StopAgentSession` SHALL deliberately
terminate the in-container agent and release the session, and SHALL succeed for
an unknown or already-stopped session (idempotent teardown). `GetAgentStatus`
SHALL return one session's status, or every live session's when no id is given,
reconciled to the owning Runner's authoritative session set.

#### Scenario: Starting an agent session streams its status

- **Given** a provisioned agent container with no live session
- **When** a client calls `StartAgentSession` for it
- **Then** the server brings the first-party agent online and publishes an
  `AgentSessionStatus` transition to `STARTING` then `READY` on the event
  stream, and returns the session id.

### Requirement: One live session per container; replacing goes through reload

A container holds at most one live session. `StartAgentSession` for a container
that already has a live session SHALL be rejected (the existing session is
unaffected); replacing a live session SHALL go through `ReloadAgentSession`.
Reload is defined as *teardown-then-fresh-start against the same container*: the
server stops the current agent and session, then starts a new one under the
**same** session id (so the id stays a stable handle) and the agent reloads from
its workspace state.

### Requirement: One container per agent account

The server SHALL bind at most one live container to a given agent account. A
`SpawnAgent` for an agent account that already holds a live session SHALL be
rejected with `ALREADY_EXISTS` before any container is provisioned (the
existing session is unaffected). This binds the reject-on-live rule to the
*agent account*, not merely to the container name: the existing
one-session-per-container requirement above coincides with it today only
because the container name is derived from the account, an incidental property
a future Runner naming containers differently would break.

### Requirement: A deliberate stop is distinct from an unexpected exit or a lost Runner

The server SHALL distinguish three session-ending conditions. A deliberate
stop/reload SHALL NOT be reported as `ERRORED`. An unexpected agent exit (a
crash, out-of-memory, or engine restart) SHALL transition the session to
`ERRORED` on the event stream and SHALL NOT be auto-reconnected; recovery is an
explicit `ReloadAgentSession`. A lost **Runner link** is distinct from both: the
session SHALL transition to `DISCONNECTED` — live session truth is temporarily
unreachable but not lost — and a bounded reattach window SHALL govern recovery.
A reattach within the window resumes the session; window expiry falls to
`ERRORED`, after which the no-auto-reconnect policy applies.

#### Scenario: A Runner disconnect moves its sessions to DISCONNECTED, not ERRORED

- **Given** a Runner with a live agent session
- **When** the Runner's link to the server drops
- **Then** the session transitions to `DISCONNECTED` on the event stream, and a
  reattach within the bounded window resumes it rather than ending it as
  `ERRORED`.

> **Implementation status (SEA-1243 T4):** T4 ships the `DISCONNECTED` state on
> the contract and the disconnect *signal* — a lost Runner link fails the
> session's in-flight commands. The server-side **reattach-window enforcement**
> — the per-session registry that publishes `DISCONNECTED` on link loss, the
> bounded timer, the expiry→`ERRORED` transition, and `GetAgentStatus`
> reconciliation to the Runner's set on reattach — is **T9**
> (`docs/designs/platform/go-toolchain-default.md`:979). Until T9 lands, the
> window/expiry state machine in this Requirement is not yet enforced.

### Requirement: Relayed agent events publish onto the event stream, Runner-sequenced

The first-party agent emits `compass.v1` frames natively; the owning Runner
relays them to the server over the internal Runner seam, carrying a
**Runner-assigned sequence** so the server detects an in-transit gap. The server
SHALL publish each relayed event onto `SubscribeEvents` with the monotonic `seq`
the bus assigns (distinct from the Runner sequence): the agent's message and
thought output as `AgentMessageChunk`, its tool calls as `AgentToolCall`, and its
plan updates as `AgentPlan`, each carrying the session id so a UI attributes
activity to the right agent; a lifecycle transition publishes as
`AgentSessionStatus`. A relayed frame whose variant is unset or unrecognized
SHALL be logged and counted rather than silently discarded, so a new agent
capability surfaces in diagnostics.

### Requirement: A session's observation pane is an authorized live tail

The server SHALL expose `SubscribeAgentSession` — a server-streaming RPC taking
a session id and streaming that session's `AgentSessionFrame`s to the caller.
Before any frame is sent, the server SHALL authorize the caller against the
session's **durable ownership chain**: it resolves `session_id` →
`container_name` → `agent_account_id` → the agent's `home_channel_id` and admits
the caller only if it is a member of that home channel. An unknown session and a
session the caller may not see SHALL be reported identically — the same
not-found — so a caller cannot probe which sessions exist. A call carrying no
authenticated caller SHALL fail closed as unauthenticated (a door that attaches
no caller identity is a wiring bug, never an open door), and a call to a server
with no Runner seam SHALL be `Unavailable`.

Past the gate the stream is a **live tail**: the server forwards frames as the
session emits them and ends the stream cleanly — never as an error — when the
client hangs up, the session ends, or the subscriber falls too far behind its
buffer. The pane carries no snapshot replay; a subscriber sees only frames from
the point it joined.

> **Implementation status (SEA-1342):** this increment ships the authorized live
> tail only. A dropped-for-lag subscriber ends like any other clean stream end;
> the reattach/resync machinery that would let a client recover the frames it
> missed (the deferred daemon-lifecycle work) is not yet built, so lag recovery
> is a fresh `SubscribeAgentSession` from the current tail.

#### Scenario: A non-member cannot tell a private session from a missing one

- **Given** a live agent session whose owning agent's home channel the caller is
  not a member of
- **When** the caller subscribes with that session id, and separately with a
  session id that does not exist
- **Then** the server rejects both with the identical not-found, so neither
  response reveals whether the session exists.

#### Scenario: An authorized member tails a live session

- **Given** a caller that is a member of the session's owning home channel
- **When** it subscribes and the session emits a frame
- **Then** the server streams that frame to the caller, and ends the stream
  cleanly when the client disconnects.

## The communication layer

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
`SearchMessages`), and the comms event stream (`SubscribeComms`).

### Requirement: The caller is the connection's account, never a request field

Every `CommsService` RPC SHALL authorize against the account authenticated on
the connection, never an identity carried in the request (which would be
spoofable). On the local socket the caller is attributed to the bootstrap admin;
the network door's token interceptor sets the real caller. No request message
carries a caller identity.

### Requirement: Reads and writes are scoped to the caller's visible set

The server SHALL scope every listing, read, and search to the channels, groups,
and accounts the caller may see, enforced in the store (SQL), not at the RPC
edge. A message read for a channel the caller is not a member of SHALL return
nothing rather than the channel's contents — a non-member cannot read a private
channel's history by naming its id.

#### Scenario: A non-member lists a private channel's messages

- **Given** a channel the caller is not a member of
- **When** the caller calls `ListMessages` (or `SearchMessages`) naming it
- **Then** the result is empty, indistinguishable from an empty channel, rather
  than leaking the channel's messages.

### Requirement: A mutation is authorized against the caller's membership

Every mutating `CommsService` RPC SHALL authorize the caller in the store (SQL),
in the same transaction that performs the mutation, against the same visible set
that scopes a read — a caller who cannot see a channel SHALL NOT be able to
mutate it. Refusal SHALL be the not-found result, indistinguishable from a
nonexistent target, so a write probe never reveals that a private channel or
group exists. `PostMessage`, `RespondToAsk`, and `UpdateChannelMembers` SHALL
require the caller to be a member of the target channel; `OpenAgentWorkspace`
SHALL require the caller to be a member of the agent's home channel.

#### Scenario: A non-member posts to a private channel

- **Given** a channel the caller is not a member of
- **When** the caller calls `PostMessage` (or `UpdateChannelMembers`) naming it
- **Then** the server returns not-found and persists nothing — the same result
  as a nonexistent channel, never a distinct not-authorized error.

### Requirement: Creating a channel is authorized against its parent group

`CreateChannel` into a group SHALL be authorized against that parent group: the
caller SHALL own the group, or be an agent whose owning user owns the group, or
the group SHALL be visible to everyone (a shared group). A group the caller
neither owns nor may see — and an unknown group — SHALL both return not-found, so
a non-owner cannot probe which group ids exist. An ungrouped channel (a DM or
group-DM, or a top-level channel) has no parent group to authorize against; the
caller is a founding member by construction.

#### Scenario: A non-owner creates a channel in a private group

- **Given** an owner-visibility group the caller neither owns nor belongs to
- **When** the caller calls `CreateChannel` naming that group
- **Then** the server returns not-found, indistinguishable from a nonexistent
  group.

### Requirement: The `SubscribeComms` fan-out is visibility-scoped

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
`AgentWorkspaceChanged` by the agent's home-channel membership. A
`CommsResyncRequired` is a control frame and is always delivered. A non-visible
event is silently skipped, not a resync; a failure to resolve visibility ends
the stream rather than delivering an unfiltered event.

Because a subscriber's live tail is filtered, it observes gaps in the bus `seq`
sequence (an event dropped for a channel it cannot see). A gap is normal: the
client SHALL treat it as such and resume from its last-seen `seq`, never as a
buffer underflow that forces a resync.

A member removed from a channel is the one exception: it SHALL receive the single
`ChannelChanged` that records its own removal (that event names the removed
accounts), after which the channel goes silent to it as a non-member. This is the
one event a departing member could not otherwise observe, since it no longer
matches the channel's post-change membership.

#### Scenario: A non-member does not receive a private channel's events

- **Given** two subscribers, one a member of a private channel and one not
- **When** a caller posts a message to that channel
- **Then** the member's stream receives the `MessagePosted` and the non-member's
  does not.

#### Scenario: A removed member receives its removal, then silence

- **Given** a member of a channel, subscribed to `SubscribeComms`
- **When** another member removes it via `UpdateChannelMembers`
- **Then** it receives exactly one `ChannelChanged` naming it among the removed
  accounts, and no further events for that channel.

### Requirement: A mutation writes the store, then publishes its event

Each mutating RPC SHALL commit to the Postgres store first, then publish the
corresponding event (`MessagePosted`, `MessageUpdated`, `ChannelChanged`, …) on
the comms bus. A subscriber on `SubscribeComms` SHALL observe the event, and the
committed row SHALL be readable through the read RPCs — both, for one mutation.

An idempotent retry SHALL NOT re-publish. When `PostMessage` carries a
`client_request_id` that matches an already-stored message, the server returns
the stored message and publishes **no** `MessagePosted` — nothing was committed,
so no live event fires. A subscriber therefore sees one `MessagePosted` per
distinct post, never a duplicate under a client retry.

#### Scenario: A post reaches a subscriber and the store

- **Given** a client subscribed to `SubscribeComms`
- **When** another caller `PostMessage`s to a channel both can see
- **Then** the subscriber receives a `MessagePosted` for it, and `ListMessages`
  returns the same message.

#### Scenario: A retried post republishes nothing

- **Given** a `PostMessage` already stored under a `client_request_id`
- **When** the same caller retries `PostMessage` with that same id
- **Then** the server returns the already-stored message and publishes no second
  `MessagePosted` — the subscriber sees the post exactly once.

### Requirement: A message may thread under a parent

`PostMessage` MAY carry a `parent_message_id` naming the message this one replies
to; the server persists it and echoes it back on the stored `Message`. A root
message leaves it unset. The parent SHALL be an existing message **in the same
channel** — a parent that names no message, or one that lives in a different
channel, is rejected as an invalid argument and indistinguishably so (the
rejection never reveals whether the id exists in another channel) — so a reply
never dangles off a nonexistent id and never references a message across a
channel boundary the author may not see.

#### Scenario: A reply threads under its parent

- **Given** a stored root message in a channel the caller is a member of
- **When** the caller `PostMessage`s naming that message as `parent_message_id`
- **Then** the stored reply carries that `parent_message_id`, and `ListMessages`
  returns it on the reply; a `parent_message_id` naming no message, or a message
  in a different channel, is rejected as an invalid argument

### Requirement: `SubscribeComms` mirrors the event stream's replay contract

`SubscribeComms` SHALL follow the same `seq`/`instance_epoch` replay model as
`SubscribeEvents` (see [The event stream](#the-event-stream)) on its own,
independent bus instance: its own `seq` space and its own per-boot
`instance_epoch`. A cursor the ring cannot serve, or a stale `instance_epoch`
from a prior instance, SHALL yield a terminal `CommsResyncRequired`; the client
re-snapshots from the Postgres read RPCs, deduplicating by message id.

#### Scenario: A client reconnects across a server restart

- **Given** a client that subscribed before the server restarted, so the running
  instance's `instance_epoch` differs from the one it holds
- **When** it resubscribes with `since_seq > 0` and the stale epoch
- **Then** the server answers `CommsResyncRequired`, and the client re-snapshots
  the messages via `ListMessages`, seeing each exactly once.

### Requirement: Answering an ask is visibility-scoped and serialized

`RespondToAsk` SHALL record the answer only when the caller is a member of the
channel carrying the ask; an ask the caller cannot see is indistinguishable from
a nonexistent one (both not-found), so ask existence never leaks across a
visibility boundary. The server SHALL validate the answer against the ask's
offered options and its single- or multi-select arity, rejecting an unoffered
option, an over-arity answer, or an empty answer. Concurrent answers to distinct
asks on one message SHALL each be recorded — neither lost — by serializing the
read-modify-write on the message row.

#### Scenario: A non-member answers an ask it cannot see

- **Given** an ask in a channel the caller is not a member of
- **When** the caller calls `RespondToAsk` for it
- **Then** the server returns not-found — the same result as a nonexistent ask,
  never a distinct not-authorized error.

### Requirement: Channel membership carries a separate subscribe opt-in

`UpdateChannelMembers` SHALL manage both the channel's member set and each
member's subscribe flag. Joining a channel grants read access (the member
appears in the channel's members); subscribing is a separate per-member opt-in
(the member additionally appears in the channel's subscribers). One RPC covers
join, subscribe-toggle, and removal.

One comms behavior is designed but **not yet built**, so it is out of scope for
this spec until it lands (behind its own design amendment): **reserved-ping
resolution** (`@agents`/`@users`/`@everyone` expanding to a member set) — no ping
contract exists yet.

The `snapshot_seq` **consistent point-in-time snapshot** across a paginated
re-snapshot is **partially built**: the server half has landed (SEA-1333) — the
`SubscribeComms` `since_seq=0` response carries a leading `snapshot_seq` boundary
frame and the message read RPCs enforce it as `seq <= snapshot_seq`. The
normative Requirement (the boundary-frame contract + narrowing this reserved
slot) is deliberately held until the client-consumption seam is confirmed, so
the spec is not flipped to "built" mid-seam.

## Development endpoint

A browser cannot dial a Unix socket, so local UI development against a real
browser uses an opt-in loopback TCP endpoint. It is a development affordance, not
part of the shipped surface.

### Requirement: The dev endpoint is opt-in and loopback-only

The server SHALL serve a gRPC-Web endpoint over TCP only when `--dev-http` is
given. The address MUST be a loopback address (`127.0.0.1` or `::1`); a
non-loopback address SHALL be rejected. The rejection SHALL be enforced by the
server itself, not only by the CLI, since the serving entry point is an exported
package function and the endpoint is unauthenticated with permissive CORS.

#### Scenario: A non-loopback dev address is rejected

- **Given** `compass-server --dev-http` with a routable (non-loopback) address
- **When** it starts
- **Then** it exits with an error naming the loopback requirement and binds
  nothing.

### Requirement: The dev endpoint applies gRPC-Web CORS

When serving the dev endpoint the server SHALL apply a CORS layer that exposes
the gRPC-Web status trailers (`grpc-status`, `grpc-message`,
`grpc-status-details-bin`) so a browser gRPC-Web client can read call status. The
Unix socket SHALL NOT apply CORS — it is a same-origin surface (the shell's
webview or a native client).

### Requirement: The dev endpoint denies admin-only RPCs

The dev endpoint mounts no bearer interceptor, so it never carries an
authenticated caller. Its admin-only RPCs (`IssueToken` and the agent-session
lifecycle RPCs) SHALL therefore fail closed `PermissionDenied`, while the
authenticated-open RPCs (`GetServerInfo`, `SubscribeEvents`) and the
ambient-admin comms RPCs remain reachable. This prevents a browser page loaded
against a configured dev endpoint from minting a bootstrap-admin token via
`IssueToken` and replaying it against the network door.

#### Scenario: `IssueToken` on the dev endpoint is denied

- **Given** a server started with `--dev-http`
- **When** a client calls `IssueToken` over the dev endpoint
- **Then** the server rejects it `PermissionDenied` and mints no token, while
  `GetServerInfo` and a comms read over the same endpoint succeed.

## The network door

A distinct authenticated door for clients that reach the server over an
untrusted network rather than the local Unix socket. Unlike the socket (whose
`0600` mode is its whole credential) and the dev endpoint (loopback, ambient
identity, no bearer auth — so its admin-only RPCs are denied), the network door
is the internet-facing surface: it terminates TLS, authenticates every RPC with
a bearer token, and gates the admin-only RPCs. It is off by default and opened
only with `--listen`.

### Requirement: The network door requires TLS

The server SHALL open the network door only when `--listen` is given, and only
with a TLS certificate and key (`--tls-cert` / `--tls-key`). A bearer token over
cleartext is credential disclosure, so `--listen` without both SHALL be refused
up front — enforced by the server itself, not only the CLI, since the serving
entry point is an exported package function. The door SHALL require a minimum of
TLS 1.3.

#### Scenario: `--listen` without a keypair is rejected

- **Given** `compass-server --listen` with a TCP address but no `--tls-cert` /
  `--tls-key`
- **When** it starts
- **Then** it exits with an error naming the TLS requirement and binds nothing.

### Requirement: The network door authenticates a bearer token and gates admin RPCs

The server SHALL authenticate every network-door RPC with a bearer token: a
request with no token or an unresolvable token SHALL be rejected
`Unauthenticated`, on both unary and streaming RPCs. The admin-only RPCs
(`IssueToken` and the agent-session lifecycle RPCs) SHALL additionally require
the caller to resolve to the admin account, rejecting a non-admin
`PermissionDenied`; the authenticated-open RPCs (`GetServerInfo`,
`SubscribeEvents`) SHALL admit any valid bearer.

#### Scenario: A token-less call on the network door is rejected

- **Given** a running network door
- **When** a client calls any RPC with no bearer token
- **Then** the server rejects it `Unauthenticated` before the handler runs.

#### Scenario: A non-admin token calls an admin-only RPC

- **Given** a valid non-admin bearer token
- **When** the client calls `IssueToken` over the network door
- **Then** the server rejects it `PermissionDenied`, while the same token on
  `GetServerInfo` succeeds.

### Requirement: The network door mints and persists a bootstrap admin token

When the network door is opened, the server SHALL mint a bootstrap admin bearer
token and write it to a file under the state directory at mode `0600`, so an
operator can authenticate the first admin client. The token file SHALL be
written atomically (a temp file `0600`, synced, then renamed into place; the
temp file removed on any error), and the token itself SHALL NOT be logged — only
its path. A socket-only start (no `--listen`) SHALL write no token file.

#### Scenario: Opening the network door writes the admin token 0600

- **Given** `compass-server --listen` with a valid keypair and a state directory
- **When** the server reaches its serving state
- **Then** the admin-token file exists at mode `0600`, authenticates as the
  admin account, and the token value never appears in the server's logs.

### Requirement: The network door defaults closed to browser origins

The network door SHALL apply no CORS policy unless `--cors-allowed-origin` names
a single explicit browser origin; when set, the door SHALL allow exactly that
one origin (never a wildcard), admit the `Authorization` request header, and
SHALL NOT enable credentialed (cookie) CORS. A preflight from any other origin
SHALL NOT be reflected.

#### Scenario: A foreign origin preflight is not reflected

- **Given** a network door started with `--cors-allowed-origin` naming one origin
- **When** a CORS preflight arrives from a different origin
- **Then** the server does not reflect it, and the configured origin's preflight
  is answered with exactly that origin (never `*`) and without credentialed CORS.

## Not yet specified

These surfaces are designed (see the design record) but not yet implemented, and
are out of scope for this spec until they land:

- **Dispatcher and board/audit events** — the agent session layer above plugs a
  Dispatcher MCP endpoint into a named seam, but its logic is a separate issue;
  the board and audit event payloads are likewise unbuilt. The Bridge UI that
  consumes the agent event stream is out of scope.
- **Desktop shell (Tauri)** — a thin shell bridging the UI webview to the
  server's Unix socket over its own IPC (a browser cannot dial a Unix socket),
  streaming gRPC-Web responses back as ordered byte frames with no localhost TCP
  port. Designed in the records; the shell is not built in the current backend.
- **Windows / WSL2** — a token-guarded loopback (or named-pipe) transport, since
  AF_UNIX sockets do not cross the WSL2 boundary.
- **Concurrent-start locking** — an exclusive start-lock closing the
  probe→bind race between two servers starting at once.
