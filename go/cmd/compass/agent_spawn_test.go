//go:build unix

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
)

// spawnFakes serves the CommsService and CompassService halves of `agent spawn`
// from one httptest server, recording each request and its call order.
type spawnFakes struct {
	compassv1connect.UnimplementedCommsServiceHandler
	compass spawnCompass

	createErr error
	accounts  []*compassv1.Account
	calls     []string
	gotCreate *compassv1.CreateAgentRequest
	gotAuth   []string
}

type spawnCompass struct {
	compassv1connect.UnimplementedCompassServiceHandler
	parent   *spawnFakes
	spawnErr error
	gotSpawn []*compassv1.SpawnAgentRequest
}

func (f *spawnFakes) CreateAgent(_ context.Context, req *connect.Request[compassv1.CreateAgentRequest]) (*connect.Response[compassv1.CreateAgentResponse], error) {
	f.calls = append(f.calls, "CreateAgent")
	f.gotAuth = append(f.gotAuth, req.Header().Get("Authorization"))
	f.gotCreate = req.Msg
	if f.createErr != nil {
		return nil, f.createErr
	}
	acc := &compassv1.Account{
		Id: "agent-1", Handle: req.Msg.GetHandle(),
		Kind: &compassv1.Account_Agent{Agent: &compassv1.AgentAccount{OwnerUserId: "admin-1"}},
	}
	f.accounts = append(f.accounts, acc)
	return connect.NewResponse(&compassv1.CreateAgentResponse{Account: acc}), nil
}

func (f *spawnFakes) ListAccounts(_ context.Context, req *connect.Request[compassv1.ListAccountsRequest]) (*connect.Response[compassv1.ListAccountsResponse], error) {
	f.calls = append(f.calls, "ListAccounts")
	f.gotAuth = append(f.gotAuth, req.Header().Get("Authorization"))
	return connect.NewResponse(&compassv1.ListAccountsResponse{Accounts: f.accounts}), nil
}

func (c *spawnCompass) WhoAmI(_ context.Context, req *connect.Request[compassv1.WhoAmIRequest]) (*connect.Response[compassv1.WhoAmIResponse], error) {
	c.parent.calls = append(c.parent.calls, "WhoAmI")
	c.parent.gotAuth = append(c.parent.gotAuth, req.Header().Get("Authorization"))
	return connect.NewResponse(&compassv1.WhoAmIResponse{AccountId: "admin-1"}), nil
}

func (c *spawnCompass) SpawnAgent(_ context.Context, req *connect.Request[compassv1.SpawnAgentRequest]) (*connect.Response[compassv1.SpawnAgentResponse], error) {
	c.parent.calls = append(c.parent.calls, "SpawnAgent")
	c.parent.gotAuth = append(c.parent.gotAuth, req.Header().Get("Authorization"))
	c.gotSpawn = append(c.gotSpawn, req.Msg)
	if c.spawnErr != nil {
		return nil, c.spawnErr
	}
	return connect.NewResponse(&compassv1.SpawnAgentResponse{SessionId: "sess-1", ContainerName: "ctr-1"}), nil
}

func newSpawnFakes() *spawnFakes {
	f := &spawnFakes{accounts: []*compassv1.Account{
		{Id: "admin-1", Handle: "matt", Kind: &compassv1.Account_User{User: &compassv1.UserAccount{}}},
		{
			Id: "agent-0", Handle: "ops",
			Kind: &compassv1.Account_Agent{Agent: &compassv1.AgentAccount{OwnerUserId: "admin-1"}},
		},
	}}
	f.compass.parent = f
	return f
}

func startSpawnServer(t *testing.T, f *spawnFakes) agentSpawnClients {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(compassv1connect.NewCommsServiceHandler(f))
	mux.Handle(compassv1connect.NewCompassServiceHandler(&f.compass))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cfg := connConfig{serverAddr: srv.URL, token: "test-token"}
	comms, err := newCommsClient(cfg)
	if err != nil {
		t.Fatalf("newCommsClient: %v", err)
	}
	compass, err := newClient(cfg)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return agentSpawnClients{comms: comms, compass: compass}
}

