# The event stream

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

## Requirement: `since_seq` gives a gap-free resubscribe

Each `SubscribeEventsResponse` for a positioned event SHALL carry a `seq`
strictly greater than every prior positioned event's on the stream. A subscribe
with `since_seq = 0` SHALL first snapshot the currently retained events (oldest
first) and then tail live updates; a subscribe with `since_seq = N > 0` SHALL
replay only events with `seq > N` before tailing. The replay-to-live handoff
SHALL neither drop an event nor deliver one twice.

### Scenario: A client resubscribes after a dropped connection

- **Given** a client that last received event `seq = N`, still within the
  retained window
- **When** it reconnects with `since_seq = N`
- **Then** the server replays every retained event with `seq > N` in order, then
  continues the live tail, with no gap and no duplicate at the boundary.

### Scenario: A fresh client snapshots then tails

- **Given** a running server with retained events
- **When** a client subscribes with `since_seq = 0`
- **Then** it receives the retained events oldest-first, then live updates as
  they are published.

## Requirement: An unservable cursor yields `ResyncRequired`, not a gap

When a client's `since_seq` cannot be served by a gap-free replay — it predates
the oldest retained event, or it sits at or beyond the next `seq` the server
would assign (a stale cursor from a prior server instance) — the server SHALL
send a terminal `ResyncRequired` event and close the stream rather than return a
gRPC error or silently skip events. A subscriber that falls behind the live tail
by more than the retained window SHALL likewise receive `ResyncRequired`.
`ResyncRequired` SHALL carry `seq = 0` and MUST NOT be treated as a cursor; the
client discards its cursor and reconnects with `since_seq = 0`.

### Scenario: A cursor predates the retained window

- **Given** a client reconnecting with a `since_seq` older than the oldest
  retained event
- **When** the server handles the subscribe
- **Then** it sends a single `ResyncRequired` (`seq = 0`) and closes the stream,
  and the client re-snapshots with `since_seq = 0`.

### Scenario: A live subscriber falls too far behind

- **Given** a subscribed client that stops reading and lags past the retained
  window
- **When** the server can no longer serve its position from the ring
- **Then** the server emits `ResyncRequired` and ends that stream rather than
  delivering a gap.

## Requirement: A cursor from a prior server instance forces a resync

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

### Scenario: A client reconnects with a prior instance's cursor

- **Given** a server restarted since a client last subscribed, so its
  `instance_epoch` differs from the one the client holds
- **When** the client resubscribes with `since_seq > 0` and the stale epoch
- **Then** the server answers with `ResyncRequired` and the client re-snapshots
  at `since_seq = 0`, rather than tailing below the fresh stream.
