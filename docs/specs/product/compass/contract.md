# The `compass.v1` contract

`compass.v1` is the owned door between any UI and the server: gRPC services
served over Connect so a native client (gRPC over the local transport) and a
browser (gRPC-Web) share them. It is versioned in the package name (`compass.v1`)
so a future schema generation doesn't retrofit a version segment. Two services
live in the package: **`CompassService`** (server liveness, the event stream, and
the agent-session lifecycle) and **`CommsService`** (the communication layer —
accounts, channels, messages, and their event stream), described in turn below.

`CompassService` exposes:

- **`GetServerInfo`** — a unary liveness probe returning the server's build and
  contract version and the most recently enrolled Runner with its per-server
  enrollment sequence. The first round-trip a UI makes after connecting.
- **`SubscribeEvents`** — a server-streaming event channel, the sole push path
  from the server to the UI (see [The event stream](event-stream.md) for its
  ordering and resubscribe semantics).
- **`ProvisionAgentWorkspace`** — creates the isolated per-agent container for a
  workstream, routing Client → Server → RunnerHub → Runner (the server holds no
  container-engine code; see [Agent sessions](agent-sessions.md)).
- **`StartAgentSession` / `StopAgentSession` / `ReloadAgentSession` /
  `GetAgentStatus`** — the agent-session lifecycle surface (see
  [Agent sessions](agent-sessions.md)): bring the first-party in-container agent
  online, stop it, reload it in place, and query status.
- **`SkipBatchWindow`** — asks the owning Runner to start the agent's pending
  idle batch immediately (see [Session relay and observation](session-relay.md)).
- **`IssueToken`** — an admin-only RPC minting a bearer token for an existing
  account (the sole public-contract path to a non-bootstrap account's
  credential). Its admin-gated enforcement and token semantics are served by the
  network door (see [The network door](network-door.md)).
- **`RenameAccount`** — an admin-only RPC that renames a user or agent in place.
  Agents keep their owner; a freed handle may be reclaimed.
- **`GetRefLinkRules`** — the link rules a UI uses to turn issue and PR
  references in text into links, built from the server's configured forges.

The event payload is an extensible `oneof` — `ServerStatus` and `ResyncRequired`
plus the agent-session payloads (`AgentSessionStatus`, `AgentMessageChunk`,
`AgentToolCall`, `AgentPlan`); board and audit variants are added as backward-
compatible additions behind the breaking-change gate.

## Requirement: The server serves the `compass.v1` service surface

The server SHALL implement the `compass.v1` `CompassService` with exactly the
RPCs the schema declares. `GetServerInfo` SHALL return the server's semantic
`version`, the `api_version` string identifying the contract it serves
(`compass.v1`), the `rev` (full git commit) the binary was built from, empty
when the build was not stamped, and the most recently enrolled Runner id and its
per-server enrollment sequence. The reported Runner remains present across
disconnects; the sequence increases on every enrollment so clients can detect a
fresh enroll. The list is empty before the first enrollment and when no Runner
door is mounted. `SubscribeEvents` SHALL be the only server-streaming RPC and
the only path by which the server pushes state to a UI.

### Scenario: A UI probes a freshly connected server

- **Given** a running server reachable over its local transport
- **When** a client calls `GetServerInfo`
- **Then** the server returns its build `version`, `api_version = "compass.v1"`,
  and no enrolled Runner records before a Runner enrolls.

### Scenario: A client probes after a Runner enrolls

- **Given** a Runner has enrolled with the server
- **When** a client calls `GetServerInfo`
- **Then** the server includes the Runner id and its per-server enrollment
  sequence in `enrolled_runners`, retaining that record across disconnects.

### Scenario: A client detects a fresh Runner enrollment

- **Given** a Runner has previously enrolled with the server
- **When** the Runner enrolls again and a client calls `GetServerInfo`
- **Then** the server reports the same Runner id with a higher per-server
  enrollment sequence.

## Requirement: An admin renames a user or agent account in place

