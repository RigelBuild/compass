//go:build pgtest && unix

package server

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"

	"github.com/RigelBuild/compass/go/internal/auth"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

func verifyAgentTokenRequest(bearer, token string) *connect.Request[compassv1internal.VerifyAgentTokenRequest] {
	req := connect.NewRequest(&compassv1internal.VerifyAgentTokenRequest{Token: token})
	if bearer != "" {
		req.Header().Set("Authorization", "Bearer "+bearer)
	}
	return req
}

// The network door serves VerifyAgentToken to the llm-gateway service only, and
// a revoked agent token stops verifying.
func TestGatewayAuthDoorVerifiesAgentTokens(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(ctx, pgtest.RequireDSN(t))
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	owner, err := st.CreateUser(ctx, store.NewUser{Handle: "gateway-auth-owner", DisplayName: "Gateway Auth Owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "gateway-auth-agent", DisplayName: "Agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	ownerToken, err := auth.IssueAccountToken(ctx, st, owner.ID)
	if err != nil {
		t.Fatalf("IssueAccountToken: %v", err)
	}
	const serviceToken, otherServiceToken = "gateway-auth-service-token", "gateway-auth-other-service-token"
	for token, id := range map[string]string{serviceToken: auth.LLMGatewayServiceID, otherServiceToken: "other-service"} {
		if err := st.PutTokenHash(ctx, sha256.Sum256([]byte(token)), store.Subject{Kind: store.SubjectService, ID: id}); err != nil {
			t.Fatalf("PutTokenHash(%s): %v", id, err)
		}
	}
	tokens := auth.NewGatewayTokens(st)
	agentToken, err := tokens.Mint(ctx, string(agent.ID))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	otelIC, err := otelconnect.NewInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewInterceptor: %v", err)
	}
	resolve := func(ctx context.Context, presented string, want store.SubjectKind) (store.Subject, error) {
		return auth.ResolveToken(ctx, st, presented, want)
	}
	mux := http.NewServeMux()
	mountGatewayServices(mux, gatewayServices{auth: newGatewayAuthService(tokens)}, otelIC, newRunnerResolve(resolve, nil))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := compassv1internalconnect.NewGatewayAuthClient(http.DefaultClient, server.URL)

	got, err := client.VerifyAgentToken(ctx, verifyAgentTokenRequest(serviceToken, agentToken))
	if err != nil {
		t.Fatalf("VerifyAgentToken as gateway: %v", err)
	}
	if got.Msg.GetAgentAccountId() != string(agent.ID) || got.Msg.GetOwnerUserId() != string(owner.ID) {
		t.Fatalf("VerifyAgentToken = %v, want agent %q owner %q", got.Msg, agent.ID, owner.ID)
	}
	for name, bearer := range map[string]string{"no bearer": "", "account bearer": ownerToken, "other service": otherServiceToken} {
		if _, err := client.VerifyAgentToken(ctx, verifyAgentTokenRequest(bearer, agentToken)); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("%s: VerifyAgentToken error = %v, want Unauthenticated", name, err)
		}
	}
	if err := tokens.Revoke(ctx, string(agent.ID)); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := client.VerifyAgentToken(ctx, verifyAgentTokenRequest(serviceToken, agentToken)); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("VerifyAgentToken after revoke error = %v, want Unauthenticated", err)
	}
}
