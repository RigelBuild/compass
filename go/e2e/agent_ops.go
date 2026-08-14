//go:build podman

package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// CreateAgent creates a first-party agent account over CommsService and returns
// its account id. Thin client-RPC primitive the later legs reuse; returns an
// error rather than panicking so the caller (a test) decides fatality. The
// per-call deadline is threaded from ctx.
func (f *Fixture) CreateAgent(ctx context.Context, handle, displayName string) (accountID string, err error) {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Comms().CreateAgent(rctx, connect.NewRequest(&compassv1.CreateAgentRequest{
		Handle:      handle,
		DisplayName: displayName,
	}))
	if err != nil {
		return "", fmt.Errorf("CreateAgent RPC: %w", err)
	}
	return resp.Msg.GetAccount().GetId(), nil
}

// Provision provisions the agent's per-account workspace container over
// CompassService and returns the assigned container name. clientRequestID is the
// idempotency key. Repo carriage was removed (SEA-1527), so no repo/ref fields
// exist to set.
func (f *Fixture) Provision(ctx context.Context, accountID, clientRequestID string) (containerName string, err error) {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Compass().ProvisionAgentWorkspace(rctx, connect.NewRequest(&compassv1.ProvisionAgentWorkspaceRequest{
		AgentAccountId:  accountID,
		ClientRequestId: clientRequestID,
	}))
	if err != nil {
		return "", fmt.Errorf("ProvisionAgentWorkspace RPC: %w", err)
	}
	return resp.Msg.GetContainerName(), nil
}

// StartSession brings the agent in a provisioned container online over
// CompassService and returns the server-side session id.
func (f *Fixture) StartSession(ctx context.Context, containerName string) (sessionID string, err error) {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Compass().StartAgentSession(rctx, connect.NewRequest(&compassv1.StartAgentSessionRequest{
		ContainerName: containerName,
	}))
	if err != nil {
		return "", fmt.Errorf("StartAgentSession RPC: %w", err)
	}
	return resp.Msg.GetSessionId(), nil
}

// Resume brings the agent in a freshly provisioned container online over
// CompassService resuming a persisted logical session: resumeSessionID is the
// session id to resume (the id a prior StartSession returned), reconstructed
// server-side into the new container. Returns the server-MINTED live session id
// for the resumed lifetime (a NEW id — the durable transcript stays keyed under
// resumeSessionID). Returns an error rather than panicking so the caller decides
// fatality; the per-call deadline is threaded from ctx.
func (f *Fixture) Resume(ctx context.Context, containerName, resumeSessionID string) (sessionID string, err error) {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Compass().StartAgentSession(rctx, connect.NewRequest(&compassv1.StartAgentSessionRequest{
		ContainerName:   containerName,
		ResumeSessionId: resumeSessionID,
	}))
	if err != nil {
		return "", fmt.Errorf("StartAgentSession(resume) RPC: %w", err)
	}
	return resp.Msg.GetSessionId(), nil
}

// AwaitSessionSettled subscribes to the session frame stream and returns once the
// session has settled — the first frame reporting AGENT_SESSION_STATE_READY. It
// is FULLY EVENT-GATED: it reads frames off the stream until READY or the
// deadline elapses — no sleeps, no polling, no retry loops. It derives its own
// deadline from ctx (settleTimeout) so a wedged stream fails visibly rather than
// blocking to the go-test timeout — the guarantee holds for every caller, not
// just one that remembers to pass a bounded ctx.
func (f *Fixture) AwaitSessionSettled(ctx context.Context, sessionID string) error {
	ctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()

	stream, err := f.Compass().SubscribeAgentSession(ctx, connect.NewRequest(&compassv1.SubscribeAgentSessionRequest{
		SessionId: sessionID,
	}))
	if err != nil {
		return fmt.Errorf("SubscribeAgentSession RPC: %w", err)
	}
	defer stream.Close()

	for stream.Receive() {
		if stream.Msg().GetState() == compassv1.AgentSessionState_AGENT_SESSION_STATE_READY {
			return nil
		}
	}
	if err := stream.Err(); err != nil {
		return fmt.Errorf("SubscribeAgentSession stream: %w", err)
	}
	return fmt.Errorf("session %s frame stream ended before reaching READY", sessionID)
}