// TestRunAgentSpawn asserts spawn creates the account, then spawns it under the
// owner-qualified handle with the request id, and prints the session.
func TestRunAgentSpawn(t *testing.T) {
	f := newSpawnFakes()
	clients := startSpawnServer(t, f)

	var out strings.Builder
	args := agentSpawnArgs{handle: "lead", displayName: "Lead", parent: "ops", requestID: "boot-1"}
	if err := runAgentSpawn(context.Background(), clients, args, &out); err != nil {
		t.Fatalf("runAgentSpawn: %v", err)
	}
	if got, want := strings.Join(f.calls, ","), "CreateAgent,WhoAmI,ListAccounts,SpawnAgent"; got != want {
		t.Errorf("call order = %s, want %s", got, want)
	}
	if c := f.gotCreate; c.GetHandle() != "lead" || c.GetDisplayName() != "Lead" || c.GetParentHandle() != "ops" {
		t.Errorf("CreateAgent = %v, want handle lead, display Lead, parent ops", c)
	}
	if len(f.compass.gotSpawn) != 1 {
		t.Fatalf("SpawnAgent calls = %d, want 1", len(f.compass.gotSpawn))
	}
	s := f.compass.gotSpawn[0]
	if s.GetAgentHandle() != "matt/lead" || s.GetClientRequestId() != "boot-1" {
		t.Errorf("SpawnAgent = %v, want agent_handle matt/lead, client_request_id boot-1", s)
	}
	for i, a := range f.gotAuth {
		if a != "Bearer test-token" {
			t.Errorf("call %d Authorization = %q, want Bearer test-token", i, a)
		}
	}
	got := out.String()
	for _, want := range []string{"matt/lead", "sess-1", "ctr-1"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing %q", got, want)
		}
	}
}

// TestRunAgentSpawnDefaults asserts the display name falls back to the handle and
// an empty --request-id mints a fresh key per run: a stable derived key would
// rejoin a stopped session from the server's spawn memo. A failed spawn names
// the key so the operator can retry with it.
func TestRunAgentSpawnDefaults(t *testing.T) {
	f := newSpawnFakes()
	f.compass.spawnErr = connect.NewError(connect.CodeDeadlineExceeded, errors.New("slow runner"))
	clients := startSpawnServer(t, f)

	args := agentSpawnArgs{handle: "lead"}
	errs := make([]error, 0, 2)
	for range 2 {
		errs = append(errs, runAgentSpawn(context.Background(), clients, args, &strings.Builder{}))
	}
	if got := f.gotCreate.GetDisplayName(); got != "lead" {
		t.Errorf("display_name = %q, want the handle", got)
	}
	first, second := f.compass.gotSpawn[0].GetClientRequestId(), f.compass.gotSpawn[1].GetClientRequestId()
	if first == "" || first == second {
		t.Errorf("client_request_id = %q then %q, want a distinct non-empty key per run", first, second)
	}
	if errs[0] == nil || !strings.Contains(errs[0].Error(), "--request-id "+first) {
		t.Errorf("spawn error = %v, want it to name --request-id %s", errs[0], first)
	}
}

// TestRunAgentSpawnExistingAccount asserts a rerun after the account already
// exists (AlreadyExists) still spawns it: the verb is safe to retry after a
// spawn failure.
func TestRunAgentSpawnExistingAccount(t *testing.T) {
	f := newSpawnFakes()
	f.createErr = connect.NewError(connect.CodeAlreadyExists, errors.New("handle taken"))
	f.accounts = append(f.accounts, &compassv1.Account{
		Id: "agent-1", Handle: "lead",
		Kind: &compassv1.Account_Agent{Agent: &compassv1.AgentAccount{OwnerUserId: "admin-1"}},
	})
	clients := startSpawnServer(t, f)

	var out strings.Builder
	if err := runAgentSpawn(context.Background(), clients, agentSpawnArgs{handle: "lead"}, &out); err != nil {
		t.Fatalf("runAgentSpawn: %v", err)
	}
	if len(f.compass.gotSpawn) != 1 || f.compass.gotSpawn[0].GetAgentHandle() != "matt/lead" {
		t.Fatalf("SpawnAgent = %v, want one spawn of matt/lead", f.compass.gotSpawn)
	}
	if !strings.Contains(out.String(), "already exists") || !strings.Contains(out.String(), "not applied") {
		t.Errorf("output %q should say the account existed and the flags were not applied", out.String())
	}
}

