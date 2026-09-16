//go:build unix

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/store"
)

// fakeServerSecrets is a fake SecretsService handler returning a canned
// ListServerSecrets response, so the list subcommand's RPC wiring is tested
// without a live Server or Postgres (mirroring fakeSecrets for the user-facing
// verbs).
type fakeServerSecrets struct {
	compassv1connect.UnimplementedSecretsServiceHandler
	list    *compassv1.ListServerSecretsResponse
	gotAuth string
}

func (f *fakeServerSecrets) ListServerSecrets(_ context.Context, req *connect.Request[compassv1.ListServerSecretsRequest]) (*connect.Response[compassv1.ListServerSecretsResponse], error) {
	f.gotAuth = req.Header().Get("Authorization")
	list := f.list
	if list == nil {
		list = &compassv1.ListServerSecretsResponse{}
	}
	return connect.NewResponse(list), nil
}

// startFakeServerSecretsServer stands up the fake SecretsService over a
// plain-HTTP httptest server and returns a client wired to it with the bearer
// interceptor.
func startFakeServerSecretsServer(t *testing.T, fake *fakeServerSecrets) compassv1connect.SecretsServiceClient {
	t.Helper()
	path, handler := compassv1connect.NewSecretsServiceHandler(fake)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := newSecretsClient(connConfig{serverAddr: srv.URL, token: "test-token"})
	if err != nil {
		t.Fatalf("newSecretsClient: %v", err)
	}
	return client
}

// TestRunServerSecretList asserts list renders "<BARE_NAME>: set|unset" with the
// reserved prefix stripped and the bare name at LINE START — the exact shape the
// deployment's seed script matches with a "\n<NAME>: " glob — and that declared
// but unpopulated names render unset (declared != set on the server path).
func TestRunServerSecretList(t *testing.T) {
	fake := &fakeServerSecrets{list: &compassv1.ListServerSecretsResponse{ServerSecrets: []*compassv1.ServerSecretStatus{
		{Name: "SERVER_FORGE_APP_PEM", IsSet: true},
		{Name: "SERVER_LINEAR_WEBHOOK_SECRET", IsSet: false},
	}}}
	client := startFakeServerSecretsServer(t, fake)

	var out strings.Builder
	if err := runServerSecretList(context.Background(), client, &out); err != nil {
		t.Fatalf("runServerSecretList: %v", err)
	}
	got := out.String()
	// The seed script's glob is line-anchored, so assert the newline-prefixed
	// form. Prefixing the whole output with "\n" lets the first line match the
	// same anchored pattern as every later one.
	for _, want := range []string{
		"\nFORGE_APP_PEM: set\n",
		"\nLINEAR_WEBHOOK_SECRET: unset\n",
	} {
		if !strings.Contains("\n"+got, want) {
			t.Errorf("list output %q is missing line-anchored %q", got, want)
		}
	}
	if strings.Contains(got, "SERVER_") {
		t.Errorf("list output %q leaks the reserved prefix; the bare name must start the line", got)
	}
	if fake.gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want Bearer test-token", fake.gotAuth)
	}
}

// TestRunServerSecretListStripsCompassPrefix pins that the master-key prefix is
// stripped too. The master key is a real server_secrets row
// (store.MasterKeyName, COMPASS_-prefixed), so it lists here; stripping only
// SERVER_ would print it prefixed while every sibling printed bare, and the
// seed script's line-anchored glob would miss it.
func TestRunServerSecretListStripsCompassPrefix(t *testing.T) {
	fake := &fakeServerSecrets{list: &compassv1.ListServerSecretsResponse{ServerSecrets: []*compassv1.ServerSecretStatus{
		{Name: store.MasterKeyName, IsSet: true},
	}}}
	client := startFakeServerSecretsServer(t, fake)

	var out strings.Builder
	if err := runServerSecretList(context.Background(), client, &out); err != nil {
		t.Fatalf("runServerSecretList: %v", err)
	}
	got := out.String()
	if want := "\nMASTER_KEY: set\n"; !strings.Contains("\n"+got, want) {
		t.Errorf("list output %q is missing line-anchored %q", got, want)
	}
	if strings.Contains(got, store.CompassPrefix) {
		t.Errorf("list output %q leaks the reserved prefix; the bare name must start the line", got)
	}
}

// TestRunServerSecretListEmpty asserts an empty registry renders a clear
// message, not an error.
func TestRunServerSecretListEmpty(t *testing.T) {
	fake := &fakeServerSecrets{}
	client := startFakeServerSecretsServer(t, fake)

	var out strings.Builder
	if err := runServerSecretList(context.Background(), client, &out); err != nil {
		t.Fatalf("runServerSecretList: %v", err)
	}
	if !strings.Contains(out.String(), "no server secrets declared") {
		t.Errorf("empty-list output %q does not report an empty registry", out.String())
	}
}
