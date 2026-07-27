package auth

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/sealedsecurity/compass/go/internal/store"
)

// Sentinel errors for the bearer rejection paths, wrapped in a
// CodeUnauthenticated connect.Error at the door. Separate values so a test (or a
// log) can tell which rejection fired without matching on message text.
var (
	errMissingAuthorization = errors.New("missing authorization header")
	errNotBearer            = errors.New("authorization header is not a bearer token")
	errInvalidToken         = errors.New("invalid or revoked token")
	errAdminOnly            = errors.New("admin-only session RPC")
)

// streamAuth is the streaming-handler half of a door's auth: it resolves the
// caller from the stream's request header (via resolve) and threads the caller
// into the handler's context. WrapUnary and WrapStreamingClient are pass-throughs
// — the door authenticates inbound streaming handler calls, and the unary door is
// served by the UnaryInterceptorFunc variants. resolve returns a
// CodeUnauthenticated error to reject the stream before the handler runs.
type streamAuth struct {
	resolve func(ctx context.Context, header string) (store.AccountID, error)
}

func (a streamAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc { return next }

func (a streamAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a streamAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		account, err := a.resolve(ctx, conn.RequestHeader().Get(authorizationHeader))
		if err != nil {
			return err
		}
		return next(withCaller(ctx, account), conn)
	}
}
