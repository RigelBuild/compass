// Package comms implements the compass.v1 CommsService over the Postgres store
// of record (T1) and the generic event bus. It is the communication-layer door:
// accounts, channel groups + channels, messages, agent workspaces, and the
// comms event stream.
//
// The handler is a thin shell. It holds no state of its own — the store is the
// single source of truth and enforces D9 visibility server-side in SQL, so every
// read passes the caller as the visibility scope and every write authorizes
// against it. The handler's job at each RPC is threefold: map the wire request
// onto the store's domain types, call the store, and (for a mutation) publish the
// corresponding event onto the comms bus after the commit — write-through
// fan-out, so a subscriber sees a change only once it is durable.
//
// The comms bus is a second instance of the generic events.Bus, distinct from
// CompassService's SubscribeEvents bus: its own seq space and its own per-boot
// instance_epoch. The two streams share the implementation, not the instance.
//
// Files:
//   - comms.go — the Comms handler: NewComms, the actor seam, and the 13
//     non-stream RPCs (SubscribeComms lives in subscribe.go).
//   - subscribe.go — SubscribeComms: ring snapshot then live tail, mirroring
//     CompassService.SubscribeEvents.
//   - mapping.go — proto <-> store mapping at the edge, and the write-through
//     event publishers.
//   - context.go — the authenticated-caller context seam and the store-error to
//     connect-code edge map.
package comms