// TestRunAgentSpawnErrors asserts each failure stops the chain, names its step,
// and offers --request-id only when a retry with it can succeed.
func TestRunAgentSpawnErrors(t *testing.T) {
	spawnFails := func(code connect.Code) func(*spawnFakes) {
		return func(f *spawnFakes) { f.compass.spawnErr = connect.NewError(code, errors.New("spawn failed")) }
	}
	cases := []struct {
		name      string
		setup     func(*spawnFakes)
		args      agentSpawnArgs
		wantErr   []string
		wantHint  bool
		wantSpawn bool
	}{
		{
			name:    "missing handle",
			args:    agentSpawnArgs{},
			wantErr: []string{"--handle is required"},
		},
		{
			name: "create fails",
			setup: func(f *spawnFakes) {
				f.createErr = connect.NewError(connect.CodePermissionDenied, errors.New("admin only"))
			},
			args:    agentSpawnArgs{handle: "lead"},
			wantErr: []string{"creating agent", "admin only"},
		},
		{
			name: "existing account under another owner is not visible",
			setup: func(f *spawnFakes) {
				f.createErr = connect.NewError(connect.CodeAlreadyExists, errors.New("handle taken"))
				f.accounts = append(f.accounts, &compassv1.Account{
					Id: "agent-x", Handle: "lead",
					Kind: &compassv1.Account_Agent{Agent: &compassv1.AgentAccount{OwnerUserId: "other-owner"}},
				})
			},
			args:    agentSpawnArgs{handle: "lead"},
			wantErr: []string{`no agent "lead" owned by the caller is visible`, "handle taken"},
		},
		{
			name:      "spawn unavailable is retryable",
			setup:     spawnFails(connect.CodeUnavailable),
			args:      agentSpawnArgs{handle: "lead", requestID: "k1"},
			wantErr:   []string{"spawning agent matt/lead"},
			wantHint:  true,
			wantSpawn: true,
		},
		{
			name:      "already live points at status",
			setup:     spawnFails(connect.CodeAlreadyExists),
			args:      agentSpawnArgs{handle: "lead", requestID: "k1"},
			wantErr:   []string{"spawning agent matt/lead", "compass agent status"},
			wantSpawn: true,
		},
		{
			name:      "errored agent is not retryable",
			setup:     spawnFails(connect.CodeFailedPrecondition),
			args:      agentSpawnArgs{handle: "lead", requestID: "k1"},
			wantErr:   []string{"spawning agent matt/lead"},
			wantSpawn: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSpawnFakes()
			if tc.setup != nil {
				tc.setup(f)
			}
			clients := startSpawnServer(t, f)
			err := runAgentSpawn(context.Background(), clients, tc.args, &strings.Builder{})
			if err == nil {
				t.Fatal("runAgentSpawn error = nil, want a failure")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("runAgentSpawn error = %v, want it to contain %q", err, want)
				}
			}
			if hint := strings.Contains(err.Error(), "--request-id k1"); hint != tc.wantHint {
				t.Errorf("error %v offers the --request-id hint = %v, want %v", err, hint, tc.wantHint)
			}
			if spawned := len(f.compass.gotSpawn) > 0; spawned != tc.wantSpawn {
				t.Errorf("SpawnAgent called = %v, want %v", spawned, tc.wantSpawn)
			}
		})
	}
}
