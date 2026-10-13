//go:build unix

package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
)

func TestServerHealthCheck(t *testing.T) {
	health := newServerHealth()
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
	client := grpchealth.NewClient(&http.Client{Transport: transport}, server.URL, connect.WithGRPC())

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

func TestHealthzStatusMapping(t *testing.T) {
	health := newServerHealth()
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
