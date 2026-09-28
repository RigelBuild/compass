//go:build pgtest && unix

package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
	"github.com/RigelBuild/compass/go/internal/usage"
)

func TestUsageSeriesScopesAccounts(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, pgtest.RequireDSN(t))
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	owner1, err := st.CreateUser(ctx, store.NewUser{Handle: "owner1", DisplayName: "owner1"})
	if err != nil {
		t.Fatalf("CreateUser owner1: %v", err)
	}
	owner2, err := st.CreateUser(ctx, store.NewUser{Handle: "owner2", DisplayName: "owner2"})
	if err != nil {
		t.Fatalf("CreateUser owner2: %v", err)
	}
	zeroOwner, err := st.CreateUser(ctx, store.NewUser{Handle: "zero", DisplayName: "zero"})
	if err != nil {
		t.Fatalf("CreateUser zero: %v", err)
	}
	a, err := st.CreateAgent(ctx, owner1.ID, store.NewAgent{Handle: "a", DisplayName: "a"})
	if err != nil {
		t.Fatalf("CreateAgent a: %v", err)
	}
	b, err := st.CreateAgent(ctx, owner1.ID, store.NewAgent{Handle: "b", DisplayName: "b", ParentAgentID: a.ID})
	if err != nil {
		t.Fatalf("CreateAgent b: %v", err)
	}
	c, err := st.CreateAgent(ctx, owner1.ID, store.NewAgent{Handle: "c", DisplayName: "c", ParentAgentID: b.ID})
	if err != nil {
		t.Fatalf("CreateAgent c: %v", err)
	}
	d, err := st.CreateAgent(ctx, owner2.ID, store.NewAgent{Handle: "d", DisplayName: "d"})
	if err != nil {
		t.Fatalf("CreateAgent d: %v", err)
	}

	usageStore := usage.NewPostgres(st)
	const start = int64(1_800_000_000_000)
	for i, agent := range []store.Account{a, b, c, d} {
		owner := owner1.ID
		if agent.ID == d.ID {
			owner = owner2.ID
		}
		provider := "anthropic"
		if i == 3 {
			provider = "openai"
		}
		if err := usageStore.AppendTokenUsage(ctx, []usage.TokenUsageEvent{{
			ID: fmt.Sprintf("event-%d", i), OccurredAtUnixMs: start, AgentAccountID: string(agent.ID),
			OwnerUserID: string(owner), Provider: provider, Model: "m", InputTokens: int64(i + 1),
			OutputTokens: int64(i + 2), TotalTokens: int64(i + 3), CostMicroUSD: int64((i + 1) * 10), Outcome: "ok",
		}}); err != nil {
			t.Fatalf("AppendTokenUsage: %v", err)
		}
	}
	svc := newUsageService(usageStore, st)
	path, handler := compassv1connect.NewUsageServiceHandler(svc, connect.WithInterceptors(auth.BearerInterceptor(st), auth.NewAdminGate(admin.ID)))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	httpServer := newUsageH2CServer(t, mux)
	client := newUsageH2CClient(t, httpServer)
	tokens := make(map[store.AccountID]string)
	for _, account := range []store.Account{admin, owner1, owner2, zeroOwner, a, b, c, d} {
		token, err := auth.IssueAccountToken(ctx, st, account.ID)
		if err != nil {
			t.Fatalf("IssueAccountToken(%s): %v", account.ID, err)
		}
		tokens[account.ID] = token
	}
	base := &compassv1.GetUsageSeriesRequest{StartUnixMs: start, EndUnixMs: start + 3_600_000, Granularity: compassv1.Granularity_GRANULARITY_HOUR}
	for _, tc := range []struct {
		name   string
		caller store.AccountID
		req    *compassv1.GetUsageSeriesRequest
		want   int64
		code   connect.Code
	}{
		{name: "owner all agents", caller: owner1.ID, req: base, want: 6},
		{name: "owner subtree", caller: owner1.ID, req: withUsageFilter(base, a.ID, true), want: 6},
		{name: "owner exact agent", caller: owner1.ID, req: withUsageFilter(base, a.ID, false), want: 1},
		{name: "owner rejects foreign agent", caller: owner1.ID, req: withUsageFilter(base, d.ID, false), code: connect.CodePermissionDenied},
		{name: "agent subtree", caller: b.ID, req: base, want: 5},
		{name: "agent cannot read ancestor", caller: b.ID, req: withUsageFilter(base, a.ID, false), code: connect.CodePermissionDenied},
		{name: "admin all agents", caller: admin.ID, req: base, want: 10},
		{name: "empty owner", caller: zeroOwner.ID, req: base, want: 0},
		{name: "unspecified granularity", caller: owner1.ID, req: &compassv1.GetUsageSeriesRequest{StartUnixMs: start, EndUnixMs: start + 1}, code: connect.CodeInvalidArgument},
		{name: "invalid window", caller: owner1.ID, req: &compassv1.GetUsageSeriesRequest{StartUnixMs: start, EndUnixMs: start, Granularity: compassv1.Granularity_GRANULARITY_HOUR}, code: connect.CodeInvalidArgument},
		{name: "subtree requires agent", caller: owner1.ID, req: withUsageFilter(base, "", true), code: connect.CodeInvalidArgument},
		{name: "provider filter", caller: admin.ID, req: withUsageProvider(base, "openai"), want: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := connect.NewRequest(tc.req)
			req.Header().Set("Authorization", "Bearer "+tokens[tc.caller])
			resp, err := client.GetUsageSeries(ctx, req)
			if tc.code != 0 {
				if connect.CodeOf(err) != tc.code {
					t.Fatalf("error = %v, code = %v, want %v", err, connect.CodeOf(err), tc.code)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetUsageSeries: %v", err)
			}
			if tc.want == 0 {
				if len(resp.Msg.Buckets) != 0 {
					t.Fatalf("buckets = %+v, want none", resp.Msg.Buckets)
				}
				return
			}
			if len(resp.Msg.Buckets) != 1 || resp.Msg.Buckets[0].InputTokens != tc.want {
				t.Fatalf("buckets = %+v, want one bucket with input tokens %d", resp.Msg.Buckets, tc.want)
			}
		})
	}
}

func withUsageFilter(req *compassv1.GetUsageSeriesRequest, id store.AccountID, subtree bool) *compassv1.GetUsageSeriesRequest {
	copy := *req
	copy.AgentAccountId = string(id)
	copy.IncludeSubtree = subtree
	return &copy
}

func withUsageProvider(req *compassv1.GetUsageSeriesRequest, provider string) *compassv1.GetUsageSeriesRequest {
	copy := *req
	copy.Provider = provider
	return &copy
}

func newUsageH2CClient(t *testing.T, baseURL string) compassv1connect.UsageServiceClient {
	t.Helper()
	tr := h2cTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})
	t.Cleanup(tr.CloseIdleConnections)
	return compassv1connect.NewUsageServiceClient(&http.Client{Transport: tr}, baseURL)
}

func newUsageH2CServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}
