package server

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/internal/auth"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/store"
)

// gatewayTokenVerifier is the slice of auth.TokenMinter the verify door needs.
type gatewayTokenVerifier interface {
	Verify(ctx context.Context, token string) (store.GatewayCaller, error)
}

// gatewayAuthService serves the LLM gateway's per-agent token verification.
// The caller must already be authenticated as the llm-gateway service.
type gatewayAuthService struct {
	compassv1internalconnect.UnimplementedGatewayAuthHandler
	tokens gatewayTokenVerifier
}

var _ compassv1internalconnect.GatewayAuthHandler = (*gatewayAuthService)(nil)

func newGatewayAuthService(tokens gatewayTokenVerifier) *gatewayAuthService {
	return &gatewayAuthService{tokens: tokens}
}

// errGatewayTokenInvalid is fixed so the response never says why a token failed.
var errGatewayTokenInvalid = errors.New("invalid agent token")

// VerifyAgentToken resolves an agent's gateway bearer to its agent and owner.
func (s *gatewayAuthService) VerifyAgentToken(ctx context.Context, req *connect.Request[compassv1internal.VerifyAgentTokenRequest]) (*connect.Response[compassv1internal.VerifyAgentTokenResponse], error) {
	caller, err := s.tokens.Verify(ctx, req.Msg.GetToken())
	if err != nil {
		if errors.Is(err, auth.ErrGatewayTokenInvalid) {
			return nil, connect.NewError(connect.CodeUnauthenticated, errGatewayTokenInvalid)
		}
		// The error never carries the token: Verify wraps only store faults.
		slog.ErrorContext(ctx, "gateway agent token verification failed", "error", err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("agent token verification failed"))
	}
	return connect.NewResponse(&compassv1internal.VerifyAgentTokenResponse{
		AgentAccountId: string(caller.AgentAccountID),
		OwnerUserId:    string(caller.OwnerUserID),
	}), nil
}
