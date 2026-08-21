//go:build unix

// The SecretsVersion emit seam: a signal-only Server->Runner push telling a live
// session that its declared secret set changed, so the Runner re-fetches via
// FetchSecrets. Unlike the command relay (commands.go), this registers no
// pendingCall and waits for no result — SecretsVersion has no result variant on
// the Runner's request stream (runner.proto: SessionsResponse.command tag 8).
// The SecretsService write handlers (server/secrets_service.go) call
// SignalSecretsVersion after a successful Set/Delete.
package runnerhub

import (
	"strconv"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// mintSecretsVersion returns the next opaque per-Server monotonic set-change
// token as a decimal string.
//
// SECURITY INVARIANT — the token is an OPAQUE MONOTONIC SET-CHANGE token, NOT
// value-derived and NOT a content-hash of the secret set. Its ONLY job is to say
// "re-fetch now"; it carries no entropy about any secret's value. A content-hash
// token would gratuitously mint a confirmation oracle: an observer of the signal
// could confirm a guessed secret value by matching the hash. The oracle-sensitive
// per-value diff already lives downstream on ResolvedSecret.version (a content
// hash, redacted for exactly that reason — secrets.go String/GoString omit it);
// duplicating that hash here for zero benefit would reopen it on the un-redacted
// signal path. So this stays a monotonic counter, deliberately un-redacted.
// (Frozen invariant, driver-committed: runner.proto:213-220 SecretsVersion doc,
// design record §916-918. Do NOT "optimize" this into a hash.)
func (h *Hub) mintSecretsVersion() string {
	return strconv.FormatUint(h.secretsVersion.Add(1), 10)
}

// SignalSecretsVersion mints one fresh set-change token and pushes a
// SecretsVersion signal to every live session bound in the hub. The whole
// declared set is injected into every container (inject-all MVP), so any registry
// write changes the set for all live sessions; each is signalled to re-fetch.
//
// Best-effort and fire-and-forget: a session whose Runner stream is momentarily
// detached is a no-op (commandRouter.push), because the Runner re-fetches its set
// on reconnect regardless — a dropped signal costs nothing permanent. No Runner
// enrolled, or no live sessions, is a clean no-op success (nothing to notify).
//
// One token per call (not per session): the token is a per-Server monotonic
// counter, so a single write mints one version that every notified session sees,
// and the NEXT write mints a strictly greater one — the monotonicity the T6
// rotation diff relies on.
func (h *Hub) SignalSecretsVersion() error {
	version := h.mintSecretsVersion()

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
	// not silently under-notify.
	for _, sessionID := range sessionIDs {
		cmd := &compassv1internal.SessionsResponse{
			Command: &compassv1internal.SessionsResponse_SecretsVersion{
				SecretsVersion: &compassv1internal.SecretsVersion{
					SessionId: sessionID,
					Version:   version,
				},
			},
		}
		if err := router.router.push(cmd); err != nil {
			return err
		}
	}
	return nil
}
