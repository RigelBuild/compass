# Fabric subjects and JetStream configuration

The written spec for the NATS eventing substrate: the four subject grammars, the
dead-letter subject, and the JetStream stream/consumer configuration. Frozen by
`docs/designs/infra/runtime/compass-managed-multitenancy/design.md` §T3/§Q3;
this file is the operational restatement that later tasks build against, and
`package fabric` is its only implementation.

## Subject grammar

| Grammar | Plane | Builder | Direction |
| --- | --- | --- | --- |
| `compass.<tenant>.comms.<kind>` | JetStream | `CommsSubject(tenant, kind)` | Server → Servers (comms/delivery fan-out) |
| `compass.*.comms.<kind>` | JetStream | `CommsWildcardSubject(kind)` | Servers → one delivery consumer (cross-tenant fan-in, **subscribe-side only**) |
| `compass.runner.<runner_id>.cmd` | core NATS | `RunnerCommandSubject(runnerID)` | Server → one Runner (async command push) |
| `compass.runner.events` | core NATS, queue group `compass-runner-events` | `RunnerEventsSubject()` | Runners → exactly one Server (event fan-in) |
| `client.<sessionID>` | core NATS | `ClientSubject(sessionID)` | Server → one live client connection |
| `compass.dlq.comms` | core NATS | `DLQSubject` | fabric → operator (parked events) |

`client.<sessionID>` sits outside the `compass.` root deliberately — the frozen
grammar names it that way, and it must not be captured by the comms stream's
subject wildcard.

### The tenant-wildcard subscribe: `compass.*.comms.<kind>`

Publish is always per-tenant and concrete. The read side has a second entry
point, `EventFabric.SubscribeKind(ctx, kind, fn)`, which subscribes on
`compass.*.comms.<kind>` — one kind, every tenant. The T3 delivery consumer is
a per-Server **singleton** serving all tenants, so a per-tenant subscribe would
need one consumer per tenant created at tenant-creation time; the wildcard gives
it one durable queue-group consumer instead, and tenant creation stays a
Postgres insert.

- **The wildcard is on the tenant token only.** The kind stays concrete and is
  validated by `ValidSubjectToken`. A wildcard kind would put all seven comms
  kinds on the delivery consumer, waking it (and its Postgres re-read) for every
  unrelated write.
- **No stream-config change.** `Subjects` is already `compass.*.comms.*`, which
  captures this subject by construction; JetStream accepts a wildcard
  `FilterSubject` on a durable consumer.
- **Its own durable consumer.** `Durable` is `comms-` + sha256(subject), so the
  wildcard subject hashes to a name distinct from every concrete-tenant
  consumer. Shared and durable as usual: each matching event is claimed by
  exactly one Server instance. Wildcard and concrete consumers on the same kind
  are independent durables, so an event matching both is delivered once to each;
  a migration introducing `SubscribeKind` must retire the concrete subscribes
  rather than double-handle events.
- **`Subscribe` stays concrete-only.** `validCommsSubject` still rejects a `*`
  token, so the wildcard is reachable only through `SubscribeKind`'s own
  validated builder — a caller cannot hand-write a cross-tenant subject.
- **Publish cannot target it.** `Publish` derives its subject from the ref via
  `CommsSubject`, and `EventRef.valid` rejects a `*` tenant, so a wildcard
  publish is impossible rather than merely discouraged.

### Token validation: reject, never sanitize

NATS reserves `.` (token separator), `*` and `>` (wildcards), and rejects
whitespace in a subject token. Tenant ids, runner ids and session ids are opaque
to the fabric, so `ValidSubjectToken` **refuses** a token carrying any of those
rather than escaping it. Escaping would need an unambiguous inverse the grammar
does not have, and a silently-rewritten token would publish a tenant's events to
a subject nobody is subscribed to — a routing bug masquerading as a quiet
success. An id with a reserved character is a bug where the id is minted.

### Event kinds

The `<kind>` token is one of the seven comms kinds (`EventKind` in
`eventref.go`), all snake_case so each is a legal single token:
`account_changed`, `channel_group_changed`, `channel_changed`,
`agent_workspace_changed`, `message_posted`, `message_updated`,
`topic_upserted`.

### Payload: a reference, never a copy

Every comms subject carries a JSON-encoded `EventRef` — `{tenant, kind, row_id}`
— and never the changed row. Postgres is the sole durability truth; the
subscriber re-reads the row the ref names. That is what makes a replay or a
double delivery idempotent and a drop recoverable from the delivery cursor.

## JetStream: stream `COMPASS_COMMS`

Created idempotently with `CreateOrUpdateStream`, so a restart, a second Server
and a config change converge instead of racing.

| Field | Value | Why |
| --- | --- | --- |
| `Name` | `COMPASS_COMMS` | One stream for every tenant; the consumer's subject filter isolates tenants, so tenant creation stays a Postgres insert rather than a JetStream admin op. |
| `Subjects` | `compass.*.comms.*` | Exactly the four-token comms grammar, tenant and kind wildcarded. |
| `Retention` | limits | A message ages out on `MaxAge` rather than vanishing on ack, so a second consumer group and a bounded replay stay possible. |
| `Storage` | file | Durability across a NATS restart (§Q3). |
| `Replicas` | 1 (3 clustered) | Single-node NATS is R1 by construction; §Q3 specifies R3 when clustered. Postgres is the recovery truth either way. |
| `Discard` | old | At the age/size limit, drop the oldest rather than refusing new publishes — a refused publish would fail a live comms write for the sake of a transport's backlog. |
| `MaxAge` | 24h | Bounds the replay window. A subscriber further behind than this recovers by cursor sweep, not replay. |
| `Duplicates` | 2m | Publish-side dedup window (see MsgID below). |

