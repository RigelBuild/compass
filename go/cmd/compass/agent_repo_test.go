//go:build unix

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
)

type fakeAgentRepositoryService struct {
	compassv1connect.UnimplementedAgentRepositoryServiceHandler
	grantRequest  *compassv1.GrantAgentRepositoryRequest
	grantAdded    bool
	grantErr      error
	revokeRequest *compassv1.RevokeAgentRepositoryRequest
	revokeRemoved bool
	revokeErr     error
	listRequest   *compassv1.ListAgentRepositoriesRequest
	listResponse  *compassv1.ListAgentRepositoriesResponse
	listErr       error
	gotAuth       string
}

func (f *fakeAgentRepositoryService) GrantAgentRepository(ctx context.Context, req *compassv1.GrantAgentRepositoryRequest) (*compassv1.GrantAgentRepositoryResponse, error) {
	f.grantRequest, f.gotAuth = req, requestAuth(ctx)
	if f.grantErr != nil {
		return nil, f.grantErr
	}
	return &compassv1.GrantAgentRepositoryResponse{Added: f.grantAdded}, nil
}

func (f *fakeAgentRepositoryService) RevokeAgentRepository(ctx context.Context, req *compassv1.RevokeAgentRepositoryRequest) (*compassv1.RevokeAgentRepositoryResponse, error) {
	f.revokeRequest, f.gotAuth = req, requestAuth(ctx)
	if f.revokeErr != nil {
		return nil, f.revokeErr
	}
	return &compassv1.RevokeAgentRepositoryResponse{Removed: f.revokeRemoved}, nil
}

func (f *fakeAgentRepositoryService) ListAgentRepositories(ctx context.Context, req *compassv1.ListAgentRepositoriesRequest) (*compassv1.ListAgentRepositoriesResponse, error) {
	f.listRequest, f.gotAuth = req, requestAuth(ctx)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listResponse, nil
}

func startFakeAgentRepositoryServer(t *testing.T, fake *fakeAgentRepositoryService) compassv1connect.AgentRepositoryServiceClient {
	t.Helper()
	mux := http.NewServeMux()
	server := connect.NewServer()
	compassv1connect.RegisterAgentRepositoryServiceHandler(server, fake)
	connecthttp.Mount(mux, server, connecthttp.WithReadMaxBytes(0))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := newAgentRepositoryClient(connConfig{serverAddr: srv.URL, token: "test-token"})
	if err != nil {
		t.Fatalf("newAgentRepositoryClient: %v", err)
	}
	return client
}

func TestRunAgentRepoAdd(t *testing.T) {
	for _, tc := range []struct {
		name  string
		repo  string
		added bool
		want  string
	}{
		{name: "added", repo: "OWNER/WorkStream", added: true, want: "added owner/workstream\n"},
		{name: "already present", repo: "OWNER/WorkStream", want: "already present owner/workstream\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentRepositoryService{grantAdded: tc.added}
			client := startFakeAgentRepositoryServer(t, fake)
			var out strings.Builder
			if err := runAgentRepoAdd(context.Background(), client, "owner/worker", tc.repo, &out); err != nil {
				t.Fatalf("runAgentRepoAdd: %v", err)
			}
			if fake.grantRequest.GetAgentHandle() != "owner/worker" || fake.grantRequest.GetRepository() != tc.repo {
				t.Fatalf("grant request = %v", fake.grantRequest)
			}
			if fake.gotAuth != "Bearer test-token" {
				t.Errorf("Authorization = %q", fake.gotAuth)
			}
			if got := out.String(); got != tc.want {
				t.Errorf("output = %q, want %q", got, tc.want)
			}
		})
	}
	fake := &fakeAgentRepositoryService{grantErr: connect.NewError(connect.CodePermissionDenied, "denied")}
	var out strings.Builder
	err := runAgentRepoAdd(context.Background(), startFakeAgentRepositoryServer(t, fake), "worker", "owner/workstream", &out)
	if err == nil || out.Len() != 0 {
		t.Fatalf("server error = %v, output = %q; want error and no success output", err, out.String())
	}
}

