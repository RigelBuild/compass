//go:build unix

// The ConfigVersion emit seam: a signal-only Server->Runner push telling a live
// session the fleet config bundle changed, so the Runner re-fetches and Reloads
// live agents. Like the SecretsVersion signal and unlike the command relay, this
// registers no pendingCall and waits for no result.
package runnerhub

import (
	"context"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
)

// SignalConfigChanged nudges an attached Runner to re-fetch the fleet config.
// The frame has no payload, and a dropped nudge is harmless because the Runner
// re-fetches on reconnect. No Runner or no live sessions is a clean no-op.
func (h *Hub) SignalConfigChanged(ctx context.Context) error {
	h.mu.Lock()
	router := h.runner
	hasLiveSessions := len(h.sessionAccounts) > 0
	h.mu.Unlock()

	if router == nil {
		return nil
	}
	if !hasLiveSessions {
		return nil
	}
	// ConfigVersion is fleet-wide, so ONE push per attached router carries it to
	// the whole fleet (single-Runner MVP = one router = one push). A future
	// multi-Runner change MUST push once PER ROUTER and accumulate-and-continue
	// (errors.Join) so a wedged stream does not silently under-notify the fleet.
	cmd := &compassv1internal.SessionsResponse{
		Traceparent: otelx.Traceparent(ctx),
		Command: &compassv1internal.SessionsResponse_ConfigVersion{
			ConfigVersion: &compassv1internal.ConfigVersion{},
		},
	}
	return router.router.push(ctx, cmd)
}
