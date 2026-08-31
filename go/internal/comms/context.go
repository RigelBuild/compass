package comms

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/internal/store"
)

// actorContextKey is the private context key under which the authenticated
// caller's AccountID is carried. The T3 network door's token interceptor sets it
// per request; the shipped local-socket door sets nothing, so actorFromContext
// falls back to the bootstrap admin. Unexported so only this package can set or
// read it — the caller identity can never be spoofed through a request field.
type actorContextKey struct{}

// WithActor returns a context carrying actor as the authenticated caller. The
// T3 interceptor calls it after resolving a token to an account; tests call it
// to exercise a specific caller. On the socket door it is never called, and the
// handler attributes the bootstrap admin.
func WithActor(ctx context.Context, actor store.AccountID) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actor)
}

// actorFrom reports the authenticated caller set on ctx, if any. The bool is
// false on the socket door (no interceptor set one), which is the handler's
// signal to fall back to the bootstrap admin.
func actorFrom(ctx context.Context) (store.AccountID, bool) {
	actor, ok := ctx.Value(actorContextKey{}).(store.AccountID)
	return actor, ok
}

// errEmptyBlock is the invalid-argument cause for a message block whose oneof is
// unset (neither text nor ask). Sent as an InvalidArgument connect error.
var errEmptyBlock = errors.New("message block has neither text nor ask set")

// errServerOwnedBlock is the invalid-argument cause for a client write path
// carrying an ask_answer block: the ask_answer variant is server-owned,
// constructed only by RespondToAsk, so an inbound one (PostMessage / the agent
// update path) is rejected at the wire edge — the only layer with caller
// identity (RIG-2257).
var errServerOwnedBlock = errors.New("ask_answer blocks are server-owned")

// errMissingMemberUpdate is the internal-error cause when a member id recorded in
// the ordered list has no accumulated update in the byID map — an impossible
// state (touch populates both together), guarded rather than dereferenced blind.
var errMissingMemberUpdate = errors.New("member update missing for ordered account id")

// edgeError maps a store error onto the connect status code the wire contract
// expects, at the service edge. The store's sentinel errors (errors.go) are the
// vocabulary; anything unrecognized is an internal error, never leaked verbatim.
func edgeError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, store.ErrInvalidArgument):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, store.ErrPermissionDenied):
		return connect.NewError(connect.CodePermissionDenied, err)
	case errors.Is(err, store.ErrFailedPrecondition):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, store.ErrConflict):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, store.ErrTokenRevoked):
		return connect.NewError(connect.CodeUnauthenticated, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
