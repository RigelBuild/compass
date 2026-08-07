//go:build podman

package e2e

import "time"

// rpcTimeout bounds a single authed RPC over the loopback door: generous for a
// local call, short enough that a wedged connection fails visibly rather than
// hanging the test. Deterministic per-call deadline, never a retry loop.
const rpcTimeout = 30 * time.Second

// settleTimeout bounds AwaitSessionSettled: a real agent turn can run well past
// a single RPC's rpcTimeout (model round-trips, tool calls), so this is
// deliberately generous — but finite, so a session that never reaches READY
// fails visibly here instead of blocking to the go-test timeout. A deterministic
// deadline, never a retry loop.
const settleTimeout = 2 * time.Minute

// deliverTimeout bounds AwaitDelivery: a post's fan onto a subscription is a
// single bus round-trip, but the observed post may trail a multi-turn agent
// scenario (a scripted spawn + settle before the @mention post), so this is
// generous relative to rpcTimeout — but finite, so a subscription that never
// carries the matching MessagePosted fails visibly here instead of blocking to
// the go-test timeout. A deterministic deadline, never a retry loop.
const deliverTimeout = 1 * time.Minute
