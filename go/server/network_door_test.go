//go:build pgtest && unix

package server

// Integration tests for the authenticated TLS network door (RIG-1195 T3b): TLS
// termination against an in-test self-signed pair, the bearer+admin-gate chain
// over a real connect client, the IssueToken handler's input contract, the
// oracle-safety of the bearer rejection paths, and the bootstrap-admin token's
// --listen gating and 0600 persistence. White-box (package server) so tests
// construct the unexported service, drive Serve, and reference the network-door
// helpers (adminTokenFile) directly.
//
// Store-gated: the network door authenticates every RPC against the Postgres
// store of record (IssueAccountToken persists a hash there; the bearer
// interceptor resolves against it) and Serve opens the store at startup, so
// every test here needs a real database. Behind `//go:build pgtest && unix` via
// the shared pgtest harness (pgtest.RequireDSN → an isolated-schema DSN, or
// t.Skip when no runtime), with DatabaseDSN set on every ServeConfig.
//
// Hermetic: every cert, key, socket, and state dir lives under t.TempDir(); the
// network door binds an OS-assigned loopback port (a throwaway net.Listen reads
// a free port, then releases it). No wall-clock sleep is used for sync — Serve
// readiness is event-gated on the door actually serving an RPC; testTimeout is a
// deadline safety net that turns a wedged handler into a fast failure, never a
// synchronization device.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

// unknownAccountID is a non-empty account id deliberately distinct from any
// bootstrap admin's or member's id — the "account does not exist" reference for
// the NotFound paths. store.AccountID is a bare string now, so any well-formed
// but never-created id is unknown; a fixed literal keeps the assertion
// deterministic (no dependence on a generated value).
const unknownAccountID = "no-such-account"

// freeLoopbackAddr returns a currently-free 127.0.0.1 address by binding an
// ephemeral port and immediately releasing it, so Serve can rebind it without a
// fixed-port collision. The brief sanctions the tiny bind/close/rebind race
// (there is no way to hand Serve an already-bound listener); nothing else in the
// test races on this port.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a free loopback port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return addr
}

// writeSelfSignedCert generates an ECDSA P-256 self-signed cert/key valid for
// 127.0.0.1 into dir (PEM), returning the two paths and a cert pool that trusts
// it — the operator-provisioned keypair a TLS client and Serve share. The
// validity window (±1h around now) is a certificate field, not a timing sync
// device; the ±1h margin makes it robust to clock skew across the run. Key
// generation is the one place randomness is required (any valid keypair works),
// so it does not make an assertion nondeterministic.
func writeSelfSignedCert(t *testing.T, dir string) (certPath, keyPath string, pool *x509.CertPool) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating ECDSA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "compass-network-door-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("creating self-signed cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing generated cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshaling EC private key: %v", err)
	}

	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("writing cert PEM: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("writing key PEM: %v", err)
	}

	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certPath, keyPath, pool
}

// newTLSClient builds a connect client that speaks HTTP/2-over-TLS to addr,
// trusting only pool — the shape a real network-door client uses (ALPN h2
// negotiated from the transport's Protocols). Idle conns are closed via
// t.Cleanup.
func newTLSClient(t *testing.T, addr string, pool *x509.CertPool) compassv1connect.CompassServiceClient {
	t.Helper()
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetHTTP2(true)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"},
		Protocols:       p,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return compassv1connect.NewCompassServiceClient(&http.Client{Transport: tr}, "https://"+addr)
}

// serveInBackground starts Serve on cfg in a goroutine and registers a cleanup
// that cancels it and asserts the clean-shutdown contract (Serve returns nil on
// ctx cancel). The returned nothing: readiness is gated separately via
// waitServing.
func serveInBackground(t *testing.T, cfg ServeConfig) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, cfg) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("Serve on clean ctx-cancel shutdown = %v, want nil", err)
			}
		case <-timeAfter():
			t.Error("Serve did not return after ctx cancel")
		}
	})
}

