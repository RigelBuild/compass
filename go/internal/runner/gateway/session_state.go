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
// same stream and same lock order it after every admitted frame. ERRORED is that
// stream's last frame (the gate is sealed), so the report then closes it and
// returns the Server's ack. With no shared stream, or if the state never reached
// Send there, it falls back to a bounded one-shot stream.
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
		sent, err := g.sendSharedState(ctx, shared, frame)
		if sent {
			// ERRORED may already be at the Server; a one-shot retry could duplicate it.
			return err
		}
		sharedErr = err
	}
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateSendTimeout)
	defer cancel()
	pub := newSessionPublisher(sendCtx, g.events, sessionID, g.seq)
	forwardErr := pub.forward(frame)
	if forwardErr == nil {
		sharedErr = nil
	}
	return errors.Join(sharedErr, forwardErr, pub.close())
}

// sendSharedState sends frame on the shared publisher, then detaches and closes
// it and awaits the ack, all within stateSendTimeout. The budget keeps ctx's
// values but not its cancellation, because the drain may already have used it up.
// On timeout it cancels the stream and waits for the stalled call, so an unsent
// RunnerSeq is rolled back before the caller allocates one for the fallback.
// sent reports whether Send accepted the state; err is the send or close error.
func (g *Gateway) sendSharedState(ctx context.Context, pub *sessionPublisher, frame *compassv1internal.AgentFrame) (sent bool, err error) {
	stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateSendTimeout)
	defer cancel()
	type result struct {
		sent bool
		err  error
	}
	done := make(chan result, 1)
	go func() {
		if g.beforeSharedState != nil {
			g.beforeSharedState()
		}
		if err := pub.forwardState(frame); err != nil {
			done <- result{err: err}
			return
		}
		g.detachPublisher(pub)
		done <- result{sent: true, err: pub.close()}
	}()
	var res result
	select {
	case res = <-done:
		return res.sent, res.err
	case <-stateCtx.Done():
	}
	pub.cancel()
	if res = <-done; res.err != nil {
		return res.sent, errors.Join(errSharedStateTimeout, res.err)
	}
	return res.sent, nil
}
