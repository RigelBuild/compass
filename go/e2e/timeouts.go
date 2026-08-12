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

// enrollPollInterval and enrollPollBudget bound the runner-enrollment readiness
// poll NewFixture runs after stack.Up returns. Up returns as soon as the
// compass-runner CHILD is spawned (stack spawnChain step 7), but the runner
// enrolls ASYNCHRONOUSLY — it dials the server over the TLS door and enrolls
// after Up has already returned — so a leg that Provisions immediately races
// that enrollment. These bound the enrollment counterpart to the stack's own
// waitReady/waitPostgres poll: enrollment is a fast one-time transition (a
// single server dial once the server is already answering), so the budget is
// far smaller than readyPollBudget while still failing a genuinely wedged
// enrollment legibly rather than hanging to the go-test timeout; the interval
// matches readyPollInterval's magnitude. A deterministic deadline, never a
// retry loop.
const (
	enrollPollInterval = 100 * time.Millisecond
	enrollPollBudget   = 15 * time.Second
)
