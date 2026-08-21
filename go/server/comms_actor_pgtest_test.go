//go:build pgtest && unix

package server

// Comms-actor attribution over the authenticated network door (SEA-1195 T3b).
// The isolated auth-package test (internal/auth/interceptor_pgtest_test.go's
// TestBearerInterceptorSetsCommsActorNotAdminFallback) proves withCaller sets the
// comms actor when driven through a hand-built BearerInterceptor; it cannot prove
// buildNetworkServer actually MOUNTS that interceptor on the CommsService chain.
// This test closes that gap end-to-end: it drives a real network door through the
// production serving path (Serve → buildNetworkServer, --listen + TLS) and calls
// CommsService.CreateChannelGroup over TLS as a non-admin bearer caller, asserting
// the created group's owner is the caller — not the bootstrap-admin fallback comms
// attributes when no actor is set. It reddens if buildNetworkServer dropped
// withCaller, or mounted the comms handler on a chain that lost the caller, since
// attribution would then fall back to the admin the comms service was built with.
//
// Store-gated (Serve opens the store, the bearer interceptor resolves the token
// against it) so it lives in the `pgtest` lane. It reuses the TLS + door harness
// from network_door_test.go (writeSelfSignedCert, freeLoopbackAddr,
// serveInBackground, waitServing) and adds only its own TLS CommsService client.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

// newTLSCommsClient builds a CommsService connect client that speaks
// HTTP/2-over-TLS to addr, trusting only pool — the CommsService counterpart to
// network_door_test.go's newTLSClient (which serves CompassService). The network
// door mounts both services on one door, so this reaches the comms RPCs over the
// same TLS transport a real browser/native client uses. Idle conns close via
// t.Cleanup.
func newTLSCommsClient(t *testing.T, addr string, pool *x509.CertPool) compassv1connect.CommsServiceClient {
	t.Helper()
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetHTTP2(true)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"},
		Protocols:       p,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return compassv1connect.NewCommsServiceClient(&http.Client{Transport: tr}, "https://"+addr)
}

// TestNetworkDoorCommsActorIsBearerCallerNotAdmin pins invariant #2 (the
// comms-actor half of withCaller) through buildNetworkServer's real interceptor
// chain: a non-admin account's bearer token, presented over the TLS network door
// on CommsService.CreateChannelGroup, must own the resulting group as the CALLER,
// not the bootstrap admin. comms exposes no context reader, so the store-recorded
// owner is the only observable proof the comms actor was threaded from the bearer
// interceptor into the comms handler on the door's mounted chain. If
// buildNetworkServer dropped withCaller (or mounted comms without the bearer
// interceptor), attribution would fall back to the admin and this reddens.
func TestNetworkDoorCommsActorIsBearerCallerNotAdmin(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "compass.sock")
	stateDir := filepath.Join(dir, "state")
	certPath, keyPath, pool := writeSelfSignedCert(t, dir)
	addr := freeLoopbackAddr(t)

	// Open a store against the SAME per-test schema Serve will use, to seed the
	// non-admin member, mint its bearer token, and learn the bootstrap-admin id
	// (the fallback actor the assertion must reject). Migration is idempotent, so
	// Serve re-opening this DSN is a no-op; BootstrapAdmin is idempotent by handle,
	// so it returns the same admin id Serve then fetches. Done synchronously before
	// Serve starts, so there is no concurrent-Open race on the admin insert.
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: bootstrapAdminHandle, DisplayName: bootstrapAdminDisplayName})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	member, err := st.CreateUser(ctx, store.NewUser{Handle: "member", DisplayName: "member"})
	if err != nil {
		t.Fatalf("CreateUser(member): %v", err)
	}
	memberTok, err := auth.IssueAccountToken(ctx, st, member.ID)
	if err != nil {
		t.Fatalf("IssueAccountToken(member): %v", err)
	}

	serveInBackground(t, ServeConfig{
		SocketPath:  socketPath,
		DatabaseDSN: dsn,
		Version:     "comms-actor-test",
		Listen:      addr,
		TLS:         &TLSConfig{CertPath: certPath, KeyPath: keyPath},
		StateDir:    stateDir,
	})
	// The network listener binds before the socket (Serve's ordering), so once the
	// socket serves an RPC the TLS door is accepting.
	waitServing(t, socketPath)

	// Create a channel group as the non-admin member, over the TLS network door.
	// CreateChannelGroup is authenticatedOpen, so the member's valid bearer clears
	// the bearer resolve and the admin gate and reaches the comms handler.
	client := newTLSCommsClient(t, addr, pool)
	req := connect.NewRequest(&compassv1.CreateChannelGroupRequest{Name: "caller-space"})
	req.Header().Set("Authorization", "Bearer "+memberTok)
	rctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	resp, err := client.CreateChannelGroup(rctx, req)
	if err != nil {
		t.Fatalf("member bearer CreateChannelGroup over the network door: %v", err)
	}

	owner := resp.Msg.GetGroup().GetOwnerUserId()
	if owner != string(member.ID) {
		t.Fatalf("channel group owner = %q, want the member caller %q (buildNetworkServer must thread withCaller's comms actor through the mounted interceptor chain, not fall back to the admin %q)", owner, member.ID, admin.ID)
	}
}
