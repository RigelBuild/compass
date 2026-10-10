//go:build pgtest && unix

package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

type recordingAgentRepoSignaler struct {
	accounts []store.AccountID
	err      error
}

func (s *recordingAgentRepoSignaler) SignalSecretsVersionFor(_ context.Context, account store.AccountID) error {
	s.accounts = append(s.accounts, account)
	return s.err
}

type agentRepositoryFixture struct {
	client     compassv1connect.AgentRepositoryServiceClient
	store      *store.Store
	dsn        string
	admin      store.AccountID
	owner      store.AccountID
	outsider   store.AccountID
	agent      store.AccountID
	adminToken string
	ownerToken string
	agentToken string
	otherToken string
	signaler   *recordingAgentRepoSignaler
}

func newAgentRepositoryFixture(t *testing.T) agentRepositoryFixture {
	t.Helper()
	ctx := context.Background()
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)

	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "repo-admin", DisplayName: "Admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	owner, err := st.CreateUser(ctx, store.NewUser{Handle: "repo-owner", DisplayName: "Owner"})
	if err != nil {
		t.Fatalf("CreateUser(owner): %v", err)
	}
	outsider, err := st.CreateUser(ctx, store.NewUser{Handle: "repo-outsider", DisplayName: "Outsider"})
	if err != nil {
		t.Fatalf("CreateUser(outsider): %v", err)
	}
	agent, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "worker", DisplayName: "Worker"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := st.GrantForgeScope(ctx, store.ForgeScope{AccountID: owner.ID, Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "owner/base"}); err != nil {
		t.Fatalf("GrantForgeScope(owner base): %v", err)
	}

	issue := func(account store.AccountID) string {
		t.Helper()
		token, err := auth.IssueAccountToken(ctx, st, account)
		if err != nil {
			t.Fatalf("IssueAccountToken(%s): %v", account, err)
		}
		return token
	}
	signaler := &recordingAgentRepoSignaler{}
	svc := newAgentRepositoryService(st, &gitCredentialBroker{host: "github.com"}, signaler)
	url := newAgentRepositoryH2CServer(t, svc, st, admin.ID)
	client := compassv1connect.NewAgentRepositoryServiceClient(newAgentRepositoryH2CClient(t), url)
	return agentRepositoryFixture{
		client: client, store: st, dsn: dsn, admin: admin.ID, owner: owner.ID,
		outsider: outsider.ID, agent: agent.ID,
		adminToken: issue(admin.ID), ownerToken: issue(owner.ID),
		agentToken: issue(agent.ID), otherToken: issue(outsider.ID), signaler: signaler,
	}
}

func newAgentRepositoryH2CServer(t *testing.T, svc compassv1connect.AgentRepositoryServiceHandler, st *store.Store, admin store.AccountID) string {
	t.Helper()
	path, handler := compassv1connect.NewAgentRepositoryServiceHandler(svc,
		connect.WithInterceptors(auth.BearerInterceptor(st), auth.NewAdminGate(admin)))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewUnstartedServer(mux)
	server.Config.Protocols = cleartextHTTP2()
	server.Start()
	t.Cleanup(server.Close)
	return server.URL
}

func withAgentRepositoryToken[T any](token string, req *connect.Request[T]) *connect.Request[T] {
	req.Header().Set("Authorization", "Bearer "+token)
	return req
}

func seedTenant(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) store.TenantID {
	t.Helper()
	if _, err := pool.Exec(ctx,
		"INSERT INTO tenants (id, slug, display_name, created_at_unix_ms) VALUES ($1, $2, $3, $4)",
		id, id, "Agent repository tenant", time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("seed tenant %q: %v", id, err)
	}
	return store.TenantID(id)
}

func newAgentRepositoryH2CClient(t *testing.T) *http.Client {
	t.Helper()
	transport := h2cTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, addr)
	})
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}

func grantAgentRepository(client compassv1connect.AgentRepositoryServiceClient, ctx context.Context, token, handle, repository string) (*connect.Response[compassv1.GrantAgentRepositoryResponse], error) {
	return client.GrantAgentRepository(ctx, withAgentRepositoryToken(token,
		connect.NewRequest(&compassv1.GrantAgentRepositoryRequest{AgentHandle: handle, Repository: repository})))
}