func TestRunAgentRepoRemove(t *testing.T) {
	for _, tc := range []struct {
		name    string
		repo    string
		removed bool
		want    string
	}{
		{name: "removed", repo: "OWNER/WorkStream", removed: true, want: "removed owner/workstream\n"},
		{name: "not present", repo: "OWNER/WorkStream", want: "not present owner/workstream\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentRepositoryService{revokeRemoved: tc.removed}
			var out strings.Builder
			if err := runAgentRepoRemove(context.Background(), startFakeAgentRepositoryServer(t, fake), "worker", tc.repo, &out); err != nil {
				t.Fatalf("runAgentRepoRemove: %v", err)
			}
			if fake.revokeRequest.GetAgentHandle() != "worker" || fake.revokeRequest.GetRepository() != tc.repo {
				t.Fatalf("revoke request = %v", fake.revokeRequest)
			}
			if got := out.String(); got != tc.want {
				t.Errorf("output = %q, want %q", got, tc.want)
			}
		})
	}
	fake := &fakeAgentRepositoryService{revokeErr: connect.NewError(connect.CodePermissionDenied, "denied")}
	var out strings.Builder
	err := runAgentRepoRemove(context.Background(), startFakeAgentRepositoryServer(t, fake), "worker", "owner/workstream", &out)
	if err == nil || out.Len() != 0 {
		t.Fatalf("server error = %v, output = %q; want error and no success output", err, out.String())
	}
}

func TestRunAgentRepoList(t *testing.T) {
	fake := &fakeAgentRepositoryService{listResponse: &compassv1.ListAgentRepositoriesResponse{Repositories: []string{"owner/alpha", "owner/beta"}}}
	var out strings.Builder
	if err := runAgentRepoList(context.Background(), startFakeAgentRepositoryServer(t, fake), "owner/worker", &out); err != nil {
		t.Fatalf("runAgentRepoList: %v", err)
	}
	if fake.listRequest.GetAgentHandle() != "owner/worker" {
		t.Fatalf("list request = %v", fake.listRequest)
	}
	if fake.gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q", fake.gotAuth)
	}
	if got, want := out.String(), "owner/alpha\nowner/beta\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	fake = &fakeAgentRepositoryService{listErr: connect.NewError(connect.CodePermissionDenied, "denied")}
	out.Reset()
	err := runAgentRepoList(context.Background(), startFakeAgentRepositoryServer(t, fake), "worker", &out)
	if err == nil || out.Len() != 0 {
		t.Fatalf("server error = %v, output = %q; want error and no success output", err, out.String())
	}
}

func TestAgentRepoCommandTree(t *testing.T) {
	root := newRootCmd()
	for _, tc := range []struct {
		verb  string
		use   string
		valid []string
		bad   [][]string
	}{
		{verb: "add", use: "add <agent> <org/name>", valid: []string{"worker", "owner/repo"}, bad: [][]string{{"worker"}, {"worker", "owner/repo", "extra"}}},
		{verb: "remove", use: "remove <agent> <org/name>", valid: []string{"worker", "owner/repo"}, bad: [][]string{{"worker"}, {"worker", "owner/repo", "extra"}}},
		{verb: "list", use: "list <agent>", valid: []string{"worker"}, bad: [][]string{{}, {"worker", "extra"}}},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			cmd, _, err := root.Find([]string{"agent", "repo", tc.verb})
			if err != nil || cmd.Name() != tc.verb || cmd.Use != tc.use {
				t.Fatalf("command lookup = (%v, %v), want agent repo %s with Use %q", cmd, err, tc.verb, tc.use)
			}
			if cmd.Args == nil {
				t.Fatalf("%s has no argument validator", tc.verb)
			}
			if err := cmd.Args(cmd, tc.valid); err != nil {
				t.Fatalf("accepted args %v: %v", tc.valid, err)
			}
			for _, args := range tc.bad {
				if err := cmd.Args(cmd, args); err == nil {
					t.Errorf("accepted args %v, want argument-count rejection", args)
				}
			}
		})
	}
}
