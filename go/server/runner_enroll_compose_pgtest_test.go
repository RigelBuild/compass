//go:build pgtest && unix

package server

// Whole-flow composition of the Server<->Runner enrollment seam (RIG-1914 S2,
// DL-178). This is the one seam no suite on main composes: a REAL Runner (the
// production internal/runner.Dial client) enrolling through the PRODUCTION
// RunnerService door — the one buildNetworkServer mounts behind its Runner-subject
// bearer interceptor — over TLS, with a REAL minted Runner token resolved against
// the store of record.
//
// Every other RunnerService test stops short of this composition. The runnerhub
// package's own wire tests (seam_test.go, integration_pgtest_test.go) mount the
// handler on a bare h2c httptest server via NewMountedHandler with a FAKE
// resolver — they never build the door through buildNetworkServer, never
// terminate TLS, and never resolve a token minted by MintRunnerToken against a
// real store. This test closes exactly that gap: it drives the real serving path
// (Serve -> buildDoors -> buildNetworkServer, --listen + TLS), mints a Runner
// token with runnerhub.MintRunnerToken against the same per-test schema Serve
// opens, and dials the served door with runner.Dial. If buildNetworkServer ever
// stopped mounting the RunnerService door, mounted it without the Runner bearer
// interceptor, or wired the wrong resolver, the positive leg reddens; the
// negative leg proves the Kind gate has teeth (a bad token is Unauthenticated),
// so a door that accepted anything would redden too.
//
// Store-gated (Serve opens the store; the Runner bearer interceptor resolves the
// token against it), so it lives in the `pgtest` lane behind the shared harness.
// It reuses network_door_test.go's TLS + door helpers (writeSelfSignedCert,
// freeLoopbackAddr, serveInBackground, waitServing) and adds only the Runner-side
// TLS HTTP client. White-box (package server) so it drives Serve through the
// unexported serving path the same way the sibling network-door pgtest tests do;
// package runner does not import package server (no import cycle), and package
// server does not import internal/runner in production — only this test does.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/runner"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// newTLSRunnerHTTPClient builds an HTTP/2-over-TLS client that trusts only pool —
// the Runner-side counterpart to network_door_test.go's newTLSClient, handed to
// runner.Dial as its RunnerConfig.HTTPClient so the Runner reaches the door over
// the same terminated TLS transport a real remote Runner uses. Idle conns close
// via t.Cleanup.
func newTLSRunnerHTTPClient(t *testing.T, pool *x509.CertPool) *http.Client {
	t.Helper()
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetHTTP2(true)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"},
		Protocols:       p,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// TestRunnerEnrollsThroughNetworkDoorWithMintedToken composes the enrollment seam
// end-to-end: a real minted Runner token enrolls through the production
// RunnerService door over TLS; an unminted token is rejected Unauthenticated at
// the hash-lookup miss; and a valid ACCOUNT token is rejected at the Kind gate
// (the OQ7 cross-door rule) — the leg that defends the cross-door contract.
func TestRunnerEnrollsThroughNetworkDoorWithMintedToken(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "compass.sock")
	stateDir := filepath.Join(dir, "state")
	certPath, keyPath, pool := writeSelfSignedCert(t, dir)
	addr := freeLoopbackAddr(t)

	// Mint the Runner token against the SAME per-test schema Serve will open, so
	// the door's bearer interceptor resolves it. Migration is idempotent, so
	// Serve re-opening this DSN is a no-op; the token hash committed here is the
	// credential the door then authenticates. Done synchronously before Serve
	// starts so there is no concurrent-Open race.
	const runnerID = "runner-e2e"
	dsn := pgtest.RequireDSN(t)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	token, err := runnerhub.MintRunnerToken(ctx, st, runnerID)
	if err != nil {
		t.Fatalf("MintRunnerToken: %v", err)
	}

	serveInBackground(t, ServeConfig{
		SocketPath:  socketPath,
		DatabaseDSN: dsn,
		Version:     "runner-enroll-test",
		Listen:      addr,
		TLS:         &TLSConfig{CertPath: certPath, KeyPath: keyPath},
		StateDir:    stateDir,
	})
	// The network listener binds before the socket (Serve's ordering), so once
	// the socket serves an RPC the TLS door — RunnerService included — is
	// accepting.
	waitServing(t, socketPath)

	httpClient := newTLSRunnerHTTPClient(t, pool)
	serverAddr := "https://" + addr

	// Positive leg: the real Runner dials the served door with the minted token
	// and Enroll succeeds. Dial constructs the RunnerService client behind its
	// bearer interceptor and calls Enroll; a nil error + non-nil link proves the
	// door authenticated the Runner-kind token and registered it. The first
	// enrollment of a fresh id must not report a re-attach. This is the leg that
	// would 401 if the token had never been minted/stored — the door's Kind gate
	// resolves the presented token against the store, and an unresolved token is
	// Unauthenticated (see the negative leg), so the mint is load-bearing here.
	dctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	link, err := runner.Dial(dctx, runner.RunnerConfig{
		RunnerID:   runnerID,
		ServerAddr: serverAddr,
		Token:      token,
		HTTPClient: httpClient,
	})
	if err != nil {
		t.Fatalf("runner.Dial with a real minted Runner token over the TLS network door: %v", err)
	}
	if link == nil {
		t.Fatal("runner.Dial returned a nil ServerLink with no error")
	}
	if link.Reattached() {
		t.Fatal("first Enroll reattached = true, want false (fresh registration)")
	}

	// Negative leg 1 (not-found): an unminted string is rejected Unauthenticated.
	// This exercises the hash-lookup miss branch (store.ErrNotFound ->
	// ErrTokenNotFound -> errUnauthenticated), not the Kind gate — a token that
	// never resolves fails before subj.Kind is ever compared. It proves the
	// positive leg is not a vacuous accept-anything. runner.Dial wraps the enroll
	// error, so unwrap to the connect code.
	bctx, bcancel := context.WithTimeout(ctx, testTimeout)
	defer bcancel()
	_, err = runner.Dial(bctx, runner.RunnerConfig{
		RunnerID:   runnerID,
		ServerAddr: serverAddr,
		Token:      "not-a-real-runner-token",
		HTTPClient: httpClient,
	})
	if err == nil {
		t.Fatal("runner.Dial with an unminted token = nil error, want Unauthenticated (not-found branch)")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Fatalf("unminted-token Dial code = %v, want CodeUnauthenticated", code)
	}

	// Negative leg 2 (the real Kind gate, OQ7 cross-door): present a VALID
	// account token — the bootstrap admin token buildNetworkServer minted and
	// wrote under stateDir — to the RUNNER door. It resolves to a real subject,
	// so it clears the hash-lookup branch and reaches `subj.Kind != want`
	// (SubjectAccount presented where SubjectRunner is required), which rejects
	// it Unauthenticated. This is the leg that goes red if the Kind gate is
	// removed or inverted — the not-found leg above would stay green through
	// such a regression, so this is what actually defends the cross-door rule
	// through buildNetworkServer's real resolver + TLS.
	adminToken := readAdminToken(t, stateDir)
	kctx, kcancel := context.WithTimeout(ctx, testTimeout)
	defer kcancel()
	_, err = runner.Dial(kctx, runner.RunnerConfig{
		RunnerID:   runnerID,
		ServerAddr: serverAddr,
		Token:      adminToken,
		HTTPClient: httpClient,
	})
	if err == nil {
		t.Fatal("runner.Dial with a valid ACCOUNT token on the Runner door = nil error, want Unauthenticated (the Kind gate must reject a wrong-subject token)")
	}
	if code := connect.CodeOf(err); code != connect.CodeUnauthenticated {
		t.Fatalf("account-token-on-runner-door Dial code = %v, want CodeUnauthenticated (Kind gate)", code)
	}
}