func revokeAgentRepository(client compassv1connect.AgentRepositoryServiceClient, ctx context.Context, token, handle, repository string) (*connect.Response[compassv1.RevokeAgentRepositoryResponse], error) {
	return client.RevokeAgentRepository(ctx, withAgentRepositoryToken(token,
		connect.NewRequest(&compassv1.RevokeAgentRepositoryRequest{AgentHandle: handle, Repository: repository})))
}

func listAgentRepositories(client compassv1connect.AgentRepositoryServiceClient, ctx context.Context, token, handle string) (*connect.Response[compassv1.ListAgentRepositoriesResponse], error) {
	return client.ListAgentRepositories(ctx, withAgentRepositoryToken(token,
		connect.NewRequest(&compassv1.ListAgentRepositoriesRequest{AgentHandle: handle})))
}

func TestAgentRepositoryServiceOwnerResolvesAndNormalizes(t *testing.T) {
	f := newAgentRepositoryFixture(t)
	ctx := context.Background()

	added, err := grantAgentRepository(f.client, ctx, f.ownerToken, "worker", "Owner/WorkStream")
	if err != nil || !added.Msg.GetAdded() {
		t.Fatalf("bare grant = (%v, %v), want added", added, err)
	}
	if !slices.Equal(f.signaler.accounts, []store.AccountID{f.agent}) {
		t.Fatalf("signals = %v, want one signal for target agent %q", f.signaler.accounts, f.agent)
	}
	listed, err := listAgentRepositories(f.client, ctx, f.ownerToken, "repo-owner/worker")
	if err != nil || !slices.Equal(listed.Msg.GetRepositories(), []string{"owner/workstream"}) {
		t.Fatalf("qualified list = (%v, %v), want [owner/workstream]", listed, err)
	}
	repeated, err := grantAgentRepository(f.client, ctx, f.ownerToken, "repo-owner/worker", "OWNER/WORKSTREAM")
	if err != nil || repeated.Msg.GetAdded() {
		t.Fatalf("repeat grant = (%v, %v), want added=false", repeated, err)
	}
	if len(f.signaler.accounts) != 1 {
		t.Fatalf("signals after repeat = %v, want no extra signal", f.signaler.accounts)
	}
}

func TestAgentRepositoryServiceRejectsInvalidRepositories(t *testing.T) {
	f := newAgentRepositoryFixture(t)
	for _, tc := range []struct{ name, repository string }{
		{name: "wildcard", repository: "*"},
		{name: "missing slash", repository: "org"},
		{name: "empty repository", repository: "org/"},
		{name: "nested path", repository: "a/b/c"},
		{name: "empty", repository: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := grantAgentRepository(f.client, context.Background(), f.ownerToken, "worker", tc.repository)
			if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
				t.Fatalf("grant %q code = %v (%v), want InvalidArgument", tc.repository, got, err)
			}
		})
	}
	if len(f.signaler.accounts) != 0 {
		t.Fatalf("invalid grants emitted signals: %v", f.signaler.accounts)
	}
}

func TestAgentRepositoryServiceConcealsForeignAndNonAgentTargets(t *testing.T) {
	f := newAgentRepositoryFixture(t)
	cases := []struct{ name, token, handle string }{
		{name: "another user", token: f.otherToken, handle: "repo-owner/worker"},
		{name: "admin has no override", token: f.adminToken, handle: "repo-owner/worker"},
		{name: "unknown", token: f.ownerToken, handle: "missing"},
		{name: "user is not an agent", token: f.ownerToken, handle: "repo-owner"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertAgentRepositoryTargetNotFound(t, f.client, context.Background(), tc.token, tc.handle)
		})
	}
	if len(f.signaler.accounts) != 0 {
		t.Fatalf("foreign-handle writes emitted signals: %v", f.signaler.accounts)
	}
}