// waitServing event-gates on the server actually serving RPCs: it waits for the
// socket to bind (waitListening), then round-trips GetServerInfo over the UDS.
// A successful RPC proves Serve has reached the serving stage — which, in Serve's
// ordering, is strictly after the admin-token write step — so any token-file
// assertion after this call observes the final state, not a mid-startup one.
func waitServing(t *testing.T, socketPath string) {
	t.Helper()
	waitListening(t, socketPath)
	client := newUDSClient(t, socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if _, err := client.GetServerInfo(ctx, connect.NewRequest(&compassv1.GetServerInfoRequest{})); err != nil {
		t.Fatalf("GetServerInfo over UDS during readiness gate: %v", err)
	}
}

// readAdminToken reads the bootstrap-admin bearer token from the admin-token
// file under stateDir. Call it only after waitServing has confirmed the door is
// serving — Serve writes the token before it serves, so the file is present.
func readAdminToken(t *testing.T, stateDir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(stateDir, adminTokenFile))
	if err != nil {
		t.Fatalf("read admin-token file: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("admin-token file is empty")
	}
	return string(raw)
}

// ---- Group 1: TLS handshake against a self-signed pair ----

// TestServeNetworkDoorTLSHandshakeAndRPC: with --listen and a valid keypair, a
// client that trusts the cert completes the TLS handshake with ALPN h2 and
// round-trips GetServerInfo over TLS. This defends the whole network-door
// termination path: a broken TLS load, a missing ALPN h2 advertisement, or a
// door that never serves the RPC each reddens a distinct assertion below.
func TestServeNetworkDoorTLSHandshakeAndRPC(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "compass.sock")
	stateDir := filepath.Join(dir, "state")
	certPath, keyPath, pool := writeSelfSignedCert(t, dir)
	addr := freeLoopbackAddr(t)

	serveInBackground(t, ServeConfig{
		SocketPath:  socketPath,
		DatabaseDSN: pgtest.RequireDSN(t),
		Version:     "tls-test",
		Listen:      addr,
		TLS:         &TLSConfig{CertPath: certPath, KeyPath: keyPath},
		StateDir:    stateDir,
	})
	// The network listener is bound before the socket (Serve's ordering), so once
	// the socket serves an RPC the TCP port is bound and the network server is
	// accepting — a TLS dial now connects and handshakes rather than hitting a
	// closed port.
	waitServing(t, socketPath)

	// Handshake + ALPN: a raw TLS dial trusting the cert must negotiate h2. This
	// is the direct proof the door terminates TLS and advertises HTTP/2 via ALPN
	// (networkProtocols). A deadline dialer bounds a wedged handshake.
	rawConn, err := tls.DialWithDialer(
		&net.Dialer{Deadline: time.Now().Add(testTimeout)},
		"tcp", addr,
		&tls.Config{RootCAs: pool, ServerName: "127.0.0.1", NextProtos: []string{"h2"}},
	)
	if err != nil {
		t.Fatalf("TLS handshake against the network door: %v", err)
	}
	defer rawConn.Close()
	if got := rawConn.ConnectionState().NegotiatedProtocol; got != "h2" {
		t.Fatalf("negotiated ALPN = %q, want h2 (the door must be HTTP/2-native over TLS)", got)
	}

	// RPC over TLS: the network door authenticates every RPC, so an open RPC
	// (GetServerInfo, ungated) still needs a valid bearer — present the minted
	// admin token from StateDir. Success proves the terminated connection carries
	// a real authenticated RPC and returns the configured version.
	token := readAdminToken(t, stateDir)
	client := newTLSClient(t, addr, pool)
	req := connect.NewRequest(&compassv1.GetServerInfoRequest{})
	req.Header().Set("Authorization", "Bearer "+token)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	resp, err := client.GetServerInfo(ctx, req)
	if err != nil {
		t.Fatalf("GetServerInfo over TLS: %v", err)
	}
	if got := resp.Msg.GetVersion(); got != "tls-test" {
		t.Fatalf("Version over TLS = %q, want tls-test", got)
	}
}

// TestServeNetworkDoorBadCertFailsFastLeavingNoSocket: with --listen set but the
// keypair unreadable, Serve must fail up front — before any on-disk state — so no
// socket is left behind. loadNetworkTLS runs before the socket bind, so a bad
// cert (missing file, or garbage bytes) fails Serve synchronously with nothing
// created. Table-driven over the two unreadable shapes.
func TestServeNetworkDoorBadCertFailsFastLeavingNoSocket(t *testing.T) {
	garbageDir := t.TempDir()
	garbageCert := filepath.Join(garbageDir, "garbage-cert.pem")
	garbageKey := filepath.Join(garbageDir, "garbage-key.pem")
	if err := os.WriteFile(garbageCert, []byte("not a pem certificate"), 0o600); err != nil {
		t.Fatalf("writing garbage cert: %v", err)
	}
	if err := os.WriteFile(garbageKey, []byte("not a pem key"), 0o600); err != nil {
		t.Fatalf("writing garbage key: %v", err)
	}

	cases := []struct {
		name      string
		cert, key string
	}{
		{"missing cert file", filepath.Join(garbageDir, "nonexistent.pem"), filepath.Join(garbageDir, "nonexistent.key")},
		{"garbage cert file", garbageCert, garbageKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			socketPath := filepath.Join(t.TempDir(), "compass.sock")
			err := Serve(context.Background(), ServeConfig{
				SocketPath:  socketPath,
				DatabaseDSN: pgtest.RequireDSN(t),
				Version:     "tls-test",
				Listen:      "127.0.0.1:0",
				TLS:         &TLSConfig{CertPath: tc.cert, KeyPath: tc.key},
			})
			if err == nil {
				t.Fatal("Serve with an unreadable keypair = nil, want an up-front error")
			}
			if _, statErr := os.Stat(socketPath); !os.IsNotExist(statErr) {
				t.Fatalf("socket created despite a failed TLS load (stat err = %v)", statErr)
			}
		})
	}
}