### `sync_interval: 100ms` — set on the server, not the stream

§Q3 specifies `sync_interval: 100ms` for a bounded fsync window (the December
2025 Jepsen analysis documented ~14% acknowledged-write loss under NATS
defaults). **It is not a stream field.** In nats-server it is a file-store
option, and `jetstream.StreamConfig` in nats.go v1.53.1 exposes no equivalent —
so it cannot be set from this package. It is configured on the NATS process:

- stack/server config: `jetstream { store_dir: "…", sync_interval: "100ms" }`
- Go-embedded or in-process (this package's tests):
  `server.Options{SyncInterval: 100 * time.Millisecond}`

The stack's NATS service config owns the deployment value; `testServer` in
`fabric_test.go` sets the `server.Options` field so the tests run at the
record's value rather than the server default.

### MsgID / dedup

`Publish` sets `Nats-Msg-Id` to `sha256(len:tenant | len:kind | len:row_id)`
(`EventRef.msgID`). Deterministic in the ref's three fields, so two Servers
publishing the same logical change — or one retrying a publish whose ack was
lost — collapse to one stored message within the `Duplicates` window. Hashed and
length-prefixed rather than concatenated so no field boundary is ambiguous and
a long row id does not widen the header.

## JetStream: the per-subject consumer

One durable pull consumer per subscribed subject, created with
`CreateOrUpdateConsumer`.

| Field | Value | Why |
| --- | --- | --- |
| `Durable` | `comms-` + sha256(subject) as untruncated hex (e.g. `comms-f48b3059…e25211` for `compass.tenant-a.comms.message_posted`) | Consumer names cannot contain `.`, and subject tokens may legally contain `_` (every snake_case EventKind does), so a `.` → `_` substitution is NOT injective: two distinct subjects would collapse onto one shared durable consumer and the second Subscribe would silently re-point the first's `FilterSubject` (a cross-tenant mis-delivery). Hashing is injective by construction; 70 chars is far inside JetStream's 255-char limit, and truncating would reintroduce the collision surface. Durable and shared, so every Server instance on that subject draws from one consumer: each event is claimed by exactly one instance (§Q3 queue groups), and a restart resumes rather than replaying. The consumer's `Description`/`FilterSubject` still carry the readable subject for operators. |
| `FilterSubject` | the subscribed subject | The tenant/kind isolation the single stream relies on. |
| `AckPolicy` | explicit | §Q3: explicit per-message acks. |
| `AckWait` | 30s | Redelivery backstop for a subscriber that hangs or dies mid-callback; a callback that *fails* is Nak'd for immediate redelivery instead. |
| `MaxDeliver` | 5 | Finite budget of total delivery **attempts**, not retries: `MaxDeliver=1` parks on the first failure with no retry at all. Enforced twice by design — the app-level check parks at the budget, and the consumer's server-side `MaxDeliver` is the backstop for a delivery whose metadata is unreadable. Both derive from the SAME fabric `Config`, and the consumer is shared, so every Server instance on a subject must run one config (RIG-2861: one stack config) or the shared consumer's server-side budget flip-flops with whichever instance last ran `CreateOrUpdateConsumer`. |
| `Replicas` | matches the stream | — |

Delivery semantics per message:

1. Decode the `EventRef`. Undecodable → **park immediately** (no number of
   redeliveries changes the bytes).
2. Run the subscriber callback under a panic guard. A panic becomes a failure —
   it neither takes the process down nor acks an event nobody handled.
3. Success → `Ack()`. A *failed ack* after successful handling is logged, never
   parked: it costs one redelivery, which the subscriber's Postgres re-read makes
   idempotent.
4. Failure → read `Metadata().NumDelivered`, which counts **attempts**. Below
   `MaxDeliver` → `Nak()` for immediate redelivery. At `MaxDeliver` → park.

## Dead-letter: `compass.dlq.comms`

JetStream has no native DLQ, so the fabric implements the app-level pattern: on
park, republish the raw payload to `compass.dlq.comms` and then
`TermWithReason` the message so the server stops redelivering it.

- The republish goes over **core NATS**, not JetStream. The DLQ is a diagnostic
  tap, not a recovery path — recovery always terminates in the Postgres row — and
  a DLQ publish that needed a stream would need a DLQ of its own.
- `Term` is issued **even if the DLQ publish fails**, with both failures logged:
  a poison message redelivering forever is the worse outcome.
- Headers on the parked message: `Compass-Original-Subject` (the concrete
  subject the message was delivered on, even for a wildcard (`SubscribeKind`)
  consumer, so it always names the tenant) and `Compass-Park-Reason` (the
  error), so an operator reading the DLQ needs no log correlation.

The attempt count comes from the message's server-side metadata rather than any
local counter, which is what makes the budget hold across Server instances and
restarts.
