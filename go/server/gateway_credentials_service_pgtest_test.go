//go:build pgtest && unix

package server

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/gatewaycred"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

func TestGatewayCredentialsDoorServiceBearerAllowlist(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(ctx, pgtest.RequireDSN(t))
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	owner, err := st.CreateUser(ctx, store.NewUser{Handle: "gateway-door-owner", DisplayName: "Gateway Door Owner"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	agent, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "gateway-door-agent", DisplayName: "Gateway Door Agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	accountToken, err := auth.IssueAccountToken(ctx, st, owner.ID)
	if err != nil {
		t.Fatalf("IssueAccountToken: %v", err)
	}
	const serviceToken = "gateway-door-service-token"
	if err := st.PutTokenHash(ctx, sha256.Sum256([]byte(serviceToken)), store.Subject{Kind: store.SubjectService, ID: auth.LLMGatewayServiceID}); err != nil {
		t.Fatalf("PutTokenHash(gateway service): %v", err)
	}
	const otherServiceToken = "gateway-door-other-service-token"
	if err := st.PutTokenHash(ctx, sha256.Sum256([]byte(otherServiceToken)), store.Subject{Kind: store.SubjectService, ID: "other-service"}); err != nil {
		t.Fatalf("PutTokenHash(other service): %v", err)
	}
	const runnerToken = "gateway-door-runner-token"
	if err := st.PutTokenHash(ctx, sha256.Sum256([]byte(runnerToken)), store.Subject{Kind: store.SubjectRunner, ID: "gateway-door-runner"}); err != nil {
		t.Fatalf("PutTokenHash(runner): %v", err)
	}

	memory := gatewaycred.NewMemory()
	memory.SetAgentOwner(agent.ID, owner.ID)
	if _, err := memory.Create(store.WithTenant(ctx, st.EffectiveTenant(ctx)), gatewaycred.NewAPIKeyCredential(
		gatewaycred.Credential{Provider: "anthropic", Scope: gatewaycred.ScopeOwn, OwnerUserID: owner.ID}, "door-key")); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	svc := newGatewayCredentialsService(st, memory, memory, nil)
	resolve := func(ctx context.Context, presented string, want store.SubjectKind) (store.Subject, error) {
		return auth.ResolveToken(ctx, st, presented, want)
	}
	path, handler := compassv1internalconnect.NewGatewayCredentialsHandler(svc,
		connect.WithInterceptors(auth.ServiceBearerInterceptor(resolve, auth.LLMGatewayServiceID)))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client := compassv1internalconnect.NewGatewayCredentialsClient(http.DefaultClient, server.URL)

	for _, tt := range []struct {
		name     string
		token    string
		wantCode connect.Code
		wantOK   bool
	}{
		{name: "allowlisted service token", token: serviceToken, wantOK: true},
		{name: "different service id", token: otherServiceToken, wantCode: connect.CodeUnauthenticated},
		{name: "account token", token: accountToken, wantCode: connect.CodeUnauthenticated},
		{name: "runner token", token: runnerToken, wantCode: connect.CodeUnauthenticated},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := connect.NewRequest(&compassv1internal.ListCredentialPoolRequest{AgentAccountId: string(agent.ID)})
			req.Header().Set("Authorization", "Bearer "+tt.token)
			response, err := client.ListCredentialPool(ctx, req)
			if tt.wantOK {
				if err != nil {
					t.Fatalf("allowlisted service token rejected: %v", err)
				}
				if got := response.Msg.GetCredentials(); len(got) != 1 || got[0].GetApiKey() != "door-key" {
					t.Fatalf("pool = %v, want the seeded own credential", got)
				}
				return
			}
			if connect.CodeOf(err) != tt.wantCode {
				t.Fatalf("error code = %v, want %v (err %v)", connect.CodeOf(err), tt.wantCode, err)
			}
		})
	}
}
