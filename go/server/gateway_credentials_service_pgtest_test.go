//go:build pgtest && unix

package server

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/otelconnect"

	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/envelope"
	"github.com/RigelBuild/compass/go/internal/gatewaycred"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// gatewayDoorFixture is the tenant-B world the door test drives over the real mount.
type gatewayDoorFixture struct {
	client                               compassv1internalconnect.GatewayCredentialsClient
	tokens                               *auth.GatewayTokens
	tenantCtx                            context.Context
	agentID                              store.AccountID
	agentToken, accountToken             string
	ownCredentialID, foreignCredentialID string
}

const (
	gatewayDoorServiceToken      = "gateway-door-service-token"
	gatewayDoorOtherServiceToken = "gateway-door-other-service-token"
	gatewayDoorRunnerToken       = "gateway-door-runner-token"
)

func newGatewayDoorFixture(t *testing.T) gatewayDoorFixture {
	t.Helper()
	ctx := t.Context()
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	rootOwner, err := st.CreateUser(ctx, store.NewUser{Handle: "gateway-door-root-owner", DisplayName: "Gateway Door Root Owner"})
	if err != nil {
		t.Fatalf("CreateUser(root owner): %v", err)
	}
	accountToken, err := auth.IssueAccountToken(ctx, st, rootOwner.ID)
	if err != nil {
		t.Fatalf("IssueAccountToken: %v", err)
	}
	const tenantB store.TenantID = "gateway-door-tenant-b"
	tenantPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(tenantPool.Close)
	if _, err := tenantPool.Exec(ctx,
		"INSERT INTO tenants (id, slug, display_name, created_at_unix_ms) VALUES ($1, $2, $3, $4)",
		string(tenantB), string(tenantB), "Gateway Door Tenant B", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("insert tenant B: %v", err)
	}
	tenantCtx := store.WithTenant(ctx, tenantB)
	ownerX, err := st.CreateUser(tenantCtx, store.NewUser{Handle: "gateway-door-owner-x", DisplayName: "Gateway Door Owner X"})
	if err != nil {
		t.Fatalf("CreateUser(owner X): %v", err)
	}
	agentX, err := st.CreateAgent(tenantCtx, ownerX.ID, store.NewAgent{Handle: "gateway-door-agent-x", DisplayName: "Gateway Door Agent X"})
	if err != nil {
		t.Fatalf("CreateAgent(owner X): %v", err)
	}
	ownerY, err := st.CreateUser(tenantCtx, store.NewUser{Handle: "gateway-door-owner-y", DisplayName: "Gateway Door Owner Y"})
	if err != nil {
		t.Fatalf("CreateUser(owner Y): %v", err)
	}
	tokens := auth.NewGatewayTokens(st)
	agentToken, err := tokens.Mint(tenantCtx, string(agentX.ID))
	if err != nil {
		t.Fatalf("Mint gateway token: %v", err)
	}
	if err := st.PutTokenHash(ctx, sha256.Sum256([]byte(gatewayDoorServiceToken)), store.Subject{Kind: store.SubjectService, ID: auth.LLMGatewayServiceID}); err != nil {
		t.Fatalf("PutTokenHash(gateway service): %v", err)
	}
	if err := st.PutTokenHash(ctx, sha256.Sum256([]byte(gatewayDoorOtherServiceToken)), store.Subject{Kind: store.SubjectService, ID: "other-service"}); err != nil {
		t.Fatalf("PutTokenHash(other service): %v", err)
	}
	if err := st.PutTokenHash(ctx, sha256.Sum256([]byte(gatewayDoorRunnerToken)), store.Subject{Kind: store.SubjectRunner, ID: "gateway-door-runner"}); err != nil {
		t.Fatalf("PutTokenHash(runner): %v", err)
	}
	key, err := envelope.NewKey(make([]byte, 32))
	if err != nil {
		t.Fatalf("envelope.NewKey: %v", err)
	}
	credentials := gatewaycred.NewPostgres(st, key, 1)
	ownCredential, err := credentials.Create(tenantCtx, gatewaycred.NewAPIKeyCredential(
		gatewaycred.Credential{Provider: "anthropic", Scope: gatewaycred.ScopeOwn, OwnerUserID: ownerX.ID}, "door-key-x"))
	if err != nil {
		t.Fatalf("seed owner X credential: %v", err)
	}
	foreignCredential, err := credentials.Create(tenantCtx, gatewaycred.NewAPIKeyCredential(
		gatewaycred.Credential{Provider: "openai", Scope: gatewaycred.ScopeOwn, OwnerUserID: ownerY.ID}, "door-key-y"))
	if err != nil {
		t.Fatalf("seed owner Y credential: %v", err)
	}
	otelIC, err := otelconnect.NewServerInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewServerInterceptor: %v", err)
	}
	resolve := func(ctx context.Context, presented string, want store.SubjectKind) (store.Subject, error) {
		return auth.ResolveToken(ctx, st, presented, want)
	}
	mux := http.NewServeMux()
	mountGatewayServices(mux, newGatewayServices(st, key, 1), otelIC, newRunnerResolve(resolve, nil))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return gatewayDoorFixture{
		client: compassv1internalconnect.NewGatewayCredentialsClient(connect.NewClient(connecthttp.NewTransport(http.DefaultClient, server.URL, connecthttp.WithReadMaxBytes(0)))),
		tokens: tokens, tenantCtx: tenantCtx, agentID: agentX.ID,
		agentToken: agentToken, accountToken: accountToken,
		ownCredentialID: ownCredential.ID, foreignCredentialID: foreignCredential.ID,
	}
}