// TestServeNetworkDoorListenWithoutTLSRejected: --listen with no TLS config is
// credential disclosure (a bearer token over cleartext), so loadNetworkTLS
// rejects it up front with a "requires TLS" error and no socket is created.
func TestServeNetworkDoorListenWithoutTLSRejected(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "compass.sock")
	err := Serve(context.Background(), ServeConfig{
		SocketPath:  socketPath,
		DatabaseDSN: pgtest.RequireDSN(t),
		Version:     "tls-test",
		Listen:      "127.0.0.1:0",
		// TLS intentionally nil.
	})
	if err == nil {
		t.Fatal("Serve with --listen and no TLS = nil, want a TLS-required rejection")
	}
	if msg := err.Error(); !strings.Contains(msg, "requires TLS") {
		t.Fatalf("Serve error = %q, want the TLS-required guard message", msg)
	}
	if _, statErr := os.Stat(socketPath); !os.IsNotExist(statErr) {
		t.Fatalf("socket created despite the TLS-required rejection (stat err = %v)", statErr)
	}
}

// ---- Group 2: bearer accept/reject over the network-door chain ----

// networkDoorHandler stands up the compass.v1 handler with the production
// network-door interceptor chain (bearer outer, admin gate inner) over a real
// connect client. Unlike the auth-package unit tests, this drives real generated
// procedure paths, so the AdminGate reads a populated Spec().Procedure — the
// integration seam a hand-built unary request cannot exercise.
func networkDoorHandler(t *testing.T, svc *service, st *store.Store, adminID store.AccountID) compassv1connect.CompassServiceClient {
	t.Helper()
	url := newH2CTestServerWithInterceptors(t, svc,
		auth.BearerInterceptor(st),
		auth.BearerStreamInterceptor(st),
		auth.NewAdminGate(adminID),
	)
	return newH2CClient(t, url)
}

// newNetworkStore opens a store against an isolated pgtest schema (skips when no
// runtime) and seeds the bootstrap admin plus a non-admin member — the two
// identities the door's auth matrix mints tokens for. Returns the store and both
// ids.
func newNetworkStore(t *testing.T) (*store.Store, store.AccountID, store.AccountID) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, pgtest.RequireDSN(t))
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	member, err := st.CreateUser(ctx, store.NewUser{Handle: "member", DisplayName: "member"})
	if err != nil {
		t.Fatalf("CreateUser(member): %v", err)
	}
	return st, admin.ID, member.ID
}

