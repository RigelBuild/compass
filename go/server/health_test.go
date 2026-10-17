//go:build unix

package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/grpchealth/v2"
	"connectrpc.com/otelconnect"

	"github.com/RigelBuild/compass/go/events"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/runner"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/runnertest"
	"github.com/RigelBuild/compass/go/internal/store"
)

func TestServerHealthCheck(t *testing.T) {
	health := newServerHealth(nil)
	mux := http.NewServeMux()
	health.mount(mux)
	server := httptest.NewUnstartedServer(mux)
	server.Config.Protocols = cleartextHTTP2()
	server.Start()
	t.Cleanup(server.Close)

	transport := h2cTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, addr)
	})
	t.Cleanup(transport.CloseIdleConnections)
	client := grpchealth.NewClient(connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: transport}, server.URL, connecthttp.WithReadMaxBytes(0), connecthttp.WithGRPC())))

	for _, service := range []string{"", "compass.v1.CompassService"} {
		response, err := client.Check(t.Context(), &grpchealth.CheckRequest{Service: service})
		if err != nil {
			t.Fatalf("Check(%q): %v", service, err)
		}
		if response.Status != grpchealth.StatusServing {
			t.Errorf("Check(%q) status = %s, want serving", service, response.Status)
		}
	}
	if _, err := client.Check(t.Context(), &grpchealth.CheckRequest{Service: strings.Repeat("x", healthMaxReadBytes)}); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("oversized Check error = %v, want ResourceExhausted", err)
	}
	if _, err := client.Check(t.Context(), &grpchealth.CheckRequest{Service: "nope"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("Check(nope) code = %v, want NotFound (error %v)", connect.CodeOf(err), err)
	}

	health.markDraining()
	for _, service := range []string{"", "compass.v1.CompassService"} {
		response, err := client.Check(t.Context(), &grpchealth.CheckRequest{Service: service})
		if err != nil {
			t.Fatalf("Check(%q) after drain: %v", service, err)
		}
		if response.Status != grpchealth.StatusNotServing {
			t.Errorf("Check(%q) after drain status = %s, want not_serving", service, response.Status)
		}
	}
}

func TestServerHealthRunnerService(t *testing.T) {
	nilHealth, err := newServerHealth(nil).Check(t.Context(), &grpchealth.CheckRequest{Service: "compass.runner"})
	if err != nil {
		t.Fatalf("Check(compass.runner) with nil hub: %v", err)
	}
	if nilHealth.Status != grpchealth.StatusNotServing {
		t.Fatalf("Check(compass.runner) with nil hub = %s, want not_serving", nilHealth.Status)
	}
	health, hub, srv, attached := newServerHealthRunnerServer(t)
	assertStatus := func(wantServing bool, wantHTTP int) {
		t.Helper()
		response, err := health.Check(t.Context(), &grpchealth.CheckRequest{Service: "compass.runner"})
		if err != nil {
			t.Fatalf("Check(compass.runner): %v", err)
		}
		wantStatus, wantBody := grpchealth.StatusNotServing, "not_serving"
		if wantServing {
			wantStatus, wantBody = grpchealth.StatusServing, "serving"
		}
		if response.Status != wantStatus {
			t.Errorf("Check(compass.runner) status = %s, want %s", response.Status, wantStatus)
		}
		assertHealthzResponse(t, srv.Client(), srv.URL+"/healthz?service=compass.runner", http.MethodGet, wantHTTP, wantBody)
	}
	ctx, cancel := context.WithCancel(t.Context())
	link, err := runner.Dial(ctx, runner.RunnerConfig{RunnerID: "runner-1", ServerAddr: srv.URL,
		Token: runner.StaticToken("runner-tok"), HTTPClient: runnertest.H2CClient(t)})
	if err != nil {
		t.Fatalf("runner.Dial: %v", err)
	}
	assertStatus(false, http.StatusServiceUnavailable)
	done := make(chan error, 1)
	go func() { done <- link.RunSessions(ctx, idleHost{}, slog.New(slog.DiscardHandler)) }()
	select {
	case <-attached:
	case <-timeAfter():
		t.Fatal("Sessions stream never attached")
	}
	assertStatus(true, http.StatusOK)
	cancel()
	select {
	case <-done:
	case <-timeAfter():
		t.Fatal("RunSessions did not exit after cancel")
	}
	deadline := timeAfter()
	for hub.RunnerEnrolled() {
		select {
		case <-deadline:
			t.Fatal("hub still enrolled after the Sessions stream closed")
		default:
			runtime.Gosched()
		}
	}
	assertStatus(false, http.StatusServiceUnavailable)
	ctx, cancel = context.WithCancel(t.Context())
	link, err = runner.Dial(ctx, runner.RunnerConfig{RunnerID: "runner-1", ServerAddr: srv.URL,
		Token: runner.StaticToken("runner-tok"), HTTPClient: runnertest.H2CClient(t)})
	if err != nil {
		t.Fatalf("runner.Dial after detach: %v", err)
	}
	done = make(chan error, 1)
	go func() { done <- link.RunSessions(ctx, idleHost{}, slog.New(slog.DiscardHandler)) }()
	select {
	case <-attached:
	case <-timeAfter():
		t.Fatal("Sessions stream never reattached")
	}
	if !hub.RunnerEnrolled() {
		t.Fatal("Runner is not enrolled after the Sessions stream attached")
	}
	health.markDraining()
	assertStatus(false, http.StatusServiceUnavailable)
	cancel()
	select {
	case <-done:
	case <-timeAfter():
		t.Fatal("RunSessions did not exit after cancel while draining")
	}
}

