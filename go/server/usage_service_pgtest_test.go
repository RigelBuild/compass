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
	"connectrpc.com/otelconnect"
	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/comms"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
	"github.com/RigelBuild/compass/go/internal/usage"
	"google.golang.org/protobuf/proto"
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
	// The store does not check that a parent has the same owner, so bad tree data is
	// possible: a foreign-owned agent under b must never widen owner1's reads.
	x, err := st.CreateAgent(ctx, owner2.ID, store.NewAgent{Handle: "x", DisplayName: "x", ParentAgentID: b.ID})
	if err != nil {
		t.Fatalf("CreateAgent x: %v", err)
	}
	if err := usage.NewPostgres(st).AppendTokenUsage(ctx, []usage.TokenUsageEvent{{
		ID: "event-x", OccurredAtUnixMs: 1_800_000_000_000, AgentAccountID: string(x.ID), OwnerUserID: string(owner2.ID),
		Provider: "anthropic", Model: "m", InputTokens: 100, Outcome: "ok",
	}}); err != nil {
		t.Fatalf("AppendTokenUsage x: %v", err)
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
			OutputTokens: int64(i + 2), CacheReadTokens: int64(i + 3), CacheWriteTokens: int64(i + 4),
			TotalTokens: int64(i + 5), CostMicroUSD: int64((i + 1) * 10), Outcome: "ok",
		}}); err != nil {
			t.Fatalf("AppendTokenUsage: %v", err)
		}
	}
	base := &compassv1.GetUsageSeriesRequest{StartUnixMs: start, EndUnixMs: start + 3_600_000, Granularity: compassv1.Granularity_GRANULARITY_HOUR}
	path, handler := compassv1connect.NewUsageServiceHandler(newUsageService(usageStore, st), connect.WithInterceptors(auth.BearerInterceptor(st), auth.NewAdminGate(admin.ID)))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	client := newUsageH2CClient(t, newUsageH2CServer(t, mux))
	tokens := make(map[store.AccountID]string)
	for _, account := range []store.Account{admin, owner1, owner2, zeroOwner, a, b, c, d} {
		token, err := auth.IssueAccountToken(ctx, st, account.ID)
		if err != nil {
			t.Fatalf("IssueAccountToken(%s): %v", account.ID, err)
		}
		tokens[account.ID] = token
	}
	cases := []usageCase{
		{name: "owner all agents", caller: owner1.ID, req: base, wantInput: 6, wantCount: 1, wantBucket: &compassv1.UsageBucket{BucketStartUnixMs: start, InputTokens: 6, OutputTokens: 9, CacheReadTokens: 12, CacheWriteTokens: 15, CostMicroUsd: 60}},
		{name: "owner mid-tree subtree", caller: owner1.ID, req: withUsageFilter(base, b.ID, true), wantInput: 5, wantCount: 1},
		{name: "owner subtree", caller: owner1.ID, req: withUsageFilter(base, a.ID, true), wantInput: 6, wantCount: 1},
		{name: "owner exact agent", caller: owner1.ID, req: withUsageFilter(base, a.ID, false), wantInput: 1, wantCount: 1},
		{name: "owner rejects foreign agent", caller: owner1.ID, req: withUsageFilter(base, d.ID, false), wantCode: connect.CodePermissionDenied},
		{name: "agent subtree", caller: b.ID, req: base, wantInput: 5, wantCount: 1},
		{name: "agent selects child", caller: b.ID, req: withUsageFilter(base, c.ID, false), wantInput: 3, wantCount: 1},
		{name: "agent selects child subtree", caller: b.ID, req: withUsageFilter(base, c.ID, true), wantInput: 3, wantCount: 1},
		{name: "agent cannot read ancestor", caller: b.ID, req: withUsageFilter(base, a.ID, false), wantCode: connect.CodePermissionDenied},
		{name: "owner subtree excludes foreign descendant", caller: owner1.ID, req: withUsageFilter(base, b.ID, true), wantInput: 5, wantCount: 1},
		{name: "agent scope excludes foreign descendant", caller: b.ID, req: withUsageFilter(base, b.ID, true), wantInput: 5, wantCount: 1},
		{name: "admin all agents", caller: admin.ID, req: base, wantInput: 110, wantCount: 1},
		{name: "admin exact filter", caller: admin.ID, req: withUsageFilter(base, c.ID, false), wantInput: 3, wantCount: 1},
		{name: "admin subtree filter", caller: admin.ID, req: withUsageFilter(base, b.ID, true), wantInput: 105, wantCount: 1},
		{name: "admin unknown agent", caller: admin.ID, req: withUsageFilter(base, "unknown-agent", false), wantCode: connect.CodeNotFound},
		{name: "non-admin unknown agent", caller: owner1.ID, req: withUsageFilter(base, "unknown-agent", false), wantCode: connect.CodePermissionDenied},
		{name: "non-admin rejects user id", caller: owner1.ID, req: withUsageFilter(base, owner1.ID, false), wantCode: connect.CodePermissionDenied},
		{name: "empty owner", caller: zeroOwner.ID, req: base, wantCount: 0},
		{name: "unspecified granularity", caller: owner1.ID, req: &compassv1.GetUsageSeriesRequest{StartUnixMs: start, EndUnixMs: start + 1}, wantCode: connect.CodeInvalidArgument},
		{name: "invalid window", caller: owner1.ID, req: &compassv1.GetUsageSeriesRequest{StartUnixMs: start, EndUnixMs: start, Granularity: compassv1.Granularity_GRANULARITY_HOUR}, wantCode: connect.CodeInvalidArgument},
		{name: "subtree requires agent", caller: owner1.ID, req: withUsageFilter(base, "", true), wantCode: connect.CodeInvalidArgument},
		{name: "provider filter", caller: admin.ID, req: withUsageProvider(base, "openai"), wantInput: 4, wantCount: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkUsageCase(t, ctx, client, tokens[tc.caller], tc)
		})
	}
}