// TestNetworkDoorBearerAuthAcceptAndReject is the door's authentication +
// authorization matrix over the real interceptor chain. IssueToken is the
// adminOnly probe (its target account id is irrelevant to the reject rows, which
// short-circuit before the handler); GetServerInfo is the open probe. Each row
// pins one credential class to its outcome, so a broken bearer resolve, an
// inverted gate, or an open RPC wrongly gated reddens exactly that row.
func TestNetworkDoorBearerAuthAcceptAndReject(t *testing.T) {
	ctx := context.Background()
	st, admin, member := newNetworkStore(t)
	adminTok, err := auth.IssueAccountToken(ctx, st, admin)
	if err != nil {
		t.Fatalf("IssueAccountToken(admin): %v", err)
	}
	memberTok, err := auth.IssueAccountToken(ctx, st, member)
	if err != nil {
		t.Fatalf("IssueAccountToken(member): %v", err)
	}

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("net-test", bus, st, nil, nil, nil, nil)
	client := networkDoorHandler(t, svc, st, admin)

	// issue calls IssueToken (adminOnly) carrying the given bearer value (""
	// leaves the Authorization header absent), returning connect's error code.
	issue := func(bearer string) connect.Code {
		req := connect.NewRequest(&compassv1.IssueTokenRequest{AccountHandle: string(admin)})
		if bearer != "" {
			req.Header().Set("Authorization", bearer)
		}
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		_, err := client.IssueToken(ctx, req)
		return connect.CodeOf(err)
	}

	t.Run("valid admin token on adminOnly RPC succeeds", func(t *testing.T) {
		req := connect.NewRequest(&compassv1.IssueTokenRequest{AccountHandle: string(admin)})
		req.Header().Set("Authorization", "Bearer "+adminTok)
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		resp, err := client.IssueToken(ctx, req)
		if err != nil {
			t.Fatalf("admin token on IssueToken: %v", err)
		}
		if resp.Msg.GetToken() == "" {
			t.Fatal("IssueToken returned an empty token for the admin")
		}
	})

	t.Run("no token is Unauthenticated", func(t *testing.T) {
		if code := issue(""); code != connect.CodeUnauthenticated {
			t.Fatalf("no bearer token = %v, want CodeUnauthenticated", code)
		}
	})

	t.Run("garbage token is Unauthenticated", func(t *testing.T) {
		if code := issue("Bearer not-a-real-token"); code != connect.CodeUnauthenticated {
			t.Fatalf("garbage bearer token = %v, want CodeUnauthenticated", code)
		}
	})

	t.Run("non-admin token on adminOnly RPC is PermissionDenied", func(t *testing.T) {
		if code := issue("Bearer " + memberTok); code != connect.CodePermissionDenied {
			t.Fatalf("non-admin token on IssueToken = %v, want CodePermissionDenied", code)
		}
	})

	t.Run("non-admin token on open RPC succeeds", func(t *testing.T) {
		req := connect.NewRequest(&compassv1.GetServerInfoRequest{})
		req.Header().Set("Authorization", "Bearer "+memberTok)
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		if _, err := client.GetServerInfo(ctx, req); err != nil {
			t.Fatalf("non-admin token on the open GetServerInfo RPC: %v", err)
		}
	})
}

// TestNetworkDoorRejectionIsOracleSafe is the compass-mandated no-oracle
// guarantee (token.go's ORACLE RULE): the shared resolver distinguishes three
// failure classes internally (ErrTokenNotFound / ErrTokenRevoked / ErrWrongKind)
// for audit logging, but the DOOR MUST collapse all of them to ONE
// indistinguishable client-visible response, or the response becomes an oracle
// for whether a token is unknown, revoked, or issued for the other door. This
// drives all three classes — a never-issued token (ErrTokenNotFound), a revoked
// account token (ErrTokenRevoked), and a cross-door Runner token (ErrWrongKind) —
// over the real bearer chain and asserts the client sees BYTE-IDENTICAL
// rejections: same connect.Code (Unauthenticated) AND same message
// (errInvalidToken's text), with no field distinguishing them. The revoked class
// is the one a future maintainer is most tempted to special-case ("your token
// was revoked, re-auth"); doing so would diverge its response and redden this. If
// the door ever mapped any sentinel to a distinct code or leaked the reason into
// the message, the responses would diverge and this reddens.
func TestNetworkDoorRejectionIsOracleSafe(t *testing.T) {
	ctx := context.Background()
	st, admin, member := newNetworkStore(t)

	// A cross-door Runner token: resolves in the store but to SubjectRunner, so
	// the account door's want=SubjectAccount fails with ErrWrongKind.
	const runnerToken = "cnVubmVyLXRva2Vu"
	if err := st.PutTokenHash(ctx, sha256.Sum256([]byte(runnerToken)), store.Subject{Kind: store.SubjectRunner, ID: "some-runner"}); err != nil {
		t.Fatalf("PutTokenHash(runner): %v", err)
	}
	// A well-formed base64url token never issued: ErrTokenNotFound.
	const unknownToken = "dW5rbm93bi10b2tlbg"

	// A revoked account token: issued so it resolves to SubjectAccount, then its
	// hash revoked so ResolveToken surfaces ErrTokenRevoked. This is the sentinel
	// a future maintainer is most tempted to special-case ("re-auth"); the door
	// must still collapse it into the same rejection as the other two.
	revokedToken, err := auth.IssueAccountToken(ctx, st, member)
	if err != nil {
		t.Fatalf("IssueAccountToken(revoked): %v", err)
	}
	if err := st.RevokeToken(ctx, sha256.Sum256([]byte(revokedToken))); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("oracle-test", bus, st, nil, nil, nil, nil)
	client := networkDoorHandler(t, svc, st, admin)

	// reject drives GetServerInfo (an authenticatedOpen RPC, so the ONLY gate is
	// the bearer resolve — no admin-gate step to confound the comparison) with the
	// given bearer and returns the client-visible connect.Error.
	reject := func(bearer string) *connect.Error {
		req := connect.NewRequest(&compassv1.GetServerInfoRequest{})
		req.Header().Set("Authorization", "Bearer "+bearer)
		rpcCtx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		_, err := client.GetServerInfo(rpcCtx, req)
		var ce *connect.Error
		if !errors.As(err, &ce) {
			t.Fatalf("expected a connect.Error rejection, got %v", err)
		}
		return ce
	}

	notFound := reject(unknownToken)
	wrongKind := reject(runnerToken)
	revoked := reject(revokedToken)

	// Same code: all Unauthenticated (never a distinct code per class).
	if notFound.Code() != connect.CodeUnauthenticated {
		t.Fatalf("unknown-token code = %v, want CodeUnauthenticated", notFound.Code())
	}
	if wrongKind.Code() != connect.CodeUnauthenticated {
		t.Fatalf("cross-kind-token code = %v, want CodeUnauthenticated", wrongKind.Code())
	}
	if revoked.Code() != connect.CodeUnauthenticated {
		t.Fatalf("revoked-token code = %v, want CodeUnauthenticated", revoked.Code())
	}
	// Same message: all three rejections are byte-identical, so nothing in the
	// client-visible response reveals which sentinel fired (no oracle).
	if notFound.Message() != wrongKind.Message() {
		t.Fatalf("rejection messages differ — the door leaks which failure class fired (oracle):\n  not-found:  %q\n  wrong-kind: %q", notFound.Message(), wrongKind.Message())
	}
	if notFound.Message() != revoked.Message() {
		t.Fatalf("rejection messages differ — the door leaks which failure class fired (oracle):\n  not-found: %q\n  revoked:   %q", notFound.Message(), revoked.Message())
	}
}

