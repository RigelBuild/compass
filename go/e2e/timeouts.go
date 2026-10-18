//go:build podman

package e2e

import "time"

// rpcTimeout bounds a single authed RPC over the loopback door: generous for a
// local call, short enough that a wedged connection fails visibly rather than
// hanging the test. Deterministic per-call deadline, never a retry loop.
const rpcTimeout = 30 * time.Second

// settleTimeout allows a real agent turn to span model and tool round-trips,
// while still failing visibly before the suite timeout.
const settleTimeout = 2 * time.Minute

// deliverTimeout allows for multi-turn scenarios before the matching event,
// but remains finite so a missing delivery fails before the suite timeout.
const deliverTimeout = 1 * time.Minute

// enrollPollInterval and enrollPollBudget bound waitRunnerEnrolled's Sessions
// stream attach check, which follows the cold Up enrollment gate.
const (
	enrollPollInterval = 100 * time.Millisecond
	enrollPollBudget   = 15 * time.Second
)

// seedSettlePollInterval and seedSettlePollBudget bound NewFixture's wait for
// the root supervisor's placement. This serializes seed Provision and leg
// Provision calls, which would otherwise contend on rootless-podman storage.
// Match the server's seed timeout and fail closed if placement never appears.
const (
	seedSettlePollInterval = 100 * time.Millisecond
	seedSettlePollBudget   = 2 * time.Minute
)
