//go:build podman

package e2e

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
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
// CompassService with an initial prompt and returns the server-side session id.
func (f *Fixture) StartSession(ctx context.Context, containerName, initialPrompt string) (sessionID string, err error) {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	resp, err := f.Compass().StartAgentSession(rctx, connect.NewRequest(&compassv1.StartAgentSessionRequest{
		ContainerName: containerName,
		InitialPrompt: initialPrompt,
	}))
	if err != nil {
		return "", fmt.Errorf("StartAgentSession RPC: %w", err)
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