// TestNetworkDoorStreamingBearerAuth is the streaming half of the door's auth
// matrix: the server-stream SubscribeEvents RPC over the real interceptor chain
// (BearerStreamInterceptor outer, AdminGate inner). The unary matrix
// (TestNetworkDoorBearerAuthAcceptAndReject) cannot exercise it — connect routes
// a server-stream through WrapStreamingHandler, a separate interceptor leg from
// the unary WrapUnary path. Each row pins one credential class to its outcome:
// SubscribeEvents is authenticatedOpen (not adminOnly), so a valid non-admin
// bearer is admitted; a missing or garbage bearer is rejected Unauthenticated
// before the handler runs. A dropped stream-auth wrapper, or an open stream RPC
// wrongly gated, reddens exactly the affected row.
func TestNetworkDoorStreamingBearerAuth(t *testing.T) {
	ctx := context.Background()
	st, admin, member := newNetworkStore(t)
	memberTok, err := auth.IssueAccountToken(ctx, st, member)
	if err != nil {
		t.Fatalf("IssueAccountToken(member): %v", err)
	}

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("net-test", bus, st, nil, nil, nil, nil)
	client := networkDoorHandler(t, svc, st, admin)

	// rejectCode opens a SubscribeEvents stream carrying the given bearer ("" leaves
	// the Authorization header absent) and returns the connect code the
	// door answers with. A rejected stream surfaces its terminal error on the
	// first Receive: the stream interceptor returns before the handler runs, so
	// no event ever arrives. recvStreamOrTimeout is the deadline safety net, not
	// a sleep — a wedged handler fails fast instead of hanging.
	rejectCode := func(t *testing.T, bearer string) connect.Code {
		t.Helper()
		req := connect.NewRequest(&compassv1.SubscribeEventsRequest{SinceSeq: 0})
		if bearer != "" {
			req.Header().Set("Authorization", bearer)
		}
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		stream, err := client.SubscribeEvents(ctx, req)
		if err != nil {
			return connect.CodeOf(err)
		}
		defer func() { _ = stream.Close() }()
		if recvStreamOrTimeout(t, stream) {
			t.Fatalf("a rejected stream delivered an event: %+v", stream.Msg())
		}
		return connect.CodeOf(stream.Err())
	}

	t.Run("no bearer is Unauthenticated", func(t *testing.T) {
		if code := rejectCode(t, ""); code != connect.CodeUnauthenticated {
			t.Fatalf("no bearer on SubscribeEvents = %v, want CodeUnauthenticated", code)
		}
	})

	t.Run("garbage bearer is Unauthenticated", func(t *testing.T) {
		if code := rejectCode(t, "Bearer not-a-real-token"); code != connect.CodeUnauthenticated {
			t.Fatalf("garbage bearer on SubscribeEvents = %v, want CodeUnauthenticated", code)
		}
	})

	t.Run("valid non-admin bearer opens the stream", func(t *testing.T) {
		// SubscribeEvents is authenticatedOpen, so the member's valid bearer
		// clears both the bearer resolve and the admin gate. The bus must carry
		// one event before subscribing: with an empty ring the handler tails
		// silently and never Sends, so the client's first Receive would block on
		// response headers that never flush. One primed Ready snapshot makes the
		// handler Send once — the open becomes observable without a sleep.
		bus.Publish(statusEvent())

		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		req := connect.NewRequest(&compassv1.SubscribeEventsRequest{SinceSeq: 0})
		req.Header().Set("Authorization", "Bearer "+memberTok)
		stream, err := client.SubscribeEvents(ctx, req)
		if err != nil {
			t.Fatalf("valid non-admin bearer on the open SubscribeEvents RPC: %v", err)
		}
		defer func() { _ = stream.Close() }()

		if !recvStreamOrTimeout(t, stream) {
			t.Fatalf("valid non-admin bearer opened no stream: first Receive = false, err = %v", stream.Err())
		}
		// since_seq==0 leads with the boundary frame (seq 0, no payload); the
		// primed Ready snapshot follows it.
		if b := stream.Msg(); b.GetSeq() != 0 || b.GetPayload() != nil {
			t.Fatalf("first frame = seq %d payload %T, want the boundary (seq 0 / nil)", b.GetSeq(), b.GetPayload())
		}
		if !recvStreamOrTimeout(t, stream) {
			t.Fatalf("no snapshot after the boundary: Receive = false, err = %v", stream.Err())
		}
		if stream.Msg().GetServerStatus() == nil {
			t.Fatalf("second event payload = %T, want the Ready snapshot (ServerStatus)", stream.Msg().GetPayload())
		}
		// Deferred cancel closes the stream: the handler's forward loop selects on
		// ctx.Done, returns, and releases the bus subscriber — no goroutine leak.
	})
}

