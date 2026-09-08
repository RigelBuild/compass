// Package fabric is the NATS eventing substrate under Compass's async layer:
// one client, one connection per party, three planes.
//
// The design record is
// docs/designs/infra/runtime/compass-managed-multitenancy/design.md (§T3, §Q3,
// §T4). The two seams §T3 freezes are [EventFabric] (comms/delivery event
// fan-out) and [RunnerFabric] (Server→Runner command push and Runner→Server
// event fan-in); [RoutingFabric] (§T4 binding-cache invalidation) is the third.
// [Fabric] implements all three over a single [nats.Conn], so each Runner and
// each Server holds exactly one fabric connection.
//
// # The plane split
//
// The seams ride deliberately different NATS semantics:
//
//   - [EventFabric] rides JetStream — durable at-least-once fan-out with
//     explicit per-message acks, publish-side dedup, a bounded delivery-attempt
//     count and a dead-letter subject. Comms events must survive a subscriber
//     restart, so they need a stream.
//   - [RunnerFabric] rides core NATS — best-effort at-most-once. A command to an
//     offline Runner is not a lost write: the delivery-cursor sweep in Postgres
//     recovers it ("a fabric outage degrades to sweep-recovered delivery"), so
//     paying for a stream here would buy nothing and add a second store.
//   - [RoutingFabric] rides core NATS too, but fans out rather than
//     queue-grouping: every Server caches bindings, so every Server must see
//     every invalidation, and a dropped one degrades to a cache-miss re-read
//     against Postgres. That fan-out-and-droppable pair is why it is a third
//     seam and not a method on [EventFabric], whose read paths are all durable
//     queue-group consumers claiming each event for exactly one instance.
//
// # Postgres is the only truth
//
// JetStream is a transport, never a second store. That is why an [EventRef] is a
// compact reference — tenant, kind, row id — and never a payload copy: a
// subscriber re-reads the row from Postgres, so a dropped, replayed or
// double-delivered event reconciles against the message row and the
// per-(agent, channel) delivery cursor. Consumer state here is disposable by
// construction.
//
// # Fail-closed
//
// Every error surfaces wrapped; nothing is swallowed and nothing panics. A
// subject built from an invalid token is refused rather than silently corrupted
// (see [ValidSubjectToken]), an undecodable [EventRef] is parked on the DLQ
// rather than dropped, and a subscriber callback that panics is caught, retried
// up to Config.MaxDeliver times, then parked.
//
// Those last two are JetStream properties. On [RoutingFabric] there is nowhere
// to park — core NATS has no ack, so an undecodable payload and a panicking
// callback are both logged and DROPPED, and the lost invalidation is recovered
// by the next cache-miss re-read against Postgres. Fail-closed there means the
// receiver keeps serving from Postgres, not that the message is retried.
//
// The subject grammar and the full JetStream stream/consumer/DLQ configuration
// are specified in SUBJECTS.md beside this file — that document, not this
// package's defaults, is what later tasks build against.
package fabric
