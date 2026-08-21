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

// fakeSecrets is a fake SecretsService handler recording the request each verb
// constructs and returning a canned ListSecrets response, so the secret
// subcommand RPC wiring is tested without a live Server or Postgres.
type fakeSecrets struct {
	compassv1connect.UnimplementedSecretsServiceHandler
	gotSet      *compassv1.SetSecretRequest
	list        *compassv1.ListSecretsResponse
	gotDelete   string
	deleteCalls int
	gotAuth     string
}

func (f *fakeSecrets) SetSecret(_ context.Context, req *connect.Request[compassv1.SetSecretRequest]) (*connect.Response[compassv1.SetSecretResponse], error) {
	f.gotSet = req.Msg
	f.gotAuth = req.Header().Get("Authorization")
	return connect.NewResponse(&compassv1.SetSecretResponse{}), nil
}

func (f *fakeSecrets) ListSecrets(_ context.Context, req *connect.Request[compassv1.ListSecretsRequest]) (*connect.Response[compassv1.ListSecretsResponse], error) {
	f.gotAuth = req.Header().Get("Authorization")
	list := f.list
	if list == nil {
		list = &compassv1.ListSecretsResponse{}
	}
	return connect.NewResponse(list), nil
}

func (f *fakeSecrets) DeleteSecret(_ context.Context, req *connect.Request[compassv1.DeleteSecretRequest]) (*connect.Response[compassv1.DeleteSecretResponse], error) {
	f.deleteCalls++
	f.gotDelete = req.Msg.GetName()
	f.gotAuth = req.Header().Get("Authorization")
	return connect.NewResponse(&compassv1.DeleteSecretResponse{}), nil
}

// startFakeSecretsServer stands up the fake SecretsService over a plain-HTTP
// httptest server and returns a client wired to it with the bearer interceptor.
func startFakeSecretsServer(t *testing.T, fake *fakeSecrets) compassv1connect.SecretsServiceClient {
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

// TestRunSecretSet asserts set reads the value from the injected stdin (never
// argv), sends the right name/value/delivery/kind, and stamps the bearer token.
func TestRunSecretSet(t *testing.T) {
	fake := &fakeSecrets{}
	client := startFakeSecretsServer(t, fake)

	var out strings.Builder
	in := strings.NewReader("s3cr3t\n")
	args := secretSetArgs{name: "OPENAI_KEY", delivery: "env", kind: "generic"}
	if err := runSecretSet(context.Background(), client, args, in, &out); err != nil {
		t.Fatalf("runSecretSet: %v", err)
	}
	if fake.gotSet == nil {
		t.Fatal("SetSecret was not called")
	}
	if fake.gotSet.GetName() != "OPENAI_KEY" {
		t.Errorf("name = %q, want OPENAI_KEY", fake.gotSet.GetName())
	}
	if fake.gotSet.GetValue() != "s3cr3t" {
		t.Errorf("value = %q, want s3cr3t (trailing newline trimmed, from stdin)", fake.gotSet.GetValue())
	}
	if fake.gotSet.GetDelivery() != compassv1.SecretDelivery_SECRET_DELIVERY_ENV {
		t.Errorf("delivery = %v, want ENV", fake.gotSet.GetDelivery())
	}
	if fake.gotSet.GetKind() != compassv1.SecretKind_SECRET_KIND_GENERIC {
		t.Errorf("kind = %v, want GENERIC", fake.gotSet.GetKind())
	}
	if fake.gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want Bearer test-token", fake.gotAuth)
	}
}

// TestRunSecretSetProviderKind asserts a provider secret carries its provider id
// and maps to the PROVIDER kind.
func TestRunSecretSetProviderKind(t *testing.T) {
	fake := &fakeSecrets{}
	client := startFakeSecretsServer(t, fake)

	var out strings.Builder
	args := secretSetArgs{name: "ANTHROPIC", delivery: "file", kind: "provider", provider: "anthropic"}
	if err := runSecretSet(context.Background(), client, args, strings.NewReader("v"), &out); err != nil {
		t.Fatalf("runSecretSet: %v", err)
	}
	if fake.gotSet.GetKind() != compassv1.SecretKind_SECRET_KIND_PROVIDER {
		t.Errorf("kind = %v, want PROVIDER", fake.gotSet.GetKind())
	}
	if fake.gotSet.GetProvider() != "anthropic" {
		t.Errorf("provider = %q, want anthropic", fake.gotSet.GetProvider())
	}
	if fake.gotSet.GetDelivery() != compassv1.SecretDelivery_SECRET_DELIVERY_FILE {
		t.Errorf("delivery = %v, want FILE", fake.gotSet.GetDelivery())
	}
}

