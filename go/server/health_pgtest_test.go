//go:build pgtest && unix

package server

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/grpchealth/v2"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1connect "github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/pgtest"
)

type healthDoorClient struct {
	name   string
	url    string
	client *http.Client
}

func TestServeHealthOnEveryDoor(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, roots := writeSelfSignedCert(t, dir)
	socketPath := filepath.Join(dir, "compass.sock")
	devAddrPort := netip.MustParseAddrPort(loopbackAny)
	addrs := serveInBackground(t, ServeConfig{
		SocketPath:  socketPath,
		DatabaseDSN: pgtest.RequireDSN(t),
		Version:     "health-door-test",
		DevHTTP:     &devAddrPort,
		Listen:      loopbackAny,
		TLS:         &TLSConfig{CertPath: certPath, KeyPath: keyPath},
		StateDir:    filepath.Join(dir, "state"),
	})
	waitServing(t, socketPath)

	socketTransport := h2cTransport(func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", socketPath)
	})
	devTransport := h2cTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, addr)
	})
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	networkTransport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "127.0.0.1"},
		Protocols:       protocols,
	}
	for _, transport := range []*http.Transport{socketTransport, devTransport, networkTransport} {
		t.Cleanup(transport.CloseIdleConnections)
	}
	doors := []healthDoorClient{
		{name: "socket", url: "http://unix", client: &http.Client{Transport: socketTransport}},
		{name: "dev", url: "http://" + addrs.dev, client: &http.Client{Transport: devTransport}},
		{name: "network", url: "https://" + addrs.network, client: &http.Client{Transport: networkTransport}},
	}
	for _, door := range doors {
		t.Run(door.name, func(t *testing.T) {
			checkHealthDoor(t, door)
		})
	}
}

func checkHealthDoor(t *testing.T, door healthDoorClient) {
	t.Helper()
	healthClient := grpchealth.NewClient(connect.NewClient(connecthttp.NewTransport(door.client, door.url, connecthttp.WithReadMaxBytes(0), connecthttp.WithGRPC())))
	response, err := healthClient.Check(t.Context(), &grpchealth.CheckRequest{})
	if err != nil {
		t.Fatalf("token-less Health Check: %v", err)
	}
	if response.Status != grpchealth.StatusServing {
		t.Fatalf("token-less Health Check status = %s, want serving", response.Status)
	}

	if door.name == "network" {
		client := compassv1connect.NewCompassServiceClient(connect.NewClient(connecthttp.NewTransport(door.client, door.url, connecthttp.WithReadMaxBytes(0), connecthttp.WithGRPC())))
		_, err := client.GetServerInfo(t.Context(), &compassv1.GetServerInfoRequest{})
		if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
			t.Fatalf("token-less GetServerInfo error = %v, want Unauthenticated", err)
		}
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, door.url+"/healthz", nil)
	if err != nil {
		t.Fatalf("healthz request: %v", err)
	}
	responseHTTP, err := door.client.Do(request)
	if err != nil {
		t.Fatalf("token-less GET /healthz: %v", err)
	}
	body, readErr := io.ReadAll(responseHTTP.Body)
	closeErr := responseHTTP.Body.Close()
	if readErr != nil {
		t.Fatalf("read healthz response: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close healthz response: %v", closeErr)
	}
	if responseHTTP.StatusCode != http.StatusOK || string(body) != "serving" {
		t.Fatalf("token-less GET /healthz = %d %q, want 200 serving", responseHTTP.StatusCode, body)
	}

	watchEvents := healthClient.Watch(t.Context(), &grpchealth.CheckRequest{})
	select {
	case event := <-watchEvents:
		if event.Err == nil || connect.CodeOf(event.Err) != connect.CodeUnimplemented {
			t.Fatalf("token-less Health Watch error = %v, want Unimplemented", event.Err)
		}
	case <-timeAfter():
		t.Fatal("token-less Health Watch did not return Unimplemented")
	}
}
