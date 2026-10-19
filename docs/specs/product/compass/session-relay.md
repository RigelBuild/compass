# Session relay and observation

## Requirement: Relayed agent events publish onto the event stream, Runner-sequenced

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

## Requirement: Runner relays classify session binding failures consistently

Every Runner-originated agent relay SHALL reject an unbound or foreign-Runner
session with the same `FAILED_PRECONDITION` status and error message, so those
sessions are indistinguishable. A binding that cannot be verified SHALL return
`UNAVAILABLE` so the caller can retry. A missing relay target remains
`NOT_FOUND`; invalid Runner credentials remain `UNAUTHENTICATED`.

### Scenario: Unknown and foreign sessions do not reveal liveness

- **Given** an unknown session id and a live session owned by another Runner
- **When** the Runner calls an agent relay with each id
- **Then** both calls return byte-identical `FAILED_PRECONDITION` errors, and
  neither call is delegated to an agent service.

### Scenario: A binding read fault can be retried

- **Given** a Runner relay whose session binding cannot be verified
- **When** the relay handles the call
- **Then** it returns `UNAVAILABLE` and does not delegate the call.

## Requirement: Session blob storage failures remain distinct from binding failures

Missing blob object storage SHALL return `UNIMPLEMENTED` from blob upload and
resume fetches. Binding `FAILED_PRECONDITION` does not confirm that a referenced
blob is absent: resume handling SHALL preserve the original image reference.
The agent uploader SHALL disable itself only after `UNIMPLEMENTED`; other
upload errors remain retryable on a later transcript line.

## Requirement: A session's observation pane is an authorized live tail

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

> **Implementation status (RIG-1342):** this increment ships the authorized live
> tail only. A dropped-for-lag subscriber ends like any other clean stream end;
> the reattach/resync machinery that would let a client recover the frames it
> missed (the deferred daemon-lifecycle work) is not yet built, so lag recovery
> is a fresh `SubscribeAgentSession` from the current tail.

### Scenario: A non-member cannot tell a private session from a missing one

- **Given** a live agent session whose owning agent's home channel the caller is
  not a member of
- **When** the caller subscribes with that session id, and separately with a
  session id that does not exist
- **Then** the server rejects both with the identical not-found, so neither
  response reveals whether the session exists.

### Scenario: An authorized member tails a live session

- **Given** a caller that is a member of the session's owning home channel
- **When** it subscribes and the session emits a frame
- **Then** the server streams that frame to the caller, and ends the stream
  cleanly when the client disconnects.

## Requirement: Only an owner or admin may skip a session's batch window

The server SHALL expose `SkipBatchWindow`, a unary RPC taking a session id and
relaying a `start_now` control to the owning Runner. The control fires the
agent's pending idle batch immediately; if no batch window is open, it does
nothing. A server with no Runner seam SHALL fail with `Unavailable`. A call with
no authenticated caller SHALL fail with `Unauthenticated`; an empty session id
SHALL fail with `InvalidArgument`. Only the session owner or an admin SHALL be
admitted; channel membership alone SHALL NOT authorize the call. An unknown
session and a session the caller may not control SHALL both fail with the
identical `NotFound`. A control dispatch failure SHALL fail with `Unavailable`.

### Scenario: The owner skips the open batch window

- **Given** an authenticated owner of a live agent session with an open batch
  window
- **When** the owner calls `SkipBatchWindow` for that session
- **Then** the server relays `start_now` to the owning Runner and the pending
  batch fires immediately.

### Scenario: A non-owner member cannot distinguish a missing session

- **Given** a caller who is a member of the session's home channel but is
  neither its owner nor an admin
- **When** the caller calls `SkipBatchWindow` for that session and for a session
  id that does not exist
- **Then** both calls fail with the identical `NotFound`.

## Requirement: An open batch window reaches the observation pane

When batching is on, the agent holds inbound items in an idle window so a burst
lands in one turn. The agent SHALL emit a `SessionBatchPending` session event
each time the window arms or re-arms, and when it fires or is cancelled. `count`
SHALL be the number of items waiting and `fires_at_unix_ms` the time the window
fires. A window that fires or is cancelled while the agent runs SHALL emit
`count` 0 and `fires_at_unix_ms` 0. When the agent ends, its terminal session
status closes the window instead, and no count-0 event is sent. The event SHALL
travel the session-event path to `SubscribeAgentSession` subscribers, like any
other relayed session event. It is live-only, so a subscriber that joins
mid-window sees no count until the next arm or close.

### Scenario: A burst arms, re-arms, then fires the window

- **Given** an idle agent with batching on, and a subscriber tailing its
  session
- **When** two items arrive within the window, and the window then fires
- **Then** the subscriber receives `SessionBatchPending` events with counts 1,
  2, then 0, and each nonzero event carries the time the window fires.