// TestRunSecretSetGhKind asserts a gh secret carries its host and maps to the GH
// kind.
func TestRunSecretSetGhKind(t *testing.T) {
	fake := &fakeSecrets{}
	client := startFakeSecretsServer(t, fake)

	var out strings.Builder
	args := secretSetArgs{name: "GH", delivery: "env", kind: "gh", host: "github.com"}
	if err := runSecretSet(context.Background(), client, args, strings.NewReader("tok"), &out); err != nil {
		t.Fatalf("runSecretSet: %v", err)
	}
	if fake.gotSet.GetKind() != compassv1.SecretKind_SECRET_KIND_GH {
		t.Errorf("kind = %v, want GH", fake.gotSet.GetKind())
	}
	if fake.gotSet.GetHost() != "github.com" {
		t.Errorf("host = %q, want github.com", fake.gotSet.GetHost())
	}
}

// TestRunSecretSetRejections covers the client-side validation that fails before
// any RPC: a bad/missing delivery, a provider kind without --provider, a gh kind
// without --host, and an empty stdin value.
func TestRunSecretSetRejections(t *testing.T) {
	tests := []struct {
		name string
		args secretSetArgs
		in   string
		want string
	}{
		{
			name: "missing delivery",
			args: secretSetArgs{name: "X", delivery: "", kind: "generic"},
			in:   "v",
			want: "delivery",
		},
		{
			name: "unknown delivery",
			args: secretSetArgs{name: "X", delivery: "socket", kind: "generic"},
			in:   "v",
			want: "delivery",
		},
		{
			name: "provider kind without provider",
			args: secretSetArgs{name: "X", delivery: "env", kind: "provider"},
			in:   "v",
			want: "--provider",
		},
		{
			name: "gh kind without host",
			args: secretSetArgs{name: "X", delivery: "env", kind: "gh"},
			in:   "v",
			want: "--host",
		},
		{
			name: "unknown kind",
			args: secretSetArgs{name: "X", delivery: "env", kind: "totp"},
			in:   "v",
			want: "kind",
		},
		{
			name: "empty stdin value",
			args: secretSetArgs{name: "X", delivery: "env", kind: "generic"},
			in:   "\n",
			want: "value is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeSecrets{}
			client := startFakeSecretsServer(t, fake)
			var out strings.Builder
			err := runSecretSet(context.Background(), client, tt.args, strings.NewReader(tt.in), &out)
			if err == nil {
				t.Fatalf("runSecretSet(%+v) = nil error, want rejection", tt.args)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.want)
			}
			if fake.gotSet != nil {
				t.Error("SetSecret was called despite validation failure")
			}
		})
	}
}

// TestRunSecretList asserts list renders set/unset state and routing and NEVER a
// value, and that an empty list renders a clear message rather than an error.
func TestRunSecretList(t *testing.T) {
	fake := &fakeSecrets{list: &compassv1.ListSecretsResponse{Secrets: []*compassv1.SecretStatus{
		{
			Name:     "OPENAI_KEY",
			IsSet:    true,
			Delivery: compassv1.SecretDelivery_SECRET_DELIVERY_ENV,
			Kind:     compassv1.SecretKind_SECRET_KIND_PROVIDER,
			Provider: "openai",
		},
		{
			Name:     "GH_TOKEN",
			IsSet:    false,
			Delivery: compassv1.SecretDelivery_SECRET_DELIVERY_FILE,
			Kind:     compassv1.SecretKind_SECRET_KIND_GH,
			Host:     "github.com",
		},
	}}}
	client := startFakeSecretsServer(t, fake)

	var out strings.Builder
	if err := runSecretList(context.Background(), client, &out); err != nil {
		t.Fatalf("runSecretList: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"OPENAI_KEY", "set", "provider=openai",
		"GH_TOKEN", "unset", "host=github.com", "delivery=file",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("list output %q missing %q", got, want)
		}
	}
	if fake.gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want Bearer test-token", fake.gotAuth)
	}
}

// TestRunSecretListEmpty asserts an empty list renders a clear message, not an
// error.
func TestRunSecretListEmpty(t *testing.T) {
	fake := &fakeSecrets{}
	client := startFakeSecretsServer(t, fake)
	var out strings.Builder
	if err := runSecretList(context.Background(), client, &out); err != nil {
		t.Fatalf("runSecretList: %v", err)
	}
	if !strings.Contains(out.String(), "no secrets declared") {
		t.Errorf("empty-list output %q does not report an empty fleet", out.String())
	}
}

// TestRunSecretDelete asserts delete sends the name and confirms.
func TestRunSecretDelete(t *testing.T) {
	fake := &fakeSecrets{}
	client := startFakeSecretsServer(t, fake)
	var out strings.Builder
	if err := runSecretDelete(context.Background(), client, "OPENAI_KEY", &out); err != nil {
		t.Fatalf("runSecretDelete: %v", err)
	}
	if fake.deleteCalls != 1 {
		t.Errorf("DeleteSecret calls = %d, want 1", fake.deleteCalls)
	}
	if fake.gotDelete != "OPENAI_KEY" {
		t.Errorf("deleted name = %q, want OPENAI_KEY", fake.gotDelete)
	}
	if !strings.Contains(out.String(), "OPENAI_KEY") {
		t.Errorf("delete output %q does not confirm the name", out.String())
	}
}