func newServerHealthRunnerServer(t *testing.T) (*serverHealth, *runnerhub.Hub, *httptest.Server, <-chan struct{}) {
	t.Helper()
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	hub := runnerhub.NewHub(board.NewProjection(bus), nil, nil, slog.New(slog.DiscardHandler))
	health := newServerHealth(hub)
	attached := make(chan struct{}, 2)
	hub.SetRunnerReadyHook(func() { attached <- struct{}{} })
	interceptor, err := otelconnect.NewInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewInterceptor: %v", err)
	}
	path, handler := runnerhub.NewMountedHandler(hub,
		func(_ context.Context, presented string, want store.SubjectKind) (store.Subject, error) {
			if presented != "runner-tok" || want != store.SubjectRunner {
				return store.Subject{}, store.ErrNotFound
			}
			return store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, nil
		}, nil, nil, interceptor)
	mux := http.NewServeMux()
	health.mount(mux)
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	return health, hub, srv, attached
}

func TestHealthzStatusMapping(t *testing.T) {
	health := newServerHealth(nil)
	mux := http.NewServeMux()
	health.mount(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	for _, tc := range []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantBody   string
	}{
		{name: "get", method: http.MethodGet, path: "/healthz", wantStatus: http.StatusOK, wantBody: "serving"},
		{name: "head", method: http.MethodHead, path: "/healthz", wantStatus: http.StatusOK},
		{name: "unknown service", method: http.MethodGet, path: "/healthz?service=nope", wantStatus: http.StatusNotFound, wantBody: "Not Found\n"},
		{name: "post", method: http.MethodPost, path: "/healthz", wantStatus: http.StatusMethodNotAllowed, wantBody: "Method Not Allowed\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertHealthzResponse(t, server.Client(), server.URL+tc.path, tc.method, tc.wantStatus, tc.wantBody)
		})
	}

	health.markDraining()
	for _, tc := range []struct {
		name     string
		method   string
		wantBody string
	}{
		{name: "get", method: http.MethodGet, wantBody: "not_serving"},
		{name: "head", method: http.MethodHead},
	} {
		t.Run("draining "+tc.name, func(t *testing.T) {
			assertHealthzResponse(t, server.Client(), server.URL+"/healthz", tc.method, http.StatusServiceUnavailable, tc.wantBody)
		})
	}
}

func assertHealthzResponse(t *testing.T, client *http.Client, url, method string, wantStatus int, wantBody string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatalf("new %s request: %v", method, err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		t.Fatalf("read %s response: %v", method, readErr)
	}
	if closeErr != nil {
		t.Fatalf("close %s response: %v", method, closeErr)
	}
	if response.StatusCode != wantStatus {
		t.Errorf("%s %s status = %d, want %d", method, url, response.StatusCode, wantStatus)
	}
	if string(body) != wantBody {
		t.Errorf("%s %s body = %q, want %q", method, url, body, wantBody)
	}
}
