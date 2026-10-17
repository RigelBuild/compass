//go:build unix

package gateway

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// errNoSessionForBoard marks a Board call before Start binds a session.
var errNoSessionForBoard = errors.New("gateway: no live session bound to container")
var errNilBoardResult = errors.New("gateway: relay returned no board result")

// Board forwards an agent board call under the session bound to this container.
// A missing binding returns FailedPrecondition; the Runner sets no actor.
func (g *Gateway) Board(
	ctx context.Context, req *compassv1internal.BoardCallRequest,
) (*compassv1internal.BoardCallResult, error) {
	sessionID, ok := g.sessions.Session(g.containerName)
	if !ok || sessionID == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errNoSessionForBoard.Error()).WithCause(errNoSessionForBoard)
	}

	resp, err := g.board.RelayBoardCall(ctx, &compassv1internal.RelayBoardCallRequest{
		SessionId: sessionID,
		Call:      req,
	})
	if err != nil {
		return nil, err
	}
	result := resp.GetResult()
	if result == nil {
		return nil, connect.NewError(connect.CodeInternal, errNilBoardResult.Error()).WithCause(errNilBoardResult)
	}
	return result, nil
}
