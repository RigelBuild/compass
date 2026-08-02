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

// SignalConfigVersion pushes a ConfigVersion signal carrying the given bundle
// version to every live session bound in the hub. The whole fleet shares one
// current bundle, so any config write changes it for all live sessions; each is
// signalled to re-fetch.
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
	sessionIDs := make([]string, 0, len(h.sessionAccounts))
	for sessionID := range h.sessionAccounts {
		sessionIDs = append(sessionIDs, sessionID)
	}
	h.mu.Unlock()

	if router == nil {
		return nil
	}
	// Single-Runner MVP: every live session pushes through the same router/stream
	// (hub.go), so a push failure is stream-wide — the unpushed remainder would
	// fail identically, making this early return total by construction, not a
	// partial best-effort. A future multi-Runner change giving sessions distinct
	// routers MUST switch this to accumulate-and-continue (errors.Join) so it does
	// not silently under-notify — the same posture as SignalSecretsVersion.
	for range sessionIDs {
		cmd := &compassv1internal.SessionsResponse{
			Command: &compassv1internal.SessionsResponse_ConfigVersion{
				ConfigVersion: &compassv1internal.ConfigVersion{
					Version: version,
				},
			},
		}
		if err := router.router.push(cmd); err != nil {
			return err
		}
	}
	return nil
}