func assertAgentRepositoryTargetNotFound(t *testing.T, client compassv1connect.AgentRepositoryServiceClient, ctx context.Context, token, handle string) {
	t.Helper()
	for _, op := range []string{"grant", "revoke", "list"} {
		t.Run(op, func(t *testing.T) {
			var err error
			switch op {
			case "grant":
				_, err = grantAgentRepository(client, ctx, token, handle, "owner/no-write")
			case "revoke":
				_, err = revokeAgentRepository(client, ctx, token, handle, "owner/workstream")
			case "list":
				_, err = listAgentRepositories(client, ctx, token, handle)
			}
			if got := connect.CodeOf(err); got != connect.CodeNotFound || !strings.Contains(err.Error(), handle) {
				t.Fatalf("%s %q = %v, want NotFound naming submitted handle", op, handle, err)
			}
		})
	}
}

func TestAgentRepositoryServiceAgentCanOnlyListItself(t *testing.T) {
	f := newAgentRepositoryFixture(t)
	ctx := context.Background()
	if _, err := grantAgentRepository(f.client, ctx, f.ownerToken, "worker", "owner/workstream"); err != nil {
		t.Fatalf("GrantAgentRepository: %v", err)
	}
	listed, err := listAgentRepositories(f.client, ctx, f.agentToken, "")
	if err != nil || !slices.Equal(listed.Msg.GetRepositories(), []string{"owner/workstream"}) {
		t.Fatalf("agent self-list = (%v, %v), want its own row", listed, err)
	}
	if _, err := grantAgentRepository(f.client, ctx, f.agentToken, "worker", "owner/other"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("agent grant code = %v (%v), want PermissionDenied", connect.CodeOf(err), err)
	}
	if _, err := revokeAgentRepository(f.client, ctx, f.agentToken, "", "owner/workstream"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("agent revoke code = %v (%v), want PermissionDenied", connect.CodeOf(err), err)
	}
	if _, err := listAgentRepositories(f.client, ctx, f.agentToken, "worker"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("agent list with handle code = %v (%v), want PermissionDenied", connect.CodeOf(err), err)
	}
	if !slices.Equal(f.signaler.accounts, []store.AccountID{f.agent}) {
		t.Fatalf("agent self-writes emitted signals: %v", f.signaler.accounts)
	}
}

func TestAgentRepositoryServiceOrgCheck(t *testing.T) {
	f := newAgentRepositoryFixture(t)
	ctx := context.Background()

	added, err := grantAgentRepository(f.client, ctx, f.ownerToken, "worker", "OWNER/WORKSTREAM")
	if err != nil || !added.Msg.GetAdded() {
		t.Fatalf("grant in same org = (%v, %v), want added", added, err)
	}
	if _, err := grantAgentRepository(f.client, ctx, f.ownerToken, "worker", "another/rejected"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("grant with a different exact org = %v, want FailedPrecondition", err)
	}
	rows, err := f.store.ListAgentForgeScopeRepos(ctx, f.agent, store.ForgeProviderGitHub, "github.com")
	if err != nil || !slices.Contains(rows, "owner/workstream") || slices.Contains(rows, "another/rejected") {
		t.Fatalf("agent rows after rejected mismatch = %v, %v, want existing row and no mismatched row", rows, err)
	}
	if err := f.store.GrantForgeScope(ctx, store.ForgeScope{
		AccountID: f.owner, Provider: store.ForgeProviderGitHub, Host: "github.com", Repo: "*",
	}); err != nil {
		t.Fatalf("GrantForgeScope(owner wildcard): %v", err)
	}
	if added, err := grantAgentRepository(f.client, ctx, f.ownerToken, "worker", "another/repository"); err != nil || !added.Msg.GetAdded() {
		t.Fatalf("grant under owner wildcard = (%v, %v), want added", added, err)
	}

	emptyOwner, err := f.store.CreateUser(ctx, store.NewUser{Handle: "repo-empty-owner", DisplayName: "Empty owner"})
	if err != nil {
		t.Fatalf("CreateUser(empty owner): %v", err)
	}
	emptyAgent, err := f.store.CreateAgent(ctx, emptyOwner.ID, store.NewAgent{Handle: "repo-empty-agent", DisplayName: "Empty agent"})
	if err != nil {
		t.Fatalf("CreateAgent(empty): %v", err)
	}
	emptyToken, err := auth.IssueAccountToken(ctx, f.store, emptyOwner.ID)
	if err != nil {
		t.Fatalf("IssueAccountToken(empty owner): %v", err)
	}
	if added, err := grantAgentRepository(f.client, ctx, emptyToken, "repo-empty-agent", "other-org/repository"); err != nil || !added.Msg.GetAdded() {
		t.Fatalf("grant with empty effective scope = (%v, %v), want added", added, err)
	}
	emptyRows, err := f.store.ListAgentForgeScopeRepos(ctx, emptyAgent.ID, store.ForgeProviderGitHub, "github.com")
	if err != nil || !slices.Contains(emptyRows, "other-org/repository") {
		t.Fatalf("empty-scope agent rows = %v, %v, want [other-org/repository]", emptyRows, err)
	}
}