// ---- Group 3: IssueToken handler contract + bootstrap-token gating ----

// TestIssueTokenHandlerInputContract pins the IssueToken handler's own
// input/mint contract, isolated from the door's auth (covered in group 2) by
// driving the bare handler: an EMPTY id is InvalidArgument, a non-empty but
// unknown id is NotFound, and an existing account mints a non-empty token that
// the store resolves back to that account. store.AccountID is a bare string, so
// "unparseable" is no longer a concept — empty vs unknown is the whole input
// partition. A flipped code mapping or a mint that does not register in the store
// reddens the matching sub-case.
func TestIssueTokenHandlerInputContract(t *testing.T) {
	ctx := t.Context()
	st, admin, _ := newNetworkStore(t)
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("test", bus, st, nil, nil, nil, nil)
	client := newH2CClient(t, newH2CTestServer(t, svc))

	t.Run("empty account id is InvalidArgument", func(t *testing.T) {
		rpcCtx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		_, err := client.IssueToken(rpcCtx, connect.NewRequest(&compassv1.IssueTokenRequest{AccountHandle: ""}))
		if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
			t.Fatalf("empty account_id = %v, want CodeInvalidArgument", code)
		}
	})

	t.Run("unknown account id is NotFound", func(t *testing.T) {
		rpcCtx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		_, err := client.IssueToken(rpcCtx, connect.NewRequest(&compassv1.IssueTokenRequest{AccountHandle: unknownAccountID}))
		if code := connect.CodeOf(err); code != connect.CodeNotFound {
			t.Fatalf("non-empty but unknown account_id = %v, want CodeNotFound", code)
		}
	})

	t.Run("existing account mints a resolvable token", func(t *testing.T) {
		rpcCtx, cancel := context.WithTimeout(ctx, testTimeout)
		defer cancel()
		resp, err := client.IssueToken(rpcCtx, connect.NewRequest(&compassv1.IssueTokenRequest{AccountHandle: string(admin)}))
		if err != nil {
			t.Fatalf("IssueToken for an existing account: %v", err)
		}
		token := resp.Msg.GetToken()
		if token == "" {
			t.Fatal("IssueToken returned an empty token for an existing account")
		}
		subj, err := auth.ResolveToken(ctx, st, token, store.SubjectAccount)
		if err != nil {
			t.Fatalf("the minted token does not resolve in the store: %v", err)
		}
		if subj.ID != string(admin) {
			t.Fatalf("minted token resolves to %s, want the target account %s", subj.ID, admin)
		}
	})

	t.Run("system account is PermissionDenied and mints no token", func(t *testing.T) {
		rpcCtx, cancel := context.WithTimeout(ctx, testTimeout)
		defer cancel()
		sys, err := st.EnsureSystemAccount(ctx)
		if err != nil {
			t.Fatalf("EnsureSystemAccount: %v", err)
		}
		resp, err := client.IssueToken(rpcCtx, connect.NewRequest(&compassv1.IssueTokenRequest{AccountHandle: string(sys.ID)}))
		if code := connect.CodeOf(err); code != connect.CodePermissionDenied {
			t.Fatalf("IssueToken for the system account = %v, want CodePermissionDenied — @compass is not authenticatable", code)
		}
		if resp != nil {
			t.Fatalf("IssueToken for the system account returned a response %+v — no token may be minted for @compass", resp.Msg)
		}
	})
}

