// Package fabric is the NATS eventing substrate under Compass's async layer:
// one client, one connection per party, two planes.
//
// The design record is
// docs/designs/infra/runtime/compass-managed-multitenancy/design.md (§T3, §Q3).
// The two seams it freezes are [EventFabric] (comms/delivery event fan-out) and
// [RunnerFabric] (Server→Runner command push and Runner→Server event fan-in);
// [Fabric] implements both over a single [nats.Conn], so each Runner and each
// Server holds exactly one fabric connection.
//
// # The plane split
//
// The two seams ride deliberately different NATS semantics:
//
//   - [EventFabric] rides JetStream — durable at-least-once fan-out with
//     explicit per-message acks, publish-side dedup, a bounded delivery-attempt
//     count and a dead-letter subject. Comms events must survive a subscriber
//     restart, so they need a stream.
//   - [RunnerFabric] rides core NATS — best-effort at-most-once. A command to an
//     offline Runner is not a lost write: the delivery-cursor sweep in Postgres
//     recovers it ("a fabric outage degrades to sweep-recovered delivery"), so
//     paying for a stream here would buy nothing and add a second store.
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
// The subject grammar and the full JetStream stream/consumer/DLQ configuration
// are specified in SUBJECTS.md beside this file — that document, not this
// package's defaults, is what later tasks build against.
package fabric
