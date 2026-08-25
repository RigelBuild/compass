//go:build unix

package delivery

import (
	"context"

	"github.com/RigelBuild/compass/go/internal/store"
)

// AgentWaker best-effort resumes an offline agent's session so an owed mention
// or a subscribed deliver can reach it. The server package implements it over
// the RESUME machinery (RIG-1641 T3); delivery depends only on this narrow,
// public-typed surface, mirroring comms.PresenceSource in every
// contract dimension. Nil-safe: a Consumer with no waker wired does not wake (a
// unit test with no hub still routes).
type AgentWaker interface {
	// WakeAgent best-effort resumes agent's most recent session (fresh start
	// only when none exists) so an owed mention or subscribed deliver can reach
	// it. No-op when the agent is live or has no placement. Void: a fault is
	// logged in the implementing layer, never surfaced — mention routing can
	// never fail a post (design.md established contract).
	WakeAgent(ctx context.Context, agent store.AccountID)
}
