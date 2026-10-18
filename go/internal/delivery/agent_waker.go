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
	// WakeAgent resumes an offline agent so owed work can reach it; implementations log failures.
	// Delivery never fails a post because wake is best-effort.
	WakeAgent(ctx context.Context, agent store.AccountID)
}