`RenameAccount` SHALL resolve a bare user handle or an owner-qualified agent
handle without viewer-scoped filtering. It SHALL rename the account's resolution
handle and display copy in one store transaction. Same-owner DMs, agent home
channels, and coordination channels SHALL be renamed in that transaction while
their channel ids and message history remain unchanged. The new handle MUST be
valid and bare; an agent's owner does not change. Unknown accounts return
`NotFound`, invalid input returns `InvalidArgument`, and a collision in the account's handle tier returns `AlreadyExists`.
The system account cannot be renamed. The bootstrap admin and its root
supervisor return `FailedPrecondition` and remain unchanged. The response
returns the account id and its current address, qualified by the owner's current
handle for agents. A freed handle is immediately available for reuse.

### Scenario: Rename an account at either handle tier

- **Given** an existing user or an agent addressed as `owner/agent`
- **When** an admin renames it to a valid bare handle
- **Then** both handle records change together, same-owner DM names change
  without moving messages, the old handle becomes available, and the response
  returns the new address

### Scenario: Reject a rename that cannot be applied

- **Given** an admin rename request with an unknown account, invalid new handle,
  or a handle already taken in that tier
- **When** the server handles it
- **Then** it returns `NotFound`, `InvalidArgument`, or `AlreadyExists` as
  appropriate and leaves both stored handles unchanged

### Scenario: Protect the bootstrap admin and root supervisor

- **Given** the bootstrap admin or its root supervisor
- **When** an admin requests a rename
- **Then** the server returns `FailedPrecondition` and retains the current handle

### Scenario: The admin cannot rename the system account

- **Given** the system account
- **When** an admin requests a rename
- **Then** the server returns `PermissionDenied` and retains its handle

### Scenario: Account rename publishes updated account and DM state

- **Given** an account with a same-owner DM and a subscriber entitled to its
  account and channel changes
- **When** an admin renames the account
- **Then** the subscriber receives `AccountChanged` with the new handle and
  `ChannelChanged` for each renamed same-owner DM, home, or coordination channel;
  each channel retains its id and message history

## Requirement: A generated client is the only door to the server

Client code SHALL reach the server only through the generated `compass.v1`
clients — the TypeScript client (`@compass/client`) for the UI, and the Go
client the backend generates — never a raw gRPC stub or hand-written socket
call. The frontend lint configuration SHALL fail a build that imports the raw
transport outside the generated-client package.

### Scenario: UI code imports the raw transport directly

- **Given** UI code that imports the raw Connect transport instead of
  `@compass/client`
- **When** the lint gate runs
- **Then** it fails, naming the owned-door rule, so only the generated client
  reaches the server.

## Requirement: The checked-in clients cannot drift from the schema

The generated clients are checked in and CI-verified against the schema: the
build SHALL fail if regenerating from the `.proto` produces output that differs
from the committed generated code, if the schema fails its lint, or if a schema
edit is a breaking change to the published contract.

### Scenario: A schema edit lands without regenerating the clients

- **Given** an edit to the `compass.v1` schema with the checked-in generated
  clients left stale
- **When** the contract-drift gate runs (regenerate, then diff against the
  committed clients)
- **Then** it fails on the difference, so the schema and its clients can never
  silently disagree.

## Requirement: Ref link rules come from the configured forges

`GetRefLinkRules` SHALL return one GitHub rule per configured GitHub host whose
App the server reads with, and one Linear rule per enabled Linear team, keyed by
the team key and the Linear workspace URL key. A rule SHALL carry only its kind,
prefix, and URL template; it SHALL NOT carry a token, secret, or installation
identifier. When the Linear workspace key or the enabled-team list cannot be
read, the server SHALL omit the Linear rules and still return the GitHub rules.

### Scenario: A UI loads the link rules

- **Given** a server with a configured GitHub App and an enabled Linear team
- **When** a client calls `GetRefLinkRules`
- **Then** the server returns a GitHub rule for the App host and a Linear rule
  for the team, each holding only a kind, prefix, and URL template.

### Scenario: The Linear workspace key read fails

- **Given** a server with a configured GitHub App whose Linear workspace key
  read fails
- **When** a client calls `GetRefLinkRules`
- **Then** the server returns the GitHub rules and no Linear rule.