func TestGatewayCredentialsDoorServiceBearerAllowlist(t *testing.T) {
	ctx := t.Context()
	f := newGatewayDoorFixture(t)
	client, agentToken, serviceToken := f.client, f.agentToken, gatewayDoorServiceToken

	for _, tt := range []struct {
		name     string
		bearer   string
		wantCode connect.Code
		wantOK   bool
	}{
		{name: "allowlisted service token", bearer: serviceToken, wantOK: true},
		{name: "different service id", bearer: gatewayDoorOtherServiceToken, wantCode: connect.CodeUnauthenticated},
		{name: "runner token", bearer: gatewayDoorRunnerToken, wantCode: connect.CodeUnauthenticated},
		{name: "account bearer", bearer: f.accountToken, wantCode: connect.CodeUnauthenticated},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := &compassv1internal.ListCredentialPoolRequest{AgentToken: agentToken}
			ctx, info := connect.NewClientContext(ctx)
			info.RequestHeader().Set("Authorization", "Bearer "+tt.bearer)
			response, err := client.ListCredentialPool(ctx, req)
			if tt.wantOK {
				if err != nil {
					t.Fatalf("allowlisted service token rejected: %v", err)
				}
				if got := response.GetCredentials(); len(got) != 1 || got[0].GetId() != f.ownCredentialID || got[0].GetApiKey() != "door-key-x" {
					t.Fatalf("pool = %v, want tenant B owner's credential", got)
				}
				return
			}
			if connect.CodeOf(err) != tt.wantCode {
				t.Fatalf("error code = %v, want %v (err %v)", connect.CodeOf(err), tt.wantCode, err)
			}
		})
	}

	t.Run("agent token is not the service bearer", func(t *testing.T) {
		req := &compassv1internal.ListCredentialPoolRequest{AgentToken: agentToken}
		ctx, info := connect.NewClientContext(ctx)
		info.RequestHeader().Set("Authorization", "Bearer "+agentToken)
		if _, err := client.ListCredentialPool(ctx, req); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("agent token as bearer error = %v, want Unauthenticated", err)
		}
	})

	t.Run("owner token cannot access another owner's credential", func(t *testing.T) {
		req := &compassv1internal.DisableCredentialRequest{
			Id: f.foreignCredentialID, AgentToken: agentToken, ExpectedVersion: 1,
		}
		ctx, info := connect.NewClientContext(ctx)
		info.RequestHeader().Set("Authorization", "Bearer "+serviceToken)
		if _, err := client.DisableCredential(ctx, req); connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("foreign credential error = %v, want NotFound", err)
		}
	})

	if err := f.tokens.Revoke(f.tenantCtx, string(f.agentID)); err != nil {
		t.Fatalf("Revoke gateway token: %v", err)
	}
	t.Run("revoked agent token is unauthenticated", func(t *testing.T) {
		req := &compassv1internal.ListCredentialPoolRequest{AgentToken: agentToken}
		ctx, info := connect.NewClientContext(ctx)
		info.RequestHeader().Set("Authorization", "Bearer "+serviceToken)
		if _, err := client.ListCredentialPool(ctx, req); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("revoked token error = %v, want Unauthenticated", err)
		}
	})
}