func TestAgentRepositoryServiceTenantIsolation(t *testing.T) {
	f := newAgentRepositoryFixture(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, f.dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	tenantB := store.WithTenant(ctx, seedTenant(t, ctx, pool, "agent-repository-tenant-"+strconv.FormatInt(time.Now().UnixNano(), 10)))
	caller, err := f.store.CreateUser(tenantB, store.NewUser{Handle: "repo-tenant-b-user", DisplayName: "Tenant B user"})
	if err != nil {
		t.Fatalf("CreateUser(tenant B): %v", err)
	}
	token, err := auth.IssueAccountToken(tenantB, f.store, caller.ID)
	if err != nil {
		t.Fatalf("IssueAccountToken(tenant B): %v", err)
	}
	for _, handle := range []string{"repo-owner/worker", "worker"} {
		t.Run(handle, func(t *testing.T) {
			assertAgentRepositoryTargetNotFound(t, f.client, ctx, token, handle)
		})
	}
}

func TestAgentRepositoryServiceRevokeSignalsOnlyOnChange(t *testing.T) {
	f := newAgentRepositoryFixture(t)
	ctx := context.Background()
	if _, err := grantAgentRepository(f.client, ctx, f.ownerToken, "worker", "owner/workstream"); err != nil {
		t.Fatalf("GrantAgentRepository: %v", err)
	}
	before := len(f.signaler.accounts)
	removed, err := revokeAgentRepository(f.client, ctx, f.ownerToken, "worker", "OWNER/WORKSTREAM")
	if err != nil || !removed.Msg.GetRemoved() {
		t.Fatalf("revoke = (%v, %v), want removed", removed, err)
	}
	if len(f.signaler.accounts) != before+1 || f.signaler.accounts[len(f.signaler.accounts)-1] != f.agent {
		t.Fatalf("signals after revoke = %v, want one signal for %q", f.signaler.accounts, f.agent)
	}
	missing, err := revokeAgentRepository(f.client, ctx, f.ownerToken, "worker", "owner/workstream")
	if err != nil || missing.Msg.GetRemoved() {
		t.Fatalf("repeat revoke = (%v, %v), want removed=false", missing, err)
	}
	if len(f.signaler.accounts) != before+1 {
		t.Fatalf("repeat revoke emitted signal: %v", f.signaler.accounts)
	}
}

func TestAgentRepositoryServiceRequiresCallerAndApp(t *testing.T) {
	f := newAgentRepositoryFixture(t)
	ctx := context.Background()
	noAuthURL := newAgentRepositoryH2CServer(t,
		newAgentRepositoryService(f.store, &gitCredentialBroker{host: "github.com"}, nil), f.store, f.admin)
	noAuthClient := compassv1connect.NewAgentRepositoryServiceClient(newAgentRepositoryH2CClient(t), noAuthURL)
	_, err := grantAgentRepository(noAuthClient, ctx, "", "missing", "bad")
	if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
		t.Fatalf("grant without caller code = %v (%v), want Unauthenticated", got, err)
	}

	nilURL := newAgentRepositoryH2CServer(t, newAgentRepositoryService(f.store, nil, nil), f.store, f.admin)
	nilClient := compassv1connect.NewAgentRepositoryServiceClient(newAgentRepositoryH2CClient(t), nilURL)
	_, err = grantAgentRepository(nilClient, ctx, f.ownerToken, "missing", "bad")
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("grant without app code = %v (%v), want FailedPrecondition", got, err)
	}
}
