//go:build unix

package gateway

// The telemetry ingest for the Publish client-stream: the agent streams
// trace/session AgentFrames in emission order and the Runner forwards each up
// PublishEvents Runner-sequenced. ReplayCompleteAck and ControlAck are NOT
// telemetry — they route to the control lane, never upstream. EOF closes the stream.

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// Publish forwards the agent's trace/session frames up PublishEvents,
// Runner-sequenced through the ordered per-session publisher, and routes the two
// control-plane ack frames (ReplayCompleteAck, ControlAck) to the control lane
// instead of relaying them. It fails closed (CodePermissionDenied) when no
// session is bound to the container — the socket is live from Provision, before
// Start binds the session, so a frame in that window must never forward under an
// empty session id. Stream end closes the upstream PublishEvents stream and
// awaits its ack; a mid-stream upstream send error ends this stream (the agent
// reconnects per the loss model). A message past WithReadMaxBytes is a Connect
// stream error surfaced by Receive.
func (g *Gateway) Publish(
	ctx context.Context,
	stream *connect.ClientStream[compassv1internal.PublishFrameRequest],
) (*connect.Response[compassv1internal.PublishFrameResponse], error) {
	sessionID, ok := g.sessions.Session(g.containerName)
	// An empty session id is unbound too (mirrors Comms): never forward telemetry
	// under an empty session id.
	if !ok || sessionID == "" {
		return nil, connect.NewError(connect.CodePermissionDenied, errNoSessionForPublish)
	}

	if err := g.beginPublish(sessionID); err != nil {
		return nil, err
	}
	defer g.endPublish(sessionID)
	pub := g.acquirePublisher(sessionID)
	// Captured once: a Restart after this stream opened means these acks come from
	// the replaced process and must not touch the new process's control state.
	epoch := g.control.Epoch(sessionID)

	for stream.Receive() {
		frame := stream.Msg().GetFrame()
		if frame == nil {
			// An empty PublishFrameRequest (no frame set) carries nothing to
			// forward and is not an ack — skip it, matching the stdout relay's
			// tolerance of an undecodable line (relay.go:152-155), never tearing
			// the stream down.
			continue
		}
		// Control-plane acks ride the Publish spine beside telemetry but are
		// routed to the control lane, never relayed upstream.
		switch f := frame.GetFrame().(type) {
		case *compassv1internal.AgentFrame_ReplayCompleteAck:
			g.control.ReleaseReplayBarrier(sessionID, epoch)
			continue
		case *compassv1internal.AgentFrame_ControlAck:
			if f == nil {
				continue
			}
			ack := f.ControlAck
			if ack == nil {
				// A ControlAck wrapper with no ack payload — a frame contract
				// skew; skip it like an empty frame rather than tear the stream.
				continue
			}
			g.control.AckControl(sessionID, epoch, ack.GetAckedSeq(), ack.GetAppliedAbove(), ack.GetAppliedAboveRanges())
			continue
		}
		// Trace/session telemetry: forward Runner-sequenced. A durable conversation
		// frame belongs on the CommitConversationFrame unary, but if one arrives here
		// it is still a valid AgentFrame — forward rather than drop it, since
		// dropping a durable frame silently is the loss the split exists to prevent.
		if err := pub.forward(frame); err != nil {
			// A mid-stream upstream failure ends the relay; the agent reconnects.
			// Release the shared upstream stream on the way out.
			_ = g.releasePublisher(pub)
			return nil, err
		}
	}
	if err := stream.Err(); err != nil {
		// The inbound stream failed (an over-limit message past WithReadMaxBytes,
		// or a transport drop). Release the upstream and surface the error.
		_ = g.releasePublisher(pub)
		return nil, err
	}

	// Clean stream end == stdout EOF: close the upstream PublishEvents stream and
	// await its ack, then ack the agent's stream.
	if err := g.releasePublisher(pub); err != nil && !errors.Is(err, context.Canceled) {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&compassv1internal.PublishFrameResponse{}), nil
}

// publishGate tracks one session's Publish handlers. draining refuses new handlers
// while a lifecycle report waits for open ones; sealed also refuses their frames.
type publishGate struct {
	draining bool
	sealed   bool
	handlers int
	idle     chan struct{}
}

func (g *Gateway) beginPublish(sessionID string) error {
	g.publishMu.Lock()
	defer g.publishMu.Unlock()
	gate := g.publishes[sessionID]
	if gate == nil {
		gate = &publishGate{}
		if g.publishes == nil {
			g.publishes = make(map[string]*publishGate)
		}
		g.publishes[sessionID] = gate
	}
	if gate.draining || gate.sealed {
		return connect.NewError(connect.CodeFailedPrecondition, errSessionEnded)
	}
	if gate.handlers == 0 {
		gate.idle = make(chan struct{})
	}
	gate.handlers++
	return nil
}

func (g *Gateway) endPublish(sessionID string) {
	g.publishMu.Lock()
	defer g.publishMu.Unlock()
	gate := g.publishes[sessionID]
	if gate == nil {
		return
	}
	gate.handlers--
	if gate.handlers == 0 {
		close(gate.idle)
		if !gate.draining && !gate.sealed && g.publishes[sessionID] == gate {
			delete(g.publishes, sessionID)
		}
	}
}

func (g *Gateway) admitFrame(sessionID string) (uint64, error) {
	g.publishMu.Lock()
	defer g.publishMu.Unlock()
	gate := g.publishes[sessionID]
	if gate != nil && gate.sealed {
		return 0, connect.NewError(connect.CodeFailedPrecondition, errSessionEnded)
	}
	return g.seq.next(), nil
}

func (g *Gateway) fencePublishes(ctx context.Context, sessionID string) {
	g.publishMu.Lock()
	gate := g.publishes[sessionID]
	if gate == nil {
		gate = &publishGate{}
		if g.publishes == nil {
			g.publishes = make(map[string]*publishGate)
		}
		g.publishes[sessionID] = gate
	}
	gate.draining = true
	var idle <-chan struct{}
	if gate.handlers > 0 {
		idle = gate.idle
	}
	g.publishMu.Unlock()
	if idle != nil {
		select {
		case <-idle:
		case <-ctx.Done():
		}
	}
	g.publishMu.Lock()
	gate.sealed = true
	g.publishMu.Unlock()
}

func (g *Gateway) unfencePublishes(sessionID string) {
	g.publishMu.Lock()
	defer g.publishMu.Unlock()
	gate := g.publishes[sessionID]
	if gate == nil {
		return
	}
	gate.draining = false
	gate.sealed = false
	if gate.handlers == 0 {
		delete(g.publishes, sessionID)
	}
}