// TestServeWithListenWritesAdminToken0600: on a --listen start, Serve mints the
// bootstrap-admin bearer token and persists it under StateDir at mode exactly
// 0600. The file's token must authenticate as the admin over the network door —
// an adminOnly RPC (IssueToken) presenting it clears both the bearer resolve and
// the admin gate, reaching the handler (NotFound for an unknown target). A token
// that failed to resolve would be Unauthenticated; a non-admin token would be
// PermissionDenied — so CodeNotFound uniquely proves "resolves to the admin".
func TestServeWithListenWritesAdminToken0600(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "compass.sock")
	stateDir := filepath.Join(dir, "state")
	certPath, keyPath, pool := writeSelfSignedCert(t, dir)
	addr := freeLoopbackAddr(t)

	serveInBackground(t, ServeConfig{
		SocketPath:  socketPath,
		DatabaseDSN: pgtest.RequireDSN(t),
		Version:     "net-test",
		Listen:      addr,
		TLS:         &TLSConfig{CertPath: certPath, KeyPath: keyPath},
		StateDir:    stateDir,
	})
	waitServing(t, socketPath)

	tokenPath := filepath.Join(stateDir, adminTokenFile)
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("stat admin-token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("admin-token mode = %o, want 0600 (owner-only credential)", perm)
	}
	raw, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read admin-token file: %v", err)
	}
	token := string(raw)
	if token == "" {
		t.Fatal("admin-token file is empty")
	}

	client := newTLSClient(t, addr, pool)
	req := connect.NewRequest(&compassv1.IssueTokenRequest{AccountHandle: unknownAccountID})
	req.Header().Set("Authorization", "Bearer "+token)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	_, err = client.IssueToken(ctx, req)
	if code := connect.CodeOf(err); code != connect.CodeNotFound {
		t.Fatalf("file's admin token on IssueToken{unknown} = %v, want CodeNotFound (proves it resolves to the admin and clears the gate)", code)
	}
}

// TestServeSocketOnlyWritesNoAdminToken: a socket-only start (no --listen) mints
// no bearer token — the 0600 socket is the credential, so no admin-token file is
// written — yet the socket door still serves (the bootstrap-admin account is
// created unconditionally, so Serve reaches the serving stage rather than erroring
// on a missing admin). Guards the --listen gating of the token mint against a
// regression that writes the credential on the socket-only path.
func TestServeSocketOnlyWritesNoAdminToken(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "compass.sock")
	stateDir := filepath.Join(dir, "state")

	serveInBackground(t, ServeConfig{
		SocketPath:  socketPath,
		DatabaseDSN: pgtest.RequireDSN(t),
		Version:     "socket-only",
		StateDir:    stateDir,
		// No Listen, no TLS: the shipped socket-only path.
	})
	// A successful RPC over the UDS proves Serve got past bootstrapAdmin (a failed
	// bootstrap would have errored Serve before it ever served) and is serving.
	waitServing(t, socketPath)

	tokenPath := filepath.Join(stateDir, adminTokenFile)
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("admin-token file written on the socket-only path (stat err = %v), want none", err)
	}
}

