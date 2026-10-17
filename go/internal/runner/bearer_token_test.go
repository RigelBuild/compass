//go:build unix

package runner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

func TestBearerTokenFromAddsAuthorizationHeader(t *testing.T) {
	server := connect.NewServer()
	recorder := &recordingEnroll{}
	compassv1internalconnect.RegisterRunnerServiceHandler(server, recorder)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server, connecthttp.WithReadMaxBytes(0))
	httpServer := httptest.NewUnstartedServer(mux)
	httpServer.Config.Protocols = cleartextHTTP2()
	httpServer.Start()
	t.Cleanup(httpServer.Close)

	transport := connecthttp.NewTransport(h2cHTTPClient(t), httpServer.URL, connecthttp.WithReadMaxBytes(0))
	client := compassv1internalconnect.NewRunnerServiceClient(connect.NewClient(transport, bearerTokenFrom(StaticToken("test-token"))))
	if _, err := client.Enroll(t.Context(), &compassv1internal.EnrollRequest{}); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if got, want := recorder.authRequests(), []string{"Bearer test-token"}; !slices.Equal(got, want) {
		t.Fatalf("authorization headers = %v, want %v", got, want)
	}
}

func TestBearerTokenFromReturnsTokenSourceErrorBeforeOpeningCall(t *testing.T) {
	want := errors.New("token source failed")
	var opened atomic.Bool
	interceptor := bearerTokenFrom(testTokenSource{err: want})
	final := func(context.Context, connect.Spec) (connect.ClientStream, error) {
		opened.Store(true)
		return nil, errors.New("final must not run")
	}
	_, err := interceptor(final)(t.Context(), connect.Spec{})
	if !errors.Is(err, want) {
		t.Fatalf("interceptor error = %v, want %v", err, want)
	}
	if opened.Load() {
		t.Fatal("interceptor opened RPC after token source failed")
	}
}

type testTokenSource struct {
	err error
}

func (s testTokenSource) Token(context.Context) (string, error) {
	return "", s.err
}
