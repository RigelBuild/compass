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

func (f *fakeAgentRepositoryService) GrantAgentRepository(_ context.Context, req *connect.Request[compassv1.GrantAgentRepositoryRequest]) (*connect.Response[compassv1.GrantAgentRepositoryResponse], error) {
	f.grantRequest, f.gotAuth = req.Msg, req.Header().Get("Authorization")
	if f.grantErr != nil {
		return nil, f.grantErr
	}
	return connect.NewResponse(&compassv1.GrantAgentRepositoryResponse{Added: f.grantAdded}), nil
}

func (f *fakeAgentRepositoryService) RevokeAgentRepository(_ context.Context, req *connect.Request[compassv1.RevokeAgentRepositoryRequest]) (*connect.Response[compassv1.RevokeAgentRepositoryResponse], error) {
	f.revokeRequest, f.gotAuth = req.Msg, req.Header().Get("Authorization")
	if f.revokeErr != nil {
		return nil, f.revokeErr
	}
	return connect.NewResponse(&compassv1.RevokeAgentRepositoryResponse{Removed: f.revokeRemoved}), nil
}

func (f *fakeAgentRepositoryService) ListAgentRepositories(_ context.Context, req *connect.Request[compassv1.ListAgentRepositoriesRequest]) (*connect.Response[compassv1.ListAgentRepositoriesResponse], error) {
	f.listRequest, f.gotAuth = req.Msg, req.Header().Get("Authorization")
	if f.listErr != nil {
		return nil, f.listErr
	}
	return connect.NewResponse(f.listResponse), nil
}

func startFakeAgentRepositoryServer(t *testing.T, fake *fakeAgentRepositoryService) compassv1connect.AgentRepositoryServiceClient {
	t.Helper()
	path, handler := compassv1connect.NewAgentRepositoryServiceHandler(fake)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
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
		added bool
		want  string
	}{
		{name: "added", added: true, want: "added owner/workstream\n"},
		{name: "already present", want: "already present owner/workstream\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentRepositoryService{grantAdded: tc.added}
			client := startFakeAgentRepositoryServer(t, fake)
			var out strings.Builder
			if err := runAgentRepoAdd(context.Background(), client, "owner/worker", "owner/workstream", &out); err != nil {
				t.Fatalf("runAgentRepoAdd: %v", err)
			}
			if fake.grantRequest.GetAgentHandle() != "owner/worker" || fake.grantRequest.GetRepository() != "owner/workstream" {
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
	fake := &fakeAgentRepositoryService{grantErr: connect.NewError(connect.CodePermissionDenied, errors.New("denied"))}
	var out strings.Builder
	err := runAgentRepoAdd(context.Background(), startFakeAgentRepositoryServer(t, fake), "worker", "owner/workstream", &out)
	if err == nil || out.Len() != 0 {
		t.Fatalf("server error = %v, output = %q; want error and no success output", err, out.String())
	}
}

func TestRunAgentRepoRemove(t *testing.T) {
	for _, tc := range []struct {
		name    string
		removed bool
		want    string
	}{
		{name: "removed", removed: true, want: "removed owner/workstream\n"},
		{name: "not present", want: "not present owner/workstream\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAgentRepositoryService{revokeRemoved: tc.removed}
			var out strings.Builder
			if err := runAgentRepoRemove(context.Background(), startFakeAgentRepositoryServer(t, fake), "worker", "owner/workstream", &out); err != nil {
				t.Fatalf("runAgentRepoRemove: %v", err)
			}
			if fake.revokeRequest.GetAgentHandle() != "worker" || fake.revokeRequest.GetRepository() != "owner/workstream" {
				t.Fatalf("revoke request = %v", fake.revokeRequest)
			}
			if got := out.String(); got != tc.want {
				t.Errorf("output = %q, want %q", got, tc.want)
			}
		})
	}
	fake := &fakeAgentRepositoryService{revokeErr: connect.NewError(connect.CodePermissionDenied, errors.New("denied"))}
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
	fake = &fakeAgentRepositoryService{listErr: connect.NewError(connect.CodePermissionDenied, errors.New("denied"))}
	out.Reset()
	err := runAgentRepoList(context.Background(), startFakeAgentRepositoryServer(t, fake), "worker", &out)
	if err == nil || out.Len() != 0 {
		t.Fatalf("server error = %v, output = %q; want error and no success output", err, out.String())
	}
}
