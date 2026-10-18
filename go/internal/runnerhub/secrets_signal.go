//go:build unix

// The SecretsVersion emit seam: a signal-only Server->Runner push telling a live
// session its declared secret set changed, so the Runner re-fetches via
// FetchSecrets. Unlike the command relay, this registers no pendingCall and waits
// for no result. Called by the SecretsService write handlers after a Set/Delete.
package runnerhub

import (
	"context"
	"strconv"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/store"
)

// mintSecretsVersion returns an opaque monotonic set-change token, not a value-derived hash.
// Hashes can let observers confirm guessed secrets; the per-secret version remains the only content hash.
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
func (h *Hub) SignalSecretsVersion(ctx context.Context) error {
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
	tp := otelx.Traceparent(ctx)
	// Single-Runner MVP: every live session pushes through the same router/stream,
	// so a push failure is stream-wide and this early return is total, not partial.
	// A future multi-Runner change giving sessions distinct routers MUST switch to
	// accumulate-and-continue (errors.Join) so it does not silently under-notify.
	for _, sessionID := range sessionIDs {
		cmd := &compassv1internal.SessionsResponse{
			Traceparent: tp,
			Command: &compassv1internal.SessionsResponse_SecretsVersion{
				SecretsVersion: &compassv1internal.SecretsVersion{
					SessionId: sessionID,
					Version:   version,
				},
			},
		}
		if err := router.router.push(ctx, cmd); err != nil {
			return err
		}
	}
	return nil
}

// SignalSecretsVersionFor pushes one SecretsVersion to account's live session.
// No live session, or no Runner, is a nil no-op.
func (h *Hub) SignalSecretsVersionFor(ctx context.Context, account store.AccountID) error {
	sessionID, ok := h.SessionForAccount(ctx, account)
	if !ok {
		return nil
	}

	h.mu.Lock()
	binding, live := h.sessionAccounts[sessionID]
	router := h.runner
	if !live || router == nil || binding.runnerID != router.id {
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()
	cmd := &compassv1internal.SessionsResponse{
		Traceparent: otelx.Traceparent(ctx),
		Command: &compassv1internal.SessionsResponse_SecretsVersion{
			SecretsVersion: &compassv1internal.SecretsVersion{
				SessionId: sessionID,
				Version:   h.mintSecretsVersion(),
			},
		},
	}
	return router.router.push(ctx, cmd)
}
