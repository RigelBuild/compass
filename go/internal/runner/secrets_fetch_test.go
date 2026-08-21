//go:build unix

package runner

// The Runner-side FetchSecrets client: ServerLink.FetchSecrets calls the
// RunnerService over the wire and maps the wire ResolvedSecret back to the
// secrets-package resolve-surface type at this edge (mirroring runnerhub's
// resolvedSecretToProto in reverse). Every case pins a contract a plausible bug
// would break: a wrong enum mapping would misroute a secret in the materializer;
// a dropped field would lose the host/provider a routing decision needs.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/secrets"
)

// fakeFetchServer is a RunnerService handler whose FetchSecrets returns a
// scripted wire set (and records the requested session id), so the client's
// wire→pkg mapping is asserted over the real transport without a Server.
type fakeFetchServer struct {
	compassv1internalconnect.UnimplementedRunnerServiceHandler
	secrets       []*compassv1internal.ResolvedSecret
	lastSessionID string
	err           error
}

func (f *fakeFetchServer) FetchSecrets(_ context.Context, req *connect.Request[compassv1internal.FetchSecretsRequest]) (*connect.Response[compassv1internal.FetchSecretsResponse], error) {
	f.lastSessionID = req.Msg.GetSessionId()
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(&compassv1internal.FetchSecretsResponse{Secrets: f.secrets}), nil
}

// FetchAgentConfig serves the unconfigured-fleet bundle so the provision path
// (used by secrets_refresh_test's host fixture) gets past its config materialize.
func (f *fakeFetchServer) FetchAgentConfig(_ context.Context, _ *connect.Request[compassv1internal.FetchAgentConfigRequest], stream *connect.ServerStream[compassv1internal.FetchAgentConfigResponse]) error {
	return sendEmptyAgentConfig(stream)
}

// TestFetchSecretsMapsWireToResolveSurface pins the reverse edge mapping: each
// wire ResolvedSecret's delivery/kind enums are translated to the secrets-package
// enums, and value/version/host/provider ride through, so the materializer routes
// on the resolve-surface types it expects.
func TestFetchSecretsMapsWireToResolveSurface(t *testing.T) {
	server := &fakeFetchServer{secrets: []*compassv1internal.ResolvedSecret{
		{
			Name: "OPENAI", Value: "sk-openai", Version: "v1",
			Delivery: compassv1.SecretDelivery_SECRET_DELIVERY_FILE,
			Kind:     compassv1.SecretKind_SECRET_KIND_PROVIDER,
			Provider: "openai",
		},
		{
			Name: "GH", Value: "gho_tok", Version: "v2",
			Delivery: compassv1.SecretDelivery_SECRET_DELIVERY_FILE,
			Kind:     compassv1.SecretKind_SECRET_KIND_GH,
			Host:     "github.com",
		},
		{
			Name: "ENV_VAR", Value: "env-val", Version: "v3",
			Delivery: compassv1.SecretDelivery_SECRET_DELIVERY_ENV,
			Kind:     compassv1.SecretKind_SECRET_KIND_GENERIC,
		},
	}}
	client := newRunnerServiceServer(t, server)
	link := newLink(client)

	got, err := link.FetchSecrets(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("FetchSecrets = %v, want nil", err)
	}
	if server.lastSessionID != "sess-1" {
		t.Fatalf("server saw session id %q, want sess-1", server.lastSessionID)
	}
	if len(got) != 3 {
		t.Fatalf("FetchSecrets returned %d secrets, want 3", len(got))
	}

	want := []secrets.ResolvedSecret{
		{Name: "OPENAI", Value: "sk-openai", Version: "v1", Delivery: secrets.DeliveryFile, Kind: secrets.SecretProvider, Provider: "openai"},
		{Name: "GH", Value: "gho_tok", Version: "v2", Delivery: secrets.DeliveryFile, Kind: secrets.SecretGH, Host: "github.com"},
		{Name: "ENV_VAR", Value: "env-val", Version: "v3", Delivery: secrets.DeliveryEnv, Kind: secrets.SecretGeneric},
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("secret[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

// TestFetchSecretsPropagatesError: a FetchSecrets RPC failure surfaces as an
// error, never a silent empty set — a wiring/authz failure must be loud so the
// dispatch hook can log it and recover on the next signal.
func TestFetchSecretsPropagatesError(t *testing.T) {
	server := &fakeFetchServer{err: connect.NewError(connect.CodePermissionDenied, errTestDenied)}
	client := newRunnerServiceServer(t, server)
	link := newLink(client)

	_, err := link.FetchSecrets(context.Background(), "sess-1")
	if err == nil {
		t.Fatal("FetchSecrets on a denied session = nil, want an error")
	}
}

var errTestDenied = &testError{"denied"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
