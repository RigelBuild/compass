//go:build linux

package guestd

// Hermetic suite for the serve step's h2c wiring (serveHandshake) and the Health
// handler. It serves the GuestControl handler over an in-memory net.Listener
// (no AF_VSOCK, no VM) with the same cleartextHTTP2 stack production uses, dials
// it with a Connect h2c client, and asserts Health returns the boot state. This
// proves the handler implements the generated interface AND that the h2c door
// actually carries a Connect call — the pieces T4 exercises over real vsock.
//
// Shutdown is event-gated on the returned error channel + t.Context(); no
// sleeps.

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

func TestServeHandshakeHealthOverH2C(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	svc := &healthService{version: "v-test", netProvisioned: true, workspaceMounted: true}

	ctx, cancel := context.WithCancel(t.Context())
	serveErr := make(chan error, 1)
	go func() { serveErr <- serveHandshake(ctx, ln, svc) }()

	// h2c client: dial cleartext and speak prior-knowledge HTTP/2, matching the
	// server's cleartextHTTP2 door. This is the test-side mirror of the host's
	// h2c Connect client; it uses x/net/http2 only in the test, never in the
	// binary (the house pattern is the stdlib http.Protocols server path).
	h2cClient := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		},
	}

	client := compassv1internalconnect.NewGuestControlClient(h2cClient, "http://"+ln.Addr().String())
	resp, err := client.Health(t.Context(), connect.NewRequest(&compassv1internal.HealthRequest{}))
	if err != nil {
		t.Fatalf("Health call over h2c: %v", err)
	}

	msg := resp.Msg
	if msg.GetGuestdVersion() != "v-test" {
		t.Fatalf("GuestdVersion = %q, want v-test", msg.GetGuestdVersion())
	}
	if !msg.GetNetProvisioned() || !msg.GetWorkspaceMounted() {
		t.Fatalf("Health = {net:%v mount:%v}, want both true", msg.GetNetProvisioned(), msg.GetWorkspaceMounted())
	}

	cancel()
	if err := <-serveErr; err != nil {
		t.Fatalf("serveHandshake returned %v, want nil after clean shutdown", err)
	}
}

func TestServeHandshakeReportsServeFault(t *testing.T) {
	// A listener closed out from under the server makes Serve return a non-
	// ErrServerClosed error before ctx is cancelled — the fail-closed serve
	// fault: the handshake is no longer answered and run must surface it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	svc := &healthService{version: "v", netProvisioned: true, workspaceMounted: true}

	serveErr := make(chan error, 1)
	go func() { serveErr <- serveHandshake(t.Context(), ln, svc) }()

	// Close the listener to force a serve fault. Serve returns the accept error,
	// which serveHandshake reports (not ErrServerClosed).
	if err := ln.Close(); err != nil {
		t.Fatalf("closing listener: %v", err)
	}

	err = <-serveErr
	if err == nil {
		t.Fatal("serveHandshake returned nil after a serve fault, want fail-closed error")
	}
	if errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("serveHandshake reported ErrServerClosed as a fault: %v", err)
	}
}