// RemoveWorkspace tears down a provisioned agent workspace container over
// CompassService — the teardown counterpart to Provision. clientRequestID is the
// idempotency key (same retry-dedup contract as Provision). Returns an error
// rather than panicking so the caller (a test) decides fatality; a best-effort
// t.Cleanup ignores it. The per-call deadline is threaded from ctx.
func (f *Fixture) RemoveWorkspace(ctx context.Context, containerName, clientRequestID string) error {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	if _, err := f.Compass().RemoveAgentWorkspace(rctx, connect.NewRequest(&compassv1.RemoveAgentWorkspaceRequest{
		ContainerName:   containerName,
		ClientRequestId: clientRequestID,
	})); err != nil {
		return fmt.Errorf("RemoveAgentWorkspace RPC: %w", err)
	}
	return nil
}

// waitRunnerEnrolled blocks until the embedded compass-runner has enrolled with
// the server, or the budget elapses. It is the enrollment counterpart to the
// stack's own waitReady/waitPostgres poll (stack.go): stack.Up returns as soon
// as the runner CHILD is spawned, but the runner enrolls ASYNCHRONOUSLY over the
// TLS door AFTER Up returns, so a leg that Provisions immediately races that
// enrollment and fails `unavailable: no runner enrolled to serve session`. This
// gate closes that race so every Provisioning leg starts against an enrolled
// runner.
//
// The observable enrollment signal available to the cross-process fixture is a
// lightweight enrollment-gated probe. The client GetAgentStatus is served off
// the Server's board projection (server/service.go), NOT a Runner relay, so it
// answers even with no Runner and cannot observe enrollment. StopAgentSession,
// by contrast, relays through the hub's routerFor exactly as Provision does, so
// it returns the CodeUnavailable `no runner enrolled` error until a Runner has
// enrolled — and once one has, a Stop of a synthetic never-started session id is
// an idempotent Runner-side no-op (host.Stop returns success for an unknown
// session; the session-end transcript flush is skipped since the id has no
// entries), so the probe has NO container or session side effect. ONLY that
// specific unavailable-no-runner condition is treated as not-yet-ready; any
// other error is a real failure and is returned immediately. Enrollment is a
// MONOTONIC one-time transition, so this is an event-gated readiness poll on a
// real cross-process signal, not a retry-as-sync: it returns the instant the
// probe stops reporting no-runner. The poll respects ctx cancellation; a budget
// timeout is a legible error.
func (f *Fixture) waitRunnerEnrolled(ctx context.Context) error {
	deadline := f.now().Add(enrollPollBudget)
	ticker := time.NewTicker(enrollPollInterval)
	defer ticker.Stop()
	for {
		if !f.now().Before(deadline) {
			return fmt.Errorf("runner did not enroll within %s", enrollPollBudget)
		}
		if ready, err := f.runnerEnrolledProbe(ctx, deadline); err != nil {
			return err
		} else if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// waitDeliveryCursorPast blocks until post1 is no longer OWED to the agent on
// its home channel — the moment the agent's delivery cursor has advanced past
// it — or the budget elapses. leg-5's resume is only correct if this ordering
// holds BEFORE the resumed lifetime's server-side start-sweep runs: the cursor
// advances on the agent's delivery_ack (runnerhub deliverAck), NOT on the
// WORKING→READY settle AwaitSessionSettled observes, so a settle can return
// with post1 still owed. The resume start-sweep reads UndeliveredMessages for
// the agent and redelivers anything still owed into the fresh container2; were
// post1 still owed at that point the sweep would redeliver it, consume the
// resumed lifetime's canned turn, and desync the 2-turn script (a
// hang-to-timeout flake OR a mis-attributed green). Gating on the cursor — the
// real cross-process ack signal — instead of trusting the settle-implies-acked
// ordering closes that race.
//
// It is an event-gated bounded poll on UndeliveredMessages, mirroring
// waitRunnerEnrolled: a ticker-driven bound off f.now() with a deadline check
// that returns a legible timeout error, and a select on ctx.Done() vs the
// ticker so it respects cancellation — no sleeps, no retry-as-sync. It returns
// nil the instant post1ID is absent from the home-channel slice of the map
// UndeliveredMessages returns; a store error, ctx cancellation, or a budget
// timeout is a legible error.
func (f *Fixture) waitDeliveryCursorPast(ctx context.Context, st *store.Store, agent store.AccountID, home store.ChannelID, post1ID store.MessageID) error {
	deadline := f.now().Add(cursorPollBudget)
	ticker := time.NewTicker(cursorPollInterval)
	defer ticker.Stop()
	for {
		if !f.now().Before(deadline) {
			return fmt.Errorf("delivery cursor did not advance past message %s within %s", post1ID, cursorPollBudget)
		}
		owed, err := st.UndeliveredMessages(ctx, agent)
		if err != nil {
			return err
		}
		stillOwed := false
		for _, m := range owed[home] {
			if m.ID == post1ID {
				stillOwed = true
				break
			}
		}
		if !stillOwed {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// cursorPollInterval and cursorPollBudget bound waitDeliveryCursorPast: the
// wait between leg-5's pre-teardown settle and the container1 teardown for the
// agent's delivery cursor to advance past post1. The cursor advances on the
// agent's delivery_ack (runnerhub deliverAck), which trails the WORKING→READY
// settle by a bus round-trip, not by an agent turn — so like enrollment this is
// a fast one-time transition and the budget can be far smaller than
// settleTimeout while still failing a genuinely stuck ack legibly rather than
// hanging to the go-test timeout; the interval matches enrollPollInterval's
// magnitude. A deterministic deadline, never a retry loop.
const (
	cursorPollInterval = 100 * time.Millisecond
	cursorPollBudget   = 15 * time.Second
)

// enrollProbeSessionID is the synthetic, never-started session id the enrollment
// probe Stops. It is namespaced so it can never collide with a real
// Server-minted session id; a Stop of it is an idempotent Runner-side no-op.
const enrollProbeSessionID = "e2e-enroll-probe-nonexistent-session"

// runnerEnrolledProbe runs one lightweight enrollment-gated probe. It reports
// ready=true once the Runner is enrolled (the Stop relay no longer returns the
// no-runner error), ready=false while enrollment is still pending (the specific
// CodeUnavailable `no runner enrolled` condition), and a non-nil error for any
// other failure — which waitRunnerEnrolled surfaces immediately rather than
// polling through.
func (f *Fixture) runnerEnrolledProbe(ctx context.Context, deadline time.Time) (ready bool, err error) {
	perProbe := rpcTimeout
	if !deadline.IsZero() {
		if remaining := deadline.Sub(f.now()); remaining < perProbe {
			perProbe = remaining
		}
	}
	if perProbe <= 0 {
		perProbe = time.Millisecond
	}
	rctx, cancel := context.WithTimeout(ctx, perProbe)
	defer cancel()
	_, err = f.Compass().StopAgentSession(rctx, connect.NewRequest(&compassv1.StopAgentSessionRequest{
		SessionId: enrollProbeSessionID,
	}))
	ready, retry, cerr := classifyEnrollProbe(err)
	if retry {
		return false, nil
	}
	return ready, cerr
}

// classifyEnrollProbe classifies a StopAgentSession probe result into the
// enrollment-readiness signal, as a pure function so its branches are unit
// testable without a live client. A nil error means the Runner is enrolled. The
// substring "no runner enrolled" is a load-bearing cross-package coupling to the
// production error raised by routerFor at go/internal/runnerhub/hub.go; only that specific
// CodeUnavailable condition is treated as not-yet-ready (retry). A CodeUnavailable
// that does NOT carry that message — a transient transport flap — is intentionally
// surfaced as fatal rather than retried, which is acceptable for a deterministic
// e2e readiness gate. Any other error is a real failure surfaced immediately.
func classifyEnrollProbe(err error) (ready bool, retry bool, cerr error) {
	if err == nil {
		return true, false, nil
	}
	if connect.CodeOf(err) == connect.CodeUnavailable && strings.Contains(err.Error(), "no runner enrolled") {
		return false, true, nil
	}
	return false, false, fmt.Errorf("runner enrollment probe (StopAgentSession): %w", err)
}
