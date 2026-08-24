//go:build linux

package guestd

import (
	"context"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// healthService implements the generated GuestControlHandler's single RPC,
// Health (§(e)). It is constructed only after net + mount both succeed, so its
// fields are the completed boot state — a successful handshake IS the proof of
// bringup (§(d) fail-closed invariant). The values are immutable once set at
// construction, so Health is safe to serve concurrently with no locking.
type healthService struct {
	version          string
	netProvisioned   bool
	workspaceMounted bool
}

// Health answers the host's handshake with the guest's boot state.
func (s *healthService) Health(
	_ context.Context,
	_ *connect.Request[compassv1internal.HealthRequest],
) (*connect.Response[compassv1internal.HealthResponse], error) {
	return connect.NewResponse(&compassv1internal.HealthResponse{
		GuestdVersion:    s.version,
		NetProvisioned:   s.netProvisioned,
		WorkspaceMounted: s.workspaceMounted,
	}), nil
}

// compile-time assertion that healthService satisfies the generated handler
// interface — the T2 acceptance contract.
var _ compassv1internalconnect.GuestControlHandler = (*healthService)(nil)