// TestParseKindRoutingRejections asserts parseKind rejects routing flags that do
// not belong to the chosen kind, and still accepts each kind's valid routing.
func TestParseKindRoutingRejections(t *testing.T) {
	reject := []struct {
		name     string
		kind     string
		provider string
		host     string
		want     string
	}{
		{name: "generic with provider", kind: kindGeneric, provider: "foo", want: "--provider"},
		{name: "generic with host", kind: kindGeneric, host: "h", want: "--host"},
		{name: "provider with host", kind: kindProvider, provider: "foo", host: "h", want: "--host"},
		{name: "gh with provider", kind: kindGH, host: "h", provider: "foo", want: "--provider"},
		{name: "generic with both", kind: kindGeneric, provider: "foo", host: "h", want: "--provider"},
	}
	for _, tt := range reject {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseKind(tt.kind, tt.provider, tt.host)
			if err == nil {
				t.Fatalf("parseKind(%q, %q, %q) = nil error, want rejection", tt.kind, tt.provider, tt.host)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.want)
			}
			if got != compassv1.SecretKind_SECRET_KIND_UNSPECIFIED {
				t.Errorf("kind = %v, want UNSPECIFIED on error", got)
			}
		})
	}

	accept := []struct {
		name     string
		kind     string
		provider string
		host     string
		want     compassv1.SecretKind
	}{
		{name: "generic no routing", kind: kindGeneric, want: compassv1.SecretKind_SECRET_KIND_GENERIC},
		{name: "provider with provider", kind: kindProvider, provider: "anthropic", want: compassv1.SecretKind_SECRET_KIND_PROVIDER},
		{name: "gh with host", kind: kindGH, host: "github.com", want: compassv1.SecretKind_SECRET_KIND_GH},
	}
	for _, tt := range accept {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseKind(tt.kind, tt.provider, tt.host)
			if err != nil {
				t.Fatalf("parseKind(%q, %q, %q) = %v, want accept", tt.kind, tt.provider, tt.host, err)
			}
			if got != tt.want {
				t.Errorf("kind = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRunSecretSetBound asserts a stdin value over maxSecretBytes is rejected
// before any RPC, while a value exactly at the cap is accepted.
func TestRunSecretSetBound(t *testing.T) {
	t.Run("over the cap", func(t *testing.T) {
		fake := &fakeSecrets{}
		client := startFakeSecretsServer(t, fake)
		var out strings.Builder
		in := strings.NewReader(strings.Repeat("a", maxSecretBytes+1))
		err := runSecretSet(context.Background(), client, secretSetArgs{name: "X", delivery: "env", kind: "generic"}, in, &out)
		if err == nil {
			t.Fatal("runSecretSet with oversized stdin = nil error, want rejection")
		}
		if !strings.Contains(err.Error(), "limit") {
			t.Errorf("error %q does not mention the limit", err.Error())
		}
		if fake.gotSet != nil {
			t.Error("SetSecret was called despite oversized value")
		}
	})

	t.Run("exactly at the cap", func(t *testing.T) {
		fake := &fakeSecrets{}
		client := startFakeSecretsServer(t, fake)
		var out strings.Builder
		in := strings.NewReader(strings.Repeat("a", maxSecretBytes))
		if err := runSecretSet(context.Background(), client, secretSetArgs{name: "X", delivery: "env", kind: "generic"}, in, &out); err != nil {
			t.Fatalf("runSecretSet at cap: %v", err)
		}
		if fake.gotSet == nil {
			t.Fatal("SetSecret was not called for a value at the cap")
		}
		if len(fake.gotSet.GetValue()) != maxSecretBytes {
			t.Errorf("value length = %d, want %d", len(fake.gotSet.GetValue()), maxSecretBytes)
		}
	})

	t.Run("content at the cap with a trailing newline", func(t *testing.T) {
		fake := &fakeSecrets{}
		client := startFakeSecretsServer(t, fake)
		var out strings.Builder
		in := strings.NewReader(strings.Repeat("a", maxSecretBytes) + "\n")
		if err := runSecretSet(context.Background(), client, secretSetArgs{name: "X", delivery: "env", kind: "generic"}, in, &out); err != nil {
			t.Fatalf("runSecretSet at cap with trailing newline: %v", err)
		}
		if fake.gotSet == nil {
			t.Fatal("SetSecret was not called for cap-sized content with a trailing newline")
		}
		if len(fake.gotSet.GetValue()) != maxSecretBytes {
			t.Errorf("value length = %d, want %d (newline trimmed, content at cap accepted)", len(fake.gotSet.GetValue()), maxSecretBytes)
		}
	})
}