// TestServeNetworkDoorRejectsTLS12: the network door enforces a TLS 1.3 floor
// (loadNetworkTLS sets MinVersion: tls.VersionTLS13), so a client that offers at
// most TLS 1.2 (MaxVersion pinned) MUST be rejected at version negotiation — the
// bearer-token confidentiality rests entirely on TLS, so no 1.2 downgrade is
// allowed onto the door. Reddens if the floor regresses (MinVersion lowered to
// 1.2 or dropped): the 1.2-capped handshake would then succeed and err be nil.
// err != nil is the load-bearing assertion — the exact stdlib version-mismatch
// message is the runtime's, not our contract, so it is deliberately not pinned.
func TestServeNetworkDoorRejectsTLS12(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "compass.sock")
	stateDir := filepath.Join(dir, "state")
	certPath, keyPath, pool := writeSelfSignedCert(t, dir)
	addr := freeLoopbackAddr(t)

	serveInBackground(t, ServeConfig{
		SocketPath:  socketPath,
		DatabaseDSN: pgtest.RequireDSN(t),
		Version:     "tls12-reject-test",
		Listen:      addr,
		TLS:         &TLSConfig{CertPath: certPath, KeyPath: keyPath},
		StateDir:    stateDir,
	})
	// The network listener binds before the socket (Serve's ordering), so once the
	// socket serves an RPC the TCP port is accepting and a dial reaches the TLS
	// handshake rather than a closed port.
	waitServing(t, socketPath)

	// A TLS-1.2-capped client that otherwise trusts the cert and names the server
	// correctly must fail the handshake against the 1.3-floor door — the failure
	// is the version negotiation, not a cert or name mismatch. A deadline dialer
	// bounds a wedged handshake so a regression that hangs fails fast.
	conn, err := tls.DialWithDialer(
		&net.Dialer{Deadline: time.Now().Add(testTimeout)},
		"tcp", addr,
		&tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MaxVersion: tls.VersionTLS12},
	)
	if err == nil {
		conn.Close()
		t.Fatal("TLS 1.2 handshake succeeded against the network door — the TLS 1.3 floor (MinVersion) regressed")
	}
}

// syncBuffer is a mutex-guarded bytes.Buffer used to capture slog output.
// slog.SetDefault swaps a PROCESS-GLOBAL logger and Serve logs from multiple
// goroutines, so the capture is written concurrently; the lock makes Write and
// String data-race-free under -race. Without it the concurrent Write/String is a
// flagged race, not merely a lost log line.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestServeWithListenNeverLogsAdminToken: on a --listen start Serve mints the
// bootstrap-admin bearer token and logs that it was written — but the log SHALL
// carry only the token's PATH, never the token value (a logged credential lets
// anyone reading process output or aggregated logs impersonate the admin).
// Captures the process-global slog default into a mutex-guarded buffer for the
// run and restores the prior default in Cleanup (global state shared with
// sibling tests). Two assertions: the minted token string is ABSENT from the
// captured logs (reddens if the value ever leaks), and the token file path is
// PRESENT (reddens if the write is silently un-logged — which would make the
// absence check vacuously pass because nothing was logged). Owns the global
// logger for its duration, so it MUST NOT call t.Parallel().
func TestServeWithListenNeverLogsAdminToken(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	sb := &syncBuffer{}
	// Set the capturing default BEFORE serveInBackground so the startup log line
	// (buildNetworkServer's "bootstrap admin token written") is captured.
	slog.SetDefault(slog.New(slog.NewTextHandler(sb, nil)))

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "compass.sock")
	stateDir := filepath.Join(dir, "state")
	certPath, keyPath, _ := writeSelfSignedCert(t, dir)
	addr := freeLoopbackAddr(t)

	serveInBackground(t, ServeConfig{
		SocketPath:  socketPath,
		DatabaseDSN: pgtest.RequireDSN(t),
		Version:     "net-test",
		Listen:      addr,
		TLS:         &TLSConfig{CertPath: certPath, KeyPath: keyPath},
		StateDir:    stateDir,
	})
	// Gates on the door serving, which in Serve's ordering is strictly after the
	// token write + its log line — so the captured logs already contain that line.
	waitServing(t, socketPath)

	token := readAdminToken(t, stateDir)
	logs := sb.String()

	if strings.Contains(logs, token) {
		t.Fatal("admin token value leaked into the server logs (a logged bearer credential lets any log reader impersonate the admin)")
	}
	tokenPath := filepath.Join(stateDir, adminTokenFile)
	if !strings.Contains(logs, tokenPath) {
		t.Fatalf("admin-token path %q not found in logs — the bootstrap-token log line did not run, so the token-absence check above is vacuous", tokenPath)
	}
}
