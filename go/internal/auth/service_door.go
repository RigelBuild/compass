package auth

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"connectrpc.com/connect/v2"

	"github.com/RigelBuild/compass/go/internal/store"
)

// LLMGatewayServiceID is the service subject allowed to use the gateway door.
const LLMGatewayServiceID = "llm-gateway"

type serviceSubjectKey struct{}

var errServiceUnauthenticated = connect.NewError(connect.CodeUnauthenticated, "unauthenticated")

// ServiceSubjectFrom returns the service principal attached by the service bearer door.
func ServiceSubjectFrom(ctx context.Context) (store.Subject, bool) {
	subj, ok := ctx.Value(serviceSubjectKey{}).(store.Subject)
	return subj, ok
}

// ServiceBearerInterceptor authenticates a service token and restricts its subject ID.
func ServiceBearerInterceptor(
	resolve func(ctx context.Context, presented string, want store.SubjectKind) (store.Subject, error),
	allow ...string,
) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			var header *connect.Header
			if info, ok := connect.CallInfoForServerContext(ctx); ok {
				header = info.RequestHeader()
			}
			authenticated, err := authenticateServiceSubject(ctx, header.Get(authorizationHeader), resolve, allow)
			if err != nil {
				return err
			}
			return next(authenticated, spec, stream)
		}
	}
}

func authenticateServiceSubject(
	ctx context.Context,
	header string,
	resolve func(ctx context.Context, presented string, want store.SubjectKind) (store.Subject, error),
	allow []string,
) (context.Context, error) {
	if header == "" {
		slog.DebugContext(ctx, "service door rejected bearer token", "reason", "missing authorization")
		return nil, errServiceUnauthenticated
	}
	token, ok := bearerToken(header)
	if !ok {
		slog.DebugContext(ctx, "service door rejected bearer token", "reason", "malformed authorization")
		return nil, errServiceUnauthenticated
	}
	subj, err := resolve(ctx, token, store.SubjectService)
	if err != nil {
		reason := "token resolution failed"
		switch {
		case errors.Is(err, ErrTokenNotFound):
			reason = "token not found"
		case errors.Is(err, ErrTokenRevoked):
			reason = "token revoked"
		case errors.Is(err, ErrWrongKind):
			reason = "wrong subject kind"
		}
		slog.DebugContext(ctx, "service door rejected bearer token", "reason", reason)
		return nil, errServiceUnauthenticated
	}
	if subj.Kind != store.SubjectService {
		slog.DebugContext(ctx, "service door rejected bearer token", "reason", "wrong subject kind")
		return nil, errServiceUnauthenticated
	}
	if !slices.Contains(allow, subj.ID) {
		slog.DebugContext(ctx, "service door rejected bearer token", "reason", "service not allowlisted")
		return nil, errServiceUnauthenticated
	}
	return context.WithValue(ctx, serviceSubjectKey{}, subj), nil
}
