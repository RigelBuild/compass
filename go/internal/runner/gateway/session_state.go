//go:build unix

package gateway

import (
	"context"
	"errors"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

var errNoGateway = errors.New("gateway: listener has no gateway to publish through")

// PublishSessionState sends one lifecycle transition for sessionID up the same
// Runner-sequenced PublishEvents path the agent's own frames take. It is for a
// transition the agent cannot report itself, such as its own death.
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
	pub := newSessionPublisher(ctx, g.events, sessionID, &g.seq)
	forwardErr := pub.forward(frame)
	return errors.Join(forwardErr, pub.close())
}
