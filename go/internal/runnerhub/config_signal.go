//go:build unix

// The ConfigVersion emit seam: a signal-only Server->Runner push telling a live
// session that the fleet config bundle changed, so the Runner re-fetches via
// FetchAgentConfig, re-materializes the new version dir, and Reloads live agents.
// Like the SecretsVersion signal (secrets_signal.go) and unlike the command relay
// (commands.go), this registers no pendingCall and waits for no result —
// ConfigVersion has no result variant on the Runner's request stream (runner.proto:
// SessionsResponse.command tag 9). The CompassService config write handlers
// (server/service.go) call SignalConfigVersion after a successful Put/Delete.
package runnerhub

import (
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
)

// SignalConfigVersion pushes ONE ConfigVersion frame per attached Runner stream,
// carrying the given bundle version. The signal is fleet-wide — it carries no
// per-account/session key (record §563) — so a single push on the stream
// notifies the whole fleet to re-fetch. Single-Runner MVP: one router = one push.
//
// SEMANTIC DIFFERENCE from SignalSecretsVersion: the config version is NOT a
// freshly-minted opaque counter — it is the STORE's canonical content version
// (a sha256 content hash), passed in by the caller. PutAgentConfig emits the new
// bundle's version; DeleteAgentConfig emits the empty string, which tells the
// Runner the fleet was cleared to no config (re-fetch materializes an empty dir).
// A content hash is safe to expose here (unlike SecretsVersion): the config
// bundle is credential-free by rule, so the version carries no secret entropy —
// exactly why it can be the content hash and needs no mint.
//
// Best-effort and fire-and-forget: a session whose Runner stream is momentarily
// detached is a no-op (commandRouter.push), because the Runner re-fetches its
// config on reconnect regardless — a dropped signal costs nothing permanent. No
// Runner enrolled, or no live sessions, is a clean no-op success.
func (h *Hub) SignalConfigVersion(version string) error {
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
	// ConfigVersion is fleet-wide (record §527-528, §563), so ONE push per distinct
	// attached router carries it to the whole fleet on that stream; single-Runner
	// MVP = one router = one push. The !hasLiveSessions gate above keeps a Runner
	// with no live sessions a clean no-op (nothing to reload; it reconciles
	// via the version-only fetch on next Sessions (re)establishment, record
	// §677-685). A future multi-Runner change giving sessions distinct routers MUST
	// push once PER DISTINCT ROUTER and accumulate-and-continue (errors.Join)
	// across routers so a wedged stream does not silently under-notify the rest of
	// the fleet — the per-ROUTER analogue of SignalSecretsVersion's per-session loop.
	cmd := &compassv1internal.SessionsResponse{
		Command: &compassv1internal.SessionsResponse_ConfigVersion{
			ConfigVersion: &compassv1internal.ConfigVersion{
				Version: version,
			},
		},
	}
	return router.router.push(cmd)
}
