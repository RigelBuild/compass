//go:build unix

package gateway

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

var errNoSessionForBoard = errors.New("gateway: no live session bound to container")
var errNilBoardResult = errors.New("gateway: relay returned no board result")

// Board forwards an agent board call under the session bound to this container.
// The Runner sets no actor; the Server resolves the account and fails closed.
func (g *Gateway) Board(
	ctx context.Context, req *connect.Request[compassv1internal.BoardCallRequest],
) (*connect.Response[compassv1internal.BoardCallResult], error) {
	sessionID, ok := g.sessions.Session(g.containerName)
	if !ok || sessionID == "" {
		return nil, connect.NewError(connect.CodePermissionDenied, errNoSessionForBoard)
	}

	resp, err := g.board.RelayBoardCall(ctx, connect.NewRequest(&compassv1internal.RelayBoardCallRequest{
		SessionId: sessionID,
		Call:      req.Msg,
	}))
	if err != nil {
		return nil, err
	}
	result := resp.Msg.GetResult()
	if result == nil {
		return nil, connect.NewError(connect.CodeInternal, errNilBoardResult)
	}
	return connect.NewResponse(result), nil
}
