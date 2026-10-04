//go:build unix

package gateway

import (
	"context"
	"errors"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

var (
	errNoGateway          = errors.New("gateway: listener has no gateway to publish through")
	errSharedStateTimeout = errors.New("gateway: lifecycle state send on the shared stream timed out")
)

// stateSendTimeout bounds the lifecycle send itself, separate from the in-flight
// Publish wait, so a wait that spends the caller's budget still reports the state.
const stateSendTimeout = 2 * time.Second

// PublishSessionState sends a lifecycle transition over the Runner-sequenced
// PublishEvents path. It drains open Publish handlers, bounded by ctx, then seals
// frame admission so no later telemetry frame can follow the terminal state.
//
// A drain that times out can leave an admitted frame mid-Send on the shared
// stream, so the state rides that stream under its send lock when one exists:
// same stream and same lock order it after every admitted frame. Otherwise, or if
// that send fails or stalls, it falls back to a bounded one-shot stream.
func (l *SocketListener) PublishSessionState(ctx context.Context, sessionID string, state compassv1.AgentSessionState) error {
	if l.gateway == nil {
		return errNoGateway
	}
	g := l.gateway
	frame := &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_Session{
			Session: &compassv1internal.SessionFrame{State: state},
		},
	}
	g.fencePublishes(ctx, sessionID)
	var sharedErr error
	if shared := g.sharedPublisher(sessionID); shared != nil {
		if sharedErr = g.sendSharedState(ctx, shared, frame); sharedErr == nil {
			return nil
		}
	}
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateSendTimeout)
	defer cancel()
	pub := newSessionPublisher(sendCtx, g.events, sessionID, &g.seq)
	forwardErr := pub.forward(frame)
	if forwardErr == nil {
		sharedErr = nil
	}
	return errors.Join(sharedErr, forwardErr, pub.close())
}

// sendSharedState sends frame on the shared publisher, bounded by
// stateSendTimeout off ctx's values but not its cancellation (the drain may have
// spent it). On timeout it cancels that stream and waits for the stalled Send to
// return, so its RunnerSeq is settled (sent or rolled back) before the caller
// allocates one for the fallback.
func (g *Gateway) sendSharedState(ctx context.Context, pub *sessionPublisher, frame *compassv1internal.AgentFrame) error {
	stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateSendTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		if g.beforeSharedState != nil {
			g.beforeSharedState()
		}
		done <- pub.forwardState(frame)
	}()
	select {
	case err := <-done:
		return err
	case <-stateCtx.Done():
	}
	pub.cancel()
	// The state may have reached Send just before the cancel; a success stands.
	if err := <-done; err != nil {
		return errors.Join(errSharedStateTimeout, err)
	}
	return nil
}
