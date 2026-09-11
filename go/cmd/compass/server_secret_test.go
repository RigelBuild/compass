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
)

// fakeServerSecrets is a fake SecretsService handler recording the request each
// server-secret verb constructs and returning a canned ListServerSecrets
// response, so the subcommand RPC wiring is tested without a live Server or
// Postgres (mirroring fakeSecrets for the user-facing verbs).
type fakeServerSecrets struct {
	compassv1connect.UnimplementedSecretsServiceHandler
	gotSet   *compassv1.SetServerSecretRequest
	setCalls int
	list     *compassv1.ListServerSecretsResponse
	gotAuth  string
}

func (f *fakeServerSecrets) SetServerSecret(_ context.Context, req *connect.Request[compassv1.SetServerSecretRequest]) (*connect.Response[compassv1.SetServerSecretResponse], error) {
	f.setCalls++
	f.gotSet = req.Msg
	f.gotAuth = req.Header().Get("Authorization")
	return connect.NewResponse(&compassv1.SetServerSecretResponse{}), nil
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

// TestRunServerSecretListStripsGatewayPrefix pins that the OTHER reserved
// prefix is stripped too. The master-key family is a real server_secrets row
// (secrets_service.go's masterKeyName), so it lists here; stripping only
// SERVER_ would print it prefixed while every sibling printed bare, and the
// seed script's line-anchored glob would miss it.
func TestRunServerSecretListStripsGatewayPrefix(t *testing.T) {
	fake := &fakeServerSecrets{list: &compassv1.ListServerSecretsResponse{ServerSecrets: []*compassv1.ServerSecretStatus{
		{Name: "GATEWAY_CREDENTIALS_MASTER_KEY", IsSet: true},
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
	if strings.Contains(got, "GATEWAY_CREDENTIALS_") {
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

// TestRunServerSecretSetPrefixesName asserts the value comes from stdin (never
// argv) and that a bare operator-facing name is sent PREFIXED on the wire, while
// an already-prefixed name is not double-prefixed.
func TestRunServerSecretSetPrefixesName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "bare name is prefixed", input: "FORGE_APP_PEM", want: "SERVER_FORGE_APP_PEM"},
		{name: "prefixed name is unchanged", input: "SERVER_FORGE_APP_PEM", want: "SERVER_FORGE_APP_PEM"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeServerSecrets{}
			client := startFakeServerSecretsServer(t, fake)

			var out strings.Builder
			in := strings.NewReader("s3cr3t\n")
			if err := runServerSecretSet(context.Background(), client, tt.input, in, &out); err != nil {
				t.Fatalf("runServerSecretSet: %v", err)
			}
			if fake.gotSet == nil {
				t.Fatal("SetServerSecret was not called")
			}
			if fake.gotSet.GetName() != tt.want {
				t.Errorf("name = %q, want %q", fake.gotSet.GetName(), tt.want)
			}
			if fake.gotSet.GetValue() != "s3cr3t" {
				t.Errorf("value = %q, want s3cr3t (trailing newline trimmed, from stdin)", fake.gotSet.GetValue())
			}
			if fake.gotAuth != "Bearer test-token" {
				t.Errorf("Authorization = %q, want Bearer test-token", fake.gotAuth)
			}
		})
	}
}

// TestRunServerSecretSetRefusesBareMasterKey pins the round-trip hazard: `list`
// strips either reserved prefix, so the master key prints as bare MASTER_KEY.
// Wrapping that spelling would send SERVER_MASTER_KEY — a DIFFERENT secret that
// clears the server's exact-name master-key guard, minting a shadow row while
// the real key stays unprovisioned and `list` prints the same bare name twice.
// It must be refused before any RPC.
func TestRunServerSecretSetRefusesBareMasterKey(t *testing.T) {
	fake := &fakeServerSecrets{}
	client := startFakeServerSecretsServer(t, fake)

	var out strings.Builder
	in := strings.NewReader("s3cr3t\n")
	err := runServerSecretSet(context.Background(), client, "MASTER_KEY", in, &out)
	if err == nil {
		t.Fatal("bare MASTER_KEY was accepted; it must be refused rather than re-prefixed to a different secret")
	}
	if fake.gotSet != nil {
		t.Errorf("SetServerSecret was called with %q; the refusal must precede any RPC", fake.gotSet.GetName())
	}
	if !strings.Contains(err.Error(), masterKeyCLIName) {
		t.Errorf("error %q does not name %s, so it is not actionable", err, masterKeyCLIName)
	}
}

// TestRunServerSecretSetAcceptsFullGatewayName asserts the refusal is narrow:
// the FULL gateway name still reaches the server, which is what fail-closes on
// it (secrets_service.go's masterKeyName guard). The CLI must not become a
// second, divergent authority on which names are writable.
func TestRunServerSecretSetAcceptsFullGatewayName(t *testing.T) {
	fake := &fakeServerSecrets{}
	client := startFakeServerSecretsServer(t, fake)

	var out strings.Builder
	in := strings.NewReader("s3cr3t\n")
	if err := runServerSecretSet(context.Background(), client, masterKeyCLIName, in, &out); err != nil {
		t.Fatalf("runServerSecretSet: %v", err)
	}
	if fake.gotSet == nil {
		t.Fatal("SetServerSecret was not called; the server must be the authority on this refusal")
	}
	if fake.gotSet.GetName() != masterKeyCLIName {
		t.Errorf("name = %q, want %q unchanged", fake.gotSet.GetName(), masterKeyCLIName)
	}
}

// TestRunServerSecretSetEmptyStdin asserts an empty stdin value is rejected with
// the shared empty-value error BEFORE any RPC — a blank pipe must never clear a
// populated server secret.
func TestRunServerSecretSetEmptyStdin(t *testing.T) {
	fake := &fakeServerSecrets{}
	client := startFakeServerSecretsServer(t, fake)

	var out strings.Builder
	err := runServerSecretSet(context.Background(), client, "FORGE_APP_PEM", strings.NewReader("\n"), &out)
	if err == nil {
		t.Fatal("runServerSecretSet with empty stdin = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "value is required") {
		t.Errorf("error %q does not mention the required value", err.Error())
	}
	if fake.setCalls != 0 {
		t.Errorf("SetServerSecret called %d times despite an empty value", fake.setCalls)
	}
}
