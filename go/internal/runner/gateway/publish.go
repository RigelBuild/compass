//go:build unix

package gateway

// publish.go is the telemetry ingest for the Publish client-stream: the agent
// streams trace/session AgentFrames in emission order, and the Runner forwards
// each up PublishEvents Runner-sequenced (the exact stamping relay.go does for
// the retired stdout relay, minus the scanner + protojson decode). Two AgentFrame
// variants that arrive here are NOT telemetry — ReplayCompleteAck and ControlAck
// are control-plane acks routed to the control lane, never relayed upstream
// (transport-consolidation record, ack routing). Stream end is the old stdout
// EOF: close the upstream PublishEvents stream and await its ack.

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
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

	pub := g.acquirePublisher(sessionID)

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
			g.control.ReleaseReplayBarrier(sessionID)
			continue
		case *compassv1internal.AgentFrame_ControlAck:
			ack := f.ControlAck
			g.control.AckControl(sessionID, ack.GetAckedSeq(), ack.GetAppliedAbove())
			continue
		}
		// Trace/session telemetry: forward Runner-sequenced. A durable
		// conversation frame does NOT belong on this lossy stream (it takes the
		// CommitConversationFrame unary); but if one arrives here it is still a
		// valid AgentFrame the hub can classify, so forward it rather than drop
		// it — the split is enforced agent-side, and dropping a durable frame
		// silently is the exact loss the split exists to prevent.
		if err := pub.forward(frame); err != nil {
			// A mid-stream upstream failure ends the relay; the agent reconnects.
			// Release the shared upstream stream on the way out.
			_ = g.releasePublisher()
			return nil, err
		}
	}
	if err := stream.Err(); err != nil {
		// The inbound stream failed (an over-limit message past WithReadMaxBytes,
		// or a transport drop). Release the upstream and surface the error.
		_ = g.releasePublisher()
		return nil, err
	}

	// Clean stream end == stdout EOF: close the upstream PublishEvents stream and
	// await its ack, then ack the agent's stream.
	if err := g.releasePublisher(); err != nil && !errors.Is(err, context.Canceled) {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&compassv1internal.PublishFrameResponse{}), nil
}