type usageCase struct {
	name       string
	caller     store.AccountID
	req        *compassv1.GetUsageSeriesRequest
	wantInput  int64
	wantCount  int
	wantCode   connect.Code
	wantBucket *compassv1.UsageBucket
}

func checkUsageCase(t *testing.T, ctx context.Context, client compassv1connect.UsageServiceClient, token string, tc usageCase) {
	t.Helper()
	req := connect.NewRequest(proto.Clone(tc.req).(*compassv1.GetUsageSeriesRequest))
	req.Header().Set("Authorization", "Bearer "+token)
	resp, err := client.GetUsageSeries(ctx, req)
	if tc.wantCode != 0 {
		if connect.CodeOf(err) != tc.wantCode {
			t.Fatalf("error = %v, code = %v, want %v", err, connect.CodeOf(err), tc.wantCode)
		}
		return
	}
	if err != nil {
		t.Fatalf("GetUsageSeries: %v", err)
	}
	if len(resp.Msg.GetBuckets()) != tc.wantCount {
		t.Fatalf("bucket count = %d, want %d (%+v)", len(resp.Msg.GetBuckets()), tc.wantCount, resp.Msg.GetBuckets())
	}
	if tc.wantCount > 0 && resp.Msg.GetBuckets()[0].GetInputTokens() != tc.wantInput {
		t.Fatalf("input tokens = %d, want %d", resp.Msg.GetBuckets()[0].GetInputTokens(), tc.wantInput)
	}
	if tc.wantBucket != nil && !proto.Equal(resp.Msg.GetBuckets()[0], tc.wantBucket) {
		t.Fatalf("bucket = %+v, want %+v", resp.Msg.GetBuckets()[0], tc.wantBucket)
	}
}

func TestUsageSeriesNetworkDoor(t *testing.T) {
	ctx := context.Background()
	st, admin, owner := newNetworkStore(t)
	agent, err := st.CreateAgent(ctx, owner, store.NewAgent{Handle: "usage", DisplayName: "usage"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	const start = int64(1_800_000_000_000)
	if err := usage.NewPostgres(st).AppendTokenUsage(ctx, []usage.TokenUsageEvent{{
		ID: "network-event", OccurredAtUnixMs: start, AgentAccountID: string(agent.ID), OwnerUserID: string(owner), Provider: "anthropic", Model: "m",
		InputTokens: 17, OutputTokens: 19, CacheReadTokens: 23, CacheWriteTokens: 29, CostMicroUSD: 31, Outcome: "ok",
	}}); err != nil {
		t.Fatalf("AppendTokenUsage: %v", err)
	}
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(commsBus.Close)
	svc := newService("usage-test", bus, st, nil, nil, nil, nil)
	commsSvc := comms.NewComms(st, commsBus, nil, admin)
	secretsSvc := newSecretsService(st, nil, nil, nil)
	otelIC, err := otelconnect.NewInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewInterceptor: %v", err)
	}
	srv, err := buildNetworkServer(ctx, ServeConfig{StateDir: t.TempDir()}, svc, commsSvc, secretsSvc, newUsageService(usage.NewPostgres(st), st), nil, st, admin, nil, nil, otelIC, nil, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildNetworkServer: %v", err)
	}
	httpServer := httptest.NewServer(srv.Handler)
	t.Cleanup(httpServer.Close)
	token, err := auth.IssueAccountToken(ctx, st, owner)
	if err != nil {
		t.Fatalf("IssueAccountToken: %v", err)
	}
	client := compassv1connect.NewUsageServiceClient(httpServer.Client(), httpServer.URL)
	req := connect.NewRequest(&compassv1.GetUsageSeriesRequest{StartUnixMs: start, EndUnixMs: start + 3_600_000, Granularity: compassv1.Granularity_GRANULARITY_HOUR})
	req.Header().Set("Authorization", "Bearer "+token)
	resp, err := client.GetUsageSeries(ctx, req)
	if err != nil {
		t.Fatalf("network GetUsageSeries: %v", err)
	}
	if len(resp.Msg.GetBuckets()) != 1 || resp.Msg.GetBuckets()[0].GetInputTokens() != 17 {
		t.Fatalf("network buckets = %+v, want one bucket with 17 input tokens", resp.Msg.GetBuckets())
	}
}

func withUsageFilter(req *compassv1.GetUsageSeriesRequest, id store.AccountID, subtree bool) *compassv1.GetUsageSeriesRequest {
	copy := proto.Clone(req).(*compassv1.GetUsageSeriesRequest)
	copy.AgentAccountId = string(id)
	copy.IncludeSubtree = subtree
	return copy
}

func withUsageProvider(req *compassv1.GetUsageSeriesRequest, provider string) *compassv1.GetUsageSeriesRequest {
	copy := proto.Clone(req).(*compassv1.GetUsageSeriesRequest)
	copy.Provider = provider
	return copy
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
