//go:build linux

package guestd

// Hermetic suite for the serve step's h2c wiring (serveHandshake) and the Health
// handler. It serves GuestControl over an in-memory net.Listener (no AF_VSOCK,
// no VM) with the production cleartextHTTP2 stack, dials it with a Connect h2c
// client, and asserts Health returns the boot state. Shutdown event-gated, no sleeps.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

func TestServeHandshakeHealthOverH2C(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	svc := &supervisor{version: "v-test", netProvisioned: true, workspaceMounted: true, state: stateReady, execs: map[string]*childExec{}}

	ctx, cancel := context.WithCancel(t.Context())
	serveErr := make(chan error, 1)
	go func() { serveErr <- serveHandshake(ctx, ln, svc) }()

	// h2c client: prior-knowledge HTTP/2 over cleartext, matching the server's
	// cleartextHTTP2 door.
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	h2cClient := &http.Client{Transport: &http.Transport{Protocols: protocols}}

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
	svc := &supervisor{version: "v", netProvisioned: true, workspaceMounted: true, state: stateReady, execs: map[string]*childExec{}}

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
