//go:build unix

package gateway

import (
	"context"
	"errors"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

var errNoGateway = errors.New("gateway: listener has no gateway to publish through")

// stateSendTimeout bounds the lifecycle send itself, separate from the in-flight
// Publish wait, so a wait that spends the caller's budget still reports the state.
const stateSendTimeout = 2 * time.Second

// PublishSessionState sends a lifecycle transition over the Runner-sequenced
// PublishEvents path. It drains open Publish handlers, bounded by ctx, then seals
// frame admission so no later telemetry frame can follow the terminal state.
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
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stateSendTimeout)
	defer cancel()
	pub := newSessionPublisher(sendCtx, g.events, sessionID, &g.seq)
	forwardErr := pub.forward(frame)
	return errors.Join(forwardErr, pub.close())
}
